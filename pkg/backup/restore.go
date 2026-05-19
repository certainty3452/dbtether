package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/certainty3452/dbtether/pkg/storage"
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

	// Conflict handling: fail, drop, overwrite
	OnConflict string

	Logger *slog.Logger
}

// RunRestore executes the restore operation
func RunRestore(ctx context.Context, cfg *RestoreConfig) error {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	logger.Info("starting restore",
		"database", cfg.Database,
		"source", cfg.SourcePath,
		"onConflict", cfg.OnConflict,
	)

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

	// Handle conflict strategy
	switch cfg.OnConflict {
	case "drop":
		if err := dropAndRecreateDatabase(ctx, cfg, logger); err != nil {
			return fmt.Errorf("failed to drop/recreate database: %w", err)
		}
	case "fail":
		isEmpty, err := isDatabaseEmpty(ctx, cfg)
		if err != nil {
			return fmt.Errorf("failed to check if database is empty: %w", err)
		}
		if !isEmpty {
			return fmt.Errorf("database is not empty and onConflict=fail")
		}
	case "overwrite":
		// Just proceed with restore
	}

	// Restore using psql
	if err := restoreWithPsql(ctx, cfg, backupData, logger); err != nil {
		return fmt.Errorf("restore failed: %w", err)
	}

	logger.Info("restore completed successfully", "database", cfg.Database)
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
	return env
}

func dropAndRecreateDatabase(ctx context.Context, cfg *RestoreConfig, logger *slog.Logger) error {
	logger.Info("dropping and recreating database", "database", cfg.Database)

	dropConnsSQL := fmt.Sprintf(`
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE datname = %s AND pid <> pg_backend_pid()
	`, quoteLiteral(cfg.Database))

	base := psqlArgs(cfg, "postgres")

	//nolint:gosec // args are operator-controlled (CRD spec), password via env not argv
	cmd := exec.CommandContext(ctx, "psql", append(base, "-c", dropConnsSQL)...)
	cmd.Env = psqlEnv(cfg)
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("failed to terminate connections", "output", string(output))
	}

	dropSQL := fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdentifier(cfg.Database))
	//nolint:gosec // args are operator-controlled (CRD spec), password via env not argv
	cmd = exec.CommandContext(ctx, "psql", append(base, "-c", dropSQL)...)
	cmd.Env = psqlEnv(cfg)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to drop database: %s: %w", string(output), err)
	}

	createSQL := fmt.Sprintf("CREATE DATABASE %s", quoteIdentifier(cfg.Database))
	//nolint:gosec // args are operator-controlled (CRD spec), password via env not argv
	cmd = exec.CommandContext(ctx, "psql", append(base, "-c", createSQL)...)
	cmd.Env = psqlEnv(cfg)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create database: %s: %w", string(output), err)
	}

	logger.Info("database recreated", "database", cfg.Database)
	return nil
}

func isDatabaseEmpty(ctx context.Context, cfg *RestoreConfig) (bool, error) {
	sql := `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public'`

	args := append(psqlArgs(cfg, cfg.Database), "-t", "-c", sql)
	//nolint:gosec // args are operator-controlled (CRD spec), password via env not argv
	cmd := exec.CommandContext(ctx, "psql", args...)
	cmd.Env = psqlEnv(cfg)
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}

	count := strings.TrimSpace(string(output))
	return count == "0", nil
}

func restoreWithPsql(ctx context.Context, cfg *RestoreConfig, backupData io.ReadCloser, logger *slog.Logger) error {
	logger.Info("restoring database with psql", "database", cfg.Database)

	var reader io.Reader = backupData
	if strings.HasSuffix(cfg.SourcePath, ".gz") {
		gzReader, err := gzip.NewReader(backupData)
		if err != nil {
			return fmt.Errorf("failed to create gzip reader: %w", err)
		}
		defer func() {
			if closeErr := gzReader.Close(); closeErr != nil {
				logger.Warn("failed to close gzip reader", "error", closeErr)
			}
		}()
		reader = gzReader
	}

	//nolint:gosec // args are operator-controlled (CRD spec), password via env not argv
	cmd := exec.CommandContext(ctx, "psql", psqlArgs(cfg, cfg.Database)...)
	cmd.Stdin = reader
	cmd.Env = psqlEnv(cfg)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("psql failed: %s: %w", string(output), err)
	}

	logger.Info("restore completed", "database", cfg.Database)
	return nil
}

// Local copies — backup-job binary doesn't need to pull lib/pq just for two quoters.
func quoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
