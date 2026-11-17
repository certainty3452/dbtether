package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"strconv"
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
)

const backupFinalizer = "dbtether.io/backup-job"

const (
	DefaultMaxConcurrentJobsPerCluster = 3
	RequeueDelayWhenThrottled          = 30 * time.Second
	// finalizerRequeueDelay backstops the watch event from the finalizer Update in case it lags.
	finalizerRequeueDelay = 1 * time.Second
	// rerunRequeueDelay starts a pending re-run without relying on our own status patch to enqueue one.
	rerunRequeueDelay = 1 * time.Second
	// storageDeleteTimeout keeps an unresponsive storage from pinning a reconcile worker.
	storageDeleteTimeout = 30 * time.Second
)

const (
	LabelBackupName      = "dbtether.io/backup"
	LabelBackupNamespace = "dbtether.io/backup-namespace"
	LabelCluster         = "dbtether.io/cluster"
	LabelRunID           = "dbtether.io/run-id"

	AnnotationBackupName = "dbtether.io/backup-name"
)

type BackupReconciler struct {
	client.Client
	APIReader            client.Reader
	Scheme               *runtime.Scheme
	Recorder             record.EventRecorder
	Image                string
	Namespace            string
	ServiceAccountName   string
	SSLMode              string
	MaxConcurrentBackups int

	// Karpenter/autoscaler protection.
	PodAnnotations map[string]string

	PodLabels map[string]string

	JobLabels map[string]string

	PodResources corev1.ResourceRequirements

	NewStorageClient StorageClientFactory
}

const (
	EventReasonBackupStarted    = "BackupStarted"
	EventReasonBackupCompleted  = "BackupCompleted"
	EventReasonBackupFailed     = "BackupFailed"
	EventReasonBackupThrottled  = "BackupThrottled"
	EventReasonFileDeleteFailed = "FileDeleteFailed"
	EventReasonFileOrphaned     = "FileOrphaned"
	ReasonResultNotRecorded     = "ResultNotRecorded"
	ReasonJobNameCollision      = "JobNameCollision"
)

func (r *BackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var backup databasesv1alpha1.Backup
	if err := r.Get(ctx, req.NamespacedName, &backup); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !backup.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &backup)
	}

	if result, done, err := r.ensureFinalizer(ctx, &backup); done {
		return result, err
	}

	specHash := r.computeSpecHash(&backup)

	alreadyProcessed, err := r.isAlreadyProcessed(ctx, &backup, specHash, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	if alreadyProcessed {
		return ctrl.Result{}, nil
	}

	logger.V(1).Info("reconciling backup", "database", backup.Spec.DatabaseRef.Name)

	result, err := r.reconcileRun(ctx, &backup, specHash, logger)
	if errors.IsConflict(err) {
		logger.V(1).Info("backup status changed since it was read, requeueing", "reason", err.Error())
		return ctrl.Result{RequeueAfter: rerunRequeueDelay}, nil
	}
	return result, err
}

func (r *BackupReconciler) reconcileRun(ctx context.Context, backup *databasesv1alpha1.Backup, specHash string, logger logr.Logger) (ctrl.Result, error) {
	// Status tracks exactly one run, so a run in flight has to finish before the next one starts.
	if runReserved(backup) {
		return r.continueRun(ctx, backup, logger)
	}
	return r.startNewRun(ctx, backup, specHash, logger)
}

func (r *BackupReconciler) ensureFinalizer(ctx context.Context, backup *databasesv1alpha1.Backup) (result ctrl.Result, done bool, err error) {
	if controllerutil.ContainsFinalizer(backup, backupFinalizer) {
		return ctrl.Result{}, false, nil
	}
	controllerutil.AddFinalizer(backup, backupFinalizer)
	if err := r.Update(ctx, backup); err != nil {
		return ctrl.Result{}, true, err
	}
	return ctrl.Result{RequeueAfter: finalizerRequeueDelay}, true, nil
}

func (r *BackupReconciler) isAlreadyProcessed(ctx context.Context, backup *databasesv1alpha1.Backup, specHash string, logger logr.Logger) (bool, error) {
	if backup.Status.Phase == "" || backup.Status.SpecHash != specHash {
		return false, nil
	}
	if !runFinished(backup) {
		return false, nil
	}
	logger.V(1).Info("backup already processed", "phase", backup.Status.Phase)

	// A no-op spec edit (same hash) still bumps Generation; catch observedGeneration up or a health check keyed on it stays Progressing forever.
	if backup.Generation != backup.Status.ObservedGeneration {
		patch := client.MergeFrom(backup.DeepCopy())
		backup.Status.ObservedGeneration = backup.Generation
		if err := r.Status().Patch(ctx, backup, patch); err != nil {
			return true, err
		}
	}

	return true, nil
}

func runFinished(backup *databasesv1alpha1.Backup) bool {
	return backup.Status.Phase == "Completed" || backup.Status.Phase == "Failed"
}

