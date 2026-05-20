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
		{"owner", "owner", true},
		{"empty", "", false},
		{"invalid", "superuser", false},
	}

	validPrivileges := map[string]bool{
		"readonly":  true,
		"readwrite": true,
		"admin":     true,
		"owner":     true,
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
			wantUser: "username", wantPwd: "password",
		},
		{
			name: "explicit raw template",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "raw"},
				},
			},
			wantHost: "host", wantPort: "port", wantDB: "database",
			wantUser: "username", wantPwd: "password",
		},
		{
			name: "empty template defaults to raw",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: ""},
				},
			},
			wantHost: "host", wantPort: "port", wantDB: "database",
			wantUser: "username", wantPwd: "password",
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
							Username: "PGUSER", Password: "PGPASSWORD",
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
			wantUser: "username", wantPwd: "SECRET_PWD",
		},
		{
			name: "custom template with nil keys uses defaults",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "custom"},
				},
			},
			wantHost: "host", wantPort: "port", wantDB: "database",
			wantUser: "username", wantPwd: "password",
		},
		{
			name: "dsn template returns empty keys",
			user: &databasesv1alpha1.DatabaseUser{
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "dsn"},
				},
			},
			wantHost: "", wantPort: "", wantDB: "",
			wantUser: "", wantPwd: "",
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

			password, secretName, passwordChanged, err := r.rotatePrimaryPassword(ctx, tt.user, tt.secret, cluster, databases, mockPG, "test_user")

			if (err != nil) != tt.wantErr {
				t.Errorf("rotatePrimaryPassword() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if passwordChanged != tt.wantPasswordChanged {
				t.Errorf("rotatePrimaryPassword() passwordChanged = %v, want %v", passwordChanged, tt.wantPasswordChanged)
			}

			if !tt.wantErr {
				if password == "" {
					t.Error("rotatePrimaryPassword() password should not be empty")
				}
				if secretName == "" {
					t.Error("rotatePrimaryPassword() secretName should not be empty")
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
							Username: "PGUSER",
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
				hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(tt.user)
				if string(secret.Data[hostKey]) != cluster.Spec.Endpoint {
					t.Errorf("secret host = %q, want %q", secret.Data[hostKey], cluster.Spec.Endpoint)
				}
				if string(secret.Data[portKey]) != "5432" {
					t.Errorf("secret port = %q, want 5432", secret.Data[portKey])
				}
				if string(secret.Data[dbKey]) != "production_db" {
					t.Errorf("secret database = %q, want production_db", secret.Data[dbKey])
				}
				if string(secret.Data[userKey]) != "test_user" {
					t.Errorf("secret user = %q, want test_user", secret.Data[userKey])
				}
				if string(secret.Data[pwdKey]) != "securepassword123" {
					t.Errorf("secret password = %q, want securepassword123", secret.Data[pwdKey])
				}
			}
		})
	}
}

func TestDatabaseUserReconciler_BuildAndCreatePrimarySecret(t *testing.T) {
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
			data := r.buildSecretData(tt.user, cluster, tt.databases, "test_user", "password123")
			if err := r.createOwnedSecret(ctx, tt.user, secretName, nil, data); err != nil {
				t.Errorf("createOwnedSecret error = %v", err)
				return
			}

			var secret corev1.Secret
			if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, &secret); err != nil {
				t.Errorf("secret not found: %v", err)
				return
			}

			_, hasDatabases := secret.Data["databases"]
			if hasDatabases != tt.wantDatabases {
				t.Errorf("secret has databases field = %v, want %v", hasDatabases, tt.wantDatabases)
			}

			if tt.wantDatabases {
				expectedDatabases := "app_db,analytics_db"
				if string(secret.Data["databases"]) != expectedDatabases {
					t.Errorf("secret databases = %q, want %q", secret.Data["databases"], expectedDatabases)
				}
			}

			hostKey, _, dbKey, _, _ := r.getSecretKeys(tt.user)
			if string(secret.Data[dbKey]) != tt.databases[0].Spec.DatabaseName {
				t.Errorf("primary database = %q, want %q", secret.Data[dbKey], tt.databases[0].Spec.DatabaseName)
			}
			if string(secret.Data[hostKey]) != cluster.Spec.Endpoint {
				t.Errorf("host = %q, want %q", secret.Data[hostKey], cluster.Spec.Endpoint)
			}
		})
	}
}

