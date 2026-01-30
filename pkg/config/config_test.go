package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name        string
		content     string
		wantErr     bool
		checkConfig func(*testing.T, *Config)
	}{
		{
			name: "full config",
			content: `
backup:
  maxConcurrentPerCluster: 5
  podAnnotations:
    karpenter.sh/do-not-disrupt: "true"
  podLabels:
    app.kubernetes.io/name: dbtether-backup
  jobLabels:
    app.kubernetes.io/managed-by: dbtether
`,
			wantErr: false,
			checkConfig: func(t *testing.T, cfg *Config) {
				if cfg.Backup.MaxConcurrentPerCluster != 5 {
					t.Errorf("MaxConcurrentPerCluster = %d, want 5", cfg.Backup.MaxConcurrentPerCluster)
				}
				if cfg.Backup.PodAnnotations["karpenter.sh/do-not-disrupt"] != "true" {
					t.Error("PodAnnotations missing karpenter annotation")
				}
				if cfg.Backup.PodLabels["app.kubernetes.io/name"] != "dbtether-backup" {
					t.Error("PodLabels missing app.kubernetes.io/name")
				}
				if cfg.Backup.JobLabels["app.kubernetes.io/managed-by"] != "dbtether" {
					t.Error("JobLabels missing app.kubernetes.io/managed-by")
				}
			},
		},
		{
			name: "minimal config",
			content: `
backup:
  maxConcurrentPerCluster: 2
`,
			wantErr: false,
			checkConfig: func(t *testing.T, cfg *Config) {
				if cfg.Backup.MaxConcurrentPerCluster != 2 {
					t.Errorf("MaxConcurrentPerCluster = %d, want 2", cfg.Backup.MaxConcurrentPerCluster)
				}
				if len(cfg.Backup.PodAnnotations) > 0 {
					t.Error("PodAnnotations should be empty")
				}
			},
		},
		{
			name:    "empty config uses defaults",
			content: "",
			wantErr: false,
			checkConfig: func(t *testing.T, cfg *Config) {
				if cfg.Backup.MaxConcurrentPerCluster != 3 {
					t.Errorf("MaxConcurrentPerCluster = %d, want default 3", cfg.Backup.MaxConcurrentPerCluster)
				}
			},
		},
		{
			name:    "invalid yaml",
			content: "backup:\n  maxConcurrent: [invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			configPath := filepath.Join(tmpDir, "config.yaml")

			if err := os.WriteFile(configPath, []byte(tt.content), 0o644); err != nil {
				t.Fatalf("Failed to write test config: %v", err)
			}

			cfg, err := Load(configPath)
			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && tt.checkConfig != nil {
				tt.checkConfig(t, cfg)
			}
		})
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Error("Load() should return error for non-existent file")
	}
}

func TestLoadOrDefault_FileNotFound(t *testing.T) {
	cfg := LoadOrDefault("/nonexistent/path/config.yaml")
	if cfg == nil {
		t.Fatal("LoadOrDefault() should return default config, not nil")
	}
	if cfg.Backup.MaxConcurrentPerCluster != 3 {
		t.Errorf("MaxConcurrentPerCluster = %d, want default 3", cfg.Backup.MaxConcurrentPerCluster)
	}
}

func TestLoadOrDefault_EmptyPath(t *testing.T) {
	cfg := LoadOrDefault("")
	if cfg == nil {
		t.Fatal("LoadOrDefault(\"\") should return default config")
	}
	if cfg.Backup.MaxConcurrentPerCluster != 3 {
		t.Errorf("MaxConcurrentPerCluster = %d, want default 3", cfg.Backup.MaxConcurrentPerCluster)
	}
}

func TestLoadOrDefault_ValidFile(t *testing.T) {
	tmpDir := t.TempDir()
	configPath := filepath.Join(tmpDir, "config.yaml")

	content := `
backup:
  maxConcurrentPerCluster: 10
  podAnnotations:
    test-key: test-value
`
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("Failed to write test config: %v", err)
	}

	cfg := LoadOrDefault(configPath)
	if cfg.Backup.MaxConcurrentPerCluster != 10 {
		t.Errorf("MaxConcurrentPerCluster = %d, want 10", cfg.Backup.MaxConcurrentPerCluster)
	}
	if cfg.Backup.PodAnnotations["test-key"] != "test-value" {
		t.Error("PodAnnotations missing test-key")
	}
}
