package controllers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

const (
	UserFinalizerName = "databaseusers.dbtether.io/finalizer"

	// Soft-ownership signal — lets us re-attach OwnerRef without rotating the
	// password when stripped externally (ArgoCD/Flux managedFields rewrite).
	ManagedByAnnotation = "dbtether.io/managed-by"

	// Without this marker the next reconcile after mergeSecret degrades Merge
	// into Adopt (full-replace nukes foreign keys).
	ConflictPolicyAnnotation = "dbtether.io/conflict-policy"

	// Lets Merge mode tell foreign keys from ours: drop stale ones we
	// previously owned, preserve everything else.
	ManagedKeysAnnotation = "dbtether.io/managed-keys"
)

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
			// Rotation is time-driven: only fall through when it is actually due,
			// otherwise re-arm the requeue this fast path would otherwise swallow.
			if !r.shouldRotatePassword(&user) {
				return ctrl.Result{RequeueAfter: r.calculateRequeueAfter(&user)}, nil
			}
		} else {
			logger.Info("secret missing, triggering reconciliation", "secret", secretName)
		}
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
			if apierrors.IsNotFound(err) {
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
		if apierrors.IsNotFound(err) {
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
	return UsernameForUser(user)
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
	host, port, db, username, password = "host", "port", "database", "username", "password"

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
	case "dsn":
		// Forces callers to branch on Template — secret.Data[""] reads are harmless.
		return "", "", "", "", ""
	case "custom":
		host, port, db, username, password = r.applyCustomKeys(user.Spec.Secret.Keys, host, port, db, username, password)
	}
	return
}

func (r *DatabaseUserReconciler) applyCustomKeys(k *databasesv1alpha1.SecretKeys, host, port, db, username, password string) (outHost, outPort, outDB, outUser, outPwd string) {
	if k == nil {
		return host, port, db, username, password
	}
	return strOr(k.Host, host), strOr(k.Port, port), strOr(k.Database, db), strOr(k.Username, username), strOr(k.Password, password)
}

func strOr(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
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

// Annotation match + no foreign controller OwnerReference; see ManagedByAnnotation.
// Rejects any other controller-ref (not just DatabaseUser) — SetControllerReference
// would fail later with "already has a controller", and re-claim would mis-fire.
func (r *DatabaseUserReconciler) isSecretSoftOwnedByUser(secret *corev1.Secret, user *databasesv1alpha1.DatabaseUser) bool {
	if secret.Annotations[ManagedByAnnotation] != user.Name {
		return false
	}
	for _, ref := range secret.OwnerReferences {
		isUs := ref.Kind == "DatabaseUser" && ref.Name == user.Name
		if isUs {
			continue
		}
		if ref.Controller != nil && *ref.Controller {
			return false
		}
	}
	return true
}

func (r *DatabaseUserReconciler) getOnConflictPolicy(user *databasesv1alpha1.DatabaseUser) string {
	if user.Spec.Secret != nil && user.Spec.Secret.OnConflict != "" {
		return user.Spec.Secret.OnConflict
	}
	return "Fail"
}

// Recomputed from spec every reconcile — template/keys changes take effect on
// the next tick without inference from secret.Data.
func (r *DatabaseUserReconciler) buildSecretData(user *databasesv1alpha1.DatabaseUser,
	cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	username, password string) map[string][]byte {

	primaryDB := ""
	if len(databases) > 0 {
		primaryDB = r.getDatabaseNameFromSpec(databases[0])
	}

	if user.Spec.Secret != nil && user.Spec.Secret.Template == "dsn" {
		return map[string][]byte{"dsn": []byte(buildDSN(cluster, username, password, primaryDB))}
	}

	hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(user)
	data := map[string][]byte{
		hostKey: []byte(cluster.Spec.Endpoint),
		portKey: []byte(fmt.Sprintf("%d", cluster.Spec.Port)),
		dbKey:   []byte(primaryDB),
		userKey: []byte(username),
		pwdKey:  []byte(password),
	}
	if r.shouldIncludeDatabasesList(user, len(databases)) {
		dbNames := make([]string, len(databases))
		for i, db := range databases {
			dbNames[i] = r.getDatabaseNameFromSpec(db)
		}
		data["databases"] = []byte(strings.Join(dbNames, ","))
	}
	return data
}

func (r *DatabaseUserReconciler) buildPerDatabaseSecretData(user *databasesv1alpha1.DatabaseUser,
	cluster *databasesv1alpha1.DBCluster, db *databasesv1alpha1.Database,
	username, password string) map[string][]byte {

	dbName := r.getDatabaseNameFromSpec(db)

	if user.Spec.Secret != nil && user.Spec.Secret.Template == "dsn" {
		return map[string][]byte{"dsn": []byte(buildDSN(cluster, username, password, dbName))}
	}

	hostKey, portKey, dbKey, userKey, pwdKey := r.getSecretKeys(user)
	return map[string][]byte{
		hostKey: []byte(cluster.Spec.Endpoint),
		portKey: []byte(fmt.Sprintf("%d", cluster.Spec.Port)),
		dbKey:   []byte(dbName),
		userKey: []byte(username),
		pwdKey:  []byte(password),
	}
}

// DBCluster.Endpoint and DatabaseName aren't pattern-validated; net/url escaping
// prevents crafted values from producing a DSN pointing at a different host.
func buildDSN(cluster *databasesv1alpha1.DBCluster, username, password, dbName string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(username, password),
		Host:   fmt.Sprintf("%s:%d", cluster.Spec.Endpoint, cluster.Spec.Port),
		Path:   "/" + dbName,
	}
	return u.String()
}