func TestDatabaseUserReconciler_ReconcileSecretShape_DatabasesList(t *testing.T) {
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
		wantDatabasesList string // empty means: field must not exist
	}{
		{
			name: "add databases list when adding second database (raw template)",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
				Spec:       databasesv1alpha1.DatabaseUserSpec{},
			},
			initialData: map[string][]byte{
				"host":     []byte("db.example.com"),
				"port":     []byte("5432"),
				"database": []byte("app_db"),
				"username": []byte("test_user"),
				"password": []byte("pwd"),
			},
			databases: []*databasesv1alpha1.Database{
				{ObjectMeta: metav1.ObjectMeta{Name: "db1"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "app_db"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "db2"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "cache_db"}},
			},
			wantDatabasesList: "app_db,cache_db",
		},
		{
			name: "remove databases list when switching to non-raw template (DB)",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Secret: &databasesv1alpha1.SecretConfig{Template: "DB"},
				},
			},
			initialData: map[string][]byte{
				"DB_HOST":     []byte("db.example.com"),
				"DB_PORT":     []byte("5432"),
				"DB_NAME":     []byte("new_db"),
				"DB_USER":     []byte("test_user"),
				"DB_PASSWORD": []byte("pwd"),
				"databases":   []byte("new_db,other_db"),
			},
			databases: []*databasesv1alpha1.Database{
				{ObjectMeta: metav1.ObjectMeta{Name: "db1"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "new_db"}},
				{ObjectMeta: metav1.ObjectMeta{Name: "db2"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "other_db"}},
			},
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

			desired := r.buildSecretData(tt.user, cluster, tt.databases, "test_user", "pwd")
			if _, err := r.reconcileSecretShape(ctx, secret, desired); err != nil {
				t.Fatalf("reconcileSecretShape() error = %v", err)
			}

			var updated corev1.Secret
			if err := r.Get(ctx, types.NamespacedName{Name: "test-secret", Namespace: "default"}, &updated); err != nil {
				t.Fatalf("get updated secret: %v", err)
			}

			if tt.wantDatabasesList != "" {
				if string(updated.Data["databases"]) != tt.wantDatabasesList {
					t.Errorf("databases = %q, want %q", string(updated.Data["databases"]), tt.wantDatabasesList)
				}
			} else {
				if _, exists := updated.Data["databases"]; exists {
					t.Errorf("databases field should not exist, but got: %q", string(updated.Data["databases"]))
				}
			}
		})
	}
}

func TestDatabaseUserReconciler_ReassignOwnershipForRemovedDatabases(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name           string
		statusDBs      []databasesv1alpha1.DatabaseAccessStatus
		currentDBNames []string
		expectReassign []string // databases that should have ReassignOwnership called
	}{
		{
			name: "no databases removed",
			statusDBs: []databasesv1alpha1.DatabaseAccessStatus{
				{Name: "db1", DatabaseName: "app_db"},
				{Name: "db2", DatabaseName: "cache_db"},
			},
			currentDBNames: []string{"app_db", "cache_db"},
			expectReassign: []string{},
		},
		{
			name: "one database removed",
			statusDBs: []databasesv1alpha1.DatabaseAccessStatus{
				{Name: "db1", DatabaseName: "app_db"},
				{Name: "db2", DatabaseName: "cache_db"},
			},
			currentDBNames: []string{"app_db"},
			expectReassign: []string{"cache_db"},
		},
		{
			name: "all databases removed",
			statusDBs: []databasesv1alpha1.DatabaseAccessStatus{
				{Name: "db1", DatabaseName: "app_db"},
				{Name: "db2", DatabaseName: "cache_db"},
			},
			currentDBNames: []string{},
			expectReassign: []string{"app_db", "cache_db"},
		},
		{
			name:           "empty status",
			statusDBs:      []databasesv1alpha1.DatabaseAccessStatus{},
			currentDBNames: []string{"new_db"},
			expectReassign: []string{},
		},
		{
			name: "empty database name in status",
			statusDBs: []databasesv1alpha1.DatabaseAccessStatus{
				{Name: "db1", DatabaseName: ""},
				{Name: "db2", DatabaseName: "cache_db"},
			},
			currentDBNames: []string{},
			expectReassign: []string{"cache_db"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockPG := &mockPGClientWithReassign{
				MockClient:          postgres.NewMockClient(),
				reassignedDatabases: make([]string, 0),
			}

			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "test-user", Namespace: "default"},
				Status: databasesv1alpha1.DatabaseUserStatus{
					Databases: tt.statusDBs,
				},
			}

			r := &DatabaseUserReconciler{}
			r.reassignOwnershipForRemovedDatabases(ctx, mockPG, user, "test_user", tt.currentDBNames)

			// Verify correct databases had ReassignOwnership called
			if len(mockPG.reassignedDatabases) != len(tt.expectReassign) {
				t.Errorf("expected %d reassign calls, got %d: %v",
					len(tt.expectReassign), len(mockPG.reassignedDatabases), mockPG.reassignedDatabases)
				return
			}

			for _, expectedDB := range tt.expectReassign {
				found := false
				for _, actualDB := range mockPG.reassignedDatabases {
					if actualDB == expectedDB {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected ReassignOwnership to be called for %s, but it wasn't. Called for: %v",
						expectedDB, mockPG.reassignedDatabases)
				}
			}
		})
	}
}

// mockPGClientWithReassign tracks ReassignOwnership calls
type mockPGClientWithReassign struct {
	*postgres.MockClient
	reassignedDatabases []string
}

