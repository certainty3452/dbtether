package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

const FinalizerName = "dbtether.io/finalizer"

type DatabaseReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	PGClientCache postgres.ClientCacheInterface
}

func DatabaseNameFor(db *databasesv1alpha1.Database) string {
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

	log.FromContext(ctx).Info("reconciling", "database", DatabaseNameFor(&db))

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
	pgClient, err := GetPostgresClient(ctx, r.Client, r.PGClientCache, cluster)
	if err != nil {
		return r.setStatusWithRequeue(ctx, db, "Failed", fmt.Sprintf("connection error: %s", err.Error()), 60*time.Second)
	}

	if err := r.ensureCreatingStatus(ctx, db); err != nil {
		return ctrl.Result{}, err
	}

	ownershipTracked, step, applyErr := ApplyDatabaseSpec(ctx, pgClient, DatabaseNameFor(db), db)
	if applyErr != nil && step == DatabaseStepOwnership {
		return r.handleDatabaseError(ctx, db, step, applyErr)
	}

	if !ownershipTracked && (db.Status.OwnershipTracked == nil || *db.Status.OwnershipTracked) {
		log.FromContext(ctx).Info("WARNING: database ownership tracking not available (legacy database not owned by operator's PostgreSQL user). "+
			"Multiple Database CRDs may reference this database without conflict detection. "+
			"To enable tracking, change PostgreSQL owner: ALTER DATABASE <name> OWNER TO <operator_user>",
			"database", DatabaseNameFor(db))
	}
	if err := r.persistOwnershipTracked(ctx, db, ownershipTracked); err != nil {
		return ctrl.Result{}, err
	}

	// Parking in Failed would keep every DatabaseUser of this database waiting for the resync.
	if applyErr != nil {
		return r.handleDatabaseError(ctx, db, step, applyErr)
	}

	log.FromContext(ctx).Info("database ready", "database", DatabaseNameFor(db))
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

type DatabaseSpecStep string

const (
	DatabaseStepOwnership     DatabaseSpecStep = "ownership"
	DatabaseStepPublicConnect DatabaseSpecStep = "public connect"
	DatabaseStepExtensions    DatabaseSpecStep = "extensions"
)

func ApplyDatabaseSpec(ctx context.Context, pgClient postgres.ClientInterface, databaseName string, db *databasesv1alpha1.Database) (ownershipTracked bool, failedStep DatabaseSpecStep, err error) {
	forceAdopt := db.Annotations[forceAdoptAnnotation] == "true"

	ownershipTracked, err = pgClient.EnsureDatabaseWithOwner(ctx, databaseName, db.Namespace, db.Name, forceAdopt)
	if err != nil {
		return false, DatabaseStepOwnership, err
	}

	if db.Spec.RevokePublicConnect {
		if err := pgClient.RevokePublicConnect(ctx, databaseName); err != nil {
			return ownershipTracked, DatabaseStepPublicConnect, err
		}
	}

	if len(db.Spec.Extensions) > 0 {
		if err := pgClient.EnsureExtensions(ctx, databaseName, db.Spec.Extensions); err != nil {
			return ownershipTracked, DatabaseStepExtensions, err
		}
	}

	return ownershipTracked, "", nil
}

func databaseSpecFailureMessage(step DatabaseSpecStep, err error) string {
	switch step {
	case DatabaseStepPublicConnect:
		return fmt.Sprintf("failed to revoke public connect: %s", err.Error())
	case DatabaseStepExtensions:
		return fmt.Sprintf("failed to create extensions: %s", err.Error())
	default:
		return fmt.Sprintf("failed to create database: %s", err.Error())
	}
}

func (r *DatabaseReconciler) handleDatabaseError(ctx context.Context, db *databasesv1alpha1.Database, step DatabaseSpecStep, err error) (ctrl.Result, error) {
	return r.setStatusWithRequeue(ctx, db, "Failed",
		fmt.Sprintf("transient error (will retry): %s", databaseSpecFailureMessage(step, err)), 60*time.Second)
}

func (r *DatabaseReconciler) handleDeletion(ctx context.Context, db *databasesv1alpha1.Database) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(db, FinalizerName) {
		return ctrl.Result{}, nil
	}

	logger := log.FromContext(ctx)
	logger.Info("handling deletion", "database", DatabaseNameFor(db), "policy", db.Spec.DeletionPolicy)

	if _, err := r.setStatus(ctx, db, "Deleting", "deleting database..."); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.releaseDatabase(ctx, db); err != nil {
		// The CR must not disappear while a database it was meant to drop still exists (e.g. Postgres unreachable).
		if db.Spec.DeletionPolicy == "Delete" {
			return ctrl.Result{}, err
		}
		logger.Error(err, "failed to clear database ownership during retention")
	}

	patch := client.MergeFrom(db.DeepCopy())
	controllerutil.RemoveFinalizer(db, FinalizerName)
	return ctrl.Result{}, r.Patch(ctx, db, patch)
}

