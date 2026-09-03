package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

func newRerunReconciler(backup *databasesv1alpha1.Backup) *BackupReconciler {
	return newTestReconciler(
		backup,
		newTestDatabase(testDBName, testNamespace, testClusterName),
		newTestCluster(testClusterName),
		newTestStorage(testStorageName),
		newTestSecret(testSecretName, testOperatorNS),
	)
}

func rerunRequest() reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Name: testBackupName, Namespace: testNamespace}}
}

func reconcileOnce(t *testing.T, r *BackupReconciler, req reconcile.Request) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
}

func getBackup(t *testing.T, r *BackupReconciler, req reconcile.Request) databasesv1alpha1.Backup {
	t.Helper()
	var backup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &backup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	return backup
}

func listBackupJobs(t *testing.T, r *BackupReconciler) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(testOperatorNS)); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	return jobs.Items
}

func succeedJob(t *testing.T, r *BackupReconciler, job *batchv1.Job) {
	t.Helper()
	job.Status.Succeeded = 1
	if err := r.Status().Update(context.Background(), job); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
}

func retriggerBackup(t *testing.T, r *BackupReconciler, backup *databasesv1alpha1.Backup, trigger string, generation int64) {
	t.Helper()
	backup.Spec.Trigger = trigger
	backup.Generation = generation // the fake client never bumps Generation on Update, so tests set it directly
	if err := r.Update(context.Background(), backup); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
}

func completeBackupRun(t *testing.T, r *BackupReconciler, req reconcile.Request) {
	t.Helper()
	reconcileOnce(t, r, req)
	jobs := listBackupJobs(t, r)
	if len(jobs) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs))
	}
	succeedJob(t, r, &jobs[0])
	reconcileOnce(t, r, req)
}

func TestBackupReconciler_NoRerunWhenSpecUnchanged(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1

	r := newRerunReconciler(backup)
	req := rerunRequest()

	completeBackupRun(t, r, req)
	firstJobName := listBackupJobs(t, r)[0].Name
	settled := getBackup(t, r, req)

	reconcileOnce(t, r, req)

	jobs := listBackupJobs(t, r)
	if len(jobs) != 1 {
		t.Fatalf("expected no new job, got %d jobs", len(jobs))
	}
	if jobs[0].Name != firstJobName {
		t.Errorf("expected job %s to be kept, got %s", firstJobName, jobs[0].Name)
	}

	after := getBackup(t, r, req)
	if after.Status.Phase != "Completed" {
		t.Errorf(errExpectedPhase, "Completed", after.Status.Phase)
	}
	if !after.Status.StartedAt.Equal(settled.Status.StartedAt) {
		t.Errorf("expected status left untouched, startedAt moved %v -> %v", settled.Status.StartedAt, after.Status.StartedAt)
	}
	if after.ResourceVersion != settled.ResourceVersion {
		t.Errorf("expected no status patch, resourceVersion moved %s -> %s", settled.ResourceVersion, after.ResourceVersion)
	}
}

func TestBackupReconciler_TriggerChangeStartsNewRun(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1
	backup.Spec.Trigger = "v1"

	r := newRerunReconciler(backup)
	req := rerunRequest()

	completeBackupRun(t, r, req)
	firstJobName := listBackupJobs(t, r)[0].Name

	completed := getBackup(t, r, req)
	retriggerBackup(t, r, &completed, "v2", 2)
	reconcileOnce(t, r, req)

	if jobs := listBackupJobs(t, r); len(jobs) != 2 {
		t.Fatalf("expected a new job after trigger change, got %d jobs", len(jobs))
	}

	updated := getBackup(t, r, req)
	if updated.Status.JobName == firstJobName {
		t.Errorf("expected a new job name, still %s", firstJobName)
	}
	if updated.Status.Phase != "Running" {
		t.Errorf(errExpectedPhase, "Running", updated.Status.Phase)
	}
	if updated.Status.ObservedGeneration != updated.Generation {
		t.Errorf("expected observedGeneration %d, got %d", updated.Generation, updated.Status.ObservedGeneration)
	}
}

