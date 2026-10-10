package controllers

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

const (
	consentUserNamespace = "team-a"
	consentUserName      = "app-user"
	consentRole          = "app_user"
	wantSharedDenied     = `database shared/orders does not list namespace "team-a" in spec.allowedNamespaces`
)

func consentDatabase(namespace, name, pgName string, allowed ...string) *databasesv1alpha1.Database {
	return &databasesv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: databasesv1alpha1.DatabaseSpec{
			ClusterRef:        databasesv1alpha1.ClusterReference{Name: "owner-conflict-cluster"},
			DatabaseName:      pgName,
			AllowedNamespaces: allowed,
		},
		Status: databasesv1alpha1.DatabaseStatus{Phase: "Ready", DatabaseName: pgName},
	}
}

func consentUser(accesses ...databasesv1alpha1.DatabaseAccess) *databasesv1alpha1.DatabaseUser {
	return &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: consentUserName, Namespace: consentUserNamespace},
		Spec:       databasesv1alpha1.DatabaseUserSpec{Databases: accesses, Privileges: "readwrite"},
	}
}

var (
	sharedOrders = databasesv1alpha1.DatabaseAccess{Name: "orders", Namespace: "shared"}
	localCache   = databasesv1alpha1.DatabaseAccess{Name: "cache"}
)

func newConsentReconciler(objects ...*databasesv1alpha1.Database) (*DatabaseUserReconciler, *mockPGClientWithReassign, *databasesv1alpha1.DatabaseUser) {
	return newConsentReconcilerFor(consentUser(sharedOrders, localCache), objects...)
}

func newConsentReconcilerFor(user *databasesv1alpha1.DatabaseUser, dbs ...*databasesv1alpha1.Database) (*DatabaseUserReconciler, *mockPGClientWithReassign, *databasesv1alpha1.DatabaseUser) {
	secret, cluster, _ := newOwnerConflictFixture()
	objects := make([]runtime.Object, 0, 3+len(dbs))
	objects = append(objects, secret, cluster, user)
	for _, db := range dbs {
		objects = append(objects, db)
	}
	mock := &mockPGClientWithReassign{MockClient: postgres.NewMockClient()}
	return newTestReconcilerWithCache(&singleClientCache{pgClient: mock}, objects...), mock, user
}

func reconcileConsentUser(t *testing.T, r *DatabaseUserReconciler) reconcile.Result {
	t.Helper()
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: consentUserName, Namespace: consentUserNamespace}}
	var result reconcile.Result
	for range 2 {
		var err error
		if result, err = r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	return result
}

func consentUserStatus(t *testing.T, r *DatabaseUserReconciler) databasesv1alpha1.DatabaseUserStatus {
	t.Helper()
	var user databasesv1alpha1.DatabaseUser
	if err := r.Get(context.Background(), types.NamespacedName{Name: consentUserName, Namespace: consentUserNamespace}, &user); err != nil {
		t.Fatalf("get user: %v", err)
	}
	return user.Status
}

func connectedDatabases(t *testing.T, mock *mockPGClientWithReassign) []string {
	t.Helper()
	dbs, err := mock.GetUserDatabaseAccess(context.Background(), consentRole)
	if err != nil {
		t.Fatalf("GetUserDatabaseAccess: %v", err)
	}
	sort.Strings(dbs)
	return dbs
}

func statusDatabaseNames(status *databasesv1alpha1.DatabaseUserStatus) []string {
	names := make([]string, 0, len(status.Databases))
	for _, db := range status.Databases {
		names = append(names, db.DatabaseName)
	}
	return names
}

func setAllowedNamespaces(t *testing.T, r *DatabaseUserReconciler, namespace, name string, allowed ...string) {
	t.Helper()
	var db databasesv1alpha1.Database
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, &db); err != nil {
		t.Fatalf("get database: %v", err)
	}
	db.Spec.AllowedNamespaces = allowed
	if err := r.Update(context.Background(), &db); err != nil {
		t.Fatalf("update database: %v", err)
	}
}

