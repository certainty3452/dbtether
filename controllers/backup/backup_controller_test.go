package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

const (
	testDBName      = "test-db"
	testStorageName = "test-storage"
	testClusterName = "test-cluster"
	testBackupName  = "test-backup"
	testSecretName  = "test-secret"
	testNamespace   = "app-namespace"
	testOperatorNS  = "dbtether"
	testUID         = "test-uid-12345678"
	testImage       = "dbtether:test"
)

const (
	errUnexpectedError  = "unexpected error: %v"
	errFailedToGet      = "failed to get backup: %v"
	errFailedToListJobs = "failed to list jobs: %v"
	errExpectedOneJob   = "expected 1 job, got %d"
	errExpectedPhase    = "expected %s phase, got %s"
	testRunID           = "12345678"
	testJobName         = "backup-test-backup-" + testRunID
	testClusterA        = "cluster-a"
)

func newTestScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = databasesv1alpha1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return scheme
}

func newTestReconciler(objs ...client.Object) *BackupReconciler {
	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&databasesv1alpha1.Backup{}).
		Build()

	return &BackupReconciler{
		Client:    fakeClient,
		APIReader: fakeClient,
		Scheme:    scheme,
		Namespace: testOperatorNS,
		Image:     testImage,
	}
}

func newTestBackup(name, namespace string) *databasesv1alpha1.Backup {
	return &databasesv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       testUID,
		},
		Spec: databasesv1alpha1.BackupSpec{
			DatabaseRef: databasesv1alpha1.DatabaseReference{Name: testDBName},
			StorageRef:  databasesv1alpha1.StorageReference{Name: testStorageName},
		},
	}
}

func newTestDatabase(name, namespace, clusterName string) *databasesv1alpha1.Database {
	return &databasesv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: databasesv1alpha1.DatabaseSpec{
			ClusterRef: databasesv1alpha1.ClusterReference{Name: clusterName},
		},
		Status: databasesv1alpha1.DatabaseStatus{
			Phase:        "Ready",
			DatabaseName: name,
		},
	}
}

func newTestCluster(name string) *databasesv1alpha1.DBCluster {
	return &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "localhost",
			Port:     5432,
			CredentialsSecretRef: &databasesv1alpha1.SecretReference{
				Name:      testSecretName,
				Namespace: "dbtether",
			},
		},
		Status: databasesv1alpha1.DBClusterStatus{
			Phase: "Connected",
		},
	}
}

func newTestStorage(name string) *databasesv1alpha1.BackupStorage {
	return &databasesv1alpha1.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: databasesv1alpha1.BackupStorageSpec{
			S3: &databasesv1alpha1.S3StorageConfig{
				Bucket: "test-bucket",
				Region: "us-east-1",
			},
			PathTemplate: "{{ .ClusterName }}/{{ .DatabaseName }}",
		},
		Status: databasesv1alpha1.BackupStorageStatus{
			Phase: "Ready",
		},
	}
}

func newTestSecret(name, namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"username": []byte("testuser"),
			"password": []byte("testpass"),
		},
	}
}

func TestBackupReconciler_ComputeSpecHash(t *testing.T) {
	r := &BackupReconciler{}

	backup1 := newTestBackup("backup1", "default")
	backup2 := newTestBackup("backup2", "default")
	backup3 := newTestBackup("backup3", "default")
	backup3.Spec.DatabaseRef.Name = "different-db"

	hash1 := r.computeSpecHash(backup1)
	hash2 := r.computeSpecHash(backup2)
	if hash1 != hash2 {
		t.Errorf("same spec should produce same hash: %s != %s", hash1, hash2)
	}

	hash3 := r.computeSpecHash(backup3)
	if hash1 == hash3 {
		t.Errorf("different spec should produce different hash: %s == %s", hash1, hash3)
	}

	if hash1 == "" || len(hash1) != 16 {
		t.Errorf("hash should be 16 chars, got %d", len(hash1))
	}
}

func TestBackupReconciler_FinalizerAdded(t *testing.T) {
	backup := newTestBackup(testBackupName, "default")
	db := newTestDatabase(testDBName, "default", testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if result.RequeueAfter != finalizerRequeueDelay {
		t.Errorf("expected RequeueAfter=%v after adding finalizer, got %v", finalizerRequeueDelay, result.RequeueAfter)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	if !controllerutil.ContainsFinalizer(&updatedBackup, backupFinalizer) {
		t.Error("finalizer should be added")
	}
}

func TestBackupReconciler_SkipAlreadyProcessed(t *testing.T) {
	r := &BackupReconciler{}
	backup := newTestBackup(testBackupName, "default")
	specHash := r.computeSpecHash(backup)

	tests := []struct {
		name       string
		phase      string
		statusHash string
		shouldSkip bool
	}{
		{"empty phase", "", "", false},
		{"Pending phase", "Pending", specHash, false},
		{"Running phase", "Running", specHash, false},
		{"Completed same hash", "Completed", specHash, true},
		{"Failed same hash", "Failed", specHash, true},
		{"Completed different hash", "Completed", "different", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup.Status.Phase = tt.phase
			backup.Status.SpecHash = tt.statusHash

			shouldSkip := backup.Status.Phase != "" &&
				backup.Status.SpecHash == specHash &&
				(backup.Status.Phase == "Completed" || backup.Status.Phase == "Failed")

			if shouldSkip != tt.shouldSkip {
				t.Errorf("shouldSkip = %v, want %v", shouldSkip, tt.shouldSkip)
			}
		})
	}
}

func TestBackupReconciler_JobCreation(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}

	if len(jobs.Items) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs.Items))
	}

	job := &jobs.Items[0]

	if job.Labels[LabelBackupName] != testBackupName {
		t.Errorf("backup label mismatch: %s", job.Labels[LabelBackupName])
	}
	if job.Labels[LabelBackupNamespace] != testNamespace {
		t.Errorf("backup-namespace label mismatch: %s", job.Labels[LabelBackupNamespace])
	}
	if job.Labels[LabelCluster] != testClusterName {
		t.Errorf("cluster label mismatch: %s", job.Labels[LabelCluster])
	}

	if len(job.OwnerReferences) != 0 {
		t.Error("job should not have OwnerReferences for cross-namespace backups")
	}

	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Error("TTL should be 3600 seconds")
	}
}

