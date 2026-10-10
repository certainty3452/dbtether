package storage

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"
)

type StorageObject struct {
	Key          string
	Size         int64
	LastModified time.Time
}

type StorageClient interface {
	Upload(ctx context.Context, key string, body io.Reader) error

	UploadWithTags(ctx context.Context, key string, body io.Reader, tags *ObjectTags) error

	Download(ctx context.Context, key string) (io.ReadCloser, error)

	Exists(ctx context.Context, key string) (bool, error)

	Delete(ctx context.Context, key string) error

	List(ctx context.Context, prefix string) ([]StorageObject, error)

	// Any error is surfaced to the user verbatim.
	Reachable(ctx context.Context) error
}

var (
	_ StorageClient = (*S3Client)(nil)
	_ StorageClient = (*GCSClient)(nil)
	_ StorageClient = (*AzureClient)(nil)
)

var ErrNoStorageBackend = errors.New("no storage backend configured")

// Pass exactly one non-nil config; with several, S3 wins.
func NewClient(ctx context.Context, s3 *S3Config, gcs *GCSConfig, azure *AzureConfig, logger *slog.Logger) (StorageClient, error) {
	switch {
	case s3 != nil:
		return NewS3Client(ctx, s3, logger)
	case gcs != nil:
		return NewGCSClient(ctx, gcs, logger)
	case azure != nil:
		return NewAzureClient(ctx, azure, logger)
	default:
		return nil, ErrNoStorageBackend
	}
}
