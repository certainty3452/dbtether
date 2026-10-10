//go:build integration

package postgres

import (
	"context"
	"fmt"
	"maps"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"
)

func catalogRowVersions(t *testing.T, admin, dbConn *pgx.Conn, db, other, user string) map[string]string {
	t.Helper()
	versions := make(map[string]string)
	collect := func(conn *pgx.Conn, query string, args ...any) {
		rows, err := conn.Query(context.Background(), query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var key, xmin string
			if err := rows.Scan(&key, &xmin); err != nil {
				t.Fatal(err)
			}
			versions[key] = xmin
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
	}
	collect(admin, `SELECT 'pg_database ' || datname, xmin::text FROM pg_database
		WHERE datname = ANY($1)`, []string{db, other})
	collect(admin, `SELECT 'pg_authid ' || rolname, xmin::text FROM pg_authid WHERE rolname = $1`, user)
	collect(admin, `SELECT 'pg_db_role_setting ' || setrole::regrole::text, xmin::text FROM pg_db_role_setting
		WHERE setrole = $1::regrole`, user)
	collect(admin, `SELECT format('pg_auth_members %s->%s', roleid::regrole, member::regrole), xmin::text FROM pg_auth_members
		WHERE roleid = $1::regrole OR member = $1::regrole`, user)
	collect(admin, `SELECT 'pg_shdescription ' || objoid::regclass::text, xmin::text FROM pg_shdescription
		WHERE classoid = 'pg_database'::regclass AND objoid = (SELECT oid FROM pg_database WHERE datname = $1)`, db)
	collect(dbConn, `SELECT 'pg_extension ' || extname, xmin::text FROM pg_extension`)
	return versions
}

func TestConvergedReconcile_WritesNoCatalogRows(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	db := e.uniqueName("it_noop_db")
	user := e.uniqueName("it_noop_user")
	other := e.uniqueName("it_noop_other")
	e.createDatabase(t, other, e.master)
	e.createDatabase(t, db, e.master)
	if err := e.client.CreateUser(ctx, user, envPassword); err != nil {
		t.Fatal(err)
	}
	superConn := e.connectSuperuser(t, db)

	reconcile := func() {
		t.Helper()
		steps := []struct {
			name string
			run  func() error
		}{
			{"EnsureDatabaseWithOwner", func() error {
				_, err := e.client.EnsureDatabaseWithOwner(ctx, db, "ns", "db", true)
				return err
			}},
			{"RevokePublicConnect", func() error { return e.client.RevokePublicConnect(ctx, db) }},
			{"EnsureExtensions", func() error { return e.client.EnsureExtensions(ctx, db, []string{"citext", "pgcrypto"}) }},
			{"EnsureRoleMembership", func() error { return e.client.EnsureRoleMembership(ctx, user) }},
			{"SyncDatabaseAccess", func() error { return e.client.SyncDatabaseAccess(ctx, user, []string{db}) }},
			{"GrantDatabaseAccess", func() error { return e.client.GrantDatabaseAccess(ctx, user, db) }},
			{"ApplyPrivileges", func() error { return e.client.ApplyPrivileges(ctx, user, db, "readwrite", "", nil) }},
		}
		for _, step := range steps {
			if err := step.run(); err != nil {
				t.Fatalf("%s: %v", step.name, err)
			}
		}
	}

	if err := e.client.SetConnectionLimit(ctx, user, 7); err != nil {
		t.Fatal(err)
	}
	if err := e.client.SetRoleParameter(ctx, user, RoleParamIdleInTransactionTimeout, "60000ms"); err != nil {
		t.Fatal(err)
	}
	reconcile()

	before := catalogRowVersions(t, e.admin, superConn, db, other, user)
	reconcile()
	after := catalogRowVersions(t, e.admin, superConn, db, other, user)
	if !maps.Equal(before, after) {
		t.Errorf("converged reconcile rewrote catalog rows:\nbefore %v\nafter  %v", before, after)
	}
	for _, key := range []string{"pg_database " + db, "pg_database " + other, "pg_authid " + user,
		"pg_db_role_setting " + user, "pg_extension citext", "pg_extension pgcrypto"} {
		if _, ok := after[key]; !ok {
			t.Errorf("snapshot is missing %q: %v", key, after)
		}
	}
	t.Logf("catalog row xmin unchanged across the second reconcile: %v", after)
}

type driftEnv struct {
	*pgEnv
	db, other, user string
	superConn       *pgx.Conn
}

func newDriftEnv(t *testing.T) *driftEnv {
	t.Helper()
	ctx := context.Background()
	e := &driftEnv{pgEnv: newPGEnv(t)}
	e.db = e.uniqueName("it_drift_db")
	e.other = e.uniqueName("it_drift_other")
	e.user = e.uniqueName("it_drift_user")
	e.createDatabase(t, e.db, e.master)
	e.createDatabase(t, e.other, e.master)
	if err := e.client.CreateUser(ctx, e.user, envPassword); err != nil {
		t.Fatal(err)
	}
	e.superConn = e.connectSuperuser(t, e.db)
	if _, err := e.client.EnsureDatabaseWithOwner(ctx, e.db, "ns", "db", true); err != nil {
		t.Fatal(err)
	}
	if err := e.client.RevokePublicConnect(ctx, e.db); err != nil {
		t.Fatal(err)
	}
	if err := e.client.EnsureExtensions(ctx, e.db, []string{"citext"}); err != nil {
		t.Fatal(err)
	}
	if err := e.client.EnsureRoleMembership(ctx, e.user); err != nil {
		t.Fatal(err)
	}
	if err := e.client.SyncDatabaseAccess(ctx, e.user, []string{e.db}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *driftEnv) directConnect(t *testing.T, database string) bool {
	t.Helper()
	granted, err := e.client.hasDirectConnect(context.Background(), e.user, database)
	if err != nil {
		t.Fatal(err)
	}
	return granted
}

func TestGrantDatabaseAccess_ReappliesRevokedConnect(t *testing.T) {
	e := newDriftEnv(t)
	mustExec(t, e.admin, fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM %s",
		pq.QuoteIdentifier(e.db), pq.QuoteIdentifier(e.user)))
	if e.directConnect(t, e.db) {
		t.Fatal("setup: CONNECT still granted")
	}
	if err := e.client.GrantDatabaseAccess(context.Background(), e.user, e.db); err != nil {
		t.Fatal(err)
	}
	if !e.directConnect(t, e.db) {
		t.Error("GrantDatabaseAccess did not restore CONNECT")
	}
}

func TestRevokePublicConnect_ReappliesRegrantedPublicConnect(t *testing.T) {
	ctx := context.Background()
	e := newDriftEnv(t)
	granted := func() bool {
		var g bool
		if err := e.admin.QueryRow(ctx, publicConnectGrantedQuery, e.db).Scan(&g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	mustExec(t, e.admin, fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO PUBLIC", pq.QuoteIdentifier(e.db)))
	if !granted() {
		t.Fatal("setup: PUBLIC CONNECT not granted")
	}
	if err := e.client.RevokePublicConnect(ctx, e.db); err != nil {
		t.Fatal(err)
	}
	if granted() {
		t.Error("RevokePublicConnect did not revoke")
	}
}

func TestSyncDatabaseAccess_RevokesConnectOutsideList(t *testing.T) {
	e := newDriftEnv(t)
	mustExec(t, e.admin, fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s",
		pq.QuoteIdentifier(e.other), pq.QuoteIdentifier(e.user)))
	if !e.directConnect(t, e.other) {
		t.Fatal("setup: CONNECT not granted")
	}
	if err := e.client.SyncDatabaseAccess(context.Background(), e.user, []string{e.db}); err != nil {
		t.Fatal(err)
	}
	if e.directConnect(t, e.other) {
		t.Error("SyncDatabaseAccess did not revoke CONNECT outside the list")
	}
	if !e.directConnect(t, e.db) {
		t.Error("SyncDatabaseAccess dropped CONNECT on the listed database")
	}
}

func TestEnsureDatabaseWithOwner_RewritesDriftedComment(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		setup      string
		forceAdopt bool
	}{
		{name: "replaced marker with force-adopt", setup: "'dbtether:other/db'", forceAdopt: true},
		{name: "cleared comment", setup: "NULL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newDriftEnv(t)
			mustExec(t, e.admin, fmt.Sprintf("COMMENT ON DATABASE %s IS %s", pq.QuoteIdentifier(e.db), tt.setup))
			if _, err := e.client.EnsureDatabaseWithOwner(ctx, e.db, "ns", "db", tt.forceAdopt); err != nil {
				t.Fatal(err)
			}
			if got := databaseComment(t, e.admin, e.db); got != "dbtether:ns/db" {
				t.Errorf("comment = %q, want dbtether:ns/db", got)
			}
		})
	}
}

func TestEnsureExtensions_RecreatesDroppedExtension(t *testing.T) {
	e := newDriftEnv(t)
	mustExec(t, e.superConn, "DROP EXTENSION citext")
	if err := e.client.EnsureExtensions(context.Background(), e.db, []string{"citext"}); err != nil {
		t.Fatal(err)
	}
	if got := queryString(t, e.superConn, "SELECT count(*)::text FROM pg_extension WHERE extname = 'citext'"); got != "1" {
		t.Errorf("citext rows = %s, want 1", got)
	}
}

func TestEnsureRoleMembership_RegrantsRevokedMembership(t *testing.T) {
	e := newDriftEnv(t)
	mustExec(t, e.admin, fmt.Sprintf("REVOKE %s FROM %s GRANTED BY %s",
		pq.QuoteIdentifier(e.user), pq.QuoteIdentifier(e.master), pq.QuoteIdentifier(e.master)))
	inherits := func() string {
		return queryString(t, e.admin, "SELECT pg_has_role($1, $2, 'USAGE')::text", e.master, e.user)
	}
	if got := inherits(); got != "false" {
		t.Fatalf("setup: operator still inherits %s", e.user)
	}
	if err := e.client.EnsureRoleMembership(context.Background(), e.user); err != nil {
		t.Fatal(err)
	}
	if got := inherits(); got != "true" {
		t.Errorf("operator inherits %s = %s, want true", e.user, got)
	}
}
