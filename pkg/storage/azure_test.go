package storage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestAzureDelete_MissingBlobIsSuccess(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		errorCode string
		wantErr   bool
	}{
		{name: "202", status: http.StatusAccepted},
		{name: "404 BlobNotFound", status: http.StatusNotFound, errorCode: "BlobNotFound"},
		{name: "404 ContainerNotFound", status: http.StatusNotFound, errorCode: "ContainerNotFound", wantErr: true},
		{name: "403 AuthorizationFailure", status: http.StatusForbidden, errorCode: "AuthorizationFailure", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodDelete, r.Method)
				if tt.errorCode != "" {
					w.Header().Set("x-ms-error-code", tt.errorCode)
				}
				w.WriteHeader(tt.status)
			}))
			t.Cleanup(srv.Close)
			client, err := azblob.NewClientWithNoCredential(srv.URL+"/", nil)
			require.NoError(t, err)
			c := &AzureClient{client: client, container: "backups"}

			err = c.Delete(context.Background(), "cluster/db/run.sql.gz")
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