// Returns "" if no password is recoverable; custom-keyed secrets whose pwdKey
// was renamed are treated as corrupted (caller will regenerate).
func (r *DatabaseUserReconciler) extractPasswordFromSecret(secret *corev1.Secret, user *databasesv1alpha1.DatabaseUser) string {
	if secret == nil || len(secret.Data) == 0 {
		return ""
	}

	_, _, _, _, currentPwdKey := r.getSecretKeys(user)
	candidates := []string{currentPwdKey, "password", "DB_PASSWORD", "DATABASE_PASSWORD", "POSTGRES_PASSWORD"}
	seen := map[string]struct{}{}
	for _, k := range candidates {
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if pwd := string(secret.Data[k]); pwd != "" {
			return pwd
		}
	}

	return passwordFromDSN(secret.Data["dsn"])
}

func passwordFromDSN(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	u, err := url.Parse(string(data))
	if err != nil || u.User == nil {
		return ""
	}
	pwd, ok := u.User.Password()
	if !ok {
		return ""
	}
	return pwd
}

// Full-replace; companion to reconcileSecretMerge for Merge-policy secrets.
func (r *DatabaseUserReconciler) reconcileSecretShape(ctx context.Context, secret *corev1.Secret, desired map[string][]byte) (bool, error) {
	prevKeys := parseManagedKeysAnnotation(secret)
	if secretDataEqual(secret.Data, desired) && managedKeysMatch(prevKeys, desired) {
		return false, nil
	}
	secret.Data = desired
	secret.StringData = nil
	setManagedKeysAnnotation(secret, desired)
	if err := r.Update(ctx, secret); err != nil {
		return false, fmt.Errorf("update secret: %w", err)
	}
	return true, nil
}

// Overlay converge for Merge-policy secrets. Without per-reconcile awareness
// of Merge mode the cycle would full-replace and degrade Merge into Adopt.
func (r *DatabaseUserReconciler) reconcileSecretMerge(ctx context.Context, secret *corev1.Secret, desired map[string][]byte) (bool, error) {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	prevOurKeys := parseManagedKeysAnnotation(secret)

	newData := make(map[string][]byte, len(secret.Data)+len(desired))
	for k, v := range secret.Data {
		if _, wasOurs := prevOurKeys[k]; wasOurs {
			if _, stillOurs := desired[k]; !stillOurs {
				continue
			}
		}
		newData[k] = v
	}
	for k, v := range desired {
		newData[k] = v
	}

	if secretDataEqual(secret.Data, newData) && managedKeysMatch(prevOurKeys, desired) {
		return false, nil
	}
	secret.Data = newData
	setManagedKeysAnnotation(secret, desired)
	if err := r.Update(ctx, secret); err != nil {
		return false, fmt.Errorf("update secret (merge): %w", err)
	}
	return true, nil
}

func secretDataEqual(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		bv, ok := b[k]
		if !ok || !bytes.Equal(v, bv) {
			return false
		}
	}
	return true
}

// Detects annotation drift independent of secret.Data — needed so legacy
// secrets (no annotation yet) get stamped on next reconcile.
func managedKeysMatch(prev map[string]struct{}, desired map[string][]byte) bool {
	if prev == nil {
		return false
	}
	if len(prev) != len(desired) {
		return false
	}
	for k := range desired {
		if _, ok := prev[k]; !ok {
			return false
		}
	}
	return true
}

// generated=true means caller should bump PasswordUpdatedAt.
func (r *DatabaseUserReconciler) resolvePassword(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	existing *corev1.Secret, found bool, pgClient postgres.ClientInterface, username string) (password string, generated bool, err error) {

	if found {
		if pwd := r.extractPasswordFromSecret(existing, user); pwd != "" {
			return pwd, false, nil
		}
		log.FromContext(ctx).Info("secret exists but password not extractable, regenerating", "secret", existing.Name)
	}

	length := user.Spec.Password.Length
	if length == 0 {
		length = postgres.DefaultPasswordLength
	}
	newPwd, err := postgres.GeneratePassword(length)
	if err != nil {
		return "", false, fmt.Errorf("failed to generate password: %w", err)
	}

	// Skip SetPassword on first-time creation — ensureUserInPostgres will
	// CREATE USER WITH PASSWORD. Otherwise the PG user has a stale password
	// (regenerated secret or post-Ready deletion) and must be updated.
	needSetPassword := found || user.Status.Phase == "Ready"
	if needSetPassword {
		if err := pgClient.SetPassword(ctx, username, newPwd); err != nil {
			return "", false, fmt.Errorf("failed to update password in PostgreSQL: %w", err)
		}
	}
	return newPwd, true, nil
}

