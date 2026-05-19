package backup

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/storage"
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

// newStorageTestScheme registers the types needed by the BackupStorage reconciler tests.
func newStorageTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = databasesv1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

// newStorageReconciler builds a BackupStorageReconciler backed by a fake client.
// `probeErr` is what the injected storage factory returns from Reachable().
func newStorageReconciler(probeErr error, objs ...client.Object) (*BackupStorageReconciler, *fakeStorageFactory) {
	scheme := newStorageTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&databasesv1alpha1.BackupStorage{}).
		Build()

	factory := &fakeStorageFactory{reachableErr: probeErr}

	return &BackupStorageReconciler{
		Client:           fakeClient,
		Scheme:           scheme,
		NewStorageClient: factory.build,
	}, factory
}

type fakeStorageFactory struct {
	reachableErr error
	buildErr     error
	calls        int
	lastSpec     *databasesv1alpha1.BackupStorage
}

func (f *fakeStorageFactory) build(_ context.Context, _ client.Client, bs *databasesv1alpha1.BackupStorage) (storage.StorageClient, error) {
	f.calls++
	f.lastSpec = bs
	if f.buildErr != nil {
		return nil, f.buildErr
	}
	m := storage.NewMockClient()
	m.ReachableError = f.reachableErr
	return m, nil
}

// TestBackupStorageReconciler_ProbeReady covers the happy path: valid spec +
// reachable bucket → status Ready, message references reachability.
func TestBackupStorageReconciler_ProbeReady(t *testing.T) {
	bs := &databasesv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: "ready-s3"},
		Spec: databasesv1alpha1.BackupStorageSpec{
			S3: &databasesv1alpha1.S3StorageConfig{Bucket: testBucketName, Region: "eu-central-1"},
		},
	}

	r, factory := newStorageReconciler(nil, bs)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: bs.Name}})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if res.RequeueAfter != StorageValidationInterval {
		t.Errorf("expected requeue after %s on success, got %s", StorageValidationInterval, res.RequeueAfter)
	}
	if factory.calls != 1 {
		t.Errorf("expected factory called once, got %d", factory.calls)
	}

	var got databasesv1alpha1.BackupStorage
	if err := r.Get(context.Background(), types.NamespacedName{Name: bs.Name}, &got); err != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	if got.Status.Phase != "Ready" {
		t.Errorf("expected Phase=Ready, got %q (msg=%q)", got.Status.Phase, got.Status.Message)
	}
	if got.Status.Provider != "s3" {
		t.Errorf("expected Provider=s3, got %q", got.Status.Provider)
	}
}

// TestBackupStorageReconciler_ProbeFailureSurfacesError covers the missed
// failure mode from the previous behaviour: validateStorage said "one provider
// configured", reconciler flipped to Ready, the actual bucket call failed.
// The reachability probe must catch this and surface the real error.
func TestBackupStorageReconciler_ProbeFailureSurfacesError(t *testing.T) {
	bs := &databasesv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: "broken-s3"},
		Spec: databasesv1alpha1.BackupStorageSpec{
			S3: &databasesv1alpha1.S3StorageConfig{Bucket: "missing-bucket", Region: "eu-central-1"},
		},
	}

	probeErr := errors.New(`s3 bucket "missing-bucket" not reachable: AccessDenied`)
	r, _ := newStorageReconciler(probeErr, bs)

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: bs.Name}})
	if err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	// On Failed we requeue sooner — operators need fast feedback during setup.
	if res.RequeueAfter == StorageValidationInterval {
		t.Errorf("expected fast requeue on Failed, got long-interval %s", res.RequeueAfter)
	}

	var got databasesv1alpha1.BackupStorage
	if err := r.Get(context.Background(), types.NamespacedName{Name: bs.Name}, &got); err != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	if got.Status.Phase != "Failed" {
		t.Errorf("expected Phase=Failed, got %q", got.Status.Phase)
	}
	if got.Status.Message != probeErr.Error() {
		t.Errorf("expected Failed message to surface probe error verbatim, got %q", got.Status.Message)
	}
}

// TestBackupStorageReconciler_ValidationFailureSkipsProbe verifies the
// pre-existing validation still short-circuits before the probe runs — no
// point calling the SDK if the spec itself is malformed.
func TestBackupStorageReconciler_ValidationFailureSkipsProbe(t *testing.T) {
	bs := &databasesv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: "no-provider"},
		Spec:       databasesv1alpha1.BackupStorageSpec{},
	}

	r, factory := newStorageReconciler(nil, bs)

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: bs.Name}}); err != nil {
		t.Fatalf("Reconcile returned error: %v", err)
	}
	if factory.calls != 0 {
		t.Errorf("expected probe to be skipped on validation failure, factory called %d times", factory.calls)
	}

	var got databasesv1alpha1.BackupStorage
	if err := r.Get(context.Background(), types.NamespacedName{Name: bs.Name}, &got); err != nil {
		t.Fatalf("get after reconcile: %v", err)
	}
	if got.Status.Phase != "Failed" {
		t.Errorf("expected Phase=Failed, got %q", got.Status.Phase)
	}
}

// TestReadS3CredentialsErrors covers the credential-resolution branches that
// are easy to get wrong: missing namespace on the SecretRef, missing Secret,
// and incomplete Secret data.
func TestReadS3CredentialsErrors(t *testing.T) {
	scheme := newStorageTestScheme()

	tests := []struct {
		name        string
		bs          *databasesv1alpha1.BackupStorage
		secret      *corev1.Secret
		wantErrPart string
	}{
		{
			name: "missing namespace on credentialsSecretRef",
			bs: &databasesv1alpha1.BackupStorage{
				ObjectMeta: metav1.ObjectMeta{Name: "no-ns"},
				Spec: databasesv1alpha1.BackupStorageSpec{
					S3:                   &databasesv1alpha1.S3StorageConfig{Bucket: "b", Region: "r"},
					CredentialsSecretRef: &databasesv1alpha1.SecretReference{Name: "creds"},
				},
			},
			wantErrPart: "credentialsSecretRef.namespace is required",
		},
		{
			name: "secret not found",
			bs: &databasesv1alpha1.BackupStorage{
				ObjectMeta: metav1.ObjectMeta{Name: "not-found"},
				Spec: databasesv1alpha1.BackupStorageSpec{
					S3:                   &databasesv1alpha1.S3StorageConfig{Bucket: "b", Region: "r"},
					CredentialsSecretRef: &databasesv1alpha1.SecretReference{Name: "missing", Namespace: "default"},
				},
			},
			wantErrPart: "not found",
		},
		{
			name: "secret missing keys",
			bs: &databasesv1alpha1.BackupStorage{
				ObjectMeta: metav1.ObjectMeta{Name: "bad-keys"},
				Spec: databasesv1alpha1.BackupStorageSpec{
					S3:                   &databasesv1alpha1.S3StorageConfig{Bucket: "b", Region: "r"},
					CredentialsSecretRef: &databasesv1alpha1.SecretReference{Name: "creds", Namespace: "default"},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "default"},
				Data:       map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("AKIA")}, // secret key missing
			},
			wantErrPart: "missing AWS_ACCESS_KEY_ID or AWS_SECRET_ACCESS_KEY",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tt.secret != nil {
				builder = builder.WithObjects(tt.secret)
			}
			c := builder.Build()

			_, _, err := readS3Credentials(context.Background(), c, tt.bs)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.wantErrPart)
			}
			if !contains(err.Error(), tt.wantErrPart) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErrPart)
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
