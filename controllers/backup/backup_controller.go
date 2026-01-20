package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

const backupFinalizer = "dbtether.io/backup-job"

const (
	// MaxConcurrentJobsPerCluster limits parallel backup jobs per DBCluster to avoid overloading connection pool
	MaxConcurrentJobsPerCluster = 3
	// RequeueDelayWhenThrottled is how long to wait before retrying when job limit is reached
	RequeueDelayWhenThrottled = 30 * time.Second
)

type BackupReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Image     string
	Namespace string
}

// +kubebuilder:rbac:groups=dbtether.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=dbtether.io,resources=dbclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var backup databasesv1alpha1.Backup
	if err := r.Get(ctx, req.NamespacedName, &backup); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion - cleanup Job via finalizer
	if !backup.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &backup)
	}

	// Add finalizer if not present
	if !controllerutil.ContainsFinalizer(&backup, backupFinalizer) {
		controllerutil.AddFinalizer(&backup, backupFinalizer)
		if err := r.Update(ctx, &backup); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	specHash := r.computeSpecHash(&backup)

	// Protection against re-runs: if already completed with same spec, skip
	if backup.Status.Phase != "" && backup.Status.SpecHash == specHash {
		if backup.Status.Phase == "Completed" || backup.Status.Phase == "Failed" {
			logger.V(1).Info("backup already processed", "phase", backup.Status.Phase)
			return ctrl.Result{}, nil
		}
	}

	logger.V(1).Info("reconciling backup", "database", backup.Spec.DatabaseRef.Name)

	// If Job already created, just check its status (no need to fetch all resources)
	if backup.Status.JobName != "" {
		return r.checkJobStatus(ctx, &backup, specHash)
	}

	// Get all required resources for job creation
	db, cluster, storage, err := r.getResources(ctx, &backup)
	if err != nil {
		return r.updateStatus(ctx, &backup, "Failed", err.Error(), specHash)
	}

	// Throttling: check concurrent jobs per cluster
	activeJobs, err := r.countActiveJobsForCluster(ctx, cluster.Name)
	if err != nil {
		logger.Error(err, "failed to count active jobs")
		return ctrl.Result{RequeueAfter: RequeueDelayWhenThrottled}, nil
	}
	if activeJobs >= MaxConcurrentJobsPerCluster {
		logger.Info("throttling: too many concurrent backup jobs for cluster",
			"cluster", cluster.Name, "active", activeJobs, "max", MaxConcurrentJobsPerCluster)
		return r.updateStatus(ctx, &backup, "Pending", fmt.Sprintf("waiting for other backups to complete (active: %d/%d)", activeJobs, MaxConcurrentJobsPerCluster), specHash)
	}

	// Generate RunID for this backup run (used in job name, filename, and tracking)
	runID := generateRunID()

	// Create Job with all configuration
	job, err := r.createBackupJob(ctx, &backup, db, cluster, storage, runID)
	if err != nil {
		// If job already exists, find it and check status (race condition handling)
		if errors.IsAlreadyExists(err) {
			logger.V(1).Info("job already exists, checking status")
			return r.findJobByLabels(ctx, &backup, specHash)
		}
		logger.Error(err, "failed to create backup job")
		return r.updateStatus(ctx, &backup, "Failed", fmt.Sprintf("failed to create job: %s", err.Error()), specHash)
	}

	logger.Info("backup job created", "job", job.Name, "runId", runID)
	return r.updateStatusWithJobAndRunID(ctx, &backup, "Running", "backup job started", specHash, job.Name, runID)
}

func (r *BackupReconciler) computeSpecHash(backup *databasesv1alpha1.Backup) string {
	data, _ := json.Marshal(backup.Spec) //nolint:errcheck // hash doesn't need to be perfect
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:8])
}

// generateRunID creates a unique 8-character alphanumeric identifier for this backup run
func generateRunID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fallback to timestamp-based if crypto/rand fails
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xFFFFFFFF)
	}
	for i := range b {
		b[i] = charset[b[i]%byte(len(charset))]
	}
	return string(b)
}

func (r *BackupReconciler) handleDeletion(ctx context.Context, backup *databasesv1alpha1.Backup) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(backup, backupFinalizer) {
		return ctrl.Result{}, nil
	}

	// Find and delete the Job by labels
	if err := r.deleteBackupJob(ctx, backup); err != nil {
		logger.Error(err, "failed to delete backup job during cleanup")
		// Continue with finalizer removal - TTL will clean up the job
	}

	// Remove finalizer
	controllerutil.RemoveFinalizer(backup, backupFinalizer)
	if err := r.Update(ctx, backup); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("backup deleted, job cleaned up")
	return ctrl.Result{}, nil
}

