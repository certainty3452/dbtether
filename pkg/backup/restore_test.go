package backup

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/certainty3452/dbtether/pkg/storage"
)

func TestRestoreConfig_Validation(t *testing.T) {
	tests := []struct {
		name    string
		config  RestoreConfig
		isValid bool
	}{
		{
			name: "valid S3 config",
			config: RestoreConfig{
				Host:        "db.example.com",
				Port:        5432,
				Database:    "mydb",
				Username:    "admin",
				Password:    "secret",
				SourcePath:  "cluster/database/backup.sql.gz",
				StorageType: "s3",
				S3Config: &storage.S3Config{
					Bucket: "backups",
					Region: "us-east-1",
				},
				OnConflict: "fail",
			},
			isValid: true,
		},
		{
			name: "valid GCS config",
			config: RestoreConfig{
				Host:        "db.example.com",
				Port:        5432,
				Database:    "mydb",
				Username:    "admin",
				Password:    "secret",
				SourcePath:  "cluster/database/backup.sql.gz",
				StorageType: "gcs",
				GCSConfig: &storage.GCSConfig{
					Bucket: "backups",
				},
				OnConflict: "drop",
			},
			isValid: true,
		},
		{
			name: "valid Azure config",
			config: RestoreConfig{
				Host:        "db.example.com",
				Port:        5432,
				Database:    "mydb",
				Username:    "admin",
				Password:    "secret",
				SourcePath:  "cluster/database/backup.sql.gz",
				StorageType: "azure",
				AzureConfig: &storage.AzureConfig{
					Container:      "backups",
					StorageAccount: "mystorageaccount",
				},
				OnConflict: "fail",
			},
			isValid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Basic validation checks
			assert.NotEmpty(t, tt.config.Host)
			assert.NotEmpty(t, tt.config.Database)
			assert.NotEmpty(t, tt.config.SourcePath)
			assert.NotEmpty(t, tt.config.StorageType)
		})
	}
}

func TestOnConflict_Values(t *testing.T) {
	validValues := []string{"fail", "drop"}

	for _, val := range validValues {
		t.Run(val, func(t *testing.T) {
			config := RestoreConfig{
				OnConflict: val,
			}
			assert.Contains(t, validValues, config.OnConflict)
		})
	}
}