func (r *DatabaseUserReconciler) createOwnedSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	name string, extraAnnotations map[string]string, data map[string][]byte) error {

	annotations := map[string]string{ManagedByAnnotation: user.Name}
	for k, v := range extraAnnotations {
		annotations[k] = v
	}
	secret := corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   user.Namespace,
			Annotations: annotations,
		},
		Data: data,
	}
	setManagedKeysAnnotation(&secret, data)
	if err := controllerutil.SetControllerReference(user, &secret, r.Scheme); err != nil {
		return fmt.Errorf("failed to set controller reference: %w", err)
	}
	if err := r.Create(ctx, &secret); err != nil {
		return fmt.Errorf("failed to create secret: %w", err)
	}
	return nil
}

// Sorted so the annotation is stable across reconciles — Go map iteration
// order otherwise causes spurious Updates.
func setManagedKeysAnnotation(secret *corev1.Secret, data map[string][]byte) {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[ManagedKeysAnnotation] = strings.Join(keys, ",")
}

// nil ⇒ annotation absent (legacy secret); distinct from empty ⇒ annotation
// present with zero keys tracked.
func parseManagedKeysAnnotation(secret *corev1.Secret) map[string]struct{} {
	raw, ok := secret.Annotations[ManagedKeysAnnotation]
	if !ok {
		return nil
	}
	if raw == "" {
		return map[string]struct{}{}
	}
	out := make(map[string]struct{})
	for _, k := range strings.Split(raw, ",") {
		out[k] = struct{}{}
	}
	return out
}

// Adopting/merging a secret we previously stamped means the OwnerReference
// was stripped externally (e.g. ArgoCD overwriting managedFields).
func (r *DatabaseUserReconciler) warnIfOwnershipStripped(ctx context.Context, secret *corev1.Secret, user *databasesv1alpha1.DatabaseUser, op string) {
	if secret.Annotations[ManagedByAnnotation] == user.Name {
		log.FromContext(ctx).Info(
			"WARNING: re-claiming a secret previously managed by this user — OwnerReference appears to have been stripped externally; password will be rotated",
			"secret", secret.Name, "user", user.Name, "operation", op,
		)
	}
}

func (r *DatabaseUserReconciler) ensureFinalizer(ctx context.Context, user *databasesv1alpha1.DatabaseUser) (*ctrl.Result, error) {
	if controllerutil.ContainsFinalizer(user, UserFinalizerName) {
		return nil, nil
	}
	patch := client.MergeFrom(user.DeepCopy())
	controllerutil.AddFinalizer(user, UserFinalizerName)
	if err := r.Patch(ctx, user, patch); err != nil {
		return &ctrl.Result{}, err
	}
	return &ctrl.Result{}, nil
}

func (r *DatabaseUserReconciler) reconcileUser(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	databases []*databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster) (ctrl.Result, error) {

	username := r.getUsername(user)
	dbNames := make([]string, len(databases))
	for i, db := range databases {
		dbNames[i] = r.getDatabaseNameFromSpec(db)
	}
	baseStatus := statusUpdate{ClusterName: cluster.Name, Username: username}

	pgClient, err := GetPostgresClient(ctx, r.Client, r.PGClientCache, cluster)
	if err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = fmt.Sprintf("connection error: %s", err.Error())
		baseStatus.RequeueAfter = 60 * time.Second
		return r.setStatus(ctx, user, &baseStatus)
	}

	password, secretName, passwordChanged, err := r.ensureSecrets(ctx, user, databases, cluster, pgClient)
	if err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = fmt.Sprintf("secret error: %s", err.Error())
		// passwordChanged=true on partial rotation prevents a re-rotation loop
		// when a per-DB secret write failed.
		baseStatus.PasswordUpdated = passwordChanged
		baseStatus.SecretName = secretName
		return r.setStatus(ctx, user, &baseStatus)
	}
	if user.Status.SecretName != "" && user.Status.SecretName != secretName {
		r.deleteOldSecret(ctx, user.Namespace, user.Status.SecretName, user)
	}
	baseStatus.SecretName = secretName

	if err := r.syncPostgresUser(ctx, pgClient, user, username, password, dbNames); err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = err.Error()
		return r.setStatus(ctx, user, &baseStatus)
	}

	dbStatuses := r.applyPerDatabasePrivileges(ctx, pgClient, user, username, databases)

	if err := r.syncRuntimeParams(ctx, pgClient, user, username); err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = err.Error()
		return r.setStatus(ctx, user, &baseStatus)
	}

	r.verifyIsolation(ctx, pgClient, username, dbNames)

	baseStatus.PasswordUpdated = passwordChanged
	baseStatus.Databases = dbStatuses

	// A per-database apply failure must not read as Ready: surface it top-level
	// and return an error so controller-runtime retries with backoff.
	if failed := failedDatabaseNames(dbStatuses); len(failed) > 0 {
		msg := fmt.Sprintf("failed to apply privileges for database(s): %s", strings.Join(failed, ", "))
		baseStatus.Phase = "Failed"
		baseStatus.Message = msg
		if _, err := r.setStatus(ctx, user, &baseStatus); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, errors.New(msg)
	}

	log.FromContext(ctx).Info("user ready", "username", username, "databases", len(databases))

	baseStatus.Phase = "Ready"
	baseStatus.Message = fmt.Sprintf("user created with access to %d database(s)", len(databases))
	baseStatus.RequeueAfter = r.calculateRequeueAfter(user)
	return r.setStatus(ctx, user, &baseStatus)
}

