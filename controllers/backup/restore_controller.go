package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"
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

const restoreFinalizer = "dbtether.io/restore-job"

// Label keys for restore resources
const (
	LabelRestoreName      = "dbtether.io/restore"
	LabelRestoreNamespace = "dbtether.io/restore-namespace"
)

type RestoreReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Image     string
	Namespace string
}

// +kubebuilder:rbac:groups=dbtether.io,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=restores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=restores/finalizers,verbs=update

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var restore databasesv1alpha1.Restore
	if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Handle deletion - cleanup Job via finalizer
	if !restore.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &restore)
	}

	// Ensure finalizer
	if result, done := r.ensureFinalizer(ctx, &restore); done {
		return result, nil
	}

	specHash := r.computeSpecHash(&restore)

	// Skip if already processed
	if r.isAlreadyProcessed(&restore, specHash, logger) {
		return ctrl.Result{}, nil
	}

	logger.V(1).Info("reconciling restore", "target", restore.Spec.Target.DatabaseRef.Name)

	// If Job already created, just check its status
	if restore.Status.JobName != "" {
		return r.checkJobStatus(ctx, &restore, specHash)
	}

	return r.createRestoreJob(ctx, &restore, specHash, logger)
}

func (r *RestoreReconciler) ensureFinalizer(ctx context.Context, restore *databasesv1alpha1.Restore) (ctrl.Result, bool) {
	if controllerutil.ContainsFinalizer(restore, restoreFinalizer) {
		return ctrl.Result{}, false
	}
	controllerutil.AddFinalizer(restore, restoreFinalizer)
	if err := r.Update(ctx, restore); err != nil {
		return ctrl.Result{}, true
	}
	return ctrl.Result{Requeue: true}, true
}

func (r *RestoreReconciler) isAlreadyProcessed(restore *databasesv1alpha1.Restore, specHash string, logger logr.Logger) bool {
	if restore.Status.Phase == "" || restore.Status.SpecHash != specHash {
		return false
	}
	if restore.Status.Phase == "Completed" || restore.Status.Phase == "Failed" {
		logger.V(1).Info("restore already processed", "phase", restore.Status.Phase)
		return true
	}
	return false
}

func (r *RestoreReconciler) createRestoreJob(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	specHash string,
	logger logr.Logger,
) (ctrl.Result, error) {
	// Resolve source path
	sourcePath, storageRef, err := r.resolveSource(ctx, restore)
	if err != nil {
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf("failed to resolve source: %v", err), specHash)
	}

	// Get target database
	var db databasesv1alpha1.Database
	if err := r.Get(ctx, types.NamespacedName{
		Name:      restore.Spec.Target.DatabaseRef.Name,
		Namespace: restore.Namespace,
	}, &db); err != nil {
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf("target database not found: %v", err), specHash)
	}

	// Get DBCluster
	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf("cluster not found: %v", err), specHash)
	}

	// Get BackupStorage
	var storage databasesv1alpha1.BackupStorage
	if err := r.Get(ctx, types.NamespacedName{Name: storageRef}, &storage); err != nil {
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf("backup storage not found: %v", err), specHash)
	}

	// Generate RunID
	runID := generateRunID()

	// Create restore job (in operator namespace, like backup jobs)
	job, err := r.buildRestoreJob(restore, &db, &cluster, &storage, sourcePath, runID)
	if err != nil {
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf("failed to build job: %v", err), specHash)
	}

	// Note: No owner reference set because Job runs in operator namespace,
	// while Restore CRD is in user namespace. Cleanup handled by TTL and finalizer.

	if err := r.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			logger.V(1).Info("restore job already exists", "job", job.Name)
		} else {
			return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf("failed to create job: %v", err), specHash)
		}
	}

	logger.Info("restore job created", "job", job.Name, "source", sourcePath)

	return r.updateStatusWithJob(ctx, restore, "Running", "restore job started", specHash, job.Name, runID, sourcePath)
}

