package storage

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	gcs "cloud.google.com/go/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
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

func TestGCSDelete_MissingObjectIsSuccess(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "204", status: http.StatusNoContent},
		{name: "404", status: http.StatusNotFound},
		{name: "403", status: http.StatusForbidden, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, http.MethodDelete, r.Method)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				if tt.status >= 400 {
					_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"x"}}`, tt.status)
				}
			}))
			t.Cleanup(srv.Close)
			client, err := gcs.NewClient(context.Background(),
				option.WithEndpoint(srv.URL+"/storage/v1/"), option.WithoutAuthentication())
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			c := &GCSClient{client: client, bucket: "b"}

			err = c.Delete(context.Background(), "cluster/db/run.sql.gz")
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
