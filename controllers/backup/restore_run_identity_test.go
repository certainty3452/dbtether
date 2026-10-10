package backup

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
)

func reserveRestore(restore *dbtether.Restore, runID string) {
	restore.Status = dbtether.RestoreStatus{
		Phase:      "Pending",
		SpecHash:   (&RestoreReconciler{}).computeSpecHash(restore),
		RunID:      runID,
		JobName:    restoreJobName(restore.Name, runID),
		SourcePath: restore.Spec.Source.Path,
	}
}

func getRestoreObject(t *testing.T, r *RestoreReconciler) dbtether.Restore {
	t.Helper()
	var got dbtether.Restore
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}, &got))
	return got
}

func TestRestoreReconciler_StaleReadCannotReserveSecondRun(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	var stale *dbtether.Restore
	r := newLifecycleReconciler(&interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if rs, ok := obj.(*dbtether.Restore); ok && stale != nil {
				stale.DeepCopyInto(rs)
				return nil
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, restore, db, cluster, storage)
	beforeReservation := getRestoreObject(t, r)

	_, err := reconcileRestore(t, r)
	require.NoError(t, err)
	reserved := getRestoreStatus(t, r)
	require.NotEmpty(t, reserved.RunID)

	stale = &beforeReservation
	res, err := reconcileRestore(t, r)
	require.NoError(t, err)
	assert.Equal(t, rerunRequeueDelay, res.RequeueAfter)

	stale = nil
	got := getRestoreStatus(t, r)
	assert.Equal(t, reserved.RunID, got.RunID, "a reconcile on a stale read replaced the reservation")
	assert.Equal(t, reserved.JobName, got.JobName)
	assert.Len(t, listRestoreJobs(t, r), 1)
}

func TestRestoreReconciler_VanishedJobIsNeverRecreated(t *testing.T) {
	cases := map[string]struct {
		recordSuccess bool
		phases        []string
	}{
		"success recorded": {recordSuccess: true, phases: []string{"Granting", "Completed"}},
		"nothing recorded": {phases: []string{"Failed"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			restore, db, cluster, storage := newStartFixture()
			_, _, _, _, secret := newRegrantFixture()
			secret.Namespace = cluster.Spec.CredentialsSecretRef.Namespace
			r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage, secret)
			_, err := reconcileRestore(t, r)
			require.NoError(t, err)
			run := getRestoreObject(t, r)
			require.Equal(t, "Running", run.Status.Phase)
			if tc.recordSuccess {
				succeeded := metav1.Now()
				run.Status.JobSucceededAt = &succeeded
				require.NoError(t, r.Status().Update(context.Background(), &run))
			}
			jobs := listRestoreJobs(t, r)
			require.Len(t, jobs, 1)
			require.NoError(t, r.Delete(context.Background(), &jobs[0]))

			for i := 0; i < 2; i++ {
				_, err := reconcileRestore(t, r)
				require.NoError(t, err)
			}

			got := getRestoreStatus(t, r)
			assert.Contains(t, tc.phases, got.Phase)
			assert.Equal(t, run.Status.RunID, got.RunID)
			assert.Empty(t, listRestoreJobs(t, r), "a Job that existed for this restore must not be created again")
			if !tc.recordSuccess {
				assert.Contains(t, got.Message, "the target database state is unknown")
			}
		})
	}
}

func TestRestoreReconciler_ReservedRunWithoutJobEvidenceCreatesReservedJob(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	reserveRestore(restore, "never001")
	creates := 0
	r := newLifecycleReconciler(&interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); ok {
				creates++
			}
			return c.Create(ctx, obj, opts...)
		},
	}, restore, db, cluster, storage)

	for i := 0; i < 3; i++ {
		_, err := reconcileRestore(t, r)
		require.NoError(t, err)
	}

	jobs := listRestoreJobs(t, r)
	require.Len(t, jobs, 1)
	assert.Equal(t, 1, creates)
	assert.Equal(t, restoreJobName("my-restore", "never001"), jobs[0].Name)
	assert.Equal(t, "never001", envByName(&jobs[0])["RUN_ID"])
	assert.Equal(t, "never001", jobs[0].Labels[LabelRunID])
	got := getRestoreStatus(t, r)
	assert.Equal(t, "Running", got.Phase)
	assert.Equal(t, "never001", got.RunID)
	assert.NotNil(t, got.StartedAt)
}

