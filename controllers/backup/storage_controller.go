package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/storage"
)

const StorageValidationInterval = 30 * time.Minute

// StorageClientFactory builds a real storage client from a BackupStorage spec.
// Injected onto the reconciler so tests can substitute mocks without needing
// real cloud credentials. Production wiring leaves it nil and the reconciler
// falls back to defaultStorageClientFactory.
type StorageClientFactory func(ctx context.Context, c client.Client, bs *databasesv1alpha1.BackupStorage) (storage.StorageClient, error)

type BackupStorageReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// NewStorageClient overrides the default factory used to build the probe
	// client. Tests set this to return a *storage.MockClient.
	NewStorageClient StorageClientFactory
}

// +kubebuilder:rbac:groups=dbtether.io,resources=backupstorages,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=backupstorages/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=backupstorages/finalizers,verbs=update

func (r *BackupStorageReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var bs databasesv1alpha1.BackupStorage
	if err := r.Get(ctx, req.NamespacedName, &bs); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.V(1).Info("reconciling backup storage", "name", bs.Name)

	if err := r.validateStorage(&bs); err != nil {
		logger.Error(err, "storage validation failed")
		return r.updateStatus(ctx, &bs, "Failed", err.Error())
	}

	if err := r.probeStorage(ctx, &bs); err != nil {
		logger.Error(err, "storage reachability probe failed", "provider", bs.GetProvider())
		return r.updateStatus(ctx, &bs, "Failed", err.Error())
	}

	logger.Info("backup storage ready", "provider", bs.GetProvider())
	return r.updateStatus(ctx, &bs, "Ready", "storage reachable")
}

func (r *BackupStorageReconciler) validateStorage(bs *databasesv1alpha1.BackupStorage) error {
	providers := 0
	if bs.Spec.S3 != nil {
		providers++
	}
	if bs.Spec.GCS != nil {
		providers++
	}
	if bs.Spec.Azure != nil {
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

// probeStorage builds a client matching how backup/restore jobs will authenticate
// and issues a single low-cost reachability call (HeadBucket / Bucket.Attrs /
// container GetProperties). Real auth, network, and bucket errors surface here
// instead of showing up an hour later in the first backup job's logs.
func (r *BackupStorageReconciler) probeStorage(ctx context.Context, bs *databasesv1alpha1.BackupStorage) error {
	factory := r.NewStorageClient
	if factory == nil {
		factory = defaultStorageClientFactory
	}

	storageClient, err := factory(ctx, r.Client, bs)
	if err != nil {
		return fmt.Errorf("failed to build storage client: %w", err)
	}

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	return storageClient.Reachable(probeCtx)
}

// defaultStorageClientFactory mirrors the auth path used by retention cleanup
// and backup/restore jobs:
//   - S3 with CredentialsSecretRef → static AWS keys read from the Secret
//   - S3 without CredentialsSecretRef → operator ServiceAccount (IRSA / EKS PIA)
//   - GCS → operator ServiceAccount (Workload Identity / ADC)
//   - Azure → operator ServiceAccount (Workload Identity / Managed Identity)
//
// GCS and Azure do not currently read CredentialsSecretRef anywhere in the
// codebase (backup_controller.go:577 explicitly says so); reflecting that here
// keeps the probe honest: it tests exactly the auth path that subsequent jobs
// will use.
func defaultStorageClientFactory(ctx context.Context, c client.Client, bs *databasesv1alpha1.BackupStorage) (storage.StorageClient, error) {
	switch {
	case bs.Spec.S3 != nil:
		cfg := &storage.S3Config{
			Bucket:   bs.Spec.S3.Bucket,
			Region:   bs.Spec.S3.Region,
			Endpoint: bs.Spec.S3.Endpoint,
		}
		if bs.Spec.CredentialsSecretRef != nil {
			access, secret, err := readS3Credentials(ctx, c, bs)
			if err != nil {
				return nil, err
			}
			cfg.AccessKey = access
			cfg.SecretKey = secret
		}
		return storage.NewS3Client(ctx, cfg, slog.Default())

	case bs.Spec.GCS != nil:
		return storage.NewGCSClient(ctx, &storage.GCSConfig{
			Bucket:  bs.Spec.GCS.Bucket,
			Project: bs.Spec.GCS.Project,
		}, slog.Default())

	case bs.Spec.Azure != nil:
		return storage.NewAzureClient(ctx, &storage.AzureConfig{
			Container:      bs.Spec.Azure.Container,
			StorageAccount: bs.Spec.Azure.StorageAccount,
		}, slog.Default())
	}

	return nil, errors.New("no provider configured (validateStorage should have caught this)")
}

// readS3Credentials resolves AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY from the
// Secret referenced by CredentialsSecretRef. Keys must match what backup jobs
// inject (see backup_controller.go injectStorageEnv).
func readS3Credentials(ctx context.Context, c client.Client, bs *databasesv1alpha1.BackupStorage) (accessKey, secretKey string, err error) {
	ref := bs.Spec.CredentialsSecretRef
	ns := ref.Namespace
	if ns == "" {
		// BackupStorage is cluster-scoped; without a namespace on the ref we
		// don't know where to look. Surface a precise error.
		return "", "", fmt.Errorf("credentialsSecretRef.namespace is required for cluster-scoped BackupStorage %q", bs.Name)
	}

	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ns}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("credentialsSecretRef %s/%s not found", ns, ref.Name)
		}
		return "", "", fmt.Errorf("reading credentialsSecretRef %s/%s: %w", ns, ref.Name, err)
	}

	access := string(secret.Data["AWS_ACCESS_KEY_ID"])
	sec := string(secret.Data["AWS_SECRET_ACCESS_KEY"])
	if access == "" || sec == "" {
		return "", "", fmt.Errorf("credentialsSecretRef %s/%s missing AWS_ACCESS_KEY_ID or AWS_SECRET_ACCESS_KEY", ns, ref.Name)
	}
	return access, sec, nil
}

func (r *BackupStorageReconciler) updateStatus(ctx context.Context, bs *databasesv1alpha1.BackupStorage,
	phase, message string) (ctrl.Result, error) {

	// Check if status actually changed to avoid triggering unnecessary reconciliations
	statusChanged := bs.Status.Phase != phase ||
		bs.Status.Message != message ||
		bs.Status.Provider != bs.GetProvider() ||
		bs.Status.ObservedGeneration != bs.Generation

	if statusChanged {
		patch := client.MergeFrom(bs.DeepCopy())
		bs.Status.Phase = phase
		bs.Status.Message = message
		bs.Status.Provider = bs.GetProvider()
		bs.Status.ObservedGeneration = bs.Generation
		bs.Status.LastValidation = metav1.Now()

		if err := r.Status().Patch(ctx, bs, patch); err != nil {
			return ctrl.Result{}, err
		}
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
