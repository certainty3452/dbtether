package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

const backupFinalizer = "dbtether.io/backup-job"

const (
	// DefaultMaxConcurrentJobsPerCluster is the default limit for parallel backup jobs per DBCluster
	DefaultMaxConcurrentJobsPerCluster = 3
	// RequeueDelayWhenThrottled is how long to wait before retrying when job limit is reached
	RequeueDelayWhenThrottled = 30 * time.Second
)

// Label keys for backup resources
const (
	LabelBackupName      = "dbtether.io/backup"
	LabelBackupNamespace = "dbtether.io/backup-namespace"
	LabelCluster         = "dbtether.io/cluster"
)

type BackupReconciler struct {
	client.Client
	Scheme               *runtime.Scheme
	Recorder             record.EventRecorder
	Image                string
	Namespace            string
	SSLMode              string // propagated into Job env so it matches operator's posture
	MaxConcurrentBackups int    // limit per DBCluster, default 3

	// PodAnnotations are applied to all backup pods.
	// Set via Helm values (backup.podAnnotations) for Karpenter/autoscaler protection.
	PodAnnotations map[string]string

	// PodLabels are applied to all backup pods (in addition to required labels).
	// Set via Helm values (backup.podLabels).
	PodLabels map[string]string

	// JobLabels are applied to all backup Job objects (in addition to required labels).
	// Set via Helm values (backup.jobLabels).
	JobLabels map[string]string

	// PodResources defines resource limits/requests for backup pods.
	// Set via Helm values (backup.resources).
	PodResources corev1.ResourceRequirements
}

// Event reason constants for backup operations
const (
	EventReasonBackupStarted   = "BackupStarted"
	EventReasonBackupCompleted = "BackupCompleted"
	EventReasonBackupFailed    = "BackupFailed"
	EventReasonBackupThrottled = "BackupThrottled"
)

func (r *BackupReconciler) maxConcurrent() int {
	if r.MaxConcurrentBackups <= 0 {
		return DefaultMaxConcurrentJobsPerCluster
	}
	return r.MaxConcurrentBackups
}

// +kubebuilder:rbac:groups=dbtether.io,resources=backups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=backups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=backups/finalizers,verbs=update
// +kubebuilder:rbac:groups=dbtether.io,resources=dbclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

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

	// Ensure finalizer
	if result, done, err := r.ensureFinalizer(ctx, &backup); done {
		return result, err
	}

	specHash := r.computeSpecHash(&backup)

	// Skip if already processed
	if r.isAlreadyProcessed(&backup, specHash, logger) {
		return ctrl.Result{}, nil
	}

	logger.V(1).Info("reconciling backup", "database", backup.Spec.DatabaseRef.Name)

	// If Job already created, just check its status
	if backup.Status.JobName != "" {
		return r.checkJobStatus(ctx, &backup, specHash)
	}

	return r.createBackupJobIfAllowed(ctx, &backup, specHash, logger)
}

func (r *BackupReconciler) ensureFinalizer(ctx context.Context, backup *databasesv1alpha1.Backup) (result ctrl.Result, done bool, err error) {
	if controllerutil.ContainsFinalizer(backup, backupFinalizer) {
		return ctrl.Result{}, false, nil
	}
	controllerutil.AddFinalizer(backup, backupFinalizer)
	if err := r.Update(ctx, backup); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{Requeue: true}, true, nil
}

func (r *BackupReconciler) isAlreadyProcessed(backup *databasesv1alpha1.Backup, specHash string, logger logr.Logger) bool {
	if backup.Status.Phase == "" || backup.Status.SpecHash != specHash {
		return false
	}
	if backup.Status.Phase == "Completed" || backup.Status.Phase == "Failed" {
		logger.V(1).Info("backup already processed", "phase", backup.Status.Phase)
		return true
	}
	return false
}

