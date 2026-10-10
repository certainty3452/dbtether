package backup

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	dbtether "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/controllers"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

const wantDeniedForDev = `database prod/prod-db does not list namespace "dev" in spec.allowedNamespaces`

func completedBackup(namespace, name string, ref dbtether.DatabaseReference, path string, completed time.Time) *dbtether.Backup {
	at := metav1.NewTime(completed)
	return &dbtether.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: dbtether.BackupSpec{
			DatabaseRef: ref,
			StorageRef:  dbtether.StorageReference{Name: "prod-storage"},
		},
		Status: dbtether.BackupStatus{Phase: "Completed", Path: path, CompletedAt: &at},
	}
}

func prodDatabase(allowed ...string) *dbtether.Database {
	return &dbtether.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "prod-db", Namespace: "prod"},
		Spec:       dbtether.DatabaseSpec{AllowedNamespaces: allowed},
	}
}

func devRestore(source dbtether.RestoreSource) *dbtether.Restore {
	return &dbtether.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "dev-restore", Namespace: "dev"},
		Spec:       dbtether.RestoreSpec{Source: source},
	}
}

func prodBackupRef() dbtether.RestoreSource {
	return dbtether.RestoreSource{BackupRef: &dbtether.BackupReference{Name: "prod-backup", Namespace: "prod"}}
}

func prodBackup() *dbtether.Backup {
	return completedBackup("prod", "prod-backup", dbtether.DatabaseReference{Name: "prod-db"}, "prod/backup.sql.gz", time.Now())
}

func TestResolveSource_BackupRefCrossNamespace(t *testing.T) {
	tests := []struct {
		name        string
		objects     []runtime.Object
		wantPath    string
		wantPending string
		wantInvalid bool
	}{
		{name: "allowed", objects: []runtime.Object{prodBackup(), prodDatabase("dev")}, wantPath: "prod/backup.sql.gz"},
		{name: "denied", objects: []runtime.Object{prodBackup(), prodDatabase("staging")}, wantPending: wantDeniedForDev},
		{name: "source database gone", objects: []runtime.Object{prodBackup()}, wantInvalid: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newFakeRestoreReconciler(tt.objects...)

			path, _, err := r.resolveSource(context.Background(), devRestore(prodBackupRef()))

			switch {
			case tt.wantPending != "":
				require.Error(t, err)
				assert.True(t, isDependencyNotReady(err), "a missing consent must retry: %v", err)
				assert.Equal(t, tt.wantPending, err.Error())
			case tt.wantInvalid:
				require.Error(t, err)
				assert.True(t, isInvalidRestoreSource(err), "a deleted source database must fail terminally: %v", err)
			default:
				require.NoError(t, err)
				assert.Equal(t, tt.wantPath, path)
			}
		})
	}
}

func TestResolveSource_LatestFromCrossNamespaceDenied(t *testing.T) {
	r := newFakeRestoreReconciler(prodBackup(), prodDatabase())

	_, _, err := r.resolveSource(context.Background(), devRestore(dbtether.RestoreSource{
		LatestFrom: &dbtether.LatestFromSource{DatabaseRef: dbtether.DatabaseReference{Name: "prod-db"}, Namespace: "prod"},
	}))

	require.Error(t, err)
	assert.True(t, isDependencyNotReady(err))
	assert.Equal(t, wantDeniedForDev, err.Error())
}

func TestResolveSource_LatestFromWildcardConsent(t *testing.T) {
	db := &dbtether.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "dev"},
		Spec:       dbtether.DatabaseSpec{AllowedNamespaces: []string{"*"}},
	}
	r := newFakeRestoreReconciler(
		completedBackup("dev", "of-dev", dbtether.DatabaseReference{Name: "app", Namespace: "dev"}, "dev/app.sql.gz", time.Now()),
		db,
	)
	restore := devRestore(dbtether.RestoreSource{
		LatestFrom: &dbtether.LatestFromSource{DatabaseRef: dbtether.DatabaseReference{Name: "app", Namespace: "dev"}, Namespace: "dev"},
	})
	restore.Namespace = "box-1"

	path, _, err := r.resolveSource(context.Background(), restore)

	require.NoError(t, err)
	assert.Equal(t, "dev/app.sql.gz", path)
}