func TestQuoteIdentifier(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			input:    "simple_name",
			expected: `"simple_name"`,
		},
		{
			input:    "name with spaces",
			expected: `"name with spaces"`,
		},
		{
			input:    `name"with"quotes`,
			expected: `"name""with""quotes"`,
		},
		{
			input:    "UPPERCASE",
			expected: `"UPPERCASE"`,
		},
		{
			input:    "",
			expected: `""`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := quoteIdentifier(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestRestoreConfig_SSLMode(t *testing.T) {
	config := RestoreConfig{
		SSLMode: "require",
	}
	assert.Equal(t, "require", config.SSLMode)

	config.SSLMode = "disable"
	assert.Equal(t, "disable", config.SSLMode)

	config.SSLMode = "verify-full"
	assert.Equal(t, "verify-full", config.SSLMode)
}

func TestPsqlArgs_PasswordNotInArgv(t *testing.T) {
	cfg := &RestoreConfig{
		Host: "db.example.com", Port: 5432, Username: "admin",
		Password: "p@ssw0rd!secret", SSLMode: "require",
	}
	args := psqlArgs(cfg, "mydb")
	for _, a := range args {
		assert.NotContains(t, a, cfg.Password,
			"password must never appear in psql argv; goes through PGPASSWORD only")
		assert.NotContains(t, a, "sslmode=",
			"sslmode must go through PGSSLMODE env, not --set= (psql var) or argv")
	}
}

func TestPsqlEnv_PropagatesPasswordAndSSLMode(t *testing.T) {
	cfg := &RestoreConfig{Password: "secret", SSLMode: "require"}
	env := psqlEnv(cfg)
	var sawPwd, sawSSL bool
	for _, e := range env {
		if e == "PGPASSWORD=secret" {
			sawPwd = true
		}
		if e == "PGSSLMODE=require" {
			sawSSL = true
		}
	}
	assert.True(t, sawPwd, "PGPASSWORD missing")
	assert.True(t, sawSSL, "PGSSLMODE missing — sslmode would silently fall back to libpq default")
}

func TestPsqlEnv_SetsAConnectTimeout(t *testing.T) {
	assert.Contains(t, psqlEnv(&RestoreConfig{Password: "secret"}), "PGCONNECT_TIMEOUT=30",
		"without it a hung TCP connect keeps the Job running until its deadline")
}

func TestPsqlEnv_KeepsAConnectTimeoutFromTheEnvironment(t *testing.T) {
	t.Setenv("PGCONNECT_TIMEOUT", "5")

	env := psqlEnv(&RestoreConfig{Password: "secret"})

	assert.Contains(t, env, "PGCONNECT_TIMEOUT=5")
	assert.NotContains(t, env, "PGCONNECT_TIMEOUT=30")
}

func TestPsqlEnv_SkipsEmptySSLMode(t *testing.T) {
	cfg := &RestoreConfig{Password: "secret", SSLMode: ""}
	for _, e := range psqlEnv(cfg) {
		assert.NotEqual(t, "PGSSLMODE=", e,
			"empty PGSSLMODE would be rejected by libpq; skip the var entirely so libpq uses its default")
	}
}

func TestServerDiagnostic(t *testing.T) {
	const (
		endpoint = "orders.cluster-abc123.eu-central-1.rds.amazonaws.com"
		address  = "10.4.2.7"
	)
	connectionPrefix := fmt.Sprintf("psql: error: connection to server at %q (%s), port 5432 failed: ", endpoint, address)

	tests := []struct {
		name   string
		stderr string
		want   string
	}{
		{
			name: "connection refused",
			stderr: connectionPrefix + "Connection refused\n" +
				"\tIs the server running on that host and accepting TCP/IP connections?\n",
			want: "Connection refused",
		},
		{
			name:   "authentication failure",
			stderr: connectionPrefix + "FATAL:  password authentication failed for user \"dbtether_admin\"\n",
			want:   "FATAL:  password authentication failed for user \"dbtether_admin\"",
		},
		{
			name:   "missing database",
			stderr: connectionPrefix + "FATAL:  database \"orders\" does not exist\n",
			want:   "FATAL:  database \"orders\" does not exist",
		},
		{
			name:   "statement error",
			stderr: "ERROR:  database \"orders\" is being accessed by other users\n",
			want:   "ERROR:  database \"orders\" is being accessed by other users",
		},
		{
			name: "pg_dump connection refused",
			stderr: fmt.Sprintf("pg_dump: error: connection to server at %q (%s), port 5432 failed: Connection refused\n"+
				"\tIs the server running on that host and accepting TCP/IP connections?\n", endpoint, address),
			want: "Connection refused",
		},
		{
			name: "pg_dump authentication failure",
			stderr: fmt.Sprintf("pg_dump: error: connection to server at %q (%s), port 5432 failed: "+
				"FATAL:  password authentication failed for user \"dbtether_admin\"\n", endpoint, address),
			want: "FATAL:  password authentication failed for user \"dbtether_admin\"",
		},
		{
			name: "pg_dump query failure keeps the server message and drops the quoted query",
			stderr: "pg_dump: error: query failed: ERROR:  permission denied for table secret\n" +
				"pg_dump: detail: Query was: LOCK TABLE public.secret IN ACCESS SHARE MODE\n" +
				"pg_dump: hint: Try granting SELECT.\n",
			want: "ERROR:  permission denied for table secret",
		},
		{
			name:   "pg_dump version mismatch",
			stderr: "pg_dump: error: aborting because of server version mismatch\n",
			want:   "aborting because of server version mismatch",
		},
		{
			name:   "pg_dump found nothing to dump",
			stderr: "pg_dump: error: no matching tables were found\n",
			want:   "no matching tables were found",
		},
		{
			name:   "pg_dump detail carrying the server message",
			stderr: "pg_dump: detail: Error message from server: ERROR:  permission denied for table secret\n",
			want:   "ERROR:  permission denied for table secret",
		},
		{
			name: "detail, hint and quoted line dropped",
			stderr: "ERROR:  invalid input syntax for type integer: \"x\"\n" +
				"LINE 1: INSERT INTO t VALUES ('x')\n" +
				"                              ^\n" +
				"DETAIL:  Failing row contains (x).\n" +
				"HINT:  Use a numeric literal.\n",
			want: "ERROR:  invalid input syntax for type integer: \"x\"",
		},
		{
			name:   "two failures collapse into one line",
			stderr: "ERROR:  database \"orders\" does not exist\nERROR:  current transaction is aborted\n",
			want:   "ERROR:  database \"orders\" does not exist; ERROR:  current transaction is aborted",
		},
		{
			name:   "unresolvable host name carries no server message",
			stderr: fmt.Sprintf("psql: error: could not translate host name %q to address: nodename nor servname provided\n", endpoint),
			want:   "",
		},
		{
			name:   "empty",
			stderr: "",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serverDiagnostic(tt.stderr)

			assert.Equal(t, tt.want, got)
			assert.NotContains(t, got, endpoint, "the endpoint must not travel back into status.message")
			assert.NotContains(t, got, address, "the server address must not travel back into status.message")
		})
	}
}

func TestServerDiagnosticIsBounded(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "psql", line: "ERROR:  relation \"table_%04d\" does not exist\n", want: `ERROR:  relation "table_0000" does not exist`},
		{
			name: "pg_dump",
			line: "pg_dump: error: query failed: ERROR:  permission denied for table_%04d\n",
			want: "ERROR:  permission denied for table_0000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr strings.Builder
			for i := range 5000 {
				_, err := fmt.Fprintf(&stderr, tt.line, i)
				require.NoError(t, err)
			}
			require.Greater(t, stderr.Len(), 200<<10)

			got := serverDiagnostic(stderr.String())

			assert.LessOrEqual(t, len(got), psqlStderrTailBytes)
			assert.True(t, strings.HasPrefix(got, tt.want),
				"a short command fails on its first statement, so the head is the diagnostic part")
		})
	}
}

