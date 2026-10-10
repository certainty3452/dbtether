package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/controllers"
	pkgbackup "github.com/certainty3452/dbtether/pkg/backup"
	"github.com/certainty3452/dbtether/pkg/storage"
)

const (
	scheduleFinalizerName        = "dbtether.io/schedule-finalizer"
	LabelScheduleName            = "dbtether.io/schedule"
	LabelScheduleNS              = "dbtether.io/schedule-namespace"
	AnnotationLastRetentionClean = "dbtether.io/last-retention-cleanup"

	RetentionCleanupDebounce = 60 * time.Second

	schedulePendingRequeueDelay = 30 * time.Second

	retentionCleanupTimeout = 10 * time.Minute
)

type BackupScheduleReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Log       *zap.SugaredLogger
	Namespace string
}

func (r *BackupScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.With("schedule", req.NamespacedName)

	var schedule dbtether.BackupSchedule
	if err := r.Get(ctx, req.NamespacedName, &schedule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !schedule.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &schedule)
	}

	if result, err := r.ensureScheduleFinalizer(ctx, &schedule); result != nil {
		return *result, err
	}

	log.Debugw("reconciling backup schedule",
		"database", schedule.Spec.DatabaseRef.Name,
		"cronSchedule", schedule.Spec.Schedule,
	)

	if schedule.Spec.Suspend {
		return r.updateStatus(ctx, &schedule, "Suspended", "Schedule is suspended", nil, nil)
	}

	cronSchedule, err := r.parseCronSchedule(schedule.Spec.Schedule)
	if err != nil {
		log.Errorw("invalid cron schedule", "error", err)
		return r.updateStatus(ctx, &schedule, "Failed", fmt.Sprintf("Invalid cron schedule: %v", err), nil, nil)
	}

	if msg, err := r.crossNamespaceProblem(ctx, &schedule); err != nil {
		return ctrl.Result{}, err
	} else if msg != "" {
		return r.updateStatusWithRequeue(ctx, &schedule, "Pending", msg, nil, nil, schedulePendingRequeueDelay)
	}

	nextRun := r.calculateNextRun(&schedule, cronSchedule)

	if time.Now().After(nextRun) || time.Now().Equal(nextRun) {
		return r.handleScheduledBackup(ctx, &schedule, cronSchedule, nextRun, log)
	}

	return r.scheduleNextRun(ctx, &schedule, nextRun, log)
}

// A same-namespace Database is left to the Backups, which wait for it themselves.
func (r *BackupScheduleReconciler) crossNamespaceProblem(ctx context.Context, schedule *dbtether.BackupSchedule) (string, error) {
	if controllers.DatabaseRefKey(schedule.Spec.DatabaseRef, schedule.Namespace).Namespace == schedule.Namespace {
		return "", nil
	}
	_, err := getConsentedDatabase(ctx, r.Client, schedule.Spec.DatabaseRef, schedule.Namespace)
	if isDependencyNotReady(err) {
		return err.Error(), nil
	}
	return "", err
}

func (r *BackupScheduleReconciler) ensureScheduleFinalizer(ctx context.Context, schedule *dbtether.BackupSchedule) (*ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(schedule, scheduleFinalizerName) {
		return nil, nil
	}
	patch := client.MergeFrom(schedule.DeepCopy())
	controllerutil.AddFinalizer(schedule, scheduleFinalizerName)
	if err := r.Patch(ctx, schedule, patch); err != nil {
		return &ctrl.Result{}, err
	}
	return &ctrl.Result{RequeueAfter: finalizerRequeueDelay}, nil
}

func (r *BackupScheduleReconciler) parseCronSchedule(cronExpr string) (cron.Schedule, error) {
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	return parser.Parse(cronExpr)
}

func (r *BackupScheduleReconciler) calculateNextRun(schedule *dbtether.BackupSchedule, cronSchedule cron.Schedule) time.Time {
	var lastRun time.Time
	if schedule.Status.LastBackupTime != nil {
		lastRun = schedule.Status.LastBackupTime.Time
	} else {
		lastRun = schedule.CreationTimestamp.Time
	}
	return cronSchedule.Next(lastRun)
}