func TestDatabaseUserConsent_GrantedWhenNamespaceAllowed(t *testing.T) {
	r, mock, _ := newConsentReconciler(
		consentDatabase("shared", "orders", "orders_db", consentUserNamespace),
		consentDatabase(consentUserNamespace, "cache", "cache_db"),
	)

	reconcileConsentUser(t, r)

	status := consentUserStatus(t, r)
	if status.Phase != "Ready" {
		t.Fatalf("phase = %q (%s), want Ready", status.Phase, status.Message)
	}
	if got := connectedDatabases(t, mock); !equalStrings(got, []string{"cache_db", "orders_db"}) {
		t.Errorf("CONNECT on %v, want [cache_db orders_db]", got)
	}
}

func TestDatabaseUserConsent_DeniedDatabaseIsNotGranted(t *testing.T) {
	r, mock, _ := newConsentReconciler(
		consentDatabase("shared", "orders", "orders_db", "team-b"),
		consentDatabase(consentUserNamespace, "cache", "cache_db"),
	)

	result := reconcileConsentUser(t, r)

	status := consentUserStatus(t, r)
	if status.Phase != "Failed" || status.Message != wantSharedDenied {
		t.Errorf("status = %s %q, want Failed %q", status.Phase, status.Message, wantSharedDenied)
	}
	if result.RequeueAfter == 0 {
		t.Error("a denied user must keep its periodic requeue")
	}
	if got := connectedDatabases(t, mock); !equalStrings(got, []string{"cache_db"}) {
		t.Errorf("CONNECT on %v, want only [cache_db]", got)
	}
	if got := statusDatabaseNames(&status); !equalStrings(got, []string{"cache_db"}) {
		t.Errorf("status.databases = %v, want only [cache_db]", got)
	}
}

func TestDatabaseUserConsent_OnlyDeniedDatabaseCreatesNothing(t *testing.T) {
	user := consentUser(sharedOrders)
	r, mock, _ := newConsentReconcilerFor(user, consentDatabase("shared", "orders", "orders_db"))

	reconcileConsentUser(t, r)

	status := consentUserStatus(t, r)
	if status.Phase != "Failed" || status.Message != wantSharedDenied {
		t.Errorf("status = %s %q, want Failed %q", status.Phase, status.Message, wantSharedDenied)
	}
	if users := mock.GetUsers(); len(users) != 0 {
		t.Errorf("roles %v created for a user no Database allows", users)
	}
}