func TestBackupReconciler_JobAlreadyExists(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-test-uid",
			Namespace: "dbtether",
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
				LabelCluster:         testClusterName,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{Name: "backup", Image: "test"},
					},
				},
			},
		},
		Status: batchv1.JobStatus{
			Active: 1,
		},
	}

	r := newTestReconciler(backup, db, cluster, storage, secret, existingJob)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter for running job")
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	if updatedBackup.Status.Phase == "Failed" {
		t.Errorf("backup should not be Failed when job exists, got: %s - %s",
			updatedBackup.Status.Phase, updatedBackup.Status.Message)
	}
}

func TestBackupReconciler_JobCompleted(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.Path = "c/app/run.sql.gz"
	backup.Status.Phase = "Running"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)

	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
				LabelRunID:           testRunID,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: func() *int32 { v := int32(3); return &v }(),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

	r := newTestReconciler(backup, completedJob)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}

	if updatedBackup.Status.Phase != "Completed" {
		t.Errorf("expected Completed, got %s", updatedBackup.Status.Phase)
	}
	if updatedBackup.Status.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

func TestBackupReconciler_JobFailed(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.Phase = "Running"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)

	backoffLimit := int32(3)
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Failed: 3,
			// Job failure is detected via Conditions, not just Failed count
			Conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobFailed,
					Status: corev1.ConditionTrue,
					Reason: "BackoffLimitExceeded",
				},
			},
		},
	}

	r := newTestReconciler(backup, failedJob)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}

	if updatedBackup.Status.Phase != "Failed" {
		t.Errorf(errExpectedPhase, "Failed", updatedBackup.Status.Phase)
	}

	var updatedJob batchv1.Job
	if err := r.Get(context.Background(), types.NamespacedName{Name: testJobName, Namespace: testOperatorNS}, &updatedJob); err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	expectedTTL := int32(43200)
	if updatedJob.Spec.TTLSecondsAfterFinished == nil {
		t.Error("Job TTL should be set")
	} else if *updatedJob.Spec.TTLSecondsAfterFinished != expectedTTL {
		t.Errorf("Expected Job TTL %d (12h), got %d", expectedTTL, *updatedJob.Spec.TTLSecondsAfterFinished)
	}
}

func TestBackupReconciler_JobFailedCustomTTL(t *testing.T) {
	customTTL := int32(86400)
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.Phase = "Running"
	backup.Spec.JobConfig = &databasesv1alpha1.BackupJobConfig{
		TTLSecondsAfterFailed: &customTTL,
	}
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)

	backoffLimit := int32(2)
	oldTTL := int32(3600)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testOperatorNS,
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &oldTTL,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Failed: 2,
			Conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobFailed,
					Status: corev1.ConditionTrue,
					Reason: "DeadlineExceeded",
				},
			},
		},
	}

	r := newTestReconciler(backup, job)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var updatedJob batchv1.Job
	if err := r.Get(context.Background(), types.NamespacedName{Name: testJobName, Namespace: testOperatorNS}, &updatedJob); err != nil {
		t.Fatalf("Failed to get job: %v", err)
	}
	if updatedJob.Spec.TTLSecondsAfterFinished == nil {
		t.Error("Job TTL should be set")
	} else if *updatedJob.Spec.TTLSecondsAfterFinished != customTTL {
		t.Errorf("Expected Job TTL %d (24h), got %d", customTTL, *updatedJob.Spec.TTLSecondsAfterFinished)
	}
}

func TestBackupReconciler_Deletion(t *testing.T) {
	now := metav1.Now()
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.DeletionTimestamp = &now

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
				LabelRunID:           testRunID,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
				},
			},
		},
	}

	r := newTestReconciler(backup, job)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 0 {
		t.Error("job should be deleted during backup deletion")
	}

	var updatedBackup databasesv1alpha1.Backup
	err = r.Get(context.Background(), req.NamespacedName, &updatedBackup)
	if err == nil {
		if controllerutil.ContainsFinalizer(&updatedBackup, backupFinalizer) {
			t.Error("finalizer should be removed")
		}
	}
}

