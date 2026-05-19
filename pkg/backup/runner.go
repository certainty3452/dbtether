package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/certainty3452/dbtether/pkg/storage"
)

type BackupConfig struct {
	// Database connection
	Host     string
	Port     int
	Database string
	Username string
	Password string

	// Storage
	StorageType string // "s3", "gcs", "azure"
	S3Config    storage.S3Config
	GCSConfig   storage.GCSConfig
	AzureConfig storage.AzureConfig

	// Output
	PathTemplate     string
	FilenameTemplate string

	// Metadata for templates and tags
	ClusterName  string
	DatabaseName string
	BackupName   string
	Namespace    string
	RunID        string // Unique identifier for this backup run
}

type TemplateData struct {
	ClusterName  string
	DatabaseName string
	Year         string
	Month        string
	Day          string
	Timestamp    string
	RunID        string // Unique identifier for this backup run (8 alphanumeric chars)
}

// BackupResult contains the results of a backup operation
type BackupResult struct {
	Path             string // Full path to the backup file in storage
	Size             int64  // Size of compressed backup in bytes
	UncompressedSize int64  // Size before compression
	Duration         time.Duration
}

func RunBackup(ctx context.Context, cfg *BackupConfig) (*BackupResult, error) {
	startTime := time.Now()

	now := time.Now().UTC()
	data := TemplateData{
		ClusterName:  cfg.ClusterName,
		DatabaseName: cfg.DatabaseName,
		Year:         now.Format("2006"),
		Month:        now.Format("01"),
		Day:          now.Format("02"),
		Timestamp:    now.Format("20060102-150405"),
		RunID:        cfg.RunID,
	}

	path, err := executeTemplate(cfg.PathTemplate, &data)
	if err != nil {
		return nil, fmt.Errorf("failed to execute path template: %w", err)
	}

	filename, err := executeTemplate(cfg.FilenameTemplate, &data)
	if err != nil {
		return nil, fmt.Errorf("failed to execute filename template: %w", err)
	}

	fullPath := strings.TrimSuffix(path, "/") + "/" + filename

	tags := &storage.ObjectTags{
		Database:   cfg.DatabaseName,
		Cluster:    cfg.ClusterName,
		BackupName: cfg.BackupName,
		Namespace:  cfg.Namespace,
		Timestamp:  data.Timestamp,
		CreatedBy:  "dbtether",
	}

	result, err := runStreamingBackup(ctx, cfg, fullPath, tags)
	if err != nil {
		return nil, err
	}

	result.Duration = time.Since(startTime)
	return result, nil
}

// runStreamingBackup streams pg_dump output through gzip directly to storage.
// Memory usage is O(buffer size) instead of O(database size).
func runStreamingBackup(ctx context.Context, cfg *BackupConfig, fullPath string, tags *storage.ObjectTags) (*BackupResult, error) {
	pr, pw := io.Pipe()

	var uncompressedSize atomic.Int64
	var compressedSize atomic.Int64
	var pgDumpErr error
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		pgDumpErr = runPgDumpToWriter(ctx, cfg, pw, &uncompressedSize)
	}()

	gzipReader := newGzipStreamReader(pr, &compressedSize)
	uploadErr := uploadToStorage(ctx, cfg, fullPath, gzipReader, tags)

	// Unblocks pg_dump goroutine if upload aborted mid-stream — otherwise it hangs.
	if uploadErr != nil {
		_ = pr.CloseWithError(uploadErr)
	}

	wg.Wait()

	if pgDumpErr != nil {
		return nil, fmt.Errorf("pg_dump failed: %w", pgDumpErr)
	}
	if uploadErr != nil {
		return nil, fmt.Errorf("upload failed: %w", uploadErr)
	}

	return &BackupResult{
		Path:             fullPath,
		Size:             compressedSize.Load(),
		UncompressedSize: uncompressedSize.Load(),
	}, nil
}

func runPgDumpToWriter(ctx context.Context, cfg *BackupConfig, pw *io.PipeWriter, size *atomic.Int64) error {
	defer func() { _ = pw.Close() }()

	// #nosec G204 -- args from trusted config (CRD spec), not user input
	cmd := exec.CommandContext(ctx, "pg_dump",
		"--host", cfg.Host,
		"--port", fmt.Sprintf("%d", cfg.Port),
		"--dbname", cfg.Database,
		"--username", cfg.Username,
		"--format=plain",
		"--no-owner",
		"--no-acl",
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cfg.Password)
	cmd.Stdout = &countingWriter{w: pw, count: size}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		err = fmt.Errorf("pg_dump error: %s, stderr: %s", err, stderr.String())
		_ = pw.CloseWithError(err)
		return err
	}
	return nil
}

func uploadToStorage(ctx context.Context, cfg *BackupConfig, path string, data io.Reader, tags *storage.ObjectTags) error {
	// S3 uses multipart Uploader (UploadStreaming); GCS/Azure unify through
	// UploadWithTags. Both go through the shared StorageClient factory.
	if cfg.StorageType == "s3" {
		client, err := storage.NewS3Client(ctx, &cfg.S3Config, nil)
		if err != nil {
			return fmt.Errorf("failed to create S3 client: %w", err)
		}
		return client.UploadStreaming(ctx, path, data, tags)
	}

	var gcs *storage.GCSConfig
	var az *storage.AzureConfig
	switch cfg.StorageType {
	case "gcs":
		gcs = &cfg.GCSConfig
	case "azure":
		az = &cfg.AzureConfig
	default:
		return fmt.Errorf("unsupported storage type: %s", cfg.StorageType)
	}
	client, err := storage.NewClient(ctx, nil, gcs, az, nil)
	if err != nil {
		return fmt.Errorf("failed to create storage client: %w", err)
	}
	if closer, ok := client.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	return client.UploadWithTags(ctx, path, data, tags)
}

type countingWriter struct {
	w     io.Writer
	count *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.count.Add(int64(n))
	return n, err
}

type gzipStreamReader struct {
	pr            *io.PipeReader
	pw            *io.PipeWriter
	compressedCnt *atomic.Int64
	done          chan struct{}
}

func newGzipStreamReader(input io.Reader, compressedCount *atomic.Int64) io.Reader {
	pr, pw := io.Pipe()
	g := &gzipStreamReader{
		pr:            pr,
		pw:            pw,
		compressedCnt: compressedCount,
		done:          make(chan struct{}),
	}

	go func() {
		defer close(g.done)
		defer func() { _ = pw.Close() }()

		countingPW := &countingWriter{w: pw, count: compressedCount}
		gzWriter := gzip.NewWriter(countingPW)

		_, err := io.Copy(gzWriter, input)
		if err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		if err := gzWriter.Close(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}()

	return pr
}

func executeTemplate(tmpl string, data *TemplateData) (string, error) {
	t, err := template.New("").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