func runReserved(backup *databasesv1alpha1.Backup) bool {
	return backup.Status.RunID != "" && backup.Status.JobName != "" && !runFinished(backup)
}

// Each of these is written only after the run's Job exists, so a missing Job with any of them is gone, not pending creation.
func jobEvidence(backup *databasesv1alpha1.Backup) bool {
	return backup.Status.Phase == "Running" || backup.Status.StartedAt != nil || backup.Status.Path != ""
}

func (r *BackupReconciler) startNewRun(ctx context.Context, backup *databasesv1alpha1.Backup, specHash string, logger logr.Logger) (ctrl.Result, error) {
	db, cluster, storage, err := r.getResources(ctx, backup)
	if err != nil {
		// GitOps applies manifests out of order, so a missing dependency retries instead of failing terminally.
		if isDependencyNotReady(err) {
			return r.markPending(ctx, backup, specHash, err.Error())
		}
		return ctrl.Result{}, err
	}

	if msg := jobCredentialsProblem(cluster, storage, r.Namespace, "backup"); msg != "" {
		return r.markPending(ctx, backup, specHash, msg)
	}

	throttled, err := clusterThrottleMessage(ctx, r.Client, r.Namespace, cluster.Name, r.MaxConcurrentBackups, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	if throttled != "" {
		return r.markPending(ctx, backup, specHash, throttled)
	}

	if err := r.reserveRun(ctx, backup, specHash); err != nil {
		return ctrl.Result{}, err
	}
	return r.createRunJob(ctx, backup, db, cluster, storage, logger)
}

func (r *BackupReconciler) reserveRun(ctx context.Context, backup *databasesv1alpha1.Backup, specHash string) error {
	runID := generateRunID()
	return r.patchRunStatus(ctx, backup, func(status *databasesv1alpha1.BackupStatus) {
		*status = databasesv1alpha1.BackupStatus{
			Phase:              "Pending",
			Message:            "creating backup job",
			SpecHash:           specHash,
			JobName:            backupJobName(backup.Name, runID),
			RunID:              runID,
			ObservedGeneration: backup.Generation,
		}
	})
}

func (r *BackupReconciler) createRunJob(ctx context.Context, backup *databasesv1alpha1.Backup,
	db *databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster, storage *databasesv1alpha1.BackupStorage,
	logger logr.Logger) (ctrl.Result, error) {

	job := r.buildBackupJob(backup, db, cluster, storage)
	err := r.Create(ctx, job)
	switch {
	case err == nil:
		logger.Info("backup job created", "job", job.Name, "runId", backup.Status.RunID)
		if r.Recorder != nil {
			r.Recorder.Event(backup, corev1.EventTypeNormal, EventReasonBackupStarted,
				fmt.Sprintf("Started backup job %s for database %s", job.Name, backup.Spec.DatabaseRef.Name))
		}
	case errors.IsAlreadyExists(err):
		existing, found, getErr := r.getRunJob(ctx, backup)
		if getErr != nil {
			return ctrl.Result{}, getErr
		}
		if !found {
			return ctrl.Result{RequeueAfter: rerunRequeueDelay}, nil
		}
		if !jobBelongsToRun(existing, backup.Status.RunID, backupJobSelector(backup)) {
			return r.markFailed(ctx, backup, backup.Status.SpecHash, ReasonJobNameCollision, jobNameCollisionMessage(existing))
		}
		logger.V(1).Info("backup job already created for this run", "job", job.Name)
	default:
		return r.handleJobCreationError(ctx, backup, err, logger)
	}

	if err := r.confirmRun(ctx, backup, "backup job started"); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

func (r *BackupReconciler) confirmRun(ctx context.Context, backup *databasesv1alpha1.Backup, message string) error {
	return r.patchRunStatus(ctx, backup, func(status *databasesv1alpha1.BackupStatus) {
		status.Phase = "Running"
		status.Message = message
		if status.StartedAt == nil {
			now := metav1.Now()
			status.StartedAt = &now
		}
	})
}

func (r *BackupReconciler) continueRun(ctx context.Context, backup *databasesv1alpha1.Backup, logger logr.Logger) (ctrl.Result, error) {
	job, found, err := r.getRunJob(ctx, backup)
	if err != nil {
		return ctrl.Result{}, err
	}
	if found {
		if !jobBelongsToRun(job, backup.Status.RunID, backupJobSelector(backup)) {
			return r.markFailed(ctx, backup, backup.Status.SpecHash, ReasonJobNameCollision, jobNameCollisionMessage(job))
		}
		if backup.Status.Phase != "Running" {
			if err := r.confirmRun(ctx, backup, "backup job running"); err != nil {
				return ctrl.Result{}, err
			}
		}
		return r.evaluateJobStatus(ctx, backup, job)
	}
	if jobEvidence(backup) {
		return r.finishRun(ctx, backup, nil)
	}
	return r.createReservedJob(ctx, backup, logger)
}

func (r *BackupReconciler) createReservedJob(ctx context.Context, backup *databasesv1alpha1.Backup, logger logr.Logger) (ctrl.Result, error) {
	// A stale cache could hide the evidence that this run's Job already ran and is gone.
	var fresh databasesv1alpha1.Backup
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(backup), &fresh); err != nil {
		return ctrl.Result{}, err
	}
	if fresh.ResourceVersion != backup.ResourceVersion {
		return ctrl.Result{RequeueAfter: rerunRequeueDelay}, nil
	}
	if specHash := r.computeSpecHash(backup); specHash != backup.Status.SpecHash {
		return r.startNewRun(ctx, backup, specHash, logger)
	}

	db, cluster, storage, err := r.getResources(ctx, backup)
	if err != nil {
		if isDependencyNotReady(err) {
			return r.waitForReservedJob(ctx, backup, err.Error())
		}
		return ctrl.Result{}, err
	}
	if msg := jobCredentialsProblem(cluster, storage, r.Namespace, "backup"); msg != "" {
		return r.waitForReservedJob(ctx, backup, msg)
	}
	throttled, err := clusterThrottleMessage(ctx, r.Client, r.Namespace, cluster.Name, r.MaxConcurrentBackups, logger)
	if err != nil {
		return ctrl.Result{}, err
	}
	if throttled != "" {
		return r.waitForReservedJob(ctx, backup, throttled)
	}
	return r.createRunJob(ctx, backup, db, cluster, storage, logger)
}

func (r *BackupReconciler) waitForReservedJob(ctx context.Context, backup *databasesv1alpha1.Backup, message string) (ctrl.Result, error) {
	if err := r.patchRunStatus(ctx, backup, func(status *databasesv1alpha1.BackupStatus) {
		status.Message = message
	}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: RequeueDelayWhenThrottled}, nil
}

func (r *BackupReconciler) getRunJob(ctx context.Context, backup *databasesv1alpha1.Backup) (*batchv1.Job, bool, error) {
	return getJob(ctx, r.Client, r.APIReader, types.NamespacedName{Name: backup.Status.JobName, Namespace: r.Namespace})
}

// The cache can lag behind a Job created moments ago, so only the API server may report it missing.
func getJob(ctx context.Context, cached, apiReader client.Reader, key types.NamespacedName) (*batchv1.Job, bool, error) {
	var job batchv1.Job
	err := cached.Get(ctx, key, &job)
	if errors.IsNotFound(err) {
		err = apiReader.Get(ctx, key, &job)
	}
	if errors.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &job, true, nil
}

func jobFinished(job *batchv1.Job) bool {
	if job.Status.Succeeded > 0 {
		return true
	}
	_, failed := isJobFailed(job)
	return failed
}

func jobBelongsToRun(job *batchv1.Job, runID string, owner client.MatchingLabels) bool {
	if job.Labels[LabelRunID] != runID {
		return false
	}
	for key, value := range owner {
		if job.Labels[key] != value {
			return false
		}
	}
	return true
}

func jobNameCollisionMessage(job *batchv1.Job) string {
	return fmt.Sprintf("job %s/%s already exists and belongs to another run or owner (name collision); it was left untouched",
		job.Namespace, job.Name)
}

// Job env resolves secretKeyRefs in the Job's own namespace and never sees the operator's own environment.
func jobCredentialsProblem(cluster *databasesv1alpha1.DBCluster, storage *databasesv1alpha1.BackupStorage, jobNamespace, jobKind string) string {
	if cluster.Spec.CredentialsSecretRef == nil {
		return fmt.Sprintf("DBCluster %s has no credentialsSecretRef: %s Jobs cannot read the operator's credentialsFromEnv, so %s needs credentialsSecretRef pointing to a Secret in the operator namespace %s",
			cluster.Name, jobKind, jobKind, jobNamespace)
	}
	var refs []string
	if ref := cluster.Spec.CredentialsSecretRef; ref.Namespace != jobNamespace {
		refs = append(refs, fmt.Sprintf("DBCluster %s credentials Secret %s/%s", cluster.Name, ref.Namespace, ref.Name))
	}
	if ref := storage.Spec.CredentialsSecretRef; ref != nil && ref.Namespace != jobNamespace {
		refs = append(refs, fmt.Sprintf("BackupStorage %s credentials Secret %s/%s", storage.Name, ref.Namespace, ref.Name))
	}
	if len(refs) == 0 {
		return ""
	}
	return fmt.Sprintf("%s must be in the operator namespace %s: %s Jobs run there and can only read Secrets from it",
		strings.Join(refs, " and "), jobNamespace, jobKind)
}

func clusterThrottleMessage(ctx context.Context, c client.Reader, namespace, clusterName string, configuredMax int, logger logr.Logger) (string, error) {
	activeJobs, err := countActiveJobsForCluster(ctx, c, namespace, clusterName)
	if err != nil {
		return "", fmt.Errorf("failed to count active jobs: %w", err)
	}
	maxConcurrent := configuredMax
	if maxConcurrent <= 0 {
		maxConcurrent = DefaultMaxConcurrentJobsPerCluster
	}
	if activeJobs < maxConcurrent {
		return "", nil
	}
	logger.Info("throttling: too many concurrent jobs for cluster",
		"cluster", clusterName, "active", activeJobs, "max", maxConcurrent)
	return fmt.Sprintf("waiting for other backup/restore jobs on cluster %s (active: %d/%d)", clusterName, activeJobs, maxConcurrent), nil
}

func (r *BackupReconciler) handleJobCreationError(ctx context.Context, backup *databasesv1alpha1.Backup, err error, logger logr.Logger) (ctrl.Result, error) {
	if errors.IsInvalid(err) {
		logger.Error(err, "backup job spec rejected by the API server")
		return r.markFailed(ctx, backup, backup.Status.SpecHash, "InvalidJobSpec", err.Error())
	}
	logger.Error(err, "failed to create backup job")
	// Job creation failure says nothing about the backup itself; the returned error drives controller-runtime's backoff.
	if _, statusErr := r.waitForReservedJob(ctx, backup, fmt.Sprintf("failed to create job: %s", err.Error())); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	return ctrl.Result{}, err
}

// A changed output re-runs every Backup in the fleet after an upgrade.
func (r *BackupReconciler) computeSpecHash(backup *databasesv1alpha1.Backup) string {
	spec := backup.Spec
	// Changing the policy must not re-run the Backup.
	spec.DeletionPolicy = ""
	data, _ := json.Marshal(spec) //nolint:errcheck // hash doesn't need to be perfect
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:8])
}

