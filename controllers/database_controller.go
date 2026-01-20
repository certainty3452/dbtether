package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

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
	"github.com/certainty3452/dbtether/pkg/postgres"
)

const FinalizerName = "dbtether.io/finalizer"

type DatabaseReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	PGClientCache postgres.ClientCacheInterface
}

// +kubebuilder:rbac:groups=dbtether.io,resources=databases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=databases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=databases/finalizers,verbs=update
// +kubebuilder:rbac:groups=dbtether.io,resources=dbclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *DatabaseReconciler) getDatabaseName(db *databasesv1alpha1.Database) string {
	if db.Spec.DatabaseName != "" {
		return db.Spec.DatabaseName
	}
	return strings.ReplaceAll(db.Name, "-", "_")
}

func (r *DatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var db databasesv1alpha1.Database
	if err := r.Get(ctx, req.NamespacedName, &db); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	log.FromContext(ctx).Info("reconciling", "database", r.getDatabaseName(&db))

	if !db.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &db)
	}

	if result, err := r.ensureFinalizer(ctx, &db); result != nil || err != nil {
		return *result, err
	}

	cluster, result, err := r.getReadyCluster(ctx, &db)
	if result != nil || err != nil {
		return *result, err
	}

	return r.reconcileDatabase(ctx, &db, cluster)
}

func (r *DatabaseReconciler) ensureFinalizer(ctx context.Context, db *databasesv1alpha1.Database) (*ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(db, FinalizerName) {
		return nil, nil
	}

	controllerutil.AddFinalizer(db, FinalizerName)
	if err := r.Update(ctx, db); err != nil {
		return &ctrl.Result{}, err
	}
	return &ctrl.Result{}, nil
}

func (r *DatabaseReconciler) getReadyCluster(ctx context.Context, db *databasesv1alpha1.Database) (*databasesv1alpha1.DBCluster, *ctrl.Result, error) {
	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			result, err := r.setStatusWithRequeue(ctx, db, "Pending",
				fmt.Sprintf("waiting for DBCluster '%s'", db.Spec.ClusterRef.Name), 30*time.Second)
			return nil, &result, err
		}
		return nil, &ctrl.Result{}, err
	}

	if cluster.Status.Phase != "Connected" {
		result, err := r.setStatusWithRequeue(ctx, db, "Waiting",
			fmt.Sprintf("waiting for DBCluster '%s' to be connected", cluster.Name), 20*time.Second)
		return nil, &result, err
	}

	return &cluster, nil, nil
}

func (r *DatabaseReconciler) reconcileDatabase(ctx context.Context, db *databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster) (ctrl.Result, error) {
	pgClient, err := r.getPostgresClient(ctx, cluster)
	if err != nil {
		return r.setStatusWithRequeue(ctx, db, "Failed", fmt.Sprintf("connection error: %s", err.Error()), 60*time.Second)
	}

	if err := r.ensureCreatingStatus(ctx, db); err != nil {
		return ctrl.Result{}, err
	}

	ownershipTracked, err := r.ensureDatabase(ctx, db, pgClient)
	if err != nil {
		return r.handleDatabaseError(ctx, db, err)
	}

	// Log warning once if ownership tracking failed (legacy database not owned by operator)
	if !ownershipTracked && (db.Status.OwnershipTracked == nil || *db.Status.OwnershipTracked) {
		log.FromContext(ctx).Info("WARNING: database ownership tracking not available (legacy database not owned by operator's PostgreSQL user). "+
			"Multiple Database CRDs may reference this database without conflict detection. "+
			"To enable tracking, change PostgreSQL owner: ALTER DATABASE <name> OWNER TO <operator_user>",
			"database", r.getDatabaseName(db))
	}
	// Update ownership tracked status
	db.Status.OwnershipTracked = &ownershipTracked

	if err := r.ensureExtensions(ctx, db, pgClient); err != nil {
		return r.setStatus(ctx, db, "Failed", fmt.Sprintf("failed to create extensions: %s", err.Error()))
	}

	log.FromContext(ctx).Info("database ready", "database", r.getDatabaseName(db))
	return r.setStatus(ctx, db, "Ready", "database is ready")
}

func (r *DatabaseReconciler) ensureCreatingStatus(ctx context.Context, db *databasesv1alpha1.Database) error {
	if db.Status.Phase != "" && db.Status.Phase != "Pending" && db.Status.Phase != "Waiting" {
		return nil
	}
	_, err := r.setStatus(ctx, db, "Creating", "creating database...")
	return err
}

const forceAdoptAnnotation = "dbtether.io/force-adopt"

func (r *DatabaseReconciler) ensureDatabase(ctx context.Context, db *databasesv1alpha1.Database, pgClient postgres.ClientInterface) (ownershipTracked bool, err error) {
	dbName := r.getDatabaseName(db)

	// Check for force-adopt annotation
	forceAdopt := db.Annotations[forceAdoptAnnotation] == "true"

	// Use ownership tracking to prevent conflicts across namespaces
	ownershipTracked, err = pgClient.EnsureDatabaseWithOwner(ctx, dbName, db.Namespace, db.Name, forceAdopt)
	if err != nil {
		return false, err
	}

	if db.Spec.RevokePublicConnect {
		if err := pgClient.RevokePublicConnect(ctx, dbName); err != nil {
			log.FromContext(ctx).V(1).Info("failed to revoke public connect", "error", err.Error())
		}
	}

	return ownershipTracked, nil
}

