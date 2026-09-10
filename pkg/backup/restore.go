package backup

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/certainty3452/dbtether/pkg/storage"
)

const (
	onConflictFail       = "fail"
	onConflictDrop       = "drop"
	maintenanceDatabase  = "postgres"
	gzipMagic            = "\x1f\x8b"
	customFormatMagic    = "PGDMP"
	connectionFailedMark = " failed: "
	connectTimeoutVar    = "PGCONNECT_TIMEOUT"
	connectTimeout       = "30"

	peekChunkBytes = 32 << 10
	peekLimitBytes = 1 << 20
)

// RestoreConfig contains all parameters needed for a restore operation
type RestoreConfig struct {
	// Database connection
	Host     string
	Port     int
	Database string
	Username string
	Password string
	SSLMode  string

	// Source
	SourcePath string

	// Storage configuration
	StorageType string

	// S3 config
	S3Config *storage.S3Config

	// GCS config
	GCSConfig *storage.GCSConfig

	// Azure config
	AzureConfig *storage.AzureConfig

	OnConflict string

	Logger *slog.Logger
}

// RunRestore executes the restore operation
func RunRestore(ctx context.Context, cfg *RestoreConfig) error {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if cfg.OnConflict != onConflictFail && cfg.OnConflict != onConflictDrop {
		return fmt.Errorf("unsupported onConflict %q: expected fail or drop", cfg.OnConflict)
	}

	logger.Info("starting restore",
		"database", cfg.Database,
		"source", cfg.SourcePath,
		"onConflict", cfg.OnConflict,
	)

	psql, err := resolvePsql(ctx, pgClientRoot, &serverProbe{
		Host:     cfg.Host,
		Port:     cfg.Port,
		Username: cfg.Username,
		Database: probeDatabase(cfg),
		Env:      psqlEnv(cfg),
	}, logger)
	if err != nil {
		return err
	}

	// Download backup file from storage
	backupData, err := downloadBackup(ctx, cfg, logger)
	if err != nil {
		return fmt.Errorf("failed to download backup: %w", err)
	}
	defer func() {
		if closeErr := backupData.Close(); closeErr != nil {
			logger.Warn("failed to close backup data", "error", closeErr)
		}
	}()

	if err := runRestoreStream(ctx, cfg, psql, backupData, logger); err != nil {
		return err
	}

	logger.Info("restore completed successfully", "database", cfg.Database)
	return nil
}

func runRestoreStream(ctx context.Context, cfg *RestoreConfig, psql string, backupData io.Reader, logger *slog.Logger) error {
	dump, closer, err := openDumpStream(backupData)
	if closer != nil {
		defer func() {
			if closeErr := closer.Close(); closeErr != nil {
				logger.Warn("failed to close gzip reader", "error", closeErr)
			}
		}()
	}
	if err != nil {
		return err
	}

	// Handle conflict strategy
	switch cfg.OnConflict {
	case onConflictDrop:
		if err := dropAndRecreateDatabase(ctx, cfg, psql, logger); err != nil {
			return fmt.Errorf("failed to drop/recreate database: %w", err)
		}
	case onConflictFail:
		isEmpty, err := isDatabaseEmpty(ctx, cfg, psql)
		if err != nil {
			return fmt.Errorf("failed to check if database is empty: %w", err)
		}
		if !isEmpty {
			return fmt.Errorf("database is not empty and onConflict=fail")
		}
	}

	if err := runPsqlRestore(ctx, cfg, psql, dump, logger); err != nil {
		return fmt.Errorf("restore failed: %w", err)
	}
	return nil
}