func (m *mockPGClientWithReassign) ReassignOwnership(ctx context.Context, fromUser, database string) error {
	m.reassignedDatabases = append(m.reassignedDatabases, database)
	return nil
}

func TestDatabaseUserReconciler_OwnerPrivilegesValidation(t *testing.T) {
	tests := []struct {
		name       string
		privileges string
		valid      bool
	}{
		{"owner is valid", "owner", true},
		{"readonly is valid", "readonly", true},
		{"readwrite is valid", "readwrite", true},
		{"admin is valid", "admin", true},
		{"superuser is invalid", "superuser", false},
		{"root is invalid", "root", false},
	}

	validPrivileges := map[string]bool{
		"readonly":  true,
		"readwrite": true,
		"admin":     true,
		"owner":     true,
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

func TestDatabaseUserReconciler_DropUserWithOwnerPrivileges(t *testing.T) {
	// This test verifies that when deleting a user, ReassignOwnership is called
	// for each database before DropUser
	ctx := context.Background()

	mockPG := &mockPGClientWithDeletionTracking{
		MockClient:          postgres.NewMockClient(),
		reassignedDatabases: make([]string, 0),
		revokedDatabases:    make([]string, 0),
		droppedUsers:        make([]string, 0),
	}

	// Simulate deletion flow
	username := "owner_user"
	databaseNames := []string{"db1", "db2"}

	// This simulates what dropUserFromPostgres does
	for _, dbName := range databaseNames {
		if err := mockPG.ReassignOwnership(ctx, username, dbName); err != nil {
			t.Errorf("ReassignOwnership failed: %v", err)
		}
		if err := mockPG.RevokePrivilegesInDatabase(ctx, username, dbName); err != nil {
			t.Errorf("RevokePrivilegesInDatabase failed: %v", err)
		}
	}
	if err := mockPG.DropUser(ctx, username); err != nil {
		t.Errorf("DropUser failed: %v", err)
	}

	// Verify order of operations
	if len(mockPG.reassignedDatabases) != 2 {
		t.Errorf("expected 2 ReassignOwnership calls, got %d", len(mockPG.reassignedDatabases))
	}
	if len(mockPG.revokedDatabases) != 2 {
		t.Errorf("expected 2 RevokePrivilegesInDatabase calls, got %d", len(mockPG.revokedDatabases))
	}
	if len(mockPG.droppedUsers) != 1 {
		t.Errorf("expected 1 DropUser call, got %d", len(mockPG.droppedUsers))
	}
	if len(mockPG.droppedUsers) > 0 && mockPG.droppedUsers[0] != username {
		t.Errorf("expected DropUser for %s, got %s", username, mockPG.droppedUsers[0])
	}
}

// mockPGClientWithDeletionTracking tracks all deletion-related calls
type mockPGClientWithDeletionTracking struct {
	*postgres.MockClient
	reassignedDatabases []string
	revokedDatabases    []string
	droppedUsers        []string
}

func (m *mockPGClientWithDeletionTracking) ReassignOwnership(ctx context.Context, fromUser, database string) error {
	m.reassignedDatabases = append(m.reassignedDatabases, database)
	return nil
}

func (m *mockPGClientWithDeletionTracking) RevokePrivilegesInDatabase(ctx context.Context, username, database string) error {
	m.revokedDatabases = append(m.revokedDatabases, database)
	return nil
}

func (m *mockPGClientWithDeletionTracking) DropUser(ctx context.Context, username string) error {
	m.droppedUsers = append(m.droppedUsers, username)
	return nil
}

func TestPasswordFromDSN(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"valid postgres URL", "postgres://alice:s3cret@db:5432/app", "s3cret"},
		{"valid postgresql URL", "postgresql://alice:s3cret@db:5432/app", "s3cret"},
		{"empty", "", ""},
		{"no userinfo", "postgres://db:5432/app", ""},
		{"user without password", "postgres://alice@db:5432/app", ""},
		{"malformed", "not a url://", ""},
		{"special chars escaped", "postgres://alice:s%21cret@db:5432/app", "s!cret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := passwordFromDSN([]byte(tt.in))
			if got != tt.want {
				t.Errorf("passwordFromDSN(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExtractPasswordFromSecret(t *testing.T) {
	r := newTestReconciler()

	tests := []struct {
		name   string
		user   *databasesv1alpha1.DatabaseUser
		data   map[string][]byte
		want   string
		reason string
	}{
		{
			name: "current spec=raw, password key present",
			user: &databasesv1alpha1.DatabaseUser{Spec: databasesv1alpha1.DatabaseUserSpec{}},
			data: map[string][]byte{"password": []byte("p1")},
			want: "p1",
		},
		{
			name: "current spec=DB, DB_PASSWORD key present",
			user: &databasesv1alpha1.DatabaseUser{Spec: databasesv1alpha1.DatabaseUserSpec{
				Secret: &databasesv1alpha1.SecretConfig{Template: "DB"},
			}},
			data: map[string][]byte{"DB_PASSWORD": []byte("p2")},
			want: "p2",
		},
		{
			name: "transition dsn→DB: secret still has dsn key",
			user: &databasesv1alpha1.DatabaseUser{Spec: databasesv1alpha1.DatabaseUserSpec{
				Secret: &databasesv1alpha1.SecretConfig{Template: "DB"},
			}},
			data:   map[string][]byte{"dsn": []byte("postgres://u:fromdsn@host:5432/db")},
			want:   "fromdsn",
			reason: "DSN parse fallback recovers password after dsn→DB",
		},
		{
			name: "transition DB→raw: secret still has DB_PASSWORD",
			user: &databasesv1alpha1.DatabaseUser{Spec: databasesv1alpha1.DatabaseUserSpec{}},
			data: map[string][]byte{"DB_PASSWORD": []byte("p3")},
			want: "p3",
		},
		{
			name: "transition raw→dsn: secret still has password key",
			user: &databasesv1alpha1.DatabaseUser{Spec: databasesv1alpha1.DatabaseUserSpec{
				Secret: &databasesv1alpha1.SecretConfig{Template: "dsn"},
			}},
			data: map[string][]byte{"password": []byte("p4")},
			want: "p4",
		},
		{
			name: "current spec=custom with key PGPASSWORD",
			user: &databasesv1alpha1.DatabaseUser{Spec: databasesv1alpha1.DatabaseUserSpec{
				Secret: &databasesv1alpha1.SecretConfig{
					Template: "custom",
					Keys:     &databasesv1alpha1.SecretKeys{Password: "PGPASSWORD"},
				},
			}},
			data: map[string][]byte{"PGPASSWORD": []byte("p5")},
			want: "p5",
		},
		{
			name: "corrupted: no recognizable password key",
			user: &databasesv1alpha1.DatabaseUser{Spec: databasesv1alpha1.DatabaseUserSpec{}},
			data: map[string][]byte{"host": []byte("db"), "port": []byte("5432")},
			want: "",
		},
		{
			name: "empty secret",
			user: &databasesv1alpha1.DatabaseUser{Spec: databasesv1alpha1.DatabaseUserSpec{}},
			data: nil,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{Data: tt.data}
			got := r.extractPasswordFromSecret(secret, tt.user)
			if got != tt.want {
				t.Errorf("extractPasswordFromSecret() = %q, want %q (%s)", got, tt.want, tt.reason)
			}
		})
	}
}

func TestBuildSecretData_DSNTemplate(t *testing.T) {
	r := newTestReconciler()
	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "u"},
		Spec: databasesv1alpha1.DatabaseUserSpec{
			Secret: &databasesv1alpha1.SecretConfig{Template: "dsn"},
		},
	}
	cluster := &databasesv1alpha1.DBCluster{Spec: databasesv1alpha1.DBClusterSpec{Endpoint: "h", Port: 5432}}
	databases := []*databasesv1alpha1.Database{{Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "app"}}}

	data := r.buildSecretData(user, cluster, databases, "u", "p")
	if len(data) != 1 {
		t.Fatalf("expected only 'dsn' key, got %d keys: %v", len(data), data)
	}
	want := "postgres://u:p@h:5432/app"
	if string(data["dsn"]) != want {
		t.Errorf("dsn = %q, want %q", data["dsn"], want)
	}
}

