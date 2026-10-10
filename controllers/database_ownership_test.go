package controllers

import (
	"context"
	"testing"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestDatabaseReconciler_DeletingForeignOwnedDatabase(t *testing.T) {
	for _, policy := range []string{"Delete", "Retain"} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			secret, cluster, db := newDatabaseSpecFixture("shared-db", func(spec *databasesv1alpha1.DatabaseSpec) {
				spec.DeletionPolicy = policy
			})
			now := metav1.Now()
			db.DeletionTimestamp = &now

			mock := postgres.NewMockClient()
			require.NoError(t, mock.CreateDatabaseWithOwner(ctx, "shared_db", "other-ns", "other-db"))
			r := newDatabaseTestReconciler(&singleClientCache{pgClient: mock}, secret, cluster, db)

			req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "shared-db", Namespace: "default"}}
			_, err := r.Reconcile(ctx, req)
			require.NoError(t, err)

			exists, err := mock.DatabaseExists(ctx, "shared_db")
			require.NoError(t, err)
			assert.True(t, exists, "database owned by another Database CR was dropped")
			ns, name, err := mock.GetDatabaseOwner(ctx, "shared_db")
			require.NoError(t, err)
			assert.Equal(t, [2]string{"other-ns", "other-db"}, [2]string{ns, name}, "foreign ownership marker was changed")

			var updated databasesv1alpha1.Database
			err = r.Get(ctx, req.NamespacedName, &updated)
			if err == nil {
				assert.False(t, controllerutil.ContainsFinalizer(&updated, FinalizerName), "finalizer must still be removed")
			} else {
				assert.True(t, apierrors.IsNotFound(err))
			}
		})
	}
}

func TestDatabaseReconciler_ClusterWithCredentialsFromEnvOnly(t *testing.T) {
	ctx := context.Background()
	t.Setenv("TEST_DB_USER", "env-admin")
	t.Setenv("TEST_DB_PASS", "env-secret")

	secret, cluster, db := newDatabaseSpecFixture("env-db", func(*databasesv1alpha1.DatabaseSpec) {})
	cluster.Spec.CredentialsSecretRef = nil
	cluster.Spec.CredentialsFromEnv = &databasesv1alpha1.CredentialsFromEnv{
		Username: "TEST_DB_USER",
		Password: "TEST_DB_PASS",
	}

	cache := &configRecordingCache{MockClientCache: postgres.NewMockClientCache()}
	r := newDatabaseTestReconciler(cache, secret, cluster, db)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "env-db", Namespace: "default"}}

	assert.NotPanics(t, func() { _, _ = r.Reconcile(ctx, req) })

	require.NotNil(t, cache.got, "no postgres client was requested")
	assert.Equal(t, "env-admin", cache.got.Username)
	assert.Equal(t, "env-secret", cache.got.Password)
}

type configRecordingCache struct {
	*postgres.MockClientCache
	got *postgres.Config
}

//nolint:gocritic // signature is fixed by the ClientCache interface
func (c *configRecordingCache) Get(ctx context.Context, clusterName string, config postgres.Config) (postgres.ClientInterface, error) {
	c.got = &config
	return c.MockClientCache.Get(ctx, clusterName, config)
}
