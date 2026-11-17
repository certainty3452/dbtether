package backup

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/storage"
)

const (
	backupPath = "cluster/db/run1.sql.gz"
	otherPath  = "cluster/db/run2.sql.gz"
)

type fileTestEnv struct {
	r              *BackupReconciler
	files          *storage.MockClient
	events         *record.FakeRecorder
	clientsBuilt   int
	clientBuildErr error
}

func newFileTestEnv(objs ...client.Object) *fileTestEnv {
	env := &fileTestEnv{
		r:      newTestReconciler(objs...),
		files:  storage.NewMockClient(),
		events: record.NewFakeRecorder(100),
	}
	env.r.Recorder = env.events
	env.r.NewStorageClient = func(context.Context, client.Client, *databasesv1alpha1.BackupStorage) (storage.StorageClient, error) {
		env.clientsBuilt++
		if env.clientBuildErr != nil {
			return nil, env.clientBuildErr
		}
		return env.files, nil
	}
	return env
}

func (env *fileTestEnv) hasFile(path string) bool {
	_, ok := env.files.GetObject(path)
	return ok
}

func (env *fileTestEnv) requireEvent(t *testing.T, reason, want string) {
	t.Helper()
	for {
		select {
		case event := <-env.events.Events:
			if strings.Contains(event, "Warning "+reason) && strings.Contains(event, want) {
				return
			}
		default:
			t.Fatalf("expected a Warning %s event containing %q", reason, want)
		}
	}
}

func deletingBackup(policy string, status *databasesv1alpha1.BackupStatus) *databasesv1alpha1.Backup {
	now := metav1.Now()
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.DeletionTimestamp = &now
	backup.Spec.DeletionPolicy = policy
	backup.Status = *status
	return backup
}

func reconcileDeletion(env *fileTestEnv) error {
	_, err := env.r.Reconcile(context.Background(), rerunRequest())
	return err
}

func requireFinalizerRemoved(t *testing.T, env *fileTestEnv) {
	t.Helper()
	var backup databasesv1alpha1.Backup
	err := env.r.Get(context.Background(), rerunRequest().NamespacedName, &backup)
	if err == nil && controllerutil.ContainsFinalizer(&backup, backupFinalizer) {
		t.Fatal("expected the finalizer to be removed")
	}
}

func requireFinalizerKept(t *testing.T, env *fileTestEnv) {
	t.Helper()
	var backup databasesv1alpha1.Backup
	if err := env.r.Get(context.Background(), rerunRequest().NamespacedName, &backup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	if !controllerutil.ContainsFinalizer(&backup, backupFinalizer) {
		t.Fatal("expected the finalizer to be kept")
	}
}

func TestBackupDeletion_RetainKeepsFile(t *testing.T) {
	for _, policy := range []string{"Retain", ""} {
		t.Run("policy "+policy, func(t *testing.T) {
			env := newFileTestEnv(deletingBackup(policy, &databasesv1alpha1.BackupStatus{Path: backupPath}), newTestStorage(testStorageName))
			env.files.AddObject(backupPath, []byte("dump"), metav1.Now().Time)

			if err := reconcileDeletion(env); err != nil {
				t.Fatalf(errUnexpectedError, err)
			}

			requireFinalizerRemoved(t, env)
			if !env.hasFile(backupPath) {
				t.Error("expected the file to be retained")
			}
			if env.clientsBuilt != 0 {
				t.Errorf("expected no storage access under Retain, built %d clients", env.clientsBuilt)
			}
		})
	}
}

func TestBackupDeletion_DeleteRemovesFile(t *testing.T) {
	env := newFileTestEnv(deletingBackup("Delete", &databasesv1alpha1.BackupStatus{Path: backupPath}), newTestStorage(testStorageName))
	env.files.AddObject(backupPath, []byte("dump"), metav1.Now().Time)
	env.files.AddObject(otherPath, []byte("other"), metav1.Now().Time)

	if err := reconcileDeletion(env); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	requireFinalizerRemoved(t, env)
	if env.hasFile(backupPath) {
		t.Error("expected the Backup's file to be deleted")
	}
	if !env.hasFile(otherPath) {
		t.Error("expected files the Backup does not own to be left alone")
	}
}

func TestBackupDeletion_StorageErrorKeepsFinalizerUntilDeleteSucceeds(t *testing.T) {
	env := newFileTestEnv(deletingBackup("Delete", &databasesv1alpha1.BackupStatus{Path: backupPath}), newTestStorage(testStorageName))
	env.files.AddObject(backupPath, []byte("dump"), metav1.Now().Time)
	env.files.DeleteError = errors.New("access denied")

	if err := reconcileDeletion(env); err == nil {
		t.Fatal("expected the storage error to be returned for a requeue")
	}
	requireFinalizerKept(t, env)
	env.requireEvent(t, EventReasonFileDeleteFailed, backupPath)

	env.files.DeleteError = nil
	if err := reconcileDeletion(env); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	requireFinalizerRemoved(t, env)
	if env.hasFile(backupPath) {
		t.Error("expected the file to be deleted once storage recovers")
	}
}

func TestBackupDeletion_StorageClientErrorKeepsFinalizer(t *testing.T) {
	env := newFileTestEnv(deletingBackup("Delete", &databasesv1alpha1.BackupStatus{Path: backupPath}), newTestStorage(testStorageName))
	env.files.AddObject(backupPath, []byte("dump"), metav1.Now().Time)
	env.clientBuildErr = errors.New("secret dbtether/s3-creds not found")

	if err := reconcileDeletion(env); err == nil {
		t.Fatal("expected the client error to be returned for a requeue")
	}
	requireFinalizerKept(t, env)
	env.requireEvent(t, EventReasonFileDeleteFailed, backupPath)
}

func TestBackupDeletion_StorageLookupErrorKeepsFinalizer(t *testing.T) {
	env := newFileTestEnv(deletingBackup("Delete", &databasesv1alpha1.BackupStatus{Path: backupPath}), newTestStorage(testStorageName))
	env.files.AddObject(backupPath, []byte("dump"), metav1.Now().Time)
	env.r.Client = interceptor.NewClient(env.r.Client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := obj.(*databasesv1alpha1.BackupStorage); ok {
				return errors.New("apiserver unavailable")
			}
			return c.Get(ctx, key, obj, opts...)
		},
	})

	if err := reconcileDeletion(env); err == nil {
		t.Fatal("expected the lookup error to be returned for a requeue")
	}

	requireFinalizerKept(t, env)
	env.requireEvent(t, EventReasonFileDeleteFailed, backupPath+": cannot read BackupStorage "+testStorageName+": apiserver unavailable")
	if !env.hasFile(backupPath) {
		t.Error("expected the file to stay in storage")
	}
}