func (r *BackupReconciler) deleteBackupJob(ctx context.Context, backup *databasesv1alpha1.Backup) error {
	// Find job by labels
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.Namespace), client.MatchingLabels{
		"dbtether.io/backup":           backup.Name,
		"dbtether.io/backup-namespace": backup.Namespace,
	}); err != nil {
		return err
	}

	propagation := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		if err := r.Delete(ctx, &jobs.Items[i], &client.DeleteOptions{
			PropagationPolicy: &propagation,
		}); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

func (r *BackupReconciler) countActiveJobsForCluster(ctx context.Context, clusterName string) (int, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.Namespace)); err != nil {
		return 0, err
	}

	active := 0
	for i := range jobs.Items {
		job := &jobs.Items[i]
		// Check if this job is for our cluster (via labels)
		if job.Labels["dbtether.io/cluster"] != clusterName {
			continue
		}
		// Count running jobs (not completed, not failed)
		if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
			active++
		}
	}
	return active, nil
}

func (r *BackupReconciler) getResources(ctx context.Context, backup *databasesv1alpha1.Backup) (
	*databasesv1alpha1.Database, *databasesv1alpha1.DBCluster, *databasesv1alpha1.BackupStorage, error) {

	// Get Database
	var db databasesv1alpha1.Database
	if err := r.Get(ctx, types.NamespacedName{
		Name:      backup.Spec.DatabaseRef.Name,
		Namespace: backup.Namespace,
	}, &db); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil, fmt.Errorf("database %s not found", backup.Spec.DatabaseRef.Name)
		}
		return nil, nil, nil, err
	}

	if db.Status.Phase != "Ready" {
		return nil, nil, nil, fmt.Errorf("database %s is not ready (phase: %s)", backup.Spec.DatabaseRef.Name, db.Status.Phase)
	}

	// Get DBCluster
	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil, fmt.Errorf("cluster %s not found", db.Spec.ClusterRef.Name)
		}
		return nil, nil, nil, err
	}

	if cluster.Status.Phase != "Connected" {
		return nil, nil, nil, fmt.Errorf("cluster %s is not connected", db.Spec.ClusterRef.Name)
	}

	// Get BackupStorage
	var storage databasesv1alpha1.BackupStorage
	if err := r.Get(ctx, types.NamespacedName{Name: backup.Spec.StorageRef.Name}, &storage); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil, fmt.Errorf("backup storage %s not found", backup.Spec.StorageRef.Name)
		}
		return nil, nil, nil, err
	}

	if storage.Status.Phase != "Ready" {
		return nil, nil, nil, fmt.Errorf("backup storage %s is not ready (phase: %s)", backup.Spec.StorageRef.Name, storage.Status.Phase)
	}

	return &db, &cluster, &storage, nil
}

func (r *BackupReconciler) createBackupJob(ctx context.Context, backup *databasesv1alpha1.Backup,
	db *databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster, storage *databasesv1alpha1.BackupStorage, runID string) (*batchv1.Job, error) {

	jobName := fmt.Sprintf("backup-%s-%s", backup.Name, runID)

	// Build environment variables (preallocate for common case)
	env := make([]corev1.EnvVar, 0, 24)
	env = append(env, []corev1.EnvVar{
		{Name: "DB_HOST", Value: cluster.Spec.Endpoint},
		{Name: "DB_PORT", Value: strconv.Itoa(cluster.Spec.Port)},
		{Name: "DB_NAME", Value: db.Status.DatabaseName},
		{Name: "CLUSTER_NAME", Value: cluster.Name},
		{Name: "DATABASE_NAME", Value: db.Status.DatabaseName},
		{Name: "PATH_TEMPLATE", Value: storage.Spec.PathTemplate},
		{Name: "FILENAME_TEMPLATE", Value: backup.Spec.FilenameTemplate},
		// Metadata for S3 object tags
		{Name: "BACKUP_NAME", Value: backup.Name},
		{Name: "BACKUP_NAMESPACE", Value: backup.Namespace},
		// RunID for unified identification (job name, filename, tracking)
		{Name: "RUN_ID", Value: runID},
		// Job info for self-annotation
		{Name: "JOB_NAME", Value: jobName},
		{Name: "JOB_NAMESPACE", Value: r.Namespace},
	}...)

	// Add DB credentials from cluster
	env = append(env, r.getClusterCredentialsEnv(cluster)...)

	// Add storage configuration
	env = append(env, r.getStorageEnv(storage)...)

	backoffLimit := int32(3)

	// Use TTL from spec, or default to 1 hour
	var ttlSeconds int32 = 3600
	if backup.Spec.TTLAfterCompletion != nil {
		ttlSeconds = int32(backup.Spec.TTLAfterCompletion.Seconds())
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: r.Namespace,
			Labels: map[string]string{
				"dbtether.io/backup":           backup.Name,
				"dbtether.io/backup-namespace": backup.Namespace,
				"dbtether.io/database":         backup.Spec.DatabaseRef.Name,
				"dbtether.io/cluster":          cluster.Name,
			},
			// No OwnerReference - cross-namespace not allowed
			// Cleanup via finalizer on Backup + TTL as fallback
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSeconds,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ServiceAccountName: "dbtether", // Uses operator's SA for IRSA
					Containers: []corev1.Container{
						{
							Name:  "backup",
							Image: r.Image,
							Args:  []string{"--mode=job"},
							Env:   env,
						},
					},
				},
			},
		},
	}

	if err := r.Create(ctx, job); err != nil {
		return nil, err
	}

	return job, nil
}

