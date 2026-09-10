//go:build integration

package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	testDSNEnv      = "DBTETHER_TEST_DSN"
	clientDirsEnv   = "DBTETHER_TEST_PG_CLIENT_DIRS"
	scratchPassword = "integration"
	largeObject     = "decode(repeat('a1b2c3d4', 64), 'hex')"
)

// BEGIN; at column 0 is not valid plpgsql, and the body only has to look like statements.
const sourceFixture = `
CREATE EXTENSION pg_trgm;

CREATE TABLE public.notes (body text);
INSERT INTO public.notes VALUES ('COMMENT ON EXTENSION pg_trgm IS ''a data row'';'), ('BEGIN;');

SET check_function_bodies = off;
CREATE FUNCTION public.tricky() RETURNS integer LANGUAGE plpgsql AS $fn$
BEGIN
IF false THEN
COMMENT ON EXTENSION pg_trgm IS 'inside body';
BEGIN;
COMMIT;
END IF;
RETURN 1;
END
$fn$;

SELECT lo_from_bytea(0, ` + largeObject + `);
`

var (
	fixtureRows = []string{"BEGIN;", "COMMENT ON EXTENSION pg_trgm IS 'a data row';"}

	fixtureBodyLines = []string{
		"\nCOMMENT ON EXTENSION pg_trgm IS 'inside body';\n",
		"\nBEGIN;\n",
		"\nCOMMIT;\n",
	}
)

func TestRestorePgDumpRoundTrip(t *testing.T) {
	ctx := context.Background()
	dsn := requireTestDSN(t)
	dump := dumpFixtureDatabase(t, dsn, "pg_dump")
	cfg, dbName := scratchTarget(t, dsn)

	if err := restoreStream(t, cfg, "psql", openDump(t, dump)); err != nil {
		t.Fatalf("restore of a pg_dump fixture as the database owner failed: %v", err)
	}

	conn := connectToDatabase(t, dsn, dbName)
	assertFixtureRows(t, conn)
	assertFixtureFunction(t, conn)
	assertFixtureLargeObject(t, conn)

	var extensions int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_extension WHERE extname = 'pg_trgm'").Scan(&extensions); err != nil {
		t.Fatalf("failed to look up pg_trgm: %v", err)
	}
	if extensions != 1 {
		t.Errorf("pg_trgm is present %d times after the restore, want 1", extensions)
	}
}

func assertFixtureRows(t *testing.T, conn *pgx.Conn) {
	t.Helper()

	rows, err := conn.Query(context.Background(), `SELECT body FROM public.notes ORDER BY body COLLATE "C"`)
	if err != nil {
		t.Fatalf("failed to read the restored rows: %v", err)
	}
	restored, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("failed to collect the restored rows: %v", err)
	}
	if len(restored) != len(fixtureRows) {
		t.Fatalf("restored %d rows, want %d: %q", len(restored), len(fixtureRows), restored)
	}
	for i, want := range fixtureRows {
		if restored[i] != want {
			t.Errorf("restored row %d is %q, want %q", i, restored[i], want)
		}
	}
}

func assertFixtureFunction(t *testing.T, conn *pgx.Conn) {
	t.Helper()

	var body string
	if err := conn.QueryRow(context.Background(), "SELECT prosrc FROM pg_proc WHERE proname = 'tricky'").Scan(&body); err != nil {
		t.Fatalf("failed to read the restored function body: %v", err)
	}
	for _, line := range fixtureBodyLines {
		if !strings.Contains(body, line) {
			t.Errorf("function body %q lost the line %q", body, line)
		}
	}
}

func assertFixtureLargeObject(t *testing.T, conn *pgx.Conn) {
	t.Helper()

	ctx := context.Background()
	var objects int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM pg_largeobject_metadata").Scan(&objects); err != nil {
		t.Fatalf("failed to count large objects: %v", err)
	}
	if objects != 1 {
		t.Fatalf("target holds %d large objects, want 1", objects)
	}

	var identical bool
	if err := conn.QueryRow(ctx,
		"SELECT lo_get(oid) = "+largeObject+" FROM pg_largeobject_metadata").Scan(&identical); err != nil {
		t.Fatalf("failed to read the restored large object: %v", err)
	}
	if !identical {
		t.Error("the restored large object does not carry the bytes it was dumped with")
	}
}