func TestServerDiagnosticCutsOnARuneBoundary(t *testing.T) {
	got := serverDiagnostic("ERROR: " + strings.Repeat("…", psqlStderrTailBytes))

	assert.LessOrEqual(t, len(got), psqlStderrTailBytes)
	assert.True(t, utf8.ValidString(got), "status.message would carry a broken rune")
}

func TestWithDiagnostic(t *testing.T) {
	cause := errors.New("exit status 2")

	assert.Equal(t, "exit status 2", withDiagnostic(cause, "").Error())
	assert.Equal(t, "exit status 2: FATAL:  database \"orders\" does not exist",
		withDiagnostic(cause, "FATAL:  database \"orders\" does not exist").Error())
	assert.ErrorIs(t, withDiagnostic(cause, "FATAL:  boom"), cause)
}

func TestQuoteLiteral(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"mydb", "'mydb'"},
		{"my'db", "'my''db'"},
		{"'; DROP DATABASE postgres; --", "'''; DROP DATABASE postgres; --'"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			assert.Equal(t, tt.want, quoteLiteral(tt.in))
		})
	}
}

func TestTailWriter_KeepsTheLastLines(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		writes []string
		want   string
	}{
		{name: "exactly the limit", limit: 8, writes: []string{"psql:abc\n"}, want: "psql:abc"},
		{name: "line assembled across writes", limit: 8, writes: []string{"psq", "l:abc\n"}, want: "psql:abc"},
		{name: "line longer than the limit keeps its head", limit: 6, writes: []string{"psql:abcdefgh\n"}, want: "psql:a"},
		{name: "lines overflowing in total evict the oldest", limit: 12, writes: []string{"psql:a\n", "psql:b\n", "psql:c\n"}, want: "psql:b; psql:c"},
		{name: "trailing line without a newline kept", limit: 12, writes: []string{"psql:a\n", "psql:b"}, want: "psql:a; psql:b"},
		{name: "unprefixed lines dropped", limit: 64, writes: []string{"ERROR:  boom\n", "CONTEXT:  COPY notes, line 1: \"4111111111111111\"\n"}, want: ""},
		{name: "blank lines only", limit: 8, writes: []string{"\n", "\r\n", "   \n"}, want: ""},
		{name: "nothing written", limit: 8, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &tailWriter{limit: tt.limit}
			for _, chunk := range tt.writes {
				n, err := w.Write([]byte(chunk))
				require.NoError(t, err)
				assert.Equal(t, len(chunk), n, "a short write would make psql fail on its own stderr")
			}
			assert.Equal(t, tt.want, w.String())
		})
	}
}

