//go:build integration

package postgres

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"
)

const testDSNEnv = "DBTETHER_TEST_DSN"

// Everything the master creates in public, in the exact shape the catalog query must
// report: the linked sequences, the multirange and the extension members are absent.
var fixtureStatements = []string{
	`CREATE TABLE plain_table (id int)`,
	`CREATE TABLE parted (id int, part_key int) PARTITION BY RANGE (part_key)`,
	`CREATE TABLE parted_p1 PARTITION OF parted FOR VALUES FROM (0) TO (100)`,
	`CREATE TABLE serial_table (id serial PRIMARY KEY)`,
	`CREATE TABLE identity_table (id int GENERATED ALWAYS AS IDENTITY)`,
	`CREATE SEQUENCE standalone_seq`,
	`CREATE VIEW plain_view AS SELECT id FROM plain_table`,
	`CREATE MATERIALIZED VIEW mat_view AS SELECT id FROM plain_table`,
	`CREATE TYPE mood AS ENUM ('ok', 'bad')`,
	`CREATE DOMAIN positive_int AS int CHECK (VALUE > 0)`,
	`CREATE TYPE int_range AS RANGE (subtype = int4)`,
	`CREATE TYPE addr AS (city text, zip text)`,
	`CREATE FUNCTION overloaded(int) RETURNS int LANGUAGE sql IMMUTABLE AS $$ SELECT $1 $$`,
	`CREATE FUNCTION overloaded(text) RETURNS int LANGUAGE sql IMMUTABLE AS $$ SELECT length($1) $$`,
	`CREATE PROCEDURE noop() LANGUAGE plpgsql AS $$ BEGIN END $$`,
	`CREATE AGGREGATE sum_int(int) (SFUNC = int4pl, STYPE = int4, INITCOND = '0')`,
	`CREATE EXTENSION pg_trgm`,
}

var wantPendingObjects = []string{
	"public.addr",
	"public.identity_table",
	"public.int_range",
	"public.mat_view",
	"public.mood",
	"public.noop()",
	"public.overloaded(integer)",
	"public.overloaded(text)",
	"public.parted",
	"public.parted_p1",
	"public.plain_table",
	"public.plain_view",
	"public.positive_int",
	"public.serial_table",
	"public.standalone_seq",
	"public.sum_int(integer)",
}

var wantStatementKinds = map[string]string{
	"public.mat_view":         "ALTER MATERIALIZED VIEW",
	"public.noop()":           "ALTER PROCEDURE",
	"public.plain_table":      "ALTER TABLE",
	"public.plain_view":       "ALTER VIEW",
	"public.positive_int":     "ALTER DOMAIN",
	"public.standalone_seq":   "ALTER SEQUENCE",
	"public.sum_int(integer)": "ALTER AGGREGATE",
	"public.mood":             "ALTER TYPE",
}

func TestOwnershipTransfer(t *testing.T) {
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

	serverVersion := serverVersionNum(t, admin)
	if serverVersion < 160000 {
		t.Skipf("server_version_num is %d; before 16 a CREATEROLE role gets no implicit membership to exercise", serverVersion)
	}

	suffix := time.Now().UnixNano()
	master := fmt.Sprintf("dbtether_master_%d", suffix)
	app := fmt.Sprintf("dbtether_app_%d", suffix)
	dbName := fmt.Sprintf("dbtether_it_%d", suffix)
	const password = "integration"

	mustExec(t, admin, fmt.Sprintf("CREATE ROLE %s NOSUPERUSER CREATEROLE CREATEDB LOGIN PASSWORD %s",
		pq.QuoteIdentifier(master), pq.QuoteLiteral(password)))
	mustExec(t, admin, fmt.Sprintf("CREATE DATABASE %s OWNER %s",
		pq.QuoteIdentifier(dbName), pq.QuoteIdentifier(master)))
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pq.QuoteIdentifier(dbName)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(app)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(master)))
	})

	config := configFromDSN(t, dsn)
	config.Username = master
	config.Password = password
	config.Database = dbName

	client, err := NewClient(ctx, config)
	if err != nil {
		t.Fatalf("failed to create client as %s: %v", master, err)
	}
	t.Cleanup(client.Close)

	conn, err := client.connectToDatabase(ctx, dbName)
	if err != nil {
		t.Fatalf("failed to connect to %s as %s: %v", dbName, master, err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })

	createFixtures(t, conn, app, password)

	assertPendingObjects(t, conn, app)

	if member := hasRole(t, admin, master, app, "MEMBER"); !member {
		t.Fatal("expected the creator's implicit grant to report MEMBER; the USAGE probe below is meaningless without it")
	}
	if usage := hasRole(t, admin, master, app, "USAGE"); usage {
		t.Fatal("expected the creator's implicit grant to carry no USAGE before ApplyPrivileges")
	}

	if err := client.ApplyPrivileges(ctx, app, dbName, "owner", nil); err != nil {
		t.Fatalf("ApplyPrivileges(owner) failed: %v", err)
	}

	if usage := hasRole(t, admin, master, app, "USAGE"); !usage {
		t.Fatal("expected ApplyPrivileges(owner) to grant the operator USAGE on the app role")
	}

	assertRelationsOwnedBy(t, conn, app)
	assertTypesOwnedBy(t, conn, app, master, serverVersion)
	assertRoutinesOwnedBy(t, conn, app, master)

	pending, err := pendingOwnershipStatements(ctx, conn, app)
	if err != nil {
		t.Fatalf("pendingOwnershipStatements failed after the transfer: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected nothing left to transfer, got %d statements: %v", len(pending), pending)
	}

	assertTransferSetsLockTimeout(t, conn, app)
	assertLockTimeoutStopsTransfer(t, client, conn, dbName, app)

	if err := client.ReassignOwnership(ctx, app, dbName); err != nil {
		t.Fatalf("ReassignOwnership failed: %v", err)
	}

	assertMembershipDiesWithRole(t, admin, app)

	t.Run("NOINHERIT operator role", func(t *testing.T) {
		assertNoInheritOperatorRejected(t, admin, dsn)
	})
}

