package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

const UserFinalizerName = "databaseusers.dbtether.io/finalizer"

type DatabaseUserReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	PGClientCache postgres.ClientCacheInterface
}

// +kubebuilder:rbac:groups=dbtether.io,resources=databaseusers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=dbtether.io,resources=databaseusers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=dbtether.io,resources=databaseusers/finalizers,verbs=update
// +kubebuilder:rbac:groups=dbtether.io,resources=databases,verbs=get;list;watch
// +kubebuilder:rbac:groups=dbtether.io,resources=dbclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

func (r *DatabaseUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var user databasesv1alpha1.DatabaseUser
	if err := r.Get(ctx, req.NamespacedName, &user); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	username := r.getUsername(&user)

	// Check if secret still exists before early exit
	if user.Status.Phase == "Ready" && user.Status.ObservedGeneration == user.Generation {
		secretName := r.getSecretName(&user)
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: user.Namespace}, &secret); err == nil {
			return ctrl.Result{}, nil
		}
		logger.Info("secret missing, triggering reconciliation", "secret", secretName)
	}
	logger.V(1).Info("reconciling", "username", username)

	if !user.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &user)
	}

	if result, err := r.ensureFinalizer(ctx, &user); result != nil || err != nil {
		return *result, err
	}

	db, cluster, result, err := r.getReadyDatabaseAndCluster(ctx, &user)
	if result != nil || err != nil {
		return *result, err
	}

	if result, err := r.ensureOwnerReference(ctx, &user, db); result != nil || err != nil {
		return *result, err
	}

	return r.reconcileUser(ctx, &user, db, cluster)
}

func (r *DatabaseUserReconciler) getUsername(user *databasesv1alpha1.DatabaseUser) string {
	if user.Spec.Username != "" {
		return user.Spec.Username
	}
	return strings.ReplaceAll(user.Name, "-", "_")
}

func (r *DatabaseUserReconciler) getDatabaseNameFromSpec(db *databasesv1alpha1.Database) string {
	if db.Spec.DatabaseName != "" {
		return db.Spec.DatabaseName
	}
	return strings.ReplaceAll(db.Name, "-", "_")
}

func (r *DatabaseUserReconciler) getSecretName(user *databasesv1alpha1.DatabaseUser) string {
	if user.Spec.Secret != nil && user.Spec.Secret.Name != "" {
		return user.Spec.Secret.Name
	}
	return user.Name + "-credentials"
}

func (r *DatabaseUserReconciler) getSecretKeys(user *databasesv1alpha1.DatabaseUser) (host, port, db, username, password string) {
	host, port, db, username, password = "host", "port", "database", "user", "password"

	if user.Spec.Secret == nil {
		return
	}

	switch user.Spec.Secret.Template {
	case "DB":
		return "DB_HOST", "DB_PORT", "DB_NAME", "DB_USER", "DB_PASSWORD"
	case "DATABASE":
		return "DATABASE_HOST", "DATABASE_PORT", "DATABASE_NAME", "DATABASE_USER", "DATABASE_PASSWORD"
	case "POSTGRES":
		return "POSTGRES_HOST", "POSTGRES_PORT", "POSTGRES_DATABASE", "POSTGRES_USER", "POSTGRES_PASSWORD"
	case "custom":
		host, port, db, username, password = r.applyCustomKeys(user.Spec.Secret.Keys, host, port, db, username, password)
	}
	return
}

func (r *DatabaseUserReconciler) applyCustomKeys(k *databasesv1alpha1.SecretKeys, host, port, db, username, password string) (outHost, outPort, outDB, outUser, outPwd string) {
	outHost, outPort, outDB, outUser, outPwd = host, port, db, username, password
	if k == nil {
		return
	}
	if k.Host != "" {
		outHost = k.Host
	}
	if k.Port != "" {
		outPort = k.Port
	}
	if k.Database != "" {
		outDB = k.Database
	}
	if k.User != "" {
		outUser = k.User
	}
	if k.Password != "" {
		outPwd = k.Password
	}
	return
}

func (r *DatabaseUserReconciler) isSecretOwnedByUser(secret *corev1.Secret, user *databasesv1alpha1.DatabaseUser) bool {
	for _, ref := range secret.OwnerReferences {
		if ref.Kind == "DatabaseUser" && ref.Name == user.Name && ref.UID == user.UID {
			return true
		}
	}
	return false
}