func TestResolvedClientsRoundTripAgainstTheLiveServer(t *testing.T) {
	ctx := context.Background()
	dsn := requireTestDSN(t)
	dirs := requireClientDirs(t)
	root := linkClientRoot(t, dirs)
	majors := sortedMajors(dirs)
	serverMajor := liveServerMajor(t, dsn)
	probe := probeFromDSN(t, dsn)

	psql, err := resolvePsql(ctx, root, probe, discardLogger())
	if err != nil {
		t.Fatalf("failed to resolve psql for a server on major %d: %v", serverMajor, err)
	}
	assertResolvedClient(t, psql, root, expectedPsqlMajor(majors, serverMajor))

	pgDump, err := resolvePgDump(ctx, root, probe, discardLogger())
	wantDump, dumpable := expectedDumpMajor(majors, serverMajor)
	if !dumpable {
		if err == nil {
			t.Fatalf("resolved %s for a server on major %d, newer than every client in %v", pgDump, serverMajor, majors)
		}
		return
	}
	if err != nil {
		t.Fatalf("failed to resolve pg_dump for a server on major %d: %v", serverMajor, err)
	}
	assertResolvedClient(t, pgDump, root, wantDump)

	cfg, dbName := scratchTarget(t, dsn)
	if err := restoreStream(t, cfg, psql, openDump(t, dumpFixtureDatabase(t, dsn, pgDump))); err != nil {
		t.Fatalf("round trip through the resolved clients failed: %v", err)
	}
	assertFixtureRows(t, connectToDatabase(t, dsn, dbName))
}

func TestRestoreStreamRefusesAnEmptyObjectBeforeDropping(t *testing.T) {
	ctx := context.Background()
	dsn := requireTestDSN(t)
	dbName := scratchDatabase(t, adminConn(t, dsn), "")
	cfg := restoreConfigFromDSN(t, dsn, dbName)
	cfg.OnConflict = onConflictDrop
	mustExec(t, connectToDatabase(t, dsn, dbName), "CREATE TABLE public.precious (id int)")

	err := runRestoreStream(ctx, cfg, "psql", strings.NewReader(""), discardLogger())

	if err == nil {
		t.Fatal("expected an object without statements to fail the restore")
	}
	if !strings.Contains(err.Error(), "backup contains no SQL statements") {
		t.Errorf("error %q does not name the empty backup", err)
	}

	assertSurvived(t, dsn, dbName, "public.precious")
}

func TestRestoreStreamRefusesACustomFormatArchive(t *testing.T) {
	dsn := requireTestDSN(t)
	dbName := scratchDatabase(t, adminConn(t, dsn), "")
	cfg := restoreConfigFromDSN(t, dsn, dbName)
	cfg.OnConflict = onConflictDrop
	mustExec(t, connectToDatabase(t, dsn, dbName), "CREATE TABLE public.precious (id int)")

	archive := "PGDMP\x00\x03\x0e\x00\x00;\x00 toc ;\x00 data ;"

	err := runRestoreStream(context.Background(), cfg, "psql", strings.NewReader(archive), discardLogger())

	if err == nil {
		t.Fatal("expected a custom-format archive to fail the restore")
	}
	if !strings.Contains(err.Error(), "custom-format archive") {
		t.Errorf("error %q does not name the archive format", err)
	}
	assertSurvived(t, dsn, dbName, "public.precious")
}

func assertSurvived(t *testing.T, dsn, dbName, table string) {
	t.Helper()

	var survived bool
	if err := connectToDatabase(t, dsn, dbName).QueryRow(context.Background(),
		"SELECT to_regclass($1) IS NOT NULL", table).Scan(&survived); err != nil {
		t.Fatalf("failed to look up %s: %v", table, err)
	}
	if !survived {
		t.Errorf("the target was dropped before the unusable backup was rejected; %s is gone", table)
	}
}