func serverVersionNum(t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	version, err := strconv.Atoi(queryString(t, conn, "SHOW server_version_num"))
	if err != nil {
		t.Fatalf("failed to read server_version_num: %v", err)
	}
	return version
}

func assertTransferSetsLockTimeout(t *testing.T, conn *pgx.Conn, app string) {
	t.Helper()
	ctx := context.Background()

	if before := queryString(t, conn, "SHOW lock_timeout"); before == "5s" {
		t.Fatalf("lock_timeout is already %q before the transfer; the assertion below proves nothing", before)
	}

	mustExec(t, conn, "CREATE TABLE lock_timeout_probe (id int)")
	statements, err := pendingOwnershipStatements(ctx, conn, app)
	if err != nil {
		t.Fatalf("pendingOwnershipStatements failed: %v", err)
	}
	if len(statements) != 1 {
		t.Fatalf("expected only the probe table to be pending, got %v", statements)
	}
	if err := transferOwnership(ctx, conn, statements); err != nil {
		t.Fatalf("transferOwnership failed: %v", err)
	}

	if got := queryString(t, conn, "SHOW lock_timeout"); got != "5s" {
		t.Errorf("lock_timeout on the transfer connection is %q, want \"5s\"", got)
	}
}

func assertLockTimeoutStopsTransfer(t *testing.T, client *Client, conn *pgx.Conn, dbName, app string) {
	t.Helper()
	ctx := context.Background()

	mustExec(t, conn, "CREATE TABLE lock_stop_a (id int)")
	mustExec(t, conn, "CREATE TABLE lock_stop_b (id int)")

	blocker, err := client.connectToDatabase(ctx, dbName)
	if err != nil {
		t.Fatalf("failed to open the blocking connection: %v", err)
	}
	defer func() { _ = blocker.Close(ctx) }()
	mustExec(t, blocker, "BEGIN")
	mustExec(t, blocker, "LOCK TABLE lock_stop_a IN ACCESS SHARE MODE")
	defer mustExec(t, blocker, "ROLLBACK")

	statements, err := pendingOwnershipStatements(ctx, conn, app)
	if err != nil {
		t.Fatalf("pendingOwnershipStatements failed: %v", err)
	}

	err = transferOwnership(ctx, conn, statements)
	if err == nil {
		t.Fatal("expected the ALTER blocked by the open transaction to time out")
	}
	if !strings.Contains(err.Error(), "public.lock_stop_a") {
		t.Errorf("error %q does not name the blocked table", err)
	}
	if strings.Contains(err.Error(), "public.lock_stop_b") {
		t.Errorf("error %q names a table the loop should have stopped before", err)
	}
	if owner := queryString(t, conn,
		"SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid = 'public.lock_stop_b'::regclass"); owner == app {
		t.Error("the transfer went on past the lock timeout instead of leaving the rest to the next reconcile")
	}
}