func generateRunID() string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
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

	if err := r.deleteBackupJob(ctx, backup); err != nil {
		logger.Error(err, "failed to delete backup job during cleanup")
	}

	if backup.Spec.DeletionPolicy == "Delete" {
		if err := r.deleteBackupFile(ctx, backup); err != nil {
			return ctrl.Result{}, err
		}
	}

	patch := client.MergeFrom(backup.DeepCopy())
	controllerutil.RemoveFinalizer(backup, backupFinalizer)
	if err := r.Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("backup deleted, job cleaned up")
	return ctrl.Result{}, nil
}

// A missing BackupStorage is reported, not retried: it would block deletion forever.
func (r *BackupReconciler) deleteBackupFile(ctx context.Context, backup *databasesv1alpha1.Backup) error {
	path := backup.Status.Path
	if path == "" {
		return nil
	}
	logger := log.FromContext(ctx)

	ctx, cancel := context.WithTimeout(ctx, storageDeleteTimeout)
	defer cancel()

	var bs databasesv1alpha1.BackupStorage
	if err := r.Get(ctx, types.NamespacedName{Name: backup.Spec.StorageRef.Name}, &bs); err != nil {
		if !errors.IsNotFound(err) {
			if r.Recorder != nil {
				r.Recorder.Event(backup, corev1.EventTypeWarning, EventReasonFileDeleteFailed,
					fmt.Sprintf("Failed to delete %s: cannot read BackupStorage %s: %v", path, backup.Spec.StorageRef.Name, err))
			}
			return fmt.Errorf("failed to get BackupStorage %s: %w", backup.Spec.StorageRef.Name, err)
		}
		logger.Info("backup storage not found, leaving file in place", "storage", backup.Spec.StorageRef.Name, "path", path)
		if r.Recorder != nil {
			r.Recorder.Event(backup, corev1.EventTypeWarning, EventReasonFileOrphaned,
				fmt.Sprintf("BackupStorage %s not found, file %s left in storage", backup.Spec.StorageRef.Name, path))
		}
		return nil
	}

	factory := r.NewStorageClient
	if factory == nil {
		factory = defaultStorageClientFactory
	}
	storageClient, err := factory(ctx, r.Client, &bs)
	if err != nil {
		if r.Recorder != nil {
			r.Recorder.Event(backup, corev1.EventTypeWarning, EventReasonFileDeleteFailed,
				fmt.Sprintf("Failed to delete %s: %v", path, err))
		}
		return fmt.Errorf("failed to build storage client for BackupStorage %s: %w", bs.Name, err)
	}
	if closer, ok := storageClient.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	if err := storageClient.Delete(ctx, path); err != nil {
		if r.Recorder != nil {
			r.Recorder.Event(backup, corev1.EventTypeWarning, EventReasonFileDeleteFailed,
				fmt.Sprintf("Failed to delete %s: %v", path, err))
		}
		return fmt.Errorf("failed to delete backup file %s: %w", path, err)
	}
	logger.Info("deleted backup file", "storage", bs.Name, "path", path)
	return nil
}

