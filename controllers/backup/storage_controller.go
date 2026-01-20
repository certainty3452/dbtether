package backup

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

const StorageValidationInterval = 30 * time.Minute

type BackupStorageReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=dbtether.io,resources=backupstorages,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=backupstorages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=backupstorages/finalizers,verbs=update

func (r *BackupStorageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var storage databasesv1alpha1.BackupStorage
	if err := r.Get(ctx, req.NamespacedName, &storage); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.V(1).Info("reconciling backup storage", "name", storage.Name)

	if err := r.validateStorage(&storage); err != nil {
		logger.Error(err, "storage validation failed")
		return r.updateStatus(ctx, &storage, "Failed", err.Error())
	}

	logger.Info("backup storage ready", "provider", storage.GetProvider())
	return r.updateStatus(ctx, &storage, "Ready", "storage validated successfully")
}

func (r *BackupStorageReconciler) validateStorage(storage *databasesv1alpha1.BackupStorage) error {
	providers := 0
	if storage.Spec.S3 != nil {
		providers++
	}
	if storage.Spec.GCS != nil {
		providers++
	}
	if storage.Spec.Azure != nil {
		providers++
	}

	if providers == 0 {
		return fmt.Errorf("one of s3, gcs, or azure must be specified")
	}
	if providers > 1 {
		return fmt.Errorf("only one of s3, gcs, or azure can be specified")
	}

	return nil
}

func (r *BackupStorageReconciler) updateStatus(ctx context.Context, storage *databasesv1alpha1.BackupStorage,
	phase, message string) (ctrl.Result, error) {

	patch := client.MergeFrom(storage.DeepCopy())
	storage.Status.Phase = phase
	storage.Status.Message = message
	storage.Status.Provider = storage.GetProvider()
	storage.Status.ObservedGeneration = storage.Generation

	now := metav1.Now()
	storage.Status.LastValidation = now

	if err := r.Status().Patch(ctx, storage, patch); err != nil {
		return ctrl.Result{}, err
	}

	if phase == "Failed" {
		return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
	}

	return ctrl.Result{RequeueAfter: StorageValidationInterval}, nil
}

func (r *BackupStorageReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.BackupStorage{}).
		Complete(r)
}
