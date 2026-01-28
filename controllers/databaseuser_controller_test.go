package controllers

import (
	"context"
	"errors"
	"testing"
	"time"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testUserName   = "my-user"
	testClusterRef = "my-cluster"
)

func TestDatabaseUserReconciler_GetUsername(t *testing.T) {
	r := &DatabaseUserReconciler{}

	tests := []struct {
		name         string
		specUsername string
		metaName     string
		want         string
	}{
		{"uses spec.username when set", "custom_user", testUserName, "custom_user"},
		{"falls back to metadata.name with dash conversion", "", testUserName, "my_user"},
		{"prefers spec.username", "explicit", "fallback", "explicit"},
		{"converts multiple dashes", "", "my-app-user", "my_app_user"},
		{"no conversion needed", "", "myuser", "myuser"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name: tt.metaName,
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Username: tt.specUsername,
				},
			}
			got := r.getUsername(user)
			if got != tt.want {
				t.Errorf("getUsername() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_Privileges(t *testing.T) {
	tests := []struct {
		name       string
		privileges string
		valid      bool
	}{
		{"readonly", "readonly", true},
		{"readwrite", "readwrite", true},
		{"admin", "admin", true},
		{"empty", "", false},
		{"invalid", "superuser", false},
	}

	validPrivileges := map[string]bool{
		"readonly":  true,
		"readwrite": true,
		"admin":     true,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validPrivileges[tt.privileges]
			if got != tt.valid {
				t.Errorf("privileges %q valid = %v, want %v", tt.privileges, got, tt.valid)
			}
		})
	}
}

func TestDatabaseUserReconciler_PasswordLength(t *testing.T) {
	tests := []struct {
		name       string
		specLength int
		wantLength int
	}{
		{"default when 0", 0, 16},
		{"custom 32", 32, 32},
		{"minimum 12", 12, 12},
		{"maximum 64", 64, 64},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Password: databasesv1alpha1.PasswordConfig{
						Length: tt.specLength,
					},
				},
			}

			length := user.Spec.Password.Length
			if length == 0 {
				length = 16
			}

			if length != tt.wantLength {
				t.Errorf("password length = %v, want %v", length, tt.wantLength)
			}
		})
	}
}

func TestDatabaseUserReconciler_GetSecretName(t *testing.T) {
	r := &DatabaseUserReconciler{}

	tests := []struct {
		name string
		user *databasesv1alpha1.DatabaseUser
		want string
	}{
		{
			name: "default when secret is nil",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "my-user"},
				Spec:       databasesv1alpha1.DatabaseUserSpec{},
			},
			want: "my-user-credentials",
		},
		{
			name: "default when secret.name is empty",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "my-user"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{},
				},
			},
			want: "my-user-credentials",
		},
		{
			name: "custom name",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "my-user"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Name: "custom-secret"},
				},
			},
			want: "custom-secret",
		},
		{
			name: "custom name with template",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "my-user"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{
						Name:     "db-creds",
						Template: "DATABASE",
					},
				},
			},
			want: "db-creds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.getSecretName(tt.user)
			if got != tt.want {
				t.Errorf("getSecretName() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_GetSecretKeys(t *testing.T) {
	r := &DatabaseUserReconciler{}

	tests := []struct {
		name     string
		user     *databasesv1alpha1.DatabaseUser
		wantHost string
		wantPort string
		wantDB   string
		wantUser string
		wantPwd  string
	}{
		{
			name: "default raw template (nil secret)",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			wantHost: "host", wantPort: "port", wantDB: "database",
			wantUser: "user", wantPwd: "password",
		},
		{
			name: "explicit raw template",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "raw"},
				},
			},
			wantHost: "host", wantPort: "port", wantDB: "database",
			wantUser: "user", wantPwd: "password",
		},
		{
			name: "empty template defaults to raw",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: ""},
				},
			},
			wantHost: "host", wantPort: "port", wantDB: "database",
			wantUser: "user", wantPwd: "password",
		},
		{
			name: "DB template",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "DB"},
				},
			},
			wantHost: "DB_HOST", wantPort: "DB_PORT", wantDB: "DB_NAME",
			wantUser: "DB_USER", wantPwd: "DB_PASSWORD",
		},
		{
			name: "DATABASE template",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "DATABASE"},
				},
			},
			wantHost: "DATABASE_HOST", wantPort: "DATABASE_PORT", wantDB: "DATABASE_NAME",
			wantUser: "DATABASE_USER", wantPwd: "DATABASE_PASSWORD",
		},
		{
			name: "POSTGRES template",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "POSTGRES"},
				},
			},
			wantHost: "POSTGRES_HOST", wantPort: "POSTGRES_PORT", wantDB: "POSTGRES_DATABASE",
			wantUser: "POSTGRES_USER", wantPwd: "POSTGRES_PASSWORD",
		},
		{
			name: "custom template with all keys",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "custom",
						Keys: &databasesv1alpha1.SecretKeys{
							Host: "PGHOST", Port: "PGPORT", Database: "PGDATABASE",
							User: "PGUSER", Password: "PGPASSWORD",
						},
					},
				},
			},
			wantHost: "PGHOST", wantPort: "PGPORT", wantDB: "PGDATABASE",
			wantUser: "PGUSER", wantPwd: "PGPASSWORD",
		},
		{
			name: "custom template with partial keys",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "custom",
						Keys:     &databasesv1alpha1.SecretKeys{Password: "SECRET_PWD"},
					},
				},
			},
			wantHost: "host", wantPort: "port", wantDB: "database",
			wantUser: "user", wantPwd: "SECRET_PWD",
		},
		{
			name: "custom template with nil keys uses defaults",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "custom"},
				},
			},
			wantHost: "host", wantPort: "port", wantDB: "database",
			wantUser: "user", wantPwd: "password",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHost, gotPort, gotDB, gotUser, gotPwd := r.getSecretKeys(tt.user)
			if gotHost != tt.wantHost {
				t.Errorf("host = %v, want %v", gotHost, tt.wantHost)
			}
			if gotPort != tt.wantPort {
				t.Errorf("port = %v, want %v", gotPort, tt.wantPort)
			}
			if gotDB != tt.wantDB {
				t.Errorf("database = %v, want %v", gotDB, tt.wantDB)
			}
			if gotUser != tt.wantUser {
				t.Errorf("user = %v, want %v", gotUser, tt.wantUser)
			}
			if gotPwd != tt.wantPwd {
				t.Errorf("password = %v, want %v", gotPwd, tt.wantPwd)
			}
		})
	}
}