func TestTailWriter_String(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "crlf line ending trimmed and the unprefixed line dropped",
			input: "psql:<stdin>:12: ERROR:  relation \"notes\" does not exist\r\nLINE 1: SELECT 1\n",
			want:  "psql:<stdin>:12: ERROR:  relation \"notes\" does not exist",
		},
		{
			name: "detail and context lines dropped",
			input: "psql:<stdin>:12: ERROR:  invalid input syntax for type integer\n" +
				"CONTEXT:  COPY notes, line 1: \"card 4111111111111111\"\n" +
				"DETAIL:  Failing row contains (card 4111111111111111).\n",
			want: "psql:<stdin>:12: ERROR:  invalid input syntax for type integer",
		},
		{
			name:  "only a context line",
			input: "CONTEXT:  COPY notes, line 1: \"card 4111111111111111\"\n",
			want:  "",
		},
		{
			name:  "unresolvable host name dropped",
			input: "psql: error: could not translate host name \"orders.cluster-abc123.eu-central-1.rds.amazonaws.com\" to address: nodename nor servname provided\n",
			want:  "",
		},
		{
			name:  "blank line between diagnostics",
			input: "psql:<stdin>:12: ERROR:  relation \"notes\" does not exist\n\npsql:<stdin>:13: ERROR:  current transaction is aborted\n",
			want:  "psql:<stdin>:12: ERROR:  relation \"notes\" does not exist; psql:<stdin>:13: ERROR:  current transaction is aborted",
		},
		{
			name:  "empty buffer",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &tailWriter{limit: psqlStderrTailBytes}
			_, err := w.Write([]byte(tt.input))
			require.NoError(t, err)
			assert.Equal(t, tt.want, w.String())
		})
	}
}

func TestTailWriter_StripsTheConnectionPrefixFromAPsqlError(t *testing.T) {
	const (
		endpoint = "orders.cluster-abc123.eu-central-1.rds.amazonaws.com"
		address  = "10.4.2.7"
	)

	w := &tailWriter{limit: psqlStderrTailBytes}
	_, err := fmt.Fprintf(w, "psql: error: connection to server at %q (%s), port 5432 failed: "+
		"FATAL:  remaining connection slots are reserved for non-replication superuser connections\n", endpoint, address)
	require.NoError(t, err)

	got := w.String()

	assert.Equal(t, "FATAL:  remaining connection slots are reserved for non-replication superuser connections", got)
	assert.NotContains(t, got, endpoint, "a connection failure before the first statement must not put the endpoint in status.message")
	assert.NotContains(t, got, address)
}

func TestTailWriter_KeepsTheDumpPositionOfAStatementError(t *testing.T) {
	w := &tailWriter{limit: psqlStderrTailBytes}

	_, err := w.Write([]byte("psql:<stdin>:12: ERROR:  relation \"notes\" does not exist\n"))
	require.NoError(t, err)

	assert.Equal(t, `psql:<stdin>:12: ERROR:  relation "notes" does not exist`, w.String())
}

func TestTailWriter_OverLongContextKeepsTheError(t *testing.T) {
	w := &tailWriter{limit: psqlStderrTailBytes}
	row := strings.Repeat("x", 4*psqlStderrTailBytes)

	_, err := w.Write([]byte("psql:<stdin>:12: ERROR:  invalid input syntax for type integer\n" +
		"CONTEXT:  COPY notes, line 1: \"" + row + "\"\n"))
	require.NoError(t, err)

	assert.Equal(t, "psql:<stdin>:12: ERROR:  invalid input syntax for type integer", w.String(),
		"a COPY row longer than the limit must not evict the diagnostic it explains")
}