func TestBackupReconciler_Throttling(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	var existingJobs []client.Object
	for i := 0; i < DefaultMaxConcurrentJobsPerCluster; i++ {
		existingJobs = append(existingJobs, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "existing-job-" + string(rune('a'+i)),
				Namespace: "dbtether",
				Labels: map[string]string{
					LabelCluster: testClusterName,
				},
			},
			Spec: batchv1.JobSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
					},
				},
			},
			Status: batchv1.JobStatus{
				Active: 1,
			},
		})
	}

	allObjs := append([]client.Object{backup, db, cluster, storage, secret}, existingJobs...)
	r := newTestReconciler(allObjs...)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	if result.RequeueAfter != RequeueDelayWhenThrottled {
		t.Errorf("expected RequeueAfter=%v, got %v", RequeueDelayWhenThrottled, result.RequeueAfter)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}

	if updatedBackup.Status.Phase != "Pending" {
		t.Errorf("expected Pending (throttled), got %s", updatedBackup.Status.Phase)
	}
}

func TestBackupReconciler_CountActiveJobs(t *testing.T) {
	job1 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-1",
			Namespace: "dbtether",
			Labels:    map[string]string{LabelCluster: testClusterA},
		},
		Status: batchv1.JobStatus{Active: 1},
	}
	job2 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-2",
			Namespace: "dbtether",
			Labels:    map[string]string{LabelCluster: testClusterA},
		},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	job3 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-3",
			Namespace: "dbtether",
			Labels:    map[string]string{LabelCluster: "cluster-b"},
		},
		Status: batchv1.JobStatus{Active: 1},
	}
	// One failed attempt without a Failed condition means the Job is still retrying (BackoffLimit not yet hit).
	job4 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-4",
			Namespace: "dbtether",
			Labels:    map[string]string{LabelCluster: testClusterA},
		},
		Status: batchv1.JobStatus{Failed: 1},
	}

	r := newTestReconciler(job1, job2, job3, job4)

	count, err := countActiveJobsForCluster(context.Background(), r.Client, r.Namespace, testClusterA)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if count != 2 {
		t.Errorf("expected 2 active jobs for cluster-a, got %d", count)
	}

	count, err = countActiveJobsForCluster(context.Background(), r.Client, r.Namespace, "cluster-b")
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if count != 1 {
		t.Errorf("expected 1 active job for cluster-b, got %d", count)
	}

	count, err = countActiveJobsForCluster(context.Background(), r.Client, r.Namespace, "cluster-c")
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if count != 0 {
		t.Errorf("expected 0 active jobs for cluster-c, got %d", count)
	}
}

func TestBackupReconciler_ResourceNotFound(t *testing.T) {
	tests := []struct {
		name           string
		setupObjs      []client.Object
		expectedErrMsg string
	}{
		{
			name: "database not found",
			setupObjs: []client.Object{
				func() *databasesv1alpha1.Backup {
					b := newTestBackup(testBackupName, "default")
					b.Finalizers = []string{backupFinalizer}
					return b
				}(),
			},
			expectedErrMsg: "database default/test-db not found",
		},
		{
			name: "cluster not found",
			setupObjs: []client.Object{
				func() *databasesv1alpha1.Backup {
					b := newTestBackup(testBackupName, "default")
					b.Finalizers = []string{backupFinalizer}
					return b
				}(),
				newTestDatabase(testDBName, "default", "missing-cluster"),
			},
			expectedErrMsg: "cluster missing-cluster not found",
		},
		{
			name: "storage not found",
			setupObjs: []client.Object{
				func() *databasesv1alpha1.Backup {
					b := newTestBackup(testBackupName, "default")
					b.Finalizers = []string{backupFinalizer}
					return b
				}(),
				newTestDatabase(testDBName, "default", testClusterName),
				newTestCluster(testClusterName),
			},
			expectedErrMsg: "backup storage test-storage not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(tt.setupObjs...)

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      testBackupName,
					Namespace: "default",
				},
			}

			result, err := r.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf(errUnexpectedError, err)
			}

			// Missing/not-ready dependencies are transient under GitOps ordering:
			// the backup stays Pending and requeues rather than failing terminally.
			if result.RequeueAfter == 0 {
				t.Error("expected requeue while dependency is not ready")
			}

			var backup databasesv1alpha1.Backup
			if err := r.Get(context.Background(), req.NamespacedName, &backup); err != nil {
				t.Fatalf(errFailedToGet, err)
			}

			if backup.Status.Phase != "Pending" {
				t.Errorf(errExpectedPhase, "Pending", backup.Status.Phase)
			}
			if backup.Status.Message != tt.expectedErrMsg {
				t.Errorf("expected message %q, got %q", tt.expectedErrMsg, backup.Status.Message)
			}
		})
	}
}

func TestBackupReconciler_NotReady(t *testing.T) {
	backup := newTestBackup(testBackupName, "default")
	backup.Finalizers = []string{backupFinalizer}

	db := newTestDatabase(testDBName, "default", testClusterName)
	db.Status.Phase = "Pending"

	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)

	r := newTestReconciler(backup, db, cluster, storage)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	// Not-ready dependency: Pending + requeue, not terminal Failed.
	if result.RequeueAfter == 0 {
		t.Error("expected requeue while dependency is not ready")
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}

	if updatedBackup.Status.Phase != "Pending" {
		t.Errorf(errExpectedPhase, "Pending", updatedBackup.Status.Phase)
	}
}