func TestBackupReconciler_TriggerChangeWaitsForActiveRun(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1
	backup.Spec.Trigger = "v1"

	r := newRerunReconciler(backup)
	req := rerunRequest()

	reconcileOnce(t, r, req)
	running := getBackup(t, r, req)
	firstHash := running.Status.SpecHash
	firstJobName := running.Status.JobName

	retriggerBackup(t, r, &running, "v2", 2)
	reconcileOnce(t, r, req)

	activeJobs := listBackupJobs(t, r)
	if len(activeJobs) != 1 {
		t.Fatalf("expected no second job while the first is active, got %d", len(activeJobs))
	}
	if phase := getBackup(t, r, req).Status.Phase; phase != "Running" {
		t.Errorf(errExpectedPhase, "Running", phase)
	}

	succeedJob(t, r, &activeJobs[0])
	reconcileOnce(t, r, req)

	completed := getBackup(t, r, req)
	if completed.Status.Phase != "Completed" {
		t.Fatalf(errExpectedPhase, "Completed", completed.Status.Phase)
	}
	if completed.Status.SpecHash != firstHash {
		t.Errorf("expected the run's own specHash %s, got %s", firstHash, completed.Status.SpecHash)
	}
	if completed.Status.ObservedGeneration != 1 {
		t.Errorf("expected observedGeneration 1, got %d", completed.Status.ObservedGeneration)
	}
	if completed.Generation != 2 {
		t.Fatalf("expected generation 2, got %d", completed.Generation)
	}

	reconcileOnce(t, r, req)

	if jobs := listBackupJobs(t, r); len(jobs) != 2 {
		t.Fatalf("expected a second job after the first run ended, got %d", len(jobs))
	}

	rerun := getBackup(t, r, req)
	if rerun.Status.Phase != "Running" {
		t.Errorf(errExpectedPhase, "Running", rerun.Status.Phase)
	}
	if rerun.Status.JobName == firstJobName {
		t.Errorf("expected a new job name, still %s", firstJobName)
	}
	if rerun.Status.ObservedGeneration != 2 {
		t.Errorf("expected observedGeneration 2, got %d", rerun.Status.ObservedGeneration)
	}
}

func ageRunStart(t *testing.T, r *BackupReconciler, req reconcile.Request) {
	t.Helper()
	backup := getBackup(t, r, req)
	started := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	backup.Status.StartedAt = &started
	if err := r.Status().Update(context.Background(), &backup); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
}

func TestBackupReconciler_LostJobKeepsRunStartStamps(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1
	backup.Spec.Trigger = "v1"

	r := newRerunReconciler(backup)
	req := rerunRequest()

	reconcileOnce(t, r, req)
	running := getBackup(t, r, req)
	firstHash := running.Status.SpecHash
	firstJobName := running.Status.JobName

	jobs := listBackupJobs(t, r)
	if err := r.Delete(context.Background(), &jobs[0]); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	ageRunStart(t, r, req)

	current := getBackup(t, r, req)
	retriggerBackup(t, r, &current, "v2", 2)
	reconcileOnce(t, r, req)

	lost := getBackup(t, r, req)
	if lost.Status.Phase != "Failed" {
		t.Fatalf(errExpectedPhase, "Failed", lost.Status.Phase)
	}
	if lost.Status.SpecHash != firstHash {
		t.Errorf("expected the run's own specHash %s, got %s", firstHash, lost.Status.SpecHash)
	}
	if lost.Status.ObservedGeneration != 1 {
		t.Errorf("expected observedGeneration 1, got %d", lost.Status.ObservedGeneration)
	}

	reconcileOnce(t, r, req)

	if jobs := listBackupJobs(t, r); len(jobs) != 1 {
		t.Fatalf("expected the new run to start, got %d jobs", len(jobs))
	}
	rerun := getBackup(t, r, req)
	if rerun.Status.Phase != "Running" {
		t.Errorf(errExpectedPhase, "Running", rerun.Status.Phase)
	}
	if rerun.Status.JobName == firstJobName {
		t.Errorf("expected a new job name, still %s", firstJobName)
	}
	if rerun.Status.ObservedGeneration != 2 {
		t.Errorf("expected observedGeneration 2, got %d", rerun.Status.ObservedGeneration)
	}
}

func TestBackupReconciler_AdoptsUnlabeledActiveJob(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1

	orphan := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-orphan01",
			Namespace: testOperatorNS,
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
			},
		},
	}

	r := newTestReconciler(
		backup,
		newTestDatabase(testDBName, testNamespace, testClusterName),
		newTestCluster(testClusterName),
		newTestStorage(testStorageName),
		newTestSecret(testSecretName, testOperatorNS),
		orphan,
	)
	req := rerunRequest()

	reconcileOnce(t, r, req)

	if jobs := listBackupJobs(t, r); len(jobs) != 1 {
		t.Fatalf("expected the active job to be adopted, got %d jobs", len(jobs))
	}

	adopted := getBackup(t, r, req)
	if adopted.Status.JobName != orphan.Name {
		t.Errorf("expected job %s to be adopted, got %s", orphan.Name, adopted.Status.JobName)
	}
	if adopted.Status.Phase != "Running" {
		t.Errorf(errExpectedPhase, "Running", adopted.Status.Phase)
	}
	if adopted.Status.RunID != "" {
		t.Errorf("expected empty RunID for a Job without RUN_ID, got %q", adopted.Status.RunID)
	}
	if adopted.Status.ObservedGeneration != adopted.Generation {
		t.Errorf("expected observedGeneration %d, got %d", adopted.Generation, adopted.Status.ObservedGeneration)
	}
}

