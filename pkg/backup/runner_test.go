package backup

import (
	"strings"
	"testing"
)

func TestExecuteTemplate(t *testing.T) {
	data := &TemplateData{
		ClusterName:  "microservices",
		DatabaseName: "orders_db",
		Year:         "2026",
		Month:        "01",
		Day:          "19",
		Timestamp:    "20260119-143022",
		RunID:        "abc12345",
	}

	tests := []struct {
		name     string
		template string
		expected string
		wantErr  bool
	}{
		{
			name:     "path template with cluster and database",
			template: "{{ .ClusterName }}/{{ .DatabaseName }}",
			expected: "microservices/orders_db",
		},
		{
			name:     "path template with date",
			template: "{{ .ClusterName }}/{{ .Year }}-{{ .Month }}-{{ .Day }}",
			expected: "microservices/2026-01-19",
		},
		{
			name:     "filename template with timestamp",
			template: "{{ .Timestamp }}.sql.gz",
			expected: "20260119-143022.sql.gz",
		},
		{
			name:     "filename template with RunID",
			template: "{{ .DatabaseName }}_{{ .Timestamp }}_{{ .RunID }}.sql.gz",
			expected: "orders_db_20260119-143022_abc12345.sql.gz",
		},
		{
			name:     "invalid template",
			template: "{{ .InvalidField }}",
			wantErr:  true,
		},
		{
			name:     "malformed template",
			template: "{{ .ClusterName",
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := executeTemplate(tt.template, data)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestBackupConfig_Validation(t *testing.T) {
	tests := []struct {
		name    string
		cfg     BackupConfig
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid S3 config",
			cfg: BackupConfig{
				Host:        "localhost",
				Port:        5432,
				Database:    "testdb",
				Username:    "user",
				Password:    "pass",
				StorageType: "s3",
				ClusterName: "test",
			},
			wantErr: false,
		},
		{
			name: "missing host",
			cfg: BackupConfig{
				Port:     5432,
				Database: "testdb",
			},
			wantErr: true,
			errMsg:  "host",
		},
		{
			name: "unsupported storage type",
			cfg: BackupConfig{
				Host:        "localhost",
				Port:        5432,
				Database:    "testdb",
				StorageType: "ftp",
			},
			wantErr: true,
			errMsg:  "storage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateConfig(&tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error containing %q, got nil", tt.errMsg)
				} else if !strings.Contains(strings.ToLower(err.Error()), tt.errMsg) {
					t.Errorf("expected error containing %q, got %v", tt.errMsg, err)
				}
			} else if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func validateConfig(cfg *BackupConfig) error {
	if cfg.Host == "" {
		return &validationError{field: "host", msg: "host is required"}
	}
	if cfg.Database == "" {
		return &validationError{field: "database", msg: "database is required"}
	}
	if cfg.StorageType != "" && cfg.StorageType != "s3" && cfg.StorageType != "gcs" && cfg.StorageType != "azure" {
		return &validationError{field: "storage", msg: "unsupported storage type"}
	}
	return nil
}

type validationError struct {
	field string
	msg   string
}

func (e *validationError) Error() string {
	return e.field + ": " + e.msg
}
