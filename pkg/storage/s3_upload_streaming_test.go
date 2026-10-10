package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeS3 struct {
	mu      sync.Mutex
	object  []byte
	stored  bool
	parts   map[int][]byte
	srv     *httptest.Server
	deniedN int
}

func newFakeS3DenyingTagging(t *testing.T) *fakeS3 {
	f := &fakeS3{parts: map[int][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("X-Amz-Tagging") != "" {
		f.mu.Lock()
		f.deniedN++
		f.mu.Unlock()
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>AccessDenied</Code><Message>denied</Message></Error>`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		_, _ = fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>u1</UploadId></InitiateMultipartUploadResult>`)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.parts[n] = decodeAwsChunked(r, body)
		w.Header().Set("ETag", fmt.Sprintf(`"p%d"`, n))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		nums := make([]int, 0, len(f.parts))
		for n := range f.parts {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		var all []byte
		for _, n := range nums {
			all = append(all, f.parts[n]...)
		}
		f.object, f.stored = all, true
		_, _ = fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"c"</ETag></CompleteMultipartUploadResult>`)
	case r.Method == http.MethodPut:
		f.object, f.stored = decodeAwsChunked(r, body), true
		w.Header().Set("ETag", `"o"`)
	}
}

func decodeAwsChunked(r *http.Request, body []byte) []byte {
	if r.Header.Get("X-Amz-Decoded-Content-Length") == "" {
		return body
	}
	var out []byte
	for len(body) > 0 {
		i := bytes.Index(body, []byte("\r\n"))
		if i < 0 {
			break
		}
		size, err := strconv.ParseInt(string(bytes.SplitN(body[:i], []byte(";"), 2)[0]), 16, 64)
		if err != nil || size == 0 {
			break
		}
		body = body[i+2:]
		out = append(out, body[:size]...)
		body = body[size+2:]
	}
	return out
}

func TestS3UploadStreaming_TaggingDeniedNeverStoresTruncatedObject(t *testing.T) {
	const partSize = 64 * 1024 * 1024
	for _, tc := range []struct {
		name string
		size int
	}{
		{"below part size", 1024 * 1024},
		{"above part size", partSize + 5*1024*1024},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3DenyingTagging(t)
			c, err := NewS3Client(context.Background(), &S3Config{
				Bucket: "b", Region: "us-east-1", Endpoint: f.srv.URL,
				AccessKey: "AKIATEST", SecretKey: "secret",
			}, nil)
			require.NoError(t, err)

			input := make([]byte, tc.size)
			_, _ = rand.Read(input)
			pr, pw := io.Pipe()
			go func() {
				_, werr := pw.Write(input)
				_ = pw.CloseWithError(werr)
			}()

			uploadErr := c.UploadStreaming(context.Background(), "k.sql.gz", pr, &ObjectTags{Database: "d", CreatedBy: "dbtether"})

			f.mu.Lock()
			defer f.mu.Unlock()
			require.Positive(t, f.deniedN, "fake S3 must have denied the tagged upload")
			if uploadErr != nil {
				return
			}
			require.True(t, f.stored, "nil error but nothing stored")
			assert.Equal(t, len(input), len(f.object), "nil error with stored object of different size")
			assert.True(t, bytes.Equal(input, f.object), "stored object differs from input")
		})
	}
}
