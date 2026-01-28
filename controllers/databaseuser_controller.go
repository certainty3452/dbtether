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

	// Validate spec
	if err := r.validateSpec(&user); err != nil {
		return r.setStatus(ctx, &user, &statusUpdate{
			Phase:   "Failed",
			Message: fmt.Sprintf("validation error: %s", err.Error()),
		})
	}

	// Fetch all databases and validate they are on the same cluster
	databases, cluster, result, err := r.validateAndFetchDatabases(ctx, &user)
	if result != nil || err != nil {
		return *result, err
	}

	return r.reconcileUser(ctx, &user, databases, cluster)
}

// validateSpec ensures the user spec is valid
func (r *DatabaseUserReconciler) validateSpec(user *databasesv1alpha1.DatabaseUser) error {
	if user.Spec.Database != nil && len(user.Spec.Databases) > 0 {
		return fmt.Errorf("cannot specify both 'database' and 'databases' - use one or the other")
	}
	if !user.Spec.HasDatabases() {
		return fmt.Errorf("must specify either 'database' or 'databases'")
	}
	return nil
}

// validateAndFetchDatabases fetches all databases and validates they are on the same cluster
func (r *DatabaseUserReconciler) validateAndFetchDatabases(ctx context.Context, user *databasesv1alpha1.DatabaseUser) (
	[]*databasesv1alpha1.Database, *databasesv1alpha1.DBCluster, *ctrl.Result, error) {

	dbAccesses := user.Spec.GetDatabases()
	databases := make([]*databasesv1alpha1.Database, 0, len(dbAccesses))
	var clusterName string

	for _, dbAccess := range dbAccesses {
		dbNamespace := dbAccess.Namespace
		if dbNamespace == "" {
			dbNamespace = user.Namespace
		}

		var db databasesv1alpha1.Database
		if err := r.Get(ctx, types.NamespacedName{
			Name:      dbAccess.Name,
			Namespace: dbNamespace,
		}, &db); err != nil {
			if errors.IsNotFound(err) {
				result, err := r.setStatus(ctx, user, &statusUpdate{
					Phase: "Pending", Message: fmt.Sprintf("waiting for Database '%s'", dbAccess.Name), RequeueAfter: 30 * time.Second,
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

		// Validate all databases are on the same cluster
		if clusterName == "" {
			clusterName = db.Spec.ClusterRef.Name
		} else if db.Spec.ClusterRef.Name != clusterName {
			result, err := r.setStatus(ctx, user, &statusUpdate{
				Phase:   "Failed",
				Message: fmt.Sprintf("all databases must be on the same cluster: '%s' is on '%s', but '%s' is on '%s'", databases[0].Name, clusterName, db.Name, db.Spec.ClusterRef.Name),
			})
			return nil, nil, &result, err
		}

		databases = append(databases, &db)
	}

	// Fetch the cluster
	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: clusterName}, &cluster); err != nil {
		if errors.IsNotFound(err) {
			result, err := r.setStatus(ctx, user, &statusUpdate{
				Phase: "Pending", Message: fmt.Sprintf("waiting for DBCluster '%s'", clusterName), RequeueAfter: 30 * time.Second,
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

	return databases, &cluster, nil, nil
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

func (r *DatabaseUserReconciler) getSecretNameForDatabase(user *databasesv1alpha1.DatabaseUser, dbName string) string {
	return user.Name + "-" + dbName + "-credentials"
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

// shouldIncludeDatabasesList returns true if the secret should include a "databases" field
// Only for: secretGeneration=primary + template=raw (or empty) + more than 1 database
func (r *DatabaseUserReconciler) shouldIncludeDatabasesList(user *databasesv1alpha1.DatabaseUser, dbCount int) bool {
	// perDatabase mode - no databases list needed
	if user.Spec.SecretGeneration == "perDatabase" {
		return false
	}
	// Only for multiple databases
	if dbCount <= 1 {
		return false
	}
	// Only for raw template (or no template specified)
	template := ""
	if user.Spec.Secret != nil {
		template = user.Spec.Secret.Template
	}
	return template == "" || template == "raw"
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

//nolint:gocyclo,funlen // reconciler orchestration requires multiple steps
func (r *DatabaseUserReconciler) reconcileUser(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	databases []*databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster) (ctrl.Result, error) {

	logger := log.FromContext(ctx)
	username := r.getUsername(user)

	// Build database names list
	dbNames := make([]string, len(databases))
	for i, db := range databases {
		dbNames[i] = r.getDatabaseNameFromSpec(db)
	}

	// Base status
	baseStatus := statusUpdate{
		ClusterName: cluster.Name,
		Username:    username,
	}

	pgClient, err := r.getPostgresClient(ctx, cluster)
	if err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = fmt.Sprintf("connection error: %s", err.Error())
		baseStatus.RequeueAfter = 60 * time.Second
		return r.setStatus(ctx, user, &baseStatus)
	}

	// Ensure secrets and get password
	password, secretName, passwordChanged, err := r.ensureSecrets(ctx, user, databases, cluster, pgClient)
	if err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = fmt.Sprintf("secret error: %s", err.Error())
		return r.setStatus(ctx, user, &baseStatus)
	}

	if user.Status.SecretName != "" && user.Status.SecretName != secretName {
		r.deleteOldSecret(ctx, user.Namespace, user.Status.SecretName, user)
	}

	if err := r.ensureUserInPostgres(ctx, pgClient, username, password); err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = err.Error()
		baseStatus.SecretName = secretName
		return r.setStatus(ctx, user, &baseStatus)
	}

	// Sync database access (grant to allowed, revoke from others)
	if err := pgClient.SyncDatabaseAccess(ctx, username, dbNames); err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = fmt.Sprintf("failed to sync database access: %s", err.Error())
		baseStatus.SecretName = secretName
		return r.setStatus(ctx, user, &baseStatus)
	}

	// Apply privileges per database
	dbStatuses := make([]databasesv1alpha1.DatabaseAccessStatus, len(databases))
	dbAccesses := user.Spec.GetDatabases()

	for i, db := range databases {
		dbName := r.getDatabaseNameFromSpec(db)
		privileges := dbAccesses[i].Privileges
		if privileges == "" {
			privileges = user.Spec.Privileges
		}
		if privileges == "" {
			privileges = "readonly"
		}

		additionalGrants := make([]postgres.TableGrant, len(user.Spec.AdditionalGrants))
		for j, g := range user.Spec.AdditionalGrants {
			additionalGrants[j] = postgres.TableGrant{
				Tables:     g.Tables,
				Privileges: g.Privileges,
			}
		}

		if err := pgClient.ApplyPrivileges(ctx, username, dbName, privileges, additionalGrants); err != nil {
			dbStatuses[i] = databasesv1alpha1.DatabaseAccessStatus{
				Name:         dbAccesses[i].Name,
				Namespace:    dbAccesses[i].Namespace,
				DatabaseName: dbName,
				Phase:        "Failed",
				Privileges:   privileges,
				Message:      err.Error(),
			}
		} else {
			dbStatuses[i] = databasesv1alpha1.DatabaseAccessStatus{
				Name:         dbAccesses[i].Name,
				Namespace:    dbAccesses[i].Namespace,
				DatabaseName: dbName,
				Phase:        "Ready",
				Privileges:   privileges,
			}
		}

		// Set secret name for perDatabase mode
		if user.Spec.SecretGeneration == "perDatabase" {
			dbStatuses[i].SecretName = r.getSecretNameForDatabase(user, db.Name)
		}
	}

	// Set connection limit
	if user.Spec.ConnectionLimit != 0 {
		if err := pgClient.SetConnectionLimit(ctx, username, user.Spec.ConnectionLimit); err != nil {
			logger.Error(err, "failed to set connection limit")
		}
	}

	// Verify isolation
	r.verifyIsolation(ctx, pgClient, username, dbNames)

	logger.Info("user ready", "username", username, "databases", len(databases))

	baseStatus.Phase = "Ready"
	baseStatus.Message = fmt.Sprintf("user created with access to %d database(s)", len(databases))
	baseStatus.SecretName = secretName
	baseStatus.PasswordUpdated = passwordChanged
	baseStatus.Databases = dbStatuses
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

func (r *DatabaseUserReconciler) verifyIsolation(ctx context.Context, pgClient postgres.ClientInterface,
	username string, expectedDatabases []string) {

	logger := log.FromContext(ctx)
	accessibleDatabases, err := pgClient.GetUserDatabaseAccess(ctx, username)
	if err != nil {
		logger.V(1).Info("failed to verify database isolation", "error", err.Error())
		return
	}

	// Build expected set
	expected := make(map[string]bool, len(expectedDatabases))
	for _, db := range expectedDatabases {
		expected[db] = true
	}

	// Check for unexpected access
	for _, db := range accessibleDatabases {
		if !expected[db] {
			logger.V(1).Info("user has access to unexpected database (may be inherited from PUBLIC role)",
				"database", db, "expected", expectedDatabases)
		}
	}
}

//nolint:gocyclo // secret management with multiple strategies requires complexity
func (r *DatabaseUserReconciler) ensureSecrets(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	databases []*databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster,
	pgClient postgres.ClientInterface) (password, primarySecretName string, passwordChanged bool, err error) {

	logger := log.FromContext(ctx)
	primarySecretName = r.getSecretName(user)
	username := r.getUsername(user)

	// Check if primary secret exists
	var primarySecret corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Name: primarySecretName, Namespace: user.Namespace}, &primarySecret)

	if err == nil { //nolint:nestif // secret handling requires multiple checks
		// Secret exists
		if r.isSecretOwnedByUser(&primarySecret, user) {
			if r.shouldRotatePassword(user) {
				return r.rotatePassword(ctx, user, &primarySecret, cluster, databases, pgClient, username)
			}
			_, _, _, _, pwdKey := r.getSecretKeys(user)
			password = string(primarySecret.Data[pwdKey])
			// Update secret with current databases list
			if err := r.updateSecretDatabases(ctx, user, &primarySecret, cluster, databases, username); err != nil {
				logger.Error(err, "failed to update secret databases list")
			}
			return password, primarySecretName, false, nil
		}

		// Not our secret - handle based on onConflict policy
		policy := r.getOnConflictPolicy(user)
		switch policy {
		case "Adopt":
			return r.adoptSecret(ctx, user, &primarySecret, cluster, databases, pgClient, username)
		case "Merge":
			return r.mergeSecret(ctx, user, &primarySecret, cluster, databases, pgClient, username)
		default:
			return "", "", false, fmt.Errorf("secret %s already exists and is not owned by this DatabaseUser", primarySecretName)
		}
	}

	if !errors.IsNotFound(err) {
		return "", "", false, err
	}

	// Secret is missing - generate new password
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

	// Create secrets based on secretGeneration strategy
	if user.Spec.SecretGeneration == "perDatabase" {
		// Create separate secret for each database
		for _, db := range databases {
			secretName := r.getSecretNameForDatabase(user, db.Name)
			if err := r.createDatabaseSecret(ctx, user, secretName, cluster, db, username, password); err != nil {
				return "", "", false, fmt.Errorf("failed to create secret for database %s: %w", db.Name, err)
			}
		}
		// Return the first database's secret name as primary
		if len(databases) > 0 {
			primarySecretName = r.getSecretNameForDatabase(user, databases[0].Name)
		}
	} else {
		// Create single primary secret with all databases
		if err := r.createPrimarySecret(ctx, user, primarySecretName, cluster, databases, username, password); err != nil {
			return "", "", false, err
		}
	}

	return password, primarySecretName, true, nil
}

func (r *DatabaseUserReconciler) createPrimarySecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secretName string, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	username, password string) error {

	hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(user)

	// Primary database is the first one
	primaryDB := ""
	if len(databases) > 0 {
		primaryDB = r.getDatabaseNameFromSpec(databases[0])
	}

	secretData := map[string]string{
		hostKey: cluster.Spec.Endpoint,
		portKey: fmt.Sprintf("%d", cluster.Spec.Port),
		dbKey:   primaryDB,
		userKey: username,
		pwdKey:  password,
	}

	// Add databases list only for raw template with multiple databases
	if r.shouldIncludeDatabasesList(user, len(databases)) {
		dbNames := make([]string, len(databases))
		for i, db := range databases {
			dbNames[i] = r.getDatabaseNameFromSpec(db)
		}
		secretData["databases"] = strings.Join(dbNames, ",")
	}

	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: user.Namespace,
			Annotations: map[string]string{
				"dbtether.io/managed-by": user.Name,
			},
		},
		StringData: secretData,
	}

	if err := controllerutil.SetControllerReference(user, &secret, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	if err := r.Create(ctx, &secret); err != nil {
		return fmt.Errorf("failed to create secret: %w", err)
	}

	return nil
}

func (r *DatabaseUserReconciler) createDatabaseSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secretName string, cluster *databasesv1alpha1.DBCluster, db *databasesv1alpha1.Database,
	username, password string) error {

	hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(user)

	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: user.Namespace,
			Annotations: map[string]string{
				"dbtether.io/managed-by": user.Name,
				"dbtether.io/database":   db.Name,
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

	if err := controllerutil.SetControllerReference(user, &secret, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}

	// Check if secret already exists
	var existing corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: user.Namespace}, &existing); err == nil {
		// Update existing
		existing.Data = nil
		existing.StringData = secret.StringData
		existing.Annotations = secret.Annotations
		return r.Update(ctx, &existing)
	} else if !errors.IsNotFound(err) {
		return err
	}

	return r.Create(ctx, &secret)
}

