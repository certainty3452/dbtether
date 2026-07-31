package controllers

import (
	"strings"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

// UserGrants is the privilege set a DatabaseUser is supposed to hold on a single
// database. Shared with the Restore controller so a restore re-applies exactly
// what the DatabaseUser controller would apply.
type UserGrants struct {
	Username         string
	Privileges       string
	AdditionalGrants []postgres.TableGrant
}

// ResolveUserGrants resolves the grants for one entry of the user's database list.
func ResolveUserGrants(user *databasesv1alpha1.DatabaseUser, access databasesv1alpha1.DatabaseAccess) UserGrants {
	return UserGrants{
		Username:         UsernameForUser(user),
		Privileges:       privilegesForDatabase(access.Privileges, user.Spec.Privileges),
		AdditionalGrants: translateAdditionalGrants(user.Spec.AdditionalGrants),
	}
}

// ResolveUserGrantsForDatabase resolves the grants the user holds on the Database
// CR identified by namespace/name. ok is false when the user does not reference it.
func ResolveUserGrantsForDatabase(user *databasesv1alpha1.DatabaseUser, namespace, name string) (grants UserGrants, ok bool) {
	for _, access := range user.Spec.GetDatabases() {
		if access.Name == name && databaseAccessNamespace(user, access) == namespace {
			return ResolveUserGrants(user, access), true
		}
	}
	return UserGrants{}, false
}

// UsernameForUser is the PostgreSQL role name backing a DatabaseUser.
func UsernameForUser(user *databasesv1alpha1.DatabaseUser) string {
	if user.Spec.Username != "" {
		return user.Spec.Username
	}
	return strings.ReplaceAll(user.Name, "-", "_")
}

func databaseAccessNamespace(user *databasesv1alpha1.DatabaseUser, access databasesv1alpha1.DatabaseAccess) string {
	if access.Namespace != "" {
		return access.Namespace
	}
	return user.Namespace
}

func translateAdditionalGrants(grants []databasesv1alpha1.TableGrant) []postgres.TableGrant {
	out := make([]postgres.TableGrant, len(grants))
	for j, g := range grants {
		privs := make([]postgres.TablePrivilege, len(g.Privileges))
		for k, p := range g.Privileges {
			privs[k] = postgres.TablePrivilege(p)
		}
		out[j] = postgres.TableGrant{Tables: g.Tables, Privileges: privs}
	}
	return out
}

func privilegesForDatabase(perDB, fallback string) string {
	if perDB != "" {
		return perDB
	}
	if fallback != "" {
		return fallback
	}
	return "readonly"
}