func TestBackupReconciler_AdoptsActiveJobWithRunID(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1

	orphan := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-orphan02",
			Namespace: testOperatorNS,
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "backup", Env: []corev1.EnvVar{{Name: "RUN_ID", Value: "orphanid1"}}},
					},
				},
			},
		},
	}

	r := newTestReconciler(
		backup,
		newTestDatabase(testDBName, testNamespace, testClusterName),
		newTestCluster(testClusterName),
		newTestStorage(testStorageName),
		newTestSecret(testSecretName, testOperatorNS),
		orphan,
	)
	req := rerunRequest()

	reconcileOnce(t, r, req)

	adopted := getBackup(t, r, req)
	if adopted.Status.RunID != "orphanid1" {
		t.Errorf("expected RunID from the Job's RUN_ID env, got %q", adopted.Status.RunID)
	}
}

func TestBackupReconciler_FinishedJobWithCurrentHashNotAdopted(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 2
	backup.Spec.Trigger = "v2"

	currentHash := (&BackupReconciler{}).computeSpecHash(backup)
	backup.Status.Phase = "Completed"
	backup.Status.SpecHash = "stale-older-hash"
	backup.Status.ObservedGeneration = 1

	finishedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-finished1",
			Namespace: testOperatorNS,
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
				LabelCluster:         testClusterName,
				LabelSpecHash:        currentHash,
			},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}

	r := newTestReconciler(
		backup,
		newTestDatabase(testDBName, testNamespace, testClusterName),
		newTestCluster(testClusterName),
		newTestStorage(testStorageName),
		newTestSecret(testSecretName, testOperatorNS),
		finishedJob,
	)
	req := rerunRequest()

	reconcileOnce(t, r, req)

	jobs := listBackupJobs(t, r)
	if len(jobs) != 2 {
		t.Fatalf("expected a new job alongside the finished one, got %d jobs", len(jobs))
	}

	updated := getBackup(t, r, req)
	if updated.Status.JobName == finishedJob.Name {
		t.Errorf("expected a new job, got the finished one %s adopted", finishedJob.Name)
	}
	if updated.Status.Phase != "Running" {
		t.Errorf(errExpectedPhase, "Running", updated.Status.Phase)
	}
}

// A changed golden means every existing Backup would re-run after an operator upgrade.
func TestComputeSpecHash_Golden(t *testing.T) {
	backoffLimit := int32(5)
	deadline := int64(1800)
	failedTTL := int32(7200)
	ttl := metav1.Duration{Duration: 2 * time.Hour}

	backup := &databasesv1alpha1.Backup{
		Spec: databasesv1alpha1.BackupSpec{
			DatabaseRef:        databasesv1alpha1.DatabaseReference{Name: "orders-db", Namespace: "team-a"},
			StorageRef:         databasesv1alpha1.StorageReference{Name: "prod-storage"},
			FilenameTemplate:   "{{ .Timestamp }}.sql.gz",
			Trigger:            "v1",
			TTLAfterCompletion: &ttl,
			JobConfig: &databasesv1alpha1.BackupJobConfig{
				BackoffLimit:          &backoffLimit,
				ActiveDeadlineSeconds: &deadline,
				TTLSecondsAfterFailed: &failedTTL,
			},
		},
	}

	const golden = "756cdc6370d072fb"
	if hash := (&BackupReconciler{}).computeSpecHash(backup); hash != golden {
		t.Errorf("computeSpecHash() = %q, want golden %q", hash, golden)
	}
}

func TestComputeSpecHash_Golden_NoTrigger(t *testing.T) {
	backoffLimit := int32(5)
	deadline := int64(1800)
	failedTTL := int32(7200)
	ttl := metav1.Duration{Duration: 2 * time.Hour}

	backup := &databasesv1alpha1.Backup{
		Spec: databasesv1alpha1.BackupSpec{
			DatabaseRef:        databasesv1alpha1.DatabaseReference{Name: "orders-db", Namespace: "team-a"},
			StorageRef:         databasesv1alpha1.StorageReference{Name: "prod-storage"},
			FilenameTemplate:   "{{ .Timestamp }}.sql.gz",
			TTLAfterCompletion: &ttl,
			JobConfig: &databasesv1alpha1.BackupJobConfig{
				BackoffLimit:          &backoffLimit,
				ActiveDeadlineSeconds: &deadline,
				TTLSecondsAfterFailed: &failedTTL,
			},
		},
	}

	const golden = "e957f3cb28a4b0a0"
	if hash := (&BackupReconciler{}).computeSpecHash(backup); hash != golden {
		t.Errorf("computeSpecHash() = %q, want golden %q", hash, golden)
	}
}