func (r *BackupScheduleReconciler) handleScheduledBackup(
	ctx context.Context,
	schedule *dbtether.BackupSchedule,
	cronSchedule cron.Schedule,
	nextRun time.Time,
	log *zap.SugaredLogger,
) (ctrl.Result, error) {
	backupName := r.generateBackupName(schedule, nextRun)
	nextRunMeta := metav1.NewTime(nextRun)

	existingBackup := &dbtether.Backup{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: schedule.Namespace, Name: backupName}, existingBackup); err == nil {
		log.Debugw("backup already exists for scheduled time", "backup", backupName, "scheduledTime", nextRun)
		return r.updateStatusAfterBackup(ctx, schedule, cronSchedule, nextRun)
	}

	log.Infow("creating scheduled backup", "scheduledTime", nextRun, "backupName", backupName)

	backup, err := r.createBackup(ctx, schedule, backupName)
	if err != nil {
		if client.IgnoreAlreadyExists(err) == nil {
			log.Debugw("backup created by concurrent reconcile", "backup", backupName)
			return r.updateStatusAfterBackup(ctx, schedule, cronSchedule, nextRun)
		}
		if errors.IsInvalid(err) {
			log.Errorw("backup rejected by the API server", "error", err)
			return r.updateStatus(ctx, schedule, "Failed", fmt.Sprintf("Failed to create backup: %v", err), nil, &nextRunMeta)
		}
		return ctrl.Result{}, err
	}

	log.Infow("scheduled backup created", "backup", backup.Name, "runId", backup.Status.RunID)
	return r.updateStatusAfterBackup(ctx, schedule, cronSchedule, nextRun)
}

func (r *BackupScheduleReconciler) updateStatusAfterBackup(
	ctx context.Context,
	schedule *dbtether.BackupSchedule,
	cronSchedule cron.Schedule,
	lastRun time.Time,
) (ctrl.Result, error) {
	nowMeta := metav1.Now()
	newNextRun := cronSchedule.Next(lastRun)
	newNextRunMeta := metav1.NewTime(newNextRun)
	return r.updateStatus(ctx, schedule, "Active", "", &nowMeta, &newNextRunMeta)
}

func (r *BackupScheduleReconciler) scheduleNextRun(
	ctx context.Context,
	schedule *dbtether.BackupSchedule,
	nextRun time.Time,
	log *zap.SugaredLogger,
) (ctrl.Result, error) {
	requeueAfter := time.Until(nextRun)
	if requeueAfter < 0 {
		requeueAfter = time.Second
	}

	// Copy: the goroutine outlives this reconcile.
	scheduleCopy := schedule.DeepCopy()
	r.startRetentionCleanup(ctx, scheduleCopy, log)

	nextRunMeta := metav1.NewTime(nextRun)
	return r.updateStatusWithRequeue(ctx, schedule, "Active", "", nil, &nextRunMeta, requeueAfter)
}

// Deterministic per scheduled time so concurrent reconciles collide on the name instead of duplicating.
func (r *BackupScheduleReconciler) generateBackupName(schedule *dbtether.BackupSchedule, scheduledTime time.Time) string {
	timeSuffix := scheduledTime.UTC().Format("20060102-1504")
	return fmt.Sprintf("%s-%s", boundedName(schedule.Name, validation.DNS1123SubdomainMaxLength-len(timeSuffix)-1), timeSuffix)
}

func (r *BackupScheduleReconciler) createBackup(ctx context.Context, schedule *dbtether.BackupSchedule, backupName string) (*dbtether.Backup, error) {
	backup := &dbtether.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupName,
			Namespace: schedule.Namespace,
			Labels: map[string]string{
				LabelScheduleName: boundedLabelValue(schedule.Name),
				LabelScheduleNS:   schedule.Namespace,
			},
		},
		Spec: dbtether.BackupSpec{
			DatabaseRef: schedule.Spec.DatabaseRef,
			StorageRef:  schedule.Spec.StorageRef,
		},
	}

	if schedule.Spec.FilenameTemplate != "" {
		backup.Spec.FilenameTemplate = schedule.Spec.FilenameTemplate
	}

	if schedule.Spec.JobConfig != nil {
		backup.Spec.JobConfig = schedule.Spec.JobConfig
	}

	if err := controllerutil.SetControllerReference(schedule, backup, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference: %w", err)
	}

	if err := r.Create(ctx, backup); err != nil {
		return nil, fmt.Errorf("failed to create backup: %w", err)
	}

	return backup, nil
}