func TestRestoreReconciler_ReservationForAnOutdatedSourceIsReplacedBeforeCreatingJob(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	reserveRestore(restore, "old00001")
	restore.Status.SourcePath = "cluster1/my-database/20260101-000000.sql.gz"
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage)

	_, err := reconcileRestore(t, r)
	require.NoError(t, err)

	jobs := listRestoreJobs(t, r)
	require.Len(t, jobs, 1)
	got := getRestoreStatus(t, r)
	assert.NotEqual(t, "old00001", got.RunID)
	assert.Equal(t, got.JobName, jobs[0].Name)
	assert.Equal(t, restore.Spec.Source.Path, got.SourcePath)
	assert.Equal(t, restore.Spec.Source.Path, envByName(&jobs[0])["SOURCE_PATH"])
}

func TestRestoreReconciler_StaleReservationIsNotRecreatedOverFreshEvidence(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	reserveRestore(restore, "ran00002")
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage)
	fresh := restore.DeepCopy()
	reader := fake.NewClientBuilder().WithScheme(newRestoreTestScheme()).WithObjects(fresh).
		WithStatusSubresource(&dbtether.Restore{}).Build()
	started := metav1.Now()
	fresh.Status.StartedAt = &started
	require.NoError(t, reader.Status().Update(context.Background(), fresh))
	r.APIReader = reader

	res, err := reconcileRestore(t, r)
	require.NoError(t, err)

	assert.Positive(t, res.RequeueAfter)
	assert.Empty(t, listRestoreJobs(t, r), "the cache hid that this restore's Job already started")
	assert.Equal(t, "Pending", getRestoreStatus(t, r).Phase)
}

func TestRestoreReconciler_JobCreationAlreadyExists_ContinuesWithReservedJob(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	creates := 0
	r := newLifecycleReconciler(&interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*batchv1.Job); !ok {
				return c.Create(ctx, obj, opts...)
			}
			creates++
			if err := c.Create(ctx, obj.DeepCopyObject().(client.Object), opts...); err != nil {
				return err
			}
			return apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, obj.GetName())
		},
	}, restore, db, cluster, storage)

	for i := 0; i < 2; i++ {
		_, err := reconcileRestore(t, r)
		require.NoError(t, err)
	}

	got := getRestoreStatus(t, r)
	assert.Equal(t, "Running", got.Phase)
	jobs := listRestoreJobs(t, r)
	require.Len(t, jobs, 1)
	assert.Equal(t, got.JobName, jobs[0].Name)
	assert.Equal(t, 1, creates)
}

func TestRestoreReconciler_JobNameCollisionFailsWithoutTouchingForeignJob(t *testing.T) {
	cases := map[string]func(labels map[string]string){
		"another run":   func(labels map[string]string) { labels[LabelRunID] = "foreign1" },
		"another owner": func(labels map[string]string) { labels[LabelRestoreNamespace] = "other-ns" },
	}
	for name, foreignize := range cases {
		t.Run(name, func(t *testing.T) {
			restore, db, cluster, storage := newStartFixture()
			var foreign *batchv1.Job
			r := newLifecycleReconciler(&interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
					job, ok := obj.(*batchv1.Job)
					if !ok {
						return c.Create(ctx, obj, opts...)
					}
					if foreign == nil {
						foreign = job.DeepCopy()
						foreignize(foreign.Labels)
						if err := c.Create(ctx, foreign, opts...); err != nil {
							return err
						}
					}
					return apierrors.NewAlreadyExists(schema.GroupResource{Group: "batch", Resource: "jobs"}, obj.GetName())
				},
			}, restore, db, cluster, storage)

			for i := 0; i < 2; i++ {
				_, err := reconcileRestore(t, r)
				require.NoError(t, err)
			}

			got := getRestoreStatus(t, r)
			assert.Equal(t, "Failed", got.Phase)
			assert.Contains(t, got.Message, "name collision")
			assert.Empty(t, got.JobName)
			jobs := listRestoreJobs(t, r)
			require.Len(t, jobs, 1)
			assert.Equal(t, foreign.ResourceVersion, jobs[0].ResourceVersion, "the foreign Job must be left untouched")
			assert.Equal(t, foreign.Labels, jobs[0].Labels)
		})
	}
}