func completeBackupRunWithPath(t *testing.T, r *BackupReconciler, req reconcile.Request, path string) {
	t.Helper()
	reconcileOnce(t, r, req)
	jobs := listBackupJobs(t, r)
	if len(jobs) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs))
	}
	job := &jobs[0]
	job.Annotations = map[string]string{annotationBackupPath: path}
	if err := r.Update(context.Background(), job); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	job.Status.Succeeded = 1
	if err := r.Status().Update(context.Background(), job); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	reconcileOnce(t, r, req)
}

func listBackupOwnedJobs(t *testing.T, r *BackupReconciler, name string) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(testOperatorNS), client.MatchingLabels{LabelBackupName: name}); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	return jobs.Items
}

// saturateClusterThrottle creates unlabeled Jobs on testClusterName so the next reconcile throttles.
func saturateClusterThrottle(t *testing.T, r *BackupReconciler) {
	t.Helper()
	for i := 0; i < DefaultMaxConcurrentJobsPerCluster; i++ {
		blocker := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "throttle-blocker-" + string(rune('a'+i)),
				Namespace: testOperatorNS,
				Labels:    map[string]string{LabelCluster: testClusterName},
			},
			Status: batchv1.JobStatus{Active: 1},
		}
		if err := r.Create(context.Background(), blocker); err != nil {
			t.Fatalf(errUnexpectedError, err)
		}
	}
}

// clearClusterThrottle deletes only the synthetic blockers, leaving the backup's own Jobs alone.
func clearClusterThrottle(t *testing.T, r *BackupReconciler) {
	t.Helper()
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(testOperatorNS), client.MatchingLabels{LabelCluster: testClusterName}); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if _, ownedByBackup := job.Labels[LabelBackupName]; ownedByBackup {
			continue
		}
		if err := r.Delete(context.Background(), job); err != nil {
			t.Fatalf(errUnexpectedError, err)
		}
	}
}

func assertPendingWithNewHash(t *testing.T, backup *databasesv1alpha1.Backup, oldHash string) {
	t.Helper()
	if backup.Status.Phase != "Pending" {
		t.Fatalf(errExpectedPhase, "Pending", backup.Status.Phase)
	}
	if backup.Status.SpecHash == "" || backup.Status.SpecHash == oldHash {
		t.Errorf("expected the new spec hash, got %q (old was %q)", backup.Status.SpecHash, oldHash)
	}
	if backup.Status.JobName != "" {
		t.Errorf("expected no JobName from the old run, got %q", backup.Status.JobName)
	}
	if backup.Status.Path != "" {
		t.Errorf("expected no Path from the old run, got %q", backup.Status.Path)
	}
}

// TestBackupReconciler_MarkPending_Throttled covers marking a re-triggered, throttled backup Pending,
// with either a stale finished Job from the previous run still around, or none at all.
func TestBackupReconciler_MarkPending_Throttled(t *testing.T) {
	tests := []struct {
		name              string
		deleteFinishedJob bool
		expectedJobCount  int
	}{
		{name: "stale finished job present", deleteFinishedJob: false, expectedJobCount: 2},
		{name: "no job", deleteFinishedJob: true, expectedJobCount: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := newTestBackup(testBackupName, testNamespace)
			backup.Finalizers = []string{backupFinalizer}
			backup.Generation = 1

			r := newRerunReconciler(backup)
			req := rerunRequest()

			completeBackupRunWithPath(t, r, req, "cluster/db/run1.sql.gz")
			completed := getBackup(t, r, req)
			oldHash := completed.Status.SpecHash

			if tt.deleteFinishedJob {
				for _, job := range listBackupJobs(t, r) {
					if err := r.Delete(context.Background(), &job); err != nil {
						t.Fatalf(errUnexpectedError, err)
					}
				}
			}

			saturateClusterThrottle(t, r)
			retriggerBackup(t, r, &completed, "v2", 2)
			reconcileOnce(t, r, req)

			pending := getBackup(t, r, req)
			assertPendingWithNewHash(t, &pending, oldHash)

			clearClusterThrottle(t, r)
			reconcileOnce(t, r, req)

			if jobs := listBackupOwnedJobs(t, r, testBackupName); len(jobs) != tt.expectedJobCount {
				t.Fatalf("expected %d jobs, got %d", tt.expectedJobCount, len(jobs))
			}
			if phase := getBackup(t, r, req).Status.Phase; phase != "Running" {
				t.Errorf(errExpectedPhase, "Running", phase)
			}
		})
	}
}