func TestTailWriter_OldestNoticesEvictedErrorSurvives(t *testing.T) {
	w := &tailWriter{limit: psqlStderrTailBytes}
	for i := range 200 {
		_, err := fmt.Fprintf(w, "psql:<stdin>:%d: NOTICE:  step %03d skipped\n", i, i)
		require.NoError(t, err)
	}

	_, err := w.Write([]byte("psql:<stdin>:4711: ERROR:  relation \"notes\" already exists\n"))
	require.NoError(t, err)

	got := w.String()
	assert.Contains(t, got, `ERROR:  relation "notes" already exists`)
	assert.Contains(t, got, "step 199")
	assert.NotContains(t, got, "step 000")
	assert.Less(t, len(got), 2*psqlStderrTailBytes)
}

func TestTailWriter_OverLongErrorLineCutsOnARuneBoundary(t *testing.T) {
	w := &tailWriter{limit: 10}

	_, err := w.Write([]byte("psql:" + strings.Repeat("é", 10) + "\n"))
	require.NoError(t, err)

	got := w.String()
	assert.True(t, utf8.ValidString(got), "status.message would carry a broken rune")
	assert.Equal(t, "psql:éé", got)
}

func TestTailWriter_OverLongErrorLineKeepsItsHead(t *testing.T) {
	w := &tailWriter{limit: psqlStderrTailBytes}
	line := "psql:<stdin>:12: ERROR:  " + strings.Repeat("y", 4*psqlStderrTailBytes)

	_, err := w.Write([]byte(line + "\n"))
	require.NoError(t, err)

	assert.Equal(t, line[:psqlStderrTailBytes], w.String())
}

func TestDecompressDump(t *testing.T) {
	const dump = "CREATE TABLE public.notes (body text);\n"

	tests := []struct {
		name   string
		stream []byte
		want   string
		wantGz bool
	}{
		{name: "gzip stream stored under a .sql key", stream: gzipped(t, dump), want: dump, wantGz: true},
		{name: "plain stream stored under a .gz key", stream: []byte(dump), want: dump},
		{name: "empty stream", stream: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, closer, err := decompressDump(bytes.NewReader(tt.stream))
			require.NoError(t, err)
			assert.Equal(t, tt.wantGz, closer != nil, "gzip is detected by the magic bytes, never by the key")
			if closer != nil {
				defer func() { require.NoError(t, closer.Close()) }()
			}

			got, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

func TestProbeDatabase(t *testing.T) {
	tests := []struct {
		onConflict string
		want       string
	}{
		{onConflict: onConflictDrop, want: "postgres"},
		{onConflict: onConflictFail, want: "orders"},
	}

	for _, tt := range tests {
		t.Run(tt.onConflict, func(t *testing.T) {
			assert.Equal(t, tt.want, probeDatabase(&RestoreConfig{Database: "orders", OnConflict: tt.onConflict}))
		})
	}
}

func TestOpenDumpStream(t *testing.T) {
	dump := manyStatements(5000)

	tests := []struct {
		name    string
		stream  []byte
		want    string
		wantErr string
	}{
		{name: "empty object", wantErr: "backup contains no SQL statements"},
		{name: "folder marker of blank lines", stream: []byte("\n \n\t\n"), wantErr: "backup contains no SQL statements"},
		{
			name:    "header comments only",
			stream:  []byte("--\n-- PostgreSQL database dump\n--\n\n-- Dumped from database version 16.15\n"),
			wantErr: "backup contains no SQL statements",
		},
		{name: "gzip of an empty stream", stream: gzipped(t, ""), wantErr: "backup contains no SQL statements"},
		{
			name:    "only statements the filter drops",
			stream:  []byte("BEGIN;\nSET transaction_timeout = 0;\nCOMMIT;\n"),
			wantErr: "backup contains no SQL statements",
		},
		{name: "bare terminators", stream: []byte(";;;\n"), wantErr: "backup contains no SQL statements"},
		{name: "blank statements", stream: []byte("  ;\n;\n"), wantErr: "backup contains no SQL statements"},
		{
			name:    "custom-format archive",
			stream:  []byte("PGDMP\x00\x03\x0e\x00\x00;\x00;\x00;junk"),
			wantErr: "backup is a pg_dump custom-format archive; only plain-format SQL dumps can be restored",
		},
		{
			name:    "gzipped custom-format archive",
			stream:  gzipped(t, "PGDMP\x00\x03\x0e\x00\x00;\x00;\x00;junk"),
			wantErr: "backup is a pg_dump custom-format archive; only plain-format SQL dumps can be restored",
		},
		{
			name:    "binary payload without the archive magic",
			stream:  []byte("\x89PNG\x00\x1a\n binary ; payload"),
			wantErr: "backup is not a plain-format SQL dump",
		},
		{name: "a single statement", stream: []byte("SET statement_timeout = 0;\n"), want: "SET statement_timeout = 0;\n"},
		{name: "a dump longer than the peek buffer", stream: []byte(dump), want: dump},
		{name: "a gzipped dump", stream: gzipped(t, dump), want: dump},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader, closer, err := openDumpStream(bytes.NewReader(tt.stream))
			if closer != nil {
				defer func() { _ = closer.Close() }()
			}

			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}

			require.NoError(t, err)
			got, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got), "the peeked head and the rest must stream back byte-identical")
		})
	}
}

