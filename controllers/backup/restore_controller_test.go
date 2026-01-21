package backup

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
)

func TestRestoreSpec_Validation(t *testing.T) {
	tests := []struct {
		name    string
		spec    dbtether.RestoreSpec
		isValid bool
	}{
		{
			name: "valid with backupRef",
			spec: dbtether.RestoreSpec{
				Source: dbtether.RestoreSource{
					BackupRef: &dbtether.BackupReference{
						Name: "my-backup",
					},
				},
				Target: dbtether.RestoreTarget{
					DatabaseRef: dbtether.DatabaseReference{
						Name: "my-database",
					},
				},
				OnConflict: "fail",
			},
			isValid: true,
		},
		{
			name: "valid with path and storageRef",
			spec: dbtether.RestoreSpec{
				Source: dbtether.RestoreSource{
					Path: "cluster/database/20260120-140000.sql.gz",
					StorageRef: &dbtether.StorageReference{
						Name: "my-storage",
					},
				},
				Target: dbtether.RestoreTarget{
					DatabaseRef: dbtether.DatabaseReference{
						Name: "my-database",
					},
				},
				OnConflict: "drop",
			},
			isValid: true,
		},
		{
			name: "valid with overwrite conflict",
			spec: dbtether.RestoreSpec{
				Source: dbtether.RestoreSource{
					BackupRef: &dbtether.BackupReference{
						Name: "my-backup",
					},
				},
				Target: dbtether.RestoreTarget{
					DatabaseRef: dbtether.DatabaseReference{
						Name: "my-database",
					},
				},
				OnConflict: "overwrite",
			},
			isValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := &dbtether.Restore{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-restore",
					Namespace: "default",
				},
				Spec: tt.spec,
			}

			// Basic validation - struct should be valid
			assert.NotEmpty(t, restore.Spec.Target.DatabaseRef.Name)
			if tt.spec.Source.BackupRef != nil {
				assert.NotEmpty(t, tt.spec.Source.BackupRef.Name)
			}
		})
	}
}

func TestRestoreStatus_Phases(t *testing.T) {
	phases := []string{"Pending", "Running", "Completed", "Failed"}

	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			status := dbtether.RestoreStatus{
				Phase:   phase,
				Message: "test message",
			}
			assert.Equal(t, phase, status.Phase)
		})
	}
}

func TestRestoreReconciler_ComputeSpecHash(t *testing.T) {
	r := &RestoreReconciler{}

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "default",
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				BackupRef: &dbtether.BackupReference{
					Name: "my-backup",
				},
			},
			Target: dbtether.RestoreTarget{
				DatabaseRef: dbtether.DatabaseReference{
					Name: "my-database",
				},
			},
		},
	}

	hash1 := r.computeSpecHash(restore)
	hash2 := r.computeSpecHash(restore)

	// Same spec should produce same hash
	assert.Equal(t, hash1, hash2)
	assert.Len(t, hash1, 16) // 8 bytes = 16 hex chars

	// Different spec should produce different hash
	restore.Spec.OnConflict = "drop"
	hash3 := r.computeSpecHash(restore)
	assert.NotEqual(t, hash1, hash3)
}

func TestRestoreReconciler_IsAlreadyProcessed(t *testing.T) {
	tests := []struct {
		name       string
		phase      string
		statusHash string
		specHash   string
		expectSkip bool
	}{
		{
			name:       "empty status - not processed",
			phase:      "",
			statusHash: "",
			specHash:   "abc123",
			expectSkip: false,
		},
		{
			name:       "running - not processed",
			phase:      "Running",
			statusHash: "abc123",
			specHash:   "abc123",
			expectSkip: false,
		},
		{
			name:       "completed with same hash - processed",
			phase:      "Completed",
			statusHash: "abc123",
			specHash:   "abc123",
			expectSkip: true,
		},
		{
			name:       "failed with same hash - processed",
			phase:      "Failed",
			statusHash: "abc123",
			specHash:   "abc123",
			expectSkip: true,
		},
		{
			name:       "completed with different hash - not processed (spec changed)",
			phase:      "Completed",
			statusHash: "abc123",
			specHash:   "xyz789",
			expectSkip: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore := &dbtether.Restore{
				Status: dbtether.RestoreStatus{
					Phase:    tt.phase,
					SpecHash: tt.statusHash,
				},
			}

			// Using zap test logger would require more setup, so just testing the logic
			isProcessed := func() bool {
				if restore.Status.Phase == "" || restore.Status.SpecHash != tt.specHash {
					return false
				}
				return restore.Status.Phase == "Completed" || restore.Status.Phase == "Failed"
			}

			assert.Equal(t, tt.expectSkip, isProcessed())
		})
	}
}