func (r *BackupReconciler) createBackupJobIfAllowed(ctx context.Context, backup *databasesv1alpha1.Backup, specHash string, logger logr.Logger) (ctrl.Result, error) {
	// FIRST: Check if job already exists by labels (prevents race condition with duplicate jobs)
	existingJob, err := r.findExistingJob(ctx, backup)
	if err != nil {
		return ctrl.Result{}, err
	}
	if existingJob != nil {
		// Job already exists - update status and evaluate
		logger.V(1).Info("job already exists, using existing", "job", existingJob.Name)
		runID := r.extractRunIDFromJobName(existingJob.Name, backup.Name)
		if _, err := r.updateStatusWithJobAndRunID(ctx, backup, "Running", "backup job running", specHash, existingJob.Name, runID); err != nil {
			return ctrl.Result{}, err
		}
		return r.evaluateJobStatus(ctx, backup, existingJob, specHash)
	}

	// Get all required resources for job creation
	db, cluster, storage, err := r.getResources(ctx, backup)
	if err != nil {
		// Dependencies may simply not be applied/ready yet under GitOps ordering;
		// keep the backup pending and retry rather than burning it as terminally Failed.
		if isDependencyNotReady(err) {
			return r.updateStatus(ctx, backup, "Pending", err.Error(), specHash)
		}
		return ctrl.Result{}, err
	}

	// Check throttling
	if result, throttled := r.checkThrottling(ctx, backup, cluster.Name, specHash, logger); throttled {
		return result, nil
	}

	// Generate RunID for this backup run (only if no existing job)
	runID := generateRunID()

	// Create Job
	job, err := r.createBackupJob(ctx, backup, db, cluster, storage, runID)
	if err != nil {
		return r.handleJobCreationError(ctx, backup, specHash, err, logger)
	}

	logger.Info("backup job created", "job", job.Name, "runId", runID)

	// Emit start event
	if r.Recorder != nil {
		eventMessage := fmt.Sprintf("Started backup job %s for database %s",
			job.Name, backup.Spec.DatabaseRef.Name)
		r.Recorder.Event(backup, corev1.EventTypeNormal, EventReasonBackupStarted, eventMessage)
	}

	return r.updateStatusWithJobAndRunID(ctx, backup, "Running", "backup job started", specHash, job.Name, runID)
}

// findExistingJob looks for an existing job by backup labels
func (r *BackupReconciler) findExistingJob(ctx context.Context, backup *databasesv1alpha1.Backup) (*batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.Namespace), client.MatchingLabels{
		LabelBackupName:      backup.Name,
		LabelBackupNamespace: backup.Namespace,
	}); err != nil {
		return nil, err
	}

	if len(jobs.Items) == 0 {
		return nil, nil
	}

	return &jobs.Items[0], nil
}

// extractRunIDFromJobName extracts RunID from job name format: backup-{backupName}-{runID}
func (r *BackupReconciler) extractRunIDFromJobName(jobName, backupName string) string {
	prefix := fmt.Sprintf("backup-%s-", backupName)
	if len(jobName) > len(prefix) {
		return jobName[len(prefix):]
	}
	return ""
}

func (r *BackupReconciler) checkThrottling(ctx context.Context, backup *databasesv1alpha1.Backup, clusterName, specHash string, logger logr.Logger) (ctrl.Result, bool) {
	activeJobs, err := r.countActiveJobsForCluster(ctx, clusterName)
	if err != nil {
		logger.Error(err, "failed to count active jobs")
		return ctrl.Result{RequeueAfter: RequeueDelayWhenThrottled}, true
	}
	maxConcurrent := r.maxConcurrent()
	if activeJobs >= maxConcurrent {
		logger.Info("throttling: too many concurrent backup jobs for cluster",
			"cluster", clusterName, "active", activeJobs, "max", maxConcurrent)
		result, _ := r.updateStatus(ctx, backup, "Pending", fmt.Sprintf("waiting for other backups to complete (active: %d/%d)", activeJobs, maxConcurrent), specHash)
		return result, true
	}
	return ctrl.Result{}, false
}

func (r *BackupReconciler) handleJobCreationError(ctx context.Context, backup *databasesv1alpha1.Backup, specHash string, err error, logger logr.Logger) (ctrl.Result, error) {
	if errors.IsAlreadyExists(err) {
		logger.V(1).Info("job already exists, checking status")
		return r.findJobByLabels(ctx, backup, specHash)
	}
	logger.Error(err, "failed to create backup job")
	return r.updateStatus(ctx, backup, "Failed", fmt.Sprintf("failed to create job: %s", err.Error()), specHash)
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

	// Remove finalizer using Patch to avoid optimistic locking conflicts
	patch := client.MergeFrom(backup.DeepCopy())
	controllerutil.RemoveFinalizer(backup, backupFinalizer)
	if err := r.Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("backup deleted, job cleaned up")
	return ctrl.Result{}, nil
}

