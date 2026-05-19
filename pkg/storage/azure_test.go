package storage

import (
	"testing"
)

func TestAzureConfig_Validation(t *testing.T) {
	tests := []struct {
		name  string
		cfg   AzureConfig
		valid bool
	}{
		{
			name: "valid config with container and storage account",
			cfg: AzureConfig{
				Container:      "backups",
				StorageAccount: "mystorageaccount",
			},
			valid: true,
		},
		{
			name: "missing container",
			cfg: AzureConfig{
				StorageAccount: "mystorageaccount",
			},
			valid: false,
		},
		{
			name: "missing storage account",
			cfg: AzureConfig{
				Container: "backups",
			},
			valid: false,
		},
		{
			name:  "empty config",
			cfg:   AzureConfig{},
			valid: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation - both container and storage account are required
			hasRequired := tt.cfg.Container != "" && tt.cfg.StorageAccount != ""
			if hasRequired != tt.valid {
				t.Errorf("expected valid=%v, got container=%q storageAccount=%q",
					tt.valid, tt.cfg.Container, tt.cfg.StorageAccount)
			}
		})
	}
}

func TestAzureConfig_Fields(t *testing.T) {
	cfg := AzureConfig{
		Container:      "db-backups",
		StorageAccount: "prodstorageaccount",
	}

	if cfg.Container != "db-backups" {
		t.Errorf("container mismatch: got %q", cfg.Container)
	}

	if cfg.StorageAccount != "prodstorageaccount" {
		t.Errorf("storage account mismatch: got %q", cfg.StorageAccount)
	}
}

func TestStrPtr(t *testing.T) {
	input := "test-value"
	result := strPtr(input)

	if result == nil {
		t.Fatal("strPtr returned nil")
	}

	if *result != input {
		t.Errorf("strPtr value mismatch: got %q, want %q", *result, input)
	}
}