func downloadBackup(ctx context.Context, cfg *RestoreConfig, logger *slog.Logger) (io.ReadCloser, error) {
	logger.Info("downloading backup", "path", cfg.SourcePath, "storageType", cfg.StorageType)

	switch cfg.StorageType {
	case "s3":
		client, err := storage.NewS3Client(ctx, cfg.S3Config, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to create S3 client: %w", err)
		}
		return client.Download(ctx, cfg.SourcePath)

	case "gcs":
		client, err := storage.NewGCSClient(ctx, cfg.GCSConfig, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to create GCS client: %w", err)
		}
		return client.Download(ctx, cfg.SourcePath)

	case "azure":
		client, err := storage.NewAzureClient(ctx, cfg.AzureConfig, logger)
		if err != nil {
			return nil, fmt.Errorf("failed to create Azure client: %w", err)
		}
		return client.Download(ctx, cfg.SourcePath)

	default:
		return nil, fmt.Errorf("unsupported storage type: %s", cfg.StorageType)
	}
}

// argv is world-readable via /proc; sslmode/password must go through libpq env.
func psqlArgs(cfg *RestoreConfig, dbName string) []string {
	return []string{
		"-h", cfg.Host,
		"-p", fmt.Sprintf("%d", cfg.Port),
		"-U", cfg.Username,
		"-d", dbName,
	}
}

func psqlEnv(cfg *RestoreConfig) []string {
	env := append(os.Environ(), "PGPASSWORD="+cfg.Password)
	if cfg.SSLMode != "" {
		env = append(env, "PGSSLMODE="+cfg.SSLMode)
	}
	return withConnectTimeout(env)
}

// A TCP connect that never completes would otherwise keep the Job running until its deadline.
func withConnectTimeout(env []string) []string {
	for _, entry := range env {
		if strings.HasPrefix(entry, connectTimeoutVar+"=") {
			return env
		}
	}
	return append(env, connectTimeoutVar+"="+connectTimeout)
}

// The drop path must survive a target left missing by an earlier failed run.
func probeDatabase(cfg *RestoreConfig) string {
	if cfg.OnConflict == onConflictDrop {
		return maintenanceDatabase
	}
	return cfg.Database
}

func dropAndRecreateDatabase(ctx context.Context, cfg *RestoreConfig, psql string, logger *slog.Logger) error {
	logger.Info("dropping and recreating database", "database", cfg.Database)

	dropConnsSQL := fmt.Sprintf(`
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE datname = %s AND pid <> pg_backend_pid()
	`, quoteLiteral(cfg.Database))

	base := psqlArgs(cfg, maintenanceDatabase)
	env := psqlEnv(cfg)

	if diagnostic, err := runPsql(ctx, psql, env, append(base, "-c", dropConnsSQL), io.Discard); err != nil {
		logger.Warn("failed to terminate connections", "error", withDiagnostic(err, diagnostic))
	}

	dropSQL := fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdentifier(cfg.Database))
	if diagnostic, err := runPsql(ctx, psql, env, append(base, "-c", dropSQL), io.Discard); err != nil {
		return fmt.Errorf("failed to drop database: %w", withDiagnostic(err, diagnostic))
	}

	createSQL := fmt.Sprintf("CREATE DATABASE %s", quoteIdentifier(cfg.Database))
	if diagnostic, err := runPsql(ctx, psql, env, append(base, "-c", createSQL), io.Discard); err != nil {
		return fmt.Errorf("failed to create database: %w", withDiagnostic(err, diagnostic))
	}

	logger.Info("database recreated", "database", cfg.Database)
	return nil
}

func isDatabaseEmpty(ctx context.Context, cfg *RestoreConfig, psql string) (bool, error) {
	sql := `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname NOT LIKE 'pg_toast%'
		  AND n.nspname NOT LIKE 'pg_temp%'
		  AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')`

	var count bytes.Buffer
	args := append(psqlArgs(cfg, cfg.Database), "-t", "-c", sql)
	diagnostic, err := runPsql(ctx, psql, psqlEnv(cfg), args, &count)
	if err != nil {
		return false, withDiagnostic(err, diagnostic)
	}

	return strings.TrimSpace(count.String()) == "0", nil
}