func TestDatabaseUserConsent_RemovedNamespaceRevokesAccess(t *testing.T) {
	tests := []struct {
		name      string
		user      *databasesv1alpha1.DatabaseUser
		remaining []string
	}{
		{name: "other databases stay granted", user: consentUser(sharedOrders, localCache), remaining: []string{"cache_db"}},
		{name: "last database withdrawn", user: consentUser(sharedOrders), remaining: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, mock, _ := newConsentReconcilerFor(tt.user,
				consentDatabase("shared", "orders", "orders_db", consentUserNamespace),
				consentDatabase(consentUserNamespace, "cache", "cache_db"),
			)
			reconcileConsentUser(t, r)
			if phase := consentUserStatus(t, r).Phase; phase != "Ready" {
				t.Fatalf("phase before revocation = %q, want Ready", phase)
			}

			setAllowedNamespaces(t, r, "shared", "orders")
			reconcileConsentUser(t, r)

			status := consentUserStatus(t, r)
			if status.Phase != "Failed" || status.Message != wantSharedDenied {
				t.Errorf("status = %s %q, want Failed %q", status.Phase, status.Message, wantSharedDenied)
			}
			if got := connectedDatabases(t, mock); !equalStrings(got, tt.remaining) {
				t.Errorf("CONNECT on %v, want %v", got, tt.remaining)
			}
			if !equalStrings(mock.reassignedDatabases, []string{"orders_db"}) {
				t.Errorf("ownership reassigned in %v, want [orders_db]", mock.reassignedDatabases)
			}
			if got := statusDatabaseNames(&status); !equalStrings(got, tt.remaining) {
				t.Errorf("status.databases = %v, want %v", got, tt.remaining)
			}
		})
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestEnqueueDatabaseUsers_ReturnsReferencingUsersOfEveryNamespace(t *testing.T) {
	inTeamB := consentUser(sharedOrders)
	inTeamB.Namespace = "team-b"
	unrelated := consentUser(localCache)
	unrelated.Name = "cache-only"
	r := newTestReconciler(consentUser(sharedOrders, localCache), inTeamB, unrelated)

	requests := r.enqueueDatabaseUsers(context.Background(), consentDatabase("shared", "orders", "orders_db"))

	got := make([]string, 0, len(requests))
	for _, req := range requests {
		got = append(got, req.String())
	}
	sort.Strings(got)
	if want := []string{"team-a/app-user", "team-b/app-user"}; !equalStrings(got, want) {
		t.Errorf("enqueued %v, want %v", got, want)
	}
}

func TestDatabaseUserConsent_UnallowedOwnerClaimDoesNotWinElection(t *testing.T) {
	ownerAccess := databasesv1alpha1.DatabaseAccess{Name: "orders", Namespace: "shared", Privileges: "owner"}
	intruder := consentUser(ownerAccess)
	intruder.Namespace = "team-b"
	intruder.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-time.Hour))
	user := consentUser(ownerAccess)
	user.CreationTimestamp = metav1.Now()
	secret, cluster, _ := newOwnerConflictFixture()
	r, _ := newRecordingReconciler(secret, cluster, intruder, user, consentDatabase("shared", "orders", "orders_db", consentUserNamespace))

	reconcileConsentUser(t, r)

	status := consentUserStatus(t, r)
	if status.Phase != "Ready" {
		t.Fatalf("status = %s %q, want Ready: team-b is not allowed, so its older claim must not count", status.Phase, status.Message)
	}
	if len(status.Databases) != 1 || status.Databases[0].Privileges != "owner" {
		t.Errorf("status.databases = %+v, want owner on orders_db", status.Databases)
	}
}

func TestGetClusterAndDatabasesForDeletion_SpecFallbackSkipsUnallowedDatabases(t *testing.T) {
	r, _, user := newConsentReconciler(
		consentDatabase("shared", "orders", "orders_db"),
		consentDatabase(consentUserNamespace, "cache", "cache_db"),
	)

	clusterName, databaseNames := r.getClusterAndDatabasesForDeletion(context.Background(), user)

	if clusterName != "owner-conflict-cluster" {
		t.Errorf("clusterName = %q, want owner-conflict-cluster", clusterName)
	}
	if !equalStrings(databaseNames, []string{"cache_db"}) {
		t.Errorf("databases cleaned up on deletion = %v, want only [cache_db]", databaseNames)
	}
}

func reconcileConsentUserOnce(r *DatabaseUserReconciler) (reconcile.Result, error) {
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: consentUserName, Namespace: consentUserNamespace},
	})
}

func TestDatabaseUserConsent_RevokesWithdrawnDatabaseWhileOtherDatabaseIsNotReady(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, r *DatabaseUserReconciler)
	}{
		{"other database Failed", func(t *testing.T, r *DatabaseUserReconciler) {
			setDatabasePhase(t, r, consentUserNamespace, "cache", "Failed")
		}},
		{"other database Pending", func(t *testing.T, r *DatabaseUserReconciler) {
			setDatabasePhase(t, r, consentUserNamespace, "cache", "Pending")
		}},
		{"other database missing", func(t *testing.T, r *DatabaseUserReconciler) {
			db := &databasesv1alpha1.Database{ObjectMeta: metav1.ObjectMeta{Namespace: consentUserNamespace, Name: "cache"}}
			if err := r.Delete(context.Background(), db); err != nil {
				t.Fatalf("delete database: %v", err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, mock, _ := newConsentReconciler(
				consentDatabase("shared", "orders", "orders_db", consentUserNamespace),
				consentDatabase(consentUserNamespace, "cache", "cache_db"),
			)
			reconcileConsentUser(t, r)
			if phase := consentUserStatus(t, r).Phase; phase != "Ready" {
				t.Fatalf("phase before revocation = %q, want Ready", phase)
			}

			setAllowedNamespaces(t, r, "shared", "orders")
			tt.mutate(t, r)
			_, _ = reconcileConsentUserOnce(r)

			for _, db := range connectedDatabases(t, mock) {
				if db == "orders_db" {
					t.Fatalf("CONNECT on orders_db still granted after its allowedNamespaces dropped the user")
				}
			}
		})
	}
}

func setDatabasePhase(t *testing.T, r *DatabaseUserReconciler, namespace, name, phase string) {
	t.Helper()
	var db databasesv1alpha1.Database
	if err := r.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: name}, &db); err != nil {
		t.Fatalf("get database: %v", err)
	}
	db.Status.Phase = phase
	if err := r.Update(context.Background(), &db); err != nil {
		t.Fatalf("update database: %v", err)
	}
}

