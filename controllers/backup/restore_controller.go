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

// One sequential set of PG round-trips per DatabaseUser of the target database.
const regrantTimeout = 5 * time.Minute

// Keeps one permanently broken user (e.g. additionalGrants naming a table an old dump lacked) from pinning Granting.
const grantAttemptLimit = 5

// Fixed delay: workqueue backoff (~5ms start) would burn the attempt budget on a brief PG outage.
const grantRetryDelay = 30 * time.Second

const (
	LabelRestoreName      = "dbtether.io/restore"
	LabelRestoreNamespace = "dbtether.io/restore-namespace"

	AnnotationRestoreName = "dbtether.io/restore-name"
)

const EventReasonRestoreGrantsSkipped = "RestoreGrantsSkipped"

type RestoreReconciler struct {
	client.Client
	APIReader          client.Reader
	Scheme             *runtime.Scheme
	Recorder           record.EventRecorder
	Image              string
	Namespace          string
	ServiceAccountName string
	SSLMode            string
	PodResources       corev1.ResourceRequirements
	PGClientCache      postgres.ClientCacheInterface

	MaxConcurrentJobs int
}

func (r *RestoreReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var restore databasesv1alpha1.Restore
	if err := r.Get(ctx, req.NamespacedName, &restore); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !restore.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &restore)
	}

	if result, done, err := r.ensureFinalizer(ctx, &restore); done {
		return result, err
	}

	if restore.Status.Phase == "Completed" || restore.Status.Phase == "Failed" {
		return ctrl.Result{}, nil
	}

	logger.V(1).Info("reconciling restore", "target", restore.Spec.Target.DatabaseRef.Name)

	result, err := r.reconcileRun(ctx, &restore, r.computeSpecHash(&restore), logger)
	if errors.IsConflict(err) {
		logger.V(1).Info("restore status changed since it was read, requeueing", "reason", err.Error())
		return ctrl.Result{RequeueAfter: rerunRequeueDelay}, nil
	}
	return result, err
}

// A Restore runs at most one Job ever: a reservation is replaced only while its Job provably never existed.
func (r *RestoreReconciler) reconcileRun(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string, logger logr.Logger) (ctrl.Result, error) {
	switch {
	case restore.Status.JobName != "" && restore.Status.SpecHash != "" && restore.Status.SpecHash != specHash:
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf(
			"spec changed after restore job %s started; that run is no longer tracked, create a new Restore", restore.Status.JobName),
			restore.Status.SpecHash)
	case restore.Status.Phase == "Granting":
		return r.finishGranting(ctx, restore, specHash)
	case restore.Status.JobSucceededAt != nil:
		return r.enterGranting(ctx, restore, specHash)
	case restore.Status.JobName != "":
		return r.continueRun(ctx, restore, specHash, logger)
	default:
		return r.startRestore(ctx, restore, specHash, logger)
	}
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

type restoreInputs struct {
	sourcePath string
	db         *databasesv1alpha1.Database
	cluster    *databasesv1alpha1.DBCluster
	storage    *databasesv1alpha1.BackupStorage
}