func TestRestoreUnfilteredPgDumpFailsForNonOwner(t *testing.T) {
	ctx := context.Background()
	dsn := requireTestDSN(t)
	dump := stripFromDump(t, dumpFixtureDatabase(t, dsn, "pg_dump"), "SET transaction_timeout")
	cfg, _ := scratchTarget(t, dsn)

	err := runPsqlRestore(ctx, cfg, "psql", openDump(t, dump), discardLogger())
	if err == nil {
		t.Fatal("expected the unfiltered dump to fail; the filter would have nothing to protect against")
	}
	if !strings.Contains(err.Error(), "must be owner of extension") {
		t.Errorf("error %q is not the extension-ownership refusal", err)
	}
}

func TestRestoreDropsTheSetTransactionTimeoutPreV18ServersReject(t *testing.T) {
	dsn := requireTestDSN(t)
	legacy := insertIntoDump(t, dumpFixtureDatabase(t, dsn, "pg_dump"), "SET lock_timeout = 0;\n", "SET transaction_timeout = 0;\n")
	cfg, dbName := scratchTarget(t, dsn)

	if err := restoreStream(t, cfg, "psql", openDump(t, legacy)); err != nil {
		t.Fatalf("restore of a dump taken with pg_dump 18 failed: %v", err)
	}

	assertFixtureRows(t, connectToDatabase(t, dsn, dbName))
}

func TestDatabaseEmptyCountsRelationsOutsidePublic(t *testing.T) {
	ctx := context.Background()
	dsn := requireTestDSN(t)
	dbName := scratchDatabase(t, adminConn(t, dsn), "")
	cfg := restoreConfigFromDSN(t, dsn, dbName)

	empty, err := isDatabaseEmpty(ctx, cfg, "psql")
	if err != nil {
		t.Fatalf("failed to inspect the fresh database: %v", err)
	}
	if !empty {
		t.Fatal("a freshly created database is not reported empty; onConflict=fail would never restore")
	}

	mustExec(t, connectToDatabase(t, dsn, dbName), "CREATE TEMP TABLE scratch (id int)")

	empty, err = isDatabaseEmpty(ctx, cfg, "psql")
	if err != nil {
		t.Fatalf("failed to inspect the database holding another session's temp table: %v", err)
	}
	if !empty {
		t.Error("another session's temp table made the database look non-empty; onConflict=fail would refuse a legitimate restore")
	}

	mustExec(t, connectToDatabase(t, dsn, dbName), "CREATE SCHEMA analytics; CREATE TABLE analytics.events (id int)")

	empty, err = isDatabaseEmpty(ctx, cfg, "psql")
	if err != nil {
		t.Fatalf("failed to inspect the populated database: %v", err)
	}
	if empty {
		t.Error("a table outside public left the database looking empty; onConflict=fail would have restored over it")
	}
}

func TestDropAndRecreateDatabaseLeavesAnEmptyTarget(t *testing.T) {
	ctx := context.Background()
	dsn := requireTestDSN(t)
	dbName := scratchDatabase(t, adminConn(t, dsn), "")
	cfg := restoreConfigFromDSN(t, dsn, dbName)
	mustExec(t, connectToDatabase(t, dsn, dbName), "CREATE TABLE public.leftover (id int)")

	if err := dropAndRecreateDatabase(ctx, cfg, "psql", discardLogger()); err != nil {
		t.Fatalf("failed to drop and recreate %s: %v", dbName, err)
	}

	empty, err := isDatabaseEmpty(ctx, cfg, "psql")
	if err != nil {
		t.Fatalf("failed to inspect the recreated database: %v", err)
	}
	if !empty {
		t.Error("the recreated database still holds relations")
	}
}

func TestPsqlErrorsCarryTheServerMessageOnly(t *testing.T) {
	ctx := context.Background()
	dsn := requireTestDSN(t)
	cfg := restoreConfigFromDSN(t, dsn, "dbtether_no_such_database")

	_, err := isDatabaseEmpty(ctx, cfg, "psql")
	if err == nil {
		t.Fatal("expected the missing database to fail the emptiness check")
	}
	if !strings.Contains(err.Error(), `FATAL:  database "dbtether_no_such_database" does not exist`) {
		t.Errorf("error %q does not carry the server message", err)
	}
	if strings.Contains(err.Error(), cfg.Host) {
		t.Errorf("error %q carries the endpoint psql prefixes its diagnostics with", err)
	}
}