func (r *RestoreReconciler) resolveSource(ctx context.Context, restore *databasesv1alpha1.Restore) (sourcePath, storageRefName string, err error) {
	source := restore.Spec.Source

	// Option 1: BackupRef - get path from existing Backup
	if source.BackupRef != nil {
		return r.resolveFromBackupRef(ctx, restore, source.BackupRef)
	}

	// Option 2: LatestFrom - find latest successful backup for a database
	if source.LatestFrom != nil {
		return r.resolveFromLatest(ctx, restore, source.LatestFrom)
	}

	// Option 3: Direct path
	if source.Path != "" {
		if source.StorageRef == nil {
			return "", "", fmt.Errorf("storageRef is required when using path")
		}
		return source.Path, source.StorageRef.Name, nil
	}

	return "", "", fmt.Errorf("either backupRef, latestFrom, or path must be specified")
}

func (r *RestoreReconciler) resolveFromBackupRef(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	ref *databasesv1alpha1.BackupReference,
) (sourcePath, storageRefName string, err error) {
	ns := ref.Namespace
	if ns == "" {
		ns = restore.Namespace
	}

	var backup databasesv1alpha1.Backup
	if err := r.Get(ctx, types.NamespacedName{
		Name:      ref.Name,
		Namespace: ns,
	}, &backup); err != nil {
		return "", "", fmt.Errorf("backup not found: %w", err)
	}

	if backup.Status.Phase != "Completed" {
		return "", "", fmt.Errorf("backup is not completed (phase: %s)", backup.Status.Phase)
	}

	if backup.Status.Path == "" {
		return "", "", fmt.Errorf("backup has no path in status")
	}

	return backup.Status.Path, backup.Spec.StorageRef.Name, nil
}

func (r *RestoreReconciler) resolveFromLatest(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	latestFrom *databasesv1alpha1.LatestFromSource,
) (sourcePath, storageRefName string, err error) {
	ns := latestFrom.Namespace
	if ns == "" {
		ns = restore.Namespace
	}

	// List all backups in the namespace
	var backupList databasesv1alpha1.BackupList
	if err := r.List(ctx, &backupList, client.InNamespace(ns)); err != nil {
		return "", "", fmt.Errorf("failed to list backups: %w", err)
	}

	// Filter by database and find the latest completed one
	var latestBackup *databasesv1alpha1.Backup
	var latestTime *metav1.Time

	for i := range backupList.Items {
		backup := &backupList.Items[i]

		// Skip if not for this database
		if backup.Spec.DatabaseRef.Name != latestFrom.DatabaseRef.Name {
			continue
		}

		// Skip if not completed
		if backup.Status.Phase != "Completed" {
			continue
		}

		// Skip if no path
		if backup.Status.Path == "" {
			continue
		}

		// Check if this is the latest
		if backup.Status.CompletedAt == nil {
			continue
		}

		if latestTime == nil || backup.Status.CompletedAt.After(latestTime.Time) {
			latestBackup = backup
			latestTime = backup.Status.CompletedAt
		}
	}

	if latestBackup == nil {
		return "", "", fmt.Errorf("no completed backup found for database %s", latestFrom.DatabaseRef.Name)
	}

	return latestBackup.Status.Path, latestBackup.Spec.StorageRef.Name, nil
}

