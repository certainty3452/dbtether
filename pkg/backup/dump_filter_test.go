package backup

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func TestRestoreScriptFiltering(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "single-line extension comment dropped",
			input: "CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;\nCOMMENT ON EXTENSION pg_trgm IS 'trigrams';\nCREATE TABLE public.t (id int);\n",
			want:  "CREATE EXTENSION IF NOT EXISTS pg_trgm WITH SCHEMA public;\nCREATE TABLE public.t (id int);\n",
		},
		{
			name:  "multi-line extension comment dropped",
			input: "COMMENT ON EXTENSION pg_trgm IS 'line one;\nline two';\nCREATE TABLE public.t (id int);\n",
			want:  "CREATE TABLE public.t (id int);\n",
		},
		{
			name:  "doubled quote inside the dropped literal",
			input: "COMMENT ON EXTENSION pg_trgm IS 'it''s; procedural';\nCREATE TABLE public.t (id int);\n",
			want:  "CREATE TABLE public.t (id int);\n",
		},
		{
			name:  "transaction control around the large objects dropped",
			input: "BEGIN;\n\nSELECT pg_catalog.lo_open('16564', 131072);\nSELECT pg_catalog.lo_close(0);\n\nCOMMIT;\n",
			want:  "\nSELECT pg_catalog.lo_open('16564', 131072);\nSELECT pg_catalog.lo_close(0);\n\n",
		},
		{
			name:  "transaction timeout dropped",
			input: "SET lock_timeout = 0;\nSET transaction_timeout = 0;\nSET row_security = off;\n",
			want:  "SET lock_timeout = 0;\nSET row_security = off;\n",
		},
		{
			name:  "transaction timeout inside a function body kept",
			input: transactionTimeoutBody,
			want:  transactionTimeoutBody,
		},
		{
			name: "transaction timeout as a copy data row kept",
			input: "COPY public.notes (body) FROM stdin;\n" +
				"SET transaction_timeout = 0;\n" +
				"\\.\n" +
				"SET transaction_timeout = 0;\n" +
				"CREATE INDEX notes_body ON public.notes (body);\n",
			want: "COPY public.notes (body) FROM stdin;\n" +
				"SET transaction_timeout = 0;\n" +
				"\\.\n" +
				"CREATE INDEX notes_body ON public.notes (body);\n",
		},
		{
			name:  "comment on table kept",
			input: "COMMENT ON TABLE public.t IS 'documented';\nCOMMENT ON COLUMN public.t.id IS 'pk';\n",
			want:  "COMMENT ON TABLE public.t IS 'documented';\nCOMMENT ON COLUMN public.t.id IS 'pk';\n",
		},
		{
			name:  "restrict meta-commands kept",
			input: "\\restrict pXGyzM6UYDSVxmMl77\nBEGIN;\n\\unrestrict pXGyzM6UYDSVxmMl77\n",
			want:  "\\restrict pXGyzM6UYDSVxmMl77\n\\unrestrict pXGyzM6UYDSVxmMl77\n",
		},
		{
			name:  "statement lines inside an untagged function body kept",
			input: untaggedBody,
			want:  untaggedBody,
		},
		{
			name:  "statement lines inside a tagged function body kept",
			input: taggedBody,
			want:  taggedBody,
		},
		{
			name:  "statement lines inside a multi-line literal kept",
			input: multiLineLiteral,
			want:  multiLineLiteral,
		},
		{
			name:  "statement lines inside a line comment kept",
			input: "-- BEGIN;\n-- COMMENT ON EXTENSION pg_trgm IS 'x';\nCREATE TABLE public.t (id int);\n",
			want:  "-- BEGIN;\n-- COMMENT ON EXTENSION pg_trgm IS 'x';\nCREATE TABLE public.t (id int);\n",
		},
		{
			name: "copy data rows kept and the terminator restores dropping",
			input: "COPY public.notes (body) FROM stdin;\n" +
				"COMMENT ON EXTENSION pg_trgm IS 'a data row';\n" +
				"BEGIN;\n" +
				"COMMIT;\n" +
				"\\.\n" +
				"BEGIN;\n" +
				"CREATE INDEX notes_body ON public.notes (body);\n",
			want: "COPY public.notes (body) FROM stdin;\n" +
				"COMMENT ON EXTENSION pg_trgm IS 'a data row';\n" +
				"BEGIN;\n" +
				"COMMIT;\n" +
				"\\.\n" +
				"CREATE INDEX notes_body ON public.notes (body);\n",
		},
		{
			name:  "quoted identifier does not open a literal",
			input: "CREATE TABLE public.\"it's\" (id int);\nBEGIN;\nCREATE TABLE public.t (id int);\n",
			want:  "CREATE TABLE public.\"it's\" (id int);\nCREATE TABLE public.t (id int);\n",
		},
		{
			name:  "quoted identifier does not open a dollar quote",
			input: "CREATE TABLE public.\"a$b$c\" (id int);\nBEGIN;\nCREATE TABLE public.t (id int);\n",
			want:  "CREATE TABLE public.\"a$b$c\" (id int);\nCREATE TABLE public.t (id int);\n",
		},
		{
			name:  "doubled quote inside an identifier",
			input: "CREATE TABLE public.\"x\"\"y\" (id int);\nCOMMIT;\nCREATE TABLE public.t (id int);\n",
			want:  "CREATE TABLE public.\"x\"\"y\" (id int);\nCREATE TABLE public.t (id int);\n",
		},
		{
			name: "quoted identifier does not open a comment in a copy header",
			input: "COPY public.\"a--b\" (id) FROM stdin;\n" +
				"1\n" +
				"BEGIN;\n" +
				"\\.\n" +
				"COMMIT;\n" +
				"CREATE INDEX ab_id ON public.\"a--b\" (id);\n",
			want: "COPY public.\"a--b\" (id) FROM stdin;\n" +
				"1\n" +
				"BEGIN;\n" +
				"\\.\n" +
				"CREATE INDEX ab_id ON public.\"a--b\" (id);\n",
		},
		{
			name:  "copy to stdout is not a data block",
			input: "COPY public.notes (body) TO stdout;\nBEGIN;\n",
			want:  "COPY public.notes (body) TO stdout;\n",
		},
		{
			name:  "indented transaction control kept",
			input: "  BEGIN;\n\tCOMMIT;\n",
			want:  "  BEGIN;\n\tCOMMIT;\n",
		},
		{
			name:  "missing trailing newline preserved",
			input: "COMMENT ON EXTENSION pg_trgm IS 'x';\nCREATE TABLE public.t (id int);",
			want:  "CREATE TABLE public.t (id int);",
		},
		{
			name:  "dropped statement without trailing newline",
			input: "CREATE TABLE public.t (id int);\nCOMMENT ON EXTENSION pg_trgm IS 'x';",
			want:  "CREATE TABLE public.t (id int);\n",
		},
		{
			name:  "crlf line endings preserved",
			input: "COMMENT ON EXTENSION pg_trgm IS 'x';\r\nCREATE TABLE public.t (id int);\r\n",
			want:  "CREATE TABLE public.t (id int);\r\n",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readers := map[string]func() io.Reader{
				"plain":    func() io.Reader { return strings.NewReader(tt.input) },
				"one-byte": func() io.Reader { return iotest.OneByteReader(strings.NewReader(tt.input)) },
			}
			for readerName, newReader := range readers {
				t.Run(readerName, func(t *testing.T) {
					got, err := io.ReadAll(newRestoreScript(newReader()))
					if err != nil {
						t.Fatalf("ReadAll: %v", err)
					}
					if string(got) != tt.want {
						t.Errorf("filtered output is %q, want %q", got, tt.want)
					}
				})

				t.Run(readerName+" three-byte destination", func(t *testing.T) {
					got, err := readInChunks(t, newRestoreScript(newReader()), 3)
					if err != nil {
						t.Fatalf("Read: %v", err)
					}
					if got != tt.want {
						t.Errorf("filtered output is %q, want %q", got, tt.want)
					}
				})
			}
		})
	}
}

