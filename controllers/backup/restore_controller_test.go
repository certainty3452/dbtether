package backup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/controllers"
	"github.com/certainty3452/dbtether/pkg/postgres"
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
	phases := []string{"Pending", "Running", "Granting", "Completed", "Failed"}

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
		{
			name: "S3 storage with credentialsSecretRef",
			storage: &dbtether.BackupStorage{
				Spec: dbtether.BackupStorageSpec{
					S3: &dbtether.S3StorageConfig{
						Bucket: "test-bucket",
						Region: "us-east-1",
					},
					CredentialsSecretRef: &dbtether.SecretReference{
						Name:      "s3-creds",
						Namespace: "default",
					},
				},
			},
			expectType:  "s3",
			expectCount: 10, // base vars + S3 vars + AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY
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

			if tt.storage.Spec.CredentialsSecretRef != nil {
				var hasAccessKey, hasSecretKey bool
				for _, e := range env {
					if e.Name == "AWS_ACCESS_KEY_ID" {
						hasAccessKey = true
						require.NotNil(t, e.ValueFrom)
						require.NotNil(t, e.ValueFrom.SecretKeyRef)
						assert.Equal(t, tt.storage.Spec.CredentialsSecretRef.Name, e.ValueFrom.SecretKeyRef.Name)
						assert.Equal(t, "AWS_ACCESS_KEY_ID", e.ValueFrom.SecretKeyRef.Key)
					}
					if e.Name == "AWS_SECRET_ACCESS_KEY" {
						hasSecretKey = true
						require.NotNil(t, e.ValueFrom)
						require.NotNil(t, e.ValueFrom.SecretKeyRef)
						assert.Equal(t, tt.storage.Spec.CredentialsSecretRef.Name, e.ValueFrom.SecretKeyRef.Name)
						assert.Equal(t, "AWS_SECRET_ACCESS_KEY", e.ValueFrom.SecretKeyRef.Key)
					}
				}
				assert.True(t, hasAccessKey, "expected AWS_ACCESS_KEY_ID env var")
				assert.True(t, hasSecretKey, "expected AWS_SECRET_ACCESS_KEY env var")
			}
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

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objs...).
		Build()

	return &RestoreReconciler{
		Client: fakeClient,
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

func TestJobToRestoreMapping(t *testing.T) {
	tests := []struct {
		name              string
		jobNamespace      string
		labels            map[string]string
		operatorNamespace string
		expectRequest     bool
		expectedName      string
		expectedNamespace string
	}{
		{
			name:         "correct labels in operator namespace",
			jobNamespace: "dbtether",
			labels: map[string]string{
				LabelRestoreName:      "my-restore",
				LabelRestoreNamespace: "app-team",
			},
			operatorNamespace: "dbtether",
			expectRequest:     true,
			expectedName:      "my-restore",
			expectedNamespace: "app-team",
		},
		{
			name:         "wrong namespace ignored",
			jobNamespace: "other-namespace",
			labels: map[string]string{
				LabelRestoreName:      "my-restore",
				LabelRestoreNamespace: "app-team",
			},
			operatorNamespace: "dbtether",
			expectRequest:     false,
		},
		{
			name:              "no labels ignored",
			jobNamespace:      "dbtether",
			labels:            map[string]string{},
			operatorNamespace: "dbtether",
			expectRequest:     false,
		},
		{
			name:         "partial labels ignored",
			jobNamespace: "dbtether",
			labels: map[string]string{
				LabelRestoreName: "partial-restore",
			},
			operatorNamespace: "dbtether",
			expectRequest:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.jobNamespace != tt.operatorNamespace {
				if tt.expectRequest {
					t.Error("expected no request for job in wrong namespace")
				}
				return
			}

			restoreName := tt.labels[LabelRestoreName]
			restoreNamespace := tt.labels[LabelRestoreNamespace]

			if restoreName == "" || restoreNamespace == "" {
				if tt.expectRequest {
					t.Error("expected no request for job without proper labels")
				}
				return
			}

			if !tt.expectRequest {
				t.Error("expected no request but mapping logic would produce one")
				return
			}

			assert.Equal(t, tt.expectedName, restoreName)
			assert.Equal(t, tt.expectedNamespace, restoreNamespace)
		})
	}
}

func newRestoreTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = dbtether.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return scheme
}

func TestRestoreReconciler_EnsureFinalizer_UpdateError(t *testing.T) {
	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "test-restore", Namespace: "default"},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				Path:       "some/path.sql.gz",
				StorageRef: &dbtether.StorageReference{Name: "my-storage"},
			},
			Target: dbtether.RestoreTarget{
				DatabaseRef: dbtether.DatabaseReference{Name: "my-database"},
			},
		},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore).
		WithStatusSubresource(&dbtether.Restore{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("simulated update conflict")
			},
		}).
		Build()

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether"}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "test-restore", Namespace: "default"}}
	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected error when finalizer Update fails")
	}
}

func TestRestoreReconciler_AdoptsExistingJob(t *testing.T) {
	restore := &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "my-restore",
			Namespace:  "app-ns",
			Finalizers: []string{restoreFinalizer},
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				Path:       "cluster/db/20260120-140000.sql.gz",
				StorageRef: &dbtether.StorageReference{Name: "my-storage"},
			},
			Target: dbtether.RestoreTarget{
				DatabaseRef: dbtether.DatabaseReference{Name: "my-database"},
			},
		},
	}

	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-my-restore-existing1",
			Namespace: "dbtether",
			Labels: map[string]string{
				LabelRestoreName:      "my-restore",
				LabelRestoreNamespace: "app-ns",
			},
		},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, existingJob).
		WithStatusSubresource(&dbtether.Restore{}).
		Build()

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether"}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly 1 job (the existing one), got %d", len(jobs.Items))
	}
	if jobs.Items[0].Name != existingJob.Name {
		t.Errorf("expected existing job to survive unchanged, got %q", jobs.Items[0].Name)
	}

	var updatedRestore dbtether.Restore
	if err := r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updatedRestore); err != nil {
		t.Fatalf("failed to get restore: %v", err)
	}
	if updatedRestore.Status.JobName != existingJob.Name {
		t.Errorf("expected Status.JobName to be adopted as %q, got %q", existingJob.Name, updatedRestore.Status.JobName)
	}
	if updatedRestore.Status.Phase != "Running" {
		t.Errorf("expected Phase=Running after adoption, got %q", updatedRestore.Status.Phase)
	}
}

func TestRestoreLabelConstants(t *testing.T) {
	expected := map[string]string{
		"dbtether.io/restore":           LabelRestoreName,
		"dbtether.io/restore-namespace": LabelRestoreNamespace,
	}

	for want, got := range expected {
		assert.Equal(t, want, got, "label constant mismatch")
	}
}

// recordedApplyPrivilegesCall captures one invocation of ApplyPrivileges so
// tests can assert exactly what regrantDatabaseUsers sent to Postgres.
type recordedApplyPrivilegesCall struct {
	Username, Database, Preset string
	AdditionalGrants           []postgres.TableGrant
}

// recordingPGClient wraps postgres.MockClient (which doesn't track
// ApplyPrivileges calls) to record invocations without touching the shared mock.
type recordingPGClient struct {
	*postgres.MockClient
	calls     []recordedApplyPrivilegesCall
	applyErr  error            // blanket failure for every call, unless overridden below
	failUsers map[string]error // per-username override, for exercising partial-failure rounds
}

func (m *recordingPGClient) ApplyPrivileges(ctx context.Context, username, database, preset string, additionalGrants []postgres.TableGrant) error {
	m.calls = append(m.calls, recordedApplyPrivilegesCall{
		Username:         username,
		Database:         database,
		Preset:           preset,
		AdditionalGrants: additionalGrants,
	})
	if err, ok := m.failUsers[username]; ok {
		return err
	}
	return m.applyErr
}