func (r *DatabaseUserReconciler) getOnConflictPolicy(user *databasesv1alpha1.DatabaseUser) string {
	if user.Spec.Secret != nil && user.Spec.Secret.OnConflict != "" {
		return user.Spec.Secret.OnConflict
	}
	return "Fail"
}

func (r *DatabaseUserReconciler) ensureFinalizer(ctx context.Context, user *databasesv1alpha1.DatabaseUser) (*ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(user, UserFinalizerName) {
		return nil, nil
	}
	controllerutil.AddFinalizer(user, UserFinalizerName)
	if err := r.Update(ctx, user); err != nil {
		return &ctrl.Result{}, err
	}
	return &ctrl.Result{}, nil
}

func (r *DatabaseUserReconciler) ensureOwnerReference(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	db *databasesv1alpha1.Database) (*ctrl.Result, error) {

	for _, ref := range user.OwnerReferences {
		if ref.Kind == "Database" && ref.Name == db.Name {
			return nil, nil
		}
	}

	if err := controllerutil.SetOwnerReference(db, user, r.Scheme); err != nil {
		return &ctrl.Result{}, err
	}
	if err := r.Update(ctx, user); err != nil {
		return &ctrl.Result{}, err
	}
	return &ctrl.Result{}, nil
}

func (r *DatabaseUserReconciler) getReadyDatabaseAndCluster(ctx context.Context, user *databasesv1alpha1.DatabaseUser) (
	*databasesv1alpha1.Database, *databasesv1alpha1.DBCluster, *ctrl.Result, error) {

	dbNamespace := user.Spec.DatabaseRef.Namespace
	if dbNamespace == "" {
		dbNamespace = user.Namespace
	}

	var db databasesv1alpha1.Database
	if err := r.Get(ctx, types.NamespacedName{
		Name:      user.Spec.DatabaseRef.Name,
		Namespace: dbNamespace,
	}, &db); err != nil {
		if errors.IsNotFound(err) {
			result, err := r.setStatus(ctx, user, &statusUpdate{
				Phase: "Pending", Message: fmt.Sprintf("waiting for Database '%s'", user.Spec.DatabaseRef.Name), RequeueAfter: 30 * time.Second,
			})
			return nil, nil, &result, err
		}
		return nil, nil, &ctrl.Result{}, err
	}

	if db.Status.Phase != "Ready" {
		result, err := r.setStatus(ctx, user, &statusUpdate{
			Phase: "Pending", Message: fmt.Sprintf("waiting for Database '%s' to be ready", db.Name), RequeueAfter: 20 * time.Second,
		})
		return nil, nil, &result, err
	}

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: db.Spec.ClusterRef.Name}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			result, err := r.setStatus(ctx, user, &statusUpdate{
				Phase: "Pending", Message: fmt.Sprintf("waiting for DBCluster '%s'", db.Spec.ClusterRef.Name), RequeueAfter: 30 * time.Second,
			})
			return nil, nil, &result, err
		}
		return nil, nil, &ctrl.Result{}, err
	}

	if cluster.Status.Phase != "Connected" {
		result, err := r.setStatus(ctx, user, &statusUpdate{
			Phase: "Pending", Message: fmt.Sprintf("waiting for DBCluster '%s' to be connected", cluster.Name), RequeueAfter: 20 * time.Second,
		})
		return nil, nil, &result, err
	}

	return &db, &cluster, nil, nil
}

func (r *DatabaseUserReconciler) reconcileUser(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	db *databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster) (ctrl.Result, error) {

	logger := log.FromContext(ctx)
	username := r.getUsername(user)
	databaseName := r.getDatabaseNameFromSpec(db)

	// Status params for display fields
	baseStatus := statusUpdate{
		ClusterName:  cluster.Name,
		DatabaseName: databaseName,
		Username:     username,
	}

	pgClient, err := r.getPostgresClient(ctx, cluster)
	if err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = fmt.Sprintf("connection error: %s", err.Error())
		baseStatus.RequeueAfter = 60 * time.Second
		return r.setStatus(ctx, user, &baseStatus)
	}

	password, secretName, passwordChanged, err := r.ensureSecret(ctx, user, db, cluster, pgClient)
	if err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = fmt.Sprintf("secret error: %s", err.Error())
		return r.setStatus(ctx, user, &baseStatus)
	}

	if err := r.ensureUserInPostgres(ctx, pgClient, username, password); err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = err.Error()
		baseStatus.SecretName = secretName
		return r.setStatus(ctx, user, &baseStatus)
	}

	if err := r.configureUserAccess(ctx, pgClient, user, db, username); err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = err.Error()
		baseStatus.SecretName = secretName
		return r.setStatus(ctx, user, &baseStatus)
	}

	r.verifyIsolation(ctx, pgClient, username, databaseName)

	logger.Info("user ready", "username", username, "privileges", user.Spec.Privileges)

	baseStatus.Phase = "Ready"
	baseStatus.Message = fmt.Sprintf("user created with %s privileges", user.Spec.Privileges)
	baseStatus.SecretName = secretName
	baseStatus.PasswordUpdated = passwordChanged
	baseStatus.RequeueAfter = r.calculateRequeueAfter(user)
	return r.setStatus(ctx, user, &baseStatus)
}