func (r *RestoreReconciler) startRestore(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string, logger logr.Logger) (ctrl.Result, error) {
	inputs, result, err := r.resolveInputs(ctx, restore, specHash)
	if inputs == nil {
		return result, err
	}
	throttled, err := clusterThrottleMessage(ctx, r.Client, r.Namespace, inputs.cluster.Name, r.MaxConcurrentJobs, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	if throttled != "" {
		return r.waitForSlot(ctx, restore, specHash, throttled)
	}
	return r.reserveAndCreate(ctx, restore, specHash, inputs, logger)
}

func (r *RestoreReconciler) resolveInputs(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string) (*restoreInputs, ctrl.Result, error) {
	sourcePath, storageRef, err := r.resolveSource(ctx, restore)
	if err != nil {
		result, err := r.handleResolveError(ctx, restore, specHash, fmt.Errorf("source: %w", err))
		return nil, result, err
	}

	db, cluster, storage, err := r.getResources(ctx, restore, storageRef)
	if err != nil {
		result, err := r.handleResolveError(ctx, restore, specHash, err)
		return nil, result, err
	}

	if msg := jobCredentialsProblem(cluster, storage, r.Namespace, "restore"); msg != "" {
		result, err := r.updateStatus(ctx, restore, "Pending", msg, specHash)
		return nil, result, err
	}

	return &restoreInputs{sourcePath: sourcePath, db: db, cluster: cluster, storage: storage}, ctrl.Result{}, nil
}

func (r *RestoreReconciler) reserveAndCreate(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string,
	inputs *restoreInputs, logger logr.Logger) (ctrl.Result, error) {

	runID := generateRunID()
	if err := r.patchRunStatus(ctx, restore, func(status *databasesv1alpha1.RestoreStatus) {
		*status = databasesv1alpha1.RestoreStatus{
			Phase:              "Pending",
			Message:            "creating restore job",
			SpecHash:           specHash,
			JobName:            restoreJobName(restore.Name, runID),
			RunID:              runID,
			SourcePath:         inputs.sourcePath,
			ObservedGeneration: restore.Generation,
		}
	}); err != nil {
		return ctrl.Result{}, err
	}
	return r.createRunJob(ctx, restore, specHash, inputs, logger)
}

func (r *RestoreReconciler) createRunJob(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string,
	inputs *restoreInputs, logger logr.Logger) (ctrl.Result, error) {

	job := r.buildRestoreJob(restore, inputs.db, inputs.cluster, inputs.storage)
	err := r.Create(ctx, job)
	switch {
	case err == nil:
		logger.Info("restore job created", "job", job.Name, "runId", restore.Status.RunID, "source", restore.Status.SourcePath)
	case errors.IsAlreadyExists(err):
		existing, found, getErr := r.getRunJob(ctx, restore)
		if getErr != nil {
			return ctrl.Result{}, getErr
		}
		if !found {
			return ctrl.Result{RequeueAfter: rerunRequeueDelay}, nil
		}
		if !jobBelongsToRun(existing, restore.Status.RunID, restoreJobSelector(restore)) {
			return r.failJobNameCollision(ctx, restore, specHash, existing)
		}
		logger.V(1).Info("restore job already created for this run", "job", job.Name)
	default:
		return r.handleJobCreationError(ctx, restore, specHash, err, logger)
	}

	if err := r.confirmRun(ctx, restore, "restore job started"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *RestoreReconciler) confirmRun(ctx context.Context, restore *databasesv1alpha1.Restore, message string) error {
	return r.patchRunStatus(ctx, restore, func(status *databasesv1alpha1.RestoreStatus) {
		status.Phase = "Running"
		status.Message = message
		if status.StartedAt == nil {
			now := metav1.Now()
			status.StartedAt = &now
		}
	})
}

func (r *RestoreReconciler) continueRun(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string, logger logr.Logger) (ctrl.Result, error) {
	job, found, err := r.getRunJob(ctx, restore)
	if err != nil {
		return ctrl.Result{}, err
	}
	if found {
		if !jobBelongsToRun(job, restore.Status.RunID, restoreJobSelector(restore)) {
			return r.failJobNameCollision(ctx, restore, specHash, job)
		}
		if restore.Status.Phase != "Running" {
			if err := r.confirmRun(ctx, restore, "restore job running"); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.evaluateJobStatus(ctx, restore, job, specHash)
	}

	// A stale cache could hide that this restore's Job already ran and recorded its start or success.
	var fresh databasesv1alpha1.Restore
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(restore), &fresh); err != nil {
		return ctrl.Result{}, err
	}
	if fresh.ResourceVersion != restore.ResourceVersion {
		return ctrl.Result{RequeueAfter: rerunRequeueDelay}, nil
	}

	if restoreJobEvidence(restore) {
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf(
			"restore job %s disappeared without recording a result; the target database state is unknown", restore.Status.JobName), specHash)
	}
	return r.createReservedJob(ctx, restore, specHash, logger)
}

// Each of these is written only after the restore's Job exists, so a missing Job with any of them is gone, not pending creation.
func restoreJobEvidence(restore *databasesv1alpha1.Restore) bool {
	return restore.Status.Phase == "Running" || restore.Status.StartedAt != nil
}

func (r *RestoreReconciler) createReservedJob(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string, logger logr.Logger) (ctrl.Result, error) {
	inputs, result, err := r.resolveInputs(ctx, restore, specHash)
	if inputs == nil {
		return result, err
	}
	throttled, err := clusterThrottleMessage(ctx, r.Client, r.Namespace, inputs.cluster.Name, r.MaxConcurrentJobs, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	if throttled != "" {
		return r.waitForSlot(ctx, restore, specHash, throttled)
	}
	if inputs.sourcePath != restore.Status.SourcePath {
		return r.reserveAndCreate(ctx, restore, specHash, inputs, logger)
	}
	return r.createRunJob(ctx, restore, specHash, inputs, logger)
}

func (r *RestoreReconciler) waitForSlot(ctx context.Context, restore *databasesv1alpha1.Restore, specHash, message string) (ctrl.Result, error) {
	if err := r.patchRunStatus(ctx, restore, func(status *databasesv1alpha1.RestoreStatus) {
		status.Phase = "Pending"
		status.Message = message
		status.SpecHash = specHash
		status.ObservedGeneration = restore.Generation
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueDelayWhenThrottled}, nil
}

func (r *RestoreReconciler) getRunJob(ctx context.Context, restore *databasesv1alpha1.Restore) (*batchv1.Job, bool, error) {
	return getJob(ctx, r.Client, r.APIReader, types.NamespacedName{Name: restore.Status.JobName, Namespace: r.Namespace})
}

func (r *RestoreReconciler) failJobNameCollision(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string,
	existing *batchv1.Job) (ctrl.Result, error) {

	message := jobNameCollisionMessage(existing)
	log.FromContext(ctx).Error(nil, "restore job name taken by a foreign job", "job", existing.Name)
	if err := r.patchRunStatus(ctx, restore, func(status *databasesv1alpha1.RestoreStatus) {
		status.Phase = "Failed"
		status.Message = message
		status.SpecHash = specHash
		status.JobName = ""
		status.ObservedGeneration = restore.Generation
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *RestoreReconciler) handleResolveError(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	specHash string,
	err error,
) (ctrl.Result, error) {
	switch {
	case isDependencyNotReady(err):
		return r.updateStatus(ctx, restore, "Pending", fmt.Sprintf("waiting for %v", err), specHash)
	case isInvalidRestoreSource(err):
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf("failed to resolve %v", err), specHash)
	default:
		return ctrl.Result{}, err
	}
}

func (r *RestoreReconciler) handleJobCreationError(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	specHash string,
	err error,
	logger logr.Logger,
) (ctrl.Result, error) {
	if errors.IsInvalid(err) {
		logger.Error(err, "restore job spec rejected by the API server")
		return r.updateStatus(ctx, restore, "Failed", fmt.Sprintf("failed to create job: %v", err), specHash)
	}
	logger.Error(err, "failed to create restore job")
	// Create may have landed despite the error; the next reconcile finds it under the reserved name.
	if _, statusErr := r.updateStatus(ctx, restore, "Pending", fmt.Sprintf("failed to create job: %v", err), specHash); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

func (r *RestoreReconciler) getResources(ctx context.Context, restore *databasesv1alpha1.Restore, storageRef string) (
	*databasesv1alpha1.Database, *databasesv1alpha1.DBCluster, *databasesv1alpha1.BackupStorage, error) {

	dbName := restore.Spec.Target.DatabaseRef.Name
	var db databasesv1alpha1.Database
	if err := r.Get(ctx, types.NamespacedName{Name: dbName, Namespace: restore.Namespace}, &db); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil, newDependencyNotReady("target database %s: not found", dbName)
		}
		return nil, nil, nil, fmt.Errorf("failed to get target database %s: %w", dbName, err)
	}
	if db.Status.Phase != "Ready" {
		return nil, nil, nil, newDependencyNotReady("target database %s to become Ready (phase: %s)", dbName, db.Status.Phase)
	}

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil, newDependencyNotReady("cluster %s: not found", db.Spec.ClusterRef.Name)
		}
		return nil, nil, nil, fmt.Errorf("failed to get cluster %s: %w", db.Spec.ClusterRef.Name, err)
	}

	var storage databasesv1alpha1.BackupStorage
	if err := r.Get(ctx, types.NamespacedName{Name: storageRef}, &storage); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, nil, newDependencyNotReady("backup storage %s: not found", storageRef)
		}
		return nil, nil, nil, fmt.Errorf("failed to get backup storage %s: %w", storageRef, err)
	}

	return &db, &cluster, &storage, nil
}