type failingReassignPG struct {
	*postgres.MockClient
	err error
}

func (m *failingReassignPG) ReassignOwnership(context.Context, string, string) error {
	return m.err
}

func TestDatabaseUserConsent_FailedOwnershipReassignKeepsStatusDatabasesUntilRetrySucceeds(t *testing.T) {
	secret, cluster, _ := newOwnerConflictFixture()
	user := consentUser(sharedOrders)
	mock := &failingReassignPG{MockClient: postgres.NewMockClient(), err: errors.New("reassign boom")}
	r := newTestReconcilerWithCache(&singleClientCache{pgClient: mock}, secret, cluster, user,
		consentDatabase("shared", "orders", "orders_db", consentUserNamespace))
	reconcileConsentUser(t, r)
	before := consentUserStatus(t, r)
	if got := statusDatabaseNames(&before); !equalStrings(got, []string{"orders_db"}) {
		t.Fatalf("status.databases before withdrawal = %v, want [orders_db]", got)
	}

	setAllowedNamespaces(t, r, "shared", "orders")
	result, err := reconcileConsentUserOnce(r)

	status := consentUserStatus(t, r)
	if err == nil && result.RequeueAfter == 0 {
		t.Error("failed ownership reassignment returned neither an error nor a requeue")
	}
	if got := statusDatabaseNames(&status); !equalStrings(got, []string{"orders_db"}) {
		t.Fatalf("status.databases = %v after failed reassignment, want [orders_db] so the retry still knows what to clean up", got)
	}

	mock.err = nil
	if _, err := reconcileConsentUserOnce(r); err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	status = consentUserStatus(t, r)
	if got := statusDatabaseNames(&status); len(got) != 0 {
		t.Errorf("status.databases = %v after successful retry, want empty", got)
	}
}

func TestDatabaseUserConsent_RemovedNamespaceRevokesAccessEvenWhenSecretWriteFails(t *testing.T) {
	r, mock, _ := newConsentReconciler(
		consentDatabase("shared", "orders", "orders_db", consentUserNamespace),
		consentDatabase(consentUserNamespace, "cache", "cache_db"),
	)
	reconcileConsentUser(t, r)
	if phase := consentUserStatus(t, r).Phase; phase != "Ready" {
		t.Fatalf("phase before revocation = %q, want Ready", phase)
	}

	secretWriteFails := false
	denied := apierrors.NewInvalid(schema.GroupKind{Kind: "Secret"}, "immutable", nil)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok && secretWriteFails {
				return denied
			}
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if _, ok := obj.(*corev1.Secret); ok && secretWriteFails {
				return denied
			}
			return c.Patch(ctx, obj, patch, opts...)
		},
	})

	setAllowedNamespaces(t, r, "shared", "orders")
	secretWriteFails = true
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: consentUserName, Namespace: consentUserNamespace}}
	for range 2 {
		_, _ = r.Reconcile(context.Background(), req)
	}

	if got := connectedDatabases(t, mock); !equalStrings(got, []string{"cache_db"}) {
		t.Errorf("CONNECT on %v, want only [cache_db]: access to orders_db must be withdrawn although the Secret write failed", got)
	}
}
