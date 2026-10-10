//go:build integration

package postgres

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"
)

const privilegeFixture = `
CREATE TABLE items (id serial PRIMARY KEY, name text);
CREATE TABLE audit (id int);
CREATE VIEW item_names AS SELECT name FROM items;
CREATE SEQUENCE counter;
CREATE FUNCTION answer() RETURNS int LANGUAGE sql AS 'SELECT 42';`

func TestApplyPrivileges_ConvergedCallWritesNothing(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name     string
		preset   string
		grants   []TableGrant
		ownerApp bool
	}{
		{name: "readonly", preset: "readonly"},
		{name: "readwrite", preset: "readwrite"},
		{name: "admin", preset: "admin"},
		{name: "owner", preset: "owner"},
		{name: "readonly with additional grants", preset: "readonly", grants: []TableGrant{
			{Tables: []string{"audit"}, Privileges: []TablePrivilege{"INSERT", "DELETE"}},
			{Tables: []string{"counter", "items_id_seq"}, Privileges: []TablePrivilege{"USAGE", "UPDATE", "INSERT"}},
		}},
		{name: "readwrite with owner role", preset: "readwrite", ownerApp: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newPGEnv(t)
			db := e.uniqueName("it_converged")
			user := e.uniqueName("it_user")
			e.createDatabase(t, db, e.master)
			if err := e.client.CreateUser(ctx, user, envPassword); err != nil {
				t.Fatal(err)
			}
			conn := e.connectAs(t, e.master, envPassword, db)
			mustExec(t, conn, privilegeFixture)

			ownerRole := ""
			if tt.ownerApp {
				ownerRole = e.uniqueName("it_app")
				if err := e.client.CreateUser(ctx, ownerRole, envPassword); err != nil {
					t.Fatal(err)
				}
				if err := e.client.ApplyPrivileges(ctx, ownerRole, db, "owner", "", nil); err != nil {
					t.Fatalf("ApplyPrivileges(owner) for the owner role: %v", err)
				}
			}

			if err := e.client.ApplyPrivileges(ctx, user, db, tt.preset, ownerRole, tt.grants); err != nil {
				t.Fatalf("first ApplyPrivileges: %v", err)
			}
			if pending := pendingPrivilegeStatements(t, conn, user, tt.preset, ownerRole, tt.grants); len(pending) > 0 {
				t.Fatalf("converged user still has pending statements: %q", pending)
			}
			before := aclRowVersions(t, conn)
			if err := e.client.ApplyPrivileges(ctx, user, db, tt.preset, ownerRole, tt.grants); err != nil {
				t.Fatalf("second ApplyPrivileges: %v", err)
			}
			after := aclRowVersions(t, conn)
			if !maps.Equal(before, after) {
				t.Errorf("second ApplyPrivileges rewrote catalog rows:\nbefore %v\nafter  %v", before, after)
			}
			t.Logf("catalog row xmin unchanged across the second call: %v", after)
		})
	}
}

func TestApplyPrivileges_RepairsPartialDriftWithOnlyNeededStatements(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	db := e.uniqueName("it_drift")
	user := e.uniqueName("it_user")
	e.createDatabase(t, db, e.master)
	if err := e.client.CreateUser(ctx, user, envPassword); err != nil {
		t.Fatal(err)
	}
	conn := e.connectAs(t, e.master, envPassword, db)
	mustExec(t, conn, "CREATE TABLE lost (id int); CREATE TABLE extra (id int); CREATE TABLE delegated (id int); CREATE TABLE untouched (id int)")
	if err := e.client.ApplyPrivileges(ctx, user, db, "readwrite", "", nil); err != nil {
		t.Fatalf("first ApplyPrivileges: %v", err)
	}

	grantee := pq.QuoteIdentifier(user)
	mustExec(t, conn, "REVOKE DELETE ON lost FROM "+grantee)
	mustExec(t, conn, "GRANT TRUNCATE ON extra TO "+grantee)
	mustExec(t, conn, "GRANT SELECT ON delegated TO "+grantee+" WITH GRANT OPTION")
	mustExec(t, conn, "ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT TRIGGER ON TABLES TO "+grantee)

	want := []string{
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + pq.QuoteIdentifier(e.master) + " IN SCHEMA public REVOKE TRIGGER ON TABLES FROM " + grantee,
		"REVOKE GRANT OPTION FOR SELECT ON TABLE public.delegated FROM " + grantee,
		"REVOKE TRUNCATE ON TABLE public.extra FROM " + grantee,
		"GRANT DELETE ON TABLE public.lost TO " + grantee,
	}
	if got := pendingPrivilegeStatements(t, conn, user, "readwrite", "", nil); !slices.Equal(got, want) {
		t.Fatalf("pending statements:\ngot  %q\nwant %q", got, want)
	}

	before := aclRowVersions(t, conn)
	if err := e.client.ApplyPrivileges(ctx, user, db, "readwrite", "", nil); err != nil {
		t.Fatalf("repairing ApplyPrivileges: %v", err)
	}
	after := aclRowVersions(t, conn)
	for key := range before {
		changed := before[key] != after[key]
		repaired := slices.Contains([]string{"class lost", "class extra", "class delegated", "default_acl " + e.master + " r"}, key)
		if changed != repaired {
			t.Errorf("%s: rewritten=%v, want %v", key, changed, repaired)
		}
	}
	if pending := pendingPrivilegeStatements(t, conn, user, "readwrite", "", nil); len(pending) > 0 {
		t.Errorf("drift not repaired, still pending: %q", pending)
	}
}

func pendingPrivilegeStatements(t *testing.T, conn *pgx.Conn, username, preset, ownerRole string, grants []TableGrant) []string {
	t.Helper()
	ctx := context.Background()
	desired, err := newDesiredPrivileges(username, preset, ownerRole, grants)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	statements, err := privilegeStatements(ctx, tx, username, &desired)
	if err != nil {
		t.Fatalf("privilegeStatements: %v", err)
	}
	return statements
}

func aclRowVersions(t *testing.T, conn *pgx.Conn) map[string]string {
	t.Helper()
	rows, err := conn.Query(context.Background(), `
		SELECT 'namespace ' || nspname, xmin::text FROM pg_namespace WHERE nspname = 'public'
		UNION ALL
		SELECT 'class ' || relname, xmin::text FROM pg_class WHERE relnamespace = 'public'::regnamespace
		UNION ALL
		SELECT 'proc ' || proname, xmin::text FROM pg_proc WHERE pronamespace = 'public'::regnamespace
		UNION ALL
		SELECT format('default_acl %s %s', pg_get_userbyid(defaclrole), defaclobjtype), xmin::text FROM pg_default_acl
		WHERE defaclnamespace = 'public'::regnamespace`)
	if err != nil {
		t.Fatal(err)
	}
	versions := make(map[string]string)
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
	return versions
}
