package postgres

import (
	"context"
	"testing"
)

func TestMockClient_EnsureDatabaseWithOwner(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name               string
		setupDatabases     map[string]bool
		setupOwners        map[string]string
		dbName             string
		ownerNS            string
		ownerName          string
		forceAdopt         bool
		expectedTracked    bool
		expectError        bool
		expectedOwnerAfter string
	}{
		{
			name:               "create new database",
			setupDatabases:     map[string]bool{},
			setupOwners:        map[string]string{},
			dbName:             "newdb",
			ownerNS:            "team-a",
			ownerName:          "mydb",
			expectedTracked:    true,
			expectError:        false,
			expectedOwnerAfter: "team-a/mydb",
		},
		{
			name:               "claim existing database without owner",
			setupDatabases:     map[string]bool{"legacydb": true},
			setupOwners:        map[string]string{},
			dbName:             "legacydb",
			ownerNS:            "team-a",
			ownerName:          "imported",
			expectedTracked:    true,
			expectError:        false,
			expectedOwnerAfter: "team-a/imported",
		},
		{
			name:               "existing database already owned by same CRD",
			setupDatabases:     map[string]bool{"mydb": true},
			setupOwners:        map[string]string{"mydb": "team-a/mydb"},
			dbName:             "mydb",
			ownerNS:            "team-a",
			ownerName:          "mydb",
			expectedTracked:    true,
			expectError:        false,
			expectedOwnerAfter: "team-a/mydb",
		},
		{
			name:               "existing database owned by different CRD - error",
			setupDatabases:     map[string]bool{"shareddb": true},
			setupOwners:        map[string]string{"shareddb": "team-a/original"},
			dbName:             "shareddb",
			ownerNS:            "team-b",
			ownerName:          "duplicate",
			expectedTracked:    false,
			expectError:        true,
			expectedOwnerAfter: "team-a/original", // unchanged
		},
		{
			name:               "force adopt from different owner",
			setupDatabases:     map[string]bool{"shareddb": true},
			setupOwners:        map[string]string{"shareddb": "team-a/original"},
			dbName:             "shareddb",
			ownerNS:            "team-b",
			ownerName:          "takeover",
			forceAdopt:         true,
			expectedTracked:    true,
			expectError:        false,
			expectedOwnerAfter: "team-b/takeover",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := NewMockClient()
			mock.databases = tt.setupDatabases
			mock.dbOwners = tt.setupOwners

			tracked, err := mock.EnsureDatabaseWithOwner(ctx, tt.dbName, tt.ownerNS, tt.ownerName, tt.forceAdopt)

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}

			if tracked != tt.expectedTracked {
				t.Errorf("ownershipTracked: expected %v, got %v", tt.expectedTracked, tracked)
			}

			// Check final owner state
			actualOwner := mock.dbOwners[tt.dbName]
			if actualOwner != tt.expectedOwnerAfter {
				t.Errorf("owner: expected %q, got %q", tt.expectedOwnerAfter, actualOwner)
			}
		})
	}
}

func TestMockClient_GetDatabaseOwner(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		dbOwners     map[string]string
		dbName       string
		expectedNS   string
		expectedName string
	}{
		{
			name:         "database with owner",
			dbOwners:     map[string]string{"mydb": "production/orders"},
			dbName:       "mydb",
			expectedNS:   "production",
			expectedName: "orders",
		},
		{
			name:         "database without owner",
			dbOwners:     map[string]string{},
			dbName:       "legacydb",
			expectedNS:   "",
			expectedName: "",
		},
		{
			name:         "malformed owner (no slash)",
			dbOwners:     map[string]string{"baddb": "nonamespace"},
			dbName:       "baddb",
			expectedNS:   "",
			expectedName: "nonamespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := NewMockClient()
			mock.dbOwners = tt.dbOwners

			ns, name, err := mock.GetDatabaseOwner(ctx, tt.dbName)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			if ns != tt.expectedNS {
				t.Errorf("namespace: expected %q, got %q", tt.expectedNS, ns)
			}
			if name != tt.expectedName {
				t.Errorf("name: expected %q, got %q", tt.expectedName, name)
			}
		})
	}
}

func TestMockClient_ClearDatabaseOwner(t *testing.T) {
	ctx := context.Background()
	mock := NewMockClient()
	mock.dbOwners = map[string]string{"mydb": "team-a/resource"}

	if err := mock.ClearDatabaseOwner(ctx, "mydb"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	// Owner should be cleared
	if owner := mock.dbOwners["mydb"]; owner != "" {
		t.Errorf("owner should be empty after clear, got %q", owner)
	}
}

func TestMockClient_ShouldFail(t *testing.T) {
	ctx := context.Background()
	mock := NewMockClient()
	mock.ShouldFail = true
	mock.FailError = &testError{msg: "simulated failure"}

	tracked, err := mock.EnsureDatabaseWithOwner(ctx, "testdb", "ns", "name", false)
	if err == nil {
		t.Error("expected error when ShouldFail is true")
	}
	if tracked {
		t.Error("tracked should be false on error")
	}
}

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

func TestMockClient_ReassignOwnership(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name        string
		shouldFail  bool
		expectError bool
	}{
		{
			name:        "successful reassign",
			shouldFail:  false,
			expectError: false,
		},
		{
			name:        "failed reassign",
			shouldFail:  true,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := NewMockClient()
			if tt.shouldFail {
				mock.ShouldFail = true
				mock.FailError = &testError{msg: "simulated failure"}
			}

			err := mock.ReassignOwnership(ctx, "testuser", "testdb")

			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestMockClient_ApplyPrivileges_Owner(t *testing.T) {
	ctx := context.Background()
	mock := NewMockClient()

	// Test that owner privilege is accepted
	err := mock.ApplyPrivileges(ctx, "testuser", "testdb", "owner", nil)
	if err != nil {
		t.Errorf("unexpected error applying owner privileges: %v", err)
	}
}

func TestMockClient_SyncDatabaseAccess(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name             string
		initialAccess    map[string]bool
		allowedDatabases []string
		expectedAccess   map[string]bool
	}{
		{
			name:             "grant access to new databases",
			initialAccess:    map[string]bool{},
			allowedDatabases: []string{"db1", "db2"},
			expectedAccess:   map[string]bool{"db1": true, "db2": true},
		},
		{
			name:             "revoke access from removed databases",
			initialAccess:    map[string]bool{"db1": true, "db2": true, "db3": true},
			allowedDatabases: []string{"db1"},
			expectedAccess:   map[string]bool{"db1": true},
		},
		{
			name:             "mixed grant and revoke",
			initialAccess:    map[string]bool{"db1": true, "db2": true},
			allowedDatabases: []string{"db2", "db3"},
			expectedAccess:   map[string]bool{"db2": true, "db3": true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mock := NewMockClient()
			mock.userAccess["testuser"] = tt.initialAccess

			err := mock.SyncDatabaseAccess(ctx, "testuser", tt.allowedDatabases)
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}

			// Check final access state
			for db, expected := range tt.expectedAccess {
				if mock.userAccess["testuser"][db] != expected {
					t.Errorf("database %s: expected access=%v, got %v", db, expected, mock.userAccess["testuser"][db])
				}
			}

			// Check that removed databases are not accessible
			for db := range tt.initialAccess {
				if !tt.expectedAccess[db] && mock.userAccess["testuser"][db] {
					t.Errorf("database %s should have been revoked but still has access", db)
				}
			}
		})
	}
}