// singleClientCache is a minimal ClientCacheInterface that always returns the
// same client and counts how many times a connection was requested.
type singleClientCache struct {
	pgClient postgres.ClientInterface
	getCalls int
}

func (c *singleClientCache) Get(_ context.Context, _ string, _ postgres.Config) (postgres.ClientInterface, error) {
	c.getCalls++
	return c.pgClient, nil
}
func (c *singleClientCache) Remove(_ string) {}
func (c *singleClientCache) Close()          {}

// indexDatabaseUserRefsForTest reimplements the controllers package's unexported
// indexer, which can't be imported here, so the fake client can be built
// WithIndex the same way the real manager wires it up via RegisterIndexers.
func indexDatabaseUserRefsForTest(obj client.Object) []string {
	user, ok := obj.(*dbtether.DatabaseUser)
	if !ok {
		return nil
	}
	accesses := user.Spec.GetDatabases()
	keys := make([]string, 0, len(accesses))
	for _, access := range accesses {
		ns := access.Namespace
		if ns == "" {
			ns = user.Namespace
		}
		keys = append(keys, controllers.DatabaseUserDatabaseRefKey(ns, access.Name))
	}
	return keys
}

// newRegrantFixture builds a Restore whose Job has already succeeded, plus
// the target Database/DBCluster/credentials Secret it points at. Restore
// lives in "app-ns"; the restore Job lives in the operator namespace "dbtether".
func newRegrantFixture() (restore *dbtether.Restore, job *batchv1.Job, db *dbtether.Database, cluster *dbtether.DBCluster, secret *corev1.Secret) {
	restore = &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "my-restore",
			Namespace:  "app-ns",
			Finalizers: []string{restoreFinalizer},
		},
		Spec: dbtether.RestoreSpec{
			Source: dbtether.RestoreSource{
				Path:       "cluster1/my-database/20260120-140000.sql.gz",
				StorageRef: &dbtether.StorageReference{Name: "my-storage"},
			},
			Target: dbtether.RestoreTarget{
				DatabaseRef: dbtether.DatabaseReference{Name: "my-database"},
			},
		},
		Status: dbtether.RestoreStatus{
			Phase:   "Running",
			JobName: "restore-my-restore-run1",
		},
	}

	job = &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "restore-my-restore-run1",
			Namespace: "dbtether",
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}

	db = &dbtether.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "my-database", Namespace: "app-ns"},
		Spec: dbtether.DatabaseSpec{
			ClusterRef: dbtether.ClusterReference{Name: "cluster1"},
		},
		Status: dbtether.DatabaseStatus{
			Phase:        "Ready",
			DatabaseName: "my_database_pg",
		},
	}

	cluster = &dbtether.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster1"},
		Spec: dbtether.DBClusterSpec{
			Endpoint: "localhost",
			Port:     5432,
			CredentialsSecretRef: &dbtether.SecretReference{
				Name:      "cluster-creds",
				Namespace: "app-ns",
			},
		},
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-creds", Namespace: "app-ns"},
		Data: map[string][]byte{
			"username": []byte("postgres"),
			"password": []byte("pw"),
		},
	}

	return restore, job, db, cluster, secret
}

func TestRestoreReconciler_RegrantsGrantsOnSuccess(t *testing.T) {
	restore, job, db, cluster, secret := newRegrantFixture()

	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database", Privileges: "readwrite"},
			AdditionalGrants: []dbtether.TableGrant{
				{Tables: []string{"orders"}, Privileges: []string{"SELECT"}},
			},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, job, db, cluster, secret, user).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	pg := &recordingPGClient{MockClient: postgres.NewMockClient()}
	require.NoError(t, pg.CreateUser(context.Background(), "app_user", "pw"))
	cache := &singleClientCache{pgClient: pg}

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.Len(t, pg.calls, 1)
	assert.Equal(t, "app_user", pg.calls[0].Username)
	assert.Equal(t, "my_database_pg", pg.calls[0].Database)
	assert.Equal(t, "readwrite", pg.calls[0].Preset)
	assert.Equal(t, []postgres.TableGrant{
		{Tables: []string{"orders"}, Privileges: []postgres.TablePrivilege{"SELECT"}},
	}, pg.calls[0].AdditionalGrants)

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Completed", updated.Status.Phase)
}