func (r *DatabaseReconciler) ensureExtensions(ctx context.Context, db *databasesv1alpha1.Database, pgClient postgres.ClientInterface) error {
	if len(db.Spec.Extensions) == 0 {
		return nil
	}
	return pgClient.EnsureExtensions(ctx, r.getDatabaseName(db), db.Spec.Extensions)
}

func (r *DatabaseReconciler) handleDatabaseError(ctx context.Context, db *databasesv1alpha1.Database, err error) (ctrl.Result, error) {
	if postgres.IsTransientError(err) {
		return r.setStatusWithRequeue(ctx, db, "Failed",
			fmt.Sprintf("transient error (will retry): %s", err.Error()), 60*time.Second)
	}
	return r.setStatus(ctx, db, "Failed", fmt.Sprintf("failed to create database: %s", err.Error()))
}

func (r *DatabaseReconciler) handleDeletion(ctx context.Context, db *databasesv1alpha1.Database) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(db, FinalizerName) {
		return ctrl.Result{}, nil
	}

	logger := log.FromContext(ctx)
	logger.Info("handling deletion", "database", r.getDatabaseName(db), "policy", db.Spec.DeletionPolicy)

	if _, err := r.setStatus(ctx, db, "Deleting", "deleting database..."); err != nil {
		return ctrl.Result{}, err
	}

	if db.Spec.DeletionPolicy == "Delete" {
		if err := r.dropDatabaseIfPossible(ctx, db); err != nil {
			logger.Error(err, "failed to drop database during deletion")
		}
	} else {
		// Retain: clear ownership so database can be re-adopted
		if err := r.clearDatabaseOwnerIfPossible(ctx, db); err != nil {
			logger.Error(err, "failed to clear database ownership during retention")
		}
	}

	controllerutil.RemoveFinalizer(db, FinalizerName)
	return ctrl.Result{}, r.Update(ctx, db)
}

func (r *DatabaseReconciler) dropDatabaseIfPossible(ctx context.Context, db *databasesv1alpha1.Database) error {
	logger := log.FromContext(ctx)

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("cluster not found, skipping database drop")
			return nil
		}
		return err
	}

	pgClient, err := r.getPostgresClient(ctx, &cluster)
	if err != nil {
		return fmt.Errorf("failed to get postgres client: %w", err)
	}

	dbName := r.getDatabaseName(db)
	logger.Info("dropping database", "database", dbName)
	return pgClient.DropDatabase(ctx, dbName)
}

func (r *DatabaseReconciler) clearDatabaseOwnerIfPossible(ctx context.Context, db *databasesv1alpha1.Database) error {
	logger := log.FromContext(ctx)

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("cluster not found, skipping ownership clear")
			return nil
		}
		return err
	}

	pgClient, err := r.getPostgresClient(ctx, &cluster)
	if err != nil {
		return fmt.Errorf("failed to get postgres client: %w", err)
	}

	dbName := r.getDatabaseName(db)
	logger.Info("clearing database ownership for re-adoption", "database", dbName)
	return pgClient.ClearDatabaseOwner(ctx, dbName)
}

func (r *DatabaseReconciler) getPostgresClient(ctx context.Context, cluster *databasesv1alpha1.DBCluster) (postgres.ClientInterface, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
		Name:      cluster.Spec.CredentialsSecretRef.Name,
		Namespace: cluster.Spec.CredentialsSecretRef.Namespace,
	}, &secret); err != nil {
		return nil, fmt.Errorf("failed to get credentials secret: %w", err)
	}

	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	if username == "" || password == "" {
		return nil, fmt.Errorf("credentials secret must contain 'username' and 'password' keys")
	}

	return r.PGClientCache.Get(ctx, cluster.Name, postgres.Config{
		Host:     cluster.Spec.Endpoint,
		Port:     cluster.Spec.Port,
		Username: username,
		Password: password,
		Database: "postgres",
	})
}

func (r *DatabaseReconciler) setStatus(ctx context.Context, db *databasesv1alpha1.Database, phase, message string) (ctrl.Result, error) {
	patch := client.MergeFrom(db.DeepCopy())

	// Handle pending timeout: after 10 minutes, transition to Failed
	if phase == "Pending" || phase == "Waiting" {
		now := metav1.Now()
		if db.Status.PendingSince == nil {
			db.Status.PendingSince = &now
		} else if now.Sub(db.Status.PendingSince.Time) > PendingTimeout {
			phase = "Failed"
			message = fmt.Sprintf("timeout: %s (pending for over 10 minutes)", message)
		}
	} else {
		db.Status.PendingSince = nil
	}

	db.Status.Phase = phase
	db.Status.Message = message
	db.Status.ObservedGeneration = db.Generation
	db.Status.DatabaseName = r.getDatabaseName(db)

	if err := r.Status().Patch(ctx, db, patch); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *DatabaseReconciler) setStatusWithRequeue(ctx context.Context, db *databasesv1alpha1.Database, phase, message string, after time.Duration) (ctrl.Result, error) {
	if _, err := r.setStatus(ctx, db, phase, message); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func (r *DatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.Database{}).
		Complete(r)
}