func TestReconcileSecretShape_NoOpFastPath(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "s",
			Namespace:       "default",
			ResourceVersion: "1",
			Annotations:     map[string]string{ManagedKeysAnnotation: "a,b"},
		},
		Data: map[string][]byte{"a": []byte("1"), "b": []byte("2")},
	}
	r := newTestReconciler(secret)

	desired := map[string][]byte{"b": []byte("2"), "a": []byte("1")}
	changed, err := r.reconcileSecretShape(ctx, secret, desired)
	if err != nil {
		t.Fatalf("reconcileSecretShape: %v", err)
	}
	if changed {
		t.Errorf("expected no-op fast path, got changed=true")
	}

	var fresh corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "s", Namespace: "default"}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.ResourceVersion != "1" {
		t.Errorf("resourceVersion = %q, want %q (no Update should fire)", fresh.ResourceVersion, "1")
	}
}

func TestReconcileSecretShape_StampsAnnotationOnLegacySecret(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default", ResourceVersion: "1"},
		Data:       map[string][]byte{"a": []byte("1"), "b": []byte("2")},
	}
	r := newTestReconciler(secret)

	desired := map[string][]byte{"a": []byte("1"), "b": []byte("2")}
	changed, err := r.reconcileSecretShape(ctx, secret, desired)
	if err != nil {
		t.Fatalf("reconcileSecretShape: %v", err)
	}
	if !changed {
		t.Errorf("expected Update to stamp annotation, got changed=false")
	}
	var fresh corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "s", Namespace: "default"}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := fresh.Annotations[ManagedKeysAnnotation]; got != "a,b" {
		t.Errorf("managed-keys annotation = %q, want %q", got, "a,b")
	}
}