func (r *RestoreReconciler) buildRestoreJob(
	restore *databasesv1alpha1.Restore,
	db *databasesv1alpha1.Database,
	cluster *databasesv1alpha1.DBCluster,
	storage *databasesv1alpha1.BackupStorage,
	sourcePath string,
	runID string,
) (*batchv1.Job, error) {
	jobName := fmt.Sprintf("restore-%s-%s", restore.Name, runID)

	// Build environment variables
	env := r.buildEnvVars(db, cluster, storage, sourcePath, restore.Spec.OnConflict)

	backoffLimit := int32(0)
	ttlSeconds := int32(3600) // 1 hour

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: r.Namespace,
			Labels: map[string]string{
				LabelRestoreName:      restore.Name,
				LabelRestoreNamespace: restore.Namespace,
				LabelCluster:          cluster.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						LabelRestoreName:      restore.Name,
						LabelRestoreNamespace: restore.Namespace,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: "dbtether", // Uses operator's SA for IRSA
					Containers: []corev1.Container{
						{
							Name:  "restore",
							Image: r.Image,
							Args:  []string{"--mode=restore"},
							Env:   env,
						},
					},
				},
			},
		},
	}

	return job, nil
}

func (r *RestoreReconciler) buildEnvVars(
	db *databasesv1alpha1.Database,
	cluster *databasesv1alpha1.DBCluster,
	storage *databasesv1alpha1.BackupStorage,
	sourcePath string,
	onConflict string,
) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "DB_HOST", Value: cluster.Spec.Endpoint},
		{Name: "DB_PORT", Value: fmt.Sprintf("%d", cluster.Spec.Port)},
		{Name: "DB_NAME", Value: db.Status.DatabaseName},
		{Name: "SOURCE_PATH", Value: sourcePath},
		{Name: "ON_CONFLICT", Value: onConflict},
	}

	// Add credentials from secret
	if cluster.Spec.CredentialsSecretRef != nil {
		env = append(env,
			corev1.EnvVar{
				Name: "DB_USER",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: cluster.Spec.CredentialsSecretRef.Name,
						},
						Key: "username",
					},
				},
			},
			corev1.EnvVar{
				Name: "DB_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{
					SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: cluster.Spec.CredentialsSecretRef.Name,
						},
						Key: "password",
					},
				},
			},
		)
	}

	// Add storage config
	if storage.Spec.S3 != nil {
		env = append(env,
			corev1.EnvVar{Name: "STORAGE_TYPE", Value: "s3"},
			corev1.EnvVar{Name: "S3_BUCKET", Value: storage.Spec.S3.Bucket},
			corev1.EnvVar{Name: "S3_REGION", Value: storage.Spec.S3.Region},
		)
		if storage.Spec.S3.Endpoint != "" {
			env = append(env, corev1.EnvVar{Name: "S3_ENDPOINT", Value: storage.Spec.S3.Endpoint})
		}
	}

	if storage.Spec.GCS != nil {
		env = append(env,
			corev1.EnvVar{Name: "STORAGE_TYPE", Value: "gcs"},
			corev1.EnvVar{Name: "GCS_BUCKET", Value: storage.Spec.GCS.Bucket},
		)
		if storage.Spec.GCS.Project != "" {
			env = append(env, corev1.EnvVar{Name: "GCS_PROJECT", Value: storage.Spec.GCS.Project})
		}
	}

	if storage.Spec.Azure != nil {
		env = append(env,
			corev1.EnvVar{Name: "STORAGE_TYPE", Value: "azure"},
			corev1.EnvVar{Name: "AZURE_CONTAINER", Value: storage.Spec.Azure.Container},
			corev1.EnvVar{Name: "AZURE_ACCOUNT", Value: storage.Spec.Azure.StorageAccount},
		)
	}

	return env
}

func (r *RestoreReconciler) checkJobStatus(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string) (ctrl.Result, error) {
	var job batchv1.Job
	if err := r.Get(ctx, types.NamespacedName{
		Name:      restore.Status.JobName,
		Namespace: r.Namespace,
	}, &job); err != nil {
		if errors.IsNotFound(err) {
			// Job deleted externally
			return r.updateStatus(ctx, restore, "Failed", "restore job was deleted", specHash)
		}
		return ctrl.Result{}, err
	}

	return r.evaluateJobStatus(ctx, restore, &job, specHash)
}

