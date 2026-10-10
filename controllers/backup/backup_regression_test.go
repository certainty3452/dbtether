package backup

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

func reconcileBackup(t *testing.T, r *BackupReconciler, b *databasesv1alpha1.Backup) (reconcile.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)})
}

func listJobs(t *testing.T, r *BackupReconciler) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	require.NoError(t, r.List(context.Background(), &jobs, client.InNamespace(testOperatorNS)))
	return jobs.Items
}

func envByName(job *batchv1.Job) map[string]string {
	out := map[string]string{}
	for _, e := range job.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}

func TestBackupReconciler_JobLostAfterSuccessCompletesWithRecordedPath(t *testing.T) {
	const path = "c/app/20260301-000000.sql.gz"
	started := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.Phase = "Running"
	backup.Status.Path = path
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.StartedAt = &started
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)
	r := newTestReconciler(backup)

	_, err := reconcileBackup(t, r, backup)
	require.NoError(t, err)

	var got databasesv1alpha1.Backup
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(backup), &got))
	assert.Equal(t, "Completed", got.Status.Phase)
	assert.Equal(t, path, got.Status.Path)
}

func TestBackupReconciler_CredentialsSecretOutsideOperatorNamespaceCreatesNoJob(t *testing.T) {
	cases := map[string]func(*databasesv1alpha1.DBCluster, *databasesv1alpha1.BackupStorage){
		"cluster credentials": func(c *databasesv1alpha1.DBCluster, _ *databasesv1alpha1.BackupStorage) {
			c.Spec.CredentialsSecretRef.Namespace = "other-ns"
		},
		"storage credentials": func(_ *databasesv1alpha1.DBCluster, s *databasesv1alpha1.BackupStorage) {
			s.Spec.CredentialsSecretRef = &databasesv1alpha1.SecretReference{Name: "s3-creds", Namespace: "other-ns"}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			backup := newTestBackup(testBackupName, testNamespace)
			backup.Finalizers = []string{backupFinalizer}
			cluster := newTestCluster(testClusterName)
			stor := newTestStorage(testStorageName)
			mutate(cluster, stor)
			r := newTestReconciler(backup, newTestDatabase(testDBName, testNamespace, testClusterName), cluster, stor)

			_, _ = reconcileBackup(t, r, backup)

			assert.Zero(t, len(listJobs(t, r)), "Job env resolves the Secret by name in the operator namespace only")
			var got databasesv1alpha1.Backup
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(backup), &got))
			assert.Contains(t, []string{"Pending", "Failed"}, got.Status.Phase)
			assert.Contains(t, got.Status.Message, "other-ns")
			assert.Contains(t, got.Status.Message, testOperatorNS)
		})
	}
}

func TestBackupReconciler_JobEnvLetsRunnerBuildNonS3Client(t *testing.T) {
	cases := map[string]struct {
		storage func(*databasesv1alpha1.BackupStorage)
		want    map[string]string
	}{
		"gcs": {
			storage: func(s *databasesv1alpha1.BackupStorage) {
				s.Spec.S3 = nil
				s.Spec.GCS = &databasesv1alpha1.GCSStorageConfig{Bucket: "gb", Project: "gp"}
			},
			want: map[string]string{"STORAGE_TYPE": "gcs", "GCS_BUCKET": "gb", "GCS_PROJECT": "gp"},
		},
		"azure": {
			storage: func(s *databasesv1alpha1.BackupStorage) {
				s.Spec.S3 = nil
				s.Spec.Azure = &databasesv1alpha1.AzureStorageConfig{Container: "ac", StorageAccount: "aa"}
			},
			want: map[string]string{"STORAGE_TYPE": "azure", "AZURE_CONTAINER": "ac", "AZURE_ACCOUNT": "aa"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			backup := newTestBackup(testBackupName, testNamespace)
			backup.Finalizers = []string{backupFinalizer}
			stor := newTestStorage(testStorageName)
			tc.storage(stor)
			r := newTestReconciler(backup, newTestDatabase(testDBName, testNamespace, testClusterName),
				newTestCluster(testClusterName), stor, newTestSecret(testSecretName, testOperatorNS))

			_, err := reconcileBackup(t, r, backup)
			require.NoError(t, err)

			jobs := listJobs(t, r)
			require.Len(t, jobs, 1)
			env := envByName(&jobs[0])
			for k, v := range tc.want {
				assert.Equal(t, v, env[k], k)
			}
		})
	}
}