func (r *DatabaseUserReconciler) updateSecretDatabases(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	username string) error {

	hostKey, portKey, dbKey, userKey, _ := r.getSecretKeys(user)

	// Primary database is the first one
	primaryDB := ""
	if len(databases) > 0 {
		primaryDB = r.getDatabaseNameFromSpec(databases[0])
	}

	// Build expected databases list for comparison
	var expectedDatabasesList string
	if r.shouldIncludeDatabasesList(user, len(databases)) {
		dbNames := make([]string, len(databases))
		for i, db := range databases {
			dbNames[i] = r.getDatabaseNameFromSpec(db)
		}
		expectedDatabasesList = strings.Join(dbNames, ",")
	}

	// Check if update is needed
	currentDBs := string(secret.Data["databases"])
	currentPrimaryDB := string(secret.Data[dbKey])
	if currentDBs == expectedDatabasesList && currentPrimaryDB == primaryDB {
		return nil
	}

	secret.Data[hostKey] = []byte(cluster.Spec.Endpoint)
	secret.Data[portKey] = []byte(fmt.Sprintf("%d", cluster.Spec.Port))
	secret.Data[dbKey] = []byte(primaryDB)
	secret.Data[userKey] = []byte(username)

	// Update or remove databases field
	if expectedDatabasesList != "" {
		secret.Data["databases"] = []byte(expectedDatabasesList)
	} else {
		delete(secret.Data, "databases")
	}

	return r.Update(ctx, secret)
}