func TestDatabaseUserReconciler_ShouldIncludeDatabasesList(t *testing.T) {
	r := &DatabaseUserReconciler{}

	tests := []struct {
		name     string
		user     *databasesv1alpha1.DatabaseUser
		dbCount  int
		expected bool
	}{
		{
			name: "raw template with multiple databases",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			dbCount:  3,
			expected: true,
		},
		{
			name: "raw template with single database",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			dbCount:  1,
			expected: false,
		},
		{
			name: "POSTGRES template with multiple databases",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "POSTGRES"},
				},
			},
			dbCount:  3,
			expected: false,
		},
		{
			name: "perDatabase mode",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					SecretGeneration: "perDatabase",
				},
			},
			dbCount:  3,
			expected: false,
		},
		{
			name: "explicit raw template with multiple databases",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "raw"},
				},
			},
			dbCount:  2,
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.shouldIncludeDatabasesList(tt.user, tt.dbCount)
			if got != tt.expected {
				t.Errorf("shouldIncludeDatabasesList() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestDatabaseUserReconciler_ValidateSpec(t *testing.T) {
	r := &DatabaseUserReconciler{}

	tests := []struct {
		name    string
		user    *databasesv1alpha1.DatabaseUser
		wantErr bool
	}{
		{
			name: "valid single database",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "my-db"},
				},
			},
			wantErr: false,
		},
		{
			name: "valid multiple databases",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: "db1"},
						{Name: "db2"},
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid - both database and databases",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "my-db"},
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: "db1"},
					},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid - neither database nor databases",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := r.validateSpec(tt.user)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateSpec() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDatabaseUserSpec_GetDatabases(t *testing.T) {
	tests := []struct {
		name      string
		spec      databasesv1alpha1.DatabaseUserSpec
		wantCount int
		wantFirst string
	}{
		{
			name: "single database returns list of one",
			spec: databasesv1alpha1.DatabaseUserSpec{
				Database: &databasesv1alpha1.DatabaseAccess{Name: "my-db"},
			},
			wantCount: 1,
			wantFirst: "my-db",
		},
		{
			name: "multiple databases returns full list",
			spec: databasesv1alpha1.DatabaseUserSpec{
				Databases: []databasesv1alpha1.DatabaseAccess{
					{Name: "db1"},
					{Name: "db2"},
					{Name: "db3"},
				},
			},
			wantCount: 3,
			wantFirst: "db1",
		},
		{
			name:      "empty spec returns nil",
			spec:      databasesv1alpha1.DatabaseUserSpec{},
			wantCount: 0,
			wantFirst: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.spec.GetDatabases()
			if len(got) != tt.wantCount {
				t.Errorf("GetDatabases() count = %v, want %v", len(got), tt.wantCount)
			}
			if tt.wantCount > 0 && got[0].Name != tt.wantFirst {
				t.Errorf("GetDatabases()[0].Name = %v, want %v", got[0].Name, tt.wantFirst)
			}
		})
	}
}

func TestDatabaseUserSpec_HasDatabases(t *testing.T) {
	tests := []struct {
		name string
		spec databasesv1alpha1.DatabaseUserSpec
		want bool
	}{
		{
			name: "has single database",
			spec: databasesv1alpha1.DatabaseUserSpec{
				Database: &databasesv1alpha1.DatabaseAccess{Name: "my-db"},
			},
			want: true,
		},
		{
			name: "has multiple databases",
			spec: databasesv1alpha1.DatabaseUserSpec{
				Databases: []databasesv1alpha1.DatabaseAccess{{Name: "db1"}},
			},
			want: true,
		},
		{
			name: "has neither",
			spec: databasesv1alpha1.DatabaseUserSpec{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.spec.HasDatabases()
			if got != tt.want {
				t.Errorf("HasDatabases() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_GetSecretNameForDatabase(t *testing.T) {
	r := &DatabaseUserReconciler{}

	tests := []struct {
		userName string
		dbName   string
		want     string
	}{
		{"my-user", "my-db", "my-user-my-db-credentials"},
		{"api", "orders", "api-orders-credentials"},
	}

	for _, tt := range tests {
		t.Run(tt.userName+"-"+tt.dbName, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: tt.userName},
			}
			got := r.getSecretNameForDatabase(user, tt.dbName)
			if got != tt.want {
				t.Errorf("getSecretNameForDatabase() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_IsSecretOwnedByUser(t *testing.T) {
	r := &DatabaseUserReconciler{}

	userUID := types.UID("test-uid-123")
	otherUID := types.UID("other-uid-456")

	tests := []struct {
		name   string
		secret *corev1.Secret
		user   *databasesv1alpha1.DatabaseUser
		want   bool
	}{
		{
			name: "secret owned by this user",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "DatabaseUser", Name: "my-user", UID: userUID},
					},
				},
			},
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "my-user", UID: userUID},
			},
			want: true,
		},
		{
			name: "secret owned by different user",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "DatabaseUser", Name: "other-user", UID: otherUID},
					},
				},
			},
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "my-user", UID: userUID},
			},
			want: false,
		},
		{
			name: "secret with no owner",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{},
			},
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "my-user", UID: userUID},
			},
			want: false,
		},
		{
			name: "secret owned by different kind",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "Database", Name: "my-user", UID: userUID},
					},
				},
			},
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "my-user", UID: userUID},
			},
			want: false,
		},
		{
			name: "secret with same name but different UID",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "DatabaseUser", Name: "my-user", UID: otherUID},
					},
				},
			},
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "my-user", UID: userUID},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.isSecretOwnedByUser(tt.secret, tt.user)
			if got != tt.want {
				t.Errorf("isSecretOwnedByUser() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_GetOnConflictPolicy(t *testing.T) {
	r := &DatabaseUserReconciler{}

	tests := []struct {
		name string
		user *databasesv1alpha1.DatabaseUser
		want string
	}{
		{
			name: "default when secret is nil",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			want: "Fail",
		},
		{
			name: "default when onConflict is empty",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{},
				},
			},
			want: "Fail",
		},
		{
			name: "explicit Fail",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{OnConflict: "Fail"},
				},
			},
			want: "Fail",
		},
		{
			name: "Adopt policy",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{OnConflict: "Adopt"},
				},
			},
			want: "Adopt",
		},
		{
			name: "Merge policy",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{OnConflict: "Merge"},
				},
			},
			want: "Merge",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.getOnConflictPolicy(tt.user)
			if got != tt.want {
				t.Errorf("getOnConflictPolicy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_ConnectionLimit(t *testing.T) {
	tests := []struct {
		name            string
		connectionLimit int
		shouldApply     bool
	}{
		{"unlimited (default 0)", 0, false},
		{"unlimited (-1)", -1, true},
		{"limited to 10", 10, true},
		{"limited to 1", 1, true},
		{"limited to 100", 100, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					ConnectionLimit: tt.connectionLimit,
				},
			}

			shouldApply := user.Spec.ConnectionLimit != 0
			if shouldApply != tt.shouldApply {
				t.Errorf("shouldApply connection limit = %v, want %v", shouldApply, tt.shouldApply)
			}
		})
	}
}

