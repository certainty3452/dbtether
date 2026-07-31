package postgres

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsDatabaseNotExistError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "generic error",
			err:      errors.New("some error"),
			expected: false,
		},
		{
			name: "database does not exist (3D000)",
			err: &pgconn.PgError{
				Code: "3D000",
			},
			expected: true,
		},
		{
			name: "wrapped database does not exist error",
			err: errors.Join(errors.New("connection failed"), &pgconn.PgError{
				Code: "3D000",
			}),
			expected: true,
		},
		{
			name: "different postgres error code",
			err: &pgconn.PgError{
				Code: "42P01", // undefined_table
			},
			expected: false,
		},
		{
			name: "syntax error code",
			err: &pgconn.PgError{
				Code: "42601", // syntax_error
			},
			expected: false,
		},
		{
			name: "permission denied error code",
			err: &pgconn.PgError{
				Code: "42501", // insufficient_privilege
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isDatabaseNotExistError(tt.err)
			if result != tt.expected {
				t.Errorf("isDatabaseNotExistError(%v) = %v, expected %v", tt.err, result, tt.expected)
			}
		})
	}
}

func TestJoinAllowedPrivileges(t *testing.T) {
	tests := []struct {
		name    string
		input   []TablePrivilege
		want    string
		wantErr bool
		wantBad string
	}{
		{name: "single SELECT", input: []TablePrivilege{"SELECT"}, want: "SELECT"},
		{name: "canonicalises case", input: []TablePrivilege{"select", "Insert"}, want: "SELECT, INSERT"},
		{name: "trims whitespace", input: []TablePrivilege{" SELECT "}, want: "SELECT"},
		{name: "full allowlist", input: []TablePrivilege{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "USAGE"},
			want: "SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER, USAGE"},
		{name: "rejects SQL injection payload",
			input: []TablePrivilege{"SELECT; CREATE ROLE evil SUPERUSER --"}, wantErr: true,
			wantBad: "SELECT; CREATE ROLE evil SUPERUSER --"},
		{name: "rejects bogus privilege", input: []TablePrivilege{"DROP"}, wantErr: true, wantBad: "DROP"},
		{name: "rejects empty string", input: []TablePrivilege{""}, wantErr: true, wantBad: ""},
		{name: "rejects whitespace only", input: []TablePrivilege{" "}, wantErr: true, wantBad: " "},
		{name: "rejects empty slice", input: []TablePrivilege{}, wantErr: true},
		{name: "rejects nil slice", input: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := joinAllowedPrivileges(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (result: %q)", got)
				}
				if !errors.Is(err, ErrInvalidTablePrivilege) {
					t.Fatalf("expected ErrInvalidTablePrivilege, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error is not transient",
			err:      nil,
			expected: false,
		},
		{
			name:     "any error is transient",
			err:      errors.New("connection timeout"),
			expected: true,
		},
		{
			name: "postgres error is transient",
			err: &pgconn.PgError{
				Code: "08006", // connection_failure
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsTransientError(tt.err)
			if result != tt.expected {
				t.Errorf("IsTransientError(%v) = %v, expected %v", tt.err, result, tt.expected)
			}
		})
	}
}

// Concurrent cold Gets against an unreachable address (pgxpool connects lazily,
// so no live Postgres is needed). Pointer identity of returned clients can't be
// asserted: finding a cached-but-dead pool triggers legitimate Ping-evict-recreate.
// What the double-checked lock prevents is racing creators discarding pools
// without Close(), each leaking a background goroutine (see companion test).
func TestClientCache_Get_ConcurrentColdCache_NoLeakedPools(t *testing.T) {
	cache := NewClientCache()
	t.Cleanup(cache.Close)

	config := Config{Host: "127.0.0.1", Port: 1, Username: "u", Password: "p", Database: "postgres"}
	// Unique per run so this is a genuinely cold key - no other test's cached
	// entry can interfere with the goroutine-count measurement below.
	key := fmt.Sprintf("race-cluster-%d", time.Now().UnixNano())
	const goroutines = 50

	runtime.GC()
	before := runtime.NumGoroutine()

	errs := make([]error, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = cache.Get(context.Background(), key, config)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}

	if got := len(cache.clients); got != 1 {
		t.Fatalf("expected exactly one cached client for the key, got %d", got)
	}

	after := pollGoroutineCount(t, 3*time.Second, func(count int) bool {
		return count-before < goroutines/2
	})

	if leaked := after - before; leaked >= goroutines/2 {
		t.Fatalf("goroutine count grew by %d after %d concurrent cold Gets for one key; "+
			"that many surviving goroutines means pools are being created and discarded "+
			"without Close (double-checked locking regressed)", leaked, goroutines)
	}
}

// Companion to the test above: an un-Closed pgxpool leaks its background
// health-check goroutine indefinitely; Close() is what reclaims it.
func TestClientCache_Get_LeaksBackgroundGoroutineUntilClose(t *testing.T) {
	runtime.GC()
	before := runtime.NumGoroutine()

	const n = 10
	clients := make([]*Client, n)
	for i := range clients {
		c, err := NewClient(context.Background(), Config{Host: "127.0.0.1", Port: 1, Username: "u", Password: "p"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		clients[i] = c
	}

	afterCreate := pollGoroutineCount(t, 3*time.Second, func(count int) bool {
		return count-before >= n
	})
	if got := afterCreate - before; got < n {
		t.Fatalf("expected at least %d extra goroutines for %d un-Closed pools, got %d", n, n, got)
	}

	for _, c := range clients {
		c.Close()
	}

	afterClose := pollGoroutineCount(t, 3*time.Second, func(count int) bool {
		return count <= before
	})
	if afterClose > before {
		t.Fatalf("expected goroutine count to return to baseline %d after Close, got %d", before, afterClose)
	}
}

// pollGoroutineCount polls runtime.NumGoroutine() until cond holds or deadline
// elapses, returning the last observed count. Fixed sleeps flake on loaded CI
// runners — goroutines may need longer than the sleep to start or exit.
func pollGoroutineCount(t *testing.T, deadline time.Duration, cond func(count int) bool) int {
	t.Helper()
	deadlineAt := time.Now().Add(deadline)
	for {
		runtime.GC()
		count := runtime.NumGoroutine()
		if cond(count) || time.Now().After(deadlineAt) {
			return count
		}
		time.Sleep(10 * time.Millisecond)
	}
}