func (r *DatabaseUserReconciler) rotatePassword(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
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

	// Update secrets based on generation strategy
	if user.Spec.SecretGeneration == "perDatabase" { //nolint:nestif // per-database secret updates require nested logic
		for _, db := range databases {
			dbSecretName := r.getSecretNameForDatabase(user, db.Name)
			if err := r.createDatabaseSecret(ctx, user, dbSecretName, cluster, db, username, password); err != nil {
				logger.Error(err, "failed to update database secret during rotation", "secret", dbSecretName)
			}
		}
		if len(databases) > 0 {
			secretName = r.getSecretNameForDatabase(user, databases[0].Name)
		}
	} else {
		// Update primary secret
		hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(user)

		primaryDB := ""
		if len(databases) > 0 {
			primaryDB = r.getDatabaseNameFromSpec(databases[0])
		}

		secret.Data[pwdKey] = []byte(password)
		secret.Data[hostKey] = []byte(cluster.Spec.Endpoint)
		secret.Data[portKey] = []byte(fmt.Sprintf("%d", cluster.Spec.Port))
		secret.Data[dbKey] = []byte(primaryDB)
		secret.Data[userKey] = []byte(username)

		// Update or remove databases field
		if r.shouldIncludeDatabasesList(user, len(databases)) {
			dbNames := make([]string, len(databases))
			for i, db := range databases {
				dbNames[i] = r.getDatabaseNameFromSpec(db)
			}
			secret.Data["databases"] = []byte(strings.Join(dbNames, ","))
		} else {
			delete(secret.Data, "databases")
		}

		if err := r.Update(ctx, secret); err != nil {
			return "", "", false, fmt.Errorf("failed to update secret: %w", err)
		}
	}

	logger.Info("password rotated successfully", "username", username)
	return password, secretName, true, nil
}