func TestDatabaseUserReconciler_SecretRegeneration(t *testing.T) {
	tests := []struct {
		name         string
		currentPhase string
		expectRegen  bool
	}{
		{"not regeneration when Pending", "Pending", false},
		{"not regeneration when Creating", "Creating", false},
		{"regeneration when Ready", "Ready", true},
		{"not regeneration when Failed", "Failed", false},
		{"not regeneration when empty", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{
					Phase: tt.currentPhase,
				},
			}

			regenerated := user.Status.Phase == "Ready"
			if regenerated != tt.expectRegen {
				t.Errorf("regenerated = %v, want %v", regenerated, tt.expectRegen)
			}
		})
	}
}

func TestDatabaseUserReconciler_ShouldReconcileWhenSecretMissing(t *testing.T) {
	tests := []struct {
		name                string
		phase               string
		observedGeneration  int64
		generation          int64
		secretExists        bool
		shouldSkipReconcile bool
	}{
		{
			name:                "skip when Ready, generation matches, secret exists",
			phase:               "Ready",
			observedGeneration:  1,
			generation:          1,
			secretExists:        true,
			shouldSkipReconcile: true,
		},
		{
			name:                "reconcile when Ready, generation matches, secret MISSING",
			phase:               "Ready",
			observedGeneration:  1,
			generation:          1,
			secretExists:        false,
			shouldSkipReconcile: false,
		},
		{
			name:                "reconcile when generation changed",
			phase:               "Ready",
			observedGeneration:  1,
			generation:          2,
			secretExists:        true,
			shouldSkipReconcile: false,
		},
		{
			name:                "reconcile when not Ready",
			phase:               "Pending",
			observedGeneration:  1,
			generation:          1,
			secretExists:        true,
			shouldSkipReconcile: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "test-user",
					Generation: tt.generation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					Phase:              tt.phase,
					ObservedGeneration: tt.observedGeneration,
				},
			}

			// Simulate the early exit logic
			shouldSkip := false
			if user.Status.Phase == "Ready" && user.Status.ObservedGeneration == user.Generation {
				if tt.secretExists {
					shouldSkip = true
				}
			}

			if shouldSkip != tt.shouldSkipReconcile {
				t.Errorf("shouldSkipReconcile = %v, want %v", shouldSkip, tt.shouldSkipReconcile)
			}
		})
	}
}

func TestDatabaseUserReconciler_ShouldRotatePassword(t *testing.T) {
	r := &DatabaseUserReconciler{}

	now := metav1.Now()
	thirtyDaysAgo := metav1.NewTime(time.Now().Add(-31 * 24 * time.Hour))
	tenDaysAgo := metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))

	tests := []struct {
		name         string
		rotation     *databasesv1alpha1.RotationConfig
		updatedAt    *metav1.Time
		shouldRotate bool
	}{
		{
			name:         "no rotation config",
			rotation:     nil,
			updatedAt:    &thirtyDaysAgo,
			shouldRotate: false,
		},
		{
			name:         "rotation days is 0",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 0},
			updatedAt:    &thirtyDaysAgo,
			shouldRotate: false,
		},
		{
			name:         "no passwordUpdatedAt",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:    nil,
			shouldRotate: false,
		},
		{
			name:         "password expired (31 days old, 30 day rotation)",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:    &thirtyDaysAgo,
			shouldRotate: true,
		},
		{
			name:         "password not expired (10 days old, 30 day rotation)",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:    &tenDaysAgo,
			shouldRotate: false,
		},
		{
			name:         "password just created",
			rotation:     &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:    &now,
			shouldRotate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: tt.rotation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.updatedAt,
				},
			}

			got := r.shouldRotatePassword(user)
			if got != tt.shouldRotate {
				t.Errorf("shouldRotatePassword() = %v, want %v", got, tt.shouldRotate)
			}
		})
	}
}

func TestDatabaseUserReconciler_CalculateRequeueAfter(t *testing.T) {
	r := &DatabaseUserReconciler{}

	now := metav1.Now()
	tenDaysAgo := metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))
	thirtyOneDaysAgo := metav1.NewTime(time.Now().Add(-31 * 24 * time.Hour))

	tests := []struct {
		name      string
		rotation  *databasesv1alpha1.RotationConfig
		updatedAt *metav1.Time
		expectGT  time.Duration
		expectLT  time.Duration
	}{
		{
			name:      "no rotation config",
			rotation:  nil,
			updatedAt: &now,
			expectGT:  -1,
			expectLT:  1,
		},
		{
			name:      "rotation days is 0",
			rotation:  &databasesv1alpha1.RotationConfig{Days: 0},
			updatedAt: &now,
			expectGT:  -1,
			expectLT:  1,
		},
		{
			name:      "no passwordUpdatedAt",
			rotation:  &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt: nil,
			expectGT:  -1,
			expectLT:  1,
		},
		{
			name:      "password 10 days old, 30 day rotation -> ~20 days requeue",
			rotation:  &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt: &tenDaysAgo,
			expectGT:  19 * 24 * time.Hour,
			expectLT:  21 * 24 * time.Hour,
		},
		{
			name:      "password expired -> 1 minute requeue",
			rotation:  &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt: &thirtyOneDaysAgo,
			expectGT:  30 * time.Second,
			expectLT:  2 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: tt.rotation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.updatedAt,
				},
			}

			got := r.calculateRequeueAfter(user)
			if got <= tt.expectGT || got >= tt.expectLT {
				t.Errorf("calculateRequeueAfter() = %v, expected between %v and %v", got, tt.expectGT, tt.expectLT)
			}
		})
	}
}

