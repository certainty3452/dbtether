package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"
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
	"github.com/certainty3452/dbtether/controllers"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

const restoreFinalizer = "dbtether.io/restore-job"

// regrantTimeout bounds the grant re-apply step, which is one sequential set of
// PG round-trips per DatabaseUser of the target database.
const regrantTimeout = 5 * time.Minute

// grantAttemptLimit caps the rounds spent on DatabaseUsers whose grants keep
// failing (e.g. additionalGrants naming a table an older dump never had), so one
// permanently broken user cannot pin a finished restore in Granting forever.
const grantAttemptLimit = 5

// grantRetryDelay paces regrant rounds with a fixed delay: workqueue backoff
// starts at ~5ms and would burn the whole attempt budget in under a second on
// a transient outage (e.g. a PG restart) that a later round would survive.
const grantRetryDelay = 30 * time.Second

// Label keys for restore resources
const (
	LabelRestoreName      = "dbtether.io/restore"
	LabelRestoreNamespace = "dbtether.io/restore-namespace"
)

// Event reason constants for restore operations
const EventReasonRestoreGrantsSkipped = "RestoreGrantsSkipped"

type RestoreReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	Recorder      record.EventRecorder
	Image         string
	Namespace     string
	SSLMode       string // propagated into Job env so it matches operator's posture
	PodResources  corev1.ResourceRequirements
	PGClientCache postgres.ClientCacheInterface
}

// +kubebuilder:rbac:groups=dbtether.io,resources=restores,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=restores/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=restores/finalizers,verbs=update
// +kubebuilder:rbac:groups=dbtether.io,resources=databases,verbs=get;list;watch
// +kubebuilder:rbac:groups=dbtether.io,resources=databaseusers,verbs=get;list;watch
// +kubebuilder:rbac:groups=dbtether.io,resources=dbclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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
	if result, done, err := r.ensureFinalizer(ctx, &restore); done {
		return result, err
	}

	specHash := r.computeSpecHash(&restore)

	// Skip if already processed
	if r.isAlreadyProcessed(&restore, specHash, logger) {
		return ctrl.Result{}, nil
	}

	logger.V(1).Info("reconciling restore", "target", restore.Spec.Target.DatabaseRef.Name)

	// The data already landed, so the Job is irrelevant from here on — and its TTL
	// may well have collected it. Looking it up would report NotFound and bury a
	// successful restore under a "job was deleted" failure.
	if restore.Status.Phase == "Granting" && restore.Status.SpecHash == specHash {
		return r.finishGranting(ctx, &restore, specHash)
	}

	// If Job already created, just check its status
	if restore.Status.JobName != "" {
		return r.checkJobStatus(ctx, &restore, specHash)
	}

	return r.createRestoreJob(ctx, &restore, specHash, logger)
}