func TestRestoreRollsBackAfterTheLargeObjects(t *testing.T) {
	ctx := context.Background()
	dsn := requireTestDSN(t)
	dump := appendToDump(t, dumpFixtureDatabase(t, dsn, "pg_dump"), "\nSELECT 1/0;\n")
	cfg, dbName := scratchTarget(t, dsn)

	err := restoreStream(t, cfg, "psql", openDump(t, dump))
	if err == nil {
		t.Fatal("expected the trailing division by zero to fail the restore")
	}
	if !strings.Contains(err.Error(), "division by zero") {
		t.Errorf("error %q does not carry the psql diagnostic", err)
	}

	conn := connectToDatabase(t, dsn, dbName)
	counts := map[string]string{
		"tables": `SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')`,
		"functions":     "SELECT count(*) FROM pg_proc WHERE proname = 'tricky'",
		"large objects": "SELECT count(*) FROM pg_largeobject_metadata",
	}
	for name, query := range counts {
		var left int
		if err := conn.QueryRow(ctx, query).Scan(&left); err != nil {
			t.Fatalf("failed to count %s: %v", name, err)
		}
		if left != 0 {
			t.Errorf("%d %s outlived the failed restore; pg_dump's own COMMIT was not filtered out", left, name)
		}
	}
}

func TestRestoreRollsBackOnStatementError(t *testing.T) {
	dsn := requireTestDSN(t)
	admin := adminConn(t, dsn)
	dbName := scratchDatabase(t, admin, "")

	cfg := restoreConfigFromDSN(t, dsn, dbName)
	dump := "CREATE TABLE public.survivor (id int);\nCREATE TABLE public.broken (id int;\n"

	err := restoreStream(t, cfg, "psql", strings.NewReader(dump))
	if err == nil {
		t.Fatal("expected the broken statement to fail the restore")
	}
	if !strings.Contains(err.Error(), "syntax error") {
		t.Errorf("error %q does not carry the psql diagnostic", err)
	}

	assertNotRestored(t, dsn, dbName, "public.survivor")
}

func TestRestoreRollsBackOnTruncatedStream(t *testing.T) {
	dsn := requireTestDSN(t)
	admin := adminConn(t, dsn)
	dbName := scratchDatabase(t, admin, "")

	cfg := restoreConfigFromDSN(t, dsn, dbName)
	truncated := &errAfterReader{
		data: "CREATE TABLE public.survivor (id int);\n",
		err:  io.ErrUnexpectedEOF,
	}

	err := restoreStream(t, cfg, "psql", truncated)
	if err == nil {
		t.Fatal("expected the truncated stream to fail the restore")
	}
	if !strings.Contains(err.Error(), "backup stream truncated") {
		t.Errorf("error %q does not report the truncated stream", err)
	}

	assertNotRestored(t, dsn, dbName, "public.survivor")
}

func assertNotRestored(t *testing.T, dsn, dbName, table string) {
	t.Helper()

	conn := connectToDatabase(t, dsn, dbName)
	var created bool
	if err := conn.QueryRow(context.Background(), "SELECT to_regclass($1) IS NOT NULL", table).Scan(&created); err != nil {
		t.Fatalf("failed to look up %s: %v", table, err)
	}
	if created {
		t.Errorf("%s outlived the failed restore; the statements were not run in one aborted transaction", table)
	}
}

func dumpFixtureDatabase(t *testing.T, dsn, pgDump string) string {
	t.Helper()

	admin := adminConn(t, dsn)
	source := scratchDatabase(t, admin, "")
	mustExec(t, connectToDatabase(t, dsn, source), sourceFixture)

	path := filepath.Join(t.TempDir(), "fixture.sql")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("failed to create %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()

	parsed := mustParseDSN(t, dsn)
	cmd := exec.Command(pgDump,
		"--host", parsed.Host,
		"--port", fmt.Sprintf("%d", parsed.Port),
		"--dbname", source,
		"--username", parsed.User,
		"--format=plain",
		"--no-owner",
		"--no-acl",
	)
	cmd.Env = append(os.Environ(), "PGPASSWORD="+parsed.Password, "PGSSLMODE="+sslModeOf(parsed))
	cmd.Stdout = file
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("pg_dump failed: %v: %s", err, stderr.String())
	}
	return path
}

