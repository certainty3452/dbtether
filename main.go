package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/controllers"
	"github.com/certainty3452/dbtether/controllers/backup"
	backuppkg "github.com/certainty3452/dbtether/pkg/backup"
	"github.com/certainty3452/dbtether/pkg/config"
	"github.com/certainty3452/dbtether/pkg/postgres"
	"github.com/certainty3452/dbtether/pkg/storage"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

const (
	errUnableToCreateController = "unable to create controller"
	terminationLogPath          = "/dev/termination-log"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(databasesv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var mode string
	var operatorNamespace string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for controller manager.")
	flag.StringVar(&mode, "mode", "controller", "Run mode: controller (default) or job")
	flag.StringVar(&operatorNamespace, "namespace", "dbtether", "Namespace for backup Jobs")

	opts := ctrlzap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(ctrlzap.New(ctrlzap.UseFlagOptions(&opts)))

	switch mode {
	case "job":
		if err := runBackupJobMode(context.Background()); err != nil {
			writeTerminationMessage(terminationLogPath, err)
			os.Exit(1)
		}
		return
	case "restore":
		if err := runRestoreJobMode(context.Background()); err != nil {
			writeTerminationMessage(terminationLogPath, err)
			os.Exit(1)
		}
		return
	default:
		serviceAccount, err := operatorServiceAccountFromEnv()
		if err != nil {
			setupLog.Error(err, "invalid operator configuration")
			os.Exit(1)
		}
		runController(metricsAddr, probeAddr, enableLeaderElection, operatorNamespace, serviceAccount)
	}
}

func operatorServiceAccountFromEnv() (string, error) {
	serviceAccount := os.Getenv("OPERATOR_SERVICE_ACCOUNT")
	if serviceAccount == "" {
		return "", fmt.Errorf("OPERATOR_SERVICE_ACCOUNT must be set: backup and restore Jobs run under the operator's ServiceAccount")
	}
	return serviceAccount, nil
}

func runController(metricsAddr, probeAddr string, enableLeaderElection bool, operatorNamespace, serviceAccount string) {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "dbtether.io",
		// Backup and restore Jobs, and the Pods behind them, only ever exist here.
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Pod{}:  {Namespaces: map[string]cache.Config{operatorNamespace: {}}},
				&batchv1.Job{}: {Namespaces: map[string]cache.Config{operatorNamespace: {}}},
			},
		},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	pgClientCache := postgres.NewClientCache()

	setupMainControllers(mgr, pgClientCache)
	setupBackupControllers(mgr, operatorNamespace, serviceAccount, pgClientCache)
	setupHealthChecks(mgr)

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func setupMainControllers(mgr ctrl.Manager, pgClientCache postgres.ClientCacheInterface) {
	if err := (&controllers.DBClusterReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PGClientCache: pgClientCache,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, errUnableToCreateController, "controller", "DBCluster")
		os.Exit(1)
	}

	if err := (&controllers.DatabaseReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PGClientCache: pgClientCache,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, errUnableToCreateController, "controller", "Database")
		os.Exit(1)
	}

	if err := (&controllers.DatabaseUserReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PGClientCache: pgClientCache,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, errUnableToCreateController, "controller", "DatabaseUser")
		os.Exit(1)
	}
}