func (r *BackupReconciler) getClusterCredentialsEnv(cluster *databasesv1alpha1.DBCluster) []corev1.EnvVar {
	var env []corev1.EnvVar

	if cluster.Spec.CredentialsSecretRef != nil {
		env = append(env,
			corev1.EnvVar{
				Name: "DB_USER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: cluster.Spec.CredentialsSecretRef.Name},
						Key:                  "username",
					},
				},
			},
			corev1.EnvVar{
				Name: "DB_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: cluster.Spec.CredentialsSecretRef.Name},
						Key:                  "password",
					},
				},
			},
		)
	}
	// CredentialsFromEnv not supported for backup jobs - use credentialsSecretRef

	return env
}

func (r *BackupReconciler) getStorageEnv(storage *databasesv1alpha1.BackupStorage) []corev1.EnvVar {
	var env []corev1.EnvVar

	if storage.Spec.S3 != nil {
		env = append(env,
			corev1.EnvVar{Name: "STORAGE_TYPE", Value: "s3"},
			corev1.EnvVar{Name: "S3_BUCKET", Value: storage.Spec.S3.Bucket},
			corev1.EnvVar{Name: "S3_REGION", Value: storage.Spec.S3.Region},
		)
		if storage.Spec.S3.Endpoint != "" {
			env = append(env, corev1.EnvVar{Name: "S3_ENDPOINT", Value: storage.Spec.S3.Endpoint})
		}

		// Add credentials if specified
		if storage.Spec.CredentialsSecretRef != nil {
			env = append(env,
				corev1.EnvVar{
					Name: "AWS_ACCESS_KEY_ID",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: storage.Spec.CredentialsSecretRef.Name},
							Key:                  "AWS_ACCESS_KEY_ID",
						},
					},
				},
				corev1.EnvVar{
					Name: "AWS_SECRET_ACCESS_KEY",
					ValueFrom: &corev1.EnvVarSource{
						SecretKeyRef: &corev1.SecretKeySelector{
							LocalObjectReference: corev1.LocalObjectReference{Name: storage.Spec.CredentialsSecretRef.Name},
							Key:                  "AWS_SECRET_ACCESS_KEY",
						},
					},
				},
			)
		}
		// If no credentials, IRSA/Pod Identity will be used
	}

	// GCS and Azure support will be added in future versions

	return env
}

func (r *BackupReconciler) checkJobStatus(ctx context.Context, backup *databasesv1alpha1.Backup,
	specHash string) (ctrl.Result, error) {

	// Find job by name or labels
	var job batchv1.Job
	if backup.Status.JobName != "" {
		if err := r.Get(ctx, types.NamespacedName{
			Name:      backup.Status.JobName,
			Namespace: r.Namespace,
		}, &job); err != nil {
			if errors.IsNotFound(err) {
				// Job might have been cleaned by TTL, check by labels
				return r.findJobByLabels(ctx, backup, specHash)
			}
			return ctrl.Result{}, err
		}
	} else {
		return r.findJobByLabels(ctx, backup, specHash)
	}

	return r.evaluateJobStatus(ctx, backup, &job, specHash)
}

