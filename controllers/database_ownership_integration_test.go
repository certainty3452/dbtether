//go:build integration

package controllers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	testDSNEnv  = "DBTETHER_TEST_DSN"
	envPassword = "integration"
)

type livePG struct {
	admin  *pgx.Conn
	client *postgres.Client
	dbName string
}

func newLivePG(t *testing.T) *livePG {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set", testDSNEnv)
	}
	admin, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = admin.Close(ctx) })

	suffix := time.Now().UnixNano()
	master := fmt.Sprintf("dbtether_ctrl_master_%d", suffix)
	dbName := fmt.Sprintf("dbtether_ctrl_it_%d", suffix)
	_, err = admin.Exec(ctx, fmt.Sprintf("CREATE ROLE %s NOSUPERUSER CREATEROLE CREATEDB LOGIN PASSWORD %s",
		pq.QuoteIdentifier(master), pq.QuoteLiteral(envPassword)))
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pq.QuoteIdentifier(dbName)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(master)))
	})

	parsed, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	client, err := postgres.NewClient(ctx, postgres.Config{
		Host: parsed.Host, Port: int(parsed.Port), Username: master, Password: envPassword, SSLMode: "disable",
	})
	require.NoError(t, err)
	t.Cleanup(client.Close)
	return &livePG{admin: admin, client: client, dbName: dbName}
}

func (p *livePG) comment(t *testing.T) string {
	t.Helper()
	var comment *string
	require.NoError(t, p.admin.QueryRow(context.Background(),
		"SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1", p.dbName).Scan(&comment))
	if comment == nil {
		return ""
	}
	return *comment
}

func (p *livePG) exists(t *testing.T) bool {
	t.Helper()
	var exists bool
	require.NoError(t, p.admin.QueryRow(context.Background(),
		"SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", p.dbName).Scan(&exists))
	return exists
}

func TestDatabaseReconciler_DeletingForeignOwnedDatabaseAgainstPostgres(t *testing.T) {
	for _, policy := range []string{"Delete", "Retain"} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			pg := newLivePG(t)
			require.NoError(t, pg.client.CreateDatabaseWithOwner(ctx, pg.dbName, "other-ns", "other-db"))
			wantComment := pg.comment(t)
			require.NotEmpty(t, wantComment)

			secret, cluster, db := newDatabaseSpecFixture("shared-db", func(spec *databasesv1alpha1.DatabaseSpec) {
				spec.DeletionPolicy = policy
				spec.DatabaseName = pg.dbName
			})
			now := metav1.Now()
			db.DeletionTimestamp = &now
			r := newDatabaseTestReconciler(&singleClientCache{pgClient: pg.client}, secret, cluster, db)

			_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: "shared-db", Namespace: "default"}})
			require.NoError(t, err)

			require.True(t, pg.exists(t), "database owned by another Database CR was dropped")
			assert.Equal(t, wantComment, pg.comment(t), "foreign ownership marker was changed")
		})
	}
}

func TestDatabaseReconciler_DatabaseWithForeignCommentFailsAndKeepsComment(t *testing.T) {
	ctx := context.Background()
	pg := newLivePG(t)
	require.NoError(t, pg.client.CreateDatabaseWithOwner(ctx, pg.dbName, "other-ns", "other-db"))
	const foreign = "managed by the platform team"
	_, err := pg.admin.Exec(ctx, fmt.Sprintf("COMMENT ON DATABASE %s IS %s", pq.QuoteIdentifier(pg.dbName), pq.QuoteLiteral(foreign)))
	require.NoError(t, err)

	secret, cluster, db := newDatabaseSpecFixture("claimant", func(spec *databasesv1alpha1.DatabaseSpec) {
		spec.DatabaseName = pg.dbName
	})
	r := newDatabaseTestReconciler(&singleClientCache{pgClient: pg.client}, secret, cluster, db)
	req := reconcile.Request{NamespacedName: types.NamespacedName{Name: "claimant", Namespace: "default"}}

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	var updated databasesv1alpha1.Database
	require.NoError(t, r.Get(ctx, req.NamespacedName, &updated))
	assert.Equal(t, "Failed", updated.Status.Phase)
	assert.True(t, strings.Contains(updated.Status.Message, foreign) || strings.Contains(updated.Status.Message, "comment"),
		"message %q does not mention the existing comment", updated.Status.Message)
	assert.Equal(t, foreign, pg.comment(t))
}