func (r *RestoreReconciler) ensureFinalizer(ctx context.Context, restore *databasesv1alpha1.Restore) (result ctrl.Result, done bool, err error) {
	if controllerutil.ContainsFinalizer(restore, restoreFinalizer) {
		return ctrl.Result{}, false, nil
	}
	controllerutil.AddFinalizer(restore, restoreFinalizer)
	if err := r.Update(ctx, restore); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: finalizerRequeueDelay}, true, nil
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
		if isDependencyNotReady(err) {
			return r.updateStatus(ctx, restore, "Pending", fmt.Sprintf("waiting for source: %v", err), specHash)
		}
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf("failed to resolve source: %v", err), specHash)
	}

	// Adopt an already-created job: the job name embeds a fresh runID, so a status
	// patch that fails after Create would otherwise spawn a second concurrent pg_restore.
	existingJob, err := r.findExistingJob(ctx, restore)
	if err != nil {
		return ctrl.Result{}, err
	}
	if existingJob != nil {
		logger.V(1).Info("restore job already exists, using existing", "job", existingJob.Name)
		runID := r.extractRunIDFromJobName(existingJob.Name, restore.Name)
		return r.updateStatusWithJob(ctx, restore, "Running", "restore job running", specHash, existingJob.Name, runID, sourcePath)
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

	switch backup.Status.Phase {
	case "", "Pending", "Running":
		return "", "", newDependencyNotReady("backup %s is not completed yet (phase: %s)", ref.Name, backup.Status.Phase)
	case "Failed":
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
	ttlSeconds := int32(3600)
	if restore.Spec.TTLAfterCompletion != nil {
		ttlSeconds = int32(restore.Spec.TTLAfterCompletion.Seconds())
	}

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
							Name:      "restore",
							Image:     r.Image,
							Args:      []string{"--mode=restore"},
							Env:       env,
							Resources: r.PodResources,
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
	if r.SSLMode != "" {
		env = append(env, corev1.EnvVar{Name: "DB_SSLMODE", Value: r.SSLMode})
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

		env = append(env, storageCredentialsEnv(storage)...)
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
		// Completed gates downstream automation, so it must mean "usable by the
		// database's registered users" — hold the phase until grants are back.
		// Persisting Granting first is what lets a retry outlive the Job's TTL.
		if _, err := r.updateStatus(ctx, restore, "Granting", "restore succeeded, applying grants", specHash); err != nil {
			return ctrl.Result{}, err
		}
		return r.finishGranting(ctx, restore, specHash)
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

	// Remove finalizer using Patch to avoid optimistic locking conflicts
	patch := client.MergeFrom(restore.DeepCopy())
	controllerutil.RemoveFinalizer(restore, restoreFinalizer)
	if err := r.Patch(ctx, restore, patch); err != nil {
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

	// Granting is only entered from a fresh Job success, so the grant attempt
	// budget starts over here rather than carrying over from an earlier run.
	if phase == "Granting" {
		restore.Status.GrantAttempts = 0
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
	specHash, message string,
) (ctrl.Result, error) {
	patch := client.MergeFrom(restore.DeepCopy())

	restore.Status.Phase = "Completed"
	restore.Status.Message = message
	restore.Status.SpecHash = specHash
	restore.Status.Duration = restoreDuration(restore)
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
		// Jobs don't have OwnerReference (cross-namespace), so we watch by labels
		Watches(&batchv1.Job{}, r.jobEventHandler()).
		Complete(r)
}

func (r *RestoreReconciler) jobEventHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		job, ok := obj.(*batchv1.Job)
		if !ok || job.Namespace != r.Namespace {
			return nil
		}

		restoreName := job.Labels[LabelRestoreName]
		restoreNamespace := job.Labels[LabelRestoreNamespace]
		if restoreName == "" || restoreNamespace == "" {
			return nil
		}

		return []ctrl.Request{{
			NamespacedName: types.NamespacedName{
				Name:      restoreName,
				Namespace: restoreNamespace,
			},
		}}
	})
}

func (r *RestoreReconciler) findExistingJob(ctx context.Context, restore *databasesv1alpha1.Restore) (*batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.Namespace), client.MatchingLabels{
		LabelRestoreName:      restore.Name,
		LabelRestoreNamespace: restore.Namespace,
	}); err != nil {
		return nil, err
	}

	if len(jobs.Items) == 0 {
		return nil, nil
	}

	return &jobs.Items[0], nil
}

func (r *RestoreReconciler) extractRunIDFromJobName(jobName, restoreName string) string {
	prefix := fmt.Sprintf("restore-%s-", restoreName)
	if len(jobName) > len(prefix) {
		return jobName[len(prefix):]
	}
	return ""
}

