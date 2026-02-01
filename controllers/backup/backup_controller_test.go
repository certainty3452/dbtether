package backup

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

// Test constants to avoid magic strings
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

// Error message templates for tests
const (
	errUnexpectedError   = "unexpected error: %v"
	errFailedToGet       = "failed to get backup: %v"
	errFailedToListJobs  = "failed to list jobs: %v"
	errExpectedOneJob    = "expected 1 job, got %d"
	errExpectedPhase     = "expected %s phase, got %s"
	testJobName          = "backup-test-backup-12345678"
	annotationBackupPath = "dbtether.io/backup-path"
	testClusterA         = "cluster-a"
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

// TestBackupReconciler_ComputeSpecHash tests hash computation
func TestBackupReconciler_ComputeSpecHash(t *testing.T) {
	r := &BackupReconciler{}

	backup1 := newTestBackup("backup1", "default")
	backup2 := newTestBackup("backup2", "default")
	backup3 := newTestBackup("backup3", "default")
	backup3.Spec.DatabaseRef.Name = "different-db"

	// Same spec should produce same hash
	hash1 := r.computeSpecHash(backup1)
	hash2 := r.computeSpecHash(backup2)
	if hash1 != hash2 {
		t.Errorf("same spec should produce same hash: %s != %s", hash1, hash2)
	}

	// Different spec should produce different hash
	hash3 := r.computeSpecHash(backup3)
	if hash1 == hash3 {
		t.Errorf("different spec should produce different hash: %s == %s", hash1, hash3)
	}

	// Hash should be non-empty and deterministic length
	if hash1 == "" || len(hash1) != 16 {
		t.Errorf("hash should be 16 chars, got %d", len(hash1))
	}
}

// TestBackupReconciler_FinalizerAdded tests finalizer is added on first reconcile
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

	// First reconcile should add finalizer and requeue
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	// Check that it requests immediate requeue (Requeue=true means RequeueAfter=0 with immediate requeue)
	if result.RequeueAfter != 0 {
		t.Error("expected immediate requeue (RequeueAfter=0) after adding finalizer")
	}

	// Verify finalizer was added
	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	if !controllerutil.ContainsFinalizer(&updatedBackup, backupFinalizer) {
		t.Error("finalizer should be added")
	}
}

// TestBackupReconciler_SkipAlreadyProcessed tests skip logic for completed/failed backups
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

// TestBackupReconciler_JobCreation tests job creation with correct labels
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

	// Verify job was created in operator namespace
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}

	if len(jobs.Items) != 1 {
		t.Fatalf(errExpectedOneJob, len(jobs.Items))
	}

	job := &jobs.Items[0]

	// Verify labels for cross-namespace tracking
	if job.Labels[LabelBackupName] != testBackupName {
		t.Errorf("backup label mismatch: %s", job.Labels[LabelBackupName])
	}
	if job.Labels[LabelBackupNamespace] != testNamespace {
		t.Errorf("backup-namespace label mismatch: %s", job.Labels[LabelBackupNamespace])
	}
	if job.Labels[LabelCluster] != testClusterName {
		t.Errorf("cluster label mismatch: %s", job.Labels[LabelCluster])
	}

	// Verify no OwnerReference (cross-namespace not allowed)
	if len(job.OwnerReferences) != 0 {
		t.Error("job should not have OwnerReferences for cross-namespace backups")
	}

	// Verify TTL is set
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 3600 {
		t.Error("TTL should be 3600 seconds")
	}
}

// TestBackupReconciler_JobAlreadyExists tests race condition handling
func TestBackupReconciler_JobAlreadyExists(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	// Pre-create a job with matching labels
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
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{Name: "backup", Image: "test"},
					},
				},
			},
		},
		Status: batchv1.JobStatus{
			Active: 1, // Job is running
		},
	}

	r := newTestReconciler(backup, db, cluster, storage, secret, existingJob)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: testNamespace,
		},
	}

	// Reconcile should find existing job and not fail
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	// Should requeue to check job status
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter for running job")
	}

	// Verify backup status is not Failed
	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}
	if updatedBackup.Status.Phase == "Failed" {
		t.Errorf("backup should not be Failed when job exists, got: %s - %s",
			updatedBackup.Status.Phase, updatedBackup.Status.Message)
	}
}