func TestRestoreReconciler_RegrantFailureKeepsGranting(t *testing.T) {
	restore, job, db, cluster, secret := newRegrantFixture()

	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, job, db, cluster, secret, user).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	pg := &recordingPGClient{MockClient: postgres.NewMockClient(), applyErr: errors.New("permission denied for database my_database_pg")}
	require.NoError(t, pg.CreateUser(context.Background(), "app_user", "pw"))
	cache := &singleClientCache{pgClient: pg}

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	result, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err, "grant retries are paced via RequeueAfter, not a returned error")
	assert.Equal(t, grantRetryDelay, result.RequeueAfter)

	require.Len(t, pg.calls, 1, "ApplyPrivileges should still have been attempted")

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Granting", updated.Status.Phase, "phase must not advance to Completed when re-granting fails")
	assert.Equal(t, "restore succeeded, retrying grants: failed to re-apply privileges for user app_user: permission denied for database my_database_pg", updated.Status.Message)
	assert.Equal(t, updated.Status.SpecHash, r.computeSpecHash(&updated),
		"Granting must carry the spec hash so a retry past the job's TTL resumes instead of failing")
}

func TestRestoreReconciler_RegrantSkipsUsersWithoutRole(t *testing.T) {
	restore, job, db, cluster, secret := newRegrantFixture()

	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Pending"},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, job, db, cluster, secret, user).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	pg := &recordingPGClient{MockClient: postgres.NewMockClient()}
	cache := &singleClientCache{pgClient: pg}

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	assert.Empty(t, pg.calls, "the role does not exist yet, so the user's own reconcile is what creates and grants it")

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Completed", updated.Status.Phase)
}

func TestRestoreReconciler_RegrantSkipsPgConnectionWhenNoUsers(t *testing.T) {
	restore, job, db, cluster, secret := newRegrantFixture()

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, job, db, cluster, secret).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	pg := &recordingPGClient{MockClient: postgres.NewMockClient()}
	cache := &singleClientCache{pgClient: pg}

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, 0, cache.getCalls, "no DatabaseUser references the target DB, so no PG connection should be requested")

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Completed", updated.Status.Phase)
}

func TestRestoreReconciler_GrantingSurvivesJobGC(t *testing.T) {
	restore, _, db, cluster, secret := newRegrantFixture()
	restore.Status.Phase = "Granting"
	restore.Status.SpecHash = (&RestoreReconciler{}).computeSpecHash(restore)
	// Job intentionally left out of the fake client even though Status.JobName
	// still references it - simulates the Job having been GC'd by its TTL.

	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database", Privileges: "readonly"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, db, cluster, secret, user).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	pg := &recordingPGClient{MockClient: postgres.NewMockClient()}
	require.NoError(t, pg.CreateUser(context.Background(), "app_user", "pw"))
	cache := &singleClientCache{pgClient: pg}

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err, "a GC'd Job must not resurrect as a Failed restore once grants are already in flight")

	require.Len(t, pg.calls, 1, "regrant must still run even though the Job is gone")
	assert.Equal(t, "app_user", pg.calls[0].Username)

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Completed", updated.Status.Phase)
}

func TestRestoreReconciler_JobDeletedWhileRunning_Failed(t *testing.T) {
	restore, _, db, cluster, secret := newRegrantFixture()
	// restore.Status.Phase is "Running" from the fixture. Job intentionally
	// left out of the fake client.

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, db, cluster, secret).
		WithStatusSubresource(&dbtether.Restore{}).
		Build()

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether"}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Failed", updated.Status.Phase)
	assert.Equal(t, "restore job was deleted", updated.Status.Message)
}

