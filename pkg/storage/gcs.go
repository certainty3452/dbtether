package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	gcs "cloud.google.com/go/storage"
	"google.golang.org/api/iterator"
)

type GCSClient struct {
	client *gcs.Client
	bucket string
	logger *slog.Logger
}

type GCSConfig struct {
	Bucket  string
	Project string
}

func NewGCSClient(ctx context.Context, cfg *GCSConfig, logger *slog.Logger) (*GCSClient, error) {
	if logger == nil {
		logger = slog.Default()
	}

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

func (c *GCSClient) UploadWithTags(ctx context.Context, key string, data io.Reader, tags *ObjectTags) error {
	wc := c.client.Bucket(c.bucket).Object(key).NewWriter(ctx)

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

func (c *GCSClient) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := c.client.Bucket(c.bucket).Object(key).NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to download from GCS: %w", err)
	}
	return rc, nil
}

func (c *GCSClient) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.client.Bucket(c.bucket).Object(key).Attrs(ctx)
	if err != nil {
		if errors.Is(err, gcs.ErrObjectNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("failed to stat GCS object: %w", err)
	}
	return true, nil
}

func (c *GCSClient) Delete(ctx context.Context, key string) error {
	if err := c.client.Bucket(c.bucket).Object(key).Delete(ctx); err != nil && !errors.Is(err, gcs.ErrObjectNotExist) {
		return fmt.Errorf("failed to delete from GCS: %w", err)
	}
	return nil
}

// Missing bucket gets a dedicated error; other failures are auth/network and surface verbatim.
func (c *GCSClient) Reachable(ctx context.Context) error {
	_, err := c.client.Bucket(c.bucket).Attrs(ctx)
	if err != nil {
		if errors.Is(err, gcs.ErrBucketNotExist) {
			return fmt.Errorf("gcs bucket %q does not exist", c.bucket)
		}
		return fmt.Errorf("gcs bucket %q not reachable: %w", c.bucket, err)
	}
	return nil
}

func (c *GCSClient) List(ctx context.Context, prefix string) ([]StorageObject, error) {
	var objects []StorageObject

	it := c.client.Bucket(c.bucket).Objects(ctx, &gcs.Query{Prefix: prefix})
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to list GCS objects: %w", err)
		}

		objects = append(objects, StorageObject{
			Key:          attrs.Name,
			Size:         attrs.Size,
			LastModified: attrs.Updated,
		})
	}

	return objects, nil
}

func (c *GCSClient) Close() error {
	return c.client.Close()
}