func TestBackupReconciler_JobNameAndLabelsStayValidForLongBackupNames(t *testing.T) {
	sched := &BackupScheduleReconciler{}
	scheduleDerived := sched.generateBackupName(
		&databasesv1alpha1.BackupSchedule{ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("s", 40)}},
		time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC),
	)
	names := map[string]string{
		"50 chars":         strings.Repeat("b", 50),
		"100 chars":        strings.Repeat("b", 100),
		"from 40-char sch": scheduleDerived,
	}
	for label, name := range names {
		t.Run(label, func(t *testing.T) {
			backup := newTestBackup(name, testNamespace)
			backup.Finalizers = []string{backupFinalizer}
			r := newTestReconciler(backup, newTestDatabase(testDBName, testNamespace, testClusterName),
				newTestCluster(testClusterName), newTestStorage(testStorageName), newTestSecret(testSecretName, testOperatorNS))

			_, err := reconcileBackup(t, r, backup)
			require.NoError(t, err)

			jobs := listJobs(t, r)
			require.Len(t, jobs, 1, "no Job created for a valid Backup")
			job := jobs[0]
			assert.Empty(t, validation.IsDNS1123Label(job.Name), "job name %q", job.Name)
			for _, lbls := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
				for k, v := range lbls {
					assert.Empty(t, validation.IsValidLabelValue(v), "label %s=%q", k, v)
				}
			}
		})
	}
}

func TestBackupReconciler_EnvOnlyClusterCredentialsCreateNoJob(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	cluster := newTestCluster(testClusterName)
	cluster.Spec.CredentialsSecretRef = nil
	cluster.Spec.CredentialsFromEnv = &databasesv1alpha1.CredentialsFromEnv{Username: "PG_USER", Password: "PG_PASSWORD"}
	r := newTestReconciler(backup, newTestDatabase(testDBName, testNamespace, testClusterName), cluster, newTestStorage(testStorageName))

	res, err := reconcileBackup(t, r, backup)
	require.NoError(t, err)

	assert.Zero(t, len(listJobs(t, r)))
	var got databasesv1alpha1.Backup
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(backup), &got))
	assert.Equal(t, "Pending", got.Status.Phase)
	assert.Contains(t, got.Status.Message, "credentialsSecretRef")
	assert.Contains(t, got.Status.Message, testOperatorNS)
	assert.Positive(t, res.RequeueAfter)
}

func TestBackupThrottlingCountsRestoreJobsOfLongNamedCluster(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	cluster.Name = strings.Repeat("c", 70)
	restore.Status = databasesv1alpha1.RestoreStatus{RunID: "run00001", JobName: restoreJobName(restore.Name, "run00001"), SourcePath: "p.sql.gz"}
	restoreJob := (&RestoreReconciler{Namespace: testOperatorNS}).buildRestoreJob(restore, db, cluster, storage)
	r := newTestReconciler(restoreJob)

	active, err := countActiveJobsForCluster(context.Background(), r.Client, r.Namespace, cluster.Name)
	require.NoError(t, err)
	assert.Equal(t, 1, active)
}

func TestBackupReconciler_AdoptingJobKeepsResultRecordedByJob(t *testing.T) {
	const path = "c/app/20260301-000000.sql.gz"
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1
	backup.Status.Phase = "Pending"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)
	backup.Status.RunID = testRunID
	backup.Status.JobName = testJobName
	backup.Status.Path = path
	backup.Status.Size = "12MB"
	backup.Status.Duration = "42s"
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testOperatorNS,
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
	}
	r := newTestReconciler(backup, job,
		newTestDatabase(testDBName, testNamespace, testClusterName),
		newTestCluster(testClusterName), newTestStorage(testStorageName), newTestSecret(testSecretName, testOperatorNS))
	req := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(backup)}

	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var adopted databasesv1alpha1.Backup
	require.NoError(t, r.Get(context.Background(), req.NamespacedName, &adopted))
	require.Equal(t, testJobName, adopted.Status.JobName)
	assert.Equal(t, path, adopted.Status.Path, "adoption wiped the path the Job recorded")
	assert.Equal(t, "12MB", adopted.Status.Size)
	assert.Equal(t, "42s", adopted.Status.Duration)

	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(job), job))
	job.Status.Succeeded = 1
	require.NoError(t, r.Status().Update(context.Background(), job))
	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, r.Get(context.Background(), req.NamespacedName, &adopted))
	assert.Equal(t, "Completed", adopted.Status.Phase)
	assert.Equal(t, path, adopted.Status.Path)
}

func TestBackupReconciler_SucceededJobWithoutRecordedPathIsNeverCompleted(t *testing.T) {
	started := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.Phase = "Running"
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.StartedAt = &started
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testOperatorNS,
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	r := newTestReconciler(backup, job)

	_, _ = reconcileBackup(t, r, backup)

	var got databasesv1alpha1.Backup
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(backup), &got))
	assert.False(t, got.Status.Phase == "Completed" && got.Status.Path == "", "Completed with empty status.path")
}