func setupBackupControllers(mgr ctrl.Manager, operatorNamespace, serviceAccount string, pgClientCache postgres.ClientCacheInterface) {
	if err := (&backup.BackupStorageReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, errUnableToCreateController, "controller", "BackupStorage")
		os.Exit(1)
	}

	operatorImage := os.Getenv("OPERATOR_IMAGE")
	if operatorImage == "" {
		operatorImage = "ghcr.io/certainty3452/dbtether:latest"
	}
	operatorSSLMode := controllers.OperatorSSLMode()

	cfg := config.LoadOrDefault(config.DefaultConfigPath)
	setupLog.Info("loaded configuration",
		"maxConcurrentPerCluster", cfg.Backup.MaxConcurrentPerCluster,
		"podAnnotations", cfg.Backup.PodAnnotations,
	)

	if err := (&backup.BackupReconciler{
		Client:               mgr.GetClient(),
		APIReader:            mgr.GetAPIReader(),
		Scheme:               mgr.GetScheme(),
		Recorder:             mgr.GetEventRecorderFor("backup-controller"),
		Image:                operatorImage,
		Namespace:            operatorNamespace,
		ServiceAccountName:   serviceAccount,
		SSLMode:              operatorSSLMode,
		MaxConcurrentBackups: cfg.Backup.MaxConcurrentPerCluster,
		PodAnnotations:       cfg.Backup.PodAnnotations,
		PodLabels:            cfg.Backup.PodLabels,
		JobLabels:            cfg.Backup.JobLabels,
		PodResources:         cfg.Backup.Resources.ToK8sResources(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, errUnableToCreateController, "controller", "Backup")
		os.Exit(1)
	}

	sugarLog, _ := zap.NewDevelopment()
	if err := (&backup.BackupScheduleReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Log:       sugarLog.Sugar(),
		Namespace: operatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, errUnableToCreateController, "controller", "BackupSchedule")
		os.Exit(1)
	}

	if err := (&backup.RestoreReconciler{
		Client:             mgr.GetClient(),
		APIReader:          mgr.GetAPIReader(),
		Scheme:             mgr.GetScheme(),
		Recorder:           mgr.GetEventRecorderFor("restore-controller"),
		Image:              operatorImage,
		Namespace:          operatorNamespace,
		ServiceAccountName: serviceAccount,
		SSLMode:            operatorSSLMode,
		PodResources:       cfg.Backup.Resources.ToK8sResources(),
		PGClientCache:      pgClientCache,
		MaxConcurrentJobs:  cfg.Backup.MaxConcurrentPerCluster,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, errUnableToCreateController, "controller", "Restore")
		os.Exit(1)
	}
}

func setupHealthChecks(mgr ctrl.Manager) {
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}
}

type backupRunner func(context.Context, *backuppkg.BackupConfig) (*backuppkg.BackupResult, error)

type backupResultRecorder func(context.Context, *backuppkg.BackupConfig, *backuppkg.BackupResult) error

type backupStartRecorder func(context.Context, *backuppkg.BackupConfig) error

func backupConfigFromEnv() *backuppkg.BackupConfig {
	cfg := &backuppkg.BackupConfig{
		Host:     getEnvRequired("DB_HOST"),
		Port:     getEnvInt("DB_PORT", 5432),
		Database: getEnvRequired("DB_NAME"),
		Username: getEnvRequired("DB_USER"),
		Password: getEnvRequired("DB_PASSWORD"),
		SSLMode:  getEnv("DB_SSLMODE", "require"),

		StorageType: getEnvRequired("STORAGE_TYPE"),

		PathTemplate:     getEnv("PATH_TEMPLATE", "{{ .ClusterName }}/{{ .DatabaseName }}"),
		FilenameTemplate: getEnv("FILENAME_TEMPLATE", backuppkg.DefaultFilenameTemplate),

		ClusterName:  getEnvRequired("CLUSTER_NAME"),
		DatabaseName: getEnvRequired("DATABASE_NAME"),
		BackupName:   getEnvRequired("BACKUP_NAME"),
		Namespace:    getEnvRequired("BACKUP_NAMESPACE"),
		RunID:        getEnvRequired("RUN_ID"),
	}

	switch cfg.StorageType {
	case "s3":
		cfg.S3Config = s3ConfigFromEnv()
	case "gcs":
		cfg.GCSConfig = gcsConfigFromEnv()
	case "azure":
		cfg.AzureConfig = azureConfigFromEnv()
	}
	return cfg
}

func runBackupJobMode(ctx context.Context) error {
	cfg := backupConfigFromEnv()
	c, err := newJobClient()
	if err != nil {
		return err
	}
	recordStart := func(ctx context.Context, cfg *backuppkg.BackupConfig) error {
		return recordBackupStart(ctx, c, cfg)
	}
	recordResult := func(ctx context.Context, cfg *backuppkg.BackupConfig, result *backuppkg.BackupResult) error {
		return recordBackupResult(ctx, c, cfg, result)
	}
	return runBackupJob(ctx, cfg, recordStart, backuppkg.RunBackup, recordResult)
}