func TestResolveSource_LatestFromPatternConsent(t *testing.T) {
	tests := []struct {
		name        string
		allowed     string
		wantPending bool
	}{
		{name: "matching pattern", allowed: "box-*"},
		{name: "other pattern", allowed: "other-*", wantPending: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &dbtether.Database{
				ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "dev"},
				Spec:       dbtether.DatabaseSpec{AllowedNamespaces: []string{tt.allowed}},
			}
			r := newFakeRestoreReconciler(
				completedBackup("dev", "of-dev", dbtether.DatabaseReference{Name: "app", Namespace: "dev"}, "dev/app.sql.gz", time.Now()),
				db,
			)
			restore := devRestore(dbtether.RestoreSource{
				LatestFrom: &dbtether.LatestFromSource{DatabaseRef: dbtether.DatabaseReference{Name: "app", Namespace: "dev"}, Namespace: "dev"},
			})
			restore.Namespace = "box-1"

			path, _, err := r.resolveSource(context.Background(), restore)

			if tt.wantPending {
				require.Error(t, err)
				assert.True(t, isDependencyNotReady(err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "dev/app.sql.gz", path)
		})
	}
}

func TestResolveSource_LatestFromMatchesDatabaseNamespace(t *testing.T) {
	now := time.Now()
	r := newFakeRestoreReconciler(
		completedBackup("shared", "of-dev", dbtether.DatabaseReference{Name: "app", Namespace: "dev"}, "dev/app.sql.gz", now.Add(-time.Hour)),
		completedBackup("shared", "of-qa", dbtether.DatabaseReference{Name: "app", Namespace: "qa"}, "qa/app.sql.gz", now),
	)

	path, _, err := r.resolveSource(context.Background(), devRestore(dbtether.RestoreSource{
		LatestFrom: &dbtether.LatestFromSource{DatabaseRef: dbtether.DatabaseReference{Name: "app", Namespace: "dev"}, Namespace: "shared"},
	}))

	require.NoError(t, err)
	assert.Equal(t, "dev/app.sql.gz", path, "a newer backup of a same-named Database in another namespace must not match")
}

func TestRestoreReconcile_CrossNamespaceSourceDeniedIsPending(t *testing.T) {
	restore, db, cluster, storage := newStartFixture()
	restore.Spec.Source = dbtether.RestoreSource{BackupRef: &dbtether.BackupReference{Name: "prod-backup", Namespace: "prod"}}
	r := newLifecycleReconciler(&interceptor.Funcs{}, restore, db, cluster, storage, prodBackup(), prodDatabase())

	result, err := reconcileRestore(t, r)

	require.NoError(t, err)
	assert.Positive(t, result.RequeueAfter)
	status := getRestoreStatus(t, r)
	assert.Equal(t, "Pending", status.Phase)
	assert.Equal(t, `waiting for source: database prod/prod-db does not list namespace "app-ns" in spec.allowedNamespaces`, status.Message)
	assert.Empty(t, listRestoreJobs(t, r))
}

func TestRestoreReconciler_RegrantSkipsUsersOfNamespacesTheDatabaseDoesNotAllow(t *testing.T) {
	restore, job, db, cluster, secret := newRegrantFixture()
	db.Spec.AllowedNamespaces = []string{"partner-ns"}
	base := time.Now().Add(-time.Hour)
	ownerIn := func(namespace, name string, created time.Time) *dbtether.DatabaseUser {
		return &dbtether.DatabaseUser{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, CreationTimestamp: metav1.NewTime(created)},
			Spec: dbtether.DatabaseUserSpec{
				Database: &dbtether.DatabaseAccess{Name: "my-database", Namespace: "app-ns", Privileges: "owner"},
			},
		}
	}
	intruder := ownerIn("intruder-ns", "intruder", base)
	partner := ownerIn("partner-ns", "partner", base.Add(time.Minute))
	local := ownerIn("app-ns", "local", base.Add(2*time.Minute))

	pg := &recordingPGClient{MockClient: postgres.NewMockClient()}
	for _, role := range []string{"intruder", "partner", "local"} {
		require.NoError(t, pg.CreateUser(context.Background(), role, "pw"))
	}
	scheme := newRestoreTestScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(restore, job, db, cluster, secret, intruder, partner, local).
		WithStatusSubresource(&dbtether.Restore{}).
		WithIndex(&dbtether.DatabaseUser{}, controllers.DatabaseUserDatabaseRefIndex, indexDatabaseUserRefsForTest).
		Build()
	r := &RestoreReconciler{
		Client: c, APIReader: c, Scheme: scheme, Namespace: "dbtether", PGClientCache: &singleClientCache{pgClient: pg},
	}

	_, err := r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: "my-restore", Namespace: "app-ns"}})
	require.NoError(t, err)

	presets := map[string]string{}
	for _, call := range pg.calls {
		presets[call.Username] = call.Preset
	}
	assert.Equal(t, map[string]string{"partner": "owner", "local": "admin"}, presets,
		"the older intruder neither gets grants nor wins the owner election")
}