func runningBackupWithoutPath() *databasesv1alpha1.Backup {
	started := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.Phase = "Running"
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.StartedAt = &started
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)
	return backup
}

func succeededJob() *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testOperatorNS,
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
}

func readerWithRecordedPath(backup *databasesv1alpha1.Backup, path string) client.Reader {
	fresh := backup.DeepCopy()
	fresh.Status.Path = path
	fresh.Status.Size = "12MB"
	fresh.Status.Duration = "42s"
	return fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(fresh).Build()
}

func TestBackupReconciler_CompletesWithPathTheCacheHasNotSeenYet(t *testing.T) {
	const path = "c/app/20260301-000000.sql.gz"
	cases := map[string][]client.Object{
		"job succeeded": {succeededJob()},
		"job gone":      nil,
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			backup := runningBackupWithoutPath()
			r := newTestReconciler(append([]client.Object{backup}, extra...)...)
			r.APIReader = readerWithRecordedPath(backup, path)

			_, err := reconcileBackup(t, r, backup)
			require.NoError(t, err)

			var got databasesv1alpha1.Backup
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(backup), &got))
			assert.Equal(t, "Completed", got.Status.Phase)
			assert.Equal(t, path, got.Status.Path)
			assert.Equal(t, "12MB", got.Status.Size)
			assert.Equal(t, "42s", got.Status.Duration)
		})
	}
}

func TestBackupReconciler_SucceededJobWithoutPathFailsWithoutRetry(t *testing.T) {
	backup := runningBackupWithoutPath()
	r := newTestReconciler(backup, succeededJob(), newTestDatabase(testDBName, testNamespace, testClusterName),
		newTestCluster(testClusterName), newTestStorage(testStorageName), newTestSecret(testSecretName, testOperatorNS))
	recorder := record.NewFakeRecorder(10)
	r.Recorder = recorder

	_, err := reconcileBackup(t, r, backup)
	require.NoError(t, err)

	var got databasesv1alpha1.Backup
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(backup), &got))
	assert.Equal(t, "Failed", got.Status.Phase)
	assert.Equal(t, ReasonResultNotRecorded, got.Status.FailureReason)
	assert.Contains(t, got.Status.Message, "create a new Backup")
	assert.Contains(t, got.Status.Message, "spec.trigger")
	require.Len(t, recorder.Events, 1)
	assert.Contains(t, <-recorder.Events, "Warning "+ReasonResultNotRecorded+" ")

	_, err = reconcileBackup(t, r, backup)
	require.NoError(t, err)
	assert.Len(t, listJobs(t, r), 1, "a run without a recorded result must not start a new Job")
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(backup), &got))
	assert.Equal(t, "Failed", got.Status.Phase)
}

func TestBackupReconciler_JobGoneWithoutPathFailsWithoutRetry(t *testing.T) {
	backup := runningBackupWithoutPath()
	r := newTestReconciler(backup, newTestDatabase(testDBName, testNamespace, testClusterName),
		newTestCluster(testClusterName), newTestStorage(testStorageName), newTestSecret(testSecretName, testOperatorNS))

	_, err := reconcileBackup(t, r, backup)
	require.NoError(t, err)

	var got databasesv1alpha1.Backup
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(backup), &got))
	assert.Equal(t, "Failed", got.Status.Phase)
	assert.Equal(t, "JobNotFound", got.Status.FailureReason)
	assert.Empty(t, got.Status.Path)

	_, err = reconcileBackup(t, r, backup)
	require.NoError(t, err)
	assert.Empty(t, listJobs(t, r))
}

func finishedBackupJob(name, runID string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testOperatorNS,
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
				LabelCluster:         testClusterName,
				LabelRunID:           runID,
			},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
}

func reservedBackup(runID string) *databasesv1alpha1.Backup {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.Phase = "Pending"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)
	backup.Status.RunID = runID
	backup.Status.JobName = backupJobName(testBackupName, runID)
	return backup
}

func TestBackupReconciler_JobFinishedBeforeRunningStatusWriteIsAdoptedNotDuplicated(t *testing.T) {
	const path = "c/app/20260301-000000.sql.gz"
	backup := reservedBackup("orphan01")
	backup.Status.Path = path
	r := newRerunReconciler(backup)
	require.NoError(t, r.Create(context.Background(), finishedBackupJob(backup.Status.JobName, "orphan01")))
	req := rerunRequest()

	reconcileOnce(t, r, req)
	reconcileOnce(t, r, req)

	assert.Len(t, listBackupJobs(t, r), 1, "the finished Job is this run's Job; a second one would dump the database again")
	got := getBackup(t, r, req)
	assert.Equal(t, "Completed", got.Status.Phase)
	assert.Equal(t, path, got.Status.Path)
}

