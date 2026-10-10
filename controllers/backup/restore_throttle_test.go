package backup

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const throttleMessageAtLimit = "waiting for other backup/restore jobs on cluster cluster1 (active: 1/1)"

func foreignBackupJob() *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foreign-backup",
			Namespace: "dbtether",
			Labels:    map[string]string{LabelCluster: "cluster1", LabelBackupName: "other"},
		},
		Status: batchv1.JobStatus{Active: 1},
	}
}

func finishForeignJob(t *testing.T, r *RestoreReconciler) {
	t.Helper()
	job := foreignBackupJob()
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(job), job))
	job.Status = batchv1.JobStatus{Succeeded: 1}
	require.NoError(t, r.Status().Update(t.Context(), job))
}

func TestRestoreReconciler_NewRunWaitsAtClusterLimitWithoutReserving(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage, foreignBackupJob())
	r.MaxConcurrentJobs = 1

	res, err := reconcileRestore(t, r)
	require.NoError(t, err)

	assert.Equal(t, RequeueDelayWhenThrottled, res.RequeueAfter)
	assert.Len(t, listRestoreJobs(t, r), 1, "only the foreign Job exists")
	got := getRestoreStatus(t, r)
	assert.Equal(t, "Pending", got.Phase)
	assert.Equal(t, throttleMessageAtLimit, got.Message)
	assert.Empty(t, got.RunID)
	assert.Empty(t, got.JobName)

	finishForeignJob(t, r)
	_, err = reconcileRestore(t, r)
	require.NoError(t, err)

	jobs := listRestoreJobs(t, r)
	require.Len(t, jobs, 2)
	got = getRestoreStatus(t, r)
	assert.Equal(t, "Running", got.Phase)
	assert.NotEmpty(t, got.RunID)
}

func TestRestoreReconciler_ReservedRunWaitsAtClusterLimitKeepingIdentity(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	reserveRestore(restore, "wait0001")
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage, foreignBackupJob())
	r.MaxConcurrentJobs = 1

	res, err := reconcileRestore(t, r)
	require.NoError(t, err)

	assert.Equal(t, RequeueDelayWhenThrottled, res.RequeueAfter)
	assert.Len(t, listRestoreJobs(t, r), 1, "only the foreign Job exists")
	got := getRestoreStatus(t, r)
	assert.Equal(t, "Pending", got.Phase)
	assert.Equal(t, throttleMessageAtLimit, got.Message)
	assert.Equal(t, "wait0001", got.RunID)
	assert.Equal(t, restoreJobName("my-restore", "wait0001"), got.JobName)

	finishForeignJob(t, r)
	_, err = reconcileRestore(t, r)
	require.NoError(t, err)

	got = getRestoreStatus(t, r)
	assert.Equal(t, "Running", got.Phase)
	assert.Equal(t, "wait0001", got.RunID)
	var created batchv1.Job
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Namespace: "dbtether", Name: restoreJobName("my-restore", "wait0001")}, &created))
}

func TestRestoreReconciler_OwnJobAtClusterLimitIsStillEvaluated(t *testing.T) {
	for name, tc := range map[string]struct {
		status    batchv1.JobStatus
		wantPhase string
	}{
		"active":    {batchv1.JobStatus{Active: 1}, "Running"},
		"succeeded": {batchv1.JobStatus{Succeeded: 1}, "Granting"},
	} {
		t.Run(name, func(t *testing.T) {
			restore, db, cluster, storage := newStartFixture()
			reserveRestore(restore, "own00001")
			own := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name:      restoreJobName("my-restore", "own00001"),
					Namespace: "dbtether",
					Labels: map[string]string{
						LabelCluster: "cluster1", LabelRestoreName: "my-restore",
						LabelRestoreNamespace: "app-ns", LabelRunID: "own00001",
					},
				},
				Status: tc.status,
			}
			r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage, own)
			r.MaxConcurrentJobs = 1

			_, _ = reconcileRestore(t, r)

			got := getRestoreStatus(t, r)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.NotContains(t, got.Message, "waiting for other")
		})
	}
}
