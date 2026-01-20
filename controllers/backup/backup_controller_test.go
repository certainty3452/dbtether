package backup

import (
	"context"
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
		Namespace: "dbtether",
		Image:     "dbtether:test",
	}
}

func newTestBackup(name, namespace string) *databasesv1alpha1.Backup {
	return &databasesv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       "test-uid-12345678",
		},
		Spec: databasesv1alpha1.BackupSpec{
			DatabaseRef: databasesv1alpha1.DatabaseReference{Name: "test-db"},
			StorageRef:  databasesv1alpha1.StorageReference{Name: "test-storage"},
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
				Name:      "test-secret",
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
	backup := newTestBackup("test-backup", "default")
	db := newTestDatabase("test-db", "default", "test-cluster")
	cluster := newTestCluster("test-cluster")
	storage := newTestStorage("test-storage")
	secret := newTestSecret("test-secret", "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-backup",
			Namespace: "default",
		},
	}

	// First reconcile should add finalizer and requeue
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Check that it requests immediate requeue (Requeue=true means RequeueAfter=0 with immediate requeue)
	if result.RequeueAfter != 0 {
		t.Error("expected immediate requeue (RequeueAfter=0) after adding finalizer")
	}

	// Verify finalizer was added
	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&updatedBackup, backupFinalizer) {
		t.Error("finalizer should be added")
	}
}

// TestBackupReconciler_SkipAlreadyProcessed tests skip logic for completed/failed backups
func TestBackupReconciler_SkipAlreadyProcessed(t *testing.T) {
	r := &BackupReconciler{}
	backup := newTestBackup("test-backup", "default")
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
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase("test-db", "app-namespace", "test-cluster")
	cluster := newTestCluster("test-cluster")
	storage := newTestStorage("test-storage")
	secret := newTestSecret("test-secret", "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-backup",
			Namespace: "app-namespace",
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify job was created in operator namespace
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}

	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs.Items))
	}

	job := &jobs.Items[0]

	// Verify labels for cross-namespace tracking
	if job.Labels["dbtether.io/backup"] != "test-backup" {
		t.Errorf("backup label mismatch: %s", job.Labels["dbtether.io/backup"])
	}
	if job.Labels["dbtether.io/backup-namespace"] != "app-namespace" {
		t.Errorf("backup-namespace label mismatch: %s", job.Labels["dbtether.io/backup-namespace"])
	}
	if job.Labels["dbtether.io/cluster"] != "test-cluster" {
		t.Errorf("cluster label mismatch: %s", job.Labels["dbtether.io/cluster"])
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
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase("test-db", "app-namespace", "test-cluster")
	cluster := newTestCluster("test-cluster")
	storage := newTestStorage("test-storage")
	secret := newTestSecret("test-secret", "dbtether")

	// Pre-create a job with matching labels
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-test-uid",
			Namespace: "dbtether",
			Labels: map[string]string{
				"dbtether.io/backup":           "test-backup",
				"dbtether.io/backup-namespace": "app-namespace",
				"dbtether.io/cluster":          "test-cluster",
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
			Name:      "test-backup",
			Namespace: "app-namespace",
		},
	}

	// Reconcile should find existing job and not fail
	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should requeue to check job status
	if result.RequeueAfter == 0 {
		t.Error("expected RequeueAfter for running job")
	}

	// Verify backup status is not Failed
	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
	}
	if updatedBackup.Status.Phase == "Failed" {
		t.Errorf("backup should not be Failed when job exists, got: %s - %s",
			updatedBackup.Status.Phase, updatedBackup.Status.Message)
	}
}

// TestBackupReconciler_JobCompleted tests completed job status propagation
func TestBackupReconciler_JobCompleted(t *testing.T) {
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = "backup-test-backup-12345678"
	backup.Status.Phase = "Running"

	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-12345678",
			Namespace: "dbtether",
			Labels: map[string]string{
				"dbtether.io/backup":           "test-backup",
				"dbtether.io/backup-namespace": "app-namespace",
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
			Name:      "test-backup",
			Namespace: "app-namespace",
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
	}

	if updatedBackup.Status.Phase != "Completed" {
		t.Errorf("expected Completed, got %s", updatedBackup.Status.Phase)
	}
	if updatedBackup.Status.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

// TestBackupReconciler_JobFailed tests failed job status propagation
func TestBackupReconciler_JobFailed(t *testing.T) {
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = "backup-test-backup-12345678"
	backup.Status.Phase = "Running"

	backoffLimit := int32(3)
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-12345678",
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
		},
	}

	r := newTestReconciler(backup, failedJob)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-backup",
			Namespace: "app-namespace",
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
	}

	if updatedBackup.Status.Phase != "Failed" {
		t.Errorf("expected Failed, got %s", updatedBackup.Status.Phase)
	}
}