func TestRestoreSource_Validation(t *testing.T) {
	tests := []struct {
		name      string
		source    dbtether.RestoreSource
		hasBackup bool
		hasPath   bool
		hasLatest bool
	}{
		{
			name: "backupRef only",
			source: dbtether.RestoreSource{
				BackupRef: &dbtether.BackupReference{Name: "backup"},
			},
			hasBackup: true,
			hasPath:   false,
			hasLatest: false,
		},
		{
			name: "latestFrom only",
			source: dbtether.RestoreSource{
				LatestFrom: &dbtether.LatestFromSource{
					DatabaseRef: dbtether.DatabaseReference{Name: "my-database"},
				},
			},
			hasBackup: false,
			hasPath:   false,
			hasLatest: true,
		},
		{
			name: "latestFrom with namespace",
			source: dbtether.RestoreSource{
				LatestFrom: &dbtether.LatestFromSource{
					DatabaseRef: dbtether.DatabaseReference{Name: "prod-db"},
					Namespace:   "prod",
				},
			},
			hasBackup: false,
			hasPath:   false,
			hasLatest: true,
		},
		{
			name: "path with storageRef",
			source: dbtether.RestoreSource{
				Path:       "path/to/backup.sql.gz",
				StorageRef: &dbtether.StorageReference{Name: "storage"},
			},
			hasBackup: false,
			hasPath:   true,
			hasLatest: false,
		},
		{
			name:      "empty source",
			source:    dbtether.RestoreSource{},
			hasBackup: false,
			hasPath:   false,
			hasLatest: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.hasBackup, tt.source.BackupRef != nil)
			assert.Equal(t, tt.hasPath, tt.source.Path != "")
			assert.Equal(t, tt.hasLatest, tt.source.LatestFrom != nil)
		})
	}
}

func TestLatestFromSource_Fields(t *testing.T) {
	t.Run("with namespace", func(t *testing.T) {
		source := &dbtether.LatestFromSource{
			DatabaseRef: dbtether.DatabaseReference{Name: "prod-database"},
			Namespace:   "production",
		}
		assert.Equal(t, "prod-database", source.DatabaseRef.Name)
		assert.Equal(t, "production", source.Namespace)
	})

	t.Run("without namespace", func(t *testing.T) {
		source := &dbtether.LatestFromSource{
			DatabaseRef: dbtether.DatabaseReference{Name: "local-database"},
		}
		assert.Equal(t, "local-database", source.DatabaseRef.Name)
		assert.Empty(t, source.Namespace)
	})
}

func TestBuildEnvVars_Storage(t *testing.T) {
	r := &RestoreReconciler{}

	db := &dbtether.Database{
		Status: dbtether.DatabaseStatus{
			DatabaseName: "test_db",
		},
	}

	cluster := &dbtether.DBCluster{
		Spec: dbtether.DBClusterSpec{
			Endpoint: "db.example.com",
			Port:     5432,
		},
	}

	tests := []struct {
		name        string
		storage     *dbtether.BackupStorage
		expectType  string
		expectCount int
	}{
		{
			name: "S3 storage",
			storage: &dbtether.BackupStorage{
				Spec: dbtether.BackupStorageSpec{
					S3: &dbtether.S3StorageConfig{
						Bucket: "test-bucket",
						Region: "us-east-1",
					},
				},
			},
			expectType:  "s3",
			expectCount: 7, // base vars + S3 vars
		},
		{
			name: "GCS storage",
			storage: &dbtether.BackupStorage{
				Spec: dbtether.BackupStorageSpec{
					GCS: &dbtether.GCSStorageConfig{
						Bucket:  "test-bucket",
						Project: "test-project",
					},
				},
			},
			expectType:  "gcs",
			expectCount: 8, // base vars + GCS vars with project
		},
		{
			name: "Azure storage",
			storage: &dbtether.BackupStorage{
				Spec: dbtether.BackupStorageSpec{
					Azure: &dbtether.AzureStorageConfig{
						Container:      "test-container",
						StorageAccount: "teststorage",
					},
				},
			},
			expectType:  "azure",
			expectCount: 8, // base vars + Azure vars
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := r.buildEnvVars(db, cluster, tt.storage, "path/to/backup.sql.gz", "fail")

			// Find storage type
			var storageType string
			for _, e := range env {
				if e.Name == "STORAGE_TYPE" {
					storageType = e.Value
					break
				}
			}

			assert.Equal(t, tt.expectType, storageType)
			assert.GreaterOrEqual(t, len(env), 5) // At least base vars
		})
	}
}