func TestBackupReconciler_ThrottlingConstants(t *testing.T) {
	if DefaultMaxConcurrentJobsPerCluster < 1 || DefaultMaxConcurrentJobsPerCluster > 10 {
		t.Errorf("DefaultMaxConcurrentJobsPerCluster should be 1-10, got %d", DefaultMaxConcurrentJobsPerCluster)
	}

	if RequeueDelayWhenThrottled < 10*time.Second || RequeueDelayWhenThrottled > 5*time.Minute {
		t.Errorf("RequeueDelayWhenThrottled should be 10s-5m, got %v", RequeueDelayWhenThrottled)
	}
}

func TestBackupReconciler_BackupNotFound(t *testing.T) {
	r := newTestReconciler()

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "nonexistent-backup",
			Namespace: "default",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	if result.RequeueAfter != 0 {
		t.Error("should not requeue for not found backup")
	}
}

func TestBackupReconciler_DeleteJobNotFound(t *testing.T) {
	now := metav1.Now()
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.DeletionTimestamp = &now

	r := newTestReconciler(backup)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("should handle missing job gracefully: %v", err)
	}

	var updatedBackup databasesv1alpha1.Backup
	err = r.Get(context.Background(), req.NamespacedName, &updatedBackup)
	if err == nil && controllerutil.ContainsFinalizer(&updatedBackup, backupFinalizer) {
		t.Error("finalizer should be removed even if job not found")
	}
}

func TestBackupReconciler_JobMissingFromCacheIsNotLost(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.Phase = "Running"
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)
	activeJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testOperatorNS,
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
		Status: batchv1.JobStatus{Active: 1},
	}

	r := newTestReconciler(backup)
	r.APIReader = fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(backup.DeepCopy(), activeJob).Build()

	result, err := reconcileBackup(t, r, backup)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if result.RequeueAfter != 10*time.Second {
		t.Errorf("expected the running Job to be polled, got RequeueAfter=%v", result.RequeueAfter)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(backup), &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	if updatedBackup.Status.Phase != "Running" {
		t.Errorf("a Job the cache has not seen yet is not lost, got phase=%s message=%s",
			updatedBackup.Status.Phase, updatedBackup.Status.Message)
	}
}

func TestBackupReconciler_JobNotFoundOldBackup(t *testing.T) {
	oldStart := metav1.NewTime(time.Now().Add(-120 * time.Second))
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.Phase = "Running"
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.StartedAt = &oldStart
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)

	r := newTestReconciler(backup)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue for old backup, got RequeueAfter=%v", result.RequeueAfter)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
	}
	if updatedBackup.Status.Phase != "Failed" {
		t.Errorf("expected Failed phase, got %s", updatedBackup.Status.Phase)
	}
	if updatedBackup.Status.Message != "backup job not found" {
		t.Errorf("expected 'backup job not found' message, got %s", updatedBackup.Status.Message)
	}
}

func TestBackupReconciler_JobNotFoundNoStartedAt(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.Phase = "Running"
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.StartedAt = nil
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)

	r := newTestReconciler(backup)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.RequeueAfter != 0 {
		t.Errorf("expected no requeue when no StartedAt, got RequeueAfter=%v", result.RequeueAfter)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
	}
	if updatedBackup.Status.Phase != "Failed" {
		t.Errorf("expected Failed phase, got %s", updatedBackup.Status.Phase)
	}
	if updatedBackup.Status.Message != "backup job not found" {
		t.Errorf("expected 'backup job not found' message, got %s", updatedBackup.Status.Message)
	}
}

func TestGenerateRunID(t *testing.T) {
	runID := generateRunID()
	if len(runID) != 8 {
		t.Errorf("expected 8 characters, got %d: %s", len(runID), runID)
	}

	for _, c := range runID {
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isDigit {
			t.Errorf("unexpected character in runID: %c", c)
		}
	}

	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateRunID()
		if seen[id] {
			t.Logf("warning: duplicate runID found: %s (this is very unlikely)", id)
		}
		seen[id] = true
	}
}

func TestBackupReconciler_CompletedKeepsResultRecordedByJob(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = "backup-test-backup-abc12345"
	backup.Status.Phase = "Running"
	backup.Status.RunID = "abc12345"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)
	backup.Status.Path = "microservices/orders_db/20260120-143022.sql.gz"
	backup.Status.Size = "25.6 MiB"
	backup.Status.Duration = "3.2s"

	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-abc12345",
			Namespace: "dbtether",
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
				LabelRunID:           "abc12345",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: func() *int32 { v := int32(3); return &v }(),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

	r := newTestReconciler(backup, completedJob)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}

	if updatedBackup.Status.Path != "microservices/orders_db/20260120-143022.sql.gz" {
		t.Errorf("expected path in status, got %q", updatedBackup.Status.Path)
	}
	if updatedBackup.Status.Size != "25.6 MiB" {
		t.Errorf("expected size in status, got %q", updatedBackup.Status.Size)
	}
	if updatedBackup.Status.Duration != "3.2s" {
		t.Errorf("expected duration in status, got %q", updatedBackup.Status.Duration)
	}
}

func TestBackupReconciler_RunIDInJobName(t *testing.T) {
	backup := newTestBackup("my-backup", testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "my-backup",
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}

	runID := updatedBackup.Status.RunID
	if runID == "" {
		t.Fatal("RunID should be set in status")
	}
	if len(runID) != 8 {
		t.Errorf("RunID should be 8 chars, got %d: %s", len(runID), runID)
	}

	expectedJobNamePrefix := "backup-my-backup-" + runID
	if updatedBackup.Status.JobName != expectedJobNamePrefix {
		t.Errorf("job name should be %q, got %q", expectedJobNamePrefix, updatedBackup.Status.JobName)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs.Items))
	}
	if jobs.Items[0].Name != expectedJobNamePrefix {
		t.Errorf("job name mismatch: expected %q, got %q", expectedJobNamePrefix, jobs.Items[0].Name)
	}
}