func TestBackupReconciler_MarkPending_DependencyNotReady(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1

	r := newRerunReconciler(backup)
	req := rerunRequest()

	completeBackupRunWithPath(t, r, req, "cluster/db/run1.sql.gz")
	completed := getBackup(t, r, req)
	oldHash := completed.Status.SpecHash

	var db databasesv1alpha1.Database
	if err := r.Get(context.Background(), types.NamespacedName{Name: testDBName, Namespace: testNamespace}, &db); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	db.Status.Phase = "Pending"
	if err := r.Update(context.Background(), &db); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	retriggerBackup(t, r, &completed, "v2", 2)
	reconcileOnce(t, r, req)

	pending := getBackup(t, r, req)
	assertPendingWithNewHash(t, &pending, oldHash)

	db.Status.Phase = "Ready"
	if err := r.Update(context.Background(), &db); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	reconcileOnce(t, r, req)

	if jobs := listBackupOwnedJobs(t, r, testBackupName); len(jobs) != 2 {
		t.Fatalf("expected 2 jobs (the finished run plus the new one), got %d", len(jobs))
	}
	if phase := getBackup(t, r, req).Status.Phase; phase != "Running" {
		t.Errorf(errExpectedPhase, "Running", phase)
	}
}

func TestBackupReconciler_StartRunClearsPreviousResult(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1

	r := newRerunReconciler(backup)
	req := rerunRequest()

	completeBackupRunWithPath(t, r, req, "cluster/db/run1.sql.gz")
	completed := getBackup(t, r, req)
	if completed.Status.Path == "" || completed.Status.CompletedAt == nil {
		t.Fatalf("expected a completed run with a path and completedAt, got %+v", completed.Status)
	}

	// Fields a success run and a failure run each set separately; startRun must clear all of them regardless of which run set them.
	completed.Status.Size = "12 MB"
	completed.Status.Duration = "3s"
	completed.Status.FailureReason = "SomeReason"
	completed.Status.FailedAttempts = 2
	completed.Status.LastPodName = "backup-test-backup-pod"
	if err := r.Status().Update(context.Background(), &completed); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	retriggerBackup(t, r, &completed, "v2", 2)
	reconcileOnce(t, r, req)

	running := getBackup(t, r, req)
	if running.Status.Phase != "Running" {
		t.Fatalf(errExpectedPhase, "Running", running.Status.Phase)
	}
	if running.Status.Path != "" {
		t.Errorf("expected Path cleared on re-run, got %q", running.Status.Path)
	}
	if running.Status.Size != "" {
		t.Errorf("expected Size cleared on re-run, got %q", running.Status.Size)
	}
	if running.Status.Duration != "" {
		t.Errorf("expected Duration cleared on re-run, got %q", running.Status.Duration)
	}
	if running.Status.CompletedAt != nil {
		t.Errorf("expected CompletedAt cleared on re-run, got %v", running.Status.CompletedAt)
	}
	if running.Status.FailureReason != "" {
		t.Errorf("expected FailureReason cleared on re-run, got %q", running.Status.FailureReason)
	}
	if running.Status.FailedAttempts != 0 {
		t.Errorf("expected FailedAttempts cleared on re-run, got %d", running.Status.FailedAttempts)
	}
	if running.Status.LastPodName != "" {
		t.Errorf("expected LastPodName cleared on re-run, got %q", running.Status.LastPodName)
	}
}

func TestBackupReconciler_ControllerLabelsWinOverUserLabels(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}

	r := newRerunReconciler(backup)
	r.JobLabels = map[string]string{LabelSpecHash: "user-override"}
	r.PodLabels = map[string]string{LabelSpecHash: "user-override"}
	req := rerunRequest()

	reconcileOnce(t, r, req)

	jobs := listBackupJobs(t, r)
	if len(jobs) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs))
	}
	job := jobs[0]
	expectedHash := (&BackupReconciler{}).computeSpecHash(backup)
	if job.Labels[LabelSpecHash] != expectedHash {
		t.Errorf("expected controller-owned Job spec-hash label %q, got %q", expectedHash, job.Labels[LabelSpecHash])
	}
	if job.Spec.Template.Labels[LabelSpecHash] != expectedHash {
		t.Errorf("expected controller-owned pod spec-hash label %q, got %q", expectedHash, job.Spec.Template.Labels[LabelSpecHash])
	}
}

