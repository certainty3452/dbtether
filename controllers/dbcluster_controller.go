package controllers

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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

// +kubebuilder:rbac:groups=dbtether.io,resources=dbclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=dbclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=dbclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

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

	username, password, err := r.getCredentials(ctx, &cluster)
	if err != nil {
		logger.Error(err, "failed to get credentials")
		return r.updateStatus(ctx, &cluster, "Failed", fmt.Sprintf("credentials error: %s", err.Error()), "")
	}

	pgConfig := postgres.Config{
		Host:     cluster.Spec.Endpoint,
		Port:     cluster.Spec.Port,
		Username: username,
		Password: password,
		Database: "postgres",
	}

	pgClient, err := r.PGClientCache.Get(ctx, cluster.Name, pgConfig)
	if err != nil {
		logger.Error(err, "failed to connect")
		return r.updateStatus(ctx, &cluster, "Failed", fmt.Sprintf("connection failed: %s", err.Error()), "")
	}

	if err := pgClient.Ping(ctx); err != nil {
		logger.Error(err, "ping failed")
		r.PGClientCache.Remove(cluster.Name)
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

func (r *DBClusterReconciler) getCredentials(ctx context.Context, cluster *databasesv1alpha1.DBCluster) (username, password string, err error) {
	logger := log.FromContext(ctx)
	hasSecretRef := cluster.Spec.CredentialsSecretRef != nil
	hasEnvRef := cluster.Spec.CredentialsFromEnv != nil

	if hasSecretRef && hasEnvRef {
		logger.Info("both credentialsSecretRef and credentialsFromEnv specified, using credentialsFromEnv")
	}

	if hasEnvRef {
		return r.getCredentialsFromEnv(cluster.Spec.CredentialsFromEnv)
	}

	if hasSecretRef {
		return r.getCredentialsFromSecret(ctx, cluster.Spec.CredentialsSecretRef)
	}

	return "", "", fmt.Errorf("either credentialsSecretRef or credentialsFromEnv must be specified")
}

func (r *DBClusterReconciler) getCredentialsFromEnv(cfg *databasesv1alpha1.CredentialsFromEnv) (username, password string, err error) {
	username = os.Getenv(cfg.Username)
	if username == "" {
		return "", "", fmt.Errorf("environment variable %s not set or empty", cfg.Username)
	}

	password = os.Getenv(cfg.Password)
	if password == "" {
		return "", "", fmt.Errorf("environment variable %s not set or empty", cfg.Password)
	}

	return username, password, nil
}

func (r *DBClusterReconciler) getCredentialsFromSecret(ctx context.Context, ref *databasesv1alpha1.SecretReference) (username, password string, err error) {
	var secret corev1.Secret
	err = r.Get(ctx, types.NamespacedName{
		Name:      ref.Name,
		Namespace: ref.Namespace,
	}, &secret)
	if err != nil {
		return "", "", err
	}

	username = string(secret.Data["username"])
	password = string(secret.Data["password"])

	if username == "" || password == "" {
		return "", "", fmt.Errorf("secret must contain 'username' and 'password' keys")
	}

	return username, password, nil
}

func (r *DBClusterReconciler) updateStatus(ctx context.Context, cluster *databasesv1alpha1.DBCluster, phase, message, version string) (ctrl.Result, error) {
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
