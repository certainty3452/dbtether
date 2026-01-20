package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

	// Generate path and filename from templates
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

	// Run pg_dump
	dumpData, err := runPgDump(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg_dump failed: %w", err)
	}
	uncompressedSize := int64(len(dumpData))

	// Compress with gzip
	var compressed bytes.Buffer
	gzWriter := gzip.NewWriter(&compressed)
	if _, err := gzWriter.Write(dumpData); err != nil {
		return nil, fmt.Errorf("gzip compression failed: %w", err)
	}
	if err := gzWriter.Close(); err != nil {
		return nil, fmt.Errorf("gzip close failed: %w", err)
	}
	compressedSize := int64(compressed.Len())

	// Build tags for object metadata
	tags := &storage.ObjectTags{
		Database:   cfg.DatabaseName,
		Cluster:    cfg.ClusterName,
		BackupName: cfg.BackupName,
		Namespace:  cfg.Namespace,
		Timestamp:  data.Timestamp,
		CreatedBy:  "dbtether",
	}

	// Upload to storage
	switch cfg.StorageType {
	case "s3":
		s3Client, err := storage.NewS3Client(ctx, &cfg.S3Config, nil) // nil logger = use slog.Default()
		if err != nil {
			return nil, fmt.Errorf("failed to create S3 client: %w", err)
		}
		if err := s3Client.UploadWithTags(ctx, fullPath, bytes.NewReader(compressed.Bytes()), tags); err != nil {
			return nil, fmt.Errorf("S3 upload failed: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported storage type: %s", cfg.StorageType)
	}

	return &BackupResult{
		Path:             fullPath,
		Size:             compressedSize,
		UncompressedSize: uncompressedSize,
		Duration:         time.Since(startTime),
	}, nil
}

func runPgDump(ctx context.Context, cfg *BackupConfig) ([]byte, error) {
	// Use separate arguments instead of connection string for security
	// Each argument is isolated and properly escaped by exec.CommandContext
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

	// Password via environment variable (standard PostgreSQL approach)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+cfg.Password)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pg_dump error: %s, stderr: %s", err, stderr.String())
	}

	return stdout.Bytes(), nil
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