func (r *DatabaseUserReconciler) shouldRotatePassword(user *databasesv1alpha1.DatabaseUser) bool {
	if user.Spec.Rotation == nil || user.Spec.Rotation.Days == 0 {
		return false
	}
	if user.Status.PasswordUpdatedAt == nil {
		return false
	}
	age := time.Since(user.Status.PasswordUpdatedAt.Time)
	maxAge := time.Duration(user.Spec.Rotation.Days) * 24 * time.Hour
	return age > maxAge
}

func (r *DatabaseUserReconciler) calculateRequeueAfter(user *databasesv1alpha1.DatabaseUser) time.Duration {
	if user.Spec.Rotation == nil || user.Spec.Rotation.Days == 0 {
		return 0
	}
	if user.Status.PasswordUpdatedAt == nil {
		return 0
	}

	maxAge := time.Duration(user.Spec.Rotation.Days) * 24 * time.Hour
	nextRotation := user.Status.PasswordUpdatedAt.Add(maxAge)
	requeue := time.Until(nextRotation)

	if requeue <= 0 {
		return time.Minute // Rotation overdue, check again soon
	}
	return requeue
}

func (r *DatabaseUserReconciler) ensureUserInPostgres(ctx context.Context, pgClient postgres.ClientInterface,
	username, password string) error {

	exists, err := pgClient.UserExists(ctx, username)
	if err != nil {
		return fmt.Errorf("failed to check user: %s", err.Error())
	}

	if exists {
		if err := pgClient.SetPassword(ctx, username, password); err != nil {
			return fmt.Errorf("failed to set password: %s", err.Error())
		}
		return nil
	}

	if err := pgClient.CreateUser(ctx, username, password); err != nil {
		return fmt.Errorf("failed to create user: %s", err.Error())
	}
	return nil
}

func (r *DatabaseUserReconciler) configureUserAccess(ctx context.Context, pgClient postgres.ClientInterface,
	user *databasesv1alpha1.DatabaseUser, db *databasesv1alpha1.Database, username string) error {

	logger := log.FromContext(ctx)

	if err := pgClient.RevokeAllDatabaseAccess(ctx, username); err != nil {
		logger.Error(err, "failed to revoke postgres access")
	}

	if err := pgClient.GrantDatabaseAccess(ctx, username, r.getDatabaseNameFromSpec(db)); err != nil {
		return fmt.Errorf("failed to grant database access: %s", err.Error())
	}

	additionalGrants := make([]postgres.TableGrant, len(user.Spec.AdditionalGrants))
	for i, g := range user.Spec.AdditionalGrants {
		additionalGrants[i] = postgres.TableGrant{
			Tables:     g.Tables,
			Privileges: g.Privileges,
		}
	}

	if err := pgClient.ApplyPrivileges(ctx, username, r.getDatabaseNameFromSpec(db), user.Spec.Privileges, additionalGrants); err != nil {
		return fmt.Errorf("failed to apply privileges: %s", err.Error())
	}

	if user.Spec.ConnectionLimit != 0 {
		if err := pgClient.SetConnectionLimit(ctx, username, user.Spec.ConnectionLimit); err != nil {
			return fmt.Errorf("failed to set connection limit: %s", err.Error())
		}
	}

	return nil
}