func TestDatabaseUserReconciler_CheckAndTriggerRotation(t *testing.T) {
	thirtyOneDaysAgo := metav1.NewTime(time.Now().Add(-31 * 24 * time.Hour))
	tenDaysAgo := metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))

	tests := []struct {
		name          string
		rotation      *databasesv1alpha1.RotationConfig
		updatedAt     *metav1.Time
		expectTrigger bool
	}{
		{
			name:          "no rotation config - no trigger",
			rotation:      nil,
			updatedAt:     &thirtyOneDaysAgo,
			expectTrigger: false,
		},
		{
			name:          "rotation enabled, password expired - trigger",
			rotation:      &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:     &thirtyOneDaysAgo,
			expectTrigger: true,
		},
		{
			name:          "rotation enabled, password not expired - no trigger",
			rotation:      &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:     &tenDaysAgo,
			expectTrigger: false,
		},
		{
			name:          "rotation enabled, no passwordUpdatedAt - no trigger",
			rotation:      &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:     nil,
			expectTrigger: false,
		},
	}

	r := &DatabaseUserReconciler{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: tt.rotation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.updatedAt,
				},
			}

			// Test shouldRotatePassword which is called by checkAndTriggerRotation
			shouldRotate := r.shouldRotatePassword(user)
			if shouldRotate != tt.expectTrigger {
				t.Errorf("shouldRotatePassword() = %v, want %v", shouldRotate, tt.expectTrigger)
			}
		})
	}
}

func TestDatabaseUserReconciler_RequeueAfterReturned(t *testing.T) {
	tenDaysAgo := metav1.NewTime(time.Now().Add(-10 * 24 * time.Hour))

	tests := []struct {
		name           string
		rotation       *databasesv1alpha1.RotationConfig
		updatedAt      *metav1.Time
		expectRequeue  bool
		minRequeueTime time.Duration
		maxRequeueTime time.Duration
	}{
		{
			name:          "no rotation - no requeue",
			rotation:      nil,
			updatedAt:     &tenDaysAgo,
			expectRequeue: false,
		},
		{
			name:           "rotation enabled, 10 days old - requeue in ~20 days",
			rotation:       &databasesv1alpha1.RotationConfig{Days: 30},
			updatedAt:      &tenDaysAgo,
			expectRequeue:  true,
			minRequeueTime: 19 * 24 * time.Hour,
			maxRequeueTime: 21 * 24 * time.Hour,
		},
	}

	r := &DatabaseUserReconciler{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: tt.rotation,
				},
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.updatedAt,
				},
			}

			requeue := r.calculateRequeueAfter(user)

			if tt.expectRequeue {
				if requeue <= 0 {
					t.Errorf("expected positive requeue duration, got %v", requeue)
				}
				if requeue < tt.minRequeueTime || requeue > tt.maxRequeueTime {
					t.Errorf("requeue = %v, expected between %v and %v", requeue, tt.minRequeueTime, tt.maxRequeueTime)
				}
			} else if requeue > 0 {
				t.Errorf("expected no requeue (0), got %v", requeue)
			}
		})
	}
}

func TestDatabaseUserReconciler_PasswordUpdatedAtOnReady(t *testing.T) {
	tests := []struct {
		name             string
		phase            string
		passwordUpdated  bool
		existingPwdTime  *metav1.Time
		expectPwdTimeSet bool
	}{
		{
			name:             "first Ready with passwordUpdated=true sets timestamp",
			phase:            "Ready",
			passwordUpdated:  true,
			existingPwdTime:  nil,
			expectPwdTimeSet: true,
		},
		{
			name:             "first Ready with passwordUpdated=false still sets timestamp",
			phase:            "Ready",
			passwordUpdated:  false,
			existingPwdTime:  nil,
			expectPwdTimeSet: true,
		},
		{
			name:             "subsequent Ready with passwordUpdated=false keeps existing timestamp",
			phase:            "Ready",
			passwordUpdated:  false,
			existingPwdTime:  &metav1.Time{Time: time.Now().Add(-24 * time.Hour)},
			expectPwdTimeSet: true, // keeps existing
		},
		{
			name:             "subsequent Ready with passwordUpdated=true updates timestamp",
			phase:            "Ready",
			passwordUpdated:  true,
			existingPwdTime:  &metav1.Time{Time: time.Now().Add(-24 * time.Hour)},
			expectPwdTimeSet: true,
		},
		{
			name:             "Failed phase does not set timestamp",
			phase:            "Failed",
			passwordUpdated:  false,
			existingPwdTime:  nil,
			expectPwdTimeSet: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{
					PasswordUpdatedAt: tt.existingPwdTime,
				},
			}

			// Simulate setStatus logic
			if tt.passwordUpdated || (user.Status.PasswordUpdatedAt == nil && tt.phase == "Ready") {
				now := metav1.Now()
				user.Status.PasswordUpdatedAt = &now
			}

			if tt.expectPwdTimeSet && user.Status.PasswordUpdatedAt == nil {
				t.Error("expected PasswordUpdatedAt to be set, but it was nil")
			}
			if !tt.expectPwdTimeSet && user.Status.PasswordUpdatedAt != nil {
				t.Error("expected PasswordUpdatedAt to be nil, but it was set")
			}
		})
	}
}

func TestDatabaseUserReconciler_DeletionPolicy(t *testing.T) {
	tests := []struct {
		name           string
		deletionPolicy string
		expectDrop     bool
	}{
		{"Delete policy drops user", "Delete", true},
		{"Retain policy keeps user", "Retain", false},
		{"Empty policy defaults to Delete", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					DeletionPolicy: tt.deletionPolicy,
				},
			}

			shouldDrop := user.Spec.DeletionPolicy != "Retain"
			if shouldDrop != tt.expectDrop {
				t.Errorf("shouldDrop = %v, want %v", shouldDrop, tt.expectDrop)
			}
		})
	}
}

func TestDatabaseUserReconciler_GetClusterFromStatus(t *testing.T) {
	tests := []struct {
		name              string
		statusClusterName string
		statusDBName      string
		expectCluster     string
		expectDB          string
	}{
		{
			name:              "uses status when populated",
			statusClusterName: testClusterRef,
			statusDBName:      "my_database",
			expectCluster:     testClusterRef,
			expectDB:          "my_database",
		},
		{
			name:              "returns empty when status not populated",
			statusClusterName: "",
			statusDBName:      "",
			expectCluster:     "",
			expectDB:          "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{
					ClusterName: tt.statusClusterName,
					Databases: []databasesv1alpha1.DatabaseAccessStatus{
						{DatabaseName: tt.statusDBName},
					},
				},
			}

			// Simulate getClusterAndDatabasesForDeletion logic (status check only)
			clusterName := ""
			var databaseNames []string
			if user.Status.ClusterName != "" {
				clusterName = user.Status.ClusterName
				for _, db := range user.Status.Databases {
					databaseNames = append(databaseNames, db.DatabaseName)
				}
			}

			if clusterName != tt.expectCluster {
				t.Errorf("clusterName = %v, want %v", clusterName, tt.expectCluster)
			}
			if len(databaseNames) > 0 && databaseNames[0] != tt.expectDB {
				t.Errorf("databaseName = %v, want %v", databaseNames[0], tt.expectDB)
			}
		})
	}
}