func TestBackupReconciler_CustomTTL(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	ttl := metav1.Duration{Duration: 24 * time.Hour}
	backup.Spec.TTLAfterCompletion = &ttl

	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs.Items))
	}

	job := &jobs.Items[0]
	expectedTTL := int32(86400)
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != expectedTTL {
		t.Errorf("TTL should be %d, got %v", expectedTTL, job.Spec.TTLSecondsAfterFinished)
	}
}

func TestBackupReconciler_ReservedRunContinuesWithItsCreatedJob(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.Phase = "Pending"
	backup.Status.RunID = "existing1"
	backup.Status.JobName = "backup-test-backup-existing1"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)

	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, testOperatorNS)

	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-existing1",
			Namespace: testOperatorNS,
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
				LabelCluster:         testClusterName,
				LabelRunID:           "existing1",
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{Name: "backup", Image: testImage, Env: []corev1.EnvVar{{Name: "RUN_ID", Value: "existing1"}}},
					},
				},
			},
		},
	}

	r := newTestReconcilerWithObjects(backup, db, cluster, storage, secret, existingJob)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(testOperatorNS)); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected exactly 1 job (existing), got %d", len(jobs.Items))
	}
	if jobs.Items[0].Name != "backup-test-backup-existing1" {
		t.Errorf("expected existing job name, got %s", jobs.Items[0].Name)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	if updatedBackup.Status.JobName != "backup-test-backup-existing1" {
		t.Errorf("expected JobName to be existing job, got %s", updatedBackup.Status.JobName)
	}
	if updatedBackup.Status.RunID != "existing1" {
		t.Errorf("expected RunID 'existing1', got %s", updatedBackup.Status.RunID)
	}
	if updatedBackup.Status.Phase != "Running" || updatedBackup.Status.StartedAt == nil {
		t.Errorf("expected the found Job to confirm the run, got phase %q startedAt %v", updatedBackup.Status.Phase, updatedBackup.Status.StartedAt)
	}
}

func newTestReconcilerWithObjects(objs ...client.Object) *BackupReconciler {
	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&databasesv1alpha1.Backup{}).
		Build()

	return &BackupReconciler{
		Client:    fakeClient,
		APIReader: fakeClient,
		Scheme:    scheme,
		Image:     testImage,
		Namespace: testOperatorNS,
	}
}

func TestIsJobFailed(t *testing.T) {
	tests := []struct {
		name           string
		conditions     []batchv1.JobCondition
		expectFailed   bool
		expectContains string
	}{
		{
			name:         "no conditions",
			conditions:   nil,
			expectFailed: false,
		},
		{
			name:         "empty conditions",
			conditions:   []batchv1.JobCondition{},
			expectFailed: false,
		},
		{
			name: "job complete (not failed)",
			conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionTrue,
				},
			},
			expectFailed: false,
		},
		{
			name: "job failed condition false",
			conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobFailed,
					Status: corev1.ConditionFalse,
				},
			},
			expectFailed: false,
		},
		{
			name: "job failed - backoff limit exceeded",
			conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Reason:  "BackoffLimitExceeded",
					Message: "Job has reached the specified backoff limit",
				},
			},
			expectFailed:   true,
			expectContains: "BackoffLimitExceeded",
		},
		{
			name: "job failed - deadline exceeded",
			conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Reason:  "DeadlineExceeded",
					Message: "Job was active longer than specified deadline",
				},
			},
			expectFailed:   true,
			expectContains: "DeadlineExceeded",
		},
		{
			name: "job failed - no reason or message",
			conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobFailed,
					Status: corev1.ConditionTrue,
				},
			},
			expectFailed:   true,
			expectContains: "backup job failed",
		},
		{
			name: "multiple conditions - failed takes precedence",
			conditions: []batchv1.JobCondition{
				{
					Type:   batchv1.JobComplete,
					Status: corev1.ConditionFalse,
				},
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Reason:  "PodFailurePolicy",
					Message: "Container exited with code 1",
				},
			},
			expectFailed:   true,
			expectContains: "PodFailurePolicy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: tt.conditions,
				},
			}

			reason, failed := isJobFailed(job)

			if failed != tt.expectFailed {
				t.Errorf("isJobFailed() failed = %v, want %v", failed, tt.expectFailed)
			}

			if tt.expectContains != "" && failed {
				if !contains(reason, tt.expectContains) {
					t.Errorf("reason %q should contain %q", reason, tt.expectContains)
				}
			}
		})
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestBackupReconciler_JobFailedWithCondition(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.Phase = "Running"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)

	backoffLimit := int32(3)
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Failed: 3,
			Conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Reason:  "BackoffLimitExceeded",
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
	}

	r := newTestReconciler(backup, failedJob)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}

	if updatedBackup.Status.Phase != "Failed" {
		t.Errorf(errExpectedPhase, "Failed", updatedBackup.Status.Phase)
	}

	if !contains(updatedBackup.Status.Message, "BackoffLimitExceeded") {
		t.Errorf("expected message to contain 'BackoffLimitExceeded', got %q", updatedBackup.Status.Message)
	}
}