func assertNoInheritOperatorRejected(t *testing.T, admin *pgx.Conn, dsn string) {
	t.Helper()
	ctx := context.Background()
	config := configFromDSN(t, dsn)

	suffix := time.Now().UnixNano()
	master := fmt.Sprintf("dbtether_noinherit_master_%d", suffix)
	app := fmt.Sprintf("dbtether_noinherit_app_%d", suffix)
	dbName := fmt.Sprintf("dbtether_noinherit_it_%d", suffix)
	const password = "integration"

	mustExec(t, admin, fmt.Sprintf("CREATE ROLE %s NOSUPERUSER NOINHERIT CREATEROLE LOGIN PASSWORD %s",
		pq.QuoteIdentifier(master), pq.QuoteLiteral(password)))
	mustExec(t, admin, fmt.Sprintf("CREATE DATABASE %s OWNER %s",
		pq.QuoteIdentifier(dbName), pq.QuoteIdentifier(master)))
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pq.QuoteIdentifier(dbName)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(app)))
		_, _ = admin.Exec(ctx, fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(master)))
	})

	config.Username = master
	config.Password = password
	config.Database = dbName

	client, err := NewClient(ctx, config)
	if err != nil {
		t.Fatalf("failed to create client as %s: %v", master, err)
	}
	t.Cleanup(client.Close)

	conn, err := client.connectToDatabase(ctx, dbName)
	if err != nil {
		t.Fatalf("failed to connect to %s as %s: %v", dbName, master, err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	mustExec(t, conn, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s",
		pq.QuoteIdentifier(app), pq.QuoteLiteral(password)))

	err = client.ApplyPrivileges(ctx, app, dbName, "owner", nil)
	if err == nil {
		t.Fatal("expected ApplyPrivileges(owner) to fail while the operator role is NOINHERIT")
	}
	for _, want := range []string{master, app, "WITH INHERIT TRUE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}

	// A NOINHERIT grantee gets SET without USAGE; that pair is what the refusal is diagnosing.
	if set := hasRole(t, admin, master, app, "SET"); !set {
		t.Error("expected the grant itself to have gone through, leaving the operator able to SET ROLE")
	}
	if usage := hasRole(t, admin, master, app, "USAGE"); usage {
		t.Error("expected the NOINHERIT operator to hold no USAGE on the app role")
	}
	if granted := queryString(t, conn,
		"SELECT has_schema_privilege($1, 'public', 'CREATE')::text", app); granted != "true" {
		t.Errorf("expected the admin grants to survive the refused membership, has_schema_privilege = %q", granted)
	}
}

func createFixtures(t *testing.T, conn *pgx.Conn, app, password string) {
	t.Helper()

	mustExec(t, conn, fmt.Sprintf("CREATE ROLE %s LOGIN PASSWORD %s",
		pq.QuoteIdentifier(app), pq.QuoteLiteral(password)))
	for _, statement := range fixtureStatements {
		mustExec(t, conn, statement)
	}
}

func assertMembershipDiesWithRole(t *testing.T, admin *pgx.Conn, app string) {
	t.Helper()

	appOID := roleOID(t, admin, app)
	if memberships := membershipRows(t, admin, appOID); memberships == 0 {
		t.Fatal("expected a membership row for the app role before it is dropped")
	}
	mustExec(t, admin, fmt.Sprintf("DROP ROLE %s", pq.QuoteIdentifier(app)))
	if memberships := membershipRows(t, admin, appOID); memberships != 0 {
		t.Fatalf("expected the membership to disappear with the role, got %d rows", memberships)
	}
}

func assertPendingObjects(t *testing.T, conn *pgx.Conn, app string) {
	t.Helper()
	ctx := context.Background()

	statements, err := pendingOwnershipStatements(ctx, conn, app)
	if err != nil {
		t.Fatalf("pendingOwnershipStatements failed: %v", err)
	}

	objects := make([]string, 0, len(statements))
	byObject := make(map[string]string, len(statements))
	for _, statement := range statements {
		objects = append(objects, statement.object)
		byObject[statement.object] = statement.statement
	}
	slices.Sort(objects)

	if !slices.Equal(objects, wantPendingObjects) {
		t.Fatalf("catalog query listed\n  %v\nwant\n  %v", objects, wantPendingObjects)
	}
	for object, kind := range wantStatementKinds {
		if !strings.HasPrefix(byObject[object], kind+" ") {
			t.Errorf("statement for %s is %q, want it to start with %q", object, byObject[object], kind)
		}
	}
}

func assertRelationsOwnedBy(t *testing.T, conn *pgx.Conn, app string) {
	t.Helper()
	ctx := context.Background()

	rows, err := conn.Query(ctx, `
		SELECT c.relname, pg_get_userbyid(c.relowner)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p', 'S', 'v', 'm')
		ORDER BY c.relname`)
	if err != nil {
		t.Fatalf("failed to list relations: %v", err)
	}
	defer rows.Close()

	found := 0
	for rows.Next() {
		var name, owner string
		if err := rows.Scan(&name, &owner); err != nil {
			t.Fatalf("failed to scan relation: %v", err)
		}
		found++
		if owner != app {
			t.Errorf("relation %s is owned by %s, want %s", name, owner, app)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("failed to read relations: %v", err)
	}
	// The eight fixture relations plus the two sequences the serial and identity columns carry.
	if want := 10; found != want {
		t.Errorf("found %d relations in public, want %d", found, want)
	}
}

func assertTypesOwnedBy(t *testing.T, conn *pgx.Conn, app, master string, serverVersion int) {
	t.Helper()

	for _, typeName := range []string{"mood", "positive_int", "int_range", "addr"} {
		owner := queryString(t, conn,
			`SELECT pg_get_userbyid(t.typowner) FROM pg_type t
			JOIN pg_namespace n ON n.oid = t.typnamespace
			WHERE n.nspname = 'public' AND t.typname = $1`, typeName)
		if owner != app {
			t.Errorf("type %s is owned by %s, want %s", typeName, owner, app)
		}
	}

	if serverVersion >= 170000 {
		return
	}

	multirange := queryString(t, conn,
		`SELECT string_agg(format('%s:%s', t.typname, pg_get_userbyid(t.typowner)), ',')
		FROM pg_type t
		JOIN pg_namespace n ON n.oid = t.typnamespace
		WHERE n.nspname = 'public' AND t.typtype = 'm'`)
	if want := "int_multirange:" + master; multirange != want {
		t.Errorf("multirange types are %q, want %q", multirange, want)
	}
}

func assertRoutinesOwnedBy(t *testing.T, conn *pgx.Conn, app, master string) {
	t.Helper()

	owned := queryString(t, conn,
		`SELECT string_agg(p.oid::regprocedure::text, ', ' ORDER BY p.oid::regprocedure::text)
		FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND pg_get_userbyid(p.proowner) = $1`, app)
	want := "noop(), overloaded(integer), overloaded(text), sum_int(integer)"
	if owned != want {
		t.Errorf("routines owned by %s are %q, want %q", app, owned, want)
	}

	extensionOwners := queryString(t, conn,
		`SELECT string_agg(DISTINCT pg_get_userbyid(p.proowner), ',')
		FROM pg_proc p
		JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e'
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public'`)
	if extensionOwners == "" {
		t.Fatal("no extension-owned routines found; the extension fixture did not install")
	}
	if strings.Contains(extensionOwners, app) {
		t.Errorf("extension routines are owned by %q, want them left with %s", extensionOwners, master)
	}
}

func configFromDSN(t *testing.T, dsn string) Config {
	t.Helper()

	parsed, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("failed to parse %s: %v", testDSNEnv, err)
	}
	sslMode := "disable"
	if parsed.TLSConfig != nil {
		sslMode = "require"
	}
	return Config{Host: parsed.Host, Port: int(parsed.Port), SSLMode: sslMode}
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func hasRole(t *testing.T, conn *pgx.Conn, member, role, privilege string) bool {
	t.Helper()
	var granted bool
	if err := conn.QueryRow(context.Background(),
		"SELECT pg_has_role($1, $2, $3)", member, role, privilege,
	).Scan(&granted); err != nil {
		t.Fatalf("pg_has_role(%s, %s, %s): %v", member, role, privilege, err)
	}
	return granted
}

func roleOID(t *testing.T, conn *pgx.Conn, role string) uint32 {
	t.Helper()
	var oid uint32
	if err := conn.QueryRow(context.Background(),
		"SELECT oid FROM pg_roles WHERE rolname = $1", role,
	).Scan(&oid); err != nil {
		t.Fatalf("failed to read oid of %s: %v", role, err)
	}
	return oid
}

func membershipRows(t *testing.T, conn *pgx.Conn, oid uint32) int {
	t.Helper()
	var count int
	if err := conn.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_auth_members WHERE roleid = $1::oid OR member = $1::oid", oid,
	).Scan(&count); err != nil {
		t.Fatalf("failed to count membership rows for oid %d: %v", oid, err)
	}
	return count
}

func queryString(t *testing.T, conn *pgx.Conn, sql string, args ...any) string {
	t.Helper()
	var value *string
	if err := conn.QueryRow(context.Background(), sql, args...).Scan(&value); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	if value == nil {
		return ""
	}
	return *value
}
