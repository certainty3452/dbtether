package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type S3Client struct {
	client *s3.Client
	bucket string
	logger *slog.Logger
}

type S3Config struct {
	Bucket    string
	Region    string
	Endpoint  string
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
	_, err := c.client.PutObject(ctx, c.putObjectInput(key, body, tags))
	return c.uploadError(key, tags, err)
}

func (c *S3Client) UploadStreaming(ctx context.Context, key string, body io.Reader, tags *ObjectTags) error {
	uploader := manager.NewUploader(c.client, func(u *manager.Uploader) {
		u.PartSize = 64 * 1024 * 1024
		u.Concurrency = 3
	})
	_, err := uploader.Upload(ctx, c.putObjectInput(key, body, tags))
	return c.uploadError(key, tags, err)
}

func (c *S3Client) putObjectInput(key string, body io.Reader, tags *ObjectTags) *s3.PutObjectInput {
	input := &s3.PutObjectInput{
		Bucket:      aws.String(c.bucket),
		Key:         aws.String(key),
		Body:        body,
		ContentType: aws.String("application/gzip"),
	}
	if tags != nil {
		input.Tagging = aws.String(fmt.Sprintf(
			"database=%s&cluster=%s&backup-name=%s&namespace=%s&timestamp=%s&created-by=%s",
			tags.Database,
			tags.Cluster,
			tags.BackupName,
			tags.Namespace,
			tags.Timestamp,
			tags.CreatedBy,
		))
	}
	return input
}

// The body is a one-shot stream, so a denied tagged upload cannot be retried without tags.
func (c *S3Client) uploadError(key string, tags *ObjectTags, err error) error {
	if err == nil {
		return nil
	}
	if tags != nil && isAccessDeniedError(err) {
		return fmt.Errorf("failed to upload s3://%s/%s: access denied for a tagged upload, the IAM policy needs s3:PutObjectTagging in addition to s3:PutObject: %w", c.bucket, key, err)
	}
	return fmt.Errorf("failed to upload to S3: %w", err)
}

func isAccessDeniedError(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "AccessDenied"
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
		return false, nil
	}
	return true, nil
}

func (c *S3Client) Delete(ctx context.Context, key string) error {
	_, err := c.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(c.bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isMissingKeyError(err) {
		return fmt.Errorf("failed to delete from S3: %w", err)
	}
	return nil
}

// AWS returns 204 for a missing key; some S3-compatible stores answer NoSuchKey.
// A bodiless 404 (NotFound) may be a proxy or route problem, so it stays an error.
func isMissingKeyError(err error) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey"
}

// Needs only s3:ListBucket on the bucket.
func (c *S3Client) Reachable(ctx context.Context) error {
	_, err := c.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(c.bucket),
	})
	if err != nil {
		return fmt.Errorf("s3 bucket %q not reachable: %w", c.bucket, err)
	}
	return nil
}

func (c *S3Client) List(ctx context.Context, prefix string) ([]StorageObject, error) {
	var objects []StorageObject

	paginator := s3.NewListObjectsV2Paginator(c.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(c.bucket),
		Prefix: aws.String(prefix),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list S3 objects: %w", err)
		}

		for _, obj := range page.Contents {
			lastMod := time.Time{}
			if obj.LastModified != nil {
				lastMod = *obj.LastModified
			}
			objects = append(objects, StorageObject{
				Key:          aws.ToString(obj.Key),
				Size:         aws.ToInt64(obj.Size),
				LastModified: lastMod,
			})
		}
	}

	return objects, nil
}