func TestDatabaseUserReconciler_GetDatabaseNameFromSpec(t *testing.T) {
	r := &DatabaseUserReconciler{}

	tests := []struct {
		name       string
		specDBName string
		metaName   string
		want       string
	}{
		{"uses spec.databaseName when set", "custom_db", "my-db", "custom_db"},
		{"falls back to metadata.name with dash conversion", "", "my-db", "my_db"},
		{"converts multiple dashes", "", "my-app-db", "my_app_db"},
		{"no conversion needed", "", "mydb", "mydb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := &databasesv1alpha1.Database{
				ObjectMeta: metav1.ObjectMeta{
					Name: tt.metaName,
				},
				Spec: databasesv1alpha1.DatabaseSpec{
					DatabaseName: tt.specDBName,
				},
			}
			got := r.getDatabaseNameFromSpec(db)
			if got != tt.want {
				t.Errorf("getDatabaseNameFromSpec() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDatabaseUserReconciler_PendingTimeout(t *testing.T) {
	now := metav1.Now()
	fiveMinutesAgo := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	elevenMinutesAgo := metav1.NewTime(time.Now().Add(-11 * time.Minute))

	tests := []struct {
		name          string
		phase         string
		pendingSince  *metav1.Time
		expectPhase   string
		expectTimeout bool
	}{
		{"first Pending - sets pendingSince", "Pending", nil, "Pending", false},
		{"Pending for 5 minutes - stays Pending", "Pending", &fiveMinutesAgo, "Pending", false},
		{"Pending for 11 minutes - Failed", "Pending", &elevenMinutesAgo, "Failed", true},
		{"Ready phase - clears pendingSince", "Ready", &fiveMinutesAgo, "Ready", false},
		{"Failed phase - clears pendingSince", "Failed", &fiveMinutesAgo, "Failed", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{PendingSince: tt.pendingSince},
			}

			phase := simulatePendingTimeout(tt.phase, user.Status.PendingSince, &now)

			if phase != tt.expectPhase {
				t.Errorf("phase = %v, want %v", phase, tt.expectPhase)
			}
		})
	}
}

func simulatePendingTimeout(phase string, pendingSince, now *metav1.Time) string {
	if phase == "Pending" && pendingSince != nil && now.Sub(pendingSince.Time) > PendingTimeout {
		return "Failed"
	}
	return phase
}

func TestDatabaseUserReconciler_StatusUpdate(t *testing.T) {
	tests := []struct {
		name         string
		update       statusUpdate
		expectClear  bool
		expectValues bool
	}{
		{
			name: "status update fields are applied",
			update: statusUpdate{
				ClusterName: testClusterRef,
				Username:    "my_user",
				Databases: []databasesv1alpha1.DatabaseAccessStatus{
					{DatabaseName: "my_database", Phase: "Ready"},
				},
			},
			expectClear:  false,
			expectValues: true,
		},
		{
			name: "empty fields don't overwrite",
			update: statusUpdate{
				ClusterName: "",
				Username:    "",
				Databases:   nil,
			},
			expectClear:  false,
			expectValues: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				Status: databasesv1alpha1.DatabaseUserStatus{},
			}

			// Simulate applyStatusFields logic
			if tt.update.ClusterName != "" {
				user.Status.ClusterName = tt.update.ClusterName
			}
			if len(tt.update.Databases) > 0 {
				user.Status.Databases = tt.update.Databases
			}
			if tt.update.Username != "" {
				user.Status.Username = tt.update.Username
			}

			if tt.expectValues {
				if user.Status.ClusterName != tt.update.ClusterName {
					t.Errorf("ClusterName = %v, want %v", user.Status.ClusterName, tt.update.ClusterName)
				}
				if len(user.Status.Databases) != len(tt.update.Databases) {
					t.Errorf("Databases count = %v, want %v", len(user.Status.Databases), len(tt.update.Databases))
				}
				if user.Status.Username != tt.update.Username {
					t.Errorf("Username = %v, want %v", user.Status.Username, tt.update.Username)
				}
			}
		})
	}
}

