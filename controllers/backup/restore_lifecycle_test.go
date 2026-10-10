package backup

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/controllers"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

func newStartFixture() (*dbtether.Restore, *dbtether.Database, *dbtether.DBCluster, *dbtether.BackupStorage) {
	restore, _, db, cluster, _ := newRegrantFixture()
	restore.Status = dbtether.RestoreStatus{}
	cluster.Spec.CredentialsSecretRef.Namespace = "dbtether"
	storage := &dbtether.BackupStorage{
		ObjectMeta: metav1.ObjectMeta{Name: "my-storage"},
		Spec: dbtether.BackupStorageSpec{
			S3: &dbtether.S3StorageConfig{Bucket: "bucket", Region: "us-east-1"},
		},
		Status: dbtether.BackupStorageStatus{Phase: "Ready"},
	}
	return restore, db, cluster, storage
}

func newLifecycleReconciler(funcs *interceptor.Funcs, objs ...client.Object) *RestoreReconciler {
	scheme := newRestoreTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		WithInterceptorFuncs(*funcs).
		Build()
	return &RestoreReconciler{
		Client:        c,
		APIReader:     c,
		Scheme:        scheme,
		Namespace:     "dbtether",
		Image:         "dbtether:test",
		PGClientCache: &singleClientCache{pgClient: &recordingPGClient{MockClient: postgres.NewMockClient()}},
	}
}

func reconcileRestore(t *testing.T, r *RestoreReconciler) (reconcile.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"},
	})
}

func getRestoreStatus(t *testing.T, r *RestoreReconciler) dbtether.RestoreStatus {
	t.Helper()
	var got dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &got))
	return got.Status
}

func listRestoreJobs(t *testing.T, r *RestoreReconciler) []batchv1.Job {
	t.Helper()
	var jobs batchv1.JobList
	require.NoError(t, r.List(context.Background(), &jobs, client.InNamespace("dbtether")))
	return jobs.Items
}

func TestRestoreReconciler_WaitsForTargetDatabaseReady(t *testing.T) {
	for _, phase := range []string{"Failed", "Pending"} {
		t.Run(phase, func(t *testing.T) {
			restore, db, cluster, storage := newStartFixture()
			db.Status.Phase = phase
			r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage)

			res, err := reconcileRestore(t, r)
			require.NoError(t, err)

			assert.Zero(t, len(listRestoreJobs(t, r)), "no Job expected")
			status := getRestoreStatus(t, r)
			assert.Equal(t, "Pending", status.Phase)
			assert.Contains(t, status.Message, phase)
			assert.Positive(t, res.RequeueAfter)
		})
	}
}

func TestRestoreReconciler_JobNotYetVisibleAfterCreationRequeues(t *testing.T) {
	restore, _, db, cluster, _ := newRegrantFixture()
	started := metav1.NewTime(time.Now().Add(-10 * time.Second))
	restore.Status.StartedAt = &started
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      restore.Status.JobName,
		Namespace: "dbtether",
		Labels:    map[string]string{LabelRestoreName: restore.Name, LabelRestoreNamespace: restore.Namespace, LabelRunID: restore.Status.RunID},
	}}

	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster)
	r.APIReader = fake.NewClientBuilder().WithScheme(newRestoreTestScheme()).WithObjects(job).Build()

	res, err := reconcileRestore(t, r)
	require.NoError(t, err)

	assert.Equal(t, "Running", getRestoreStatus(t, r).Phase)
	assert.Positive(t, res.RequeueAfter)
}

func TestRestoreReconciler_LostCreateResponseDoesNotFailOrDuplicate(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	lost := false
	r := newLifecycleReconciler(&interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok && !lost {
				lost = true
				if err := c.Create(ctx, obj, opts...); err != nil {
					return err
				}
				return apierrors.NewTimeoutError("request timed out", 1)
			}
			return c.Create(ctx, obj, opts...)
		},
	}, restore, db, cluster, storage)

	_, _ = reconcileRestore(t, r)
	assert.NotEqual(t, "Failed", getRestoreStatus(t, r).Phase)

	_, err := reconcileRestore(t, r)
	require.NoError(t, err)
	assert.Len(t, listRestoreJobs(t, r), 1)
	assert.Equal(t, "Running", getRestoreStatus(t, r).Phase)
}