func (r *DatabaseUserReconciler) verifyIsolation(ctx context.Context, pgClient postgres.ClientInterface,
	username, expectedDatabase string) {

	logger := log.FromContext(ctx)
	databases, err := pgClient.VerifyDatabaseIsolation(ctx, username, expectedDatabase)
	if err != nil {
		logger.V(1).Info("failed to verify database isolation", "error", err.Error())
		return
	}

	hasUnexpectedAccess := len(databases) > 1 || (len(databases) == 1 && databases[0] != expectedDatabase)
	if hasUnexpectedAccess {
		// Debug level - in shared clusters this is expected due to PUBLIC role
		logger.V(1).Info("user has access to additional databases (inherited from PUBLIC role)",
			"expected", expectedDatabase, "accessibleDatabases", len(databases))
	}
}

func (r *DatabaseUserReconciler) ensureSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	db *databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster,
	pgClient postgres.ClientInterface) (password, secretName string, passwordChanged bool, err error) {

	logger := log.FromContext(ctx)
	secretName = r.getSecretName(user)
	username := r.getUsername(user)

	var secret corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: user.Namespace}, &secret)

	// Secret exists
	if err == nil {
		if r.isSecretOwnedByUser(&secret, user) {
			if r.shouldRotatePassword(user) {
				return r.rotatePassword(ctx, user, &secret, cluster, db, pgClient, username)
			}
			_, _, _, _, pwdKey := r.getSecretKeys(user)
			return string(secret.Data[pwdKey]), secretName, false, nil
		}

		// Not our secret - handle based on onConflict policy
		policy := r.getOnConflictPolicy(user)
		switch policy {
		case "Adopt":
			return r.adoptSecret(ctx, user, &secret, cluster, db, pgClient, username)
		case "Merge":
			return r.mergeSecret(ctx, user, &secret, cluster, db, pgClient, username)
		default:
			return "", "", false, fmt.Errorf("secret %s already exists and is not owned by this DatabaseUser", secretName)
		}
	}

	if !errors.IsNotFound(err) {
		return "", "", false, err
	}

	// Secret is missing - check if this is a regeneration case (user was Ready but secret deleted)
	isRegeneration := user.Status.Phase == "Ready"

	length := user.Spec.Password.Length
	if length == 0 {
		length = postgres.DefaultPasswordLength
	}
	password, err = postgres.GeneratePassword(length)
	if err != nil {
		return "", "", false, fmt.Errorf("failed to generate password: %w", err)
	}

	// If regenerating, update password in PostgreSQL first
	if isRegeneration {
		logger.Info("regenerating password (secret was deleted)", "username", username)
		if err := pgClient.SetPassword(ctx, username, password); err != nil {
			return "", "", false, fmt.Errorf("failed to update password in PostgreSQL: %w", err)
		}
	}

	hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(user)
	secret = corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: user.Namespace,
			Annotations: map[string]string{
				"dbtether.io/managed-by": user.Name,
			},
		},
		StringData: map[string]string{
			hostKey: cluster.Spec.Endpoint,
			portKey: fmt.Sprintf("%d", cluster.Spec.Port),
			dbKey:   r.getDatabaseNameFromSpec(db),
			userKey: username,
			pwdKey:  password,
		},
	}

	if err = controllerutil.SetControllerReference(user, &secret, r.Scheme); err != nil {
		return "", "", false, fmt.Errorf("failed to set controller reference: %w", err)
	}

	if err = r.Create(ctx, &secret); err != nil {
		return "", "", false, fmt.Errorf("failed to create secret: %w", err)
	}

	return password, secretName, true, nil
}

func (r *DatabaseUserReconciler) rotatePassword(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, db *databasesv1alpha1.Database,
	pgClient postgres.ClientInterface, username string) (password, secretName string, passwordChanged bool, err error) {

	logger := log.FromContext(ctx)
	secretName = secret.Name

	logger.Info("rotating password", "username", username, "days", user.Spec.Rotation.Days)

	length := user.Spec.Password.Length
	if length == 0 {
		length = postgres.DefaultPasswordLength
	}
	password, err = postgres.GeneratePassword(length)
	if err != nil {
		return "", "", false, fmt.Errorf("failed to generate password: %w", err)
	}

	// Update PostgreSQL first
	if err := pgClient.SetPassword(ctx, username, password); err != nil {
		return "", "", false, fmt.Errorf("failed to update password in PostgreSQL: %w", err)
	}

	// Update secret in-place
	hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(user)
	secret.Data[pwdKey] = []byte(password)
	secret.Data[hostKey] = []byte(cluster.Spec.Endpoint)
	secret.Data[portKey] = []byte(fmt.Sprintf("%d", cluster.Spec.Port))
	secret.Data[dbKey] = []byte(r.getDatabaseNameFromSpec(db))
	secret.Data[userKey] = []byte(username)

	if err := r.Update(ctx, secret); err != nil {
		return "", "", false, fmt.Errorf("failed to update secret: %w", err)
	}

	logger.Info("password rotated successfully", "username", username)
	return password, secretName, true, nil
}

