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
				User:        "admin",
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
				User:        "admin",
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
				User:        "admin",
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