// TestBackupReconciler_JobCompleted tests completed job status propagation
func TestBackupReconciler_JobCompleted(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.Phase = "Running"

	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: func() *int32 { v := int32(3); return &v }(),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
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

// TestBackupReconciler_JobFailed tests failed job status propagation via Job Conditions
func TestBackupReconciler_JobFailed(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.Phase = "Running"

	backoffLimit := int32(3)
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Failed: 3, // Reached backoff limit
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
}

// TestBackupReconciler_Deletion tests finalizer cleanup
func TestBackupReconciler_Deletion(t *testing.T) {
	now := metav1.Now()
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.DeletionTimestamp = &now

	// Job that should be cleaned up
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
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

	// Verify job was deleted
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf(errFailedToListJobs, err)
	}
	if len(jobs.Items) != 0 {
		t.Error("job should be deleted during backup deletion")
	}

	// Verify finalizer was removed
	var updatedBackup databasesv1alpha1.Backup
	err = r.Get(context.Background(), req.NamespacedName, &updatedBackup)
	if err == nil {
		if controllerutil.ContainsFinalizer(&updatedBackup, backupFinalizer) {
			t.Error("finalizer should be removed")
		}
	}
}

// TestBackupReconciler_Throttling tests concurrent job limiting
func TestBackupReconciler_Throttling(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, "dbtether")

	// Create DefaultMaxConcurrentJobsPerCluster running jobs
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
						RestartPolicy: corev1.RestartPolicyOnFailure,
						Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
					},
				},
			},
			Status: batchv1.JobStatus{
				Active: 1, // Running
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

	// Should be throttled and requeue
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

// TestBackupReconciler_CountActiveJobs tests job counting for throttling
func TestBackupReconciler_CountActiveJobs(t *testing.T) {
	// Jobs for different clusters
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
		Status: batchv1.JobStatus{Succeeded: 1}, // Completed
	}
	job3 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-3",
			Namespace: "dbtether",
			Labels:    map[string]string{LabelCluster: "cluster-b"},
		},
		Status: batchv1.JobStatus{Active: 1},
	}

	r := newTestReconciler(job1, job2, job3)

	// Count for cluster-a: should be 1 (job2 is completed)
	count, err := r.countActiveJobsForCluster(context.Background(), testClusterA)
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if count != 1 {
		t.Errorf("expected 1 active job for cluster-a, got %d", count)
	}

	// Count for cluster-b: should be 1
	count, err = r.countActiveJobsForCluster(context.Background(), "cluster-b")
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if count != 1 {
		t.Errorf("expected 1 active job for cluster-b, got %d", count)
	}

	// Count for non-existent cluster: should be 0
	count, err = r.countActiveJobsForCluster(context.Background(), "cluster-c")
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}
	if count != 0 {
		t.Errorf("expected 0 active jobs for cluster-c, got %d", count)
	}
}

// TestBackupReconciler_FindJobByLabels tests job lookup by labels
func TestBackupReconciler_FindJobByLabels(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-abc123",
			Namespace: "dbtether",
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: func() *int32 { v := int32(3); return &v }(),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers:    []corev1.Container{{Name: "backup", Image: "test"}},
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}

	r := newTestReconciler(backup, job)

	result, err := r.findJobByLabels(context.Background(), backup, "testhash")
	if err != nil {
		t.Fatalf(errUnexpectedError, err)
	}

	// Should not requeue for completed job
	if result.RequeueAfter != 0 {
		t.Error("completed job should not trigger requeue")
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: testBackupName, Namespace: testNamespace,
	}, &updatedBackup); err != nil {
		t.Fatalf(errFailedToGet, err)
	}

	if updatedBackup.Status.Phase != "Completed" {
		t.Errorf("expected Completed, got %s", updatedBackup.Status.Phase)
	}
}