// failBackupJob marks a Job Failed via the JobFailed condition, the same signal evaluateJobStatus reads.
func failBackupJob(t *testing.T, r *BackupReconciler, job *batchv1.Job) {
	t.Helper()
	job.Status.Failed = 3
	job.Status.Conditions = []batchv1.JobCondition{
		{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded", Message: "boom"},
	}
	if err := r.Status().Update(context.Background(), job); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
}

func activeBackupJob(t *testing.T, r *BackupReconciler, req reconcile.Request) *batchv1.Job {
	t.Helper()
	backup := getBackup(t, r, req)
	var job batchv1.Job
	if err := r.Get(context.Background(), types.NamespacedName{Name: backup.Status.JobName, Namespace: testOperatorNS}, &job); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	return &job
}

func completeActiveRun(t *testing.T, r *BackupReconciler, req reconcile.Request) {
	t.Helper()
	succeedJob(t, r, activeBackupJob(t, r, req))
	reconcileOnce(t, r, req)
}

// TestBackupReconciler_JobResolvedUnderChangedSpecRequeues covers a run whose spec changed while its Job was
// still in flight, for each way that Job's outcome can resolve: it completes, it fails with structured info,
// or it is lost outright. All three must still requeue immediately to pick up the new spec.
func TestBackupReconciler_JobResolvedUnderChangedSpecRequeues(t *testing.T) {
	tests := []struct {
		name          string
		resolveJob    func(t *testing.T, r *BackupReconciler, req reconcile.Request, job *batchv1.Job)
		expectedPhase string
	}{
		{
			name:          "completed",
			resolveJob:    func(t *testing.T, r *BackupReconciler, _ reconcile.Request, job *batchv1.Job) { succeedJob(t, r, job) },
			expectedPhase: "Completed",
		},
		{
			name: "failed with info",
			resolveJob: func(t *testing.T, r *BackupReconciler, _ reconcile.Request, job *batchv1.Job) {
				failBackupJob(t, r, job)
			},
			expectedPhase: "Failed",
		},
		{
			name: "job lost",
			resolveJob: func(t *testing.T, r *BackupReconciler, req reconcile.Request, job *batchv1.Job) {
				if err := r.Delete(context.Background(), job); err != nil {
					t.Fatalf(errUnexpectedError, err)
				}
				ageRunStart(t, r, req)
			},
			expectedPhase: "Failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := newTestBackup(testBackupName, testNamespace)
			backup.Finalizers = []string{backupFinalizer}
			backup.Generation = 1

			r := newRerunReconciler(backup)
			req := rerunRequest()

			reconcileOnce(t, r, req)
			jobs := listBackupJobs(t, r)
			if len(jobs) != 1 {
				t.Fatalf(errExpectedOneJob, len(jobs))
			}

			tt.resolveJob(t, r, req, &jobs[0])

			running := getBackup(t, r, req)
			running.Spec.Trigger = "v2"
			running.Generation = 2
			if err := r.Update(context.Background(), &running); err != nil {
				t.Fatalf(errUnexpectedError, err)
			}

			result, err := r.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf(errUnexpectedError, err)
			}
			if result.RequeueAfter != rerunRequeueDelay {
				t.Errorf("expected RequeueAfter=%v, got %v", rerunRequeueDelay, result.RequeueAfter)
			}
			if phase := getBackup(t, r, req).Status.Phase; phase != tt.expectedPhase {
				t.Errorf(errExpectedPhase, tt.expectedPhase, phase)
			}
		})
	}
}

// jobCreateHook matches interceptor.Funcs.Create, letting tests script how the fake client's Job Create behaves.
type jobCreateHook func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error

// newJobCreateInterceptReconciler builds a reconciler around a fake client whose Job Create calls are routed
// through createHook, plus a recorder tests can assert on if they need to.
func newJobCreateInterceptReconciler(backup *databasesv1alpha1.Backup, createHook jobCreateHook) (*BackupReconciler, *record.FakeRecorder) {
	scheme := newTestScheme()
	recorder := record.NewFakeRecorder(10)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(backup, newTestDatabase(testDBName, testNamespace, testClusterName), newTestCluster(testClusterName),
			newTestStorage(testStorageName), newTestSecret(testSecretName, testOperatorNS)).
		WithStatusSubresource(&databasesv1alpha1.Backup{}).
		WithInterceptorFuncs(interceptor.Funcs{Create: createHook}).
		Build()

	return &BackupReconciler{Client: fakeClient, Scheme: scheme, Namespace: testOperatorNS, Image: testImage, Recorder: recorder}, recorder
}

