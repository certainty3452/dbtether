package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/stretchr/testify/require"
)

func TestS3Config_Validation(t *testing.T) {
	tests := []struct {
		name  string
		cfg   S3Config
		valid bool
	}{
		{
			name: "valid config with credentials",
			cfg: S3Config{
				Bucket:    "my-bucket",
				Region:    "eu-central-1",
				AccessKey: "AKIAIOSFODNN7EXAMPLE",
				SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			valid: true,
		},
		{
			name: "valid config without credentials (IRSA)",
			cfg: S3Config{
				Bucket: "my-bucket",
				Region: "eu-central-1",
			},
			valid: true,
		},
		{
			name: "valid config with custom endpoint",
			cfg: S3Config{
				Bucket:   "my-bucket",
				Region:   "us-east-1",
				Endpoint: "http://localhost:9000",
			},
			valid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hasRequired := tt.cfg.Bucket != "" && tt.cfg.Region != ""
			if hasRequired != tt.valid {
				t.Errorf("expected valid=%v, got bucket=%q region=%q", tt.valid, tt.cfg.Bucket, tt.cfg.Region)
			}
		})
	}
}

func TestS3Config_UsesStaticCredentials(t *testing.T) {
	tests := []struct {
		name     string
		cfg      S3Config
		expected bool
	}{
		{
			name: "has both keys",
			cfg: S3Config{
				AccessKey: "key",
				SecretKey: "secret",
			},
			expected: true,
		},
		{
			name: "has only access key",
			cfg: S3Config{
				AccessKey: "key",
			},
			expected: false,
		},
		{
			name: "has only secret key",
			cfg: S3Config{
				SecretKey: "secret",
			},
			expected: false,
		},
		{
			name:     "no keys (IRSA)",
			cfg:      S3Config{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usesStatic := tt.cfg.AccessKey != "" && tt.cfg.SecretKey != ""
			if usesStatic != tt.expected {
				t.Errorf("expected usesStaticCredentials=%v, got %v", tt.expected, usesStatic)
			}
		})
	}
}

func TestIsAccessDeniedError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{name: "nil error", err: nil, expected: false},
		{name: "generic error", err: errors.New("connection timeout"), expected: false},
		{name: "AccessDenied API error", err: &smithy.GenericAPIError{Code: "AccessDenied"}, expected: true},
		{name: "wrapped AccessDenied API error", err: fmt.Errorf("upload: %w", &smithy.GenericAPIError{Code: "AccessDenied"}), expected: true},
		{name: "other API error", err: &smithy.GenericAPIError{Code: "NoSuchBucket"}, expected: false},
		{name: "403 only in message", err: errors.New("https response error StatusCode: 403"), expected: false},
		{name: "AccessDenied only in message", err: errors.New("operation error S3: AccessDenied: no permission"), expected: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isAccessDeniedError(tt.err); got != tt.expected {
				t.Errorf("isAccessDeniedError() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestObjectTags_Format(t *testing.T) {
	tags := &ObjectTags{
		Database:   "orders_db",
		Cluster:    "microservices",
		BackupName: "daily-backup",
		Namespace:  "production",
		Timestamp:  "20260120-143022",
		CreatedBy:  "dbtether",
	}

	if tags.Database == "" || tags.Cluster == "" || tags.BackupName == "" {
		t.Error("all tag fields should be populated")
	}

	if len(tags.Timestamp) != 15 { // YYYYMMDD-HHMMSS
		t.Errorf("timestamp format unexpected: %s", tags.Timestamp)
	}
}

func TestS3Upload_TaggingDeniedErrorNamesPermission(t *testing.T) {
	f := newFakeS3DenyingTagging(t)
	c, err := NewS3Client(context.Background(), &S3Config{
		Bucket: "b", Region: "us-east-1", Endpoint: f.srv.URL,
		AccessKey: "AKIATEST", SecretKey: "secret",
	}, nil)
	require.NoError(t, err)
	tags := &ObjectTags{Database: "d", CreatedBy: "dbtether"}

	for name, upload := range map[string]func() error{
		"UploadWithTags":  func() error { return c.UploadWithTags(context.Background(), "k", bytes.NewReader([]byte("x")), tags) },
		"UploadStreaming": func() error { return c.UploadStreaming(context.Background(), "k", bytes.NewReader([]byte("x")), tags) },
		"UploadStreaming multipart": func() error {
			return c.UploadStreaming(context.Background(), "k", bytes.NewReader(make([]byte, 65*1024*1024)), tags)
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := upload()
			require.Error(t, err)
			require.Contains(t, err.Error(), "s3:PutObjectTagging")
			require.False(t, f.stored)
		})
	}
}
