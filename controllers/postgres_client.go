package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

// GetPostgresClient resolves a DBCluster's admin credentials and returns a cached
// client connected to its "postgres" database. Shared by every controller that
// talks to a cluster so they all key the cache the same way.
func GetPostgresClient(
	ctx context.Context,
	c client.Client,
	cache postgres.ClientCacheInterface,
	cluster *databasesv1alpha1.DBCluster,
) (postgres.ClientInterface, error) {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{
		Name:      cluster.Spec.CredentialsSecretRef.Name,
		Namespace: cluster.Spec.CredentialsSecretRef.Namespace,
	}, &secret); err != nil {
		return nil, fmt.Errorf("failed to get credentials secret: %w", err)
	}

	username := string(secret.Data["username"])
	password := string(secret.Data["password"])
	if username == "" || password == "" {
		return nil, fmt.Errorf("credentials secret must contain 'username' and 'password' keys")
	}

	return cache.Get(ctx, cluster.Name, postgres.Config{
		Host:     cluster.Spec.Endpoint,
		Port:     cluster.Spec.Port,
		Username: username,
		Password: password,
		Database: "postgres",
	})
}