// Mutates the PG role itself before per-DB privileges are applied.
func (r *DatabaseUserReconciler) syncPostgresUser(ctx context.Context, pgClient postgres.ClientInterface,
	user *databasesv1alpha1.DatabaseUser, username, password string, dbNames []string) error {

	if err := r.ensureUserInPostgres(ctx, pgClient, username, password); err != nil {
		return err
	}
	r.reassignOwnershipForRemovedDatabases(ctx, pgClient, user, username, dbNames)
	if err := pgClient.SyncDatabaseAccess(ctx, username, dbNames); err != nil {
		return fmt.Errorf("failed to sync database access: %w", err)
	}
	return nil
}

func (r *DatabaseUserReconciler) applyPerDatabasePrivileges(ctx context.Context, pgClient postgres.ClientInterface,
	user *databasesv1alpha1.DatabaseUser, username string, databases []*databasesv1alpha1.Database) []databasesv1alpha1.DatabaseAccessStatus {

	dbStatuses := make([]databasesv1alpha1.DatabaseAccessStatus, len(databases))
	dbAccesses := user.Spec.GetDatabases()

	for i, db := range databases {
		dbName := r.getDatabaseNameFromSpec(db)
		grants := ResolveUserGrants(user, dbAccesses[i])

		status := databasesv1alpha1.DatabaseAccessStatus{
			Name:         dbAccesses[i].Name,
			Namespace:    dbAccesses[i].Namespace,
			DatabaseName: dbName,
			Privileges:   grants.Privileges,
		}
		if err := pgClient.ApplyPrivileges(ctx, username, dbName, grants.Privileges, grants.AdditionalGrants); err != nil {
			status.Phase = "Failed"
			status.Message = err.Error()
		} else {
			status.Phase = "Ready"
		}
		if user.Spec.SecretGeneration == "perDatabase" {
			status.SecretName = r.getSecretNameForDatabase(user, db.Name)
		}
		dbStatuses[i] = status
	}
	return dbStatuses
}

func (r *DatabaseUserReconciler) syncRuntimeParams(ctx context.Context, pgClient postgres.ClientInterface,
	user *databasesv1alpha1.DatabaseUser, username string) error {

	if err := r.syncConnectionLimit(ctx, pgClient, username, user); err != nil {
		return fmt.Errorf("failed to sync connection limit: %w", err)
	}
	if err := r.syncIdleInTransactionTimeout(ctx, pgClient, username, user); err != nil {
		return fmt.Errorf("failed to apply idle_in_transaction_session_timeout: %w", err)
	}
	return nil
}

// Read-before-write to avoid ALTER USER on every reconcile. CRD blocks
// ConnectionLimit=0, so the int zero-value is "unset" → -1 (PG default).
func (r *DatabaseUserReconciler) syncConnectionLimit(ctx context.Context,
	pgClient postgres.ClientInterface, username string, user *databasesv1alpha1.DatabaseUser) error {

	current, err := pgClient.GetConnectionLimit(ctx, username)
	if err != nil {
		return err
	}
	desired := user.Spec.ConnectionLimit
	if desired == 0 {
		desired = -1
	}
	if current == desired {
		return nil
	}
	return pgClient.SetConnectionLimit(ctx, username, desired)
}