func (r *BackupReconciler) deleteBackupJob(ctx context.Context, backup *databasesv1alpha1.Backup) error {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(r.Namespace), backupJobSelector(backup)); err != nil {
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

func countActiveJobsForCluster(ctx context.Context, c client.Reader, namespace, clusterName string) (int, error) {
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs,
		client.InNamespace(namespace),
		client.MatchingLabels{LabelCluster: boundedLabelValue(clusterName)},
	); err != nil {
		return 0, err
	}

	active := 0
	for i := range jobs.Items {
		if !jobFinished(&jobs.Items[i]) {
			active++
		}
	}
	return active, nil
}

func (r *BackupReconciler) getResources(ctx context.Context, backup *databasesv1alpha1.Backup) (
	*databasesv1alpha1.Database, *databasesv1alpha1.DBCluster, *databasesv1alpha1.BackupStorage, error) {

	db, err := getConsentedDatabase(ctx, r.Client, backup.Spec.DatabaseRef, backup.Namespace)
	if err != nil {
		return nil, nil, nil, err
	}

	if db.Status.Phase != "Ready" {
		return nil, nil, nil, newDependencyNotReady("database %s/%s is not ready (phase: %s)", db.Namespace, db.Name, db.Status.Phase)
	}

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

	return db, &cluster, &storage, nil
}