// The reconcile ctx is the manager ctx, so cleanup is cancelled on shutdown and leader loss.
func (r *BackupScheduleReconciler) startRetentionCleanup(ctx context.Context, schedule *dbtether.BackupSchedule, log *zap.SugaredLogger) <-chan struct{} {
	done := make(chan struct{})
	cleanupCtx, cancel := context.WithTimeout(ctx, retentionCleanupTimeout)
	go func() {
		defer close(done)
		defer cancel()
		r.runRetentionCleanup(cleanupCtx, schedule, log)
	}()
	return done
}

func (r *BackupScheduleReconciler) runRetentionCleanup(ctx context.Context, schedule *dbtether.BackupSchedule, log *zap.SugaredLogger) {
	if schedule.Spec.Retention == nil {
		return
	}

	if !r.shouldRunRetentionCleanup(schedule) {
		log.Debugw("retention cleanup: skipping (debounce)", "schedule", schedule.Name)
		return
	}

	// Update annotation to mark cleanup start (prevent parallel runs)
	if err := r.updateRetentionCleanupAnnotation(ctx, schedule); err != nil {
		log.Debugw("retention cleanup: failed to update annotation (concurrent cleanup)", "error", err)
		return
	}

	storageClient, prefix, err := r.prepareRetentionCleanup(ctx, schedule, log)
	if err != nil || storageClient == nil {
		return
	}
	if closer, ok := storageClient.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	r.executeRetentionCleanup(ctx, storageClient, prefix, schedule, log)

	r.cleanupBackupCRDs(ctx, schedule, log)
}

func (r *BackupScheduleReconciler) prepareRetentionCleanup(ctx context.Context, schedule *dbtether.BackupSchedule, log *zap.SugaredLogger) (storage.StorageClient, string, error) {
	db, err := getConsentedDatabase(ctx, r.Client, schedule.Spec.DatabaseRef, schedule.Namespace)
	if err != nil {
		if !isDependencyNotReady(err) {
			log.Warnw("retention cleanup: failed to get database", "error", err)
		}
		return nil, "", err
	}

	var cluster dbtether.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if !errors.IsNotFound(err) {
			log.Warnw("retention cleanup: failed to get cluster", "error", err)
		}
		return nil, "", err
	}

	var backupStorage dbtether.BackupStorage
	if err := r.Get(ctx, types.NamespacedName{Name: schedule.Spec.StorageRef.Name}, &backupStorage); err != nil {
		if !errors.IsNotFound(err) {
			log.Warnw("retention cleanup: failed to get backup storage", "error", err)
		}
		return nil, "", err
	}

	storageClient, err := defaultStorageClientFactory(ctx, r.Client, &backupStorage)
	if err != nil {
		log.Warnw("retention cleanup: failed to create storage client", "provider", backupStorage.GetProvider(), "error", err)
		return nil, "", err
	}

	prefix, err := r.buildStoragePath(&backupStorage, &cluster, db)
	if err != nil {
		log.Warnw("retention cleanup: failed to build storage path", "error", err)
		return nil, "", err
	}

	return storageClient, prefix, nil
}

// Two schedules on one database with the same filename template share these objects; retention cannot tell them apart.
func (r *BackupScheduleReconciler) executeRetentionCleanup(ctx context.Context, storageClient storage.StorageClient, dbPath string, schedule *dbtether.BackupSchedule, log *zap.SugaredLogger) {
	filenameTemplate := schedule.Spec.FilenameTemplate
	if filenameTemplate == "" {
		filenameTemplate = pkgbackup.DefaultFilenameTemplate
	}
	filenameFilter, err := pkgbackup.FilenameTemplateToRegex(filenameTemplate)
	if err != nil {
		log.Warnw("retention cleanup: invalid filename template, skipping object retention", "template", filenameTemplate, "error", err)
		return
	}
	prefix := strings.TrimSuffix(dbPath, "/") + "/"

	retentionManager := pkgbackup.NewRetentionManager(log)
	toDelete, err := retentionManager.ApplyRetention(ctx, storageClient, prefix, schedule.Spec.Retention, filenameFilter)
	if err != nil {
		log.Warnw("retention cleanup: failed to apply retention policy", "error", err)
		return
	}

	if len(toDelete) == 0 {
		log.Debugw("retention cleanup: no files to delete", "prefix", prefix, "retention", schedule.Spec.Retention)
		return
	}

	if err := retentionManager.DeleteFiles(ctx, storageClient, toDelete); err != nil {
		log.Warnw("retention cleanup: failed to delete some files", "error", err)
	}
}