func (r *DatabaseUserReconciler) adoptSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, db *databasesv1alpha1.Database,
	pgClient postgres.ClientInterface, username string) (password, secretName string, passwordChanged bool, err error) {

	logger := log.FromContext(ctx)
	logger.Info("adopting existing secret", "secret", secret.Name)

	length := user.Spec.Password.Length
	if length == 0 {
		length = postgres.DefaultPasswordLength
	}
	password, err = postgres.GeneratePassword(length)
	if err != nil {
		return "", "", false, fmt.Errorf("failed to generate password: %w", err)
	}

	if err = pgClient.SetPassword(ctx, username, password); err != nil {
		return "", "", false, fmt.Errorf("failed to set password during adopt: %w", err)
	}

	if err = controllerutil.SetControllerReference(user, secret, r.Scheme); err != nil {
		return "", "", false, fmt.Errorf("failed to set controller reference: %w", err)
	}

	hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(user)
	secret.Data = map[string][]byte{
		hostKey: []byte(cluster.Spec.Endpoint),
		portKey: []byte(fmt.Sprintf("%d", cluster.Spec.Port)),
		dbKey:   []byte(r.getDatabaseNameFromSpec(db)),
		userKey: []byte(username),
		pwdKey:  []byte(password),
	}

	if err = r.Update(ctx, secret); err != nil {
		return "", "", false, fmt.Errorf("failed to update secret during adopt: %w", err)
	}

	return password, secret.Name, true, nil
}

func (r *DatabaseUserReconciler) mergeSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, db *databasesv1alpha1.Database,
	pgClient postgres.ClientInterface, username string) (password, secretName string, passwordChanged bool, err error) {

	logger := log.FromContext(ctx)
	logger.Info("merging into existing secret", "secret", secret.Name)

	length := user.Spec.Password.Length
	if length == 0 {
		length = postgres.DefaultPasswordLength
	}
	password, err = postgres.GeneratePassword(length)
	if err != nil {
		return "", "", false, fmt.Errorf("failed to generate password: %w", err)
	}

	if err = pgClient.SetPassword(ctx, username, password); err != nil {
		return "", "", false, fmt.Errorf("failed to set password during merge: %w", err)
	}

	if err = controllerutil.SetControllerReference(user, secret, r.Scheme); err != nil {
		return "", "", false, fmt.Errorf("failed to set controller reference: %w", err)
	}

	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(user)
	secret.Data[hostKey] = []byte(cluster.Spec.Endpoint)
	secret.Data[portKey] = []byte(fmt.Sprintf("%d", cluster.Spec.Port))
	secret.Data[dbKey] = []byte(r.getDatabaseNameFromSpec(db))
	secret.Data[userKey] = []byte(username)
	secret.Data[pwdKey] = []byte(password)

	if err = r.Update(ctx, secret); err != nil {
		return "", "", false, fmt.Errorf("failed to update secret during merge: %w", err)
	}

	return password, secret.Name, true, nil
}

func (r *DatabaseUserReconciler) handleDeletion(ctx context.Context, user *databasesv1alpha1.DatabaseUser) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(user, UserFinalizerName) {
		return ctrl.Result{}, nil
	}

	logger := log.FromContext(ctx)
	username := r.getUsername(user)
	logger.Info("handling deletion", "username", username)

	if user.Spec.DeletionPolicy != "Retain" {
		r.dropUserFromPostgres(ctx, user, username)
	} else {
		logger.Info("retaining user in PostgreSQL due to deletionPolicy", "username", username)
	}

	controllerutil.RemoveFinalizer(user, UserFinalizerName)
	return ctrl.Result{}, r.Update(ctx, user)
}