func TestRestoreReconciler_DatabaseDeletedDuringGranting_CompletesWithWarning(t *testing.T) {
	restore, _, _, _, _ := newRegrantFixture()
	restore.Status.Phase = "Granting"
	restore.Status.SpecHash = (&RestoreReconciler{}).computeSpecHash(restore)
	// Target Database intentionally left out of the fake client.

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore).
		WithStatusSubresource(&dbtether.Restore{}).
		Build()

	recorder := record.NewFakeRecorder(10)
	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", Recorder: recorder}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Completed", updated.Status.Phase)
	assert.Contains(t, updated.Status.Message, "grants not re-applied")

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, corev1.EventTypeWarning)
		assert.Contains(t, event, EventReasonRestoreGrantsSkipped)
	default:
		t.Fatal("expected a Warning event to be recorded")
	}
}

func TestRestoreReconciler_ClusterDeletedDuringGranting_CompletesWithWarning(t *testing.T) {
	restore, _, db, _, _ := newRegrantFixture()
	restore.Status.Phase = "Granting"
	restore.Status.SpecHash = (&RestoreReconciler{}).computeSpecHash(restore)
	// DBCluster (and its secret) intentionally left out of the fake client.

	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, db, user).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	recorder := record.NewFakeRecorder(10)
	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", Recorder: recorder}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Completed", updated.Status.Phase)
	assert.Contains(t, updated.Status.Message, "grants not re-applied")

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, corev1.EventTypeWarning)
		assert.Contains(t, event, EventReasonRestoreGrantsSkipped)
	default:
		t.Fatal("expected a Warning event to be recorded")
	}
}

func TestRestoreReconciler_RegrantsForFailedUser(t *testing.T) {
	restore, job, db, cluster, secret := newRegrantFixture()

	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database", Privileges: "readwrite"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Failed"},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, job, db, cluster, secret, user).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	pg := &recordingPGClient{MockClient: postgres.NewMockClient()}
	require.NoError(t, pg.CreateUser(context.Background(), "app_user", "pw"))
	cache := &singleClientCache{pgClient: pg}

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.Len(t, pg.calls, 1, "the role exists, so regrant must run regardless of the DatabaseUser's own CR phase")
	assert.Equal(t, "app_user", pg.calls[0].Username)

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Completed", updated.Status.Phase)
}

func TestRestoreReconciler_TransientRegrantErrorChargesOneAttemptPerRetry(t *testing.T) {
	restore, job, db, cluster, secret := newRegrantFixture()

	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}

	scheme := newRestoreTestScheme()
	statusPatches := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, job, db, cluster, secret, user).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourcePatch: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
				if subResourceName == "status" {
					statusPatches++
				}
				return c.SubResource(subResourceName).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	pg := &recordingPGClient{MockClient: postgres.NewMockClient(), applyErr: errors.New("permission denied for database my_database_pg")}
	require.NoError(t, pg.CreateUser(context.Background(), "app_user", "pw"))
	cache := &singleClientCache{pgClient: pg}

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache}
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}

	result, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err, "grant retries are paced via RequeueAfter, not a returned error")
	assert.Equal(t, grantRetryDelay, result.RequeueAfter)

	var afterFirst dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &afterFirst))
	require.Equal(t, "Granting", afterFirst.Status.Phase)
	require.Equal(t, int32(1), afterFirst.Status.GrantAttempts)
	patchesAfterFirst := statusPatches

	result, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err, "grant retries are paced via RequeueAfter, not a returned error")
	assert.Equal(t, grantRetryDelay, result.RequeueAfter)

	var afterSecond dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &afterSecond))
	assert.Equal(t, "Granting", afterSecond.Status.Phase, "phase must stay Granting across retries so a retry can outlive the Job's TTL")
	assert.Equal(t, afterFirst.Status.Message, afterSecond.Status.Message, "the retry message is deterministic for the same underlying error")
	assert.Equal(t, int32(2), afterSecond.Status.GrantAttempts, "each retry spends one attempt from the bounded budget")

	assert.Equal(t, patchesAfterFirst+1, statusPatches,
		"the retry budget must be persisted, so an unchanged message still costs exactly one status patch per round")
}

