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
	User     string
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

func dropAndRecreateDatabase(ctx context.Context, cfg *RestoreConfig, logger *slog.Logger) error {
	logger.Info("dropping and recreating database", "database", cfg.Database)

	// Connect to postgres database to drop/create target database
	connStr := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=postgres sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.SSLMode,
	)

	// Drop existing connections
	dropConnsSQL := fmt.Sprintf(`
		SELECT pg_terminate_backend(pid)
		FROM pg_stat_activity
		WHERE datname = '%s' AND pid <> pg_backend_pid()
	`, cfg.Database)

	cmd := exec.CommandContext(ctx, "psql", connStr, "-c", dropConnsSQL) //nolint:gosec // intentional variable-based command
	cmd.Env = append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", cfg.Password))
	if output, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("failed to terminate connections", "output", string(output))
	}

	// Drop database
	dropSQL := fmt.Sprintf("DROP DATABASE IF EXISTS %s", quoteIdentifier(cfg.Database))
	cmd = exec.CommandContext(ctx, "psql", connStr, "-c", dropSQL) //nolint:gosec // intentional variable-based command
	cmd.Env = append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", cfg.Password))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to drop database: %s: %w", string(output), err)
	}

	// Create database
	createSQL := fmt.Sprintf("CREATE DATABASE %s", quoteIdentifier(cfg.Database))
	cmd = exec.CommandContext(ctx, "psql", connStr, "-c", createSQL) //nolint:gosec // intentional variable-based command
	cmd.Env = append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", cfg.Password))
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to create database: %s: %w", string(output), err)
	}

	logger.Info("database recreated", "database", cfg.Database)
	return nil
}

func isDatabaseEmpty(ctx context.Context, cfg *RestoreConfig) (bool, error) {
	connStr := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode,
	)

	// Count tables in public schema
	sql := `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public'`

	cmd := exec.CommandContext(ctx, "psql", connStr, "-t", "-c", sql) //nolint:gosec // intentional variable-based command
	cmd.Env = append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", cfg.Password))
	output, err := cmd.Output()
	if err != nil {
		return false, err
	}

	count := strings.TrimSpace(string(output))
	return count == "0", nil
}

func restoreWithPsql(ctx context.Context, cfg *RestoreConfig, backupData io.ReadCloser, logger *slog.Logger) error {
	logger.Info("restoring database with psql", "database", cfg.Database)

	connStr := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode,
	)

	// Decompress if gzipped
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

	cmd := exec.CommandContext(ctx, "psql", connStr) //nolint:gosec // intentional variable-based command
	cmd.Stdin = reader
	cmd.Env = append(os.Environ(), fmt.Sprintf("PGPASSWORD=%s", cfg.Password))

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("psql failed: %s: %w", string(output), err)
	}

	logger.Info("restore completed", "database", cfg.Database)
	return nil
}

func quoteIdentifier(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