func (r *BackupScheduleReconciler) cleanupBackupCRDs(ctx context.Context, schedule *dbtether.BackupSchedule, log *zap.SugaredLogger) {
	if schedule.Spec.Retention == nil {
		return
	}

	// Schedules stored before the CEL rule can carry an empty policy; an empty
	// keepSet here would delete every Backup CR.
	if !pkgbackup.HasActiveRule(schedule.Spec.Retention) {
		log.Warnw("retention cleanup: policy has no active rule; no backup CRDs will be deleted",
			"schedule", schedule.Name,
		)
		return
	}

	var backups dbtether.BackupList
	if err := r.List(ctx, &backups, client.InNamespace(schedule.Namespace), scheduleBackupSelector(schedule)); err != nil {
		log.Warnw("retention cleanup: failed to list backup CRDs", "error", err)
		return
	}

	if len(backups.Items) == 0 {
		return
	}

	sort.Slice(backups.Items, func(i, j int) bool {
		return backups.Items[i].CreationTimestamp.After(backups.Items[j].CreationTimestamp.Time)
	})

	keepSet := r.calculateBackupKeepSet(backups.Items, schedule.Spec.Retention)

	if len(keepSet) == len(backups.Items) {
		return
	}

	deleted := 0
	for i := range backups.Items {
		backup := &backups.Items[i]

		if keepSet[backup.Name] {
			continue
		}

		if backup.Status.Phase != "Completed" && backup.Status.Phase != "Failed" {
			continue
		}

		if err := r.Delete(ctx, backup); err != nil {
			if !errors.IsNotFound(err) {
				log.Warnw("retention cleanup: failed to delete backup CRD",
					"backup", backup.Name,
					"error", err,
				)
			}
		} else {
			deleted++
			log.Debugw("retention cleanup: deleted old backup CRD", "backup", backup.Name)
		}
	}

	if deleted > 0 {
		log.Infow("retention cleanup: deleted old Backup CRDs",
			"schedule", schedule.Name,
			"deleted", deleted,
			"kept", len(keepSet),
		)
	}
}

// backups must be sorted newest first.
func (r *BackupScheduleReconciler) calculateBackupKeepSet(backups []dbtether.Backup, policy *dbtether.RetentionPolicy) map[string]bool {
	files := make([]pkgbackup.BackupFile, len(backups))
	for i := range backups {
		files[i] = pkgbackup.BackupFile{Key: backups[i].Name, Timestamp: backups[i].CreationTimestamp.Time}
	}
	return pkgbackup.KeepSet(files, policy)
}

func (r *BackupScheduleReconciler) shouldRunRetentionCleanup(schedule *dbtether.BackupSchedule) bool {
	if schedule.Annotations == nil {
		return true
	}

	lastCleanupStr, ok := schedule.Annotations[AnnotationLastRetentionClean]
	if !ok {
		return true
	}

	lastCleanup, err := time.Parse(time.RFC3339, lastCleanupStr)
	if err != nil {
		return true
	}

	return time.Since(lastCleanup) >= RetentionCleanupDebounce
}

func (r *BackupScheduleReconciler) updateRetentionCleanupAnnotation(ctx context.Context, schedule *dbtether.BackupSchedule) error {
	var current dbtether.BackupSchedule
	if err := r.Get(ctx, client.ObjectKeyFromObject(schedule), &current); err != nil {
		return err
	}

	if !r.shouldRunRetentionCleanup(&current) {
		return fmt.Errorf("debounce check failed with fresh data")
	}

	patch := client.MergeFrom(current.DeepCopy())

	if current.Annotations == nil {
		current.Annotations = make(map[string]string)
	}
	current.Annotations[AnnotationLastRetentionClean] = time.Now().UTC().Format(time.RFC3339)

	return r.Patch(ctx, &current, patch)
}