func runBackupJob(ctx context.Context, cfg *backuppkg.BackupConfig, recordStart backupStartRecorder, run backupRunner, record backupResultRecorder) error {
	setupLog.Info("starting backup job",
		"database", cfg.Database,
		"storage", cfg.StorageType,
		"cluster", cfg.ClusterName,
		"runId", cfg.RunID,
	)

	// The operator re-creates a missing Job only while its run shows no sign of a Job, so this must precede the dump.
	if err := recordStart(ctx, cfg); err != nil {
		err = fmt.Errorf("recording the start of run %s on Backup %s/%s failed: %w", cfg.RunID, cfg.Namespace, cfg.BackupName, err)
		setupLog.Error(err, "backup not started")
		return err
	}

	result, err := run(ctx, cfg)
	if err != nil {
		setupLog.Error(err, "backup failed")
		return err
	}

	setupLog.Info("backup uploaded",
		"path", result.Path,
		"size", formatBytes(result.Size),
		"uncompressedSize", formatBytes(result.UncompressedSize),
		"duration", result.Duration.Round(time.Millisecond).String(),
		"compressionRatio", fmt.Sprintf("%.1f%%", float64(result.Size)/float64(result.UncompressedSize)*100),
	)

	// A Backup without its path cannot be restored from, so an unrecorded upload must fail the run.
	if err := record(ctx, cfg, result); err != nil {
		err = fmt.Errorf("backup uploaded to %s but recording it on Backup %s/%s failed: %w", result.Path, cfg.Namespace, cfg.BackupName, err)
		setupLog.Error(err, "backup failed")
		return err
	}
	return nil
}

type restoreRunner func(context.Context, *backuppkg.RestoreConfig) error

type restoreRecorder func(context.Context, restoreTarget) error

type restoreTarget struct {
	Key   types.NamespacedName
	RunID string
}

func restoreConfigFromEnv() (*backuppkg.RestoreConfig, restoreTarget) {
	cfg := &backuppkg.RestoreConfig{
		Host:     getEnvRequired("DB_HOST"),
		Port:     getEnvInt("DB_PORT", 5432),
		Database: getEnvRequired("DB_NAME"),
		Username: getEnvRequired("DB_USER"),
		Password: getEnvRequired("DB_PASSWORD"),
		SSLMode:  getEnv("DB_SSLMODE", "require"),

		SourcePath:  getEnvRequired("SOURCE_PATH"),
		StorageType: getEnvRequired("STORAGE_TYPE"),
		OnConflict:  getEnv("ON_CONFLICT", "fail"),
	}

	switch cfg.StorageType {
	case "s3":
		s3 := s3ConfigFromEnv()
		cfg.S3Config = &s3
	case "gcs":
		gcs := gcsConfigFromEnv()
		cfg.GCSConfig = &gcs
	case "azure":
		az := azureConfigFromEnv()
		cfg.AzureConfig = &az
	}

	target := restoreTarget{
		Key:   types.NamespacedName{Name: getEnvRequired("RESTORE_NAME"), Namespace: getEnvRequired("RESTORE_NAMESPACE")},
		RunID: getEnvRequired("RUN_ID"),
	}
	return cfg, target
}

func runRestoreJobMode(ctx context.Context) error {
	cfg, target := restoreConfigFromEnv()
	c, err := newJobClient()
	if err != nil {
		return err
	}
	recordStart := func(ctx context.Context, target restoreTarget) error {
		return recordRestoreStart(ctx, c, target)
	}
	recordResult := func(ctx context.Context, target restoreTarget) error {
		return recordRestoreResult(ctx, c, target)
	}
	return runRestoreJob(ctx, cfg, target, recordStart, backuppkg.RunRestore, recordResult)
}

func runRestoreJob(ctx context.Context, cfg *backuppkg.RestoreConfig, target restoreTarget,
	recordStart restoreRecorder, run restoreRunner, recordResult restoreRecorder) error {
	setupLog.Info("starting restore job",
		"database", cfg.Database,
		"source", cfg.SourcePath,
		"storage", cfg.StorageType,
		"onConflict", cfg.OnConflict,
		"runId", target.RunID,
	)

	// Until the start is recorded the operator may still create this run's Job anew, so the database stays untouched.
	if err := recordStart(ctx, target); err != nil {
		err = fmt.Errorf("recording the start of run %s on Restore %s failed: %w", target.RunID, target.Key, err)
		setupLog.Error(err, "restore not started")
		return err
	}

	if err := run(ctx, cfg); err != nil {
		setupLog.Error(err, "restore failed")
		return err
	}

	setupLog.Info("restore completed successfully",
		"database", cfg.Database,
		"source", cfg.SourcePath,
	)

	// The operator must still re-apply grants, and once the Job is gone this record is its only evidence of success.
	if err := recordResult(ctx, target); err != nil {
		err = fmt.Errorf("restored %s into database %s but recording it on Restore %s failed: %w", cfg.SourcePath, cfg.Database, target.Key, err)
		setupLog.Error(err, "restore failed")
		return err
	}
	return nil
}