func TestRestoreReconciler_JobRecordedSuccessSurvivesJobGC(t *testing.T) {
	restore, _, db, cluster, secret := newRegrantFixture()
	succeeded := metav1.Now()
	restore.Status.JobSucceededAt = &succeeded
	user := &dbtether.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "app-ns"},
		Spec: dbtether.DatabaseUserSpec{
			Database: &dbtether.DatabaseAccess{Name: "my-database", Privileges: "readwrite"},
		},
		Status: dbtether.DatabaseUserStatus{Phase: "Ready"},
	}
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, secret, user)
	pg := &recordingPGClient{MockClient: postgres.NewMockClient()}
	require.NoError(t, pg.CreateUser(context.Background(), "app_user", "pw"))
	r.PGClientCache = &singleClientCache{pgClient: pg}

	_, err := reconcileRestore(t, r)
	require.NoError(t, err)

	require.Len(t, pg.calls, 1)
	assert.Equal(t, "app_user", pg.calls[0].Username)
	assert.Equal(t, "my_database_pg", pg.calls[0].Database)
	assert.Equal(t, "Completed", getRestoreStatus(t, r).Phase)
}

func TestRestoreReconciler_CredentialsOutsideOperatorNamespaceStayPending(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*dbtether.DBCluster, *dbtether.BackupStorage)
		wantNS  string
		wantRef string
	}{
		{
			name: "cluster credentials",
			mutate: func(c *dbtether.DBCluster, _ *dbtether.BackupStorage) {
				c.Spec.CredentialsSecretRef.Namespace = "app-ns"
			},
			wantNS:  "app-ns",
			wantRef: "DBCluster",
		},
		{
			name: "storage credentials",
			mutate: func(_ *dbtether.DBCluster, s *dbtether.BackupStorage) {
				s.Spec.CredentialsSecretRef = &dbtether.SecretReference{Name: "s3-creds", Namespace: "storage-ns"}
			},
			wantNS:  "storage-ns",
			wantRef: "BackupStorage",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore, db, cluster, storage := newStartFixture()
			tt.mutate(cluster, storage)
			r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage)

			_, err := reconcileRestore(t, r)
			require.NoError(t, err)

			assert.Zero(t, len(listRestoreJobs(t, r)), "no Job expected")
			status := getRestoreStatus(t, r)
			assert.Equal(t, "Pending", status.Phase)
			assert.Contains(t, status.Message, tt.wantNS)
			assert.Contains(t, status.Message, "dbtether")
			assert.Contains(t, status.Message, tt.wantRef)
		})
	}
}

func TestRestoreReconciler_MissingDependenciesStayPending(t *testing.T) {
	tests := []struct {
		name string
		objs func(*dbtether.Restore, *dbtether.Database, *dbtether.DBCluster, *dbtether.BackupStorage) []client.Object
	}{
		{"target database", func(r *dbtether.Restore, _ *dbtether.Database, c *dbtether.DBCluster, s *dbtether.BackupStorage) []client.Object {
			return []client.Object{r, c, s}
		}},
		{"cluster", func(r *dbtether.Restore, d *dbtether.Database, _ *dbtether.DBCluster, s *dbtether.BackupStorage) []client.Object {
			return []client.Object{r, d, s}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			restore, db, cluster, storage := newStartFixture()
			r := newLifecycleReconciler(&interceptor.Funcs{}, tt.objs(restore, db, cluster, storage)...)

			res, err := reconcileRestore(t, r)
			require.NoError(t, err)

			assert.Equal(t, "Pending", getRestoreStatus(t, r).Phase)
			assert.Positive(t, res.RequeueAfter)
		})
	}
}

func TestRestoreReconciler_TransientJobCreateErrorIsNotTerminal(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	r := newLifecycleReconciler(&interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				return errors.New("etcdserver: request timed out")
			}
			return c.Create(ctx, obj, opts...)
		},
	}, restore, db, cluster, storage)

	res, err := reconcileRestore(t, r)

	assert.True(t, err != nil || res.RequeueAfter > 0, "transient error must be retried")
	assert.NotEqual(t, "Failed", getRestoreStatus(t, r).Phase)
}

func TestRestoreReconciler_LongNamesProduceValidJobNameAndLabels(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	restore.Name = strings.Repeat("r", 60)
	cluster.Name = strings.Repeat("c", 70)
	db.Spec.ClusterRef.Name = cluster.Name
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage)

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: restore.Name, Namespace: "app-ns"},
	})
	require.NoError(t, err)

	jobs := listRestoreJobs(t, r)
	require.Len(t, jobs, 1)
	assert.Empty(t, validation.IsDNS1123Label(jobs[0].Name), "job name %q", jobs[0].Name)
	for _, labels := range []map[string]string{jobs[0].Labels, jobs[0].Spec.Template.Labels} {
		for k, v := range labels {
			assert.Empty(t, validation.IsValidLabelValue(v), "label %s=%q", k, v)
		}
	}
}
