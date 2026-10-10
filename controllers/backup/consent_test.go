package backup

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
)

const (
	sourceNamespace  = "data"
	wantDeniedForApp = `database data/test-db does not list namespace "app-namespace" in spec.allowedNamespaces`
)

func crossNamespaceDatabase(allowed ...string) *dbtether.Database {
	db := newTestDatabase(testDBName, sourceNamespace, testClusterName)
	db.Status.DatabaseName = "data_db"
	db.Spec.AllowedNamespaces = allowed
	return db
}

func crossNamespaceBackup() *dbtether.Backup {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Spec.DatabaseRef.Namespace = sourceNamespace
	return backup
}

func backupJobs(t *testing.T, c client.Client) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	require.NoError(t, c.List(context.Background(), &jobs, client.InNamespace(testOperatorNS)))
	return jobs.Items
}

func jobEnv(job *batchv1.Job, name string) string {
	for _, env := range job.Spec.Template.Spec.Containers[0].Env {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}

func TestBackupReconciler_CrossNamespaceDatabaseAllowed(t *testing.T) {
	localDecoy := newTestDatabase(testDBName, testNamespace, testClusterName)
	localDecoy.Status.DatabaseName = "local_db"
	r := newTestReconciler(crossNamespaceBackup(), crossNamespaceDatabase(testNamespace), localDecoy,
		newTestCluster(testClusterName), newTestStorage(testStorageName), newTestSecret(testSecretName, testOperatorNS))

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKey{Name: testBackupName, Namespace: testNamespace}})
	require.NoError(t, err)

	jobs := backupJobs(t, r.Client)
	require.Len(t, jobs, 1)
	assert.Equal(t, "data_db", jobEnv(&jobs[0], "DB_NAME"), "backup must dump the Database in databaseRef.namespace")
}

func TestBackupReconciler_CrossNamespaceDatabaseDenied(t *testing.T) {
	r := newTestReconciler(crossNamespaceBackup(), crossNamespaceDatabase("other"),
		newTestCluster(testClusterName), newTestStorage(testStorageName), newTestSecret(testSecretName, testOperatorNS))

	key := client.ObjectKey{Name: testBackupName, Namespace: testNamespace}
	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: key})
	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter)

	var backup dbtether.Backup
	require.NoError(t, r.Get(context.Background(), key, &backup))
	assert.Equal(t, "Pending", backup.Status.Phase)
	assert.Equal(t, wantDeniedForApp, backup.Status.Message)
	assert.Empty(t, backupJobs(t, r.Client))
}

func crossNamespaceSchedule() *dbtether.BackupSchedule {
	keepLast := 1
	return &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "nightly",
			Namespace:         testNamespace,
			Finalizers:        []string{scheduleFinalizerName},
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: dbtether.BackupScheduleSpec{
			DatabaseRef: dbtether.DatabaseReference{Name: testDBName, Namespace: sourceNamespace},
			StorageRef:  dbtether.StorageReference{Name: testStorageName},
			Schedule:    testCronSchedule,
			Retention:   &dbtether.RetentionPolicy{KeepLast: &keepLast},
		},
	}
}

func scheduleBackups(t *testing.T, c client.Client) []dbtether.Backup {
	t.Helper()
	var backups dbtether.BackupList
	require.NoError(t, c.List(context.Background(), &backups, client.InNamespace(testNamespace)))
	return backups.Items
}

func TestScheduleReconcile_CrossNamespaceDatabaseDenied(t *testing.T) {
	schedule := crossNamespaceSchedule()
	r := newScheduleReconcilerWith(&interceptor.Funcs{}, schedule, crossNamespaceDatabase())

	result, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(schedule)})
	require.NoError(t, err)
	assert.Equal(t, schedulePendingRequeueDelay, result.RequeueAfter)

	var got dbtether.BackupSchedule
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(schedule), &got))
	assert.Equal(t, "Pending", got.Status.Phase)
	assert.Equal(t, wantDeniedForApp, got.Status.Message)
	assert.Empty(t, scheduleBackups(t, r.Client))
}

func TestScheduleReconcile_CrossNamespaceDatabaseAllowed(t *testing.T) {
	schedule := crossNamespaceSchedule()
	schedule.Spec.Retention = nil
	r := newScheduleReconcilerWith(&interceptor.Funcs{}, schedule, crossNamespaceDatabase(testNamespace))

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(schedule)})
	require.NoError(t, err)

	backups := scheduleBackups(t, r.Client)
	require.Len(t, backups, 1)
	assert.Equal(t, sourceNamespace, backups[0].Spec.DatabaseRef.Namespace)

	var got dbtether.BackupSchedule
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(schedule), &got))
	assert.Equal(t, "Active", got.Status.Phase)
}

func TestScheduleReconcile_CrossNamespaceDatabaseMissing(t *testing.T) {
	schedule := crossNamespaceSchedule()
	r := newScheduleReconcilerWith(&interceptor.Funcs{}, schedule)

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(schedule)})
	require.NoError(t, err)

	var got dbtether.BackupSchedule
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(schedule), &got))
	assert.Equal(t, "Pending", got.Status.Phase)
	assert.Equal(t, "database data/test-db not found", got.Status.Message)
	assert.Empty(t, scheduleBackups(t, r.Client))
}

func TestPrepareRetentionCleanup_DeniedDatabaseIsNotTouched(t *testing.T) {
	schedule := crossNamespaceSchedule()
	r := newScheduleReconcilerWith(&interceptor.Funcs{}, schedule, crossNamespaceDatabase(),
		newTestCluster(testClusterName), newTestStorage(testStorageName))

	storageClient, prefix, err := r.prepareRetentionCleanup(context.Background(), schedule, zap.NewNop().Sugar())

	require.Error(t, err)
	assert.True(t, isDependencyNotReady(err))
	assert.Nil(t, storageClient)
	assert.Empty(t, prefix)
}
