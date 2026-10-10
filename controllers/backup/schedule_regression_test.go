package backup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/storage"
)

func newScheduleReconcilerWith(funcs *interceptor.Funcs, objs ...client.Object) *BackupScheduleReconciler {
	scheme := newTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&dbtether.BackupSchedule{}, &dbtether.Backup{}).
		WithInterceptorFuncs(*funcs).
		Build()
	return &BackupScheduleReconciler{Client: c, Scheme: scheme, Log: zap.NewNop().Sugar(), Namespace: testOperatorNS}
}

func retentionTestSchedule(keepLast int) *dbtether.BackupSchedule {
	return &dbtether.BackupSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: testNamespace, Finalizers: []string{scheduleFinalizerName}},
		Spec: dbtether.BackupScheduleSpec{
			DatabaseRef: dbtether.DatabaseReference{Name: "app"},
			StorageRef:  dbtether.StorageReference{Name: testStorageName},
			Schedule:    testCronSchedule,
			Retention:   &dbtether.RetentionPolicy{KeepLast: &keepLast},
		},
	}
}

func TestRetentionCleanup_DoesNotDeleteSiblingDatabasePrefix(t *testing.T) {
	r := &BackupScheduleReconciler{}
	cluster := &dbtether.DBCluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}}
	db := &dbtether.Database{Status: dbtether.DatabaseStatus{DatabaseName: "app"}}
	prefix, err := r.buildStoragePath(&dbtether.BackupStorage{}, cluster, db)
	require.NoError(t, err)

	mc := storage.NewMockClient()
	own := "c/app/20260301-000000.sql.gz"
	siblings := []string{
		"c/app2/20260101-000000.sql.gz",
		"c/app_archive/20260101-000000.sql.gz",
	}
	mc.AddObject(own, []byte("x"), time.Time{})
	for _, k := range siblings {
		mc.AddObject(k, []byte("x"), time.Time{})
	}

	r.executeRetentionCleanup(context.Background(), mc, prefix, retentionTestSchedule(1), zap.NewNop().Sugar())

	for _, k := range siblings {
		_, ok := mc.GetObject(k)
		assert.True(t, ok, "retention for database app deleted %s", k)
	}
}

func TestRetentionCleanup_DoesNotDeleteManualBackupObjects(t *testing.T) {
	mc := storage.NewMockClient()
	scheduled := "c/app/20260301-000000.sql.gz"
	manual := "c/app/20260101-000000-manual.sql.gz"
	mc.AddObject(scheduled, []byte("x"), time.Time{})
	mc.AddObject(manual, []byte("x"), time.Time{})

	r := &BackupScheduleReconciler{}
	r.executeRetentionCleanup(context.Background(), mc, "c/app/", retentionTestSchedule(1), zap.NewNop().Sugar())

	_, ok := mc.GetObject(manual)
	assert.True(t, ok, "schedule retention deleted an object written by a manual Backup")
}

func TestScheduleReconcile_RecordsLastSuccessfulBackupAndManagedCount(t *testing.T) {
	last := metav1.NewTime(time.Now())
	schedule := retentionTestSchedule(5)
	schedule.Spec.Retention = nil
	schedule.Status.LastBackupTime = &last

	mk := func(name string, phase string, completed time.Time) *dbtether.Backup {
		ct := metav1.NewTime(completed)
		return &dbtether.Backup{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: testNamespace,
				Labels: map[string]string{LabelScheduleName: schedule.Name, LabelScheduleNS: testNamespace},
			},
			Status: dbtether.BackupStatus{Phase: phase, CompletedAt: &ct},
		}
	}
	now := time.Now()
	r := newScheduleReconcilerWith(&interceptor.Funcs{},
		schedule,
		mk("nightly-a", "Completed", now.Add(-48*time.Hour)),
		mk("nightly-b", "Completed", now.Add(-24*time.Hour)),
		mk("nightly-c", "Failed", now.Add(-1*time.Hour)),
	)

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: client.ObjectKeyFromObject(schedule)})
	require.NoError(t, err)

	var got dbtether.BackupSchedule
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(schedule), &got))
	assert.Equal(t, "nightly-b", got.Status.LastSuccessfulBackup)
	assert.Equal(t, 2, got.Status.ManagedBackups)
}

func TestHandleScheduledBackup_TransientCreateErrorIsRetried(t *testing.T) {
	schedule := retentionTestSchedule(1)
	schedule.Spec.Retention = nil
	schedule.CreationTimestamp = metav1.NewTime(time.Now().Add(-48 * time.Hour))
	r := newScheduleReconcilerWith(&interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*dbtether.Backup); ok {
				return errors.New("etcdserver: request timed out")
			}
			return c.Create(ctx, obj, opts...)
		},
	}, schedule)
	cronSchedule, err := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(testCronSchedule)
	require.NoError(t, err)

	res, err := r.handleScheduledBackup(context.Background(), schedule, cronSchedule, time.Now().Add(-time.Minute), zap.NewNop().Sugar())

	assert.True(t, err != nil || res.RequeueAfter > 0, "transient Create error produced Result{} with nil error (status Failed, no retry)")
}

func TestStartRetentionCleanup_StopsWhenContextCancelled(t *testing.T) {
	blocked := make(chan struct{})
	r := newScheduleReconcilerWith(&interceptor.Funcs{
		Get: func(ctx context.Context, _ client.WithWatch, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
			close(blocked)
			<-ctx.Done()
			return ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := r.startRetentionCleanup(ctx, retentionTestSchedule(1), zap.NewNop().Sugar())

	<-blocked
	select {
	case <-done:
		t.Fatal("cleanup finished before the context was cancelled")
	default:
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup goroutine did not stop after context cancellation")
	}
}