// TestBackupReconciler_ResourceNotFound tests handling of missing resources
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
			expectedErrMsg: "database test-db not found",
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

			_, err := r.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf(errUnexpectedError, err)
			}

			var backup databasesv1alpha1.Backup
			if err := r.Get(context.Background(), req.NamespacedName, &backup); err != nil {
				t.Fatalf(errFailedToGet, err)
			}

			if backup.Status.Phase != "Failed" {
				t.Errorf(errExpectedPhase, "Failed", backup.Status.Phase)
			}
			if backup.Status.Message != tt.expectedErrMsg {
				t.Errorf("expected message %q, got %q", tt.expectedErrMsg, backup.Status.Message)
			}
		})
	}
}

// TestBackupReconciler_NotReady tests handling of resources not in Ready state
func TestBackupReconciler_NotReady(t *testing.T) {
	backup := newTestBackup(testBackupName, "default")
	backup.Finalizers = []string{backupFinalizer}

	db := newTestDatabase(testDBName, "default", testClusterName)
	db.Status.Phase = "Pending" // Not ready

	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)

	r := newTestReconciler(backup, db, cluster, storage)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      testBackupName,
			Namespace: "default",
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
}

// TestBackupReconciler_ThrottlingConstants verifies throttling configuration
func TestBackupReconciler_ThrottlingConstants(t *testing.T) {
	if DefaultMaxConcurrentJobsPerCluster < 1 || DefaultMaxConcurrentJobsPerCluster > 10 {
		t.Errorf("DefaultMaxConcurrentJobsPerCluster should be 1-10, got %d", DefaultMaxConcurrentJobsPerCluster)
	}

	if RequeueDelayWhenThrottled < 10*time.Second || RequeueDelayWhenThrottled > 5*time.Minute {
		t.Errorf("RequeueDelayWhenThrottled should be 10s-5m, got %v", RequeueDelayWhenThrottled)
	}
}

// TestBackupReconciler_BackupNotFound tests graceful handling when backup is deleted
func TestBackupReconciler_BackupNotFound(t *testing.T) {
	r := newTestReconciler() // No objects

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

	// Should not requeue for not found
	if result.RequeueAfter != 0 {
		t.Error("should not requeue for not found backup")
	}
}

// TestBackupReconciler_DeleteJobNotFound tests cleanup when job already deleted
func TestBackupReconciler_DeleteJobNotFound(t *testing.T) {
	now := metav1.Now()
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.DeletionTimestamp = &now

	// No job exists - should not fail
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

	// Verify finalizer was still removed
	var updatedBackup databasesv1alpha1.Backup
	err = r.Get(context.Background(), req.NamespacedName, &updatedBackup)
	if err == nil && controllerutil.ContainsFinalizer(&updatedBackup, backupFinalizer) {
		t.Error("finalizer should be removed even if job not found")
	}
}

// TestGenerateRunID tests RunID generation
func TestGenerateRunID(t *testing.T) {
	// Test length
	runID := generateRunID()
	if len(runID) != 8 {
		t.Errorf("expected 8 characters, got %d: %s", len(runID), runID)
	}

	// Test character set (lowercase letters and digits only)
	for _, c := range runID {
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		if !isLower && !isDigit {
			t.Errorf("unexpected character in runID: %c", c)
		}
	}

	// Test uniqueness (probabilistic)
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id := generateRunID()
		if seen[id] {
			t.Logf("warning: duplicate runID found: %s (this is very unlikely)", id)
		}
		seen[id] = true
	}
}

// TestPopulateBackupResults tests reading job annotations
func TestPopulateBackupResults(t *testing.T) {
	r := &BackupReconciler{}

	tests := []struct {
		name         string
		annotations  map[string]string
		expectedPath string
		expectedSize string
		expectedDur  string
	}{
		{
			name:         "no annotations",
			annotations:  nil,
			expectedPath: "",
			expectedSize: "",
			expectedDur:  "",
		},
		{
			name:         "empty annotations",
			annotations:  map[string]string{},
			expectedPath: "",
			expectedSize: "",
			expectedDur:  "",
		},
		{
			name: "all fields populated",
			annotations: map[string]string{
				annotationBackupPath:            "cluster/db/20260120-143022.sql.gz",
				"dbtether.io/backup-size-human": "15.2 MiB",
				"dbtether.io/backup-duration":   "2.5s",
			},
			expectedPath: "cluster/db/20260120-143022.sql.gz",
			expectedSize: "15.2 MiB",
			expectedDur:  "2.5s",
		},
		{
			name: "partial fields",
			annotations: map[string]string{
				annotationBackupPath: "some/path.sql.gz",
			},
			expectedPath: "some/path.sql.gz",
			expectedSize: "",
			expectedDur:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backup := &databasesv1alpha1.Backup{}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}

			r.populateBackupResults(backup, job)

			if backup.Status.Path != tt.expectedPath {
				t.Errorf("path: expected %q, got %q", tt.expectedPath, backup.Status.Path)
			}
			if backup.Status.Size != tt.expectedSize {
				t.Errorf("size: expected %q, got %q", tt.expectedSize, backup.Status.Size)
			}
			if backup.Status.Duration != tt.expectedDur {
				t.Errorf("duration: expected %q, got %q", tt.expectedDur, backup.Status.Duration)
			}
		})
	}
}