func appendToDump(t *testing.T, path, statement string) string {
	t.Helper()

	dump, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	extended := filepath.Join(t.TempDir(), "fixture-with-error.sql")
	if err := os.WriteFile(extended, append(dump, statement...), 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", extended, err)
	}
	return extended
}

func insertIntoDump(t *testing.T, path, anchor, statement string) string {
	t.Helper()

	dump, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}

	at := bytes.Index(dump, []byte(anchor))
	if at < 0 {
		t.Fatalf("the pg_dump preamble carries no %q to anchor on", anchor)
	}
	at += len(anchor)

	patched := filepath.Join(t.TempDir(), "fixture-legacy.sql")
	legacy := string(dump[:at]) + statement + string(dump[at:])
	if err := os.WriteFile(patched, []byte(legacy), 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", patched, err)
	}
	return patched
}

// The control fixture must fail on the extension ownership, not on a preamble line.
func stripFromDump(t *testing.T, path, prefix string) string {
	t.Helper()

	dump, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}

	var kept []string
	for _, line := range strings.Split(string(dump), "\n") {
		if !strings.HasPrefix(line, prefix) {
			kept = append(kept, line)
		}
	}

	stripped := filepath.Join(t.TempDir(), "fixture-stripped.sql")
	if err := os.WriteFile(stripped, []byte(strings.Join(kept, "\n")), 0o600); err != nil {
		t.Fatalf("failed to write %s: %v", stripped, err)
	}
	return stripped
}

func restoreStream(t *testing.T, cfg *RestoreConfig, psql string, backupData io.Reader) error {
	t.Helper()

	dump, closer, err := openDumpStream(backupData)
	if closer != nil {
		t.Cleanup(func() { _ = closer.Close() })
	}
	if err != nil {
		return err
	}
	return runPsqlRestore(context.Background(), cfg, psql, dump, discardLogger())
}

func openDump(t *testing.T, path string) io.ReadCloser {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("failed to open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func scratchTarget(t *testing.T, dsn string) (cfg *RestoreConfig, dbName string) {
	t.Helper()

	admin := adminConn(t, dsn)
	role := scratchRole(t, admin)
	dbName = scratchDatabase(t, admin, role)
	// The Database controller creates the extensions as the cluster admin before any restore.
	mustExec(t, connectToDatabase(t, dsn, dbName), "CREATE EXTENSION pg_trgm")

	cfg = restoreConfigFromDSN(t, dsn, dbName)
	cfg.Username = role
	cfg.Password = scratchPassword
	return cfg, dbName
}

func requireClientDirs(t *testing.T) map[int]string {
	t.Helper()

	spec := os.Getenv(clientDirsEnv)
	if spec == "" {
		t.Skipf("%s is not set; list the client directories to resolve between, "+
			"e.g. 16=/usr/lib/postgresql/16/bin,18=/usr/lib/postgresql/18/bin", clientDirsEnv)
	}

	dirs := make(map[int]string)
	for _, entry := range strings.Split(spec, ",") {
		major, dir, ok := strings.Cut(strings.TrimSpace(entry), "=")
		if !ok {
			t.Fatalf("%s entry %q is not major=directory", clientDirsEnv, entry)
		}
		parsed, err := strconv.Atoi(major)
		if err != nil {
			t.Fatalf("%s entry %q does not start with a major version: %v", clientDirsEnv, entry, err)
		}
		dirs[parsed] = dir
	}
	return dirs
}

func linkClientRoot(t *testing.T, dirs map[int]string) string {
	t.Helper()

	root := t.TempDir()
	for major, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s points at %s, which is not readable: %v", clientDirsEnv, dir, err)
		}
		if err := os.Symlink(dir, filepath.Join(root, fmt.Sprintf("postgresql%d", major))); err != nil {
			t.Fatalf("failed to link %s: %v", dir, err)
		}
	}
	return root
}

func sortedMajors(dirs map[int]string) []int {
	majors := make([]int, 0, len(dirs))
	for major := range dirs {
		majors = append(majors, major)
	}
	sort.Ints(majors)
	return majors
}

// Spelled out: calling the resolver's own pickers would assert nothing.
func expectedDumpMajor(majors []int, server int) (int, bool) {
	for _, major := range majors {
		if major >= server {
			return major, true
		}
	}
	return 0, false
}