func (r *BackupScheduleReconciler) buildStoragePath(
	backupStorage *dbtether.BackupStorage,
	cluster *dbtether.DBCluster,
	db *dbtether.Database,
) (string, error) {
	pathTemplate := backupStorage.Spec.PathTemplate
	if pathTemplate == "" {
		return fmt.Sprintf("%s/%s", cluster.Name, db.Status.DatabaseName), nil
	}

	tmpl, err := template.New("path").Parse(pathTemplate)
	if err != nil {
		return "", fmt.Errorf("invalid path template: %w", err)
	}

	data := map[string]string{
		"ClusterName":  cluster.Name,
		"DatabaseName": db.Status.DatabaseName,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute path template: %w", err)
	}

	return buf.String(), nil
}

func (r *BackupScheduleReconciler) handleDeletion(ctx context.Context, schedule *dbtether.BackupSchedule) (ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(schedule, scheduleFinalizerName) {
		var backups dbtether.BackupList
		if err := r.List(ctx, &backups, client.InNamespace(schedule.Namespace), scheduleBackupSelector(schedule)); err != nil {
			return ctrl.Result{}, err
		}

		for i := range backups.Items {
			if err := r.Delete(ctx, &backups.Items[i]); err != nil {
				r.Log.Warnw("failed to delete backup during schedule cleanup",
					"backup", backups.Items[i].Name,
					"error", err,
				)
			}
		}

		// Remove finalizer using Patch to avoid optimistic locking conflicts
		patch := client.MergeFrom(schedule.DeepCopy())
		controllerutil.RemoveFinalizer(schedule, scheduleFinalizerName)
		if err := r.Patch(ctx, schedule, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *BackupScheduleReconciler) updateStatus(
	ctx context.Context,
	schedule *dbtether.BackupSchedule,
	phase, message string,
	lastBackupTime, nextScheduledTime *metav1.Time,
) (ctrl.Result, error) {
	return r.updateStatusWithRequeue(ctx, schedule, phase, message, lastBackupTime, nextScheduledTime, 0)
}

func (r *BackupScheduleReconciler) updateStatusWithRequeue(
	ctx context.Context,
	schedule *dbtether.BackupSchedule,
	phase, message string,
	lastBackupTime, nextScheduledTime *metav1.Time,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	lastSuccessful, completed, err := r.completedBackupSummary(ctx, schedule)
	if err != nil {
		return ctrl.Result{}, err
	}

	patch := client.MergeFrom(schedule.DeepCopy())

	schedule.Status.Phase = phase
	schedule.Status.Message = message
	schedule.Status.ObservedGeneration = schedule.Generation
	schedule.Status.LastSuccessfulBackup = lastSuccessful
	schedule.Status.ManagedBackups = completed

	if lastBackupTime != nil {
		schedule.Status.LastBackupTime = lastBackupTime
	}
	if nextScheduledTime != nil {
		schedule.Status.NextScheduledTime = nextScheduledTime
	}

	if err := r.Status().Patch(ctx, schedule, patch); err != nil {
		return ctrl.Result{}, err
	}

	if requeueAfter > 0 {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *BackupScheduleReconciler) completedBackupSummary(ctx context.Context, schedule *dbtether.BackupSchedule) (lastSuccessful string, completed int, err error) {
	var backups dbtether.BackupList
	if err := r.List(ctx, &backups, client.InNamespace(schedule.Namespace), scheduleBackupSelector(schedule)); err != nil {
		return "", 0, err
	}
	var latest *metav1.Time
	for i := range backups.Items {
		b := &backups.Items[i]
		if b.Status.Phase != "Completed" {
			continue
		}
		completed++
		if b.Status.CompletedAt != nil && (latest == nil || b.Status.CompletedAt.After(latest.Time)) {
			latest = b.Status.CompletedAt
			lastSuccessful = b.Name
		}
	}
	return lastSuccessful, completed, nil
}

func scheduleBackupSelector(schedule *dbtether.BackupSchedule) client.MatchingLabels {
	return client.MatchingLabels{LabelScheduleName: boundedLabelValue(schedule.Name)}
}

func (r *BackupScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbtether.BackupSchedule{}).
		Owns(&dbtether.Backup{}).
		Complete(r)
}