func (r *RestoreReconciler) finishGranting(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	grantCtx, cancel := context.WithTimeout(ctx, regrantTimeout)
	defer cancel()

	failures, err := r.regrantDatabaseUsers(grantCtx, restore)
	if err != nil {
		// There is nothing left to grant on, so retrying can only spin — and the data
		// itself did land, which is what Completed reports.
		if reason, ok := unrecoverableRegrantReason(err); ok {
			logger.Error(err, "completing restore without re-applying grants", "reason", reason)
			r.recordGrantsSkipped(restore, reason)
			return r.updateStatusCompleted(ctx, restore, specHash,
				fmt.Sprintf("restore completed; grants not re-applied: %s", reason))
		}

		// The whole regrant step never got off the ground, so no attempt is charged.
		logger.Error(err, "failed to re-apply grants after restore")
		if statusErr := r.patchGrantingProgress(ctx, restore, restore.Status.GrantAttempts,
			fmt.Sprintf("restore succeeded, retrying grants: %v", err)); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}

	if len(failures) > 0 {
		return r.handleUserGrantFailures(ctx, restore, specHash, failures)
	}

	logger.Info("restore completed successfully", "duration", restoreDuration(restore))
	return r.updateStatusCompleted(ctx, restore, specHash, "restore completed successfully")
}

// handleUserGrantFailures retries the full user set — ApplyPrivileges is idempotent,
// so users that already succeeded are cheap to redo — until the attempt budget is
// spent, then completes with the offenders named.
func (r *RestoreReconciler) handleUserGrantFailures(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	specHash string,
	failures []userGrantFailure,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	attempts := restore.Status.GrantAttempts + 1
	err := joinGrantFailures(failures)
	users := failedGrantUsers(failures)

	if attempts >= grantAttemptLimit {
		logger.Error(err, "giving up on re-applying grants after restore", "attempts", attempts, "users", users)
		r.recordGrantsSkipped(restore, fmt.Sprintf("retries exhausted for %s", users))
		return r.updateStatusCompleted(ctx, restore, specHash,
			fmt.Sprintf("restore completed; grants not re-applied for: %s", users))
	}

	logger.Error(err, "failed to re-apply grants for some users after restore", "attempts", attempts, "users", users)
	if statusErr := r.patchGrantingProgress(ctx, restore, attempts,
		fmt.Sprintf("restore succeeded, retrying grants: %v", err)); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	// nil error: a real error would drive workqueue backoff, defeating grantRetryDelay above.
	return ctrl.Result{RequeueAfter: grantRetryDelay}, nil
}

// patchGrantingProgress keeps the phase at Granting while grants are retried and
// persists the attempt budget so it survives operator restarts. It stays silent when
// nothing changed so a wedged cluster can't churn the API server.
func (r *RestoreReconciler) patchGrantingProgress(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	attempts int32,
	message string,
) error {
	if restore.Status.Phase == "Granting" && restore.Status.Message == message && restore.Status.GrantAttempts == attempts {
		return nil
	}

	patch := client.MergeFrom(restore.DeepCopy())
	restore.Status.Phase = "Granting"
	restore.Status.Message = message
	restore.Status.GrantAttempts = attempts

	return r.Status().Patch(ctx, restore, patch)
}

func (r *RestoreReconciler) recordGrantsSkipped(restore *databasesv1alpha1.Restore, reason string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(restore, corev1.EventTypeWarning, EventReasonRestoreGrantsSkipped,
		fmt.Sprintf("Restore completed but grants were not re-applied: %s", reason))
}

