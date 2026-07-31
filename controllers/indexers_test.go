package controllers

import (
	"reflect"
	"testing"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDatabaseUserDatabaseRefKey(t *testing.T) {
	if got := DatabaseUserDatabaseRefKey("ns", "name"); got != "ns/name" {
		t.Errorf("DatabaseUserDatabaseRefKey() = %q, want %q", got, "ns/name")
	}
}

func TestIndexDatabaseUserDatabaseRefs(t *testing.T) {
	tests := []struct {
		name string
		user *databasesv1alpha1.DatabaseUser
		want []string
	}{
		{
			name: "spec.database yields one key, namespace defaults to user's",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "u1", Namespace: "team-a"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "orders"},
				},
			},
			want: []string{"team-a/orders"},
		},
		{
			name: "spec.database with explicit namespace is used verbatim",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "u1", Namespace: "team-a"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "orders", Namespace: "team-b"},
				},
			},
			want: []string{"team-b/orders"},
		},
		{
			name: "spec.databases[] yields N keys",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "u1", Namespace: "team-a"},
				Spec: databasesv1alpha1.DatabaseUserSpec{
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: "orders"},
						{Name: "billing", Namespace: "team-b"},
						{Name: "audit"},
					},
				},
			},
			want: []string{"team-a/orders", "team-b/billing", "team-a/audit"},
		},
		{
			name: "no databases configured yields no keys",
			user: &databasesv1alpha1.DatabaseUser{
				ObjectMeta: metav1.ObjectMeta{Name: "u1", Namespace: "team-a"},
			},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := indexDatabaseUserDatabaseRefs(tt.user)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("indexDatabaseUserDatabaseRefs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestIndexDatabaseUserDatabaseRefs_WrongType(t *testing.T) {
	if got := indexDatabaseUserDatabaseRefs(&corev1.Secret{}); got != nil {
		t.Errorf("indexDatabaseUserDatabaseRefs(non-DatabaseUser) = %#v, want nil", got)
	}
}

func TestDatabaseUserDatabaseRefIndex_FakeClientLookup(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = databasesv1alpha1.AddToScheme(scheme)

	singleDBUser := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "single-db-user", Namespace: "team-a"},
		Spec: databasesv1alpha1.DatabaseUserSpec{
			Database: &databasesv1alpha1.DatabaseAccess{Name: "orders"},
		},
	}
	multiDBUser := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "multi-db-user", Namespace: "team-a"},
		Spec: databasesv1alpha1.DatabaseUserSpec{
			Databases: []databasesv1alpha1.DatabaseAccess{
				{Name: "orders"},
				{Name: "billing", Namespace: "team-b"},
			},
		},
	}
	unrelatedUser := &databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{Name: "unrelated-user", Namespace: "team-a"},
		Spec: databasesv1alpha1.DatabaseUserSpec{
			Database: &databasesv1alpha1.DatabaseAccess{Name: "warehouse"},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(singleDBUser, multiDBUser, unrelatedUser).
		WithIndex(&databasesv1alpha1.DatabaseUser{}, DatabaseUserDatabaseRefIndex, indexDatabaseUserDatabaseRefs).
		Build()

	var users databasesv1alpha1.DatabaseUserList
	err := fakeClient.List(t.Context(), &users, client.MatchingFields{
		DatabaseUserDatabaseRefIndex: DatabaseUserDatabaseRefKey("team-a", "orders"),
	})
	if err != nil {
		t.Fatalf("List with index failed: %v", err)
	}

	got := make(map[string]bool, len(users.Items))
	for _, u := range users.Items {
		got[u.Name] = true
	}
	want := map[string]bool{"single-db-user": true, "multi-db-user": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("indexed lookup for team-a/orders = %v, want %v", got, want)
	}

	var crossNSUsers databasesv1alpha1.DatabaseUserList
	if err := fakeClient.List(t.Context(), &crossNSUsers, client.MatchingFields{
		DatabaseUserDatabaseRefIndex: DatabaseUserDatabaseRefKey("team-b", "billing"),
	}); err != nil {
		t.Fatalf("List with index failed: %v", err)
	}
	if len(crossNSUsers.Items) != 1 || crossNSUsers.Items[0].Name != "multi-db-user" {
		t.Errorf("cross-namespace indexed lookup = %v, want [multi-db-user]", crossNSUsers.Items)
	}
}
