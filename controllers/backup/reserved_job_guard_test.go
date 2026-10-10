package backup

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestBackupReconciler_ReservedRunWaitsForFreeSlotBeforeCreatingJob(t *testing.T) {
	backup := reservedBackup("held0001")
	r := newRerunReconciler(backup)
	r.MaxConcurrentBackups = 1
	req := rerunRequest()
	require.NoError(t, r.Create(context.Background(), &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: "other-backup-job", Namespace: testOperatorNS,
			Labels: map[string]string{LabelCluster: testClusterName},
		},
		Status: batchv1.JobStatus{Active: 1},
	}))

	res, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, RequeueDelayWhenThrottled, res.RequeueAfter)
	assert.Len(t, listJobs(t, r), 1, "the cluster is at its limit; the reserved Job must not be created")
	got := getBackup(t, r, req)
	assert.Equal(t, "Pending", got.Status.Phase)
	assert.Equal(t, "held0001", got.Status.RunID)
	assert.Contains(t, got.Status.Message, "waiting for other backup/restore jobs on cluster ")
	assert.Contains(t, got.Status.Message, "(active: 1/1)")

	clearClusterThrottle(t, r)
	reconcileOnce(t, r, req)

	assert.Len(t, listJobs(t, r), 1)
	got = getBackup(t, r, req)
	assert.Equal(t, "Running", got.Status.Phase)
	assert.Equal(t, "held0001", got.Status.RunID)
}

func TestBackupReconciler_ForeignJobUnderReservedNameFailsRun(t *testing.T) {
	backup := reservedBackup("mine0001")
	r := newRerunReconciler(backup)
	foreign := finishedBackupJob(backup.Status.JobName, "foreign1")
	require.NoError(t, r.Create(context.Background(), foreign))
	var before batchv1.Job
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(foreign), &before))
	req := rerunRequest()

	reconcileOnce(t, r, req)

	got := getBackup(t, r, req)
	assert.Equal(t, "Failed", got.Status.Phase)
	assert.Equal(t, ReasonJobNameCollision, got.Status.FailureReason)
	jobs := listJobs(t, r)
	require.Len(t, jobs, 1)
	assert.Equal(t, before.ResourceVersion, jobs[0].ResourceVersion, "the foreign Job must be left untouched")
	assert.Equal(t, before.Spec.TTLSecondsAfterFinished, jobs[0].Spec.TTLSecondsAfterFinished)
}

func TestRestoreReconciler_ForeignJobUnderReservedNameFailsRun(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	_, _, _, _, secret := newRegrantFixture()
	secret.Namespace = cluster.Spec.CredentialsSecretRef.Namespace
	reserveRestore(restore, "mine0001")
	foreign := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      restore.Status.JobName,
			Namespace: "dbtether",
			Labels:    map[string]string{LabelRunID: "foreign1"},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage, secret, foreign)
	var before batchv1.Job
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(foreign), &before))

	_, err := reconcileRestore(t, r)
	require.NoError(t, err)

	got := getRestoreStatus(t, r)
	assert.Equal(t, "Failed", got.Phase)
	assert.Contains(t, got.Message, "name collision")
	jobs := listRestoreJobs(t, r)
	require.Len(t, jobs, 1)
	assert.Equal(t, before.ResourceVersion, jobs[0].ResourceVersion, "the foreign Job must be left untouched")
	assert.Equal(t, before.Spec.TTLSecondsAfterFinished, jobs[0].Spec.TTLSecondsAfterFinished)
}