func TestOpenDumpStreamJudgesTheHeadRegardlessOfReadSizes(t *testing.T) {
	const dump = "SELECT 1;\nCOPY t (a) FROM stdin;\nx\x00y\n\\.\n"

	for _, size := range []int{1, 9, 16, 1 << 20} {
		t.Run(fmt.Sprintf("%d bytes per read", size), func(t *testing.T) {
			reader, _, err := openDumpStream(&chunkedReader{data: dump, size: size})

			require.NoError(t, err, "a NUL past the first statement must not depend on how the source chunks its reads")
			got, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Equal(t, dump, string(got))
		})
	}
}

type chunkedReader struct {
	data string
	size int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, io.EOF
	}
	if len(p) > r.size {
		p = p[:r.size]
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestOpenDumpStreamRejectsAStatementlessHead(t *testing.T) {
	const line = "-- a comment line that never reaches a statement\n"

	_, _, err := openDumpStream(strings.NewReader(strings.Repeat(line, peekLimitBytes/len(line)+2)))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup contains no SQL statement in its first")
}

func TestOpenDumpStreamFailsOnTruncationBeforeFirstStatement(t *testing.T) {
	_, _, err := openDumpStream(&errAfterReader{data: "CREATE TABLE public.t (id int", err: io.ErrUnexpectedEOF})

	require.Error(t, err)
	assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

func TestPeekFirstStatementStopsAtTheFirstStatement(t *testing.T) {
	script := newRestoreScript(io.MultiReader(
		strings.NewReader("SELECT 1;\nSELECT 2;\nSELECT 3;\n"),
		&errAfterReader{err: errors.New("read beyond the first statement")}))

	head, err := peekFirstStatement(script)
	require.NoError(t, err, "the peek must stop on the first completed statement, not drain the dump")

	peeked, err := io.ReadAll(head)
	require.NoError(t, err)
	assert.Equal(t, "SELECT 1;\n", string(peeked))
}

func manyStatements(count int) string {
	var dump strings.Builder
	dump.WriteString("SET statement_timeout = 0;\n")
	for i := range count {
		_, _ = fmt.Fprintf(&dump, "INSERT INTO public.notes VALUES (%d);\n", i)
	}
	return dump.String()
}

func TestRunRestoreRejectsUnknownOnConflictBeforeDownloading(t *testing.T) {
	cfg := &RestoreConfig{
		Host: "127.0.0.1", Port: 1, Database: "mydb", Username: "admin",
		SourcePath:  "cluster/database/backup.sql.gz",
		StorageType: "s3",
		S3Config:    &storage.S3Config{Bucket: "backups", Region: "us-east-1", Endpoint: "http://127.0.0.1:1"},
		OnConflict:  "truncate",
		Logger:      quietLogger(),
	}

	err := RunRestore(context.Background(), cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), `unsupported onConflict "truncate"`)
	assert.NotContains(t, err.Error(), "download", "the strategy is rejected before anything is fetched")
}

func gzipped(t *testing.T, content string) []byte {
	t.Helper()

	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	_, err := io.WriteString(writer, content)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return buf.Bytes()
}