func (r *BackupReconciler) deleteBackupJob(ctx context.Context, backup *databasesv1alpha1.Backup) error {
	// Find job by labels
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.Namespace), client.MatchingLabels{
		LabelBackupName:      backup.Name,
		LabelBackupNamespace: backup.Namespace,
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
	if err := r.List(ctx, &jobs,
		client.InNamespace(r.Namespace),
		client.MatchingLabels{"dbtether.io/cluster": clusterName},
	); err != nil {
		return 0, err
	}

	active := 0
	for i := range jobs.Items {
		job := &jobs.Items[i]
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
			return nil, nil, nil, newDependencyNotReady("database %s not found", backup.Spec.DatabaseRef.Name)
		}
		return nil, nil, nil, err
	}

	if db.Status.Phase != "Ready" {
		return nil, nil, nil, newDependencyNotReady("database %s is not ready (phase: %s)", backup.Spec.DatabaseRef.Name, db.Status.Phase)
	}

	// Get DBCluster
	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil, newDependencyNotReady("cluster %s not found", db.Spec.ClusterRef.Name)
		}
		return nil, nil, nil, err
	}

	if cluster.Status.Phase != "Connected" {
		return nil, nil, nil, newDependencyNotReady("cluster %s is not connected", db.Spec.ClusterRef.Name)
	}

	// Get BackupStorage
	var storage databasesv1alpha1.BackupStorage
	if err := r.Get(ctx, types.NamespacedName{Name: backup.Spec.StorageRef.Name}, &storage); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil, newDependencyNotReady("backup storage %s not found", backup.Spec.StorageRef.Name)
		}
		return nil, nil, nil, err
	}

	if storage.Status.Phase != "Ready" {
		return nil, nil, nil, newDependencyNotReady("backup storage %s is not ready (phase: %s)", backup.Spec.StorageRef.Name, storage.Status.Phase)
	}

	return &db, &cluster, &storage, nil
}

//nolint:funlen // Job creation requires many fields - splitting would reduce readability
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
	if r.SSLMode != "" {
		env = append(env, corev1.EnvVar{Name: "DB_SSLMODE", Value: r.SSLMode})
	}

	// Add DB credentials from cluster
	env = append(env, r.getClusterCredentialsEnv(cluster)...)

	// Add storage configuration
	env = append(env, r.getStorageEnv(storage)...)

	// Configure backoff limit (default: 3)
	backoffLimit := int32(3)
	if backup.Spec.JobConfig != nil && backup.Spec.JobConfig.BackoffLimit != nil {
		backoffLimit = *backup.Spec.JobConfig.BackoffLimit
	}

	// Use TTL from spec, or default to 1 hour
	var ttlSeconds int32 = 3600
	if backup.Spec.TTLAfterCompletion != nil {
		ttlSeconds = int32(backup.Spec.TTLAfterCompletion.Seconds())
	}

	// Configure active deadline (optional hard timeout)
	var activeDeadlineSeconds *int64
	if backup.Spec.JobConfig != nil && backup.Spec.JobConfig.ActiveDeadlineSeconds != nil {
		activeDeadlineSeconds = backup.Spec.JobConfig.ActiveDeadlineSeconds
	}

	// Required labels - needed for controller operation (job lookup, concurrency limits)
	requiredLabels := map[string]string{
		LabelBackupName:      backup.Name,
		LabelBackupNamespace: backup.Namespace,
		LabelCluster:         cluster.Name,
	}

	// Build job labels: required + Helm values (backup.jobLabels)
	jobLabels := make(map[string]string)
	for k, v := range requiredLabels {
		jobLabels[k] = v
	}
	for k, v := range r.JobLabels {
		jobLabels[k] = v
	}

	// Build pod labels: required + Helm values (backup.podLabels)
	podLabels := make(map[string]string)
	for k, v := range requiredLabels {
		podLabels[k] = v
	}
	for k, v := range r.PodLabels {
		podLabels[k] = v
	}

	// Pod annotations from Helm values (backup.podAnnotations)
	podAnnotations := make(map[string]string)
	for k, v := range r.PodAnnotations {
		podAnnotations[k] = v
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: r.Namespace,
			Labels:    jobLabels,
			// No OwnerReference - cross-namespace not allowed
			// Cleanup via finalizer on Backup + TTL as fallback
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSeconds,
			ActiveDeadlineSeconds:   activeDeadlineSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      podLabels,
					Annotations: podAnnotations,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyOnFailure,
					ServiceAccountName: "dbtether", // Uses operator's SA for IRSA
					Containers: []corev1.Container{
						{
							Name:      "backup",
							Image:     r.Image,
							Args:      []string{"--mode=job"},
							Env:       env,
							Resources: r.PodResources,
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

		env = append(env, storageCredentialsEnv(storage)...)
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
		LabelBackupName:      backup.Name,
		LabelBackupNamespace: backup.Namespace,
	}); err != nil {
		return ctrl.Result{}, err
	}

	if len(jobs.Items) == 0 {
		// Job may not be visible yet due to cache propagation right after creation.
		// If backup was started recently, requeue instead of failing immediately.
		if backup.Status.StartedAt != nil {
			age := time.Since(backup.Status.StartedAt.Time)
			if age < 90*time.Second {
				return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
			}
		}
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
		return r.updateStatusCompleted(ctx, backup, job, specHash)
	}

	if _, failed := r.isJobFailed(job); failed {
		// Update Job TTL for failed jobs if TTLSecondsAfterFailed is configured
		if err := r.updateFailedJobTTL(ctx, backup, job); err != nil {
			log.FromContext(ctx).Error(err, "failed to update TTL for failed job")
		}
		failureInfo := r.getJobFailureInfo(job)
		return r.updateStatusFailedWithInfo(ctx, backup, job, specHash, failureInfo)
	}

	// Still running - track pod name while pod exists
	if backup.Status.LastPodName == "" {
		if podName := r.getLastPodName(ctx, job); podName != "" {
			patch := client.MergeFrom(backup.DeepCopy())
			backup.Status.LastPodName = podName
			if err := r.Status().Patch(ctx, backup, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// JobFailureInfo contains structured information about a job failure
type JobFailureInfo struct {
	Reason         string // Machine-readable reason (e.g., BackoffLimitExceeded)
	Message        string // Human-readable message
	FailedAttempts int32  // Number of failed pod attempts
}

// isJobFailed checks if a Job has definitively failed by examining its conditions.
// Returns structured failure info and whether the job has failed.
func (r *BackupReconciler) isJobFailed(job *batchv1.Job) (string, bool) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			reason := "backup job failed"
			if c.Reason != "" {
				reason = fmt.Sprintf("backup job failed: %s", c.Reason)
			}
			if c.Message != "" {
				reason = fmt.Sprintf("%s - %s", reason, c.Message)
			}
			return reason, true
		}
	}
	return "", false
}

// getJobFailureInfo extracts detailed failure information from a failed job
func (r *BackupReconciler) getJobFailureInfo(job *batchv1.Job) *JobFailureInfo {
	info := &JobFailureInfo{
		FailedAttempts: job.Status.Failed,
	}

	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			info.Reason = c.Reason
			info.Message = c.Message
			break
		}
	}

	if info.Reason == "" {
		info.Reason = "Unknown"
	}
	if info.Message == "" {
		info.Message = "backup job failed without detailed message"
	}

	return info
}

