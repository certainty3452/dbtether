package backup

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPickDumpMajor(t *testing.T) {
	tests := []struct {
		name      string
		available []int
		server    int
		want      int
		wantFound bool
	}{
		{name: "the server major is bundled", available: []int{16, 17, 18}, server: 16, want: 16, wantFound: true},
		{name: "older server takes the smallest greater client", available: []int{16, 17, 18}, server: 14, want: 16, wantFound: true},
		{name: "unsorted list still takes the smallest greater client", available: []int{18, 16, 17}, server: 15, want: 16, wantFound: true},
		{name: "server newer than every bundled client has no dump client", available: []int{16, 17, 18}, server: 19},
		{name: "nothing bundled", server: 16},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			major, found := pickDumpMajor(tt.available, tt.server)
			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				assert.Equal(t, tt.want, major)
			}
		})
	}
}

func TestPickPsqlMajor(t *testing.T) {
	tests := []struct {
		name      string
		available []int
		server    int
		want      int
		wantFound bool
	}{
		{name: "the server major is bundled", available: []int{16, 17, 18}, server: 17, want: 17, wantFound: true},
		{name: "older server takes the smallest greater client", available: []int{16, 17, 18}, server: 14, want: 16, wantFound: true},
		{name: "server newer than every bundled client takes the newest below", available: []int{16, 17, 18}, server: 19, want: 18, wantFound: true},
		{name: "unsorted list still takes the newest below", available: []int{18, 16, 17}, server: 20, want: 18, wantFound: true},
		{name: "nothing bundled", server: 16},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			major, found := pickPsqlMajor(tt.available, tt.server)
			assert.Equal(t, tt.wantFound, found)
			if tt.wantFound {
				assert.Equal(t, tt.want, major)
			}
		})
	}
}

func TestBundledClientMajors(t *testing.T) {
	root := t.TempDir()
	writeClientBinary(t, root, "postgresql16", "pg_dump", "")
	writeClientBinary(t, root, "postgresql18", "psql", "")
	require.NoError(t, os.Symlink(filepath.Join(root, "postgresql18"), filepath.Join(root, "postgresql17")))
	require.NoError(t, os.Mkdir(filepath.Join(root, "postgresql"), 0o750))
	require.NoError(t, os.Mkdir(filepath.Join(root, "postgresql17beta"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "postgresql15"), nil, 0o600))

	majors, err := bundledClientMajors(root)

	require.NoError(t, err)
	assert.Equal(t, []int{16, 17, 18}, majors, "a symlinked version directory counts, a plain file does not")
}

func TestBundledClientMajorsWithoutARoot(t *testing.T) {
	majors, err := bundledClientMajors(filepath.Join(t.TempDir(), "no-such-root"))
	require.NoError(t, err)
	assert.Empty(t, majors)
}

func TestResolveFallsBackToPath(t *testing.T) {
	root := t.TempDir()
	probe := &serverProbe{Host: "127.0.0.1", Port: 1, Username: "nobody", Database: "postgres"}

	pgDump, err := resolvePgDump(context.Background(), root, probe, quietLogger())
	require.NoError(t, err, "an unreachable server must not be probed when the root bundles no client")
	assert.Equal(t, "pg_dump", pgDump)

	psql, err := resolvePsql(context.Background(), root, probe, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, "psql", psql)
}

func TestResolveProbesWithTheNewestBundledPsql(t *testing.T) {
	root := t.TempDir()
	writeClientBinary(t, root, "postgresql16", "psql", "#!/bin/sh\nexit 1\n")
	writeClientBinary(t, root, "postgresql16", "pg_dump", "")
	writeClientBinary(t, root, "postgresql18", "psql", "#!/bin/sh\necho 160015\n")

	probe := &serverProbe{Host: "db.example.com", Port: 5432, Username: "admin", Database: "orders"}

	pgDump, err := resolvePgDump(context.Background(), root, probe, quietLogger())
	require.NoError(t, err, "the probe must run the newest bundled psql, not the one matching the server")
	assert.Equal(t, filepath.Join(root, "postgresql16", "pg_dump"), pgDump)

	psql, err := resolvePsql(context.Background(), root, probe, quietLogger())
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(root, "postgresql16", "psql"), psql)
}

func TestResolveRejectsAServerNewerThanEveryDumpClient(t *testing.T) {
	root := t.TempDir()
	writeClientBinary(t, root, "postgresql16", "psql", "#!/bin/sh\necho 190002\n")
	writeClientBinary(t, root, "postgresql18", "psql", "#!/bin/sh\necho 190002\n")

	probe := &serverProbe{Host: "db.example.com", Port: 5432, Username: "admin", Database: "orders"}

	_, err := resolvePgDump(context.Background(), root, probe, quietLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pg_dump for a server on major 19")
	assert.Contains(t, err.Error(), "16, 18")

	psql, err := resolvePsql(context.Background(), root, probe, quietLogger())
	require.NoError(t, err, "psql ships SQL text only, so the newest bundled client still restores")
	assert.Equal(t, filepath.Join(root, "postgresql18", "psql"), psql)
}

func TestResolveKeepsTheConnectionPrefixOutOfTheError(t *testing.T) {
	root := t.TempDir()
	writeClientBinary(t, root, "postgresql18", "psql", "#!/bin/sh\n"+
		`echo 'psql: error: connection to server at "orders.cluster-abc.eu-central-1.rds.amazonaws.com" (10.4.2.7), `+
		`port 5432 failed: FATAL:  password authentication failed for user "dbtether_admin"' >&2`+"\nexit 2\n")

	_, err := resolvePsql(context.Background(), root,
		&serverProbe{Host: "orders.cluster-abc.eu-central-1.rds.amazonaws.com", Port: 5432, Username: "dbtether_admin", Database: "orders"},
		quietLogger())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server version probe failed")
	assert.Contains(t, err.Error(), `FATAL:  password authentication failed for user "dbtether_admin"`)
	for _, endpoint := range []string{"10.4.2.7", "rds.amazonaws.com"} {
		assert.NotContains(t, err.Error(), endpoint, "the endpoint belongs in the Job log, not in status.message")
	}
}

func writeClientBinary(t *testing.T, root, dir, name, script string) {
	t.Helper()

	path := filepath.Join(root, dir)
	if err := os.Mkdir(path, 0o750); err != nil && !os.IsExist(err) {
		t.Fatalf("failed to create %s: %v", path, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(path, name), []byte(script), 0o700))
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
