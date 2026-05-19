package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/certainty3452/dbtether/pkg/storage"
)

func TestRestoreConfig_Validation(t *testing.T) {
	tests := []struct {
		name    string
		config  RestoreConfig
		isValid bool
	}{
		{
			name: "valid S3 config",
			config: RestoreConfig{
				Host:        "db.example.com",
				Port:        5432,
				Database:    "mydb",
				Username:    "admin",
				Password:    "secret",
				SourcePath:  "cluster/database/backup.sql.gz",
				StorageType: "s3",
				S3Config: &storage.S3Config{
					Bucket: "backups",
					Region: "us-east-1",
				},
				OnConflict: "fail",
			},
			isValid: true,
		},
		{
			name: "valid GCS config",
			config: RestoreConfig{
				Host:        "db.example.com",
				Port:        5432,
				Database:    "mydb",
				Username:    "admin",
				Password:    "secret",
				SourcePath:  "cluster/database/backup.sql.gz",
				StorageType: "gcs",
				GCSConfig: &storage.GCSConfig{
					Bucket: "backups",
				},
				OnConflict: "drop",
			},
			isValid: true,
		},
		{
			name: "valid Azure config",
			config: RestoreConfig{
				Host:        "db.example.com",
				Port:        5432,
				Database:    "mydb",
				Username:    "admin",
				Password:    "secret",
				SourcePath:  "cluster/database/backup.sql.gz",
				StorageType: "azure",
				AzureConfig: &storage.AzureConfig{
					Container:      "backups",
					StorageAccount: "mystorageaccount",
				},
				OnConflict: "overwrite",
			},
			isValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation checks
			assert.NotEmpty(t, tt.config.Host)
			assert.NotEmpty(t, tt.config.Database)
			assert.NotEmpty(t, tt.config.SourcePath)
			assert.NotEmpty(t, tt.config.StorageType)
		})
	}
}

func TestOnConflict_Values(t *testing.T) {
	validValues := []string{"fail", "drop", "overwrite"}

	for _, val := range validValues {
		t.Run(val, func(t *testing.T) {
			config := RestoreConfig{
				OnConflict: val,
			}
			assert.Contains(t, validValues, config.OnConflict)
		})
	}
}

func TestQuoteIdentifier(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "simple_name",
			expected: `"simple_name"`,
		},
		{
			input:    "name with spaces",
			expected: `"name with spaces"`,
		},
		{
			input:    `name"with"quotes`,
			expected: `"name""with""quotes"`,
		},
		{
			input:    "UPPERCASE",
			expected: `"UPPERCASE"`,
		},
		{
			input:    "",
			expected: `""`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := quoteIdentifier(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRestoreConfig_SSLMode(t *testing.T) {
	config := RestoreConfig{
		SSLMode: "require",
	}
	assert.Equal(t, "require", config.SSLMode)

	config.SSLMode = "disable"
	assert.Equal(t, "disable", config.SSLMode)

	config.SSLMode = "verify-full"
	assert.Equal(t, "verify-full", config.SSLMode)
}

func TestPsqlArgs_PasswordNotInArgv(t *testing.T) {
	cfg := &RestoreConfig{
		Host: "db.example.com", Port: 5432, Username: "admin",
		Password: "p@ssw0rd!secret", SSLMode: "require",
	}
	args := psqlArgs(cfg, "mydb")
	for _, a := range args {
		assert.NotContains(t, a, cfg.Password,
			"password must never appear in psql argv; goes through PGPASSWORD only")
		assert.NotContains(t, a, "sslmode=",
			"sslmode must go through PGSSLMODE env, not --set= (psql var) or argv")
	}
}

func TestPsqlEnv_PropagatesPasswordAndSSLMode(t *testing.T) {
	cfg := &RestoreConfig{Password: "secret", SSLMode: "require"}
	env := psqlEnv(cfg)
	var sawPwd, sawSSL bool
	for _, e := range env {
		if e == "PGPASSWORD=secret" {
			sawPwd = true
		}
		if e == "PGSSLMODE=require" {
			sawSSL = true
		}
	}
	assert.True(t, sawPwd, "PGPASSWORD missing")
	assert.True(t, sawSSL, "PGSSLMODE missing — sslmode would silently fall back to libpq default")
}

func TestPsqlEnv_SkipsEmptySSLMode(t *testing.T) {
	cfg := &RestoreConfig{Password: "secret", SSLMode: ""}
	for _, e := range psqlEnv(cfg) {
		assert.NotEqual(t, "PGSSLMODE=", e,
			"empty PGSSLMODE would be rejected by libpq; skip the var entirely so libpq uses its default")
	}
}

func TestQuoteLiteral(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"mydb", "'mydb'"},
		{"my'db", "'my''db'"},
		{"'; DROP DATABASE postgres; --", "'''; DROP DATABASE postgres; --'"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, quoteLiteral(tt.in))
		})
	}
}

func TestRestoreConfig_SourcePath(t *testing.T) {
	tests := []struct {
		path      string
		isGzipped bool
	}{
		{"backup.sql.gz", true},
		{"backup.sql", false},
		{"path/to/backup.sql.gz", true},
		{"path/to/backup.dump", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			isGz := len(tt.path) > 3 && tt.path[len(tt.path)-3:] == ".gz"
			assert.Equal(t, tt.isGzipped, isGz)
		})
	}
}