// updateStatusFailedWithInfo updates the Backup status to Failed with structured failure info
// and emits a Kubernetes Event for visibility.
func (r *BackupReconciler) updateStatusFailedWithInfo(ctx context.Context, backup *databasesv1alpha1.Backup,
	job *batchv1.Job, specHash string, failureInfo *JobFailureInfo) (ctrl.Result, error) {

	logger := log.FromContext(ctx)

	patch := client.MergeFrom(backup.DeepCopy())

	// Core status fields
	backup.Status.Phase = "Failed"
	backup.Status.Message = fmt.Sprintf("backup job failed: %s", failureInfo.Reason)
	backup.Status.SpecHash = specHash
	backup.Status.ObservedGeneration = backup.Generation

	// Detailed failure info
	backup.Status.FailureReason = failureInfo.Reason
	backup.Status.FailureMessage = failureInfo.Message
	backup.Status.FailedAttempts = failureInfo.FailedAttempts

	// Try to get the last pod name for log retrieval hints
	if job != nil {
		backup.Status.LastPodName = r.getLastPodName(ctx, job)
	}

	now := metav1.Now()
	backup.Status.CompletedAt = &now

	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

	// Emit Kubernetes Event for visibility in kubectl describe and monitoring
	if r.Recorder != nil {
		eventMessage := fmt.Sprintf("Backup failed after %d attempt(s): %s - %s",
			failureInfo.FailedAttempts, failureInfo.Reason, failureInfo.Message)
		r.Recorder.Event(backup, corev1.EventTypeWarning, EventReasonBackupFailed, eventMessage)
	}

	logger.Info("backup failed",
		"reason", failureInfo.Reason,
		"message", failureInfo.Message,
		"failedAttempts", failureInfo.FailedAttempts,
		"lastPodName", backup.Status.LastPodName)

	return ctrl.Result{}, nil
}