// Only a database whose ownership marker names this CR is dropped or released; any other one belongs to someone else.
func (r *DatabaseReconciler) releaseDatabase(ctx context.Context, db *databasesv1alpha1.Database) error {
	logger := log.FromContext(ctx)

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("cluster not found, leaving the database untouched")
			return nil
		}
		return err
	}

	pgClient, err := GetPostgresClient(ctx, r.Client, r.PGClientCache, &cluster)
	if err != nil {
		return fmt.Errorf("failed to get postgres client: %w", err)
	}

	dbName := DatabaseNameFor(db)
	ownerNamespace, ownerName, err := pgClient.GetDatabaseOwner(ctx, dbName)
	if err != nil {
		return err
	}
	if ownerNamespace != db.Namespace || ownerName != db.Name {
		logger.Info("database is not marked as owned by this resource, leaving it untouched",
			"database", dbName, "markedOwner", ownerNamespace+"/"+ownerName)
		return nil
	}

	if db.Spec.DeletionPolicy == "Delete" {
		logger.Info("dropping database", "database", dbName)
		return pgClient.DropDatabase(ctx, dbName)
	}
	logger.Info("clearing database ownership for re-adoption", "database", dbName)
	return pgClient.ClearDatabaseOwner(ctx, dbName)
}

func (r *DatabaseReconciler) setStatus(ctx context.Context, db *databasesv1alpha1.Database, phase, message string) (ctrl.Result, error) {
	patch := client.MergeFrom(db.DeepCopy())

	if phase == "Pending" || phase == "Waiting" {
		now := metav1.Now()
		if db.Status.PendingSince == nil {
			db.Status.PendingSince = &now
		} else if now.Sub(db.Status.PendingSince.Time) > PendingTimeout {
			phase = "Failed"
			message = fmt.Sprintf("timeout: %s (pending for over 10 minutes)", message)
			// Reset so a later Pending episode does not time out immediately.
			db.Status.PendingSince = nil
		}
	} else {
		db.Status.PendingSince = nil
	}

	db.Status.Phase = phase
	db.Status.Message = message
	db.Status.ObservedGeneration = db.Generation
	db.Status.DatabaseName = DatabaseNameFor(db)

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

// The warn-once guard needs OwnershipTracked persisted across reconciles.
func (r *DatabaseReconciler) persistOwnershipTracked(ctx context.Context, db *databasesv1alpha1.Database, tracked bool) error {
	if db.Status.OwnershipTracked != nil && *db.Status.OwnershipTracked == tracked {
		return nil
	}
	patch := client.MergeFrom(db.DeepCopy())
	db.Status.OwnershipTracked = &tracked
	return r.Status().Patch(ctx, db, patch)
}

func (r *DatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.Database{}).
		Watches(&databasesv1alpha1.DBCluster{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueClusterDatabases),
			builder.WithPredicates(dbClusterChanged)).
		Complete(r)
}

// Spec edits and connectivity transitions only: status patches of a steady cluster must not fan out.
var dbClusterChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		oldCluster, okOld := e.ObjectOld.(*databasesv1alpha1.DBCluster)
		newCluster, okNew := e.ObjectNew.(*databasesv1alpha1.DBCluster)
		if !okOld || !okNew {
			return false
		}
		return oldCluster.Generation != newCluster.Generation || oldCluster.Status.Phase != newCluster.Status.Phase
	},
}

func (r *DatabaseReconciler) enqueueClusterDatabases(ctx context.Context, obj client.Object) []ctrl.Request {
	databases, err := databasesOnCluster(ctx, r.Client, obj.GetName())
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to enqueue Databases of cluster", "cluster", obj.GetName())
		return nil
	}
	requests := make([]ctrl.Request, 0, len(databases))
	for i := range databases {
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: databases[i].Namespace, Name: databases[i].Name}})
	}
	return requests
}

func databasesOnCluster(ctx context.Context, c client.Reader, clusterName string) ([]databasesv1alpha1.Database, error) {
	var all databasesv1alpha1.DatabaseList
	if err := c.List(ctx, &all); err != nil {
		return nil, fmt.Errorf("failed to list Databases: %w", err)
	}
	var onCluster []databasesv1alpha1.Database
	for i := range all.Items {
		if all.Items[i].Spec.ClusterRef.Name == clusterName {
			onCluster = append(onCluster, all.Items[i])
		}
	}
	return onCluster, nil
}
