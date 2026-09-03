package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"text/template"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	pkgbackup "github.com/certainty3452/dbtether/pkg/backup"
	"github.com/certainty3452/dbtether/pkg/storage"
)

const (
	scheduleFinalizerName        = "dbtether.io/schedule-finalizer"
	LabelScheduleName            = "dbtether.io/schedule"
	LabelScheduleNS              = "dbtether.io/schedule-namespace"
	AnnotationLastRetentionClean = "dbtether.io/last-retention-cleanup"

	// RetentionCleanupDebounce is the minimum time between retention cleanups
	RetentionCleanupDebounce = 60 * time.Second
)

type BackupScheduleReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	Log       *zap.SugaredLogger
	Namespace string // operator namespace
}

// +kubebuilder:rbac:groups=dbtether.io,resources=backupschedules,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=backupschedules/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=backups,verbs=get;list;watch;create;update;patch;delete

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

	nextRun := r.calculateNextRun(&schedule, cronSchedule)

	if time.Now().After(nextRun) || time.Now().Equal(nextRun) {
		return r.handleScheduledBackup(ctx, &schedule, cronSchedule, nextRun, log)
	}

	return r.scheduleNextRun(ctx, &schedule, nextRun, log)
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

	// Check if backup already exists (race condition protection)
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
		log.Errorw("failed to create backup", "error", err)
		return r.updateStatus(ctx, schedule, "Failed", fmt.Sprintf("Failed to create backup: %v", err), nil, &nextRunMeta)
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

	// Deep copy schedule for goroutine to avoid race condition
	scheduleCopy := schedule.DeepCopy()
	go r.runRetentionCleanup(context.Background(), scheduleCopy, log) //nolint:gosec // intentional: goroutine outlives request ctx

	nextRunMeta := metav1.NewTime(nextRun)
	return r.updateStatusWithRequeue(ctx, schedule, "Active", "", nil, &nextRunMeta, requeueAfter)
}

// generateBackupName creates a deterministic backup name based on scheduled time.
// Format: {schedule-name}-{YYYYMMDD-HHMM} to prevent race conditions.
func (r *BackupScheduleReconciler) generateBackupName(schedule *dbtether.BackupSchedule, scheduledTime time.Time) string {
	// Use scheduled time as suffix (deterministic for same schedule time)
	timeSuffix := scheduledTime.UTC().Format("20060102-1504")
	return fmt.Sprintf("%s-%s", schedule.Name, timeSuffix)
}

func (r *BackupScheduleReconciler) createBackup(ctx context.Context, schedule *dbtether.BackupSchedule, backupName string) (*dbtether.Backup, error) {
	backup := &dbtether.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupName,
			Namespace: schedule.Namespace,
			Labels: map[string]string{
				LabelScheduleName: schedule.Name,
				LabelScheduleNS:   schedule.Namespace,
			},
		},
		Spec: dbtether.BackupSpec{
			DatabaseRef: schedule.Spec.DatabaseRef,
			StorageRef:  schedule.Spec.StorageRef,
		},
	}

	// Inherit filename template if specified
	if schedule.Spec.FilenameTemplate != "" {
		backup.Spec.FilenameTemplate = schedule.Spec.FilenameTemplate
	}

	// Inherit job config if specified
	if schedule.Spec.JobConfig != nil {
		backup.Spec.JobConfig = schedule.Spec.JobConfig
	}

	// Set owner reference for garbage collection
	if err := controllerutil.SetControllerReference(schedule, backup, r.Scheme); err != nil {
		return nil, fmt.Errorf("failed to set owner reference: %w", err)
	}

	if err := r.Create(ctx, backup); err != nil {
		return nil, fmt.Errorf("failed to create backup: %w", err)
	}

	return backup, nil
}