func openDumpStream(backupData io.Reader) (io.Reader, io.Closer, error) {
	reader, closer, err := decompressDump(backupData)
	if err != nil {
		return nil, nil, err
	}

	if err := rejectCustomFormat(reader); err != nil {
		return nil, closer, err
	}

	script := newRestoreScript(reader)
	head, err := peekFirstStatement(script)
	if err != nil {
		return nil, closer, err
	}
	return io.MultiReader(head, script), closer, nil
}

// A custom-format archive would pass the statement peek and fail in psql with the target already dropped.
func rejectCustomFormat(dump *bufio.Reader) error {
	magic, err := dump.Peek(len(customFormatMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("failed to read the backup stream: %w", err)
	}
	if string(magic) == customFormatMagic {
		return errors.New("backup is a pg_dump custom-format archive; only plain-format SQL dumps can be restored")
	}
	return nil
}

// psql exits 0 on a dump with nothing to run, so an object without statements would report success over a dropped database.
func peekFirstStatement(script *restoreScript) (io.Reader, error) {
	var head bytes.Buffer
	chunk := make([]byte, peekChunkBytes)

	for script.Statements() == 0 {
		n, err := script.Read(chunk)
		head.Write(chunk[:n])

		if err != nil {
			if !errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("failed to read the backup stream: %w", err)
			}
			break
		}
		if head.Len() > peekLimitBytes {
			return nil, fmt.Errorf("backup contains no SQL statement in its first %d bytes", peekLimitBytes)
		}
	}

	if script.Statements() == 0 {
		return nil, errors.New("backup contains no SQL statements")
	}
	// Past the first statement, whether the peek saw a byte at all depends on the source's read sizes.
	if bytes.IndexByte(head.Bytes()[:script.HeadBytes()], 0) >= 0 {
		return nil, errors.New("backup is not a plain-format SQL dump")
	}
	return &head, nil
}

// The object key says nothing about compression: it is whatever filenameTemplate produced.
func decompressDump(backupData io.Reader) (*bufio.Reader, io.Closer, error) {
	buffered := bufio.NewReader(backupData)

	magic, err := buffered.Peek(len(gzipMagic))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, fmt.Errorf("failed to read the backup stream: %w", err)
	}
	if string(magic) != gzipMagic {
		return buffered, nil, nil
	}

	gzReader, err := gzip.NewReader(buffered)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	return bufio.NewReader(gzReader), gzReader, nil
}

func runPsqlRestore(ctx context.Context, cfg *RestoreConfig, psql string, dump io.Reader, logger *slog.Logger) error {
	logger.Info("restoring database with psql", "database", cfg.Database)

	// -f - instead of plain stdin so psql errors carry the dump line number.
	args := append(psqlArgs(cfg, cfg.Database), "-v", "ON_ERROR_STOP=1", "--single-transaction", "-f", "-")
	//nolint:gosec // args come from the CRD spec
	cmd := exec.CommandContext(ctx, psql, args...)
	cmd.Stdin = dump
	cmd.Env = psqlEnv(cfg)
	cmd.Stdout = io.Discard
	stderr := &tailWriter{limit: psqlStderrTailBytes}
	cmd.Stderr = io.MultiWriter(stderr, os.Stderr)

	if err := cmd.Run(); err != nil {
		if tail := stderr.String(); tail != "" {
			return fmt.Errorf("psql failed: %w: %s", err, tail)
		}
		return fmt.Errorf("psql failed: %w", err)
	}

	logger.Info("restore completed", "database", cfg.Database)
	return nil
}

func runPsql(ctx context.Context, psql string, env, args []string, stdout io.Writer) (string, error) {
	//nolint:gosec // args come from the CRD spec
	cmd := exec.CommandContext(ctx, psql, args...)
	cmd.Env = env
	cmd.Stdout = stdout

	var stderr bytes.Buffer
	cmd.Stderr = io.MultiWriter(&stderr, os.Stderr)

	err := cmd.Run()
	return serverDiagnostic(stderr.String()), err
}

func withDiagnostic(err error, diagnostic string) error {
	if diagnostic == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, diagnostic)
}