func (r *RestoreReconciler) resolveSource(ctx context.Context, restore *databasesv1alpha1.Restore) (sourcePath, storageRefName string, err error) {
	source := restore.Spec.Source

	if source.BackupRef != nil {
		return r.resolveFromBackupRef(ctx, restore, source.BackupRef)
	}

	if source.LatestFrom != nil {
		return r.resolveFromLatest(ctx, restore, source.LatestFrom)
	}

	if source.Path != "" {
		if source.StorageRef == nil {
			return "", "", newInvalidSource("storageRef is required when using path")
		}
		return source.Path, source.StorageRef.Name, nil
	}

	return "", "", newInvalidSource("either backupRef, latestFrom, or path must be specified")
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
		if errors.IsNotFound(err) {
			return "", "", newDependencyNotReady("backup not found: %s/%s", ns, ref.Name)
		}
		return "", "", fmt.Errorf("failed to get backup %s/%s: %w", ns, ref.Name, err)
	}

	if err := r.checkSourceConsent(ctx, restore, controllers.DatabaseRefKey(backup.Spec.DatabaseRef, backup.Namespace)); err != nil {
		return "", "", err
	}

	switch backup.Status.Phase {
	case "", "Pending", "Running":
		return "", "", newDependencyNotReady("backup %s is not completed yet (phase: %s)", ref.Name, backup.Status.Phase)
	case "Failed":
		return "", "", newInvalidSource("backup is not completed (phase: %s)", backup.Status.Phase)
	}

	if backup.Status.Path == "" {
		return "", "", newInvalidSource("backup has no path in status")
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

	source := controllers.DatabaseRefKey(latestFrom.DatabaseRef, ns)
	if err := r.checkSourceConsent(ctx, restore, source); err != nil {
		return "", "", err
	}

	var backupList databasesv1alpha1.BackupList
	if err := r.List(ctx, &backupList, client.InNamespace(ns)); err != nil {
		return "", "", fmt.Errorf("failed to list backups: %w", err)
	}

	var latestBackup *databasesv1alpha1.Backup
	var latestTime *metav1.Time

	for i := range backupList.Items {
		backup := &backupList.Items[i]

		if controllers.DatabaseRefKey(backup.Spec.DatabaseRef, backup.Namespace) != source {
			continue
		}

		// Its file may be deleted before the Restore job reads it.
		if !backup.DeletionTimestamp.IsZero() {
			continue
		}

		if backup.Status.Phase != "Completed" {
			continue
		}

		if backup.Status.Path == "" {
			continue
		}

		if backup.Status.CompletedAt == nil {
			continue
		}

		if latestTime == nil || backup.Status.CompletedAt.After(latestTime.Time) {
			latestBackup = backup
			latestTime = backup.Status.CompletedAt
		}
	}

	if latestBackup == nil {
		return "", "", newInvalidSource("no completed backup found for database %s", latestFrom.DatabaseRef.Name)
	}

	return latestBackup.Status.Path, latestBackup.Spec.StorageRef.Name, nil
}