// Read-before-write so RESET isn't sent on every reconcile when field is unset.
func (r *DatabaseUserReconciler) syncIdleInTransactionTimeout(ctx context.Context,
	pgClient postgres.ClientInterface, username string, user *databasesv1alpha1.DatabaseUser) error {

	current, hasCurrent, err := pgClient.GetRoleParameter(ctx, username, postgres.RoleParamIdleInTransactionTimeout)
	if err != nil {
		return err
	}

	if user.Spec.IdleInTransactionTimeout != nil {
		desired := fmt.Sprintf("%dms", user.Spec.IdleInTransactionTimeout.Milliseconds())
		if hasCurrent && current == desired {
			return nil
		}
		return pgClient.SetRoleParameter(ctx, username, postgres.RoleParamIdleInTransactionTimeout, desired)
	}

	if !hasCurrent {
		return nil
	}
	return pgClient.ResetRoleParameter(ctx, username, postgres.RoleParamIdleInTransactionTimeout)
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
		return fmt.Errorf("failed to check user: %w", err)
	}

	if exists {
		if err := pgClient.SetPassword(ctx, username, password); err != nil {
			return fmt.Errorf("failed to set password: %w", err)
		}
		return nil
	}

	if err := pgClient.CreateUser(ctx, username, password); err != nil {
		return fmt.Errorf("failed to create user: %w", err)
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

// Converge contract:
//   - owned + extractable: keep password, rebuild shape from spec.
//   - owned + corrupt: regenerate + SetPassword.
//   - missing: generate (+ SetPassword if previously Ready) + create.
//   - unowned: Adopt/Merge/Fail per onConflict.
func (r *DatabaseUserReconciler) ensureSecrets(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	databases []*databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster,
	pgClient postgres.ClientInterface) (password, primarySecretName string, passwordChanged bool, err error) {

	if user.Spec.SecretGeneration == "perDatabase" {
		return r.ensurePerDatabaseSecrets(ctx, user, databases, cluster, pgClient)
	}
	return r.ensurePrimarySecret(ctx, user, databases, cluster, pgClient)
}

func (r *DatabaseUserReconciler) ensurePrimarySecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	databases []*databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster,
	pgClient postgres.ClientInterface) (password, primarySecretName string, passwordChanged bool, err error) {

	primarySecretName = r.getSecretName(user)
	username := r.getUsername(user)

	var existing corev1.Secret
	getErr := r.Get(ctx, types.NamespacedName{Name: primarySecretName, Namespace: user.Namespace}, &existing)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return "", "", false, getErr
	}
	found := getErr == nil

	if found && !r.isSecretOwnedByUser(&existing, user) {
		if out := r.handleUnownedPrimarySecret(ctx, user, &existing, cluster, databases, pgClient, username); out.Done {
			return out.Password, out.SecretName, out.PasswordChanged, out.Err
		}
		// Fall through to the standard sync path — re-claim only fixed ownership.
	}

	if found && r.shouldRotatePassword(user) {
		return r.rotatePrimaryPassword(ctx, user, &existing, cluster, databases, pgClient, username)
	}

	password, passwordChanged, err = r.resolvePassword(ctx, user, &existing, found, pgClient, username)
	if err != nil {
		return "", "", false, err
	}

	desired := r.buildSecretData(user, cluster, databases, username, password)

	if !found {
		if err := r.createOwnedSecret(ctx, user, primarySecretName, nil, desired); err != nil {
			return "", "", false, err
		}
		return password, primarySecretName, true, nil
	}
	// Merge-policy secrets stay on the overlay path — full-replace here would
	// drop the foreign keys mergeSecret preserved.
	if existing.Annotations[ConflictPolicyAnnotation] == "Merge" {
		if _, err := r.reconcileSecretMerge(ctx, &existing, desired); err != nil {
			return "", "", false, err
		}
		return password, primarySecretName, passwordChanged, nil
	}
	if _, err := r.reconcileSecretShape(ctx, &existing, desired); err != nil {
		return "", "", false, err
	}
	return password, primarySecretName, passwordChanged, nil
}

// Re-attaches OwnerReference on a soft-owned secret (annotation matches, no
// foreign controller). Used wherever an unowned-but-managed secret can be
// healed in place — primary path, perDatabase first secret, per-DB loop.
func (r *DatabaseUserReconciler) doSoftReclaim(ctx context.Context, user *databasesv1alpha1.DatabaseUser, secret *corev1.Secret) error {
	log.FromContext(ctx).Info("re-claiming soft-owned secret", "secret", secret.Name, "user", user.Name)
	if err := controllerutil.SetControllerReference(user, secret, r.Scheme); err != nil {
		return fmt.Errorf("failed to re-attach OwnerReference: %w", err)
	}
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("failed to update secret with restored OwnerReference: %w", err)
	}
	return nil
}

// Soft-owned re-claim path for perDatabase mode, where Adopt/Merge isn't
// supported. Restores OwnerReference if the annotation matches; otherwise fails.
func (r *DatabaseUserReconciler) reclaimOrFail(ctx context.Context, user *databasesv1alpha1.DatabaseUser, secret *corev1.Secret) error {
	if !r.isSecretSoftOwnedByUser(secret, user) {
		return fmt.Errorf("secret %s already exists and is not owned by this DatabaseUser", secret.Name)
	}
	return r.doSoftReclaim(ctx, user, secret)
}

// unownedOutcome carries the result of handleUnownedPrimarySecret. Done=false
// means the secret was self-healed (soft-owned re-claim) and the caller should
// continue down the normal sync path.
type unownedOutcome struct {
	Done            bool
	Password        string
	SecretName      string
	PasswordChanged bool
	Err             error
}