func getConsentedDatabase(ctx context.Context, c client.Client, ref databasesv1alpha1.DatabaseReference,
	fromNamespace string) (*databasesv1alpha1.Database, error) {

	key := controllers.DatabaseRefKey(ref, fromNamespace)
	var db databasesv1alpha1.Database
	if err := c.Get(ctx, key, &db); err != nil {
		if errors.IsNotFound(err) {
			return nil, newDependencyNotReady("database %s not found", key)
		}
		return nil, err
	}
	if err := controllers.CheckReferenceConsent(&db, fromNamespace); err != nil {
		return nil, newDependencyNotReady("%s", err.Error())
	}
	return &db, nil
}

//nolint:funlen // Job creation requires many fields - splitting would reduce readability
func (r *BackupReconciler) buildBackupJob(backup *databasesv1alpha1.Backup,
	db *databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster, storage *databasesv1alpha1.BackupStorage) *batchv1.Job {

	runID := backup.Status.RunID

	env := make([]corev1.EnvVar, 0, 24)
	env = append(env, []corev1.EnvVar{
		{Name: "DB_HOST", Value: cluster.Spec.Endpoint},
		{Name: "DB_PORT", Value: strconv.Itoa(cluster.Spec.Port)},
		{Name: "DB_NAME", Value: db.Status.DatabaseName},
		{Name: "CLUSTER_NAME", Value: cluster.Name},
		{Name: "DATABASE_NAME", Value: db.Status.DatabaseName},
		{Name: "PATH_TEMPLATE", Value: storage.Spec.PathTemplate},
		{Name: "FILENAME_TEMPLATE", Value: backup.Spec.FilenameTemplate},
		{Name: "BACKUP_NAME", Value: backup.Name},
		{Name: "BACKUP_NAMESPACE", Value: backup.Namespace},
		{Name: "RUN_ID", Value: runID},
	}...)
	if r.SSLMode != "" {
		env = append(env, corev1.EnvVar{Name: "DB_SSLMODE", Value: r.SSLMode})
	}

	env = append(env, r.getClusterCredentialsEnv(cluster)...)

	env = append(env, storageEnv(storage)...)

	backoffLimit := int32(3)
	if backup.Spec.JobConfig != nil && backup.Spec.JobConfig.BackoffLimit != nil {
		backoffLimit = *backup.Spec.JobConfig.BackoffLimit
	}

	var ttlSeconds int32 = 3600
	if backup.Spec.TTLAfterCompletion != nil {
		ttlSeconds = int32(backup.Spec.TTLAfterCompletion.Seconds())
	}

	var activeDeadlineSeconds *int64
	if backup.Spec.JobConfig != nil && backup.Spec.JobConfig.ActiveDeadlineSeconds != nil {
		activeDeadlineSeconds = backup.Spec.JobConfig.ActiveDeadlineSeconds
	}

	requiredLabels := backupJobSelector(backup)
	requiredLabels[LabelCluster] = boundedLabelValue(cluster.Name)
	requiredLabels[LabelRunID] = runID

	// Required labels are applied last so a user-supplied Helm value cannot override them.
	jobLabels := make(map[string]string)
	for k, v := range r.JobLabels {
		jobLabels[k] = v
	}
	for k, v := range requiredLabels {
		jobLabels[k] = v
	}

	podLabels := make(map[string]string)
	for k, v := range r.PodLabels {
		podLabels[k] = v
	}
	for k, v := range requiredLabels {
		podLabels[k] = v
	}

	podAnnotations := make(map[string]string)
	for k, v := range r.PodAnnotations {
		podAnnotations[k] = v
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        backup.Status.JobName,
			Namespace:   r.Namespace,
			Labels:      jobLabels,
			Annotations: map[string]string{AnnotationBackupName: backup.Name},
			// No OwnerReference (cross-namespace); cleanup via Backup finalizer, TTL as fallback.
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
					// OnFailure would let the job controller delete the pod, taking the container's reported cause with it.
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: r.ServiceAccountName,
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
	return env
}

func (r *BackupReconciler) evaluateJobStatus(ctx context.Context, backup *databasesv1alpha1.Backup,
	job *batchv1.Job) (ctrl.Result, error) {

	if job.Status.Succeeded > 0 {
		return r.finishRun(ctx, backup, job)
	}

	if _, failed := isJobFailed(job); failed {
		if err := r.updateFailedJobTTL(ctx, backup, job); err != nil {
			log.FromContext(ctx).Error(err, "failed to update TTL for failed job")
		}
		failureInfo := r.getJobFailureInfo(ctx, job)
		return r.updateStatusFailedWithInfo(ctx, backup, job, failureInfo)
	}

	if backup.Status.LastPodName == "" {
		if podName := r.getLastPodName(ctx, job); podName != "" {
			patch := runStatusPatch(backup)
			backup.Status.LastPodName = podName
			if err := r.Status().Patch(ctx, backup, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

type JobFailureInfo struct {
	Reason         string
	Message        string
	FailedAttempts int32
	MessageFromPod bool
}

func isJobFailed(job *batchv1.Job) (string, bool) {
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

func (r *BackupReconciler) getJobFailureInfo(ctx context.Context, job *batchv1.Job) *JobFailureInfo {
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

	// DeadlineExceeded is a kill the pod never reports, so only the backoff verdict may be replaced.
	if info.Reason == "" || info.Reason == "BackoffLimitExceeded" {
		if reason := jobFailureReason(ctx, r.Client, job); reason != "" {
			info.Message = reason
			info.MessageFromPod = true
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

func (r *BackupReconciler) updateStatusFailedWithInfo(ctx context.Context, backup *databasesv1alpha1.Backup,
	job *batchv1.Job, failureInfo *JobFailureInfo) (ctrl.Result, error) {

	logger := log.FromContext(ctx)

	patch := runStatusPatch(backup)

	backup.Status.Phase = "Failed"
	cause := failureInfo.Reason
	if failureInfo.MessageFromPod {
		cause = failureInfo.Message
	}
	backup.Status.Message = fmt.Sprintf("backup job failed: %s", cause)

	backup.Status.FailureReason = failureInfo.Reason
	backup.Status.FailureMessage = failureInfo.Message
	backup.Status.FailedAttempts = failureInfo.FailedAttempts

	if job != nil {
		backup.Status.LastPodName = r.getLastPodName(ctx, job)
	}

	now := metav1.Now()
	backup.Status.CompletedAt = &now

	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

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

	return r.rerunResult(backup), nil
}

// A Backup without a path cannot be restored from, so only a recorded path completes a run; job is nil when the Job is gone.
func (r *BackupReconciler) finishRun(ctx context.Context, backup *databasesv1alpha1.Backup,
	job *batchv1.Job) (ctrl.Result, error) {

	result, err := r.recordedResult(ctx, backup)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case result.path != "":
		return r.updateStatusCompleted(ctx, backup, job, result)
	case job == nil:
		return r.failRunWithoutResult(ctx, backup, EventReasonBackupFailed, "backup job not found",
			"JobNotFound", "the backup job disappeared before its result was recorded")
	default:
		message := "result not recorded: create a new Backup or change spec.trigger to run it again"
		return r.failRunWithoutResult(ctx, backup, ReasonResultNotRecorded, message,
			ReasonResultNotRecorded, message)
	}
}

func (r *BackupReconciler) recordedResult(ctx context.Context, backup *databasesv1alpha1.Backup) (jobResult, error) {
	if backup.Status.Path != "" {
		return jobResult{path: backup.Status.Path, size: backup.Status.Size, duration: backup.Status.Duration}, nil
	}

	// The Job writes the path itself, so the cache may not show it yet.
	var fresh databasesv1alpha1.Backup
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(backup), &fresh); err != nil {
		return jobResult{}, err
	}
	if fresh.Status.RunID != backup.Status.RunID {
		return jobResult{}, nil
	}
	return jobResult{path: fresh.Status.Path, size: fresh.Status.Size, duration: fresh.Status.Duration}, nil
}

func (r *BackupReconciler) failRunWithoutResult(ctx context.Context, backup *databasesv1alpha1.Backup,
	eventReason, message, failureReason, failureMessage string) (ctrl.Result, error) {

	logger := log.FromContext(ctx)

	patch := runStatusPatch(backup)
	now := metav1.Now()

	backup.Status.Phase = "Failed"
	backup.Status.Message = message
	backup.Status.FailureReason = failureReason
	backup.Status.FailureMessage = failureMessage
	backup.Status.CompletedAt = &now

	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Event(backup, corev1.EventTypeWarning, eventReason, backup.Status.FailureMessage)
	}

	logger.Info("backup failed",
		"reason", backup.Status.FailureReason,
		"message", backup.Status.FailureMessage)

	return r.rerunResult(backup), nil
}

func (r *BackupReconciler) getLastPodName(ctx context.Context, job *batchv1.Job) string {
	pod := latestJobPod(ctx, r.Client, job)
	if pod == nil {
		return ""
	}
	return pod.Name
}

func latestJobPod(ctx context.Context, c client.Client, job *batchv1.Job) *corev1.Pod {
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{
		"job-name": job.Name,
	}); err != nil {
		return nil
	}

	var latest *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if latest == nil || podAttemptIsLater(pod, latest) {
			latest = pod
		}
	}
	return latest
}

func podAttemptIsLater(candidate, incumbent *corev1.Pod) bool {
	if !candidate.CreationTimestamp.Equal(&incumbent.CreationTimestamp) {
		return candidate.CreationTimestamp.After(incumbent.CreationTimestamp.Time)
	}
	return candidate.Name > incumbent.Name
}

func jobFailureReason(ctx context.Context, c client.Client, job *batchv1.Job) string {
	pod := latestJobPod(ctx, c, job)
	if pod == nil {
		return ""
	}

	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cause := terminationCause(cs.State.Terminated); cause != "" {
			return cause
		}
		if cause := terminationCause(cs.LastTerminationState.Terminated); cause != "" {
			return cause
		}
	}
	return ""
}

func terminationCause(terminated *corev1.ContainerStateTerminated) string {
	if terminated == nil {
		return ""
	}
	if message := strings.TrimSpace(terminated.Message); message != "" {
		return message
	}
	return terminated.Reason
}

type jobResult struct {
	path, size, duration string
}

// Run status writes are conditional on the version read, so a stale reconcile never mixes two runs' fields.
func runStatusPatch(backup *databasesv1alpha1.Backup) client.Patch {
	return client.MergeFromWithOptions(backup.DeepCopy(), client.MergeFromWithOptimisticLock{})
}

func (r *BackupReconciler) patchRunStatus(ctx context.Context, backup *databasesv1alpha1.Backup, mutate func(*databasesv1alpha1.BackupStatus)) error {
	patch := runStatusPatch(backup)
	mutate(&backup.Status)
	return r.Status().Patch(ctx, backup, patch)
}

// markPending drops any previous run so its file or failure is never reported for the next one.
func (r *BackupReconciler) markPending(ctx context.Context, backup *databasesv1alpha1.Backup,
	specHash, message string) (ctrl.Result, error) {

	patch := runStatusPatch(backup)

	backup.Status = databasesv1alpha1.BackupStatus{
		Phase:              "Pending",
		Message:            message,
		SpecHash:           specHash,
		ObservedGeneration: backup.Generation,
	}

	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: RequeueDelayWhenThrottled}, nil
}

// markFailed ends the run terminally: retrying an unchanged spec the API server already rejected would only repeat the rejection.
func (r *BackupReconciler) markFailed(ctx context.Context, backup *databasesv1alpha1.Backup,
	specHash, reason, message string) (ctrl.Result, error) {

	logger := log.FromContext(ctx)

	patch := runStatusPatch(backup)
	now := metav1.Now()

	backup.Status = databasesv1alpha1.BackupStatus{
		Phase:              "Failed",
		Message:            fmt.Sprintf("failed to create job: %s", message),
		SpecHash:           specHash,
		ObservedGeneration: backup.Generation,
		FailureReason:      reason,
		FailureMessage:     message,
		CompletedAt:        &now,
	}

	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		r.Recorder.Event(backup, corev1.EventTypeWarning, EventReasonBackupFailed, backup.Status.FailureMessage)
	}

	logger.Info("backup failed",
		"reason", backup.Status.FailureReason,
		"message", backup.Status.FailureMessage)

	return ctrl.Result{}, nil
}

func (r *BackupReconciler) updateStatusCompleted(ctx context.Context, backup *databasesv1alpha1.Backup,
	job *batchv1.Job, result jobResult) (ctrl.Result, error) {

	logger := log.FromContext(ctx)

	patch := runStatusPatch(backup)

	backup.Status.Phase = "Completed"
	backup.Status.Message = "backup completed successfully"
	backup.Status.Path = result.path
	backup.Status.Size = result.size
	backup.Status.Duration = result.duration

	if job != nil {
		if podName := r.getLastPodName(ctx, job); podName != "" {
			backup.Status.LastPodName = podName
		}
	}

	now := metav1.Now()
	backup.Status.CompletedAt = &now

	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return ctrl.Result{}, err
	}

	if r.Recorder != nil {
		eventMessage := fmt.Sprintf("Backup completed successfully: %s (%s)",
			backup.Status.Path, backup.Status.Size)
		r.Recorder.Event(backup, corev1.EventTypeNormal, EventReasonBackupCompleted, eventMessage)
	}

	logger.Info("backup completed",
		"path", backup.Status.Path,
		"size", backup.Status.Size,
		"duration", backup.Status.Duration)

	return r.rerunResult(backup), nil
}