func (r *RestoreReconciler) checkSourceConsent(ctx context.Context, restore *databasesv1alpha1.Restore, source types.NamespacedName) error {
	if source.Namespace == restore.Namespace {
		return nil
	}
	var db databasesv1alpha1.Database
	if err := r.Get(ctx, source, &db); err != nil {
		if errors.IsNotFound(err) {
			return newInvalidSource("source database %s no longer exists, so it cannot allow namespace %q", source, restore.Namespace)
		}
		return fmt.Errorf("failed to get source database %s: %w", source, err)
	}
	if err := controllers.CheckReferenceConsent(&db, restore.Namespace); err != nil {
		return newDependencyNotReady("%s", err.Error())
	}
	return nil
}

func (r *RestoreReconciler) buildRestoreJob(
	restore *databasesv1alpha1.Restore,
	db *databasesv1alpha1.Database,
	cluster *databasesv1alpha1.DBCluster,
	storage *databasesv1alpha1.BackupStorage,
) *batchv1.Job {
	env := r.buildEnvVars(db, cluster, storage, restore.Status.SourcePath, restore.Spec.OnConflict)
	env = append(env,
		corev1.EnvVar{Name: "RESTORE_NAME", Value: restore.Name},
		corev1.EnvVar{Name: "RESTORE_NAMESPACE", Value: restore.Namespace},
		corev1.EnvVar{Name: "RUN_ID", Value: restore.Status.RunID},
	)

	backoffLimit := int32(0)
	ttlSeconds := int32(3600)
	if restore.Spec.TTLAfterCompletion != nil {
		ttlSeconds = int32(restore.Spec.TTLAfterCompletion.Seconds())
	}

	labels := restoreJobSelector(restore)
	labels[LabelRunID] = restore.Status.RunID
	jobLabels := map[string]string{LabelCluster: boundedLabelValue(cluster.Name)}
	for k, v := range labels {
		jobLabels[k] = v
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        restore.Status.JobName,
			Namespace:   r.Namespace,
			Labels:      jobLabels,
			Annotations: map[string]string{AnnotationRestoreName: restore.Name},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttlSeconds,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: r.ServiceAccountName,
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

	env = append(env, storageEnv(storage)...)

	return env
}

func (r *RestoreReconciler) evaluateJobStatus(ctx context.Context, restore *databasesv1alpha1.Restore, job *batchv1.Job, specHash string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if job.Status.Succeeded > 0 {
		return r.enterGranting(ctx, restore, specHash)
	}

	if job.Status.Failed > 0 {
		message := "restore job failed"
		if reason := jobFailureReason(ctx, r.Client, job); reason != "" {
			message = reason
		}
		logger.Error(nil, "restore failed", "reason", message)
		return r.updateStatus(ctx, restore, "Failed", message, specHash)
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// Completed must mean usable by the database's users; persisting Granting first lets grant retries outlive the Job's TTL.
func (r *RestoreReconciler) enterGranting(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string) (ctrl.Result, error) {
	if _, err := r.updateStatus(ctx, restore, "Granting", "restore succeeded, applying grants", specHash); err != nil {
		return ctrl.Result{}, err
	}
	return r.finishGranting(ctx, restore, specHash)
}

func (r *RestoreReconciler) handleDeletion(ctx context.Context, restore *databasesv1alpha1.Restore) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(restore, restoreFinalizer) {
		return ctrl.Result{}, nil
	}

	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.Namespace), restoreJobSelector(restore)); err != nil {
		return ctrl.Result{}, err
	}
	propagation := metav1.DeletePropagationBackground
	for i := range jobs.Items {
		if err := r.Delete(ctx, &jobs.Items[i], &client.DeleteOptions{
			PropagationPolicy: &propagation,
		}); err != nil && !errors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		logger.Info("restore deleted, job cleaned up", "job", jobs.Items[i].Name)
	}

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
	if err := r.patchRunStatus(ctx, restore, func(status *databasesv1alpha1.RestoreStatus) {
		status.Phase = phase
		status.Message = message
		status.SpecHash = specHash
		status.ObservedGeneration = restore.Generation
		// Granting is entered only from a fresh Job success, so the attempt budget restarts.
		if phase == "Granting" {
			status.GrantAttempts = 0
		}
	}); err != nil {
		return ctrl.Result{}, err
	}

	if phase == "Failed" {
		return ctrl.Result{}, nil
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *RestoreReconciler) updateStatusCompleted(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	specHash, message string,
) (ctrl.Result, error) {
	if err := r.patchRunStatus(ctx, restore, func(status *databasesv1alpha1.RestoreStatus) {
		now := metav1.Now()
		status.Phase = "Completed"
		status.Message = message
		status.SpecHash = specHash
		status.Duration = restoreDuration(restore)
		status.ObservedGeneration = restore.Generation
		status.CompletedAt = &now
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// Conditional on the version read, so a decision taken on a stale read never lands.
func (r *RestoreReconciler) patchRunStatus(ctx context.Context, restore *databasesv1alpha1.Restore, mutate func(*databasesv1alpha1.RestoreStatus)) error {
	patch := client.MergeFromWithOptions(restore.DeepCopy(), client.MergeFromWithOptimisticLock{})
	mutate(&restore.Status)
	return r.Status().Patch(ctx, restore, patch)
}

func (r *RestoreReconciler) computeSpecHash(restore *databasesv1alpha1.Restore) string {
	data, _ := json.Marshal(restore.Spec)
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:8])
}

func (r *RestoreReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := controllers.RegisterIndexers(context.Background(), mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.Restore{}).
		// Jobs carry no OwnerReference (cross-namespace), so watch by labels.
		Watches(&batchv1.Job{}, r.jobEventHandler()).
		Complete(r)
}

func (r *RestoreReconciler) jobEventHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		job, ok := obj.(*batchv1.Job)
		if !ok || job.Namespace != r.Namespace {
			return nil
		}

		restoreName := job.Annotations[AnnotationRestoreName]
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

func restoreJobSelector(restore *databasesv1alpha1.Restore) client.MatchingLabels {
	return client.MatchingLabels{
		LabelRestoreName:      boundedLabelValue(restore.Name),
		LabelRestoreNamespace: restore.Namespace,
	}
}

// "restore-" + name + "-" + 8-char run id must fit a 63-char DNS label (the job-name pod label).
const maxRestoreJobNamePart = 63 - len("restore-") - 1 - 8

func restoreJobName(restoreName, runID string) string {
	return fmt.Sprintf("restore-%s-%s", boundedName(strings.ReplaceAll(restoreName, ".", "-"), maxRestoreJobNamePart), runID)
}

// Permanent: no retry can make this source restorable.
type invalidSourceError struct {
	msg string
}

func (e *invalidSourceError) Error() string { return e.msg }

func newInvalidSource(format string, args ...any) error {
	return &invalidSourceError{msg: fmt.Sprintf(format, args...)}
}

func isInvalidRestoreSource(err error) bool {
	var i *invalidSourceError
	return stderrors.As(err, &i)
}

func (r *RestoreReconciler) finishGranting(ctx context.Context, restore *databasesv1alpha1.Restore, specHash string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	grantCtx, cancel := context.WithTimeout(ctx, regrantTimeout)
	defer cancel()

	failures, err := r.regrantDatabaseUsers(grantCtx, restore)
	if err != nil {
		// Nothing left to grant on; retrying only spins, and the data did land.
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
		return r.handleGrantFailures(ctx, restore, specHash, failures)
	}

	logger.Info("restore completed successfully", "duration", restoreDuration(restore))
	return r.updateStatusCompleted(ctx, restore, specHash, "restore completed successfully")
}

// Retries the whole round, not just the failed subjects, since every step is idempotent.
func (r *RestoreReconciler) handleGrantFailures(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	specHash string,
	failures []grantFailure,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	attempts := restore.Status.GrantAttempts + 1
	err := joinGrantFailures(failures)
	subjects := failedGrantSubjects(failures)

	if attempts >= grantAttemptLimit {
		logger.Error(err, "giving up on re-applying grants after restore", "attempts", attempts, "subjects", subjects)
		r.recordGrantsSkipped(restore, fmt.Sprintf("retries exhausted for %s", subjects))
		return r.updateStatusCompleted(ctx, restore, specHash,
			fmt.Sprintf("restore completed; grants not re-applied for: %s", subjects))
	}

	logger.Error(err, "failed to re-apply grants after restore", "attempts", attempts, "subjects", subjects)
	if statusErr := r.patchGrantingProgress(ctx, restore, attempts,
		fmt.Sprintf("restore succeeded, retrying grants: %v", err)); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	// nil error: a real error would drive workqueue backoff, defeating grantRetryDelay above.
	return ctrl.Result{RequeueAfter: grantRetryDelay}, nil
}

// Silent when nothing changed so a wedged cluster can't churn the API server.
func (r *RestoreReconciler) patchGrantingProgress(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
	attempts int32,
	message string,
) error {
	if restore.Status.Phase == "Granting" && restore.Status.Message == message && restore.Status.GrantAttempts == attempts {
		return nil
	}

	return r.patchRunStatus(ctx, restore, func(status *databasesv1alpha1.RestoreStatus) {
		status.Phase = "Granting"
		status.Message = message
		status.GrantAttempts = attempts
	})
}

func (r *RestoreReconciler) recordGrantsSkipped(restore *databasesv1alpha1.Restore, reason string) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Event(restore, corev1.EventTypeWarning, EventReasonRestoreGrantsSkipped,
		fmt.Sprintf("Restore completed but grants were not re-applied: %s", reason))
}

// pg_restore (--no-owner --no-acl) and onConflict=drop wipe grants, so they are re-applied.
func (r *RestoreReconciler) regrantDatabaseUsers(
	ctx context.Context,
	restore *databasesv1alpha1.Restore,
) ([]grantFailure, error) {
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

	var failures []grantFailure

	// A recreated database has the default ACL, no ownership marker and no extensions, and no reconcile fixes that for hours.
	if _, step, err := controllers.ApplyDatabaseSpec(ctx, pgClient, db.Status.DatabaseName, &db); err != nil {
		// The users still get their grants: a budget that runs out here must not leave PUBLIC revoked and nobody else able to connect.
		failures = append(failures, grantFailure{
			subject: "database " + db.Name,
			err:     fmt.Errorf("failed to re-apply database spec (%s) for %s: %w", step, db.Name, err),
		})
	}

	var users databasesv1alpha1.DatabaseUserList
	if err := r.List(ctx, &users, client.MatchingFields{
		controllers.DatabaseUserDatabaseRefIndex: controllers.DatabaseUserDatabaseRefKey(db.Namespace, db.Name),
	}); err != nil {
		return nil, fmt.Errorf("failed to list database users: %w", err)
	}
	if len(users.Items) == 0 {
		return failures, nil
	}

	return append(failures, applyRestoredGrants(ctx, pgClient, users.Items, &db,
		controllers.ElectOwner(users.Items, &db))...), nil
}

func applyRestoredGrants(
	ctx context.Context,
	pgClient postgres.ClientInterface,
	users []databasesv1alpha1.DatabaseUser,
	db *databasesv1alpha1.Database,
	ownerWinner *databasesv1alpha1.DatabaseUser,
) []grantFailure {
	logger := log.FromContext(ctx)

	var failures []grantFailure
	for i := range users {
		user := &users[i]

		grants, ok := controllers.GrantsOnDatabase(user, db)
		if !ok {
			continue
		}

		// An invalid spec resolves to a different grant set than the user's own controller applies.
		if err := controllers.ValidateUserSpec(user); err != nil {
			logger.Info("skipping grants for a DatabaseUser with an invalid spec",
				"user", user.Name, "namespace", user.Namespace, "reason", err.Error())
			continue
		}

		// Granting to a role the user's finalizer is dropping makes that DROP ROLE fail.
		if !user.DeletionTimestamp.IsZero() {
			logger.Info("skipping grants for a DatabaseUser being deleted",
				"user", user.Name, "namespace", user.Namespace, "role", grants.Username)
			continue
		}

		if grants.Privileges == "owner" && !controllers.SameUser(ownerWinner, user) {
			logger.Info("lowering grants to admin for a DatabaseUser that lost the owner election",
				"user", user.Name, "namespace", user.Namespace, "database", db.Name)
			grants = controllers.LoserGrants(grants)
		}

		// The role decides, not the CR phase: a Failed user may own a live role, a fresh Ready one may be uncached.
		exists, err := pgClient.UserExists(ctx, grants.Username)
		if err != nil {
			failures = append(failures, grantFailure{
				subject: grants.Username,
				err:     fmt.Errorf("failed to check role %s: %w", grants.Username, err),
			})
			continue
		}
		if !exists {
			logger.Info("role not yet created; user's own reconcile will grant",
				"user", user.Name, "namespace", user.Namespace, "role", grants.Username)
			continue
		}

		if err := controllers.ApplyUserGrants(ctx, pgClient, db.Status.DatabaseName, grants, controllers.OwnerRole(ownerWinner)); err != nil {
			failures = append(failures, grantFailure{subject: grants.Username, err: err})
			continue
		}
		logger.Info("re-applied grants after restore",
			"user", grants.Username, "database", db.Status.DatabaseName, "privileges", grants.Privileges)
	}

	return failures
}

type grantFailure struct {
	subject string
	err     error
}

// joinGrantFailures flattens the round's failures onto one line, because the result
// ends up in status.message where newlines are unreadable.
func joinGrantFailures(failures []grantFailure) error {
	messages := make([]string, 0, len(failures))
	for _, failure := range failures {
		messages = append(messages, failure.err.Error())
	}
	return stderrors.New(strings.Join(messages, "; "))
}

func failedGrantSubjects(failures []grantFailure) string {
	names := make([]string, 0, len(failures))
	for _, failure := range failures {
		names = append(names, failure.subject)
	}
	return strings.Join(names, ", ")
}

// Marks a failure no retry can fix, so the restore completes with the reason recorded.
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