func (r *DatabaseUserReconciler) handleUnownedPrimarySecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	existing *corev1.Secret, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	pgClient postgres.ClientInterface, username string) unownedOutcome {

	if r.isSecretSoftOwnedByUser(existing, user) {
		if err := r.doSoftReclaim(ctx, user, existing); err != nil {
			return unownedOutcome{Done: true, Err: err}
		}
		return unownedOutcome{}
	}

	switch r.getOnConflictPolicy(user) {
	case "Adopt":
		p, sn, pc, e := r.adoptSecret(ctx, user, existing, cluster, databases, pgClient, username)
		return unownedOutcome{Done: true, Password: p, SecretName: sn, PasswordChanged: pc, Err: e}
	case "Merge":
		p, sn, pc, e := r.mergeSecret(ctx, user, existing, cluster, databases, pgClient, username)
		return unownedOutcome{Done: true, Password: p, SecretName: sn, PasswordChanged: pc, Err: e}
	default:
		return unownedOutcome{Done: true, Err: fmt.Errorf("secret %s already exists and is not owned by this DatabaseUser", existing.Name)}
	}
}

func (r *DatabaseUserReconciler) ensurePerDatabaseSecrets(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	databases []*databasesv1alpha1.Database, cluster *databasesv1alpha1.DBCluster,
	pgClient postgres.ClientInterface) (password, primarySecretName string, passwordChanged bool, err error) {

	username := r.getUsername(user)
	if len(databases) == 0 {
		return "", "", false, fmt.Errorf("no databases to create secrets for")
	}
	primarySecretName = r.getSecretNameForDatabase(user, databases[0].Name)

	// First database's secret carries the password — single source for all DBs.
	var firstSecret corev1.Secret
	getErr := r.Get(ctx, types.NamespacedName{Name: primarySecretName, Namespace: user.Namespace}, &firstSecret)
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return "", "", false, getErr
	}
	found := getErr == nil

	if found && !r.isSecretOwnedByUser(&firstSecret, user) {
		if err := r.reclaimOrFail(ctx, user, &firstSecret); err != nil {
			return "", "", false, err
		}
	}
	if found && r.shouldRotatePassword(user) {
		return r.rotatePerDatabasePassword(ctx, user, cluster, databases, pgClient, username)
	}

	password, passwordChanged, err = r.resolvePassword(ctx, user, &firstSecret, found, pgClient, username)
	if err != nil {
		return "", "", false, err
	}

	for _, db := range databases {
		if err := r.syncPerDatabaseSecret(ctx, user, cluster, db, username, password); err != nil {
			return "", "", false, err
		}
	}

	return password, primarySecretName, passwordChanged, nil
}

// Idempotent reconcile for one per-DB secret: create if missing, soft-reclaim
// if OwnerRef stripped, then converge shape.
func (r *DatabaseUserReconciler) syncPerDatabaseSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	cluster *databasesv1alpha1.DBCluster, db *databasesv1alpha1.Database, username, password string) error {

	name := r.getSecretNameForDatabase(user, db.Name)
	desired := r.buildPerDatabaseSecretData(user, cluster, db, username, password)

	var current corev1.Secret
	getErr := r.Get(ctx, types.NamespacedName{Name: name, Namespace: user.Namespace}, &current)
	if apierrors.IsNotFound(getErr) {
		ann := map[string]string{"dbtether.io/database": db.Name}
		if err := r.createOwnedSecret(ctx, user, name, ann, desired); err != nil {
			return fmt.Errorf("create secret for database %s: %w", db.Name, err)
		}
		return nil
	}
	if getErr != nil {
		return getErr
	}
	if !r.isSecretOwnedByUser(&current, user) {
		if err := r.reclaimOrFail(ctx, user, &current); err != nil {
			return err
		}
	}
	if _, err := r.reconcileSecretShape(ctx, &current, desired); err != nil {
		return fmt.Errorf("update secret for database %s: %w", db.Name, err)
	}
	return nil
}

func (r *DatabaseUserReconciler) createDatabaseSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secretName string, cluster *databasesv1alpha1.DBCluster, db *databasesv1alpha1.Database,
	username, password string) error {

	data := r.buildPerDatabaseSecretData(user, cluster, db, username, password)
	ann := map[string]string{"dbtether.io/database": db.Name}

	var existing corev1.Secret
	getErr := r.Get(ctx, types.NamespacedName{Name: secretName, Namespace: user.Namespace}, &existing)
	if apierrors.IsNotFound(getErr) {
		return r.createOwnedSecret(ctx, user, secretName, ann, data)
	}
	if getErr != nil {
		return getErr
	}
	_, err := r.reconcileSecretShape(ctx, &existing, data)
	return err
}

func (r *DatabaseUserReconciler) rotatePrimaryPassword(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	pgClient postgres.ClientInterface, username string) (password, secretName string, passwordChanged bool, err error) {

	logger := log.FromContext(ctx)
	logger.Info("rotating password", "username", username, "days", user.Spec.Rotation.Days)

	password, err = r.generateAndSetPassword(ctx, user, pgClient, username)
	if err != nil {
		return "", "", false, err
	}

	desired := r.buildSecretData(user, cluster, databases, username, password)
	// Merge-mode rotation must overlay — full-replace would nuke foreign keys.
	if secret.Annotations[ConflictPolicyAnnotation] == "Merge" {
		if _, err := r.reconcileSecretMerge(ctx, secret, desired); err != nil {
			return "", "", false, err
		}
	} else {
		if _, err := r.reconcileSecretShape(ctx, secret, desired); err != nil {
			return "", "", false, err
		}
	}

	logger.Info("password rotated successfully", "username", username)
	return password, secret.Name, true, nil
}