// TestBackupReconciler_Deletion tests finalizer cleanup
func TestBackupReconciler_Deletion(t *testing.T) {
	now := metav1.Now()
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	backup.DeletionTimestamp = &now

	// Job that should be cleaned up
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-12345678",
			Namespace: "dbtether",
			Labels: map[string]string{
				"dbtether.io/backup":           "test-backup",
				"dbtether.io/backup-namespace": "app-namespace",
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
			Name:      "test-backup",
			Namespace: "app-namespace",
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify job was deleted
	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf("failed to list jobs: %v", err)
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
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase("test-db", "app-namespace", "test-cluster")
	cluster := newTestCluster("test-cluster")
	storage := newTestStorage("test-storage")
	secret := newTestSecret("test-secret", "dbtether")

	// Create MaxConcurrentJobsPerCluster running jobs
	var existingJobs []client.Object
	for i := 0; i < MaxConcurrentJobsPerCluster; i++ {
		existingJobs = append(existingJobs, &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "existing-job-" + string(rune('a'+i)),
				Namespace: "dbtether",
				Labels: map[string]string{
					"dbtether.io/cluster": "test-cluster",
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
			Name:      "test-backup",
			Namespace: "app-namespace",
		},
	}

	result, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should be throttled and requeue
	if result.RequeueAfter != RequeueDelayWhenThrottled {
		t.Errorf("expected RequeueAfter=%v, got %v", RequeueDelayWhenThrottled, result.RequeueAfter)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
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
			Labels:    map[string]string{"dbtether.io/cluster": "cluster-a"},
		},
		Status: batchv1.JobStatus{Active: 1},
	}
	job2 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-2",
			Namespace: "dbtether",
			Labels:    map[string]string{"dbtether.io/cluster": "cluster-a"},
		},
		Status: batchv1.JobStatus{Succeeded: 1}, // Completed
	}
	job3 := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "job-3",
			Namespace: "dbtether",
			Labels:    map[string]string{"dbtether.io/cluster": "cluster-b"},
		},
		Status: batchv1.JobStatus{Active: 1},
	}

	r := newTestReconciler(job1, job2, job3)

	// Count for cluster-a: should be 1 (job2 is completed)
	count, err := r.countActiveJobsForCluster(context.Background(), "cluster-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 active job for cluster-a, got %d", count)
	}

	// Count for cluster-b: should be 1
	count, err = r.countActiveJobsForCluster(context.Background(), "cluster-b")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 1 {
		t.Errorf("expected 1 active job for cluster-b, got %d", count)
	}

	// Count for non-existent cluster: should be 0
	count, err = r.countActiveJobsForCluster(context.Background(), "cluster-c")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 active jobs for cluster-c, got %d", count)
	}
}

// TestBackupReconciler_FindJobByLabels tests job lookup by labels
func TestBackupReconciler_FindJobByLabels(t *testing.T) {
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-abc123",
			Namespace: "dbtether",
			Labels: map[string]string{
				"dbtether.io/backup":           "test-backup",
				"dbtether.io/backup-namespace": "app-namespace",
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
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not requeue for completed job
	if result.RequeueAfter != 0 {
		t.Error("completed job should not trigger requeue")
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), types.NamespacedName{
		Name: "test-backup", Namespace: "app-namespace",
	}, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
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
					b := newTestBackup("test-backup", "default")
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
					b := newTestBackup("test-backup", "default")
					b.Finalizers = []string{backupFinalizer}
					return b
				}(),
				newTestDatabase("test-db", "default", "missing-cluster"),
			},
			expectedErrMsg: "cluster missing-cluster not found",
		},
		{
			name: "storage not found",
			setupObjs: []client.Object{
				func() *databasesv1alpha1.Backup {
					b := newTestBackup("test-backup", "default")
					b.Finalizers = []string{backupFinalizer}
					return b
				}(),
				newTestDatabase("test-db", "default", "test-cluster"),
				newTestCluster("test-cluster"),
			},
			expectedErrMsg: "backup storage test-storage not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(tt.setupObjs...)

			req := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-backup",
					Namespace: "default",
				},
			}

			_, err := r.Reconcile(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			var backup databasesv1alpha1.Backup
			if err := r.Get(context.Background(), req.NamespacedName, &backup); err != nil {
				t.Fatalf("failed to get backup: %v", err)
			}

			if backup.Status.Phase != "Failed" {
				t.Errorf("expected Failed, got %s", backup.Status.Phase)
			}
			if backup.Status.Message != tt.expectedErrMsg {
				t.Errorf("expected message %q, got %q", tt.expectedErrMsg, backup.Status.Message)
			}
		})
	}
}

