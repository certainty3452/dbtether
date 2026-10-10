package controllers

import (
	"context"
	"fmt"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

// One entry point so every controller keys the client cache the same way.
func GetPostgresClient(
	ctx context.Context,
	c client.Reader,
	cache postgres.ClientCacheInterface,
	cluster *databasesv1alpha1.DBCluster,
) (postgres.ClientInterface, error) {
	config, err := ClusterConfig(ctx, c, cluster)
	if err != nil {
		return nil, err
	}
	return cache.Get(ctx, cluster.Name, config)
}

func ClusterConfig(ctx context.Context, c client.Reader, cluster *databasesv1alpha1.DBCluster) (postgres.Config, error) {
	username, password, err := clusterCredentials(ctx, c, cluster)
	if err != nil {
		return postgres.Config{}, err
	}
	return postgres.Config{
		Host:     cluster.Spec.Endpoint,
		Port:     cluster.Spec.Port,
		Username: username,
		Password: password,
		Database: "postgres",
		SSLMode:  OperatorSSLMode(),
	}, nil
}

func clusterCredentials(ctx context.Context, c client.Reader, cluster *databasesv1alpha1.DBCluster) (username, password string, err error) {
	switch {
	case cluster.Spec.CredentialsFromEnv != nil:
		return credentialsFromEnv(cluster.Spec.CredentialsFromEnv)
	case cluster.Spec.CredentialsSecretRef != nil:
		return credentialsFromSecret(ctx, c, cluster.Spec.CredentialsSecretRef)
	default:
		return "", "", fmt.Errorf("either credentialsSecretRef or credentialsFromEnv must be specified")
	}
}

func credentialsFromEnv(cfg *databasesv1alpha1.CredentialsFromEnv) (username, password string, err error) {
	username = os.Getenv(cfg.Username)
	if username == "" {
		return "", "", fmt.Errorf("environment variable %s not set or empty", cfg.Username)
	}
	password = os.Getenv(cfg.Password)
	if password == "" {
		return "", "", fmt.Errorf("environment variable %s not set or empty", cfg.Password)
	}
	return username, password, nil
}

func credentialsFromSecret(ctx context.Context, c client.Reader, ref *databasesv1alpha1.SecretReference) (username, password string, err error) {
	var secret corev1.Secret
	if err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, &secret); err != nil {
		return "", "", fmt.Errorf("failed to get credentials secret: %w", err)
	}
	username = string(secret.Data["username"])
	password = string(secret.Data["password"])
	if username == "" || password == "" {
		return "", "", fmt.Errorf("secret must contain 'username' and 'password' keys")
	}
	return username, password, nil
}

// The one reader of DB_SSLMODE: main.go configures the backup and restore Jobs from here too.
func OperatorSSLMode() string {
	if mode := os.Getenv("DB_SSLMODE"); mode != "" {
		return mode
	}
	return "require"
}