// TestDatabaseUserStatusChangeDetection verifies that status is only updated when meaningful changes occur
// This prevents unnecessary reconciliation loops caused by status patches
func TestDatabaseUserStatusChangeDetection(t *testing.T) {
	tests := []struct {
		name          string
		currentStatus databasesv1alpha1.DatabaseUserStatus
		generation    int64
		update        statusUpdate
		expectChanged bool
	}{
		{
			name: "no change - same phase and message",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Ready",
				Message:            "user created",
				ObservedGeneration: 1,
				ClusterName:        "cluster1",
				Username:           "user1",
				SecretName:         "secret1",
			},
			generation: 1,
			update: statusUpdate{
				Phase:       "Ready",
				Message:     "user created",
				ClusterName: "cluster1",
			},
			expectChanged: false,
		},
		{
			name: "phase changed",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Ready",
				Message:            "user created",
				ObservedGeneration: 1,
			},
			generation: 1,
			update: statusUpdate{
				Phase:   "Failed",
				Message: "connection error",
			},
			expectChanged: true,
		},
		{
			name: "message changed",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Pending",
				Message:            "waiting for database",
				ObservedGeneration: 1,
			},
			generation: 1,
			update: statusUpdate{
				Phase:   "Pending",
				Message: "waiting for cluster",
			},
			expectChanged: true,
		},
		{
			name: "generation changed",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Ready",
				Message:            "user created",
				ObservedGeneration: 1,
			},
			generation: 2,
			update: statusUpdate{
				Phase:   "Ready",
				Message: "user created",
			},
			expectChanged: true,
		},
		{
			name: "password updated flag triggers change",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Ready",
				Message:            "user created",
				ObservedGeneration: 1,
			},
			generation: 1,
			update: statusUpdate{
				Phase:           "Ready",
				Message:         "user created",
				PasswordUpdated: true,
			},
			expectChanged: true,
		},
		{
			name: "cluster name changed",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Ready",
				Message:            "user created",
				ObservedGeneration: 1,
				ClusterName:        "old-cluster",
			},
			generation: 1,
			update: statusUpdate{
				Phase:       "Ready",
				Message:     "user created",
				ClusterName: "new-cluster",
			},
			expectChanged: true,
		},
		{
			name: "databases added triggers change",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Ready",
				Message:            "user created",
				ObservedGeneration: 1,
			},
			generation: 1,
			update: statusUpdate{
				Phase:   "Ready",
				Message: "user created",
				Databases: []databasesv1alpha1.DatabaseAccessStatus{
					{DatabaseName: "new-db", Phase: "Ready"},
				},
			},
			expectChanged: true,
		},
		{
			name: "username changed",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Ready",
				Message:            "user created",
				ObservedGeneration: 1,
				Username:           "old-user",
			},
			generation: 1,
			update: statusUpdate{
				Phase:    "Ready",
				Message:  "user created",
				Username: "new-user",
			},
			expectChanged: true,
		},
		{
			name: "secret name changed",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Ready",
				Message:            "user created",
				ObservedGeneration: 1,
				SecretName:         "old-secret",
			},
			generation: 1,
			update: statusUpdate{
				Phase:      "Ready",
				Message:    "user created",
				SecretName: "new-secret",
			},
			expectChanged: true,
		},
		{
			name: "empty update fields don't trigger change",
			currentStatus: databasesv1alpha1.DatabaseUserStatus{
				Phase:              "Ready",
				Message:            "user created",
				ObservedGeneration: 1,
				ClusterName:        "cluster1",
				Username:           "user1",
				SecretName:         "secret1",
			},
			generation: 1,
			update: statusUpdate{
				Phase:       "Ready",
				Message:     "user created",
				ClusterName: "", // empty - should not trigger change
				Username:    "", // empty - should not trigger change
				SecretName:  "", // empty - should not trigger change
			},
			expectChanged: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the statusChanged check from setStatus
			statusChanged := tt.currentStatus.Phase != tt.update.Phase ||
				tt.currentStatus.Message != tt.update.Message ||
				tt.currentStatus.ObservedGeneration != tt.generation ||
				(tt.update.ClusterName != "" && tt.currentStatus.ClusterName != tt.update.ClusterName) ||
				(tt.update.Username != "" && tt.currentStatus.Username != tt.update.Username) ||
				(tt.update.SecretName != "" && tt.currentStatus.SecretName != tt.update.SecretName) ||
				tt.update.PasswordUpdated ||
				len(tt.update.Databases) > 0

			if statusChanged != tt.expectChanged {
				t.Errorf("statusChanged = %v, want %v", statusChanged, tt.expectChanged)
			}
		})
	}
}

// TestShouldDeleteOldSecret verifies the logic for determining when to delete old secrets
func TestShouldDeleteOldSecret(t *testing.T) {
	tests := []struct {
		name             string
		statusSecretName string
		newSecretName    string
		shouldDelete     bool
	}{
		{
			name:             "secret name changed - should delete",
			statusSecretName: "old-secret",
			newSecretName:    "new-secret",
			shouldDelete:     true,
		},
		{
			name:             "secret name unchanged - should not delete",
			statusSecretName: "my-secret",
			newSecretName:    "my-secret",
			shouldDelete:     false,
		},
		{
			name:             "status secret name empty - should not delete",
			statusSecretName: "",
			newSecretName:    "new-secret",
			shouldDelete:     false,
		},
		{
			name:             "first creation - should not delete",
			statusSecretName: "",
			newSecretName:    "first-secret",
			shouldDelete:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shouldDelete := tt.statusSecretName != "" && tt.statusSecretName != tt.newSecretName

			if shouldDelete != tt.shouldDelete {
				t.Errorf("shouldDelete = %v, want %v", shouldDelete, tt.shouldDelete)
			}
		})
	}
}

// Helper to create a fake reconciler with a fake k8s client
func newTestReconciler(objects ...runtime.Object) *DatabaseUserReconciler {
	scheme := runtime.NewScheme()
	_ = databasesv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithRuntimeObjects(objects...).
		Build()

	return &DatabaseUserReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}
}

func TestDatabaseUserReconciler_RotatePassword(t *testing.T) {
	ctx := context.Background()

	cluster := &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "localhost",
			Port:     5432,
		},
	}

	databases := []*databasesv1alpha1.Database{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "db1"},
			Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "testdb1"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "db2"},
			Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "testdb2"},
		},
	}

	tests := []struct {
		name                string
		user                *databasesv1alpha1.DatabaseUser
		secret              *corev1.Secret
		pgShouldFail        bool
		wantPasswordChanged bool
		wantErr             bool
	}{
		{
			name: "successful rotation with primary mode",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: &databasesv1alpha1.RotationConfig{Days: 30},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user-credentials", Namespace: "default"},
				Data:       map[string][]byte{"password": []byte("oldpassword")},
			},
			wantPasswordChanged: true,
			wantErr:             false,
		},
		{
			name: "rotation with custom password length",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Password: databasesv1alpha1.PasswordConfig{Length: 32},
					Rotation: &databasesv1alpha1.RotationConfig{Days: 7},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user-credentials", Namespace: "default"},
				Data:       map[string][]byte{"password": []byte("oldpassword")},
			},
			wantPasswordChanged: true,
			wantErr:             false,
		},
		{
			name: "postgres failure during rotation",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Rotation: &databasesv1alpha1.RotationConfig{Days: 30},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user-credentials", Namespace: "default"},
				Data:       map[string][]byte{"password": []byte("oldpassword")},
			},
			pgShouldFail:        true,
			wantPasswordChanged: false,
			wantErr:             true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(tt.secret)

			mockPG := postgres.NewMockClient()
			mockPG.ShouldFail = tt.pgShouldFail
			if tt.pgShouldFail {
				mockPG.FailError = errors.New("connection failed")
			}

			password, secretName, passwordChanged, err := r.rotatePassword(ctx, tt.user, tt.secret, cluster, databases, mockPG, "test_user")

			if (err != nil) != tt.wantErr {
				t.Errorf("rotatePassword() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if passwordChanged != tt.wantPasswordChanged {
				t.Errorf("rotatePassword() passwordChanged = %v, want %v", passwordChanged, tt.wantPasswordChanged)
			}

			if !tt.wantErr {
				if password == "" {
					t.Error("rotatePassword() password should not be empty")
				}
				if secretName == "" {
					t.Error("rotatePassword() secretName should not be empty")
				}
			}
		})
	}
}

