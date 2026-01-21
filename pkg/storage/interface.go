package storage

import (
	"context"
	"io"
	"time"
)

// StorageObject represents a generic storage object
type StorageObject struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// StorageClient is the interface for cloud storage operations
type StorageClient interface {
	// Upload uploads data to the given key
	Upload(ctx context.Context, key string, body io.Reader) error

	// UploadWithTags uploads data with metadata tags
	UploadWithTags(ctx context.Context, key string, body io.Reader, tags *ObjectTags) error

	// Download downloads data from the given key
	Download(ctx context.Context, key string) (io.ReadCloser, error)

	// Exists checks if the key exists
	Exists(ctx context.Context, key string) (bool, error)

	// Delete deletes the object at the given key
	Delete(ctx context.Context, key string) error

	// List lists all objects with the given prefix
	List(ctx context.Context, prefix string) ([]StorageObject, error)
}

// Verify implementations satisfy the interface
var (
	_ StorageClient = (*S3Client)(nil)
	// _ StorageClient = (*GCSClient)(nil)   // TODO: update GCS to match interface
	// _ StorageClient = (*AzureClient)(nil) // TODO: update Azure to match interface
)
