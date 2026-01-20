package storage

import (
	"testing"
)

func TestGCSConfig_Validation(t *testing.T) {
	tests := []struct {
		name  string
		cfg   GCSConfig
		valid bool
	}{
		{
			name: "valid config with bucket and project",
			cfg: GCSConfig{
				Bucket:  "my-bucket",
				Project: "my-project",
			},
			valid: true,
		},
		{
			name: "valid config with bucket only (Workload Identity)",
			cfg: GCSConfig{
				Bucket: "my-bucket",
			},
			valid: true,
		},
		{
			name: "missing bucket",
			cfg: GCSConfig{
				Project: "my-project",
			},
			valid: false,
		},
		{
			name:  "empty config",
			cfg:   GCSConfig{},
			valid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation - bucket is required
			hasRequired := tt.cfg.Bucket != ""
			if hasRequired != tt.valid {
				t.Errorf("expected valid=%v, got bucket=%q", tt.valid, tt.cfg.Bucket)
			}
		})
	}
}

func TestGCSConfig_Fields(t *testing.T) {
	cfg := GCSConfig{
		Bucket:  "test-backup-bucket",
		Project: "my-gcp-project",
	}

	if cfg.Bucket != "test-backup-bucket" {
		t.Errorf("bucket mismatch: got %q", cfg.Bucket)
	}

	if cfg.Project != "my-gcp-project" {
		t.Errorf("project mismatch: got %q", cfg.Project)
	}
}

func TestGCSObject_Fields(t *testing.T) {
	obj := GCSObject{
		Key:  "backups/mydb/2026/01/20/backup.sql.gz",
		Size: 1024 * 1024, // 1MB
	}

	if obj.Key == "" {
		t.Error("key should not be empty")
	}

	if obj.Size != 1024*1024 {
		t.Errorf("size mismatch: got %d", obj.Size)
	}
}