func (r *DatabaseUserReconciler) dropUserFromPostgres(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	username string) {

	logger := log.FromContext(ctx)

	clusterName, databaseName := r.getClusterAndDatabaseForDeletion(ctx, user)
	if clusterName == "" {
		logger.Error(nil, "cannot determine cluster for cleanup - user will remain in PostgreSQL")
		return
	}

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: clusterName}, &cluster); err != nil {
		logger.Error(err, "failed to get cluster for cleanup")
		return
	}

	pgClient, err := r.getPostgresClient(ctx, &cluster)
	if err != nil {
		logger.Error(err, "failed to get postgres client for cleanup")
		return
	}

	if databaseName != "" {
		if err := pgClient.RevokePrivilegesInDatabase(ctx, username, databaseName); err != nil {
			logger.Error(err, "failed to revoke privileges")
		}
	}

	if err := pgClient.DropUser(ctx, username); err != nil {
		logger.Error(err, "failed to drop user")
	} else {
		logger.Info("user dropped", "username", username)
	}
}

func (r *DatabaseUserReconciler) getClusterAndDatabaseForDeletion(ctx context.Context, user *databasesv1alpha1.DatabaseUser) (clusterName, databaseName string) {
	// First try to get from status (works even if Database is already deleted)
	if user.Status.ClusterName != "" {
		return user.Status.ClusterName, user.Status.DatabaseName
	}

	// Fall back to fetching Database if status not populated
	dbNamespace := user.Spec.DatabaseRef.Namespace
	if dbNamespace == "" {
		dbNamespace = user.Namespace
	}

	var db databasesv1alpha1.Database
	if err := r.Get(ctx, types.NamespacedName{
		Name:      user.Spec.DatabaseRef.Name,
		Namespace: dbNamespace,
	}, &db); err != nil {
		return "", ""
	}

	return db.Spec.ClusterRef.Name, r.getDatabaseNameFromSpec(&db)
}

func (r *DatabaseUserReconciler) getPostgresClient(ctx context.Context, cluster *databasesv1alpha1.DBCluster) (postgres.ClientInterface, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{
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

	return r.PGClientCache.Get(ctx, cluster.Name, postgres.Config{
		Host:     cluster.Spec.Endpoint,
		Port:     cluster.Spec.Port,
		Username: username,
		Password: password,
		Database: "postgres",
	})
}

// statusUpdate contains all parameters for updating DatabaseUser status
type statusUpdate struct {
	Phase           string
	Message         string
	SecretName      string
	PasswordUpdated bool
	RequeueAfter    time.Duration
	ClusterName     string
	DatabaseName    string
	Username        string
}

func (r *DatabaseUserReconciler) setStatus(ctx context.Context, user *databasesv1alpha1.DatabaseUser, update *statusUpdate) (ctrl.Result, error) {
	patch := client.MergeFrom(user.DeepCopy())

	// Handle pending timeout
	r.handlePendingTimeout(user, update)

	user.Status.Phase = update.Phase
	user.Status.Message = update.Message
	user.Status.ObservedGeneration = user.Generation

	r.applyStatusFields(user, update)

	if err := r.Status().Patch(ctx, user, patch); err != nil {
		return ctrl.Result{}, err
	}
	if update.RequeueAfter > 0 {
		return ctrl.Result{RequeueAfter: update.RequeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *DatabaseUserReconciler) handlePendingTimeout(user *databasesv1alpha1.DatabaseUser, update *statusUpdate) {
	if update.Phase == "Pending" {
		now := metav1.Now()
		if user.Status.PendingSince == nil {
			user.Status.PendingSince = &now
		} else if now.Sub(user.Status.PendingSince.Time) > PendingTimeout {
			update.Phase = "Failed"
			update.Message = fmt.Sprintf("timeout: %s (pending for over 10 minutes)", update.Message)
		}
	} else {
		user.Status.PendingSince = nil
	}
}

func (r *DatabaseUserReconciler) applyStatusFields(user *databasesv1alpha1.DatabaseUser, update *statusUpdate) {
	if update.ClusterName != "" {
		user.Status.ClusterName = update.ClusterName
	}
	if update.DatabaseName != "" {
		user.Status.DatabaseName = update.DatabaseName
	}
	if update.Username != "" {
		user.Status.Username = update.Username
	}
	if update.SecretName != "" {
		user.Status.SecretName = update.SecretName
	}
	if update.PasswordUpdated || (user.Status.PasswordUpdatedAt == nil && update.Phase == "Ready") {
		now := metav1.Now()
		user.Status.PasswordUpdatedAt = &now
	}
}

func (r *DatabaseUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.DatabaseUser{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