func TestBackupReconciler_JobCreationInvalid_TerminalFailure(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}

	invalidErr := apierrors.NewInvalid(
		schema.GroupKind{Group: "batch", Kind: "Job"},
		"backup-test-backup",
		field.ErrorList{field.Invalid(field.NewPath("spec", "template"), "x", "boom")},
	)
	r, recorder := newJobCreateInterceptReconciler(backup, func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			return invalidErr
		}
		return c.Create(ctx, obj, opts...)
	})
	req := rerunRequest()

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for a terminal failure, got %v", result.RequeueAfter)
	}

	failed := getBackup(t, r, req)
	if failed.Status.Phase != "Failed" {
		t.Fatalf(errExpectedPhase, "Failed", failed.Status.Phase)
	}
	if failed.Status.FailureReason != "InvalidJobSpec" {
		t.Errorf("expected FailureReason InvalidJobSpec, got %q", failed.Status.FailureReason)
	}
	if failed.Status.FailureMessage == "" {
		t.Errorf("expected FailureMessage to be set")
	}
	if failed.Status.CompletedAt == nil {
		t.Errorf("expected CompletedAt to be set")
	}
	if failed.Status.ObservedGeneration != failed.Generation {
		t.Errorf("expected observedGeneration %d, got %d", failed.Generation, failed.Status.ObservedGeneration)
	}

	select {
	case e := <-recorder.Events:
		if !strings.Contains(e, "Warning") {
			t.Errorf("expected a Warning event, got %q", e)
		}
	default:
		t.Error("expected a Warning event to be emitted")
	}

	// A second reconcile must not retry: same spec hash + terminal phase short-circuits via isAlreadyProcessed.
	reconcileOnce(t, r, req)
	if jobs := listBackupJobs(t, r); len(jobs) != 0 {
		t.Errorf("expected no Job created after a terminal Invalid failure, got %d", len(jobs))
	}
	settled := getBackup(t, r, req)
	if settled.ResourceVersion != failed.ResourceVersion {
		t.Errorf("expected no status patch on second reconcile, resourceVersion moved %s -> %s", failed.ResourceVersion, settled.ResourceVersion)
	}
}

// AlreadyExists defers to the next reconcile's adoption instead of writing status.
func TestBackupReconciler_JobCreationAlreadyExists_RequeuesWithoutStatusChange(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}

	alreadyExistsErr := apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, "backup-test-backup")
	var concurrentJobName string
	r, _ := newJobCreateInterceptReconciler(backup, func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		job, ok := obj.(*batchv1.Job)
		if !ok {
			return c.Create(ctx, obj, opts...)
		}
		// A concurrent reconcile's Job lands in the store (with the labels this reconcile would
		// have used) while this reconcile's own Create observes AlreadyExists.
		concurrent := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backup-test-backup-concurrent1",
				Namespace: job.Namespace,
				Labels:    job.Labels,
			},
		}
		if err := c.Create(ctx, concurrent); err != nil {
			return err
		}
		concurrentJobName = concurrent.Name
		return alreadyExistsErr
	})
	req := rerunRequest()

	before := getBackup(t, r, req)

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if result.RequeueAfter != rerunRequeueDelay {
		t.Errorf("expected RequeueAfter=%v, got %v", rerunRequeueDelay, result.RequeueAfter)
	}

	after := getBackup(t, r, req)
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("expected no status patch, resourceVersion moved %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
	if after.Status.Phase != "" {
		t.Errorf("expected no status change, got phase %q", after.Status.Phase)
	}

	// The requeued reconcile finds the concurrent Job by label and adopts it instead of retrying Create.
	reconcileOnce(t, r, req)

	adopted := getBackup(t, r, req)
	if adopted.Status.JobName != concurrentJobName {
		t.Errorf("expected the concurrent Job to be adopted, got jobName %q", adopted.Status.JobName)
	}
	if adopted.Status.Phase != "Running" {
		t.Errorf(errExpectedPhase, "Running", adopted.Status.Phase)
	}
	if jobs := listBackupJobs(t, r); len(jobs) != 1 {
		t.Errorf("expected exactly 1 Job, got %d", len(jobs))
	}
}

func TestBackupReconciler_JobCreationErrorThenRecovers(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}

	attempts := 0
	r, _ := newJobCreateInterceptReconciler(backup, func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if _, ok := obj.(*batchv1.Job); ok {
			attempts++
			if attempts == 1 {
				return apierrors.NewInternalError(errors.New("simulated transient failure"))
			}
		}
		return c.Create(ctx, obj, opts...)
	})
	req := rerunRequest()

	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected error from the failed job create")
	}
	pending := getBackup(t, r, req)
	if pending.Status.Phase != "Pending" {
		t.Fatalf(errExpectedPhase, "Pending", pending.Status.Phase)
	}

	reconcileOnce(t, r, req)

	running := getBackup(t, r, req)
	if running.Status.Phase != "Running" {
		t.Fatalf(errExpectedPhase, "Running", running.Status.Phase)
	}
	if attempts != 2 {
		t.Errorf("expected 2 job create attempts, got %d", attempts)
	}
}