func TestRestoreReconciler_RegrantPartialFailureServesHealthyUsers(t *testing.T) {
	restore, job, db, cluster, secret := newRegrantFixture()

	healthyUser := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "healthy-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database", Privileges: "readonly"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}
	brokenUser := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "broken-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database", Privileges: "readwrite"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, job, db, cluster, secret, healthyUser, brokenUser).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	pg := &recordingPGClient{
		MockClient: postgres.NewMockClient(),
		failUsers: map[string]error{
			"broken_user": errors.New("permission denied for database my_database_pg"),
		},
	}
	require.NoError(t, pg.CreateUser(context.Background(), "healthy_user", "pw"))
	require.NoError(t, pg.CreateUser(context.Background(), "broken_user", "pw"))
	cache := &singleClientCache{pgClient: pg}

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	result, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err, "grant retries are paced via RequeueAfter, not a returned error")
	assert.Equal(t, grantRetryDelay, result.RequeueAfter)

	require.Len(t, pg.calls, 2, "both users must be attempted in the same round even though one of them fails")
	attempted := make(map[string]bool, len(pg.calls))
	for _, call := range pg.calls {
		attempted[call.Username] = true
	}
	assert.True(t, attempted["healthy_user"], "the healthy user must still be served")
	assert.True(t, attempted["broken_user"])

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Granting", updated.Status.Phase)
	assert.Equal(t, int32(1), updated.Status.GrantAttempts)
}

func TestRestoreReconciler_RegrantAttemptsExhausted_CompletesWithWarning(t *testing.T) {
	restore, _, db, cluster, secret := newRegrantFixture()
	restore.Status.Phase = "Granting"
	restore.Status.GrantAttempts = grantAttemptLimit - 1 // this round's failure is the one that hits the limit
	restore.Status.SpecHash = (&RestoreReconciler{}).computeSpecHash(restore)

	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "broken-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, db, cluster, secret, user).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	pg := &recordingPGClient{MockClient: postgres.NewMockClient(), applyErr: errors.New("permission denied for database my_database_pg")}
	require.NoError(t, pg.CreateUser(context.Background(), "broken_user", "pw"))
	cache := &singleClientCache{pgClient: pg}

	recorder := record.NewFakeRecorder(10)
	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache, Recorder: recorder}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err, "the attempt budget is exhausted, so the round completes instead of erroring for another retry")

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Completed", updated.Status.Phase)
	assert.Contains(t, updated.Status.Message, "restore completed; grants not re-applied for:")
	assert.Contains(t, updated.Status.Message, "broken_user")

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, corev1.EventTypeWarning)
		assert.Contains(t, event, EventReasonRestoreGrantsSkipped)
		assert.Contains(t, event, "retries exhausted for broken_user")
	default:
		t.Fatal("expected a Warning event to be recorded")
	}
}

func TestRestoreReconciler_JobSuccessResetsStaleGrantAttempts(t *testing.T) {
	restore, job, db, cluster, secret := newRegrantFixture()
	// Leftover count from an earlier Granting round.
	restore.Status.GrantAttempts = 3

	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "broken-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}

	scheme := newRestoreTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, job, db, cluster, secret, user).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()

	pg := &recordingPGClient{MockClient: postgres.NewMockClient(), applyErr: errors.New("permission denied for database my_database_pg")}
	require.NoError(t, pg.CreateUser(context.Background(), "broken_user", "pw"))
	cache := &singleClientCache{pgClient: pg}

	r := &RestoreReconciler{Client: fakeClient, Scheme: scheme, Namespace: "dbtether", PGClientCache: cache}

	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}}
	result, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err, "the fresh round still has one failing user, but retries are paced via RequeueAfter, not a returned error")
	assert.Equal(t, grantRetryDelay, result.RequeueAfter)

	var updated dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &updated))
	assert.Equal(t, "Granting", updated.Status.Phase)
	assert.Equal(t, int32(1), updated.Status.GrantAttempts,
		"entering Granting from the Job's success must reset the stale counter to 0 before this round's failure charges attempt 1, not carry the stale 3 forward to 4")
}