// TestBackupReconciler_JobWithAnnotations tests completed job with result annotations
func TestBackupReconciler_JobWithAnnotations(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = "backup-test-backup-abc12345"
	backup.Status.Phase = "Running"
	backup.Status.RunID = "abc12345"

	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-abc12345",
			Namespace: "dbtether",
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
			},
			Annotations: map[string]string{
				annotationBackupPath:            "microservices/orders_db/20260120-143022.sql.gz",
				"dbtether.io/backup-size-human": "25.6 MiB",
				"dbtether.io/backup-duration":   "3.2s",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: func() *int32 { v := int32(3); return &v }(),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
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

	// Verify annotations were propagated to status
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

// TestBackupReconciler_RunIDInJobName tests that RunID is used in job name
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

	// Verify RunID was saved in status
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

	// Verify job name contains RunID
	expectedJobNamePrefix := "backup-my-backup-" + runID
	if updatedBackup.Status.JobName != expectedJobNamePrefix {
		t.Errorf("job name should be %q, got %q", expectedJobNamePrefix, updatedBackup.Status.JobName)
	}

	// Verify job exists with correct name
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

// TestBackupReconciler_CustomTTL tests custom TTL setting
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
	expectedTTL := int32(86400) // 24 hours in seconds
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != expectedTTL {
		t.Errorf("TTL should be %d, got %v", expectedTTL, job.Spec.TTLSecondsAfterFinished)
	}
}

// TestFindExistingJob tests finding existing jobs by labels
func TestFindExistingJob(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)

	tests := []struct {
		name        string
		jobs        []batchv1.Job
		expectFound bool
	}{
		{
			name:        "no jobs exist",
			jobs:        []batchv1.Job{},
			expectFound: false,
		},
		{
			name: "matching job exists",
			jobs: []batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backup-test-backup-abc12345",
						Namespace: testOperatorNS,
						Labels: map[string]string{
							LabelBackupName:      testBackupName,
							LabelBackupNamespace: testNamespace,
						},
					},
				},
			},
			expectFound: true,
		},
		{
			name: "job with different backup name",
			jobs: []batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backup-other-backup-abc12345",
						Namespace: testOperatorNS,
						Labels: map[string]string{
							LabelBackupName:      "other-backup",
							LabelBackupNamespace: testNamespace,
						},
					},
				},
			},
			expectFound: false,
		},
		{
			name: "job in different namespace",
			jobs: []batchv1.Job{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "backup-test-backup-abc12345",
						Namespace: "other-namespace",
						Labels: map[string]string{
							LabelBackupName:      testBackupName,
							LabelBackupNamespace: testNamespace,
						},
					},
				},
			},
			expectFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := make([]client.Object, 0, 1+len(tt.jobs))
			objects = append(objects, backup)
			for i := range tt.jobs {
				objects = append(objects, &tt.jobs[i])
			}

			r := newTestReconcilerWithObjects(objects...)

			job, err := r.findExistingJob(context.Background(), backup)
			if err != nil {
				t.Fatalf(errUnexpectedError, err)
			}

			if tt.expectFound && job == nil {
				t.Error("expected to find job, got nil")
			}
			if !tt.expectFound && job != nil {
				t.Errorf("expected no job, got %s", job.Name)
			}
		})
	}
}