func (r *DatabaseUserReconciler) rotatePerDatabasePassword(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	pgClient postgres.ClientInterface, username string) (password, secretName string, passwordChanged bool, err error) {

	logger := log.FromContext(ctx)
	logger.Info("rotating password", "username", username, "days", user.Spec.Rotation.Days)

	password, err = r.generateAndSetPassword(ctx, user, pgClient, username)
	if err != nil {
		return "", "", false, err
	}

	if len(databases) > 0 {
		secretName = r.getSecretNameForDatabase(user, databases[0].Name)
	}

	// Return passwordChanged=true even on partial failure — PG has already
	// rotated, so PasswordUpdatedAt must bump or shouldRotatePassword fires
	// again next reconcile and churns PG until the broken secret heals.
	var rotationErrs []error
	for _, db := range databases {
		dbSecretName := r.getSecretNameForDatabase(user, db.Name)
		if err := r.createDatabaseSecret(ctx, user, dbSecretName, cluster, db, username, password); err != nil {
			rotationErrs = append(rotationErrs, fmt.Errorf("secret %s: %w", dbSecretName, err))
		}
	}
	if len(rotationErrs) > 0 {
		return password, secretName, true, fmt.Errorf("rotation succeeded in PostgreSQL but %d per-database secret write(s) failed: %w", len(rotationErrs), errors.Join(rotationErrs...))
	}

	logger.Info("password rotated successfully", "username", username)
	return password, secretName, true, nil
}

func (r *DatabaseUserReconciler) generateAndSetPassword(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	pgClient postgres.ClientInterface, username string) (string, error) {

	length := user.Spec.Password.Length
	if length == 0 {
		length = postgres.DefaultPasswordLength
	}
	password, err := postgres.GeneratePassword(length)
	if err != nil {
		return "", fmt.Errorf("failed to generate password: %w", err)
	}
	if err := pgClient.SetPassword(ctx, username, password); err != nil {
		return "", fmt.Errorf("failed to update password in PostgreSQL: %w", err)
	}
	return password, nil
}

// adoptSecret takes over an unowned secret in one Update — ownership claim,
// password, and data all in a single transaction. If the Update fails after
// SetPassword, the next reconcile re-Adopts (potentially generating a fresh
// password). Avoiding two-phase claim+data prevents the worse failure mode:
// claim succeeds → secret looks owned → resolvePassword keeps the OLD value
// indefinitely while PG holds the NEW one.
func (r *DatabaseUserReconciler) adoptSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	pgClient postgres.ClientInterface, username string) (password, secretName string, passwordChanged bool, err error) {

	log.FromContext(ctx).Info("adopting existing secret", "secret", secret.Name)
	r.warnIfOwnershipStripped(ctx, secret, user, "adopt")

	password, err = r.generateAndSetPassword(ctx, user, pgClient, username)
	if err != nil {
		return "", "", false, err
	}

	if err = controllerutil.SetControllerReference(user, secret, r.Scheme); err != nil {
		return "", "", false, fmt.Errorf("failed to set controller reference: %w", err)
	}
	secret.Data = r.buildSecretData(user, cluster, databases, username, password)
	secret.StringData = nil
	setManagedKeysAnnotation(secret, secret.Data)
	if err = r.Update(ctx, secret); err != nil {
		return "", "", false, fmt.Errorf("failed to update secret during adopt: %w", err)
	}
	return password, secret.Name, true, nil
}

func (r *DatabaseUserReconciler) mergeSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	pgClient postgres.ClientInterface, username string) (password, secretName string, passwordChanged bool, err error) {

	log.FromContext(ctx).Info("merging into existing secret", "secret", secret.Name)
	r.warnIfOwnershipStripped(ctx, secret, user, "merge")

	password, err = r.generateAndSetPassword(ctx, user, pgClient, username)
	if err != nil {
		return "", "", false, err
	}

	if err = controllerutil.SetControllerReference(user, secret, r.Scheme); err != nil {
		return "", "", false, fmt.Errorf("failed to set controller reference: %w", err)
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[ConflictPolicyAnnotation] = "Merge"

	// Overlay; foreign keys stay. managed-keys annotation lets future
	// reconciles drop our orphans without touching the user's keys.
	if secret.Data == nil {
		secret.Data = make(map[string][]byte)
	}
	desired := r.buildSecretData(user, cluster, databases, username, password)
	for k, v := range desired {
		secret.Data[k] = v
	}
	setManagedKeysAnnotation(secret, desired)
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
		if err := r.dropUserFromPostgres(ctx, user, username); err != nil {
			// Keep the finalizer and requeue: the CR must not disappear while the
			// PostgreSQL role still exists (e.g. Postgres unreachable at Exec).
			return ctrl.Result{}, err
		}
	} else {
		logger.Info("retaining user in PostgreSQL due to deletionPolicy", "username", username)
	}

	patch := client.MergeFrom(user.DeepCopy())
	controllerutil.RemoveFinalizer(user, UserFinalizerName)
	return ctrl.Result{}, r.Patch(ctx, user, patch)
}

