package controllers

import (
	"reflect"
	"testing"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestUsernameForUser(t *testing.T) {
	tests := []struct {
		name         string
		specUsername string
		metaName     string
		want         string
	}{
		{"uses spec.username when set", "custom_user", "my-user", "custom_user"},
		{"falls back to metadata.name with dash conversion", "", "my-user", "my_user"},
		{"prefers spec.username over dashed name", "explicit", "fallback-name", "explicit"},
		{"converts multiple dashes", "", "my-app-user", "my_app_user"},
		{"no conversion needed", "", "myuser", "myuser"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			user := &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: tt.metaName},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Username: tt.specUsername,
				},
			}
			if got := UsernameForUser(user); got != tt.want {
				t.Errorf("UsernameForUser() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveUserGrants(t *testing.T) {
	tests := []struct {
		name   string
		user   *databasesv1alpha1.DatabaseUser
		access databasesv1alpha1.DatabaseAccess
		want   UserGrants
	}{
		{
			name: "per-database privilege override wins",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Privileges: "readonly",
				},
			},
			access: databasesv1alpha1.DatabaseAccess{Name: "db1", Privileges: "admin"},
			want: UserGrants{
				Username:         "app_user",
				Privileges:       "admin",
				AdditionalGrants: []postgres.TableGrant{},
			},
		},
		{
			name: "falls back to spec.privileges when per-database unset",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Privileges: "readwrite",
				},
			},
			access: databasesv1alpha1.DatabaseAccess{Name: "db1"},
			want: UserGrants{
				Username:         "app_user",
				Privileges:       "readwrite",
				AdditionalGrants: []postgres.TableGrant{},
			},
		},
		{
			name: "defaults to readonly when nothing set",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user"},
			},
			access: databasesv1alpha1.DatabaseAccess{Name: "db1"},
			want: UserGrants{
				Username:         "app_user",
				Privileges:       "readonly",
				AdditionalGrants: []postgres.TableGrant{},
			},
		},
		{
			name: "explicit spec.username used instead of CR name",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Username: "custom_role",
				},
			},
			access: databasesv1alpha1.DatabaseAccess{Name: "db1", Privileges: "readwrite"},
			want: UserGrants{
				Username:         "custom_role",
				Privileges:       "readwrite",
				AdditionalGrants: []postgres.TableGrant{},
			},
		},
		{
			name: "additional grants are translated",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					AdditionalGrants: []databasesv1alpha1.TableGrant{
						{Tables: []string{"t1", "t2"}, Privileges: []string{"SELECT", "INSERT"}},
					},
				},
			},
			access: databasesv1alpha1.DatabaseAccess{Name: "db1", Privileges: "readonly"},
			want: UserGrants{
				Username:   "app_user",
				Privileges: "readonly",
				AdditionalGrants: []postgres.TableGrant{
					{Tables: []string{"t1", "t2"}, Privileges: []postgres.TablePrivilege{"SELECT", "INSERT"}},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveUserGrants(tt.user, tt.access)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ResolveUserGrants() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveUserGrantsForDatabase(t *testing.T) {
	tests := []struct {
		name      string
		user      *databasesv1alpha1.DatabaseUser
		namespace string
		dbName    string
		wantOK    bool
		want      UserGrants
	}{
		{
			name: "single database, namespace defaults to user's namespace",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "team-a"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "orders", Privileges: "readwrite"},
				},
			},
			namespace: "team-a",
			dbName:    "orders",
			wantOK:    true,
			want: UserGrants{
				Username:         "app_user",
				Privileges:       "readwrite",
				AdditionalGrants: []postgres.TableGrant{},
			},
		},
		{
			name: "single database, explicit access namespace must match",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "team-a"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "orders", Namespace: "team-b", Privileges: "admin"},
				},
			},
			namespace: "team-b",
			dbName:    "orders",
			wantOK:    true,
			want: UserGrants{
				Username:         "app_user",
				Privileges:       "admin",
				AdditionalGrants: []postgres.TableGrant{},
			},
		},
		{
			name: "namespace mismatch against explicit access namespace returns not found",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "team-a"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "orders", Namespace: "team-b"},
				},
			},
			namespace: "team-a",
			dbName:    "orders",
			wantOK:    false,
			want:      UserGrants{},
		},
		{
			name: "no matching database name returns not found",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "team-a"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "orders"},
				},
			},
			namespace: "team-a",
			dbName:    "billing",
			wantOK:    false,
			want:      UserGrants{},
		},
		{
			name: "multi-database user matches second entry",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "team-a"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Privileges: "readonly",
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: "orders"},
						{Name: "billing", Privileges: "readwrite"},
					},
				},
			},
			namespace: "team-a",
			dbName:    "billing",
			wantOK:    true,
			want: UserGrants{
				Username:         "app_user",
				Privileges:       "readwrite",
				AdditionalGrants: []postgres.TableGrant{},
			},
		},
		{
			name: "user with no databases configured returns not found",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "app-user", Namespace: "team-a"},
			},
			namespace: "team-a",
			dbName:    "orders",
			wantOK:    false,
			want:      UserGrants{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ResolveUserGrantsForDatabase(tt.user, tt.namespace, tt.dbName)
			if ok != tt.wantOK {
				t.Fatalf("ResolveUserGrantsForDatabase() ok = %v, want %v", ok, tt.wantOK)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ResolveUserGrantsForDatabase() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