// TestExtractRunIDFromJobName tests RunID extraction from job names
func TestExtractRunIDFromJobName(t *testing.T) {
	r := &BackupReconciler{}

	tests := []struct {
		jobName    string
		backupName string
		expected   string
	}{
		{
			jobName:    "backup-test-backup-abc12345",
			backupName: testBackupName,
			expected:   "abc12345",
		},
		{
			jobName:    "backup-my-db-backup-xyz99999",
			backupName: "my-db-backup",
			expected:   "xyz99999",
		},
		{
			jobName:    "backup-short-a",
			backupName: "short",
			expected:   "a",
		},
		{
			jobName:    "backup-exact-",
			backupName: "exact",
			expected:   "",
		},
		{
			jobName:    "backup-norunid",
			backupName: "norunid",
			expected:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.jobName, func(t *testing.T) {
			result := r.extractRunIDFromJobName(tt.jobName, tt.backupName)
			if result != tt.expected {
				t.Errorf("extractRunIDFromJobName(%q, %q) = %q, want %q",
					tt.jobName, tt.backupName, result, tt.expected)
			}
		})
	}
}

// TestCreateBackupJobIfAllowed_ExistingJob tests that no duplicate jobs are created
func TestCreateBackupJobIfAllowed_ExistingJob(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}

	db := newTestDatabase(testDBName, testNamespace, testClusterName)
	cluster := newTestCluster(testClusterName)
	storage := newTestStorage(testStorageName)
	secret := newTestSecret(testSecretName, testOperatorNS)

	// Pre-create a job for this backup
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-existing1",
			Namespace: testOperatorNS,
			Labels: map[string]string{
				LabelBackupName:      testBackupName,
				LabelBackupNamespace: testNamespace,
				LabelCluster:         testClusterName,
			},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					Containers: []corev1.Container{
						{Name: "backup", Image: testImage},
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

	// Verify only ONE job exists (the pre-existing one)
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

	// Verify backup status was updated with existing job info
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
		Scheme:    scheme,
		Image:     testImage,
		Namespace: testOperatorNS,
	}
}

// TestIsJobFailed tests the isJobFailed function with various Job conditions
func TestIsJobFailed(t *testing.T) {
	r := &BackupReconciler{}

	tests := []struct {
		name           string
		conditions     []batchv1.JobCondition
		expectFailed   bool
		expectContains string // substring expected in reason
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

			reason, failed := r.isJobFailed(job)

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

// contains checks if s contains substr
func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}

// TestBackupReconciler_JobFailedWithCondition tests that job failure is detected via conditions
func TestBackupReconciler_JobFailedWithCondition(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.Phase = "Running"

	backoffLimit := int32(3)
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
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

	// Verify failure reason is captured
	if !contains(updatedBackup.Status.Message, "BackoffLimitExceeded") {
		t.Errorf("expected message to contain 'BackoffLimitExceeded', got %q", updatedBackup.Status.Message)
	}
}

// TestBackupReconciler_CustomBackoffLimit tests custom backoff limit configuration
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

// TestBackupReconciler_ActiveDeadlineSeconds tests active deadline configuration
func TestBackupReconciler_ActiveDeadlineSeconds(t *testing.T) {
	deadline := int64(1800) // 30 minutes
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

// TestBackupReconciler_DeadlineExceeded tests handling of deadline exceeded failure
func TestBackupReconciler_DeadlineExceeded(t *testing.T) {
	backup := newTestBackup(testBackupName, testNamespace)
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = testJobName
	backup.Status.Phase = "Running"

	deadline := int64(300)
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testJobName,
			Namespace: "dbtether",
		},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds: &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
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

// TestBackupReconciler_ZeroBackoffLimit tests zero retries configuration
func TestBackupReconciler_ZeroBackoffLimit(t *testing.T) {
	backoffLimit := int32(0) // No retries
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
	// Test the logic of selecting the most recently created pod
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
				CreationTimestamp: now, // newest
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "job-abc-pod-2",
				CreationTimestamp: earlier,
			},
		},
	}

	// Simulate getLastPodName logic
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
	// Test with empty pod list
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
	// Test with single pod
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