// pg_restore imports as the cluster admin with --no-owner --no-acl, and
// onConflict=drop recreates the database — both wipe existing grants.
// A returned error means the step could not run at all; per-user failures come
// back in the slice instead, so one unservable user does not starve the rest.
func (r *RestoreReconciler) regrantDatabaseUsers(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
) (failures []userGrantFailure, err error) {
	logger := log.FromContext(ctx)

	var db databasesv1alpha1.Database
	if err := r.Get(ctx, types.NamespacedName{
		Name:      restore.Spec.Target.DatabaseRef.Name,
		Namespace: restore.Namespace,
	}, &db); err != nil {
		if errors.IsNotFound(err) {
			return nil, newUnrecoverableRegrant(err, "target database %s no longer exists", restore.Spec.Target.DatabaseRef.Name)
		}
		return nil, fmt.Errorf("failed to get target database %s: %w", restore.Spec.Target.DatabaseRef.Name, err)
	}

	var users databasesv1alpha1.DatabaseUserList
	if err := r.List(ctx, &users, client.MatchingFields{
		controllers.DatabaseUserDatabaseRefIndex: controllers.DatabaseUserDatabaseRefKey(db.Namespace, db.Name),
	}); err != nil {
		return nil, fmt.Errorf("failed to list database users: %w", err)
	}
	if len(users.Items) == 0 {
		return nil, nil
	}

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			return nil, newUnrecoverableRegrant(err, "cluster %s no longer exists", db.Spec.ClusterRef.Name)
		}
		return nil, fmt.Errorf("failed to get cluster %s: %w", db.Spec.ClusterRef.Name, err)
	}

	pgClient, err := controllers.GetPostgresClient(ctx, r.Client, r.PGClientCache, &cluster)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to cluster %s: %w", cluster.Name, err)
	}

	for i := range users.Items {
		user := &users.Items[i]

		grants, ok := controllers.ResolveUserGrantsForDatabase(user, db.Namespace, db.Name)
		if !ok {
			continue
		}

		// The role decides, not the CR phase: a Failed user can still own a live
		// role whose grants the restore just wiped, and a freshly Ready one may not
		// be visible in this controller's cache yet.
		exists, err := pgClient.UserExists(ctx, grants.Username)
		if err != nil {
			failures = append(failures, userGrantFailure{
				username: grants.Username,
				err:      fmt.Errorf("failed to check role %s: %w", grants.Username, err),
			})
			continue
		}
		if !exists {
			logger.Info("role not yet created; user's own reconcile will grant",
				"user", user.Name, "namespace", user.Namespace, "role", grants.Username)
			continue
		}

		if err := pgClient.ApplyPrivileges(ctx, grants.Username, db.Status.DatabaseName, grants.Privileges, grants.AdditionalGrants); err != nil {
			failures = append(failures, userGrantFailure{
				username: grants.Username,
				err:      fmt.Errorf("failed to re-apply privileges for user %s: %w", grants.Username, err),
			})
			continue
		}
		logger.Info("re-applied grants after restore",
			"user", grants.Username, "database", db.Status.DatabaseName, "privileges", grants.Privileges)
	}

	return failures, nil
}

type userGrantFailure struct {
	username string
	err      error
}

// joinGrantFailures flattens the round's failures onto one line, because the result
// ends up in status.message where newlines are unreadable.
func joinGrantFailures(failures []userGrantFailure) error {
	messages := make([]string, 0, len(failures))
	for _, failure := range failures {
		messages = append(messages, failure.err.Error())
	}
	return stderrors.New(strings.Join(messages, "; "))
}

func failedGrantUsers(failures []userGrantFailure) string {
	names := make([]string, 0, len(failures))
	for _, failure := range failures {
		names = append(names, failure.username)
	}
	return strings.Join(names, ", ")
}

// unrecoverableRegrantError marks a regrant failure that no amount of retrying can
// fix, so the restore completes with the reason recorded instead of spinning.
type unrecoverableRegrantError struct {
	reason string
	err    error
}

func (e *unrecoverableRegrantError) Error() string { return e.reason + ": " + e.err.Error() }
func (e *unrecoverableRegrantError) Unwrap() error { return e.err }

func newUnrecoverableRegrant(err error, format string, args ...any) error {
	return &unrecoverableRegrantError{reason: fmt.Sprintf(format, args...), err: err}
}

func unrecoverableRegrantReason(err error) (string, bool) {
	var u *unrecoverableRegrantError
	if stderrors.As(err, &u) {
		return u.reason, true
	}
	return "", false
}

func restoreDuration(restore *databasesv1alpha1.Restore) string {
	if restore.Status.StartedAt == nil {
		return ""
	}
	return time.Since(restore.Status.StartedAt.Time).Round(time.Second).String()
}
