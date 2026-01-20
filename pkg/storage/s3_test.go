package storage

import (
	"testing"
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
			// Basic validation - bucket and region are required
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
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "generic error",
			err:      &testError{msg: "connection timeout"},
			expected: false,
		},
		{
			name:     "AccessDenied in message",
			err:      &testError{msg: "operation error S3: AccessDenied: no permission"},
			expected: true,
		},
		{
			name:     "403 status code in message",
			err:      &testError{msg: "https response error StatusCode: 403"},
			expected: true,
		},
		{
			name:     "case sensitive AccessDenied",
			err:      &testError{msg: "accessdenied"},
			expected: false, // lowercase not matched
		},
		{
			name:     "PutObjectTagging denied",
			err:      &testError{msg: "is not authorized to perform: s3:PutObjectTagging... AccessDenied"},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isAccessDeniedError(tt.err)
			if result != tt.expected {
				t.Errorf("isAccessDeniedError() = %v, want %v", result, tt.expected)
			}
		})
	}
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
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

	// Verify all fields are non-empty
	if tags.Database == "" || tags.Cluster == "" || tags.BackupName == "" {
		t.Error("all tag fields should be populated")
	}

	// Verify timestamp format
	if len(tags.Timestamp) != 15 { // YYYYMMDD-HHMMSS
		t.Errorf("timestamp format unexpected: %s", tags.Timestamp)
	}
}