func expectedPsqlMajor(majors []int, server int) int {
	if major, ok := expectedDumpMajor(majors, server); ok {
		return major
	}
	return majors[len(majors)-1]
}

func assertResolvedClient(t *testing.T, binary, root string, wantMajor int) {
	t.Helper()

	wantDir := filepath.Join(root, fmt.Sprintf("postgresql%d", wantMajor))
	if filepath.Dir(binary) != wantDir {
		t.Fatalf("resolved %s, want a binary in %s", binary, wantDir)
	}

	reported, err := exec.Command(binary, "--version").Output()
	if err != nil {
		t.Fatalf("failed to run %s --version: %v", binary, err)
	}
	if !strings.Contains(string(reported), fmt.Sprintf(") %d.", wantMajor)) {
		t.Errorf("%s reports %q, want major %d", binary, strings.TrimSpace(string(reported)), wantMajor)
	}
}

func liveServerMajor(t *testing.T, dsn string) int {
	t.Helper()

	var version int
	if err := adminConn(t, dsn).QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("failed to read the server version: %v", err)
	}
	return version / 10000
}

func probeFromDSN(t *testing.T, dsn string) *serverProbe {
	t.Helper()

	parsed := mustParseDSN(t, dsn)
	return &serverProbe{
		Host:     parsed.Host,
		Port:     int(parsed.Port),
		Username: parsed.User,
		Database: parsed.Database,
		Env:      append(os.Environ(), "PGPASSWORD="+parsed.Password, "PGSSLMODE="+sslModeOf(parsed)),
	}
}

func requireTestDSN(t *testing.T) string {
	t.Helper()

	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set; point it at a superuser DSN of a scratch PostgreSQL, "+
			"e.g. postgres://postgres:it@localhost:55432/postgres?sslmode=disable", testDSNEnv)
	}
	for _, binary := range []string{"psql", "pg_dump"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatalf("%s is set but %s is not on PATH: %v", testDSNEnv, binary, err)
		}
	}
	return dsn
}

func adminConn(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()

	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", testDSNEnv, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func connectToDatabase(t *testing.T, dsn, dbName string) *pgx.Conn {
	t.Helper()

	config := mustParseDSN(t, dsn)
	config.Database = dbName

	conn, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", dbName, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func scratchRole(t *testing.T, admin *pgx.Conn) string {
	t.Helper()

	name := fmt.Sprintf("dbtether_restore_role_%d", time.Now().UnixNano())
	mustExec(t, admin, fmt.Sprintf("CREATE ROLE %s NOSUPERUSER LOGIN PASSWORD %s",
		quoteIdentifier(name), quoteLiteral(scratchPassword)))
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = admin.Exec(ctx, "DROP OWNED BY "+quoteIdentifier(name))
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+quoteIdentifier(name)); err != nil {
			t.Errorf("failed to drop role %s: %v", name, err)
		}
	})
	return name
}

func scratchDatabase(t *testing.T, admin *pgx.Conn, owner string) string {
	t.Helper()

	name := fmt.Sprintf("dbtether_restore_it_%d", time.Now().UnixNano())
	create := "CREATE DATABASE " + quoteIdentifier(name)
	if owner != "" {
		create += " OWNER " + quoteIdentifier(owner)
	}
	mustExec(t, admin, create)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = admin.Exec(ctx,
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", name)
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdentifier(name)); err != nil {
			t.Errorf("failed to drop database %s: %v", name, err)
		}
	})
	return name
}

func restoreConfigFromDSN(t *testing.T, dsn, dbName string) *RestoreConfig {
	t.Helper()

	parsed := mustParseDSN(t, dsn)
	return &RestoreConfig{
		Host:     parsed.Host,
		Port:     int(parsed.Port),
		Database: dbName,
		Username: parsed.User,
		Password: parsed.Password,
		SSLMode:  sslModeOf(parsed),
	}
}

func mustParseDSN(t *testing.T, dsn string) *pgx.ConnConfig {
	t.Helper()

	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("failed to parse %s: %v", testDSNEnv, err)
	}
	return config
}

func sslModeOf(config *pgx.ConnConfig) string {
	if config.TLSConfig != nil {
		return "require"
	}
	return "disable"
}

func mustExec(t *testing.T, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
