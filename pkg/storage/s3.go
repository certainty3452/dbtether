package storage

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type S3Client struct {
	client *s3.Client
	bucket string
	logger *slog.Logger
}

type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string // optional, for custom endpoints
	AccessKey string // optional, empty = use IRSA/Pod Identity
	SecretKey string
}

func NewS3Client(ctx context.Context, cfg *S3Config, logger *slog.Logger) (*S3Client, error) {
	var awsCfg aws.Config
	var err error

	if logger == nil {
		logger = slog.Default()
	}

	opts := []func(*config.LoadOptions) error{
		config.WithRegion(cfg.Region),
	}

	// Use explicit credentials if provided, otherwise use default chain (IRSA/Pod Identity)
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awsCfg, err = config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	clientOpts := []func(*s3.Options){}
	if cfg.Endpoint != "" {
		clientOpts = append(clientOpts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, clientOpts...)

	return &S3Client{
		client: client,
		bucket: cfg.Bucket,
		logger: logger,
	}, nil
}

// ObjectTags contains metadata tags for uploaded objects
type ObjectTags struct {
	Database   string
	Cluster    string
	BackupName string
	Namespace  string
	Timestamp  string
	CreatedBy  string
}

func (c *S3Client) Upload(ctx context.Context, key string, body io.Reader) error {
	return c.UploadWithTags(ctx, key, body, nil)
}

func (c *S3Client) UploadWithTags(ctx context.Context, key string, body io.Reader, tags *ObjectTags) error {
	input := &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String("application/gzip"),
	}

	if tags != nil {
		// Build tagging string: Key1=Value1&Key2=Value2
		tagging := fmt.Sprintf(
			"database=%s&cluster=%s&backup-name=%s&namespace=%s&timestamp=%s&created-by=%s",
			tags.Database,
			tags.Cluster,
			tags.BackupName,
			tags.Namespace,
			tags.Timestamp,
			tags.CreatedBy,
		)
		input.Tagging = aws.String(tagging)
	}

	_, err := c.client.PutObject(ctx, input)
	if err != nil {
		// Best-effort: if tagging fails (403 AccessDenied on PutObjectTagging), retry without tags
		if tags != nil && isAccessDeniedError(err) {
			c.logger.Warn("S3 tagging permission denied, uploading without tags (add s3:PutObjectTagging to IAM policy)",
				"bucket", c.bucket,
				"key", key,
			)
			input.Tagging = nil
			_, retryErr := c.client.PutObject(ctx, input)
			if retryErr != nil {
				return fmt.Errorf("failed to upload to S3: %w", retryErr)
			}
			return nil // uploaded without tags
		}
		return fmt.Errorf("failed to upload to S3: %w", err)
	}
	return nil
}

// isAccessDeniedError checks if the error is an S3 AccessDenied error
func isAccessDeniedError(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	// Check for AccessDenied in error string (covers various AWS SDK error types)
	return strings.Contains(errStr, "AccessDenied") || strings.Contains(errStr, "403")
}

func (c *S3Client) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	result, err := c.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download from S3: %w", err)
	}
	return result.Body, nil
}

func (c *S3Client) Exists(ctx context.Context, key string) (bool, error) {
	_, err := c.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// Check if it's a "not found" error
		return false, nil
	}
	return true, nil
}

func (c *S3Client) Delete(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete from S3: %w", err)
	}
	return nil
}
