package controllers

import (
	"fmt"
	"path"

	"k8s.io/apimachinery/pkg/types"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

func CheckReferenceConsent(db *databasesv1alpha1.Database, fromNamespace string) error {
	if fromNamespace == db.Namespace || namespaceListed(db.Spec.AllowedNamespaces, fromNamespace) {
		return nil
	}
	return fmt.Errorf("database %s/%s does not list namespace %q in spec.allowedNamespaces", db.Namespace, db.Name, fromNamespace)
}

// path.Match is exact here: the schema admits only [a-z0-9*-], so "?", "[", backslash and "/" never reach it.
func namespaceListed(patterns []string, namespace string) bool {
	for _, pattern := range patterns {
		if ok, _ := path.Match(pattern, namespace); ok {
			return true
		}
	}
	return false
}

func DatabaseRefKey(ref databasesv1alpha1.DatabaseReference, defaultNamespace string) types.NamespacedName {
	if ref.Namespace != "" {
		return types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}
	}
	return types.NamespacedName{Namespace: defaultNamespace, Name: ref.Name}
}

// A user whose namespace the Database does not allow holds nothing on it, whatever its spec says.
func GrantsOnDatabase(user *databasesv1alpha1.DatabaseUser, db *databasesv1alpha1.Database) (UserGrants, bool) {
	if CheckReferenceConsent(db, user.Namespace) != nil {
		return UserGrants{}, false
	}
	return ResolveUserGrantsForDatabase(user, db.Namespace, db.Name)
}