// getLastPodName tries to find the name of the last pod that ran for this job
func (r *BackupReconciler) getLastPodName(ctx context.Context, job *batchv1.Job) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{
		"job-name": job.Name,
	}); err != nil {
		return ""
	}

	if len(pods.Items) == 0 {
		return ""
	}

	// Return the most recently created pod
	var lastPod *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if lastPod == nil || pod.CreationTimestamp.After(lastPod.CreationTimestamp.Time) {
			lastPod = pod
		}
	}

	if lastPod != nil {
		return lastPod.Name
	}
	return ""
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

	logger := log.FromContext(ctx)

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

	// Emit success event
	if r.Recorder != nil {
		eventMessage := fmt.Sprintf("Backup completed successfully: %s (%s)",
			backup.Status.Path, backup.Status.Size)
		r.Recorder.Event(backup, corev1.EventTypeNormal, EventReasonBackupCompleted, eventMessage)
	}

	logger.Info("backup completed",
		"path", backup.Status.Path,
		"size", backup.Status.Size,
		"duration", backup.Status.Duration)

	return ctrl.Result{}, nil
}

// updateFailedJobTTL updates the TTL of a failed job to allow longer retention for debugging
func (r *BackupReconciler) updateFailedJobTTL(ctx context.Context, backup *databasesv1alpha1.Backup, job *batchv1.Job) error {
	logger := log.FromContext(ctx)

	// Get TTL for failed jobs (default: 12 hours = 43200 seconds)
	var ttlAfterFailed int32 = 43200
	if backup.Spec.JobConfig != nil && backup.Spec.JobConfig.TTLSecondsAfterFailed != nil {
		ttlAfterFailed = *backup.Spec.JobConfig.TTLSecondsAfterFailed
	}

	// Check if TTL needs updating
	currentTTL := job.Spec.TTLSecondsAfterFinished
	if currentTTL != nil && *currentTTL == ttlAfterFailed {
		// Already updated, skip
		return nil
	}

	logger.Info("updating TTL for failed job", "oldTTL", currentTTL, "newTTL", ttlAfterFailed)

	// Update Job TTL
	patch := client.MergeFrom(job.DeepCopy())
	job.Spec.TTLSecondsAfterFinished = &ttlAfterFailed
	if err := r.Patch(ctx, job, patch); err != nil {
		return fmt.Errorf("failed to patch job TTL: %w", err)
	}

	return nil
}

func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.Backup{}).
		// Jobs don't have OwnerReference (cross-namespace), so we watch by labels
		Watches(&batchv1.Job{}, r.jobEventHandler()).
		Complete(r)
}

func (r *BackupReconciler) jobEventHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		job, ok := obj.(*batchv1.Job)
		if !ok || job.Namespace != r.Namespace {
			return nil
		}

		backupName := job.Labels[LabelBackupName]
		backupNamespace := job.Labels[LabelBackupNamespace]
		if backupName == "" || backupNamespace == "" {
			return nil
		}

		return []ctrl.Request{{
			NamespacedName: types.NamespacedName{
				Name:      backupName,
				Namespace: backupNamespace,
			},
		}}
	})
}

// dependencyNotReadyError distinguishes a missing/not-ready reference from other
// errors: under GitOps the referencing object can reconcile before its
// dependencies exist, so this class of error must requeue rather than fail terminally.
type dependencyNotReadyError struct {
	msg string
}

func (e *dependencyNotReadyError) Error() string { return e.msg }

func newDependencyNotReady(format string, args ...any) error {
	return &dependencyNotReadyError{msg: fmt.Sprintf(format, args...)}
}

func isDependencyNotReady(err error) bool {
	var d *dependencyNotReadyError
	return stderrors.As(err, &d)
}

// storageCredentialsEnv is shared by backup and restore jobs; returns nil when
// unset so IRSA/Pod Identity applies instead.
func storageCredentialsEnv(storage *databasesv1alpha1.BackupStorage) []corev1.EnvVar {
	if storage.Spec.CredentialsSecretRef == nil {
		return nil
	}
	return []corev1.EnvVar{
		{
			Name: "AWS_ACCESS_KEY_ID",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: storage.Spec.CredentialsSecretRef.Name},
					Key:                  "AWS_ACCESS_KEY_ID",
				},
			},
		},
		{
			Name: "AWS_SECRET_ACCESS_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: storage.Spec.CredentialsSecretRef.Name},
					Key:                  "AWS_SECRET_ACCESS_KEY",
				},
			},
		},
	}
}