// rerunResult requeues a fresh run when the spec changed while this run's Job was executing.
func (r *BackupReconciler) rerunResult(backup *databasesv1alpha1.Backup) ctrl.Result {
	if r.computeSpecHash(backup) != backup.Status.SpecHash {
		return ctrl.Result{RequeueAfter: rerunRequeueDelay}
	}
	return ctrl.Result{}
}

func (r *BackupReconciler) updateFailedJobTTL(ctx context.Context, backup *databasesv1alpha1.Backup, job *batchv1.Job) error {
	logger := log.FromContext(ctx)

	var ttlAfterFailed int32 = 43200
	if backup.Spec.JobConfig != nil && backup.Spec.JobConfig.TTLSecondsAfterFailed != nil {
		ttlAfterFailed = *backup.Spec.JobConfig.TTLSecondsAfterFailed
	}

	currentTTL := job.Spec.TTLSecondsAfterFinished
	if currentTTL != nil && *currentTTL == ttlAfterFailed {
		return nil
	}

	logger.Info("updating TTL for failed job", "oldTTL", currentTTL, "newTTL", ttlAfterFailed)

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
		// Jobs carry no OwnerReference (cross-namespace), so watch by labels.
		Watches(&batchv1.Job{}, r.jobEventHandler()).
		Complete(r)
}

