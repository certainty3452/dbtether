package postgres

import (
	"errors"
	"testing"

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