func (r *DatabaseUserReconciler) adoptSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
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

	primaryDB := ""
	if len(databases) > 0 {
		primaryDB = r.getDatabaseNameFromSpec(databases[0])
	}

	secret.Data = map[string][]byte{
		hostKey: []byte(cluster.Spec.Endpoint),
		portKey: []byte(fmt.Sprintf("%d", cluster.Spec.Port)),
		dbKey:   []byte(primaryDB),
		userKey: []byte(username),
		pwdKey:  []byte(password),
	}

	// Add databases list only for raw template with multiple databases
	if r.shouldIncludeDatabasesList(user, len(databases)) {
		dbNames := make([]string, len(databases))
		for i, db := range databases {
			dbNames[i] = r.getDatabaseNameFromSpec(db)
		}
		secret.Data["databases"] = []byte(strings.Join(dbNames, ","))
	}

	if err = r.Update(ctx, secret); err != nil {
		return "", "", false, fmt.Errorf("failed to update secret during adopt: %w", err)
	}

	return password, secret.Name, true, nil
}

func (r *DatabaseUserReconciler) mergeSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
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

	primaryDB := ""
	if len(databases) > 0 {
		primaryDB = r.getDatabaseNameFromSpec(databases[0])
	}

	secret.Data[hostKey] = []byte(cluster.Spec.Endpoint)
	secret.Data[portKey] = []byte(fmt.Sprintf("%d", cluster.Spec.Port))
	secret.Data[dbKey] = []byte(primaryDB)
	secret.Data[userKey] = []byte(username)
	secret.Data[pwdKey] = []byte(password)

	// Add databases list only for raw template with multiple databases
	if r.shouldIncludeDatabasesList(user, len(databases)) {
		dbNames := make([]string, len(databases))
		for i, db := range databases {
			dbNames[i] = r.getDatabaseNameFromSpec(db)
		}
		secret.Data["databases"] = []byte(strings.Join(dbNames, ","))
	} else {
		delete(secret.Data, "databases")
	}

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

	clusterName, databaseNames := r.getClusterAndDatabasesForDeletion(ctx, user)
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

	// Revoke privileges from all databases
	for _, dbName := range databaseNames {
		if err := pgClient.RevokePrivilegesInDatabase(ctx, username, dbName); err != nil {
			logger.Error(err, "failed to revoke privileges", "database", dbName)
		}
	}

	if err := pgClient.DropUser(ctx, username); err != nil {
		logger.Error(err, "failed to drop user")
	} else {
		logger.Info("user dropped", "username", username)
	}
}

