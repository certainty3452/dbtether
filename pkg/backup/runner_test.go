package backup

import (
	"slices"
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

func TestPgDumpEnv_PropagatesPasswordAndSSLMode(t *testing.T) {
	var sawPassword, sawSSLMode bool
	for _, entry := range pgDumpEnv(&BackupConfig{Password: "secret", SSLMode: "require"}) {
		switch entry {
		case "PGPASSWORD=secret":
			sawPassword = true
		case "PGSSLMODE=require":
			sawSSLMode = true
		}
	}

	if !sawPassword {
		t.Error("PGPASSWORD missing")
	}
	if !sawSSLMode {
		t.Error("PGSSLMODE missing — pg_dump would silently fall back to the libpq default")
	}
}

func TestPgDumpEnv_SetsAConnectTimeout(t *testing.T) {
	if !slices.Contains(pgDumpEnv(&BackupConfig{Password: "secret"}), "PGCONNECT_TIMEOUT=30") {
		t.Error("PGCONNECT_TIMEOUT missing — a hung TCP connect keeps the Job running until its deadline")
	}
}

func TestPgDumpEnv_KeepsAConnectTimeoutFromTheEnvironment(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "5")

	env := pgDumpEnv(&BackupConfig{Password: "secret"})

	if !slices.Contains(env, "PGCONNECT_TIMEOUT=5") || slices.Contains(env, "PGCONNECT_TIMEOUT=30") {
		t.Errorf("an explicit PGCONNECT_TIMEOUT must win, got %v", env)
	}
}

func TestPgDumpEnv_SkipsEmptySSLMode(t *testing.T) {
	for _, entry := range pgDumpEnv(&BackupConfig{Password: "secret"}) {
		if entry == "PGSSLMODE=" {
			t.Error("an empty PGSSLMODE is rejected by libpq; the variable has to stay unset")
		}
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