func TestBackupReconciler_RetriggerAfterFailureClearsFailureFields(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1
	backup.Spec.Trigger = "v1"

	r := newRerunReconciler(backup)
	req := rerunRequest()

	reconcileOnce(t, r, req)
	jobs := listBackupJobs(t, r)
	if len(jobs) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs))
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-pod",
			Namespace: testOperatorNS,
			Labels:    map[string]string{"job-name": jobs[0].Name},
		},
	}
	if err := r.Create(context.Background(), pod); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	failBackupJob(t, r, &jobs[0])
	reconcileOnce(t, r, req)

	failed := getBackup(t, r, req)
	if failed.Status.Phase != "Failed" {
		t.Fatalf(errExpectedPhase, "Failed", failed.Status.Phase)
	}
	if failed.Status.FailureReason == "" || failed.Status.FailedAttempts == 0 || failed.Status.LastPodName == "" {
		t.Fatalf("expected failure info populated, got %+v", failed.Status)
	}

	retriggerBackup(t, r, &failed, "v2", 2)
	reconcileOnce(t, r, req)

	running := getBackup(t, r, req)
	if running.Status.Phase != "Running" {
		t.Fatalf(errExpectedPhase, "Running", running.Status.Phase)
	}
	if running.Status.FailureReason != "" || running.Status.FailureMessage != "" ||
		running.Status.FailedAttempts != 0 || running.Status.LastPodName != "" {
		t.Errorf("expected failure fields cleared on retrigger, got %+v", running.Status)
	}
	if newJobs := listBackupJobs(t, r); len(newJobs) != 2 {
		t.Fatalf("expected a new job alongside the failed one, got %d", len(newJobs))
	}
}

func TestBackupReconciler_ObservedGenerationCatchesUpOnNoOpSpecEdit(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1

	r := newRerunReconciler(backup)
	req := rerunRequest()

	completeBackupRun(t, r, req)
	completed := getBackup(t, r, req)
	if completed.Status.ObservedGeneration != 1 {
		t.Fatalf("expected observedGeneration 1 after the first run, got %d", completed.Status.ObservedGeneration)
	}
	firstJobName := completed.Status.JobName

	completed.Generation = 2
	if err := r.Update(context.Background(), &completed); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	reconcileOnce(t, r, req)

	after := getBackup(t, r, req)
	if after.Status.ObservedGeneration != 2 {
		t.Errorf("expected observedGeneration to catch up to 2, got %d", after.Status.ObservedGeneration)
	}
	if after.Status.Phase != "Completed" {
		t.Errorf(errExpectedPhase, "Completed", after.Status.Phase)
	}
	if after.Status.JobName != firstJobName {
		t.Errorf("expected no new run to start, jobName moved from %s to %s", firstJobName, after.Status.JobName)
	}
	if jobs := listBackupJobs(t, r); len(jobs) != 1 {
		t.Errorf("expected no new job, got %d", len(jobs))
	}
}

// Cycling the trigger v1 -> v2 -> v1 still runs three distinct Jobs; the final v1 hash equals the first even though a new Job ran.
func TestBackupReconciler_TriggerCycleReRunsWithSameHash(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Generation = 1
	backup.Spec.Trigger = "v1"

	r := newRerunReconciler(backup)
	req := rerunRequest()

	reconcileOnce(t, r, req)
	completeActiveRun(t, r, req)
	run1 := getBackup(t, r, req)
	if run1.Status.Phase != "Completed" {
		t.Fatalf(errExpectedPhase, "Completed", run1.Status.Phase)
	}
	hash1, job1 := run1.Status.SpecHash, run1.Status.JobName

	retriggerBackup(t, r, &run1, "v2", 2)
	reconcileOnce(t, r, req)
	completeActiveRun(t, r, req)
	run2 := getBackup(t, r, req)
	if run2.Status.Phase != "Completed" {
		t.Fatalf(errExpectedPhase, "Completed", run2.Status.Phase)
	}
	hash2, job2 := run2.Status.SpecHash, run2.Status.JobName
	if hash2 == hash1 {
		t.Errorf("expected the v2 hash to differ from the v1 hash")
	}
	if job2 == job1 {
		t.Errorf("expected a new job for run2, still %s", job1)
	}

	retriggerBackup(t, r, &run2, "v1", 3)
	reconcileOnce(t, r, req)
	completeActiveRun(t, r, req)
	run3 := getBackup(t, r, req)
	if run3.Status.Phase != "Completed" {
		t.Fatalf(errExpectedPhase, "Completed", run3.Status.Phase)
	}
	hash3, job3 := run3.Status.SpecHash, run3.Status.JobName
	if hash3 != hash1 {
		t.Errorf("expected the v1-again hash %q to equal the first v1 hash %q", hash3, hash1)
	}
	if job3 == job1 || job3 == job2 {
		t.Errorf("expected a new job for run3, got %s (run1=%s, run2=%s)", job3, job1, job2)
	}

	jobs := listBackupJobs(t, r)
	if len(jobs) != 3 {
		t.Fatalf("expected 3 distinct jobs, got %d", len(jobs))
	}
	names := map[string]bool{}
	for _, j := range jobs {
		names[j.Name] = true
	}
	if len(names) != 3 {
		t.Errorf("expected 3 distinct job names, got %d", len(names))
	}
}