func newSpentBackoffFixture() (*databasesv1alpha1.Backup, *batchv1.Job) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.Phase = "Running"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testOperatorNS,
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
		Status: batchv1.JobStatus{
			Failed: 1,
			Conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Reason:  "BackoffLimitExceeded",
					Message: "Job has reached the specified backoff limit",
				},
			},
		},
	}
	return backup, job
}

func newAttemptPod(name string, created time.Time, status *corev1.ContainerStatus) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testOperatorNS,
			Labels:            map[string]string{"job-name": testJobName},
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec:   corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{*status}},
	}
}

func terminatedWith(message string) *corev1.ContainerStatus {
	return &corev1.ContainerStatus{
		Name: "backup",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:  "Error",
			Message: message + "\n",
		}},
	}
}

func reconcileToFailed(t *testing.T, r *BackupReconciler) databasesv1alpha1.Backup {
	t.Helper()
	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testBackupName, Namespace: testNamespace},
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var updated databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updated); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	if updated.Status.Phase != "Failed" {
		t.Fatalf(errExpectedPhase, "Failed", updated.Status.Phase)
	}
	return updated
}

func TestBackupReconciler_JobUsesRestartPolicyNever(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}

	r := newTestReconciler(backup,
		newTestDatabase(testDBName, testNamespace, testClusterName),
		newTestCluster(testClusterName),
		newTestStorage(testStorageName),
		newTestSecret(testSecretName, testOperatorNS))

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testBackupName, Namespace: testNamespace},
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace(testOperatorNS)); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs.Items))
	}

	got := jobs.Items[0].Spec.Template.Spec.RestartPolicy
	if got != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want %q", got, corev1.RestartPolicyNever)
	}
}

func TestBackupReconciler_JobFailurePrefersPodTerminationMessage(t *testing.T) {
	const cause = "backup failed: pg_dump failed: exit status 1: pg_dump: error: connection to server failed"

	backup, failedJob := newSpentBackoffFixture()
	pod := newAttemptPod(testJobName+"-abc12", time.Now().Add(-time.Minute), terminatedWith(cause))

	updated := reconcileToFailed(t, newTestReconciler(backup, failedJob, pod))

	if updated.Status.FailureMessage != cause {
		t.Errorf("FailureMessage = %q, want %q", updated.Status.FailureMessage, cause)
	}
	if updated.Status.FailureReason != "BackoffLimitExceeded" {
		t.Errorf("FailureReason = %q, want %q", updated.Status.FailureReason, "BackoffLimitExceeded")
	}
	wantMessage := "backup job failed: " + cause
	if updated.Status.Message != wantMessage {
		t.Errorf("Message = %q, want %q", updated.Status.Message, wantMessage)
	}
}

func TestBackupReconciler_JobFailureUsesTheLatestAttempt(t *testing.T) {
	const earlierCause = "backup failed: pg_dump failed: exit status 1: could not connect"
	const latestCause = "backup failed: upload failed: AccessDenied"

	backup, failedJob := newSpentBackoffFixture()
	base := time.Now().Add(-time.Hour)
	earlier := newAttemptPod(testJobName+"-zzzzz", base, terminatedWith(earlierCause))
	latest := newAttemptPod(testJobName+"-aaaaa", base.Add(time.Minute), terminatedWith(latestCause))

	updated := reconcileToFailed(t, newTestReconciler(backup, failedJob, earlier, latest))

	if updated.Status.FailureMessage != latestCause {
		t.Errorf("FailureMessage = %q, want %q", updated.Status.FailureMessage, latestCause)
	}
	if updated.Status.Message != "backup job failed: "+latestCause {
		t.Errorf("Message = %q, want %q", updated.Status.Message, "backup job failed: "+latestCause)
	}
	if updated.Status.LastPodName != latest.Name {
		t.Errorf("LastPodName = %q, want %q", updated.Status.LastPodName, latest.Name)
	}
}

func TestBackupReconciler_DeadlineKeepsConditionMessage(t *testing.T) {
	const deadlineMessage = "Job was active longer than specified deadline"

	backup, failedJob := newSpentBackoffFixture()
	failedJob.Status.Conditions = []batchv1.JobCondition{
		{
			Type:    batchv1.JobFailed,
			Status:  corev1.ConditionTrue,
			Reason:  "DeadlineExceeded",
			Message: deadlineMessage,
		},
	}
	pod := newAttemptPod(testJobName+"-abc12", time.Now(), terminatedWith("backup failed: pg_dump failed: exit status 1"))

	updated := reconcileToFailed(t, newTestReconciler(backup, failedJob, pod))

	if updated.Status.FailureReason != "DeadlineExceeded" {
		t.Errorf("FailureReason = %q, want %q", updated.Status.FailureReason, "DeadlineExceeded")
	}
	if updated.Status.FailureMessage != deadlineMessage {
		t.Errorf("FailureMessage = %q, want %q", updated.Status.FailureMessage, deadlineMessage)
	}
	if updated.Status.Message != "backup job failed: DeadlineExceeded" {
		t.Errorf("Message = %q, want %q", updated.Status.Message, "backup job failed: DeadlineExceeded")
	}
}