func (r *DatabaseUserReconciler) getClusterAndDatabasesForDeletion(ctx context.Context, user *databasesv1alpha1.DatabaseUser) (clusterName string, databaseNames []string) {
	// First try to get from status
	if user.Status.ClusterName != "" {
		databaseNames = make([]string, len(user.Status.Databases))
		for i, db := range user.Status.Databases {
			databaseNames[i] = db.DatabaseName
		}
		return user.Status.ClusterName, databaseNames
	}

	// Fall back to fetching from spec
	dbAccesses := user.Spec.GetDatabases()
	if len(dbAccesses) == 0 {
		return "", nil
	}

	for _, dbAccess := range dbAccesses {
		dbNamespace := dbAccess.Namespace
		if dbNamespace == "" {
			dbNamespace = user.Namespace
		}

		var db databasesv1alpha1.Database
		if err := r.Get(ctx, types.NamespacedName{
			Name:      dbAccess.Name,
			Namespace: dbNamespace,
		}, &db); err != nil {
			continue
		}

		if clusterName == "" {
			clusterName = db.Spec.ClusterRef.Name
		}
		databaseNames = append(databaseNames, r.getDatabaseNameFromSpec(&db))
	}

	return clusterName, databaseNames
}

func (r *DatabaseUserReconciler) deleteOldSecret(ctx context.Context, namespace, secretName string, user *databasesv1alpha1.DatabaseUser) {
	logger := log.FromContext(ctx)

	var oldSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, &oldSecret); err != nil {
		return
	}

	if !r.isSecretOwnedByUser(&oldSecret, user) {
		logger.Info("skipping old secret deletion - not owned by this user", "secret", secretName)
		return
	}

	if err := r.Delete(ctx, &oldSecret); err != nil {
		logger.Error(err, "failed to delete old secret", "secret", secretName)
	} else {
		logger.Info("deleted old secret after name change", "secret", secretName)
	}
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
	Username        string
	Databases       []databasesv1alpha1.DatabaseAccessStatus
}