func (r *RestoreReconciler) evaluateJobStatus(ctx context.Context, restore *databasesv1alpha1.Restore, job *batchv1.Job, specHash string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if job.Status.Succeeded > 0 {
		duration := ""
		if restore.Status.StartedAt != nil {
			duration = time.Since(restore.Status.StartedAt.Time).Round(time.Second).String()
		}
		logger.Info("restore completed successfully", "duration", duration)
		return r.updateStatusCompleted(ctx, restore, specHash, duration)
	}

	if job.Status.Failed > 0 {
		message := "restore job failed"
		// Try to get failure reason from pod
		if reason := r.getJobFailureReason(ctx, job); reason != "" {
			message = reason
		}
		logger.Error(nil, "restore failed", "reason", message)
		return r.updateStatus(ctx, restore, "Failed", message, specHash)
	}

	// Still running
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *RestoreReconciler) getJobFailureReason(ctx context.Context, job *batchv1.Job) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{
		"job-name": job.Name,
	}); err != nil {
		return ""
	}

	for i := range pods.Items {
		for j := range pods.Items[i].Status.ContainerStatuses {
			cs := &pods.Items[i].Status.ContainerStatuses[j]
			if cs.State.Terminated != nil && cs.State.Terminated.Reason != "" {
				return cs.State.Terminated.Reason
			}
		}
	}
	return ""
}

func (r *RestoreReconciler) handleDeletion(ctx context.Context, restore *databasesv1alpha1.Restore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(restore, restoreFinalizer) {
		return ctrl.Result{}, nil
	}

	// Delete the job if it exists
	if restore.Status.JobName != "" {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:      restore.Status.JobName,
				Namespace: r.Namespace,
			},
		}
		propagation := metav1.DeletePropagationBackground
		if err := r.Delete(ctx, job, &client.DeleteOptions{
			PropagationPolicy: &propagation,
		}); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		logger.Info("restore deleted, job cleaned up", "job", restore.Status.JobName)
	}

	controllerutil.RemoveFinalizer(restore, restoreFinalizer)
	if err := r.Update(ctx, restore); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *RestoreReconciler) updateStatus(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	phase, message, specHash string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(restore.DeepCopy())

	restore.Status.Phase = phase
	restore.Status.Message = message
	restore.Status.SpecHash = specHash
	restore.Status.ObservedGeneration = restore.Generation

	if phase == "Running" && restore.Status.StartedAt == nil {
		now := metav1.Now()
		restore.Status.StartedAt = &now
	}

	if err := r.Status().Patch(ctx, restore, patch); err != nil {
		return ctrl.Result{}, err
	}

	if phase == "Failed" {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *RestoreReconciler) updateStatusWithJob(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	phase, message, specHash, jobName, runID, sourcePath string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(restore.DeepCopy())

	restore.Status.Phase = phase
	restore.Status.Message = message
	restore.Status.SpecHash = specHash
	restore.Status.JobName = jobName
	restore.Status.RunID = runID
	restore.Status.SourcePath = sourcePath
	restore.Status.ObservedGeneration = restore.Generation

	now := metav1.Now()
	restore.Status.StartedAt = &now

	if err := r.Status().Patch(ctx, restore, patch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *RestoreReconciler) updateStatusCompleted(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	specHash, duration string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(restore.DeepCopy())

	restore.Status.Phase = "Completed"
	restore.Status.Message = "restore completed successfully"
	restore.Status.SpecHash = specHash
	restore.Status.Duration = duration
	restore.Status.ObservedGeneration = restore.Generation

	now := metav1.Now()
	restore.Status.CompletedAt = &now

	if err := r.Status().Patch(ctx, restore, patch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *RestoreReconciler) computeSpecHash(restore *databasesv1alpha1.Restore) string {
	data, _ := json.Marshal(restore.Spec)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:8])
}

func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.Restore{}).
		Owns(&batchv1.Job{}).
		Complete(r)
}