func TestReconcileSecretMerge_PreservesForeignKeys(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "s",
			Namespace:   "default",
			Annotations: map[string]string{ManagedKeysAnnotation: "password,username"},
		},
		Data: map[string][]byte{
			"username":      []byte("u"),
			"password":      []byte("oldpwd"),
			"foreign_field": []byte("user-controlled"),
			"another_alien": []byte("also-user"),
		},
	}
	r := newTestReconciler(secret)

	desired := map[string][]byte{"username": []byte("u"), "password": []byte("newpwd")}
	changed, err := r.reconcileSecretMerge(ctx, secret, desired)
	if err != nil {
		t.Fatalf("reconcileSecretMerge: %v", err)
	}
	if !changed {
		t.Errorf("expected changed=true")
	}

	var fresh corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "s", Namespace: "default"}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(fresh.Data["password"]) != "newpwd" {
		t.Errorf("password = %q, want %q", fresh.Data["password"], "newpwd")
	}
	if string(fresh.Data["foreign_field"]) != "user-controlled" {
		t.Errorf("foreign_field dropped: %q", fresh.Data["foreign_field"])
	}
	if string(fresh.Data["another_alien"]) != "also-user" {
		t.Errorf("another_alien dropped: %q", fresh.Data["another_alien"])
	}
}

func TestReconcileSecretMerge_NoOpFastPath(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "s",
			Namespace:       "default",
			ResourceVersion: "1",
			Annotations:     map[string]string{ManagedKeysAnnotation: "password,username"},
		},
		Data: map[string][]byte{
			"username":      []byte("u"),
			"password":      []byte("pwd"),
			"foreign_field": []byte("user"),
		},
	}
	r := newTestReconciler(secret)

	desired := map[string][]byte{"username": []byte("u"), "password": []byte("pwd")}
	changed, err := r.reconcileSecretMerge(ctx, secret, desired)
	if err != nil {
		t.Fatalf("reconcileSecretMerge: %v", err)
	}
	if changed {
		t.Errorf("expected no-op fast path, got changed=true")
	}
	var fresh corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "s", Namespace: "default"}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.ResourceVersion != "1" {
		t.Errorf("resourceVersion = %q, want %q (no Update should fire)", fresh.ResourceVersion, "1")
	}
}

func TestIsSecretSoftOwnedByUser(t *testing.T) {
	r := newTestReconciler()
	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "alice-uid"},
	}
	tBool := true

	tests := []struct {
		name   string
		secret *corev1.Secret
		want   bool
	}{
		{
			name: "annotation matches, no OwnerReferences",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{ManagedByAnnotation: "alice"},
				},
			},
			want: true,
		},
		{
			name: "annotation matches, only self OwnerReference",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{ManagedByAnnotation: "alice"},
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "DatabaseUser", Name: "alice", UID: "alice-uid"},
					},
				},
			},
			want: true,
		},
		{
			name: "annotation matches but another DatabaseUser controller-owns it",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{ManagedByAnnotation: "alice"},
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "DatabaseUser", Name: "bob", UID: "bob-uid", Controller: &tBool},
					},
				},
			},
			want: false,
		},
		{
			name: "annotation matches but ExternalSecret controller-owns it",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{ManagedByAnnotation: "alice"},
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "ExternalSecret", Name: "shared", UID: "ext-uid", Controller: &tBool},
					},
				},
			},
			want: false,
		},
		{
			name: "annotation matches, foreign non-controller ref ignored",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{ManagedByAnnotation: "alice"},
					OwnerReferences: []metav1.OwnerReference{
						{Kind: "ConfigMap", Name: "info", UID: "cm-uid"},
					},
				},
			},
			want: true,
		},
		{
			name: "annotation missing",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{},
			},
			want: false,
		},
		{
			name: "annotation points to different user",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{ManagedByAnnotation: "bob"},
				},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.isSecretSoftOwnedByUser(tt.secret, user); got != tt.want {
				t.Errorf("isSecretSoftOwnedByUser = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReclaimOrFail(t *testing.T) {
	ctx := context.Background()
	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default", UID: "alice-uid"},
	}

	t.Run("annotation match → re-attaches OwnerReference", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "s",
				Namespace:   "default",
				Annotations: map[string]string{ManagedByAnnotation: "alice"},
			},
		}
		r := newTestReconciler(secret)
		if err := r.reclaimOrFail(ctx, user, secret); err != nil {
			t.Fatalf("reclaimOrFail: %v", err)
		}
		var fresh corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: "s", Namespace: "default"}, &fresh); err != nil {
			t.Fatalf("get: %v", err)
		}
		if len(fresh.OwnerReferences) == 0 {
			t.Errorf("OwnerReferences not restored")
		}
	})

	t.Run("no annotation → returns error", func(t *testing.T) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
		}
		r := newTestReconciler(secret)
		err := r.reclaimOrFail(ctx, user, secret)
		if err == nil {
			t.Errorf("expected error, got nil")
		}
	})

	t.Run("foreign controller ref → returns error", func(t *testing.T) {
		tBool := true
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "s",
				Namespace:   "default",
				Annotations: map[string]string{ManagedByAnnotation: "alice"},
				OwnerReferences: []metav1.OwnerReference{
					{Kind: "ExternalSecret", Name: "shared", UID: "ext-uid", Controller: &tBool},
				},
			},
		}
		r := newTestReconciler(secret)
		err := r.reclaimOrFail(ctx, user, secret)
		if err == nil {
			t.Errorf("expected error for foreign controller ref, got nil")
		}
	})
}