func TestDatabaseUserReconciler_AdoptSecret(t *testing.T) {
	ctx := context.Background()

	cluster := &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "db.example.com",
			Port:     5432,
		},
	}

	databases := []*databasesv1alpha1.Database{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "mydb"},
			Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "production"},
		},
	}

	tests := []struct {
		name         string
		user         *databasesv1alpha1.DatabaseUser
		secret       *corev1.Secret
		pgShouldFail bool
		wantErr      bool
	}{
		{
			name: "successful adopt with raw template",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "existing-secret", Namespace: "default"},
				Data:       map[string][]byte{"some-key": []byte("some-value")},
			},
			wantErr: false,
		},
		{
			name: "adopt with DB template",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "DB"},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "existing-secret", Namespace: "default"},
				Data:       map[string][]byte{},
			},
			wantErr: false,
		},
		{
			name: "postgres failure during adopt",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "existing-secret", Namespace: "default"},
				Data:       map[string][]byte{},
			},
			pgShouldFail: true,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(tt.secret)

			mockPG := postgres.NewMockClient()
			mockPG.ShouldFail = tt.pgShouldFail
			if tt.pgShouldFail {
				mockPG.FailError = errors.New("connection failed")
			}

			password, secretName, passwordChanged, err := r.adoptSecret(ctx, tt.user, tt.secret, cluster, databases, mockPG, "test_user")

			if (err != nil) != tt.wantErr {
				t.Errorf("adoptSecret() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if password == "" {
					t.Error("adoptSecret() password should not be empty")
				}
				if secretName != tt.secret.Name {
					t.Errorf("adoptSecret() secretName = %v, want %v", secretName, tt.secret.Name)
				}
				if !passwordChanged {
					t.Error("adoptSecret() should always change password")
				}
			}
		})
	}
}

func TestDatabaseUserReconciler_MergeSecret(t *testing.T) {
	ctx := context.Background()

	cluster := &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "db.example.com",
			Port:     5432,
		},
	}

	databases := []*databasesv1alpha1.Database{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "db1"},
			Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "app_db"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "db2"},
			Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "analytics_db"},
		},
	}

	tests := []struct {
		name              string
		user              *databasesv1alpha1.DatabaseUser
		secret            *corev1.Secret
		pgShouldFail      bool
		wantDatabasesList bool
		wantErr           bool
	}{
		{
			name: "successful merge with raw template and multiple databases",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "default"},
				Data: map[string][]byte{
					"existing-key": []byte("preserve-this"),
				},
			},
			wantDatabasesList: true,
			wantErr:           false,
		},
		{
			name: "merge with POSTGRES template - no databases list",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "POSTGRES"},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "default"},
				Data:       map[string][]byte{},
			},
			wantDatabasesList: false,
			wantErr:           false,
		},
		{
			name: "merge with nil secret data",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "empty-secret", Namespace: "default"},
				Data:       nil,
			},
			wantDatabasesList: true,
			wantErr:           false,
		},
		{
			name: "postgres failure during merge",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "shared-secret", Namespace: "default"},
				Data:       map[string][]byte{},
			},
			pgShouldFail: true,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler(tt.secret)

			mockPG := postgres.NewMockClient()
			mockPG.ShouldFail = tt.pgShouldFail
			if tt.pgShouldFail {
				mockPG.FailError = errors.New("connection failed")
			}

			password, secretName, passwordChanged, err := r.mergeSecret(ctx, tt.user, tt.secret, cluster, databases, mockPG, "test_user")

			if (err != nil) != tt.wantErr {
				t.Errorf("mergeSecret() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if password == "" {
					t.Error("mergeSecret() password should not be empty")
				}
				if secretName != tt.secret.Name {
					t.Errorf("mergeSecret() secretName = %v, want %v", secretName, tt.secret.Name)
				}
				if !passwordChanged {
					t.Error("mergeSecret() should always change password")
				}
			}
		})
	}
}

func TestDatabaseUserReconciler_DeleteOldSecret(t *testing.T) {
	ctx := context.Background()

	userUID := types.UID("user-uid-123")

	tests := []struct {
		name         string
		user         *databasesv1alpha1.DatabaseUser
		secret       *corev1.Secret
		secretName   string
		shouldDelete bool
	}{
		{
			name: "deletes owned secret",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default", UID: userUID},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "old-secret",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "DatabaseUser", Name: "test-user", UID: userUID},
					},
				},
			},
			secretName:   "old-secret",
			shouldDelete: true,
		},
		{
			name: "skips non-owned secret",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default", UID: userUID},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "other-secret",
					Namespace: "default",
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "DatabaseUser", Name: "other-user", UID: "other-uid"},
					},
				},
			},
			secretName:   "other-secret",
			shouldDelete: false,
		},
		{
			name: "handles non-existent secret gracefully",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default", UID: userUID},
			},
			secret:       nil,
			secretName:   "non-existent",
			shouldDelete: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r *DatabaseUserReconciler
			if tt.secret != nil {
				r = newTestReconciler(tt.secret)
			} else {
				r = newTestReconciler()
			}

			// Should not panic
			r.deleteOldSecret(ctx, "default", tt.secretName, tt.user)

			if tt.shouldDelete && tt.secret != nil {
				// Verify secret was deleted
				var secret corev1.Secret
				err := r.Get(ctx, types.NamespacedName{Name: tt.secretName, Namespace: "default"}, &secret)
				if err == nil {
					t.Error("deleteOldSecret() should have deleted the secret")
				}
			}
		})
	}
}

func TestDatabaseUserReconciler_CreateDatabaseSecret(t *testing.T) {
	ctx := context.Background()

	cluster := &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "db.example.com",
			Port:     5432,
		},
	}

	database := &databasesv1alpha1.Database{
		ObjectMeta: metav1.ObjectMeta{Name: "mydb"},
		Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "production_db"},
	}

	tests := []struct {
		name       string
		user       *databasesv1alpha1.DatabaseUser
		secretName string
		wantErr    bool
	}{
		{
			name: "creates secret with raw template",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			secretName: "test-user-mydb-credentials",
			wantErr:    false,
		},
		{
			name: "creates secret with DB template",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "DB"},
				},
			},
			secretName: "test-user-mydb-credentials",
			wantErr:    false,
		},
		{
			name: "creates secret with custom template",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{
						Template: "custom",
						Keys: &databasesv1alpha1.SecretKeys{
							Host:     "PGHOST",
							Port:     "PGPORT",
							Database: "PGDATABASE",
							User:     "PGUSER",
							Password: "PGPASSWORD",
						},
					},
				},
			},
			secretName: "test-user-mydb-credentials",
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler()

			err := r.createDatabaseSecret(ctx, tt.user, tt.secretName, cluster, database, "test_user", "securepassword123")

			if (err != nil) != tt.wantErr {
				t.Errorf("createDatabaseSecret() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr { //nolint:nestif // test verification requires multiple checks
				// Verify secret was created
				var secret corev1.Secret
				err := r.Get(ctx, types.NamespacedName{Name: tt.secretName, Namespace: "default"}, &secret)
				if err != nil {
					t.Errorf("createDatabaseSecret() secret not found: %v", err)
					return
				}

				// Verify secret has expected keys based on template
				// Note: fake client doesn't convert StringData to Data, so check StringData
				hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(tt.user)
				if secret.StringData[hostKey] != cluster.Spec.Endpoint {
					t.Errorf("secret host = %v, want %v", secret.StringData[hostKey], cluster.Spec.Endpoint)
				}
				if secret.StringData[portKey] != "5432" {
					t.Errorf("secret port = %v, want 5432", secret.StringData[portKey])
				}
				if secret.StringData[dbKey] != "production_db" {
					t.Errorf("secret database = %v, want production_db", secret.StringData[dbKey])
				}
				if secret.StringData[userKey] != "test_user" {
					t.Errorf("secret user = %v, want test_user", secret.StringData[userKey])
				}
				if secret.StringData[pwdKey] != "securepassword123" {
					t.Errorf("secret password = %v, want securepassword123", secret.StringData[pwdKey])
				}
			}
		})
	}
}