func TestBackupReconciler_FinishedJobOfPreviousTriggerIsNotAdopted(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Spec.Trigger = "v1"
	backup.Generation = 1
	r := newRerunReconciler(backup)
	req := rerunRequest()
	completeBackupRunWithPath(t, r, req, "c/app/first.sql.gz")
	require.Equal(t, "Completed", getBackup(t, r, req).Status.Phase)
	firstJob := listBackupJobs(t, r)[0]

	current := getBackup(t, r, req)
	retriggerBackup(t, r, &current, "v2", 2)
	reconcileOnce(t, r, req)

	assert.Len(t, listBackupJobs(t, r), 2, "the new trigger starts a new Job next to the finished one")
	got := getBackup(t, r, req)
	assert.Equal(t, "Running", got.Status.Phase)
	assert.NotEqual(t, firstJob.Name, got.Status.JobName)
}

func TestBackupReconciler_StaleReadCannotReserveSecondRun(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	var stale *databasesv1alpha1.Backup
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithObjects(backup, newTestDatabase(testDBName, testNamespace, testClusterName), newTestCluster(testClusterName),
			newTestStorage(testStorageName), newTestSecret(testSecretName, testOperatorNS)).
		WithStatusSubresource(&databasesv1alpha1.Backup{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if b, ok := obj.(*databasesv1alpha1.Backup); ok && stale != nil {
					stale.DeepCopyInto(b)
					return nil
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).Build()
	r := &BackupReconciler{Client: c, APIReader: c, Scheme: c.Scheme(), Namespace: testOperatorNS, Image: testImage}
	req := rerunRequest()
	beforeReservation := getBackup(t, r, req)

	reconcileOnce(t, r, req)
	reserved := getBackup(t, r, req)
	require.NotEmpty(t, reserved.Status.RunID)

	stale = &beforeReservation
	res, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)
	assert.Equal(t, rerunRequeueDelay, res.RequeueAfter)

	stale = nil
	got := getBackup(t, r, req)
	assert.Equal(t, reserved.Status.RunID, got.Status.RunID, "a reconcile on a stale read replaced the reservation")
	assert.Equal(t, reserved.Status.JobName, got.Status.JobName)
	assert.Len(t, listJobs(t, r), 1)
}

func TestBackupReconciler_VanishedJobIsNeverRecreated(t *testing.T) {
	const path = "c/app/20260301-000000.sql.gz"
	cases := map[string]struct {
		recordedPath, phase, failureReason string
	}{
		"result recorded":     {recordedPath: path, phase: "Completed"},
		"result not recorded": {phase: "Failed", failureReason: "JobNotFound"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			backup := newTestBackup(testBackupName, testNamespace)
			backup.Finalizers = []string{backupFinalizer}
			r := newRerunReconciler(backup)
			req := rerunRequest()
			reconcileOnce(t, r, req)
			run := getBackup(t, r, req)
			require.Equal(t, "Running", run.Status.Phase)
			if tc.recordedPath != "" {
				run.Status.Path = tc.recordedPath
				require.NoError(t, r.Status().Update(context.Background(), &run))
			}
			jobs := listJobs(t, r)
			require.Len(t, jobs, 1)
			require.NoError(t, r.Delete(context.Background(), &jobs[0]))

			reconcileOnce(t, r, req)
			reconcileOnce(t, r, req)

			got := getBackup(t, r, req)
			assert.Equal(t, tc.phase, got.Status.Phase)
			assert.Equal(t, tc.recordedPath, got.Status.Path)
			assert.Equal(t, tc.failureReason, got.Status.FailureReason)
			assert.Equal(t, run.Status.RunID, got.Status.RunID)
			assert.Empty(t, listJobs(t, r), "a Job that existed for this run must not be created again")
		})
	}
}

func TestBackupReconciler_ReservedRunWithoutJobEvidenceCreatesReservedJob(t *testing.T) {
	r := newRerunReconciler(reservedBackup("never001"))
	req := rerunRequest()

	reconcileOnce(t, r, req)
	reconcileOnce(t, r, req)

	jobs := listJobs(t, r)
	require.Len(t, jobs, 1)
	assert.Equal(t, backupJobName(testBackupName, "never001"), jobs[0].Name)
	assert.Equal(t, "never001", envByName(&jobs[0])["RUN_ID"])
	assert.Equal(t, "never001", jobs[0].Labels[LabelRunID])
	got := getBackup(t, r, req)
	assert.Equal(t, "Running", got.Status.Phase)
	assert.Equal(t, "never001", got.Status.RunID)
}