func (r *BackupReconciler) findJobByLabels(ctx context.Context, backup *databasesv1alpha1.Backup,
	specHash string) (ctrl.Result, error) {

	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.Namespace), client.MatchingLabels{
		"dbtether.io/backup":           backup.Name,
		"dbtether.io/backup-namespace": backup.Namespace,
	}); err != nil {
		return ctrl.Result{}, err
	}

	if len(jobs.Items) == 0 {
		return r.updateStatus(ctx, backup, "Failed", "backup job not found", specHash)
	}

	// Use the most recent job
	job := &jobs.Items[0]

	// Update JobName in status if not set, then evaluate status
	if backup.Status.JobName == "" {
		// First update JobName, then evaluate - this handles race condition
		if _, err := r.updateStatusWithJob(ctx, backup, "Running", "backup job running", specHash, job.Name); err != nil {
			return ctrl.Result{}, err
		}
	}

	return r.evaluateJobStatus(ctx, backup, job, specHash)
}

func (r *BackupReconciler) evaluateJobStatus(ctx context.Context, backup *databasesv1alpha1.Backup,
	job *batchv1.Job, specHash string) (ctrl.Result, error) {

	if job.Status.Succeeded > 0 {
		// Read backup results from job annotations and update status with them
		return r.updateStatusCompleted(ctx, backup, job, specHash)
	}

	if job.Status.Failed > 0 && job.Spec.BackoffLimit != nil && job.Status.Failed >= *job.Spec.BackoffLimit {
		return r.updateStatus(ctx, backup, "Failed", "backup job failed", specHash)
	}

	// Still running
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *BackupReconciler) updateStatus(ctx context.Context, backup *databasesv1alpha1.Backup,
	phase, message, specHash string) (ctrl.Result, error) {

	return r.updateStatusWithJob(ctx, backup, phase, message, specHash, backup.Status.JobName)
}

func (r *BackupReconciler) updateStatusWithJob(ctx context.Context, backup *databasesv1alpha1.Backup,
	phase, message, specHash, jobName string) (ctrl.Result, error) {

	return r.updateStatusWithJobAndRunID(ctx, backup, phase, message, specHash, jobName, backup.Status.RunID)
}

func (r *BackupReconciler) updateStatusWithJobAndRunID(ctx context.Context, backup *databasesv1alpha1.Backup,
	phase, message, specHash, jobName, runID string) (ctrl.Result, error) {

	patch := client.MergeFrom(backup.DeepCopy())
	backup.Status.Phase = phase
	backup.Status.Message = message
	backup.Status.SpecHash = specHash
	backup.Status.JobName = jobName
	backup.Status.RunID = runID
	backup.Status.ObservedGeneration = backup.Generation

	now := metav1.Now()
	if phase == "Running" && backup.Status.StartedAt == nil {
		backup.Status.StartedAt = &now
	}
	if phase == "Completed" || phase == "Failed" {
		backup.Status.CompletedAt = &now
	}

	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

	if phase == "Running" {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	if phase == "Pending" {
		return ctrl.Result{RequeueAfter: RequeueDelayWhenThrottled}, nil
	}

	return ctrl.Result{}, nil
}

// updateStatusCompleted handles completed job status update including job annotations
func (r *BackupReconciler) updateStatusCompleted(ctx context.Context, backup *databasesv1alpha1.Backup,
	job *batchv1.Job, specHash string) (ctrl.Result, error) {

	patch := client.MergeFrom(backup.DeepCopy())

	// Core status fields
	backup.Status.Phase = "Completed"
	backup.Status.Message = "backup completed successfully"
	backup.Status.SpecHash = specHash
	backup.Status.ObservedGeneration = backup.Generation

	now := metav1.Now()
	backup.Status.CompletedAt = &now

	// Populate results from job annotations
	if job.Annotations != nil {
		if path := job.Annotations["dbtether.io/backup-path"]; path != "" {
			backup.Status.Path = path
		}
		if size := job.Annotations["dbtether.io/backup-size-human"]; size != "" {
			backup.Status.Size = size
		}
		if duration := job.Annotations["dbtether.io/backup-duration"]; duration != "" {
			backup.Status.Duration = duration
		}
	}

	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// populateBackupResults reads backup results from Job annotations and populates Backup status.
//
// Deprecated: use updateStatusCompleted instead which handles patching correctly.
func (r *BackupReconciler) populateBackupResults(backup *databasesv1alpha1.Backup, job *batchv1.Job) {
	if job.Annotations == nil {
		return
	}

	if path := job.Annotations["dbtether.io/backup-path"]; path != "" {
		backup.Status.Path = path
	}
	if size := job.Annotations["dbtether.io/backup-size-human"]; size != "" {
		backup.Status.Size = size
	}
	if duration := job.Annotations["dbtether.io/backup-duration"]; duration != "" {
		backup.Status.Duration = duration
	}
}

func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.Backup{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