func serverDiagnostic(stderr string) string {
	var kept []string
	for _, line := range strings.Split(stderr, "\n") {
		if message := serverMessage(strings.TrimSpace(line)); message != "" {
			kept = append(kept, message)
		}
	}

	joined := strings.Join(kept, "; ")
	if len(joined) <= psqlStderrTailBytes {
		return joined
	}
	// The cut can split a rune and status.message must stay valid UTF-8.
	return strings.ToValidUTF8(joined[:psqlStderrTailBytes], "")
}

// Anything the client quotes is an endpoint or the query, never the server's message.
func serverMessage(line string) string {
	errorPrefix := matchPrefix(line, clientErrorPrefixes)
	if errorPrefix != "" || matchPrefix(line, clientDetailPrefixes) != "" {
		if _, message, ok := strings.Cut(line, connectionFailedMark); ok {
			return message
		}
	}

	if tail := severityTail(line); tail != "" {
		return tail
	}

	if errorPrefix != "" && !strings.Contains(line, `"`) {
		return strings.TrimPrefix(line, errorPrefix)
	}
	return ""
}

func matchPrefix(line string, prefixes []string) string {
	for _, prefix := range prefixes {
		if strings.HasPrefix(line, prefix) {
			return prefix
		}
	}
	return ""
}

func severityTail(line string) string {
	for _, severity := range serverSeverities {
		if i := strings.Index(line, severity); i >= 0 {
			return line[i:]
		}
	}
	return ""
}

var (
	serverSeverities     = []string{"ERROR:", "FATAL:"}
	clientErrorPrefixes  = []string{psqlErrorPrefix, "pg_dump: error: "}
	clientDetailPrefixes = []string{"psql: detail: ", "pg_dump: detail: "}
)

// psql prints its ERROR last, after any NOTICEs, so the tail is the diagnostic part.
const psqlStderrTailBytes = 2048

const (
	psqlLinePrefix  = "psql:"
	psqlErrorPrefix = "psql: error: "
)

type tailWriter struct {
	lines   []string
	partial []byte
	limit   int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			w.buffer(p)
			break
		}
		w.buffer(p[:end])
		w.take(string(w.partial))
		w.partial = w.partial[:0]
		p = p[end+1:]
	}
	return written, nil
}

func (w *tailWriter) String() string {
	kept := w.lines
	if tail := diagnosticLine(string(w.partial)); tail != "" {
		kept = evictOldest(append(append([]string(nil), kept...), tail), w.limit)
	}
	// The cut can split a rune and status.message must stay valid UTF-8.
	return strings.ToValidUTF8(strings.Join(kept, "; "), "")
}

// Cut at the head: psql leads with "psql:<stdin>:N: ERROR:", the part worth reporting.
func (w *tailWriter) buffer(chunk []byte) {
	room := w.limit - len(w.partial)
	if room <= 0 {
		return
	}
	if len(chunk) > room {
		chunk = chunk[:room]
	}
	w.partial = append(w.partial, chunk...)
}

func (w *tailWriter) take(line string) {
	if line = diagnosticLine(line); line == "" {
		return
	}
	w.lines = evictOldest(append(w.lines, line), w.limit)
}

// Only psql's own prefix marks a line as diagnostic; DETAIL/HINT/CONTEXT/LINE carry unprefixed dump data.
func diagnosticLine(line string) string {
	line = strings.TrimSuffix(line, "\r")
	if strings.HasPrefix(line, psqlErrorPrefix) {
		return serverMessage(line)
	}
	if !strings.HasPrefix(line, psqlLinePrefix) {
		return ""
	}
	return line
}

func evictOldest(lines []string, limit int) []string {
	total := 0
	for _, line := range lines {
		total += len(line)
	}
	for total > limit && len(lines) > 1 {
		total -= len(lines[0])
		lines = lines[1:]
	}
	return lines
}

// Local copies — backup-job binary doesn't need to pull lib/pq just for two quoters.
func quoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