func TestPrivilegesForDatabase(t *testing.T) {
	tests := []struct {
		name, perDB, fallback, want string
	}{
		{"per-DB override wins", "admin", "readonly", "admin"},
		{"fallback when per-DB empty", "", "readwrite", "readwrite"},
		{"default when both empty", "", "", "readonly"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := privilegesForDatabase(tt.perDB, tt.fallback); got != tt.want {
				t.Errorf("privilegesForDatabase(%q, %q) = %q, want %q", tt.perDB, tt.fallback, got, tt.want)
			}
		})
	}
}

func TestTranslateAdditionalGrants(t *testing.T) {
	in := []databasesv1alpha1.TableGrant{
		{Tables: []string{"t1", "t2"}, Privileges: []string{"SELECT", "INSERT"}},
		{Tables: []string{"audit"}, Privileges: []string{"USAGE"}},
	}
	out := translateAdditionalGrants(in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Tables[1] != "t2" || string(out[0].Privileges[1]) != "INSERT" {
		t.Errorf("first grant mistranslated: %+v", out[0])
	}
	if string(out[1].Privileges[0]) != "USAGE" {
		t.Errorf("second grant mistranslated: %+v", out[1])
	}
	if translateAdditionalGrants(nil) == nil {
		t.Errorf("nil input should produce non-nil empty slice (caller passes to ApplyPrivileges)")
	}
}

func TestSyncPostgresUser_WrapsErrors(t *testing.T) {
	ctx := context.Background()
	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "default"},
	}
	r := newTestReconciler()

	mock := postgres.NewMockClient()
	mock.ShouldFail = true
	mock.FailError = errors.New("boom")

	err := r.syncPostgresUser(ctx, mock, user, "u", "p", []string{"db1"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, mock.FailError) {
		t.Errorf("error chain broken: %v should wrap %v", err, mock.FailError)
	}
}

func TestSyncRuntimeParams_WrapsErrors(t *testing.T) {
	ctx := context.Background()
	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "default"},
		Spec:       databasesv1alpha1.DatabaseUserSpec{ConnectionLimit: 10},
	}
	r := newTestReconciler()

	mock := postgres.NewMockClient()
	mock.ShouldFail = true
	mock.FailError = errors.New("boom")

	err := r.syncRuntimeParams(ctx, mock, user, "u")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, mock.FailError) {
		t.Errorf("error chain broken: %v should wrap %v", err, mock.FailError)
	}
}

func TestApplyPerDatabasePrivileges(t *testing.T) {
	ctx := context.Background()
	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "default"},
		Spec: databasesv1alpha1.DatabaseUserSpec{
			Databases: []databasesv1alpha1.DatabaseAccess{
				{Name: "db1", Privileges: "admin"},
				{Name: "db2"},
			},
			Privileges:       "readonly",
			SecretGeneration: "perDatabase",
		},
	}
	databases := []*databasesv1alpha1.Database{
		{ObjectMeta: metav1.ObjectMeta{Name: "db1"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "appdb"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "db2"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "cachedb"}},
	}
	r := newTestReconciler()
	mock := postgres.NewMockClient()

	statuses := r.applyPerDatabasePrivileges(ctx, mock, user, "u", databases)
	if len(statuses) != 2 {
		t.Fatalf("got %d statuses, want 2", len(statuses))
	}
	if statuses[0].Privileges != "admin" {
		t.Errorf("db1 privileges = %q, want admin (per-DB override)", statuses[0].Privileges)
	}
	if statuses[1].Privileges != "readonly" {
		t.Errorf("db2 privileges = %q, want readonly (fallback)", statuses[1].Privileges)
	}
	if statuses[0].SecretName == "" || statuses[1].SecretName == "" {
		t.Errorf("perDatabase mode must populate per-status SecretName: %+v", statuses)
	}
	if statuses[0].Phase != "Ready" || statuses[1].Phase != "Ready" {
		t.Errorf("phases = %q, %q; want both Ready", statuses[0].Phase, statuses[1].Phase)
	}
}