func TestDatabaseUserReconciler_CreatePrimarySecret(t *testing.T) {
	ctx := context.Background()

	cluster := &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "db.example.com",
			Port:     5432,
		},
	}

	tests := []struct {
		name          string
		user          *databasesv1alpha1.DatabaseUser
		databases     []*databasesv1alpha1.Database
		wantDatabases bool
	}{
		{
			name: "single database - no databases field",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			databases: []*databasesv1alpha1.Database{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "db1"},
					Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "testdb"},
				},
			},
			wantDatabases: false,
		},
		{
			name: "multiple databases with raw template - has databases field",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{},
			},
			databases: []*databasesv1alpha1.Database{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "db1"},
					Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "app_db"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "db2"},
					Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "analytics_db"},
				},
			},
			wantDatabases: true,
		},
		{
			name: "multiple databases with POSTGRES template - no databases field",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-user",
					Namespace: "default",
					UID:       "test-uid",
				},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "POSTGRES"},
				},
			},
			databases: []*databasesv1alpha1.Database{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "db1"},
					Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "app_db"},
				},
				{
					ObjectMeta: metav1.ObjectMeta{Name: "db2"},
					Spec:       databasesv1alpha1.DatabaseSpec{DatabaseName: "analytics_db"},
				},
			},
			wantDatabases: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler()

			secretName := "test-user-credentials"
			err := r.createPrimarySecret(ctx, tt.user, secretName, cluster, tt.databases, "test_user", "password123")
			if err != nil {
				t.Errorf("createPrimarySecret() error = %v", err)
				return
			}

			// Verify secret was created
			var secret corev1.Secret
			err = r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, &secret)
			if err != nil {
				t.Errorf("createPrimarySecret() secret not found: %v", err)
				return
			}

			// Check databases field - fake client uses StringData
			_, hasDatabases := secret.StringData["databases"]
			if hasDatabases != tt.wantDatabases {
				t.Errorf("secret has databases field = %v, want %v", hasDatabases, tt.wantDatabases)
			}

			if tt.wantDatabases {
				expectedDatabases := "app_db,analytics_db"
				if secret.StringData["databases"] != expectedDatabases {
					t.Errorf("secret databases = %v, want %v", secret.StringData["databases"], expectedDatabases)
				}
			}

			// Verify primary database is first one
			hostKey, _, dbKey, _, _ := r.getSecretKeys(tt.user)
			if secret.StringData[dbKey] != tt.databases[0].Spec.DatabaseName {
				t.Errorf("primary database = %v, want %v", secret.StringData[dbKey], tt.databases[0].Spec.DatabaseName)
			}

			// Verify host
			if secret.StringData[hostKey] != cluster.Spec.Endpoint {
				t.Errorf("host = %v, want %v", secret.StringData[hostKey], cluster.Spec.Endpoint)
			}
		})
	}
}

func TestDatabaseUserReconciler_UpdateSecretDatabases(t *testing.T) {
	ctx := context.Background()

	cluster := &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec: databasesv1alpha1.DBClusterSpec{
			Endpoint: "db.example.com",
			Port:     5432,
		},
	}

	tests := []struct {
		name              string
		user              *databasesv1alpha1.DatabaseUser
		initialData       map[string][]byte
		databases         []*databasesv1alpha1.Database
		expectUpdate      bool
		wantDatabasesList string
	}{
		{
			name: "add databases list when adding second database",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
				Spec:       databasesv1alpha1.DatabaseUserSpec{},
			},
			initialData: map[string][]byte{
				"host":     []byte("db.example.com"),
				"database": []byte("db1"),
			},
			databases: []*databasesv1alpha1.Database{
				{ObjectMeta: metav1.ObjectMeta{Name: "db1"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "app_db"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "db2"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "cache_db"}},
			},
			expectUpdate:      true,
			wantDatabasesList: "app_db,cache_db",
		},
		{
			name: "remove databases list with non-raw template",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "DB"},
				},
			},
			initialData: map[string][]byte{
				"DB_HOST":   []byte("db.example.com"),
				"DB_NAME":   []byte("old_db"),
				"databases": []byte("old_db,other_db"),
			},
			databases: []*databasesv1alpha1.Database{
				{ObjectMeta: metav1.ObjectMeta{Name: "db1"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "new_db"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "db2"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "other_db"}},
			},
			expectUpdate:      true,
			wantDatabasesList: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "test-secret", Namespace: "default"},
				Data:       tt.initialData,
			}
			r := newTestReconciler(secret)

			err := r.updateSecretDatabases(ctx, tt.user, secret, cluster, tt.databases, "test_user")
			if err != nil {
				t.Errorf("updateSecretDatabases() error = %v", err)
				return
			}

			// Fetch updated secret
			var updatedSecret corev1.Secret
			err = r.Get(ctx, types.NamespacedName{Name: "test-secret", Namespace: "default"}, &updatedSecret)
			if err != nil {
				t.Errorf("failed to get updated secret: %v", err)
				return
			}

			if tt.wantDatabasesList != "" {
				if string(updatedSecret.Data["databases"]) != tt.wantDatabasesList {
					t.Errorf("databases = %v, want %v", string(updatedSecret.Data["databases"]), tt.wantDatabasesList)
				}
			} else {
				if _, exists := updatedSecret.Data["databases"]; exists {
					t.Errorf("databases field should not exist, but got: %v", string(updatedSecret.Data["databases"]))
				}
			}
		})
	}
}