func (r *BackupScheduleReconciler) runRetentionCleanup(ctx context.Context, schedule *dbtether.BackupSchedule, log *zap.SugaredLogger) {
	if schedule.Spec.Retention == nil {
		return // No retention policy configured
	}

	// Check debounce - skip if cleanup was run recently
	if !r.shouldRunRetentionCleanup(schedule) {
		log.Debugw("retention cleanup: skipping (debounce)", "schedule", schedule.Name)
		return
	}

	// Update annotation to mark cleanup start (prevent parallel runs)
	if err := r.updateRetentionCleanupAnnotation(ctx, schedule); err != nil {
		log.Debugw("retention cleanup: failed to update annotation (concurrent cleanup)", "error", err)
		return
	}

	// Get resources needed for cleanup
	storageClient, prefix, err := r.prepareRetentionCleanup(ctx, schedule, log)
	if err != nil || storageClient == nil {
		return // Error already logged
	}
	if closer, ok := storageClient.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	// Apply retention policy and delete old files
	r.executeRetentionCleanup(ctx, storageClient, prefix, schedule, log)

	// Also cleanup old Backup CRDs
	r.cleanupBackupCRDs(ctx, schedule, log)
}

func (r *BackupScheduleReconciler) prepareRetentionCleanup(ctx context.Context, schedule *dbtether.BackupSchedule, log *zap.SugaredLogger) (storage.StorageClient, string, error) {
	var db dbtether.Database
	if err := r.Get(ctx, types.NamespacedName{
		Name:      schedule.Spec.DatabaseRef.Name,
		Namespace: schedule.Namespace,
	}, &db); err != nil {
		if !errors.IsNotFound(err) {
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

	storageClient, err := r.newRetentionClient(ctx, &backupStorage)
	if err != nil {
		log.Warnw("retention cleanup: failed to create storage client", "provider", backupStorage.GetProvider(), "error", err)
		return nil, "", err
	}

	prefix, err := r.buildStoragePath(&backupStorage, &cluster, &db)
	if err != nil {
		log.Warnw("retention cleanup: failed to build storage path", "error", err)
		return nil, "", err
	}

	return storageClient, prefix, nil
}

func (r *BackupScheduleReconciler) newRetentionClient(ctx context.Context, bs *dbtether.BackupStorage) (storage.StorageClient, error) {
	var s3 *storage.S3Config
	var gcs *storage.GCSConfig
	var az *storage.AzureConfig
	switch {
	case bs.Spec.S3 != nil:
		s3 = &storage.S3Config{Bucket: bs.Spec.S3.Bucket, Region: bs.Spec.S3.Region, Endpoint: bs.Spec.S3.Endpoint}
	case bs.Spec.GCS != nil:
		gcs = &storage.GCSConfig{Bucket: bs.Spec.GCS.Bucket, Project: bs.Spec.GCS.Project}
	case bs.Spec.Azure != nil:
		az = &storage.AzureConfig{Container: bs.Spec.Azure.Container, StorageAccount: bs.Spec.Azure.StorageAccount}
	}
	return storage.NewClient(ctx, s3, gcs, az, slog.Default())
}

func (r *BackupScheduleReconciler) executeRetentionCleanup(ctx context.Context, storageClient storage.StorageClient, prefix string, schedule *dbtether.BackupSchedule, log *zap.SugaredLogger) {
	filenameFilter, err := pkgbackup.FilenameTemplateToRegex(schedule.Spec.FilenameTemplate)
	if err != nil {
		log.Warnw("retention cleanup: failed to build filename filter, proceeding without filter", "error", err)
	}

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

// cleanupBackupCRDs deletes old Backup CRDs based on retention policy.
// Uses the same retention logic as S3 cleanup (keepLast, keepDaily, keepWeekly, keepMonthly).
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

	// List all Backup CRDs created by this schedule
	var backups dbtether.BackupList
	if err := r.List(ctx, &backups,
		client.InNamespace(schedule.Namespace),
		client.MatchingLabels{LabelScheduleName: schedule.Name},
	); err != nil {
		log.Warnw("retention cleanup: failed to list backup CRDs", "error", err)
		return
	}

	if len(backups.Items) == 0 {
		return
	}

	// Sort by creation time (newest first)
	sort.Slice(backups.Items, func(i, j int) bool {
		return backups.Items[i].CreationTimestamp.After(backups.Items[j].CreationTimestamp.Time)
	})

	// Calculate which backups to keep using same logic as S3 retention
	keepSet := r.calculateBackupKeepSet(backups.Items, schedule.Spec.Retention, time.Now())

	if len(keepSet) == len(backups.Items) {
		return
	}

	// Delete CRDs not in keepSet (only completed ones)
	deleted := 0
	for i := range backups.Items {
		backup := &backups.Items[i]

		if keepSet[backup.Name] {
			continue
		}

		// Only delete completed backups
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

// calculateBackupKeepSet determines which Backup CRDs to keep based on retention policy.
// Mirrors the logic in pkg/backup/retention.go for S3 files.
//
//nolint:gocyclo // retention policy logic requires checking multiple conditions
func (r *BackupScheduleReconciler) calculateBackupKeepSet(
	backups []dbtether.Backup,
	policy *dbtether.RetentionPolicy,
	now time.Time,
) map[string]bool {
	keep := make(map[string]bool)

	// keepLast: keep the N most recent backups
	if policy.KeepLast != nil && *policy.KeepLast > 0 {
		for i := 0; i < *policy.KeepLast && i < len(backups); i++ {
			keep[backups[i].Name] = true //nolint:gosec // i is bounded by len(backups)
		}
	}

	// keepDaily: keep first backup of each day for N days
	if policy.KeepDaily != nil && *policy.KeepDaily > 0 {
		cutoff := now.AddDate(0, 0, -*policy.KeepDaily)
		seenDays := make(map[string]bool)
		for i := range backups {
			ts := backups[i].CreationTimestamp.Time
			if ts.Before(cutoff) {
				continue
			}
			dayKey := ts.Format("2006-01-02")
			if !seenDays[dayKey] {
				seenDays[dayKey] = true
				keep[backups[i].Name] = true
			}
		}
	}

	// keepWeekly: keep first backup of each week for N weeks
	if policy.KeepWeekly != nil && *policy.KeepWeekly > 0 {
		cutoff := now.AddDate(0, 0, -*policy.KeepWeekly*7)
		seenWeeks := make(map[string]bool)
		for i := range backups {
			ts := backups[i].CreationTimestamp.Time
			if ts.Before(cutoff) {
				continue
			}
			year, week := ts.ISOWeek()
			weekKey := fmt.Sprintf("%d-W%02d", year, week)
			if !seenWeeks[weekKey] {
				seenWeeks[weekKey] = true
				keep[backups[i].Name] = true
			}
		}
	}

	// keepMonthly: keep first backup of each month for N months
	if policy.KeepMonthly != nil && *policy.KeepMonthly > 0 {
		cutoff := now.AddDate(0, -*policy.KeepMonthly, 0)
		seenMonths := make(map[string]bool)
		for i := range backups {
			ts := backups[i].CreationTimestamp.Time
			if ts.Before(cutoff) {
				continue
			}
			monthKey := ts.Format("2006-01")
			if !seenMonths[monthKey] {
				seenMonths[monthKey] = true
				keep[backups[i].Name] = true
			}
		}
	}

	return keep
}

// shouldRunRetentionCleanup checks if enough time has passed since last cleanup
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
		return true // Invalid format, allow cleanup
	}

	return time.Since(lastCleanup) >= RetentionCleanupDebounce
}

// updateRetentionCleanupAnnotation sets the last cleanup timestamp
func (r *BackupScheduleReconciler) updateRetentionCleanupAnnotation(ctx context.Context, schedule *dbtether.BackupSchedule) error {
	// Re-fetch to get latest version (avoid conflicts)
	var current dbtether.BackupSchedule
	if err := r.Get(ctx, client.ObjectKeyFromObject(schedule), &current); err != nil {
		return err
	}

	// Double-check debounce with fresh data
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

// buildStoragePath builds the S3 prefix from the path template
func (r *BackupScheduleReconciler) buildStoragePath(
	backupStorage *dbtether.BackupStorage,
	cluster *dbtether.DBCluster,
	db *dbtether.Database,
) (string, error) {
	pathTemplate := backupStorage.Spec.PathTemplate
	if pathTemplate == "" {
		// Default: ClusterName/DatabaseName
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
		// Cleanup: delete all Backup CRDs created by this schedule
		var backups dbtether.BackupList
		if err := r.List(ctx, &backups,
			client.InNamespace(schedule.Namespace),
			client.MatchingLabels{LabelScheduleName: schedule.Name},
		); err != nil {
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
	patch := client.MergeFrom(schedule.DeepCopy())

	schedule.Status.Phase = phase
	schedule.Status.Message = message
	schedule.Status.ObservedGeneration = schedule.Generation

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

func (r *BackupScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&dbtether.BackupSchedule{}).
		Owns(&dbtether.Backup{}).
		Complete(r)
}
