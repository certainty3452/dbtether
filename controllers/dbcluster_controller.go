package controllers

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

const HealthCheckInterval = 5 * time.Minute

type DBClusterReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	PGClientCache postgres.ClientCacheInterface
}

func (r *DBClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		if errors.IsNotFound(err) {
			r.PGClientCache.Remove(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.V(1).Info("reconciling", "endpoint", cluster.Spec.Endpoint)

	if cluster.Spec.CredentialsSecretRef != nil && cluster.Spec.CredentialsFromEnv != nil {
		logger.Info("both credentialsSecretRef and credentialsFromEnv specified, using credentialsFromEnv")
	}

	pgConfig, err := ClusterConfig(ctx, r.Client, &cluster)
	if err != nil {
		logger.Error(err, "failed to get credentials")
		return r.updateStatus(ctx, &cluster, "Failed", fmt.Sprintf("credentials error: %s", err.Error()), "")
	}

	pgClient, err := r.PGClientCache.Get(ctx, cluster.Name, pgConfig)
	if err != nil {
		logger.Error(err, "failed to connect")
		return r.updateStatus(ctx, &cluster, "Failed", fmt.Sprintf("connection failed: %s", err.Error()), "")
	}

	// The next Get replaces the client; evicting here could close a pool another reconcile just stored.
	if err := pgClient.Ping(ctx); err != nil {
		logger.Error(err, "ping failed")
		return r.updateStatus(ctx, &cluster, "Failed", fmt.Sprintf("ping failed: %s", err.Error()), "")
	}

	version, _ := pgClient.GetVersion(ctx)

	wasConnected := cluster.Status.Phase == "Connected"
	if _, err := r.updateStatus(ctx, &cluster, "Connected", "connection successful", version); err != nil {
		return ctrl.Result{}, err
	}

	if !wasConnected {
		logger.Info("connected", "version", version)
	}

	return ctrl.Result{RequeueAfter: HealthCheckInterval}, nil
}

func (r *DBClusterReconciler) updateStatus(ctx context.Context, cluster *databasesv1alpha1.DBCluster, phase, message, version string) (ctrl.Result, error) {
	statusChanged := cluster.Status.Phase != phase ||
		cluster.Status.Message != message ||
		cluster.Status.ObservedGeneration != cluster.Generation ||
		(version != "" && cluster.Status.PostgresVersion != version)

	if statusChanged {
		patch := client.MergeFrom(cluster.DeepCopy())
		cluster.Status.Phase = phase
		cluster.Status.Message = message
		cluster.Status.LastCheckTime = metav1.Now()
		cluster.Status.ObservedGeneration = cluster.Generation

		if version != "" {
			cluster.Status.PostgresVersion = version
		}

		if err := r.Status().Patch(ctx, cluster, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	if phase == "Failed" {
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *DBClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.DBCluster{}).
		Complete(r)
}
