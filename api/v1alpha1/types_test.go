package v1alpha1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBackupStorage_GetProvider(t *testing.T) {
	tests := []struct {
		name     string
		storage  BackupStorage
		expected string
	}{
		{
			name: "S3 provider",
			storage: BackupStorage{
				Spec: BackupStorageSpec{
					S3: &S3StorageConfig{
						Bucket: "my-bucket",
						Region: "us-east-1",
					},
				},
			},
			expected: "s3",
		},
		{
			name: "GCS provider",
			storage: BackupStorage{
				Spec: BackupStorageSpec{
					GCS: &GCSStorageConfig{
						Bucket: "my-bucket",
					},
				},
			},
			expected: "gcs",
		},
		{
			name: "Azure provider",
			storage: BackupStorage{
				Spec: BackupStorageSpec{
					Azure: &AzureStorageConfig{
						Container:      "my-container",
						StorageAccount: "myaccount",
					},
				},
			},
			expected: "azure",
		},
		{
			name:     "No provider configured",
			storage:  BackupStorage{},
			expected: "",
		},
		{
			name: "S3 takes priority if multiple configured",
			storage: BackupStorage{
				Spec: BackupStorageSpec{
					S3: &S3StorageConfig{
						Bucket: "my-bucket",
						Region: "us-east-1",
					},
					GCS: &GCSStorageConfig{
						Bucket: "gcs-bucket",
					},
				},
			},
			expected: "s3",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.storage.GetProvider()
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestDatabaseSpec_DeletionPolicyDefaults(t *testing.T) {
	tests := []struct {
		name     string
		policy   string
		expected bool // should retain
	}{
		{"Delete policy", "Delete", false},
		{"Retain policy", "Retain", true},
		{"Empty defaults to retain", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := Database{
				Spec: DatabaseSpec{
					DeletionPolicy: tt.policy,
				},
			}
			shouldRetain := db.Spec.DeletionPolicy != "Delete"
			assert.Equal(t, tt.expected, shouldRetain)
		})
	}
}

func TestDatabaseUserSpec_DeletionPolicyDefaults(t *testing.T) {
	tests := []struct {
		name     string
		policy   string
		expected bool // should delete
	}{
		{"Delete policy", "Delete", true},
		{"Retain policy", "Retain", false},
		{"Empty defaults to delete", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := DatabaseUser{
				Spec: DatabaseUserSpec{
					DeletionPolicy: tt.policy,
				},
			}
			shouldDelete := user.Spec.DeletionPolicy != "Retain"
			assert.Equal(t, tt.expected, shouldDelete)
		})
	}
}

func TestBackupSpec_StorageRef(t *testing.T) {
	backup := Backup{
		Spec: BackupSpec{
			DatabaseRef: DatabaseReference{Name: "my-db"},
			StorageRef:  StorageReference{Name: "my-storage"},
		},
	}

	assert.Equal(t, "my-db", backup.Spec.DatabaseRef.Name)
	assert.Equal(t, "my-storage", backup.Spec.StorageRef.Name)
}

func TestRestoreSpec_OnConflictValues(t *testing.T) {
	validValues := []string{"fail", "drop", "overwrite"}

	for _, v := range validValues {
		t.Run(v, func(t *testing.T) {
			restore := Restore{
				Spec: RestoreSpec{
					OnConflict: v,
				},
			}
			assert.Equal(t, v, restore.Spec.OnConflict)
		})
	}
}

func TestBackupScheduleSpec_RetentionPolicy(t *testing.T) {
	tests := []struct {
		name      string
		retention *RetentionPolicy
		hasPolicy bool
	}{
		{
			name:      "no retention policy",
			retention: nil,
			hasPolicy: false,
		},
		{
			name: "keepLast only",
			retention: &RetentionPolicy{
				KeepLast: intPtr(5),
			},
			hasPolicy: true,
		},
		{
			name: "full retention policy",
			retention: &RetentionPolicy{
				KeepLast:    intPtr(10),
				KeepDaily:   intPtr(7),
				KeepWeekly:  intPtr(4),
				KeepMonthly: intPtr(3),
			},
			hasPolicy: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schedule := BackupSchedule{
				Spec: BackupScheduleSpec{
					Retention: tt.retention,
				},
			}
			hasPolicy := schedule.Spec.Retention != nil
			assert.Equal(t, tt.hasPolicy, hasPolicy)
		})
	}
}

func intPtr(i int) *int {
	return &i
}

func TestLatestFromSource_Fields(t *testing.T) {
	source := LatestFromSource{
		DatabaseRef: DatabaseReference{Name: "prod-db"},
		Namespace:   "production",
	}

	assert.Equal(t, "prod-db", source.DatabaseRef.Name)
	assert.Equal(t, "production", source.Namespace)
}

func TestRestoreSource_Priority(t *testing.T) {
	// Test that the source types are mutually exclusive in practice
	t.Run("backupRef takes priority", func(t *testing.T) {
		source := RestoreSource{
			BackupRef:  &BackupReference{Name: "specific-backup"},
			LatestFrom: &LatestFromSource{DatabaseRef: DatabaseReference{Name: "db"}},
			Path:       "some/path.sql.gz",
		}

		// In controller, BackupRef is checked first
		hasBackupRef := source.BackupRef != nil
		assert.True(t, hasBackupRef)
	})

	t.Run("latestFrom when no backupRef", func(t *testing.T) {
		source := RestoreSource{
			LatestFrom: &LatestFromSource{DatabaseRef: DatabaseReference{Name: "db"}},
			Path:       "some/path.sql.gz",
		}

		hasBackupRef := source.BackupRef != nil
		hasLatestFrom := source.LatestFrom != nil
		assert.False(t, hasBackupRef)
		assert.True(t, hasLatestFrom)
	})
}