func TestBackupDeletion_SwitchToRetainReleasesStuckDeletion(t *testing.T) {
	env := newFileTestEnv(deletingBackup("Delete", &databasesv1alpha1.BackupStatus{Path: backupPath}), newTestStorage(testStorageName))
	env.files.AddObject(backupPath, []byte("dump"), metav1.Now().Time)
	env.files.DeleteError = errors.New("access denied")

	if err := reconcileDeletion(env); err == nil {
		t.Fatal("expected the storage error to be returned for a requeue")
	}
	requireFinalizerKept(t, env)
	env.requireEvent(t, EventReasonFileDeleteFailed, backupPath)
	clientsBuiltWhileStuck := env.clientsBuilt

	var stuck databasesv1alpha1.Backup
	if err := env.r.Get(context.Background(), rerunRequest().NamespacedName, &stuck); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	stuck.Spec.DeletionPolicy = "Retain"
	if err := env.r.Update(context.Background(), &stuck); err != nil {
		t.Fatalf("failed to switch deletionPolicy: %v", err)
	}

	if err := reconcileDeletion(env); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	requireFinalizerRemoved(t, env)
	if env.clientsBuilt != clientsBuiltWhileStuck {
		t.Errorf("expected no storage access after switching to Retain, built %d more clients", env.clientsBuilt-clientsBuiltWhileStuck)
	}
	if !env.hasFile(backupPath) {
		t.Error("expected the file to stay in storage")
	}
}

func TestBackupDeletion_MissingStorageOrphansFile(t *testing.T) {
	env := newFileTestEnv(deletingBackup("Delete", &databasesv1alpha1.BackupStatus{Path: backupPath}))

	if err := reconcileDeletion(env); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	requireFinalizerRemoved(t, env)
	env.requireEvent(t, EventReasonFileOrphaned, backupPath)
	if env.clientsBuilt != 0 {
		t.Errorf("expected no storage client without a BackupStorage, built %d", env.clientsBuilt)
	}
}

func TestBackupDeletion_NoPathNothingToDelete(t *testing.T) {
	env := newFileTestEnv(deletingBackup("Delete", &databasesv1alpha1.BackupStatus{Phase: "Failed"}), newTestStorage(testStorageName))

	if err := reconcileDeletion(env); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	requireFinalizerRemoved(t, env)
	if env.clientsBuilt != 0 {
		t.Errorf("expected no storage access without a path, built %d clients", env.clientsBuilt)
	}
}

func TestBackupReRun_DeleteLeavesPreviousFile(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1
	backup.Spec.Trigger = "v1"
	backup.Spec.DeletionPolicy = "Delete"
	env := newFileTestEnv(
		backup,
		newTestDatabase(testDBName, testNamespace, testClusterName),
		newTestCluster(testClusterName),
		newTestStorage(testStorageName),
		newTestSecret(testSecretName, testOperatorNS),
	)
	req := rerunRequest()

	completeBackupRunWithPath(t, env.r, req, backupPath)
	env.files.AddObject(backupPath, []byte("dump"), metav1.Now().Time)
	completed := getBackup(t, env.r, req)
	retriggerBackup(t, env.r, &completed, "v2", 2)
	reconcileOnce(t, env.r, req)
	completeActiveRun(t, env.r, req)

	done := getBackup(t, env.r, req)
	if done.Status.Phase != "Completed" || done.Status.Path == backupPath {
		t.Fatalf("expected the re-run Completed with a new path, got phase %q path %q", done.Status.Phase, done.Status.Path)
	}
	if !env.hasFile(backupPath) {
		t.Error("expected the previous run's file to be left in storage")
	}
	if env.clientsBuilt != 0 {
		t.Errorf("expected no storage access during a re-run, built %d clients", env.clientsBuilt)
	}
}

func TestComputeSpecHash_IgnoresDeletionPolicy(t *testing.T) {
	r := &BackupReconciler{}
	backup := newTestBackup(testBackupName, testNamespace)
	unset := r.computeSpecHash(backup)

	for _, policy := range []string{"Retain", "Delete"} {
		backup.Spec.DeletionPolicy = policy
		if hash := r.computeSpecHash(backup); hash != unset {
			t.Errorf("deletionPolicy %q changed the spec hash %s -> %s; every Backup would re-run", policy, unset, hash)
		}
	}
}