const untaggedBody = `CREATE FUNCTION public.tricky() RETURNS integer
    LANGUAGE plpgsql
    AS $$
BEGIN
IF false THEN
COMMENT ON EXTENSION pg_trgm IS 'inside body';
BEGIN;
COMMIT;
END IF;
RETURN 1;
END
$$;
CREATE TABLE public.t (id int);
`

const taggedBody = `CREATE FUNCTION public.tricky() RETURNS integer
    LANGUAGE plpgsql
    AS $fn$
BEGIN
IF false THEN
COMMENT ON EXTENSION pg_trgm IS 'inside body';
BEGIN;
COMMIT;
END IF;
RETURN 1;
END
$fn$;
CREATE TABLE public.t (id int);
`

const transactionTimeoutBody = `CREATE FUNCTION public.slow() RETURNS integer
    LANGUAGE plpgsql
    AS $$
BEGIN
SET transaction_timeout = 0;
RETURN 1;
END
$$;
CREATE TABLE public.t (id int);
`

const multiLineLiteral = `COMMENT ON TABLE public.notes IS 'first
BEGIN;
COMMIT;
COMMENT ON EXTENSION pg_trgm IS ''quoted'';
last';
CREATE TABLE public.t (id int);
`

func TestRestoreScriptStreamsWithoutBufferingLines(t *testing.T) {
	const chunk = 4 << 10
	const line = 8 << 20

	dump := "SELECT '" + strings.Repeat("x", line) + "';\n" + strings.Repeat("SELECT 1;\n", 1024)
	script := newRestoreScript(strings.NewReader(dump))

	buf := make([]byte, chunk)
	var got strings.Builder
	reads := 0
	for {
		n, err := script.Read(buf)
		got.Write(buf[:n])
		reads++
		if reads == 2 && n != len(buf) {
			t.Errorf("mid-stream read returned %d bytes, want the full %d: the filter is emitting line by line", n, len(buf))
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}

	if got.String() != dump {
		t.Errorf("filtered output of %d bytes differs from the %d-byte input", got.Len(), len(dump))
	}
}

func TestRestoreScriptAbortsOnTruncatedStream(t *testing.T) {
	streamErr := errors.New("unexpected EOF")

	tests := []struct {
		name  string
		input string
		err   error
		want  string
	}{
		{
			name:  "outside a copy block",
			input: "CREATE TABLE public.survivor (id int);\n",
			err:   streamErr,
			want:  "CREATE TABLE public.survivor (id int);\n" + abortStatement("dbtether: backup stream truncated: unexpected EOF"),
		},
		{
			name:  "inside a copy block",
			input: "COPY public.notes (body) FROM stdin;\nfirst row\n",
			err:   streamErr,
			want: "COPY public.notes (body) FROM stdin;\nfirst row\n\\.\n" +
				abortStatement("dbtether: backup stream truncated: unexpected EOF"),
		},
		{
			name:  "inside a copy row",
			input: "COPY public.notes (body) FROM stdin;\nfirst ro",
			err:   streamErr,
			want: "COPY public.notes (body) FROM stdin;\nfirst ro\n\\.\n" +
				abortStatement("dbtether: backup stream truncated: unexpected EOF"),
		},
		{
			name:  "partial last line preserved",
			input: "CREATE TABLE public.survivor (id int);\nINSERT INTO public.survivor VAL",
			err:   streamErr,
			want: "CREATE TABLE public.survivor (id int);\nINSERT INTO public.survivor VAL\n" +
				abortStatement("dbtether: backup stream truncated: unexpected EOF"),
		},
		{
			name:  "quotes doubled, dollars stripped and newlines folded out of the message",
			input: "",
			err:   errors.New("gzip: invalid header 'x'\n$$ read aborted"),
			want:  abortStatement("dbtether: backup stream truncated: gzip: invalid header ''x'' read aborted"),
		},
		{
			name:  "dropped statement still aborts",
			input: "COMMENT ON EXTENSION pg_trgm IS 'unfinished\n",
			err:   streamErr,
			want:  abortStatement("dbtether: backup stream truncated: unexpected EOF"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := io.ReadAll(newRestoreScript(&errAfterReader{data: tt.input, err: tt.err}))
			if !errors.Is(err, tt.err) {
				t.Fatalf("ReadAll returned %v, want %v", err, tt.err)
			}
			if string(got) != tt.want {
				t.Errorf("output is %q, want %q", got, tt.want)
			}

			chunked, err := readInChunks(t, newRestoreScript(&errAfterReader{data: tt.input, err: tt.err}), 3)
			if !errors.Is(err, tt.err) {
				t.Fatalf("chunked read returned %v, want %v", err, tt.err)
			}
			if chunked != tt.want {
				t.Errorf("chunked output is %q, want %q", chunked, tt.want)
			}
		})
	}
}

func TestRestoreScriptZeroLengthReadHoldsBackTheError(t *testing.T) {
	streamErr := errors.New("unexpected EOF")
	const statement = "CREATE TABLE public.survivor (id int);\n"
	script := newRestoreScript(&errAfterReader{data: statement, err: streamErr})

	var got strings.Builder
	buf := make([]byte, 64)
	for i := range 2 {
		n, err := script.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
	}

	if n, err := script.Read(nil); n != 0 || err != nil {
		t.Errorf("zero-length read returned (%d, %v) with the abort trailer still queued, want (0, <nil>)", n, err)
	}

	rest, err := io.ReadAll(script)
	if !errors.Is(err, streamErr) {
		t.Fatalf("ReadAll returned %v, want %v", err, streamErr)
	}
	got.Write(rest)

	want := statement + abortStatement("dbtether: backup stream truncated: unexpected EOF")
	if got.String() != want {
		t.Errorf("output is %q, want %q", got.String(), want)
	}
}

func TestRestoreScriptKeepsCleanEOFUntouched(t *testing.T) {
	dump := "COPY public.notes (body) FROM stdin;\nfirst row\n\\.\nCREATE INDEX notes_body ON public.notes (body);\n"

	got, err := io.ReadAll(newRestoreScript(strings.NewReader(dump)))
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(got) != dump {
		t.Errorf("output is %q, want %q", got, dump)
	}
}

func abortStatement(message string) string {
	return "DO $dbtether$ BEGIN RAISE EXCEPTION '" + message + "'; END $dbtether$;\n"
}

func readInChunks(t *testing.T, r io.Reader, size int) (string, error) {
	t.Helper()

	var out strings.Builder
	buf := make([]byte, size)
	for {
		n, err := r.Read(buf)
		out.Write(buf[:n])
		if errors.Is(err, io.EOF) {
			return out.String(), nil
		}
		if err != nil {
			return out.String(), err
		}
	}
}

type errAfterReader struct {
	data string
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}