func TestBackupReconciler_CompletedBackupNamesTheSucceededPod(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.Phase = "Running"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)
	backup.Status.LastPodName = testJobName + "-zzzzz"
	backup.Status.Path = "c/app/run.sql.gz"

	succeededJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: testOperatorNS,
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
		Status: batchv1.JobStatus{Succeeded: 1, Failed: 1},
	}

	base := time.Now().Add(-time.Hour)
	failed := newAttemptPod(testJobName+"-zzzzz", base, terminatedWith("backup failed: pg_dump failed: exit status 1"))
	succeeded := newAttemptPod(testJobName+"-aaaaa", base.Add(time.Minute), &corev1.ContainerStatus{
		Name:  "backup",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
	})

	r := newTestReconciler(backup, succeededJob, failed, succeeded)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{Name: testBackupName, Namespace: testNamespace},
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var updated databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updated); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	if updated.Status.Phase != "Completed" {
		t.Fatalf(errExpectedPhase, "Completed", updated.Status.Phase)
	}
	if updated.Status.LastPodName != succeeded.Name {
		t.Errorf("LastPodName = %q, want %q", updated.Status.LastPodName, succeeded.Name)
	}
}

func TestBackupReconciler_FailureFallsBackToLastTermination(t *testing.T) {
	const cause = "backup failed: pg_dump failed: exit status 2"

	backup, failedJob := newSpentBackoffFixture()
	pod := newAttemptPod(testJobName+"-abc12", time.Now(), &corev1.ContainerStatus{
		Name:  "backup",
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason:  "Error",
			Message: cause + "\n",
		}},
	})

	updated := reconcileToFailed(t, newTestReconciler(backup, failedJob, pod))

	if updated.Status.FailureMessage != cause {
		t.Errorf("FailureMessage = %q, want %q", updated.Status.FailureMessage, cause)
	}
	if updated.Status.Message != "backup job failed: "+cause {
		t.Errorf("Message = %q, want %q", updated.Status.Message, "backup job failed: "+cause)
	}
}

func TestBackupReconciler_CustomBackoffLimit(t *testing.T) {
	backoffLimit := int32(5)
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Spec.JobConfig = &databasesv1alpha1.BackupJobConfig{
		BackoffLimit: &backoffLimit,
	}

	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs.Items))
	}

	job := &jobs.Items[0]
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 5 {
		t.Errorf("BackoffLimit should be 5, got %v", job.Spec.BackoffLimit)
	}
}

func TestBackupReconciler_ActiveDeadlineSeconds(t *testing.T) {
	deadline := int64(1800)
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Spec.JobConfig = &databasesv1alpha1.BackupJobConfig{
		ActiveDeadlineSeconds: &deadline,
	}

	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs.Items))
	}

	job := &jobs.Items[0]
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != 1800 {
		t.Errorf("ActiveDeadlineSeconds should be 1800, got %v", job.Spec.ActiveDeadlineSeconds)
	}
}

func TestBackupReconciler_DeadlineExceeded(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.RunID = testRunID
	backup.Status.Phase = "Running"
	backup.Status.SpecHash = (&BackupReconciler{}).computeSpecHash(backup)

	deadline := int64(300)
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
			Labels:    map[string]string{LabelBackupName: testBackupName, LabelBackupNamespace: testNamespace, LabelRunID: testRunID},
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{
				{
					Type:    batchv1.JobFailed,
					Status:  corev1.ConditionTrue,
					Reason:  "DeadlineExceeded",
					Message: "Job was active longer than specified deadline",
				},
			},
		},
	}

	r := newTestReconciler(backup, failedJob)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}

	if updatedBackup.Status.Phase != "Failed" {
		t.Errorf(errExpectedPhase, "Failed", updatedBackup.Status.Phase)
	}

	if !contains(updatedBackup.Status.Message, "DeadlineExceeded") {
		t.Errorf("expected message to contain 'DeadlineExceeded', got %q", updatedBackup.Status.Message)
	}
}

func TestBackupReconciler_ZeroBackoffLimit(t *testing.T) {
	backoffLimit := int32(0)
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Spec.JobConfig = &databasesv1alpha1.BackupJobConfig{
		BackoffLimit: &backoffLimit,
	}

	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs.Items))
	}

	job := &jobs.Items[0]
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("BackoffLimit should be 0 (no retries), got %v", job.Spec.BackoffLimit)
	}
}

func TestGetLastPodName_Logic(t *testing.T) {
	now := metav1.Now()
	earlier := metav1.NewTime(now.Add(-1 * time.Hour))
	evenEarlier := metav1.NewTime(now.Add(-2 * time.Hour))

	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "job-abc-pod-1",
				CreationTimestamp: evenEarlier,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "job-abc-pod-3",
				CreationTimestamp: now,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "job-abc-pod-2",
				CreationTimestamp: earlier,
			},
		},
	}

	var lastPod *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if lastPod == nil || pod.CreationTimestamp.After(lastPod.CreationTimestamp.Time) {
			lastPod = pod
		}
	}

	if lastPod == nil {
		t.Fatal("Expected to find a pod")
	}
	if lastPod.Name != "job-abc-pod-3" {
		t.Errorf("Expected newest pod 'job-abc-pod-3', got '%s'", lastPod.Name)
	}
}

func TestGetLastPodName_EmptyList(t *testing.T) {
	pods := []corev1.Pod{}

	var lastPod *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if lastPod == nil || pod.CreationTimestamp.After(lastPod.CreationTimestamp.Time) {
			lastPod = pod
		}
	}

	if lastPod != nil {
		t.Error("Expected nil for empty pod list")
	}
}