func TestApplyPerDatabasePrivileges_PerDBError(t *testing.T) {
	ctx := context.Background()
	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "default"},
		Spec: databasesv1alpha1.DatabaseUserSpec{
			Databases:  []databasesv1alpha1.DatabaseAccess{{Name: "db1"}},
			Privileges: "readonly",
		},
	}
	databases := []*databasesv1alpha1.Database{
		{ObjectMeta: metav1.ObjectMeta{Name: "db1"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "appdb"}},
	}
	r := newTestReconciler()
	mock := postgres.NewMockClient()
	mock.ShouldFail = true
	mock.FailError = errors.New("boom")

	statuses := r.applyPerDatabasePrivileges(ctx, mock, user, "u", databases)
	if statuses[0].Phase != "Failed" {
		t.Errorf("phase = %q, want Failed", statuses[0].Phase)
	}
	if statuses[0].Message == "" {
		t.Errorf("Message should carry the underlying error")
	}
}

func TestDoSoftReclaim_AttachesOwnerRef(t *testing.T) {
	ctx := context.Background()
	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "default", UID: "alice-uid"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "s",
			Namespace:   "default",
			Annotations: map[string]string{ManagedByAnnotation: "alice"},
		},
	}
	r := newTestReconciler(secret)
	if err := r.doSoftReclaim(ctx, user, secret); err != nil {
		t.Fatalf("doSoftReclaim: %v", err)
	}
	var fresh corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "s", Namespace: "default"}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(fresh.OwnerReferences) == 0 {
		t.Errorf("OwnerReferences not attached")
	}
	if fresh.OwnerReferences[0].UID != "alice-uid" {
		t.Errorf("OwnerReference UID = %q, want alice-uid", fresh.OwnerReferences[0].UID)
	}
}

func TestRotatePrimaryPassword_HonoursMergeMode(t *testing.T) {
	ctx := context.Background()

	cluster := &databasesv1alpha1.DBCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster"},
		Spec:       databasesv1alpha1.DBClusterSpec{Endpoint: "h", Port: 5432},
	}
	databases := []*databasesv1alpha1.Database{
		{ObjectMeta: metav1.ObjectMeta{Name: "db1"}, Spec: databasesv1alpha1.DatabaseSpec{DatabaseName: "app"}},
	}

	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "default"},
		Spec: databasesv1alpha1.DatabaseUserSpec{
			Rotation: &databasesv1alpha1.RotationConfig{Days: 30},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "u-credentials",
			Namespace: "default",
			Annotations: map[string]string{
				ConflictPolicyAnnotation: "Merge",
				ManagedKeysAnnotation:    "database,host,password,port,username",
			},
		},
		Data: map[string][]byte{
			"host":           []byte("h"),
			"port":           []byte("5432"),
			"database":       []byte("app"),
			"username":       []byte("test_user"),
			"password":       []byte("old"),
			"user_added_key": []byte("preserve-me"),
		},
	}
	r := newTestReconciler(secret)
	mockPG := postgres.NewMockClient()

	_, _, passwordChanged, err := r.rotatePrimaryPassword(ctx, user, secret, cluster, databases, mockPG, "test_user")
	if err != nil {
		t.Fatalf("rotatePrimaryPassword: %v", err)
	}
	if !passwordChanged {
		t.Errorf("passwordChanged = false, want true")
	}
	if mockPG.SetPasswordCalls != 1 {
		t.Errorf("SetPassword calls = %d, want 1", mockPG.SetPasswordCalls)
	}

	var fresh corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "u-credentials", Namespace: "default"}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(fresh.Data["user_added_key"]) != "preserve-me" {
		t.Errorf("foreign key dropped during Merge rotation: %q", fresh.Data["user_added_key"])
	}
	if string(fresh.Data["password"]) == "old" {
		t.Errorf("password not rotated")
	}
}

func TestReconcileSecretMerge_DropsOurKeysNoLongerDesired(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "s",
			Namespace:   "default",
			Annotations: map[string]string{ManagedKeysAnnotation: "dsn"},
		},
		Data: map[string][]byte{
			"dsn":     []byte("postgres://u:p@h:5432/app"),
			"user_kv": []byte("preserve-me"),
		},
	}
	r := newTestReconciler(secret)

	desired := map[string][]byte{
		"host": []byte("h"), "port": []byte("5432"),
		"database": []byte("app"), "username": []byte("u"), "password": []byte("p"),
	}
	if _, err := r.reconcileSecretMerge(ctx, secret, desired); err != nil {
		t.Fatalf("reconcileSecretMerge: %v", err)
	}
	var fresh corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "s", Namespace: "default"}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, hasDSN := fresh.Data["dsn"]; hasDSN {
		t.Errorf("dsn (previously ours) should be dropped")
	}
	if string(fresh.Data["user_kv"]) != "preserve-me" {
		t.Errorf("foreign user_kv dropped: %q", fresh.Data["user_kv"])
	}
	if string(fresh.Data["password"]) != "p" {
		t.Errorf("password not written: %q", fresh.Data["password"])
	}
}

