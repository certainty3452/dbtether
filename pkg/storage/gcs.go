package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

// GCSClient provides Google Cloud Storage operations
type GCSClient struct {
	client *gcs.Client
	bucket string
	logger *slog.Logger
}

// GCSConfig contains configuration for GCS client
type GCSConfig struct {
	Bucket  string
	Project string
	// Credentials via GOOGLE_APPLICATION_CREDENTIALS env var or Workload Identity
}

// NewGCSClient creates a new GCS client
func NewGCSClient(ctx context.Context, cfg *GCSConfig, logger *slog.Logger) (*GCSClient, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// Uses Application Default Credentials (ADC):
	// - Workload Identity on GKE
	// - GOOGLE_APPLICATION_CREDENTIALS env var
	// - gcloud auth application-default login (local dev)
	client, err := gcs.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCS client: %w", err)
	}

	return &GCSClient{
		client: client,
		bucket: cfg.Bucket,
		logger: logger,
	}, nil
}

// Upload uploads data to GCS
func (c *GCSClient) Upload(ctx context.Context, key string, data io.Reader) error {
	wc := c.client.Bucket(c.bucket).Object(key).NewWriter(ctx)
	if _, err := io.Copy(wc, data); err != nil {
		return fmt.Errorf("failed to upload to GCS: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("failed to close GCS writer: %w", err)
	}
	return nil
}

// UploadWithTags uploads data with metadata labels
func (c *GCSClient) UploadWithTags(ctx context.Context, key string, data io.Reader, tags *ObjectTags) error {
	wc := c.client.Bucket(c.bucket).Object(key).NewWriter(ctx)

	// Set metadata (GCS equivalent of tags)
	if tags != nil {
		wc.Metadata = map[string]string{
			"database":    tags.Database,
			"cluster":     tags.Cluster,
			"backup-name": tags.BackupName,
			"namespace":   tags.Namespace,
			"timestamp":   tags.Timestamp,
			"created-by":  tags.CreatedBy,
		}
	}

	if _, err := io.Copy(wc, data); err != nil {
		return fmt.Errorf("failed to upload to GCS: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("failed to close GCS writer: %w", err)
	}
	return nil
}

// Download downloads data from GCS
func (c *GCSClient) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := c.client.Bucket(c.bucket).Object(key).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to download from GCS: %w", err)
	}
	return rc, nil
}

// Delete deletes an object from GCS
func (c *GCSClient) Delete(ctx context.Context, key string) error {
	if err := c.client.Bucket(c.bucket).Object(key).Delete(ctx); err != nil {
		return fmt.Errorf("failed to delete from GCS: %w", err)
	}
	return nil
}

// GCSObject represents an object in GCS
type GCSObject struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// List lists all objects with the given prefix
func (c *GCSClient) List(ctx context.Context, prefix string) ([]GCSObject, error) {
	var objects []GCSObject

	it := c.client.Bucket(c.bucket).Objects(ctx, &gcs.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list GCS objects: %w", err)
		}

		objects = append(objects, GCSObject{
			Key:          attrs.Name,
			Size:         attrs.Size,
			LastModified: attrs.Updated,
		})
	}

	return objects, nil
}

// Close closes the GCS client
func (c *GCSClient) Close() error {
	return c.client.Close()
}
