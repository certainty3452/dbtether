//go:build integration

package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"
)

func TestGrantDatabaseAccessAfterDropAndCreate(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; point it at a superuser DSN of a scratch PostgreSQL, "+
			"e.g. postgres://postgres:it@localhost:55432/postgres?sslmode=disable", testDSNEnv)
	}

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", testDSNEnv, err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })

	suffix := time.Now().UnixNano()
	master := fmt.Sprintf("dbtether_connect_master_%d", suffix)
	app := fmt.Sprintf("dbtether_connect_app_%d", suffix)
	dbName := fmt.Sprintf("dbtether_connect_it_%d", suffix)
	const password = "integration"

	mustExec(t, admin, fmt.Sprintf("CREATE ROLE %s NOSUPERUSER CREATEROLE CREATEDB LOGIN PASSWORD %s",
		pq.QuoteIdentifier(master), pq.QuoteLiteral(password)))
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pq.QuoteIdentifier(dbName)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(app)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(master)))
	})

	config := configFromDSN(t, dsn)
	config.Username = master
	config.Password = password

	client, err := NewClient(ctx, config)
	if err != nil {
		t.Fatalf("failed to create client as %s: %v", master, err)
	}
	t.Cleanup(client.Close)

	if err := client.CreateUser(ctx, app, password); err != nil {
		t.Fatalf("CreateUser(%s) failed: %v", app, err)
	}
	createRevokedDatabase(t, client, dbName)

	if err := client.GrantDatabaseAccess(ctx, app, dbName); err != nil {
		t.Fatalf("GrantDatabaseAccess failed: %v", err)
	}
	if !hasConnect(t, admin, app, dbName) {
		t.Fatal("expected CONNECT right after the first grant")
	}

	if err := client.DropDatabase(ctx, dbName); err != nil {
		t.Fatalf("DropDatabase failed: %v", err)
	}
	createRevokedDatabase(t, client, dbName)

	if hasConnect(t, admin, app, dbName) {
		t.Fatal("expected the recreated database to have lost the role's CONNECT; the grant below would prove nothing")
	}

	if err := client.GrantDatabaseAccess(ctx, app, dbName); err != nil {
		t.Fatalf("GrantDatabaseAccess after the recreate failed: %v", err)
	}
	if !hasConnect(t, admin, app, dbName) {
		t.Fatal("expected GrantDatabaseAccess to restore CONNECT on the recreated database")
	}

	appConn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		config.Host, config.Port, app, password, dbName, config.SSLMode))
	if err != nil {
		t.Fatalf("the regranted role still cannot log in to %s: %v", dbName, err)
	}
	_ = appConn.Close(ctx)
}

func TestRevokePublicConnectRequiresDatabaseOwnership(t *testing.T) {
	ctx := context.Background()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; point it at a superuser DSN of a scratch PostgreSQL, "+
			"e.g. postgres://postgres:it@localhost:55432/postgres?sslmode=disable", testDSNEnv)
	}

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", testDSNEnv, err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })

	suffix := time.Now().UnixNano()
	master := fmt.Sprintf("dbtether_revoke_master_%d", suffix)
	foreignOwner := fmt.Sprintf("dbtether_revoke_owner_%d", suffix)
	ownedDB := fmt.Sprintf("dbtether_revoke_owned_%d", suffix)
	foreignDB := fmt.Sprintf("dbtether_revoke_foreign_%d", suffix)
	const password = "integration"

	mustExec(t, admin, fmt.Sprintf("CREATE ROLE %s NOSUPERUSER CREATEROLE CREATEDB LOGIN PASSWORD %s",
		pq.QuoteIdentifier(master), pq.QuoteLiteral(password)))
	mustExec(t, admin, fmt.Sprintf("CREATE ROLE %s NOSUPERUSER NOLOGIN", pq.QuoteIdentifier(foreignOwner)))
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pq.QuoteIdentifier(ownedDB)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pq.QuoteIdentifier(foreignDB)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(foreignOwner)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(master)))
	})
	mustExec(t, admin, fmt.Sprintf("CREATE DATABASE %s OWNER %s",
		pq.QuoteIdentifier(foreignDB), pq.QuoteIdentifier(foreignOwner)))

	config := configFromDSN(t, dsn)
	config.Username = master
	config.Password = password

	client, err := NewClient(ctx, config)
	if err != nil {
		t.Fatalf("failed to create client as %s: %v", master, err)
	}
	t.Cleanup(client.Close)

	if err := client.CreateDatabaseWithOwner(ctx, ownedDB, "app-ns", "orders-db"); err != nil {
		t.Fatalf("CreateDatabaseWithOwner(%s) failed: %v", ownedDB, err)
	}
	if err := client.RevokePublicConnect(ctx, ownedDB); err != nil {
		t.Fatalf("RevokePublicConnect on a database the client owns: %v", err)
	}
	if publicHasConnect(t, admin, ownedDB) {
		t.Error("PUBLIC still holds CONNECT on a database the client owns")
	}

	err = client.RevokePublicConnect(ctx, foreignDB)
	if err == nil {
		t.Fatal("expected an error: PostgreSQL only warns when a non-owner revokes, leaving PUBLIC able to connect")
	}
	if !strings.Contains(err.Error(), "could not be revoked (not owner)") {
		t.Errorf("error = %v, want it to name the ownership problem", err)
	}
	if !publicHasConnect(t, admin, foreignDB) {
		t.Error("fixture is broken: PUBLIC must still hold CONNECT on the foreign database")
	}
}

func publicHasConnect(t *testing.T, conn *pgx.Conn, dbName string) bool {
	t.Helper()
	var granted bool
	if err := conn.QueryRow(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_database d
			WHERE d.datname = $1
			  AND (d.datacl IS NULL
			       OR EXISTS (SELECT 1 FROM aclexplode(d.datacl) a
			                  WHERE a.grantee = 0 AND a.privilege_type = 'CONNECT'))
		)`, dbName).Scan(&granted); err != nil {
		t.Fatalf("public CONNECT probe on %s: %v", dbName, err)
	}
	return granted
}

func createRevokedDatabase(t *testing.T, client *Client, dbName string) {
	t.Helper()
	ctx := context.Background()

	if err := client.CreateDatabaseWithOwner(ctx, dbName, "app-ns", "orders-db"); err != nil {
		t.Fatalf("CreateDatabaseWithOwner(%s) failed: %v", dbName, err)
	}
	// Without this the inherited PUBLIC grant answers every has_database_privilege probe.
	if err := client.RevokePublicConnect(ctx, dbName); err != nil {
		t.Fatalf("RevokePublicConnect(%s) failed: %v", dbName, err)
	}
}

func hasConnect(t *testing.T, conn *pgx.Conn, role, dbName string) bool {
	t.Helper()
	var granted bool
	if err := conn.QueryRow(context.Background(),
		"SELECT has_database_privilege($1, $2, 'CONNECT')", role, dbName,
	).Scan(&granted); err != nil {
		t.Fatalf("has_database_privilege(%s, %s, 'CONNECT'): %v", role, dbName, err)
	}
	return granted
}
