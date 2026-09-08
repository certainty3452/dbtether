package postgres

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
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

func TestIsLockNotAvailableError(t *testing.T) {
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
			name: "lock not available (55P03)",
			err: &pgconn.PgError{
				Code: "55P03",
			},
			expected: true,
		},
		{
			name: "wrapped lock not available error",
			err: fmt.Errorf("failed to transfer ownership of public.orders: %w", &pgconn.PgError{
				Code: "55P03",
			}),
			expected: true,
		},
		{
			name: "deadlock detected is not a lock timeout",
			err: &pgconn.PgError{
				Code: "40P01",
			},
			expected: false,
		},
		{
			name: "query canceled is not a lock timeout",
			err: &pgconn.PgError{
				Code: "57014",
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if result := isLockNotAvailableError(tt.err); result != tt.expected {
				t.Errorf("isLockNotAvailableError(%v) = %v, expected %v", tt.err, result, tt.expected)
			}
		})
	}
}

type fakeExecutor struct {
	executed []string
	errs     map[string]error
}

func (e *fakeExecutor) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	e.executed = append(e.executed, sql)
	return pgconn.CommandTag{}, e.errs[sql]
}

func TestTransferOwnership(t *testing.T) {
	const (
		alterOrders = `ALTER TABLE public.orders OWNER TO "app"`
		alterItems  = `ALTER TABLE public.items OWNER TO "app"`
		alterUsers  = `ALTER TABLE public.users OWNER TO "app"`
		alterAudit  = `ALTER TABLE public.audit OWNER TO "app"`
	)
	statements := []ownershipStatement{
		{object: "public.orders", statement: alterOrders},
		{object: "public.items", statement: alterItems},
	}
	fourStatements := []ownershipStatement{
		{object: "public.orders", statement: alterOrders},
		{object: "public.items", statement: alterItems},
		{object: "public.users", statement: alterUsers},
		{object: "public.audit", statement: alterAudit},
	}
	missing := &pgconn.PgError{Code: "42P01", Message: "relation does not exist"}
	denied := &pgconn.PgError{Code: "42501", Message: `must be able to SET ROLE "app"`}
	unsettable := &pgconn.PgError{Code: "42601", Message: "syntax error"}
	locked := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}

	tests := []struct {
		name         string
		statements   []ownershipStatement
		errs         map[string]error
		wantExecuted []string
		wantErrs     []error
		wantObjects  []string
	}{
		{
			name:       "no statements executes nothing",
			statements: nil,
		},
		{
			name:         "the lock timeout is set before the transfers",
			statements:   statements,
			wantExecuted: []string{transferLockTimeout, alterOrders, alterItems},
		},
		{
			name:         "a rejected lock timeout transfers nothing",
			statements:   statements,
			errs:         map[string]error{transferLockTimeout: unsettable},
			wantExecuted: []string{transferLockTimeout},
			wantErrs:     []error{unsettable},
		},
		{
			name:         "a failing statement does not stop the rest",
			statements:   statements,
			errs:         map[string]error{alterOrders: missing},
			wantExecuted: []string{transferLockTimeout, alterOrders, alterItems},
			wantErrs:     []error{missing},
			wantObjects:  []string{"public.orders"},
		},
		{
			name:         "every failure is reported",
			statements:   statements,
			errs:         map[string]error{alterOrders: missing, alterItems: denied},
			wantExecuted: []string{transferLockTimeout, alterOrders, alterItems},
			wantErrs:     []error{missing, denied},
			wantObjects:  []string{"public.orders", "public.items"},
		},
		{
			name:         "a lock timeout stops the transfer",
			statements:   fourStatements,
			errs:         map[string]error{alterItems: locked},
			wantExecuted: []string{transferLockTimeout, alterOrders, alterItems},
			wantErrs:     []error{locked},
			wantObjects:  []string{"public.items"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &fakeExecutor{errs: tt.errs}

			err := transferOwnership(context.Background(), exec, tt.statements)

			assertTransferError(t, err, tt.wantErrs, tt.wantObjects)
			if !slices.Equal(exec.executed, tt.wantExecuted) {
				t.Fatalf("executed %q, want %q", exec.executed, tt.wantExecuted)
			}
		})
	}
}

func TestTransferOwnershipBoundsTheMessage(t *testing.T) {
	const failures = 12
	statements := make([]ownershipStatement, 0, failures)
	errs := make(map[string]error, failures)
	var truncated error

	for i := range failures {
		object := fmt.Sprintf("public.t%02d", i)
		statement := fmt.Sprintf(`ALTER TABLE %s OWNER TO "app"`, object)
		statements = append(statements, ownershipStatement{object: object, statement: statement})
		truncated = &pgconn.PgError{Code: "42501", Message: "permission denied for " + object}
		errs[statement] = truncated
	}

	err := transferOwnership(context.Background(), &fakeExecutor{errs: errs}, statements)
	if err == nil {
		t.Fatal("expected an error naming the failed objects, got nil")
	}

	message := err.Error()
	if strings.Contains(message, "\n") {
		t.Errorf("message spans lines, got %q", message)
	}
	if !strings.HasSuffix(message, "; and 2 more") {
		t.Errorf("message %q does not end with the truncation count", message)
	}
	if got := strings.Count(message, "failed to transfer ownership of "); got != maxReportedTransferFailures {
		t.Errorf("message lists %d failures, want %d: %q", got, maxReportedTransferFailures, message)
	}
	for _, object := range []string{"public.t10", "public.t11"} {
		if strings.Contains(message, object) {
			t.Errorf("message %q names %s, which should have been truncated", message, object)
		}
	}
	if !errors.Is(err, truncated) {
		t.Errorf("errors.Is does not find the truncated failure in %v", err)
	}
}

func assertTransferError(t *testing.T, err error, want []error, objects []string) {
	t.Helper()
	if len(want) == 0 {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected an error naming %q, got nil", objects)
	}
	for _, target := range want {
		if !errors.Is(err, target) {
			t.Fatalf("expected error wrapping %v, got %v", target, err)
		}
	}
	for _, object := range objects {
		if !strings.Contains(err.Error(), object) {
			t.Fatalf("expected error naming %q, got %v", object, err)
		}
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