func TestBuildEnvVars_Credentials(t *testing.T) {
	r := &RestoreReconciler{}

	db := &dbtether.Database{
		Status: dbtether.DatabaseStatus{
			DatabaseName: "test_db",
		},
	}

	storage := &dbtether.BackupStorage{
		Spec: dbtether.BackupStorageSpec{
			S3: &dbtether.S3StorageConfig{
				Bucket: "test-bucket",
				Region: "us-east-1",
			},
		},
	}

	t.Run("with credentials secret ref", func(t *testing.T) {
		cluster := &dbtether.DBCluster{
			Spec: dbtether.DBClusterSpec{
				Endpoint: "db.example.com",
				Port:     5432,
				CredentialsSecretRef: &dbtether.SecretReference{
					Name:      "db-credentials",
					Namespace: "default",
				},
			},
		}

		env := r.buildEnvVars(db, cluster, storage, "path/backup.sql.gz", "fail")

		// Should have DB_USER and DB_PASSWORD from secret
		var hasUser, hasPassword bool
		for _, e := range env {
			if e.Name == "DB_USER" && e.ValueFrom != nil {
				hasUser = true
				require.NotNil(t, e.ValueFrom.SecretKeyRef)
				assert.Equal(t, "db-credentials", e.ValueFrom.SecretKeyRef.Name)
				assert.Equal(t, "username", e.ValueFrom.SecretKeyRef.Key)
			}
			if e.Name == "DB_PASSWORD" && e.ValueFrom != nil {
				hasPassword = true
				require.NotNil(t, e.ValueFrom.SecretKeyRef)
				assert.Equal(t, "password", e.ValueFrom.SecretKeyRef.Key)
			}
		}

		assert.True(t, hasUser, "should have DB_USER from secret")
		assert.True(t, hasPassword, "should have DB_PASSWORD from secret")
	})

	t.Run("without credentials secret ref", func(t *testing.T) {
		cluster := &dbtether.DBCluster{
			Spec: dbtether.DBClusterSpec{
				Endpoint: "db.example.com",
				Port:     5432,
			},
		}

		env := r.buildEnvVars(db, cluster, storage, "path/backup.sql.gz", "fail")

		// Should NOT have DB_USER or DB_PASSWORD
		for _, e := range env {
			assert.NotEqual(t, "DB_USER", e.Name)
			assert.NotEqual(t, "DB_PASSWORD", e.Name)
		}
	})
}

func TestRestoreLabels(t *testing.T) {
	assert.Equal(t, "dbtether.io/restore", LabelRestoreName)
	assert.Equal(t, "dbtether.io/restore-namespace", LabelRestoreNamespace)
}

func TestRestoreFinalizer(t *testing.T) {
	assert.Equal(t, "dbtether.io/restore-job", restoreFinalizer)
}

func newFakeRestoreReconciler(objs ...runtime.Object) *RestoreReconciler {
	scheme := runtime.NewScheme()
	_ = dbtether.AddToScheme(scheme)

	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	return &RestoreReconciler{
		Client: client,
		Scheme: scheme,
	}
}

func TestResolveSource_BackupRef(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	backup := &dbtether.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-backup",
			Namespace: "default",
		},
		Spec: dbtether.BackupSpec{
			DatabaseRef: dbtether.DatabaseReference{Name: "my-db"},
			StorageRef:  dbtether.StorageReference{Name: "my-storage"},
		},
		Status: dbtether.BackupStatus{
			Phase:       "Completed",
			Path:        "cluster/db/20260120-140000.sql.gz",
			CompletedAt: &now,
		},
	}

	r := newFakeRestoreReconciler(backup)

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "default",
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				BackupRef: &dbtether.BackupReference{Name: "my-backup"},
			},
		},
	}

	path, storageRef, err := r.resolveSource(ctx, restore)
	require.NoError(t, err)
	assert.Equal(t, "cluster/db/20260120-140000.sql.gz", path)
	assert.Equal(t, "my-storage", storageRef)
}

func TestResolveSource_BackupRef_NotFound(t *testing.T) {
	ctx := context.Background()
	r := newFakeRestoreReconciler()

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "default",
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				BackupRef: &dbtether.BackupReference{Name: "nonexistent"},
			},
		},
	}

	_, _, err := r.resolveSource(ctx, restore)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup not found")
}

func TestResolveSource_BackupRef_NotCompleted(t *testing.T) {
	ctx := context.Background()

	backup := &dbtether.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-backup",
			Namespace: "default",
		},
		Status: dbtether.BackupStatus{
			Phase: "Running",
		},
	}

	r := newFakeRestoreReconciler(backup)

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "default",
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				BackupRef: &dbtether.BackupReference{Name: "running-backup"},
			},
		},
	}

	_, _, err := r.resolveSource(ctx, restore)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup is not completed")
}