func TestBackupReconciler_ReservedRunWithJobEvidenceIsNotRecreated(t *testing.T) {
	const path = "c/app/20260301-000000.sql.gz"
	started := metav1.Now()
	cases := map[string]struct {
		evidence      func(*databasesv1alpha1.BackupStatus)
		phase, reason string
	}{
		"job recorded its start": {evidence: func(s *databasesv1alpha1.BackupStatus) { s.StartedAt = &started }, phase: "Failed", reason: "JobNotFound"},
		"job recorded its path":  {evidence: func(s *databasesv1alpha1.BackupStatus) { s.Path = path }, phase: "Completed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			backup := reservedBackup("ran00001")
			tc.evidence(&backup.Status)
			r := newRerunReconciler(backup)
			req := rerunRequest()

			reconcileOnce(t, r, req)
			reconcileOnce(t, r, req)

			assert.Empty(t, listJobs(t, r))
			got := getBackup(t, r, req)
			assert.Equal(t, tc.phase, got.Status.Phase)
			assert.Equal(t, tc.reason, got.Status.FailureReason)
		})
	}
}

func TestBackupReconciler_StaleReservationIsNotRecreatedOverFreshEvidence(t *testing.T) {
	backup := reservedBackup("ran00002")
	r := newRerunReconciler(backup)
	fresh := backup.DeepCopy()
	reader := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(fresh).
		WithStatusSubresource(&databasesv1alpha1.Backup{}).Build()
	started := metav1.Now()
	fresh.Status.StartedAt = &started
	require.NoError(t, reader.Status().Update(context.Background(), fresh))
	r.APIReader = reader

	res, err := reconcileBackup(t, r, backup)
	require.NoError(t, err)

	assert.Positive(t, res.RequeueAfter)
	assert.Empty(t, listJobs(t, r), "the cache hid that this run's Job already started")
}

func TestBackupReconciler_ReservationUnderOlderSpecIsReplacedBeforeCreatingJob(t *testing.T) {
	backup := reservedBackup("stale001")
	backup.Spec.Trigger = "v2"
	r := newRerunReconciler(backup)
	req := rerunRequest()

	reconcileOnce(t, r, req)
	reconcileOnce(t, r, req)

	jobs := listJobs(t, r)
	require.Len(t, jobs, 1)
	got := getBackup(t, r, req)
	assert.Equal(t, "Running", got.Status.Phase)
	assert.NotEqual(t, "stale001", got.Status.RunID)
	assert.Equal(t, (&BackupReconciler{}).computeSpecHash(backup), got.Status.SpecHash)
	assert.Equal(t, got.Status.JobName, jobs[0].Name)
	assert.Equal(t, got.Status.RunID, jobs[0].Labels[LabelRunID])
}

func TestBackupReconciler_JobNameCollisionFailsWithoutTouchingForeignJob(t *testing.T) {
	cases := map[string]func(labels map[string]string){
		"another run":   func(labels map[string]string) { labels[LabelRunID] = "foreign1" },
		"another owner": func(labels map[string]string) { labels[LabelBackupNamespace] = "other-ns" },
	}
	for name, foreignize := range cases {
		t.Run(name, func(t *testing.T) {
			backup := newTestBackup(testBackupName, testNamespace)
			backup.Finalizers = []string{backupFinalizer}
			var foreign *batchv1.Job
			r, _ := newJobCreateInterceptReconciler(backup, func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				job, ok := obj.(*batchv1.Job)
				if !ok {
					return c.Create(ctx, obj, opts...)
				}
				if foreign == nil {
					foreign = job.DeepCopy()
					foreignize(foreign.Labels)
					if err := c.Create(ctx, foreign, opts...); err != nil {
						return err
					}
				}
				return apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, obj.GetName())
			})
			req := rerunRequest()

			reconcileOnce(t, r, req)
			reconcileOnce(t, r, req)

			got := getBackup(t, r, req)
			assert.Equal(t, "Failed", got.Status.Phase)
			assert.Equal(t, ReasonJobNameCollision, got.Status.FailureReason)
			assert.Empty(t, got.Status.JobName)
			jobs := listJobs(t, r)
			require.Len(t, jobs, 1)
			assert.Equal(t, foreign.ResourceVersion, jobs[0].ResourceVersion, "the foreign Job must be left untouched")
			assert.Equal(t, foreign.Labels, jobs[0].Labels)
		})
	}
}