func (r *DatabaseUserReconciler) dropUserFromPostgres(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	username string) error {

	logger := log.FromContext(ctx)

	clusterName, databaseNames := r.getClusterAndDatabasesForDeletion(ctx, user)
	if clusterName == "" {
		// No resolvable cluster (never provisioned, or all referenced Databases
		// gone) — cleanup is impossible by design, so release the finalizer.
		logger.Info("no cluster resolvable for cleanup, skipping user drop")
		return nil
	}

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: clusterName}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("cluster not found, skipping user drop")
			return nil
		}
		return err
	}

	pgClient, err := GetPostgresClient(ctx, r.Client, r.PGClientCache, &cluster)
	if err != nil {
		return fmt.Errorf("failed to get postgres client for cleanup: %w", err)
	}

	// Reassign ownership and revoke privileges from all databases. These are
	// best-effort: if the role still owns objects, the DropUser below fails and
	// gates finalizer removal, so a transient failure here is retried anyway.
	for _, dbName := range databaseNames {
		if err := pgClient.ReassignOwnership(ctx, username, dbName); err != nil {
			logger.Error(err, "failed to reassign ownership", "database", dbName)
		}
		if err := pgClient.RevokePrivilegesInDatabase(ctx, username, dbName); err != nil {
			logger.Error(err, "failed to revoke privileges", "database", dbName)
		}
	}

	if err := pgClient.DropUser(ctx, username); err != nil {
		return fmt.Errorf("failed to drop user %s: %w", username, err)
	}
	logger.Info("user dropped", "username", username)
	return nil
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

// reassignOwnershipForRemovedDatabases transfers ownership of objects back to master
// for databases that were previously in the user's access list but are now removed.
// This prevents orphan objects when a database is removed from the spec.
func (r *DatabaseUserReconciler) reassignOwnershipForRemovedDatabases(ctx context.Context,
	pgClient postgres.ClientInterface, user *databasesv1alpha1.DatabaseUser, username string, currentDBNames []string) {

	logger := log.FromContext(ctx)

	// Build set of current database names for O(1) lookup
	currentDBSet := make(map[string]bool, len(currentDBNames))
	for _, db := range currentDBNames {
		currentDBSet[db] = true
	}

	// Check previous databases from status
	for _, dbStatus := range user.Status.Databases {
		if dbStatus.DatabaseName == "" {
			continue
		}
		// If database was in status but not in current spec, reassign ownership
		if !currentDBSet[dbStatus.DatabaseName] {
			logger.Info("reassigning ownership for removed database", "database", dbStatus.DatabaseName, "username", username)
			if err := pgClient.ReassignOwnership(ctx, username, dbStatus.DatabaseName); err != nil {
				logger.Error(err, "failed to reassign ownership for removed database",
					"database", dbStatus.DatabaseName)
				// Continue anyway - best effort cleanup
			}
		}
	}
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
	// Snapshot before any status mutation — handlePendingTimeout writes
	// PendingSince, which must be part of the patch diff to reach the API server.
	patch := client.MergeFrom(user.DeepCopy())

	pendingChanged := r.handlePendingTimeout(user, update)

	statusChanged := pendingChanged ||
		user.Status.Phase != update.Phase ||
		user.Status.Message != update.Message ||
		user.Status.ObservedGeneration != user.Generation ||
		(update.ClusterName != "" && user.Status.ClusterName != update.ClusterName) ||
		(update.Username != "" && user.Status.Username != update.Username) ||
		(update.SecretName != "" && user.Status.SecretName != update.SecretName) ||
		update.PasswordUpdated ||
		len(update.Databases) > 0

	if statusChanged {
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

func (r *DatabaseUserReconciler) handlePendingTimeout(user *databasesv1alpha1.DatabaseUser, update *statusUpdate) (pendingChanged bool) {
	if update.Phase == "Pending" {
		now := metav1.Now()
		if user.Status.PendingSince == nil {
			user.Status.PendingSince = &now
			return true
		}
		if now.Sub(user.Status.PendingSince.Time) > PendingTimeout {
			update.Phase = "Failed"
			update.Message = fmt.Sprintf("timeout: %s (pending for over 10 minutes)", update.Message)
			// Transitioning out of Pending — reset the clock so a later Pending
			// episode starts fresh instead of re-timing-out immediately.
			user.Status.PendingSince = nil
			return true
		}
		return false
	}
	if user.Status.PendingSince != nil {
		user.Status.PendingSince = nil
		return true
	}
	return false
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

func failedDatabaseNames(statuses []databasesv1alpha1.DatabaseAccessStatus) []string {
	var failed []string
	for _, s := range statuses {
		if s.Phase == "Failed" {
			failed = append(failed, s.DatabaseName)
		}
	}
	return failed
}

func (r *DatabaseUserReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.DatabaseUser{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}