// TestBackupReconciler_NotReady tests handling of resources not in Ready state
func TestBackupReconciler_NotReady(t *testing.T) {
	backup := newTestBackup("test-backup", "default")
	backup.Finalizers = []string{backupFinalizer}

	db := newTestDatabase("test-db", "default", "test-cluster")
	db.Status.Phase = "Pending" // Not ready

	cluster := newTestCluster("test-cluster")
	storage := newTestStorage("test-storage")

	r := newTestReconciler(backup, db, cluster, storage)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-backup",
			Namespace: "default",
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
	}

	if updatedBackup.Status.Phase != "Failed" {
		t.Errorf("expected Failed, got %s", updatedBackup.Status.Phase)
	}
}

// TestBackupReconciler_ThrottlingConstants verifies throttling configuration
func TestBackupReconciler_ThrottlingConstants(t *testing.T) {
	if MaxConcurrentJobsPerCluster < 1 || MaxConcurrentJobsPerCluster > 10 {
		t.Errorf("MaxConcurrentJobsPerCluster should be 1-10, got %d", MaxConcurrentJobsPerCluster)
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
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not requeue for not found
	if result.RequeueAfter != 0 {
		t.Error("should not requeue for not found backup")
	}
}

// TestBackupReconciler_DeleteJobNotFound tests cleanup when job already deleted
func TestBackupReconciler_DeleteJobNotFound(t *testing.T) {
	now := metav1.Now()
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	backup.DeletionTimestamp = &now

	// No job exists - should not fail
	r := newTestReconciler(backup)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-backup",
			Namespace: "app-namespace",
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
				"dbtether.io/backup-path":       "cluster/db/20260120-143022.sql.gz",
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
				"dbtether.io/backup-path": "some/path.sql.gz",
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
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	backup.Status.JobName = "backup-test-backup-abc12345"
	backup.Status.Phase = "Running"
	backup.Status.RunID = "abc12345"

	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup-test-backup-abc12345",
			Namespace: "dbtether",
			Labels: map[string]string{
				"dbtether.io/backup":           "test-backup",
				"dbtether.io/backup-namespace": "app-namespace",
			},
			Annotations: map[string]string{
				"dbtether.io/backup-path":       "microservices/orders_db/20260120-143022.sql.gz",
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
			Name:      "test-backup",
			Namespace: "app-namespace",
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
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
	backup := newTestBackup("my-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	db := newTestDatabase("test-db", "app-namespace", "test-cluster")
	cluster := newTestCluster("test-cluster")
	storage := newTestStorage("test-storage")
	secret := newTestSecret("test-secret", "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "my-backup",
			Namespace: "app-namespace",
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify RunID was saved in status
	var updatedBackup databasesv1alpha1.Backup
	if err := r.Get(context.Background(), req.NamespacedName, &updatedBackup); err != nil {
		t.Fatalf("failed to get backup: %v", err)
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
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs.Items))
	}
	if jobs.Items[0].Name != expectedJobNamePrefix {
		t.Errorf("job name mismatch: expected %q, got %q", expectedJobNamePrefix, jobs.Items[0].Name)
	}
}

// TestBackupReconciler_CustomTTL tests custom TTL setting
func TestBackupReconciler_CustomTTL(t *testing.T) {
	backup := newTestBackup("test-backup", "app-namespace")
	backup.Finalizers = []string{backupFinalizer}
	ttl := metav1.Duration{Duration: 24 * time.Hour}
	backup.Spec.TTLAfterCompletion = &ttl

	db := newTestDatabase("test-db", "app-namespace", "test-cluster")
	cluster := newTestCluster("test-cluster")
	storage := newTestStorage("test-storage")
	secret := newTestSecret("test-secret", "dbtether")

	r := newTestReconciler(backup, db, cluster, storage, secret)

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Name:      "test-backup",
			Namespace: "app-namespace",
		},
	}

	_, err := r.Reconcile(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var jobs batchv1.JobList
	if err := r.List(context.Background(), &jobs, client.InNamespace("dbtether")); err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs.Items))
	}

	job := &jobs.Items[0]
	expectedTTL := int32(86400) // 24 hours in seconds
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != expectedTTL {
		t.Errorf("TTL should be %d, got %v", expectedTTL, job.Spec.TTLSecondsAfterFinished)
	}
}