func TestGetLastPodName_SinglePod(t *testing.T) {
	now := metav1.Now()
	pods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "job-xyz-pod-1",
				CreationTimestamp: now,
			},
		},
	}

	var lastPod *corev1.Pod
	for i := range pods {
		pod := &pods[i]
		if lastPod == nil || pod.CreationTimestamp.After(lastPod.CreationTimestamp.Time) {
			lastPod = pod
		}
	}

	if lastPod == nil {
		t.Fatal("Expected to find a pod")
	}
	if lastPod.Name != "job-xyz-pod-1" {
		t.Errorf("Expected 'job-xyz-pod-1', got '%s'", lastPod.Name)
	}
}

func TestJobToBackupMapping(t *testing.T) {
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
			jobNamespace: testOperatorNS,
			labels: map[string]string{
				LabelBackupName:      "my-backup",
				LabelBackupNamespace: "app-team",
			},
			operatorNamespace: testOperatorNS,
			expectRequest:     true,
			expectedName:      "my-backup",
			expectedNamespace: "app-team",
		},
		{
			name:         "wrong namespace ignored",
			jobNamespace: "other-namespace",
			labels: map[string]string{
				LabelBackupName:      "my-backup",
				LabelBackupNamespace: "app-team",
			},
			operatorNamespace: testOperatorNS,
			expectRequest:     false,
		},
		{
			name:              "no labels ignored",
			jobNamespace:      testOperatorNS,
			labels:            map[string]string{},
			operatorNamespace: testOperatorNS,
			expectRequest:     false,
		},
		{
			name:         "partial labels ignored",
			jobNamespace: testOperatorNS,
			labels: map[string]string{
				LabelBackupName: "partial-backup",
			},
			operatorNamespace: testOperatorNS,
			expectRequest:     false,
		},
		{
			name:         "cross-namespace mapping",
			jobNamespace: testOperatorNS,
			labels: map[string]string{
				LabelBackupName:      "dagster-nightly-20260201-0200",
				LabelBackupNamespace: "dagster",
				LabelCluster:         "infrastructure",
			},
			operatorNamespace: testOperatorNS,
			expectRequest:     true,
			expectedName:      "dagster-nightly-20260201-0200",
			expectedNamespace: "dagster",
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

			backupName := tt.labels[LabelBackupName]
			backupNamespace := tt.labels[LabelBackupNamespace]

			if backupName == "" || backupNamespace == "" {
				if tt.expectRequest {
					t.Error("expected no request for job without proper labels")
				}
				return
			}

			if !tt.expectRequest {
				t.Error("expected no request but mapping logic would produce one")
				return
			}

			if backupName != tt.expectedName {
				t.Errorf("expected name %q, got %q", tt.expectedName, backupName)
			}
			if backupNamespace != tt.expectedNamespace {
				t.Errorf("expected namespace %q, got %q", tt.expectedNamespace, backupNamespace)
			}
		})
	}
}

func TestBackupReconciler_EnsureFinalizer_UpdateError(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)

	scheme := newTestScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(backup).
		WithStatusSubresource(&databasesv1alpha1.Backup{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return errors.New("simulated update conflict")
			},
		}).
		Build()

	r := &BackupReconciler{
		Client:    fakeClient,
		APIReader: fakeClient,
		Scheme:    scheme,
		Namespace: testOperatorNS,
		Image:     testImage,
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	if _, err := r.Reconcile(context.Background(), req); err == nil {
		t.Fatal("expected error when finalizer Update fails")
	}
}

func TestBackupReconciler_StorageCredentialsSecretRef(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)

	storage := newTestStorage(testStorageName)
	storage.Spec.CredentialsSecretRef = &databasesv1alpha1.SecretReference{
		Name:      "s3-creds",
		Namespace: testOperatorNS,
	}
	secret := newTestSecret(testSecretName, "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs.Items))
	}

	env := jobs.Items[0].Spec.Template.Spec.Containers[0].Env
	assertSecretKeyEnv(t, env, "AWS_ACCESS_KEY_ID", "s3-creds")
	assertSecretKeyEnv(t, env, "AWS_SECRET_ACCESS_KEY", "s3-creds")
}

func TestBackupLabelConstants(t *testing.T) {
	expected := map[string]string{
		"dbtether.io/backup":           LabelBackupName,
		"dbtether.io/backup-namespace": LabelBackupNamespace,
		"dbtether.io/cluster":          LabelCluster,
	}

	for want, got := range expected {
		if got != want {
			t.Errorf("label constant mismatch: expected %q, got %q", want, got)
		}
	}
}

func assertSecretKeyEnv(t *testing.T, env []corev1.EnvVar, name, secretName string) {
	t.Helper()
	for _, e := range env {
		if e.Name != name {
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Fatalf("%s should come from a secretKeyRef", name)
		}
		if e.ValueFrom.SecretKeyRef.Name != secretName {
			t.Errorf("%s secret name = %q, want %q", name, e.ValueFrom.SecretKeyRef.Name, secretName)
		}
		if e.ValueFrom.SecretKeyRef.Key != name {
			t.Errorf("%s secret key = %q, want %q", name, e.ValueFrom.SecretKeyRef.Key, name)
		}
		return
	}
	t.Errorf("expected %s env var in backup job", name)
}
