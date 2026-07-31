package controllers

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

// DatabaseUserDatabaseRefIndex indexes every DatabaseUser by the Databases it
// references (one entry per reference, "namespace/name"). Query it with
// client.MatchingFields and no namespace restriction — a DatabaseUser may live
// in a different namespace than the Database it points at.
const DatabaseUserDatabaseRefIndex = ".spec.databaseRefs"

// RegisterIndexers must run before mgr.Start. The informer cache locks its index
// set when it starts, so IndexField calls made afterwards are rejected.
func RegisterIndexers(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(
		ctx,
		&databasesv1alpha1.DatabaseUser{},
		DatabaseUserDatabaseRefIndex,
		indexDatabaseUserDatabaseRefs,
	); err != nil {
		return fmt.Errorf("failed to index DatabaseUser by %s: %w", DatabaseUserDatabaseRefIndex, err)
	}
	return nil
}

// DatabaseUserDatabaseRefKey builds the DatabaseUserDatabaseRefIndex lookup value
// for a Database CR.
func DatabaseUserDatabaseRefKey(namespace, name string) string {
	return namespace + "/" + name
}

func indexDatabaseUserDatabaseRefs(obj client.Object) []string {
	user, ok := obj.(*databasesv1alpha1.DatabaseUser)
	if !ok {
		return nil
	}

	accesses := user.Spec.GetDatabases()
	keys := make([]string, 0, len(accesses))
	for _, access := range accesses {
		keys = append(keys, DatabaseUserDatabaseRefKey(databaseAccessNamespace(user, access), access.Name))
	}
	return keys
}