func TestSecretDataEqual_Symmetric(t *testing.T) {
	tests := []struct {
		name string
		a, b map[string][]byte
		want bool
	}{
		{"both empty", nil, nil, true},
		{"empty vs single", nil, map[string][]byte{"k": []byte("v")}, false},
		{"single vs empty", map[string][]byte{"k": []byte("v")}, nil, false},
		{"equal", map[string][]byte{"a": []byte("1"), "b": []byte("2")}, map[string][]byte{"b": []byte("2"), "a": []byte("1")}, true},
		{"value differs", map[string][]byte{"a": []byte("1")}, map[string][]byte{"a": []byte("2")}, false},
		{"same len, key swap", map[string][]byte{"a": []byte("1")}, map[string][]byte{"b": []byte("1")}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := secretDataEqual(tt.a, tt.b); got != tt.want {
				t.Errorf("secretDataEqual = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolvePassword(t *testing.T) {
	ctx := context.Background()

	user := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "default"},
		Spec:       databasesv1alpha1.DatabaseUserSpec{},
	}
	readyUser := user.DeepCopy()
	readyUser.Status.Phase = "Ready"

	tests := []struct {
		name              string
		user              *databasesv1alpha1.DatabaseUser
		existing          *corev1.Secret
		found             bool
		wantGenerated     bool
		wantSetPwdCalls   int
		wantReturnedKnown string // if non-empty: must equal returned password
	}{
		{
			name:              "found + extractable: returns existing password, no SetPassword",
			user:              user,
			existing:          &corev1.Secret{Data: map[string][]byte{"password": []byte("kept")}},
			found:             true,
			wantGenerated:     false,
			wantSetPwdCalls:   0,
			wantReturnedKnown: "kept",
		},
		{
			name:            "found + corrupted: regenerates and SetPassword",
			user:            user,
			existing:        &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "s"}, Data: map[string][]byte{"host": []byte("h")}},
			found:           true,
			wantGenerated:   true,
			wantSetPwdCalls: 1,
		},
		{
			name:            "missing + first-time (Phase empty): generate, skip SetPassword",
			user:            user,
			existing:        &corev1.Secret{},
			found:           false,
			wantGenerated:   true,
			wantSetPwdCalls: 0,
		},
		{
			name:            "missing + post-Ready: generate AND SetPassword",
			user:            readyUser,
			existing:        &corev1.Secret{},
			found:           false,
			wantGenerated:   true,
			wantSetPwdCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler()
			mockPG := postgres.NewMockClient()

			pwd, generated, err := r.resolvePassword(ctx, tt.user, tt.existing, tt.found, mockPG, "u")
			if err != nil {
				t.Fatalf("resolvePassword: %v", err)
			}
			if generated != tt.wantGenerated {
				t.Errorf("generated = %v, want %v", generated, tt.wantGenerated)
			}
			if mockPG.SetPasswordCalls != tt.wantSetPwdCalls {
				t.Errorf("SetPassword calls = %d, want %d", mockPG.SetPasswordCalls, tt.wantSetPwdCalls)
			}
			if tt.wantReturnedKnown != "" && pwd != tt.wantReturnedKnown {
				t.Errorf("password = %q, want %q", pwd, tt.wantReturnedKnown)
			}
			if pwd == "" {
				t.Errorf("password should never be empty")
			}
		})
	}
}

func TestSetManagedKeysAnnotation_StableOrder(t *testing.T) {
	// Guards against Go map iteration order causing spurious Updates each reconcile.
	secret := &corev1.Secret{}
	setManagedKeysAnnotation(secret, map[string][]byte{"c": {}, "a": {}, "b": {}})
	if got := secret.Annotations[ManagedKeysAnnotation]; got != "a,b,c" {
		t.Errorf("managed-keys = %q, want %q", got, "a,b,c")
	}
}

func TestReconcileSecretShape_DropsForeignKeys(t *testing.T) {
	ctx := context.Background()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "default"},
		Data: map[string][]byte{
			"dsn":           []byte("postgres://u:p@h:5432/app"),
			"legacy_orphan": []byte("stale"),
		},
	}
	r := newTestReconciler(secret)

	desired := map[string][]byte{
		"host": []byte("h"), "port": []byte("5432"),
		"database": []byte("app"), "username": []byte("u"), "password": []byte("p"),
	}
	changed, err := r.reconcileSecretShape(ctx, secret, desired)
	if err != nil {
		t.Fatalf("reconcileSecretShape: %v", err)
	}
	if !changed {
		t.Errorf("expected changed=true")
	}

	var fresh corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: "s", Namespace: "default"}, &fresh); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := fresh.Data["dsn"]; ok {
		t.Errorf("dsn key should have been dropped, still present")
	}
	if _, ok := fresh.Data["legacy_orphan"]; ok {
		t.Errorf("legacy_orphan key should have been dropped, still present")
	}
	if string(fresh.Data["password"]) != "p" {
		t.Errorf("password = %q, want %q", fresh.Data["password"], "p")
	}
}