func (r *DatabaseUserReconciler) setStatus(ctx context.Context, user *databasesv1alpha1.DatabaseUser, update *statusUpdate) (ctrl.Result, error) {
	// Handle pending timeout (may modify update)
	r.handlePendingTimeout(user, update)

	// Check if status actually changed
	statusChanged := user.Status.Phase != update.Phase ||
		user.Status.Message != update.Message ||
		user.Status.ObservedGeneration != user.Generation ||
		(update.ClusterName != "" && user.Status.ClusterName != update.ClusterName) ||
		(update.Username != "" && user.Status.Username != update.Username) ||
		(update.SecretName != "" && user.Status.SecretName != update.SecretName) ||
		update.PasswordUpdated ||
		len(update.Databases) > 0

	if statusChanged {
		patch := client.MergeFrom(user.DeepCopy())

		user.Status.Phase = update.Phase
		user.Status.Message = update.Message
		user.Status.ObservedGeneration = user.Generation

		r.applyStatusFields(user, update)

		if err := r.Status().Patch(ctx, user, patch); err != nil {
			return ctrl.Result{}, err
		}
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
	if update.Username != "" {
		user.Status.Username = update.Username
	}
	if update.SecretName != "" {
		user.Status.SecretName = update.SecretName
	}
	if len(update.Databases) > 0 {
		user.Status.Databases = update.Databases
		user.Status.DatabasesSummary = r.buildDatabasesSummary(update.Databases)
	}
	if update.PasswordUpdated || (user.Status.PasswordUpdatedAt == nil && update.Phase == "Ready") {
		now := metav1.Now()
		user.Status.PasswordUpdatedAt = &now
	}
}

func (r *DatabaseUserReconciler) buildDatabasesSummary(databases []databasesv1alpha1.DatabaseAccessStatus) string {
	if len(databases) == 0 {
		return ""
	}
	if len(databases) == 1 {
		return databases[0].DatabaseName
	}
	return fmt.Sprintf("%s (+%d)", databases[0].DatabaseName, len(databases)-1)
}

func (r *DatabaseUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.DatabaseUser{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
