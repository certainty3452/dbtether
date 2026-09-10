package controllers

import (
	"context"
	"fmt"
	"sync"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
)

// DatabaseUserDatabaseRefIndex indexes every DatabaseUser by the Databases it
// references (one entry per reference, "namespace/name"). Query it with
// client.MatchingFields and no namespace restriction — a DatabaseUser may live
// in a different namespace than the Database it points at.
const DatabaseUserDatabaseRefIndex = ".spec.databaseRefs"

var (
	registerIndexersOnce sync.Once
	registerIndexersErr  error
)

// Must run before mgr.Start, and only once: re-registering the same field fails.
func RegisterIndexers(ctx context.Context, mgr ctrl.Manager) error {
	registerIndexersOnce.Do(func() {
		if err := mgr.GetFieldIndexer().IndexField(
			ctx,
			&databasesv1alpha1.DatabaseUser{},
			DatabaseUserDatabaseRefIndex,
			indexDatabaseUserDatabaseRefs,
		); err != nil {
			registerIndexersErr = fmt.Errorf("failed to index DatabaseUser by %s: %w", DatabaseUserDatabaseRefIndex, err)
		}
	})
	return registerIndexersErr
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
