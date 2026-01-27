package backup

import (
	"testing"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

const testBucketName = "my-bucket"

func TestBackupStorageReconciler_ValidateStorage(t *testing.T) {
	r := &BackupStorageReconciler{}

	tests := []struct {
		name    string
		storage *databasesv1alpha1.BackupStorage
		wantErr bool
		errMsg  string
	}{
		{
			name: "valid S3 config",
			storage: &databasesv1alpha1.BackupStorage{
				Spec: databasesv1alpha1.BackupStorageSpec{
					S3: &databasesv1alpha1.S3StorageConfig{
						Bucket: testBucketName,
						Region: "eu-central-1",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "valid GCS config",
			storage: &databasesv1alpha1.BackupStorage{
				Spec: databasesv1alpha1.BackupStorageSpec{
					GCS: &databasesv1alpha1.GCSStorageConfig{
						Bucket:  testBucketName,
						Project: "my-project",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "valid Azure config",
			storage: &databasesv1alpha1.BackupStorage{
				Spec: databasesv1alpha1.BackupStorageSpec{
					Azure: &databasesv1alpha1.AzureStorageConfig{
						Container:      "my-container",
						StorageAccount: "mystorageaccount",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "no provider specified",
			storage: &databasesv1alpha1.BackupStorage{
				Spec: databasesv1alpha1.BackupStorageSpec{},
			},
			wantErr: true,
			errMsg:  "one of s3, gcs, or azure must be specified",
		},
		{
			name: "multiple providers specified",
			storage: &databasesv1alpha1.BackupStorage{
				Spec: databasesv1alpha1.BackupStorageSpec{
					S3: &databasesv1alpha1.S3StorageConfig{
						Bucket: testBucketName,
						Region: "eu-central-1",
					},
					GCS: &databasesv1alpha1.GCSStorageConfig{
						Bucket:  testBucketName,
						Project: "my-project",
					},
				},
			},
			wantErr: true,
			errMsg:  "only one of s3, gcs, or azure can be specified",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.validateStorage(tt.storage)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateStorage() expected error, got nil")
				} else if err.Error() != tt.errMsg {
					t.Errorf("validateStorage() error = %v, want %v", err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateStorage() unexpected error: %v", err)
				}
			}
		})
	}
}

func TestBackupStorage_GetProvider(t *testing.T) {
	tests := []struct {
		name     string
		storage  *databasesv1alpha1.BackupStorage
		expected string
	}{
		{
			name: "S3 provider",
			storage: &databasesv1alpha1.BackupStorage{
				Spec: databasesv1alpha1.BackupStorageSpec{
					S3: &databasesv1alpha1.S3StorageConfig{Bucket: "b", Region: "r"},
				},
			},
			expected: "s3",
		},
		{
			name: "GCS provider",
			storage: &databasesv1alpha1.BackupStorage{
				Spec: databasesv1alpha1.BackupStorageSpec{
					GCS: &databasesv1alpha1.GCSStorageConfig{Bucket: "b", Project: "p"},
				},
			},
			expected: "gcs",
		},
		{
			name: "Azure provider",
			storage: &databasesv1alpha1.BackupStorage{
				Spec: databasesv1alpha1.BackupStorageSpec{
					Azure: &databasesv1alpha1.AzureStorageConfig{Container: "c", StorageAccount: "s"},
				},
			},
			expected: "azure",
		},
		{
			name: "no provider",
			storage: &databasesv1alpha1.BackupStorage{
				Spec: databasesv1alpha1.BackupStorageSpec{},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.storage.GetProvider()
			if got != tt.expected {
				t.Errorf("GetProvider() = %v, want %v", got, tt.expected)
			}
		})
	}
}

// TestBackupStorageStatusChangeDetection verifies that status is only updated when meaningful changes occur
// This prevents unnecessary reconciliation loops caused by status patches
func TestBackupStorageStatusChangeDetection(t *testing.T) {
	tests := []struct {
		name          string
		currentStatus databasesv1alpha1.BackupStorageStatus
		generation    int64
		newPhase      string
		newMessage    string
		newProvider   string
		expectChanged bool
	}{
		{
			name: "no change - same phase, message, provider",
			currentStatus: databasesv1alpha1.BackupStorageStatus{
				Phase:              "Ready",
				Message:            "storage validated",
				Provider:           "s3",
				ObservedGeneration: 1,
			},
			generation:    1,
			newPhase:      "Ready",
			newMessage:    "storage validated",
			newProvider:   "s3",
			expectChanged: false,
		},
		{
			name: "phase changed",
			currentStatus: databasesv1alpha1.BackupStorageStatus{
				Phase:              "Ready",
				Message:            "storage validated",
				Provider:           "s3",
				ObservedGeneration: 1,
			},
			generation:    1,
			newPhase:      "Failed",
			newMessage:    "validation failed",
			newProvider:   "s3",
			expectChanged: true,
		},
		{
			name: "message changed",
			currentStatus: databasesv1alpha1.BackupStorageStatus{
				Phase:              "Failed",
				Message:            "old error",
				Provider:           "s3",
				ObservedGeneration: 1,
			},
			generation:    1,
			newPhase:      "Failed",
			newMessage:    "new error",
			newProvider:   "s3",
			expectChanged: true,
		},
		{
			name: "generation changed",
			currentStatus: databasesv1alpha1.BackupStorageStatus{
				Phase:              "Ready",
				Message:            "storage validated",
				Provider:           "s3",
				ObservedGeneration: 1,
			},
			generation:    2,
			newPhase:      "Ready",
			newMessage:    "storage validated",
			newProvider:   "s3",
			expectChanged: true,
		},
		{
			name: "provider changed",
			currentStatus: databasesv1alpha1.BackupStorageStatus{
				Phase:              "Ready",
				Message:            "storage validated",
				Provider:           "s3",
				ObservedGeneration: 1,
			},
			generation:    1,
			newPhase:      "Ready",
			newMessage:    "storage validated",
			newProvider:   "gcs",
			expectChanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the statusChanged check from updateStatus
			statusChanged := tt.currentStatus.Phase != tt.newPhase ||
				tt.currentStatus.Message != tt.newMessage ||
				tt.currentStatus.Provider != tt.newProvider ||
				tt.currentStatus.ObservedGeneration != tt.generation

			if statusChanged != tt.expectChanged {
				t.Errorf("statusChanged = %v, want %v", statusChanged, tt.expectChanged)
			}
		})
	}
}
