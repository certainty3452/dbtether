package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/controllers"
	"github.com/certainty3452/dbtether/controllers/backup"
	backuppkg "github.com/certainty3452/dbtether/pkg/backup"
	"github.com/certainty3452/dbtether/pkg/postgres"
	"github.com/certainty3452/dbtether/pkg/storage"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
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

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	if mode == "job" {
		runBackupJob()
		return
	}

	runController(metricsAddr, probeAddr, enableLeaderElection, operatorNamespace)
}

func runController(metricsAddr, probeAddr string, enableLeaderElection bool, operatorNamespace string) {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "dbtether.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	pgClientCache := postgres.NewClientCache()

	// Main controllers
	if err = (&controllers.DBClusterReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PGClientCache: pgClientCache,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DBCluster")
		os.Exit(1)
	}

	if err = (&controllers.DatabaseReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PGClientCache: pgClientCache,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Database")
		os.Exit(1)
	}

	if err = (&controllers.DatabaseUserReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PGClientCache: pgClientCache,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DatabaseUser")
		os.Exit(1)
	}

	// Backup controllers
	if err = (&backup.BackupStorageReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "BackupStorage")
		os.Exit(1)
	}

	// Get operator image from env (set by Helm)
	operatorImage := os.Getenv("OPERATOR_IMAGE")
	if operatorImage == "" {
		operatorImage = "certainty3452/dbtether:latest"
	}

	if err = (&backup.BackupReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		Image:     operatorImage,
		Namespace: operatorNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Backup")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}

	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func runBackupJob() {
	// All configuration comes from environment variables set by the controller
	cfg := backuppkg.BackupConfig{
		// Database connection
		Host:     getEnvRequired("DB_HOST"),
		Port:     getEnvInt("DB_PORT", 5432),
		Database: getEnvRequired("DB_NAME"),
		Username: getEnvRequired("DB_USER"),
		Password: getEnvRequired("DB_PASSWORD"),

		// Storage
		StorageType: getEnvRequired("STORAGE_TYPE"),
		S3Config: storage.S3Config{
			Bucket:    os.Getenv("S3_BUCKET"),
			Region:    os.Getenv("S3_REGION"),
			Endpoint:  os.Getenv("S3_ENDPOINT"),
			AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"),
			SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		},

		// Templates
		PathTemplate:     getEnv("PATH_TEMPLATE", "{{ .ClusterName }}/{{ .DatabaseName }}"),
		FilenameTemplate: getEnv("FILENAME_TEMPLATE", "{{ .Timestamp }}.sql.gz"),

		// Metadata for templates and tags
		ClusterName:  getEnvRequired("CLUSTER_NAME"),
		DatabaseName: getEnvRequired("DATABASE_NAME"),
		BackupName:   getEnv("BACKUP_NAME", ""),
		Namespace:    getEnv("BACKUP_NAMESPACE", ""),
		RunID:        getEnvRequired("RUN_ID"),
	}

	setupLog.Info("starting backup job",
		"database", cfg.Database,
		"storage", cfg.StorageType,
		"cluster", cfg.ClusterName,
	)

	ctx := context.Background()
	result, err := backuppkg.RunBackup(ctx, &cfg)
	if err != nil {
		setupLog.Error(err, "backup failed")
		os.Exit(1)
	}

	setupLog.Info("backup completed successfully",
		"path", result.Path,
		"size", formatBytes(result.Size),
		"uncompressedSize", formatBytes(result.UncompressedSize),
		"duration", result.Duration.Round(time.Millisecond).String(),
		"compressionRatio", fmt.Sprintf("%.1f%%", float64(result.Size)/float64(result.UncompressedSize)*100),
	)

	// Update Job annotations with results (for controller to read)
	if err := updateJobAnnotations(ctx, result); err != nil {
		setupLog.Error(err, "failed to update job annotations (non-fatal)")
		// Non-fatal: backup succeeded, just can't report details back
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

// formatBytes formats bytes as human-readable string
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

// updateJobAnnotations updates the Job with backup result annotations
func updateJobAnnotations(ctx context.Context, result *backuppkg.BackupResult) error {
	jobName := os.Getenv("JOB_NAME")
	jobNamespace := os.Getenv("JOB_NAMESPACE")
	if jobName == "" || jobNamespace == "" {
		return fmt.Errorf("JOB_NAME or JOB_NAMESPACE not set")
	}

	config, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("failed to get kubernetes config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	// Prepare patch with annotations
	patch := fmt.Sprintf(`{"metadata":{"annotations":{
		"dbtether.io/backup-path": %q,
		"dbtether.io/backup-size": "%d",
		"dbtether.io/backup-size-human": %q,
		"dbtether.io/backup-uncompressed-size": "%d",
		"dbtether.io/backup-duration": %q
	}}}`,
		result.Path,
		result.Size,
		formatBytes(result.Size),
		result.UncompressedSize,
		result.Duration.Round(time.Millisecond).String(),
	)

	_, err = clientset.BatchV1().Jobs(jobNamespace).Patch(
		ctx,
		jobName,
		types.MergePatchType,
		[]byte(patch),
		metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch job: %w", err)
	}

	return nil
}