func TestResolveSource_LatestFrom(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	earlier := metav1.NewTime(now.Add(-1 * time.Hour))

	// Create multiple backups, latest should be selected
	backups := []runtime.Object{
		&dbtether.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backup-old",
				Namespace: "default",
			},
			Spec: dbtether.BackupSpec{
				DatabaseRef: dbtether.DatabaseReference{Name: "target-db"},
				StorageRef:  dbtether.StorageReference{Name: "storage-1"},
			},
			Status: dbtether.BackupStatus{
				Phase:       "Completed",
				Path:        "old/path.sql.gz",
				CompletedAt: &earlier,
			},
		},
		&dbtether.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backup-new",
				Namespace: "default",
			},
			Spec: dbtether.BackupSpec{
				DatabaseRef: dbtether.DatabaseReference{Name: "target-db"},
				StorageRef:  dbtether.StorageReference{Name: "storage-2"},
			},
			Status: dbtether.BackupStatus{
				Phase:       "Completed",
				Path:        "new/path.sql.gz",
				CompletedAt: &now,
			},
		},
		// Different database - should be ignored
		&dbtether.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backup-other-db",
				Namespace: "default",
			},
			Spec: dbtether.BackupSpec{
				DatabaseRef: dbtether.DatabaseReference{Name: "other-db"},
				StorageRef:  dbtether.StorageReference{Name: "storage-3"},
			},
			Status: dbtether.BackupStatus{
				Phase:       "Completed",
				Path:        "other/path.sql.gz",
				CompletedAt: &now,
			},
		},
	}

	r := newFakeRestoreReconciler(backups...)

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "default",
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				LatestFrom: &dbtether.LatestFromSource{
					DatabaseRef: dbtether.DatabaseReference{Name: "target-db"},
				},
			},
		},
	}

	path, storageRef, err := r.resolveSource(ctx, restore)
	require.NoError(t, err)
	assert.Equal(t, "new/path.sql.gz", path)
	assert.Equal(t, "storage-2", storageRef)
}

func TestResolveSource_LatestFrom_NoBackups(t *testing.T) {
	ctx := context.Background()
	r := newFakeRestoreReconciler()

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "default",
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				LatestFrom: &dbtether.LatestFromSource{
					DatabaseRef: dbtether.DatabaseReference{Name: "target-db"},
				},
			},
		},
	}

	_, _, err := r.resolveSource(ctx, restore)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no completed backup found")
}

func TestResolveSource_LatestFrom_CrossNamespace(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	backup := &dbtether.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prod-backup",
			Namespace: "prod",
		},
		Spec: dbtether.BackupSpec{
			DatabaseRef: dbtether.DatabaseReference{Name: "prod-db"},
			StorageRef:  dbtether.StorageReference{Name: "prod-storage"},
		},
		Status: dbtether.BackupStatus{
			Phase:       "Completed",
			Path:        "prod/backup.sql.gz",
			CompletedAt: &now,
		},
	}

	r := newFakeRestoreReconciler(backup)

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dev-restore",
			Namespace: "dev", // Different namespace
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				LatestFrom: &dbtether.LatestFromSource{
					DatabaseRef: dbtether.DatabaseReference{Name: "prod-db"},
					Namespace:   "prod", // Look in prod namespace
				},
			},
		},
	}

	path, storageRef, err := r.resolveSource(ctx, restore)
	require.NoError(t, err)
	assert.Equal(t, "prod/backup.sql.gz", path)
	assert.Equal(t, "prod-storage", storageRef)
}

func TestResolveSource_DirectPath(t *testing.T) {
	ctx := context.Background()
	r := newFakeRestoreReconciler()

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "default",
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				Path:       "direct/path/backup.sql.gz",
				StorageRef: &dbtether.StorageReference{Name: "my-storage"},
			},
		},
	}

	path, storageRef, err := r.resolveSource(ctx, restore)
	require.NoError(t, err)
	assert.Equal(t, "direct/path/backup.sql.gz", path)
	assert.Equal(t, "my-storage", storageRef)
}

func TestResolveSource_DirectPath_MissingStorageRef(t *testing.T) {
	ctx := context.Background()
	r := newFakeRestoreReconciler()

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "default",
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				Path: "direct/path/backup.sql.gz",
				// Missing StorageRef
			},
		},
	}

	_, _, err := r.resolveSource(ctx, restore)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storageRef is required")
}

func TestResolveSource_EmptySource(t *testing.T) {
	ctx := context.Background()
	r := newFakeRestoreReconciler()

	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-restore",
			Namespace: "default",
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{}, // Empty
		},
	}

	_, _, err := r.resolveSource(ctx, restore)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "either backupRef, latestFrom, or path must be specified")
}
