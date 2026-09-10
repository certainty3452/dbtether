package controllers

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

func ownerTestUser(namespace, name string, created time.Time, spec *databasesv1alpha1.DatabaseUserSpec) databasesv1alpha1.DatabaseUser {
	return databasesv1alpha1.DatabaseUser{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(created),
		},
		Spec: *spec,
	}
}

func terminatingOwnerTestUser(namespace, name string, created time.Time,
	spec *databasesv1alpha1.DatabaseUserSpec) databasesv1alpha1.DatabaseUser {

	user := ownerTestUser(namespace, name, created, spec)
	deleted := metav1.NewTime(created.Add(time.Minute))
	user.DeletionTimestamp = &deleted
	user.Finalizers = []string{UserFinalizerName}
	return user
}

func TestElectOwner(t *testing.T) {
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	older := base
	newer := base.Add(time.Hour)

	tests := []struct {
		name        string
		users       []databasesv1alpha1.DatabaseUser
		dbNamespace string
		dbName      string
		want        string
	}{
		{
			name:        "no users",
			users:       nil,
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "",
		},
		{
			name: "nobody resolves to owner",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "reader", older, &databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "orders"},
				}),
				ownerTestUser("team-a", "writer", newer, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "readwrite",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "",
		},
		{
			name: "single owner",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "reader", older, &databasesv1alpha1.DatabaseUserSpec{
					Database: &databasesv1alpha1.DatabaseAccess{Name: "orders"},
				}),
				ownerTestUser("team-a", "boss", newer, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "boss",
		},
		{
			name: "older owner wins regardless of slice order",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "aaa-newer", newer, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
				ownerTestUser("team-a", "zzz-older", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "zzz-older",
		},
		{
			name: "same timestamp falls back to namespace order",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-b", "aaa", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders", Namespace: "team-a"},
					Privileges: "owner",
				}),
				ownerTestUser("team-a", "zzz", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "zzz",
		},
		{
			name: "same timestamp and namespace falls back to name order",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "zzz", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
				ownerTestUser("team-a", "aaa", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "aaa",
		},
		{
			name: "per-database owner counts, spec-level owner overridden per database does not",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "downgraded", older, &databasesv1alpha1.DatabaseUserSpec{
					Privileges: "owner",
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: "orders", Privileges: "readwrite"},
						{Name: "billing"},
					},
				}),
				ownerTestUser("team-a", "promoted", newer, &databasesv1alpha1.DatabaseUserSpec{
					Privileges: "readonly",
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: "orders", Privileges: "owner"},
					},
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "promoted",
		},
		{
			name: "spec-level owner still applies to databases without an override",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "downgraded", older, &databasesv1alpha1.DatabaseUserSpec{
					Privileges: "owner",
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: "orders", Privileges: "readwrite"},
						{Name: "billing"},
					},
				}),
			},
			dbNamespace: "team-a",
			dbName:      "billing",
			want:        "downgraded",
		},
		{
			name: "terminating owner yields to the next candidate",
			users: []databasesv1alpha1.DatabaseUser{
				terminatingOwnerTestUser("team-a", "dying", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
				ownerTestUser("team-a", "successor", newer, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "successor",
		},
		{
			name: "the only owner is terminating",
			users: []databasesv1alpha1.DatabaseUser{
				terminatingOwnerTestUser("team-a", "dying", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "",
		},
		{
			name: "cross-namespace reference resolves against the Database namespace",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "local-owner", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
				ownerTestUser("team-b", "remote-owner", newer, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders", Namespace: "shared"},
					Privileges: "owner",
				}),
			},
			dbNamespace: "shared",
			dbName:      "orders",
			want:        "remote-owner",
		},
		{
			name: "claimant setting both database and databases yields to the next candidate",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "ambiguous", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Databases:  []databasesv1alpha1.DatabaseAccess{{Name: "billing"}},
					Privileges: "owner",
				}),
				ownerTestUser("team-a", "successor", newer, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "successor",
		},
		{
			name: "the only claimant sets both database and databases",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "ambiguous", older, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Databases:  []databasesv1alpha1.DatabaseAccess{{Name: "billing"}},
					Privileges: "owner",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "",
		},
		{
			name: "claimant listing the same database twice yields to the next candidate",
			users: []databasesv1alpha1.DatabaseUser{
				ownerTestUser("team-a", "duplicated", older, &databasesv1alpha1.DatabaseUserSpec{
					Databases: []databasesv1alpha1.DatabaseAccess{
						{Name: "orders"},
						{Name: "orders", Namespace: "team-a"},
					},
					Privileges: "owner",
				}),
				ownerTestUser("team-a", "successor", newer, &databasesv1alpha1.DatabaseUserSpec{
					Database:   &databasesv1alpha1.DatabaseAccess{Name: "orders"},
					Privileges: "owner",
				}),
			},
			dbNamespace: "team-a",
			dbName:      "orders",
			want:        "successor",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ElectOwner(tt.users, tt.dbNamespace, tt.dbName)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("ElectOwner() = %s/%s, want nil", got.Namespace, got.Name)
				}
				return
			}
			if got == nil {
				t.Fatalf("ElectOwner() = nil, want %q", tt.want)
			}
			if got.Name != tt.want {
				t.Errorf("ElectOwner() = %q, want %q", got.Name, tt.want)
			}
		})
	}
}

func TestLoserGrants(t *testing.T) {
	grants := UserGrants{
		Username:         "app_user",
		Privileges:       "owner",
		AdditionalGrants: []postgres.TableGrant{{Tables: []string{"orders"}, Privileges: []postgres.TablePrivilege{"SELECT"}}},
	}

	lowered := LoserGrants(grants)

	if lowered.Privileges != "admin" {
		t.Errorf("Privileges = %q, want admin", lowered.Privileges)
	}
	if grants.Privileges != "owner" {
		t.Errorf("LoserGrants mutated its argument: Privileges = %q", grants.Privileges)
	}
	if lowered.Username != "app_user" || len(lowered.AdditionalGrants) != 1 {
		t.Errorf("everything but the preset must survive, got %+v", lowered)
	}
}

func TestSameUser(t *testing.T) {
	a := &databasesv1alpha1.DatabaseUser{ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "ns"}}
	sameRef := &databasesv1alpha1.DatabaseUser{ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "ns"}}
	otherNS := &databasesv1alpha1.DatabaseUser{ObjectMeta: metav1.ObjectMeta{Name: "u", Namespace: "other"}}
	otherName := &databasesv1alpha1.DatabaseUser{ObjectMeta: metav1.ObjectMeta{Name: "v", Namespace: "ns"}}

	if !SameUser(a, sameRef) {
		t.Error("SameUser(ns/u, ns/u) = false, want true")
	}
	if SameUser(a, otherNS) {
		t.Error("SameUser(ns/u, other/u) = true, want false")
	}
	if SameUser(a, otherName) {
		t.Error("SameUser(ns/u, ns/v) = true, want false")
	}
	if SameUser(nil, a) || SameUser(a, nil) {
		t.Error("SameUser with a nil side = true, want false")
	}
}