func (r *BackupReconciler) jobEventHandler() handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []ctrl.Request {
		job, ok := obj.(*batchv1.Job)
		if !ok || job.Namespace != r.Namespace {
			return nil
		}

		backupName := job.Annotations[AnnotationBackupName]
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

// Under GitOps the referencing object can reconcile before its dependencies exist: requeue, don't fail.
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

// Shared by backup and restore jobs; nil when unset so IRSA/Pod Identity applies.
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

func storageEnv(storage *databasesv1alpha1.BackupStorage) []corev1.EnvVar {
	var env []corev1.EnvVar
	switch {
	case storage.Spec.S3 != nil:
		env = append(env,
			corev1.EnvVar{Name: "STORAGE_TYPE", Value: "s3"},
			corev1.EnvVar{Name: "S3_BUCKET", Value: storage.Spec.S3.Bucket},
			corev1.EnvVar{Name: "S3_REGION", Value: storage.Spec.S3.Region},
		)
		if storage.Spec.S3.Endpoint != "" {
			env = append(env, corev1.EnvVar{Name: "S3_ENDPOINT", Value: storage.Spec.S3.Endpoint})
		}
		env = append(env, storageCredentialsEnv(storage)...)
	case storage.Spec.GCS != nil:
		env = append(env,
			corev1.EnvVar{Name: "STORAGE_TYPE", Value: "gcs"},
			corev1.EnvVar{Name: "GCS_BUCKET", Value: storage.Spec.GCS.Bucket},
		)
		if storage.Spec.GCS.Project != "" {
			env = append(env, corev1.EnvVar{Name: "GCS_PROJECT", Value: storage.Spec.GCS.Project})
		}
	case storage.Spec.Azure != nil:
		env = append(env,
			corev1.EnvVar{Name: "STORAGE_TYPE", Value: "azure"},
			corev1.EnvVar{Name: "AZURE_CONTAINER", Value: storage.Spec.Azure.Container},
			corev1.EnvVar{Name: "AZURE_ACCOUNT", Value: storage.Spec.Azure.StorageAccount},
		)
	}
	return env
}

func backupJobSelector(backup *databasesv1alpha1.Backup) client.MatchingLabels {
	return client.MatchingLabels{
		LabelBackupName:      boundedLabelValue(backup.Name),
		LabelBackupNamespace: backup.Namespace,
	}
}

const (
	maxLabelValueLength = 63
	boundedNameHashLen  = 8
	// "backup-" + name + "-" + 8-char run id must fit a 63-char DNS label (the job-name pod label).
	maxBackupJobNamePart = 63 - len("backup-") - 1 - 8
)

func backupJobName(backupName, runID string) string {
	return fmt.Sprintf("backup-%s-%s", boundedName(strings.ReplaceAll(backupName, ".", "-"), maxBackupJobNamePart), runID)
}

func boundedLabelValue(value string) string {
	return boundedName(value, maxLabelValueLength)
}

// The hash of the full name keeps distinct long names distinct after truncation.
func boundedName(name string, maxLen int) string {
	if len(name) <= maxLen {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	prefix := strings.TrimRight(name[:maxLen-boundedNameHashLen-1], "-.")
	return prefix + "-" + hex.EncodeToString(sum[:])[:boundedNameHashLen]
}