func s3ConfigFromEnv() storage.S3Config {
	return storage.S3Config{
		Bucket:    os.Getenv("S3_BUCKET"),
		Region:    os.Getenv("S3_REGION"),
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
}

func gcsConfigFromEnv() storage.GCSConfig {
	return storage.GCSConfig{
		Bucket:  os.Getenv("GCS_BUCKET"),
		Project: os.Getenv("GCS_PROJECT"),
	}
}

func azureConfigFromEnv() storage.AzureConfig {
	return storage.AzureConfig{
		Container:      os.Getenv("AZURE_CONTAINER"),
		StorageAccount: os.Getenv("AZURE_ACCOUNT"),
	}
}

func writeTerminationMessage(path string, cause error) {
	if err := os.WriteFile(path, []byte(cause.Error()), 0o600); err != nil {
		setupLog.Error(err, "failed to write termination message")
	}
}

func getEnvRequired(key string) string {
	val := os.Getenv(key)
	if val == "" {
		setupLog.Error(nil, "required environment variable not set", "key", key)
		os.Exit(1)
	}
	return val
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	i, err := strconv.Atoi(val)
	if err != nil || i <= 0 {
		return defaultVal
	}
	return i
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

var errRunSuperseded = errors.New("run superseded")

func recordBackupStart(ctx context.Context, c client.Client, cfg *backuppkg.BackupConfig) error {
	return patchOwnRun(ctx, c, backupKey(cfg), cfg.RunID, backupRunID, func(b *databasesv1alpha1.Backup) {
		if b.Status.StartedAt == nil {
			now := metav1.Now()
			b.Status.StartedAt = &now
		}
	})
}

func recordBackupResult(ctx context.Context, c client.Client, cfg *backuppkg.BackupConfig, result *backuppkg.BackupResult) error {
	return patchOwnRun(ctx, c, backupKey(cfg), cfg.RunID, backupRunID, func(b *databasesv1alpha1.Backup) {
		b.Status.Path = result.Path
		b.Status.Size = formatBytes(result.Size)
		b.Status.Duration = result.Duration.Round(time.Millisecond).String()
	})
}

func recordRestoreStart(ctx context.Context, c client.Client, target restoreTarget) error {
	return patchOwnRun(ctx, c, target.Key, target.RunID, restoreRunID, func(restore *databasesv1alpha1.Restore) {
		if restore.Status.StartedAt == nil {
			now := metav1.Now()
			restore.Status.StartedAt = &now
		}
	})
}

func recordRestoreResult(ctx context.Context, c client.Client, target restoreTarget) error {
	return patchOwnRun(ctx, c, target.Key, target.RunID, restoreRunID, func(restore *databasesv1alpha1.Restore) {
		now := metav1.Now()
		restore.Status.JobSucceededAt = &now
	})
}

func backupKey(cfg *backuppkg.BackupConfig) types.NamespacedName {
	return types.NamespacedName{Name: cfg.BackupName, Namespace: cfg.Namespace}
}

func backupRunID(b *databasesv1alpha1.Backup) string { return b.Status.RunID }

func restoreRunID(restore *databasesv1alpha1.Restore) string { return restore.Status.RunID }

// The resourceVersion precondition stops a write landing on a run that replaced this one after the RunID check.
func patchOwnRun[T any, P interface {
	*T
	client.Object
}](ctx context.Context, c client.Client, key types.NamespacedName, runID string, currentRunID func(P) string, mutate func(P)) error {
	return retry.OnError(retry.DefaultBackoff, isRetryableRecordError, func() error {
		current := P(new(T))
		if err := c.Get(ctx, key, current); err != nil {
			return err
		}
		if got := currentRunID(current); got != runID {
			return fmt.Errorf("%w: %s now tracks run %q, not %q", errRunSuperseded, key, got, runID)
		}
		patch := client.MergeFromWithOptions(current.DeepCopyObject().(client.Object), client.MergeFromWithOptimisticLock{})
		mutate(current)
		return c.Status().Patch(ctx, current, patch)
	})
}

func newJobClient() (client.Client, error) {
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get kubernetes config: %w", err)
	}
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}
	return c, nil
}

func isRetryableRecordError(err error) bool {
	return !errors.Is(err, errRunSuperseded) && !apierrors.IsNotFound(err) && !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err)
}
