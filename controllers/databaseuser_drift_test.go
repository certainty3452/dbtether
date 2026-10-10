package controllers

import (
	"context"
	"testing"
	"time"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const readyUserName = "owner-holder"

func newReadyUser(t *testing.T) (*DatabaseUserReconciler, *recordingPGClient) {
	t.Helper()
	clusterSecret, cluster, db := newOwnerConflictFixture()
	user := newOwnerConflictUser(readyUserName, time.Now().Add(-time.Hour), "readwrite")
	r, mock := newRecordingReconciler(clusterSecret, cluster, db, user)
	reconcilePastFinalizer(t, r, readyUserName)
	require.Equal(t, "Ready", getUserStatus(t, r, readyUserName).Phase)
	return r, mock
}

func reconcileReadyUser(t *testing.T, r *DatabaseUserReconciler) (reconcile.Result, error) {
	t.Helper()
	return r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: readyUserName, Namespace: "default"},
	})
}

func TestDatabaseUserReconciler_ReadyUserRecreatesRemovedRole(t *testing.T) {
	ctx := context.Background()
	r, mock := newReadyUser(t)
	username := "owner_holder"
	require.NoError(t, mock.DropUser(ctx, username))

	_, err := reconcileReadyUser(t, r)
	require.NoError(t, err)

	exists, err := mock.UserExists(ctx, username)
	require.NoError(t, err)
	assert.True(t, exists, "role removed behind the operator's back was not recreated for a Ready user")
}

func TestDatabaseUserReconciler_ReadyUserFollowsClusterEndpointChange(t *testing.T) {
	ctx := context.Background()
	r, _ := newReadyUser(t)

	var cluster databasesv1alpha1.DBCluster
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: "owner-conflict-cluster"}, &cluster))
	cluster.Spec.Endpoint = "new-endpoint.example.com"
	require.NoError(t, r.Update(ctx, &cluster))

	_, err := reconcileReadyUser(t, r)
	require.NoError(t, err)

	var secret corev1.Secret
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: readyUserName + "-credentials", Namespace: "default"}, &secret))
	assert.Equal(t, "new-endpoint.example.com", string(secret.Data["host"]))
}

func TestDatabaseUserReconciler_ReadyUserWithoutRotationIsReverifiedPeriodically(t *testing.T) {
	r, _ := newReadyUser(t)

	result, err := reconcileReadyUser(t, r)
	require.NoError(t, err)
	assert.Greater(t, result.RequeueAfter, time.Duration(0))
}

func TestDatabaseUserReconciler_ForeignControlledSecretLeavesPasswordUntouched(t *testing.T) {
	cluster := &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec:       databasesv1alpha1.DBClusterSpec{Endpoint: "db.example.com", Port: 5432},
	}
	databases := []*databasesv1alpha1.Database{{
		ObjectMeta: metav1.ObjectMeta{Name: "mydb"},
		Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "production"},
	}}

	claims := map[string]func(*DatabaseUserReconciler) func(context.Context, *databasesv1alpha1.DatabaseUser, *corev1.Secret, *databasesv1alpha1.DBCluster, []*databasesv1alpha1.Database, postgres.ClientInterface, string) (string, string, bool, error){
		"adopt": func(r *DatabaseUserReconciler) func(context.Context, *databasesv1alpha1.DatabaseUser, *corev1.Secret, *databasesv1alpha1.DBCluster, []*databasesv1alpha1.Database, postgres.ClientInterface, string) (string, string, bool, error) {
			return r.adoptSecret
		},
		"merge": func(r *DatabaseUserReconciler) func(context.Context, *databasesv1alpha1.DatabaseUser, *corev1.Secret, *databasesv1alpha1.DBCluster, []*databasesv1alpha1.Database, postgres.ClientInterface, string) (string, string, bool, error) {
			return r.mergeSecret
		},
	}

	for name, claim := range claims {
		t.Run(name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default", UID: "test-uid"},
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "existing-secret",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: "v1", Kind: "ConfigMap", Name: "foreign", UID: "foreign-uid", Controller: ptr.To(true),
					}},
				},
				Data: map[string][]byte{"some-key": []byte("some-value")},
			}
			r := newTestReconciler(secret)
			mock := postgres.NewMockClient()

			_, _, _, err := claim(r)(context.Background(), user, secret, cluster, databases, mock, "test_user")

			assert.Error(t, err)
			assert.Zero(t, mock.SetPasswordCalls, "PostgreSQL password was changed although the secret cannot be claimed")
		})
	}
}

func TestDatabaseUserReconciler_RetainedUserLeavesSecretWithoutOwnerReference(t *testing.T) {
	ctx := context.Background()
	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{
			Name: "retained", Namespace: "default", UID: "retained-uid",
			Finalizers:        []string{UserFinalizerName},
			DeletionTimestamp: ptr.To(metav1.Now()),
		},
		Spec: databasesv1alpha1.DatabaseUserSpec{
			Database:       &databasesv1alpha1.DatabaseAccess{Name: "db1"},
			DeletionPolicy: "Retain",
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "retained-credentials", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("pw")},
	}
	r := newTestReconciler(user, secret)
	require.NoError(t, controllerutil.SetControllerReference(user, secret, r.Scheme))
	require.NoError(t, r.Update(ctx, secret))

	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "retained", Namespace: "default"}})
	require.NoError(t, err)

	var stored databasesv1alpha1.DatabaseUser
	if err := r.Get(ctx, client.ObjectKeyFromObject(user), &stored); err == nil {
		require.False(t, controllerutil.ContainsFinalizer(&stored, UserFinalizerName), "finalizer should be gone")
	} else {
		require.True(t, apierrors.IsNotFound(err))
	}
	var kept corev1.Secret
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(secret), &kept))
	assert.Empty(t, kept.OwnerReferences, "owner reference to the retained user would let garbage collection delete the secret")
}

func TestDatabaseUserReconciler_SecretUpdateConflictIsRetried(t *testing.T) {
	ctx := context.Background()
	r, _ := newReadyUser(t)

	var cluster databasesv1alpha1.DBCluster
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: "owner-conflict-cluster"}, &cluster))
	cluster.Spec.Endpoint = "moved.example.com"
	require.NoError(t, r.Update(ctx, &cluster))

	var user databasesv1alpha1.DatabaseUser
	require.NoError(t, r.Get(ctx, types.NamespacedName{Name: readyUserName, Namespace: "default"}, &user))
	user.Status.Phase = "Pending"
	require.NoError(t, r.Status().Update(ctx, &user))

	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*corev1.Secret); ok {
				return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, obj.GetName(), nil)
			}
			return c.Update(ctx, obj, opts...)
		},
	})

	result, err := reconcileReadyUser(t, r)

	assert.True(t, err != nil || result.RequeueAfter > 0, "a transient Update conflict parked the user: no error and no requeue (result %+v)", result)
}
