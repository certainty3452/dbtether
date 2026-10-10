package controllers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	databasesv1alpha1 "github.com/certainty3452/dbtether/api/v1alpha1"
	"github.com/certainty3452/dbtether/pkg/postgres"
)

const (
	UserFinalizerName = "databaseusers.dbtether.io/finalizer"

	// Lets us re-attach OwnerRef without rotating the password after ArgoCD/Flux strips it.
	ManagedByAnnotation = "dbtether.io/managed-by"

	// Without it the next reconcile degrades Merge into Adopt and nukes foreign keys.
	ConflictPolicyAnnotation = "dbtether.io/conflict-policy"

	// Lets Merge drop stale keys we owned and preserve foreign ones.
	ManagedKeysAnnotation = "dbtether.io/managed-keys"

	// Bounds how long an out-of-band role drop or privilege change goes unrepaired.
	ReverifyInterval = 10 * time.Minute
)

type DatabaseUserReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	PGClientCache postgres.ClientCacheInterface
}

func (r *DatabaseUserReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var user databasesv1alpha1.DatabaseUser
	if err := r.Get(ctx, req.NamespacedName, &user); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !user.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, &user)
	}

	if err := ValidateUserSpec(&user); err != nil {
		return r.setStatus(ctx, &user, &statusUpdate{
			Phase:   "Failed",
			Message: fmt.Sprintf("validation error: %s", err.Error()),
		})
	}

	logger.V(1).Info("reconciling", "username", r.getUsername(&user))

	if result, err := r.ensureFinalizer(ctx, &user); result != nil || err != nil {
		return *result, err
	}

	databases, cluster, blocked, err := r.validateAndFetchDatabases(ctx, &user)
	if err != nil {
		return ctrl.Result{}, err
	}
	if blocked != nil {
		return r.withdrawDeniedWhileBlocked(ctx, &user, databases.deniedNames, blocked)
	}
	if len(databases.granted) == 0 {
		return r.withdrawAllAccess(ctx, &user, databases.denied)
	}

	conflicts, err := r.evaluateOwnerConflicts(ctx, &user, databases.granted)
	if err != nil {
		return ctrl.Result{}, err
	}

	return r.reconcileUser(ctx, &user, &databases, cluster, conflicts)
}

func (r *DatabaseUserReconciler) converged(user *databasesv1alpha1.DatabaseUser) bool {
	return user.Status.Phase == "Ready" && user.Status.ObservedGeneration == user.Generation
}

func (r *DatabaseUserReconciler) secretNames(user *databasesv1alpha1.DatabaseUser) []string {
	candidates := []string{user.Status.SecretName}
	if user.Spec.SecretGeneration == "perDatabase" {
		for _, access := range user.Spec.GetDatabases() {
			candidates = append(candidates, r.getSecretNameForDatabase(user, access.Name))
		}
	} else {
		candidates = append(candidates, r.getSecretName(user))
	}
	for _, db := range user.Status.Databases {
		candidates = append(candidates, db.SecretName)
	}

	names := make([]string, 0, len(candidates))
	seen := make(map[string]struct{}, len(candidates))
	for _, name := range candidates {
		if _, dup := seen[name]; dup || name == "" {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}

// granted/accesses and denied/deniedNames are index-parallel.
type userDatabases struct {
	granted     []*databasesv1alpha1.Database
	accesses    []databasesv1alpha1.DatabaseAccess
	denied      []string
	deniedNames []string
}

// A non-nil blocked status means some dependency is not usable yet; denied databases are collected regardless.
func (r *DatabaseUserReconciler) validateAndFetchDatabases(ctx context.Context, user *databasesv1alpha1.DatabaseUser) (
	userDatabases, *databasesv1alpha1.DBCluster, *statusUpdate, error) {

	resolved, blocked, err := r.resolveSpecDatabases(ctx, user)
	if err != nil || blocked != nil || len(resolved.granted) == 0 {
		return resolved, nil, blocked, err
	}

	clusterName := resolved.granted[0].Spec.ClusterRef.Name
	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: clusterName}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return resolved, nil, &statusUpdate{
				Phase: "Pending", Message: fmt.Sprintf("waiting for DBCluster '%s'", clusterName), RequeueAfter: 30 * time.Second,
			}, nil
		}
		return resolved, nil, nil, err
	}

	if cluster.Status.Phase != "Connected" {
		return resolved, nil, &statusUpdate{
			Phase: "Pending", Message: fmt.Sprintf("waiting for DBCluster '%s' to be connected", cluster.Name), RequeueAfter: 20 * time.Second,
		}, nil
	}

	return resolved, &cluster, nil, nil
}

func (r *DatabaseUserReconciler) resolveSpecDatabases(ctx context.Context, user *databasesv1alpha1.DatabaseUser) (
	resolved userDatabases, blocked *statusUpdate, err error) {

	for _, dbAccess := range user.Spec.GetDatabases() {
		var db databasesv1alpha1.Database
		if err := r.Get(ctx, types.NamespacedName{
			Name:      dbAccess.Name,
			Namespace: databaseAccessNamespace(user, dbAccess),
		}, &db); err != nil {
			if !apierrors.IsNotFound(err) {
				return userDatabases{}, nil, err
			}
			if blocked == nil {
				blocked = &statusUpdate{
					Phase: "Pending", Message: fmt.Sprintf("waiting for Database '%s'", dbAccess.Name), RequeueAfter: 30 * time.Second,
				}
			}
			continue
		}

		if err := CheckReferenceConsent(&db, user.Namespace); err != nil {
			resolved.denied = append(resolved.denied, err.Error())
			resolved.deniedNames = append(resolved.deniedNames, DatabaseNameFor(&db))
			continue
		}
		if blocked != nil {
			continue
		}

		if db.Status.Phase != "Ready" {
			blocked = &statusUpdate{
				Phase: "Pending", Message: fmt.Sprintf("waiting for Database '%s' to be ready", db.Name), RequeueAfter: 20 * time.Second,
			}
			continue
		}

		if len(resolved.granted) > 0 && db.Spec.ClusterRef.Name != resolved.granted[0].Spec.ClusterRef.Name {
			first := resolved.granted[0]
			blocked = &statusUpdate{
				Phase:   "Failed",
				Message: fmt.Sprintf("all databases must be on the same cluster: '%s' is on '%s', but '%s' is on '%s'", first.Name, first.Spec.ClusterRef.Name, db.Name, db.Spec.ClusterRef.Name),
			}
			continue
		}

		resolved.granted = append(resolved.granted, &db)
		resolved.accesses = append(resolved.accesses, dbAccess)
	}
	return resolved, blocked, nil
}

// Withdrawn consent is enforced even while another dependency keeps the user from converging.
func (r *DatabaseUserReconciler) withdrawDeniedWhileBlocked(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	deniedNames []string, blocked *statusUpdate) (ctrl.Result, error) {

	keep := make([]string, 0, len(user.Status.Databases))
	for _, db := range user.Status.Databases {
		if db.DatabaseName != "" && !slices.Contains(deniedNames, db.DatabaseName) {
			keep = append(keep, db.DatabaseName)
		}
	}

	remaining, err := r.revokeDatabaseAccess(ctx, user, keep)
	blocked.Databases = remaining
	if err != nil {
		blocked.Phase = "Failed"
		blocked.Message = appendStatusMessage(blocked.Message, err.Error())
		return r.failAndRetry(ctx, user, blocked, err)
	}
	return r.setStatus(ctx, user, blocked)
}

func (r *DatabaseUserReconciler) withdrawAllAccess(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	denied []string) (ctrl.Result, error) {

	remaining, err := r.revokeDatabaseAccess(ctx, user, nil)
	update := &statusUpdate{
		Phase:        "Failed",
		Message:      strings.Join(denied, "; "),
		Databases:    remaining,
		RequeueAfter: r.calculateRequeueAfter(user),
	}
	if err != nil {
		update.Message = appendStatusMessage(update.Message, err.Error())
		return r.failAndRetry(ctx, user, update, err)
	}
	return r.setStatus(ctx, user, update)
}

func (r *DatabaseUserReconciler) revokeDatabaseAccess(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	keep []string) ([]databasesv1alpha1.DatabaseAccessStatus, error) {

	if len(removedDatabaseNames(user, keep)) == 0 || user.Status.ClusterName == "" {
		return statusEntriesFor(user, keep), nil
	}

	var cluster databasesv1alpha1.DBCluster
	if err := r.Get(ctx, types.NamespacedName{Name: user.Status.ClusterName}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return statusEntriesFor(user, keep), nil
		}
		return user.Status.Databases, err
	}
	pgClient, err := GetPostgresClient(ctx, r.Client, r.PGClientCache, &cluster)
	if err != nil {
		return user.Status.Databases, fmt.Errorf("connection error: %w", err)
	}

	return r.revokeFromRole(ctx, pgClient, user, keep)
}

func (r *DatabaseUserReconciler) revokeFromRole(ctx context.Context, pgClient postgres.ClientInterface,
	user *databasesv1alpha1.DatabaseUser, keep []string) ([]databasesv1alpha1.DatabaseAccessStatus, error) {

	if len(removedDatabaseNames(user, keep)) == 0 {
		return statusEntriesFor(user, keep), nil
	}
	username := r.getUsername(user)
	exists, err := pgClient.UserExists(ctx, username)
	if err != nil {
		return user.Status.Databases, fmt.Errorf("failed to check user: %w", err)
	}
	if !exists {
		return statusEntriesFor(user, keep), nil
	}
	return r.revokeRemovedDatabases(ctx, pgClient, user, username, keep)
}

func (r *DatabaseUserReconciler) getUsername(user *databasesv1alpha1.DatabaseUser) string {
	return UsernameForUser(user)
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

func (r *DatabaseUserReconciler) shouldIncludeDatabasesList(user *databasesv1alpha1.DatabaseUser, dbCount int) bool {
	if user.Spec.SecretGeneration == "perDatabase" {
		return false
	}
	if dbCount <= 1 {
		return false
	}
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

// Rejects any foreign controller-ref: SetControllerReference would fail later and re-claim would mis-fire.
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

// Recomputed from spec each reconcile, never inferred from secret.Data.
func (r *DatabaseUserReconciler) buildSecretData(user *databasesv1alpha1.DatabaseUser,
	cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	username, password string) map[string][]byte {

	primaryDB := ""
	if len(databases) > 0 {
		primaryDB = DatabaseNameFor(databases[0])
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
		data["databases"] = []byte(strings.Join(databaseNames(databases), ","))
	}
	return data
}

func (r *DatabaseUserReconciler) buildPerDatabaseSecretData(user *databasesv1alpha1.DatabaseUser,
	cluster *databasesv1alpha1.DBCluster, db *databasesv1alpha1.Database,
	username, password string) map[string][]byte {

	dbName := DatabaseNameFor(db)

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

// Endpoint/DatabaseName aren't pattern-validated; net/url escaping blocks host redirection.
func buildDSN(cluster *databasesv1alpha1.DBCluster, username, password, dbName string) string {
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(username, password),
		Host:   fmt.Sprintf("%s:%d", cluster.Spec.Endpoint, cluster.Spec.Port),
		Path:   "/" + dbName,
	}
	return u.String()
}

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

// Without per-reconcile Merge awareness the cycle would full-replace and degrade Merge into Adopt.
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

// Independent of secret.Data so Secrets without the annotation get stamped.
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

	// Skipped on first creation (ensureUserInPostgres sets it); otherwise PG keeps a stale password.
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

// Sorted: map iteration order would cause spurious Updates.
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

// nil means the annotation is absent; empty means present with zero keys.
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

// A stamped Secret without an OwnerReference lost it to another writer (e.g. ArgoCD).
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
	resolved *userDatabases, cluster *databasesv1alpha1.DBCluster,
	conflicts ownerConflicts) (ctrl.Result, error) {

	databases := resolved.granted
	username := r.getUsername(user)
	dbNames := databaseNames(databases)
	baseStatus := statusUpdate{ClusterName: cluster.Name, Username: username}

	pgClient, err := GetPostgresClient(ctx, r.Client, r.PGClientCache, cluster)
	if err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = fmt.Sprintf("connection error: %s", err.Error())
		baseStatus.RequeueAfter = 60 * time.Second
		return r.setStatus(ctx, user, &baseStatus)
	}

	// Withdrawal must not depend on Secret writes succeeding.
	if err := r.withdrawRemovedAccess(ctx, pgClient, user, dbNames, &baseStatus); err != nil {
		return r.failAndRetry(ctx, user, &baseStatus, err)
	}

	password, secretName, passwordChanged, err := r.ensureSecrets(ctx, user, databases, cluster, pgClient)
	if err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = fmt.Sprintf("secret error: %s", err.Error())
		// Partial rotation must report passwordChanged, else a failed per-DB write loops re-rotation.
		baseStatus.PasswordUpdated = passwordChanged
		baseStatus.SecretName = secretName
		return r.failAndRetry(ctx, user, &baseStatus, err)
	}
	if user.Status.SecretName != "" && user.Status.SecretName != secretName {
		r.deleteOldSecret(ctx, user.Namespace, user.Status.SecretName, user)
	}
	baseStatus.SecretName = secretName

	if err := r.syncPostgresUser(ctx, pgClient, user, username, password, dbNames); err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = err.Error()
		return r.failAndRetry(ctx, user, &baseStatus, err)
	}

	dbStatuses, applyFailures := r.applyPerDatabasePrivileges(ctx, pgClient, user, resolved, conflicts)

	if err := r.syncRuntimeParams(ctx, pgClient, user, username); err != nil {
		baseStatus.Phase = "Failed"
		baseStatus.Message = err.Error()
		return r.failAndRetry(ctx, user, &baseStatus, err)
	}

	r.verifyIsolation(ctx, pgClient, username, dbNames)

	baseStatus.PasswordUpdated = passwordChanged
	baseStatus.Databases = dbStatuses

	if len(applyFailures) > 0 {
		msg := fmt.Sprintf("failed to apply privileges for database(s): %s", strings.Join(applyFailures, ", "))
		baseStatus.Phase = "Failed"
		baseStatus.Message = msg
		return r.failAndRetry(ctx, user, &baseStatus, errors.New(msg))
	}

	// Recovery arrives through the peer and Database watches; no error, so the user waits without backoff.
	if messages := slices.Concat(resolved.denied, conflicts.messages()); len(messages) > 0 {
		baseStatus.Phase = "Failed"
		baseStatus.Message = strings.Join(messages, "; ")
		baseStatus.RequeueAfter = r.calculateRequeueAfter(user)
		return r.setStatus(ctx, user, &baseStatus)
	}

	log.FromContext(ctx).Info("user ready", "username", username, "databases", len(databases))

	baseStatus.Phase = "Ready"
	baseStatus.Message = fmt.Sprintf("user created with access to %d database(s)", len(databases))
	baseStatus.RequeueAfter = r.calculateRequeueAfter(user)
	return r.setStatus(ctx, user, &baseStatus)
}

func databaseNames(databases []*databasesv1alpha1.Database) []string {
	names := make([]string, len(databases))
	for i, db := range databases {
		names[i] = DatabaseNameFor(db)
	}
	return names
}

func (r *DatabaseUserReconciler) withdrawRemovedAccess(ctx context.Context, pgClient postgres.ClientInterface,
	user *databasesv1alpha1.DatabaseUser, keep []string, update *statusUpdate) error {

	remaining, err := r.revokeFromRole(ctx, pgClient, user, keep)
	update.Databases = remaining
	if err != nil {
		update.Phase = "Failed"
		update.Message = err.Error()
	}
	return err
}

func (r *DatabaseUserReconciler) failAndRetry(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	update *statusUpdate, cause error) (ctrl.Result, error) {

	if _, err := r.setStatus(ctx, user, update); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, cause
}

func (r *DatabaseUserReconciler) syncPostgresUser(ctx context.Context, pgClient postgres.ClientInterface,
	user *databasesv1alpha1.DatabaseUser, username, password string, dbNames []string) error {

	// A converged user skips ALTER ROLE PASSWORD: the periodic reverify must not touch credentials.
	if err := r.ensureUserInPostgres(ctx, pgClient, username, password, !r.converged(user)); err != nil {
		return err
	}
	if err := pgClient.SyncDatabaseAccess(ctx, username, dbNames); err != nil {
		return fmt.Errorf("failed to sync database access: %w", err)
	}
	return nil
}

func (r *DatabaseUserReconciler) applyPerDatabasePrivileges(ctx context.Context, pgClient postgres.ClientInterface,
	user *databasesv1alpha1.DatabaseUser, resolved *userDatabases,
	conflicts ownerConflicts) (dbStatuses []databasesv1alpha1.DatabaseAccessStatus, applyFailures []string) {

	dbStatuses = make([]databasesv1alpha1.DatabaseAccessStatus, len(resolved.granted))
	dbAccesses := resolved.accesses

	for i, db := range resolved.granted {
		dbName := DatabaseNameFor(db)
		grants := ResolveUserGrants(user, dbAccesses[i])
		lost := conflicts.lossFor(db.Namespace, db.Name)
		if lost != "" {
			grants = LoserGrants(grants)
		}

		status := databasesv1alpha1.DatabaseAccessStatus{
			Name:         dbAccesses[i].Name,
			Namespace:    dbAccesses[i].Namespace,
			DatabaseName: dbName,
			Privileges:   grants.Privileges,
			Phase:        "Ready",
		}
		if lost != "" {
			status.Phase = "Failed"
			status.Message = lost
		}
		ownerRole, err := r.electedOwnerRole(ctx, user, db)
		if err == nil {
			err = ApplyUserGrants(ctx, pgClient, dbName, grants, ownerRole)
		}
		if err != nil {
			status.Phase = "Failed"
			status.Message = appendStatusMessage(status.Message, err.Error())
			applyFailures = append(applyFailures, dbName)
		}
		if user.Spec.SecretGeneration == "perDatabase" {
			status.SecretName = r.getSecretNameForDatabase(user, db.Name)
		}
		dbStatuses[i] = status
	}
	return dbStatuses, applyFailures
}

func appendStatusMessage(existing, addition string) string {
	if existing == "" {
		return addition
	}
	return existing + "; " + addition
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

// Read-before-write; CRD blocks ConnectionLimit=0, so zero means unset (-1, PG default).
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

// Read-before-write so RESET isn't sent every reconcile.
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
	if rotation := r.rotationRequeueAfter(user); rotation > 0 && rotation < ReverifyInterval {
		return rotation
	}
	return ReverifyInterval
}

func (r *DatabaseUserReconciler) rotationRequeueAfter(user *databasesv1alpha1.DatabaseUser) time.Duration {
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
		return time.Minute
	}
	return requeue
}

func (r *DatabaseUserReconciler) ensureUserInPostgres(ctx context.Context, pgClient postgres.ClientInterface,
	username, password string, syncPassword bool) error {

	exists, err := pgClient.UserExists(ctx, username)
	if err != nil {
		return fmt.Errorf("failed to check user: %w", err)
	}

	if exists {
		if !syncPassword {
			return nil
		}
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

	expected := make(map[string]bool, len(expectedDatabases))
	for _, db := range expectedDatabases {
		expected[db] = true
	}

	for _, db := range accessibleDatabases {
		if !expected[db] {
			logger.V(1).Info("user has access to unexpected database (may be inherited from PUBLIC role)",
				"database", db, "expected", expectedDatabases)
		}
	}
}

// An owned Secret keeps its password; only a missing or corrupt one is regenerated.
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
	// Merge-policy secrets stay on the overlay path: full-replace would drop foreign keys.
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

// Heals an unowned-but-managed secret in place (annotation matches, no foreign controller).
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

// Soft-owned re-claim for perDatabase, where Adopt/Merge isn't supported; otherwise fails.
func (r *DatabaseUserReconciler) reclaimOrFail(ctx context.Context, user *databasesv1alpha1.DatabaseUser, secret *corev1.Secret) error {
	if !r.isSecretSoftOwnedByUser(secret, user) {
		return fmt.Errorf("secret %s already exists and is not owned by this DatabaseUser", secret.Name)
	}
	return r.doSoftReclaim(ctx, user, secret)
}

// Done=false means the secret was self-healed and the caller continues the normal sync path.
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

	// PG already rotated: bump PasswordUpdatedAt even on partial failure, or rotation re-fires each reconcile.
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

// Single Update: claim-then-data would leave an owned secret holding the OLD password while PG has the NEW one.
func (r *DatabaseUserReconciler) adoptSecret(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	secret *corev1.Secret, cluster *databasesv1alpha1.DBCluster, databases []*databasesv1alpha1.Database,
	pgClient postgres.ClientInterface, username string) (password, secretName string, passwordChanged bool, err error) {

	log.FromContext(ctx).Info("adopting existing secret", "secret", secret.Name)
	r.warnIfOwnershipStripped(ctx, secret, user, "adopt")

	// Claim first so a foreign controller fails us before PostgreSQL is touched.
	if err = controllerutil.SetControllerReference(user, secret, r.Scheme); err != nil {
		return "", "", false, fmt.Errorf("failed to set controller reference: %w", err)
	}
	password, err = r.generateAndSetPassword(ctx, user, pgClient, username)
	if err != nil {
		return "", "", false, err
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

	// Claim first so a foreign controller fails us before PostgreSQL is touched.
	if err = controllerutil.SetControllerReference(user, secret, r.Scheme); err != nil {
		return "", "", false, fmt.Errorf("failed to set controller reference: %w", err)
	}
	password, err = r.generateAndSetPassword(ctx, user, pgClient, username)
	if err != nil {
		return "", "", false, err
	}
	if secret.Annotations == nil {
		secret.Annotations = map[string]string{}
	}
	secret.Annotations[ConflictPolicyAnnotation] = "Merge"

	// Foreign keys stay; managed-keys lets later reconciles drop our orphans.
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
			// Keep the finalizer: the CR must not disappear while the PG role still exists.
			return ctrl.Result{}, err
		}
	} else {
		logger.Info("retaining user in PostgreSQL due to deletionPolicy", "username", username)
		if err := r.releaseSecrets(ctx, user); err != nil {
			return ctrl.Result{}, err
		}
	}

	patch := client.MergeFrom(user.DeepCopy())
	controllerutil.RemoveFinalizer(user, UserFinalizerName)
	return ctrl.Result{}, r.Patch(ctx, user, patch)
}

// Garbage collection would otherwise delete the only copy of the retained role's password.
func (r *DatabaseUserReconciler) releaseSecrets(ctx context.Context, user *databasesv1alpha1.DatabaseUser) error {
	for _, name := range r.secretNames(user) {
		var secret corev1.Secret
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: user.Namespace}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("get secret %s: %w", name, err)
		}
		kept := make([]metav1.OwnerReference, 0, len(secret.OwnerReferences))
		for _, ref := range secret.OwnerReferences {
			if ref.Kind != "DatabaseUser" || ref.UID != user.UID {
				kept = append(kept, ref)
			}
		}
		if len(kept) == len(secret.OwnerReferences) {
			continue
		}
		secret.OwnerReferences = kept
		if err := r.Update(ctx, &secret); err != nil {
			return fmt.Errorf("release secret %s: %w", name, err)
		}
		log.FromContext(ctx).Info("released secret of retained user", "secret", name)
	}
	return nil
}

func (r *DatabaseUserReconciler) dropUserFromPostgres(ctx context.Context, user *databasesv1alpha1.DatabaseUser,
	username string) error {

	logger := log.FromContext(ctx)

	clusterName, databaseNames := r.getClusterAndDatabasesForDeletion(ctx, user)
	if clusterName == "" {
		// No resolvable cluster: cleanup is impossible by design, so release the finalizer.
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

	// Best-effort: if the role still owns objects, DropUser below fails and gates the finalizer.
	if exists, err := pgClient.UserExists(ctx, username); err != nil {
		logger.Error(err, "failed to check whether the role still exists", "username", username)
	} else if exists {
		if err := pgClient.EnsureRoleMembership(ctx, username); err != nil {
			logger.Error(err, "failed to ensure role membership before reassigning ownership", "username", username)
		}
	}
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
	if user.Status.ClusterName != "" {
		databaseNames = make([]string, len(user.Status.Databases))
		for i, db := range user.Status.Databases {
			databaseNames[i] = db.DatabaseName
		}
		return user.Status.ClusterName, databaseNames
	}

	dbAccesses := user.Spec.GetDatabases()
	if len(dbAccesses) == 0 {
		return "", nil
	}

	for _, dbAccess := range dbAccesses {
		var db databasesv1alpha1.Database
		if err := r.Get(ctx, types.NamespacedName{
			Name:      dbAccess.Name,
			Namespace: databaseAccessNamespace(user, dbAccess),
		}, &db); err != nil {
			continue
		}
		if CheckReferenceConsent(&db, user.Namespace) != nil {
			continue
		}

		if clusterName == "" {
			clusterName = db.Spec.ClusterRef.Name
		}
		databaseNames = append(databaseNames, DatabaseNameFor(&db))
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

func (r *DatabaseUserReconciler) revokeRemovedDatabases(ctx context.Context, pgClient postgres.ClientInterface,
	user *databasesv1alpha1.DatabaseUser, username string, keep []string) ([]databasesv1alpha1.DatabaseAccessStatus, error) {

	uncleaned, reassignErr := reassignRemovedOwnership(ctx, pgClient, username, removedDatabaseNames(user, keep))
	// DROP OWNED in a removed database also revokes CONNECT on the kept ones, so the sync runs even after a failed reassignment.
	if err := pgClient.SyncDatabaseAccess(ctx, username, keep); err != nil {
		return user.Status.Databases, errors.Join(reassignErr, fmt.Errorf("failed to sync database access: %w", err))
	}
	return statusEntriesFor(user, slices.Concat(keep, uncleaned)), reassignErr
}

func reassignRemovedOwnership(ctx context.Context, pgClient postgres.ClientInterface,
	username string, removed []string) (failed []string, err error) {

	if len(removed) == 0 {
		return nil, nil
	}
	if err := pgClient.EnsureRoleMembership(ctx, username); err != nil {
		return removed, fmt.Errorf("failed to ensure role membership before reassigning ownership: %w", err)
	}

	var errs []error
	for _, dbName := range removed {
		log.FromContext(ctx).Info("reassigning ownership for removed database", "database", dbName, "username", username)
		if err := pgClient.ReassignOwnership(ctx, username, dbName); err != nil {
			failed = append(failed, dbName)
			errs = append(errs, fmt.Errorf("failed to reassign ownership in removed database %s: %w", dbName, err))
		}
	}
	return failed, errors.Join(errs...)
}

func removedDatabaseNames(user *databasesv1alpha1.DatabaseUser, keep []string) []string {
	var removed []string
	for _, db := range user.Status.Databases {
		if db.DatabaseName != "" && !slices.Contains(keep, db.DatabaseName) {
			removed = append(removed, db.DatabaseName)
		}
	}
	return removed
}

func statusEntriesFor(user *databasesv1alpha1.DatabaseUser, names []string) []databasesv1alpha1.DatabaseAccessStatus {
	entries := make([]databasesv1alpha1.DatabaseAccessStatus, 0, len(user.Status.Databases))
	for _, db := range user.Status.Databases {
		if slices.Contains(names, db.DatabaseName) {
			entries = append(entries, db)
		}
	}
	return entries
}

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
	// Snapshot first: handlePendingTimeout's PendingSince must be part of the patch diff.
	patch := client.MergeFrom(user.DeepCopy())

	pendingChanged := r.handlePendingTimeout(user, update)

	if pendingChanged || statusDiffers(user, update) {
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

func statusDiffers(user *databasesv1alpha1.DatabaseUser, update *statusUpdate) bool {
	return user.Status.Phase != update.Phase ||
		user.Status.Message != update.Message ||
		user.Status.ObservedGeneration != user.Generation ||
		(update.ClusterName != "" && user.Status.ClusterName != update.ClusterName) ||
		(update.Username != "" && user.Status.Username != update.Username) ||
		(update.SecretName != "" && user.Status.SecretName != update.SecretName) ||
		update.PasswordUpdated ||
		(update.Phase == "Ready" && user.Status.PasswordUpdatedAt == nil) ||
		(update.Databases != nil && !equality.Semantic.DeepEqual(user.Status.Databases, update.Databases))
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
			// Reset so a later Pending episode does not time out immediately.
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
	if update.Databases != nil {
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
	if err := RegisterIndexers(context.Background(), mgr); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&databasesv1alpha1.DatabaseUser{}).
		Owns(&corev1.Secret{}).
		Watches(&databasesv1alpha1.DatabaseUser{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueOwnerPeers),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&databasesv1alpha1.DBCluster{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueClusterUsers),
			builder.WithPredicates(dbClusterChanged)).
		Watches(&databasesv1alpha1.Database{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueDatabaseUsers),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}

func (r *DatabaseUserReconciler) enqueueDatabaseUsers(ctx context.Context, obj client.Object) []ctrl.Request {
	requests, err := r.usersReferencing(ctx, obj.GetNamespace(), obj.GetName(), map[types.NamespacedName]struct{}{})
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to enqueue DatabaseUsers of database", "database", obj.GetNamespace()+"/"+obj.GetName())
	}
	return requests
}

func (r *DatabaseUserReconciler) usersReferencing(ctx context.Context, namespace, name string,
	seen map[types.NamespacedName]struct{}) ([]ctrl.Request, error) {

	var users databasesv1alpha1.DatabaseUserList
	if err := r.List(ctx, &users, client.MatchingFields{
		DatabaseUserDatabaseRefIndex: DatabaseUserDatabaseRefKey(namespace, name),
	}); err != nil {
		return nil, err
	}
	requests := make([]ctrl.Request, 0, len(users.Items))
	for i := range users.Items {
		key := types.NamespacedName{Namespace: users.Items[i].Namespace, Name: users.Items[i].Name}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		requests = append(requests, ctrl.Request{NamespacedName: key})
	}
	return requests, nil
}

func (r *DatabaseUserReconciler) enqueueClusterUsers(ctx context.Context, obj client.Object) []ctrl.Request {
	databases, err := databasesOnCluster(ctx, r.Client, obj.GetName())
	if err != nil {
		log.FromContext(ctx).Error(err, "failed to enqueue DatabaseUsers of cluster", "cluster", obj.GetName())
		return nil
	}

	var requests []ctrl.Request
	seen := make(map[types.NamespacedName]struct{})
	for i := range databases {
		db := &databases[i]
		users, err := r.usersReferencing(ctx, db.Namespace, db.Name, seen)
		if err != nil {
			log.FromContext(ctx).Error(err, "failed to enqueue DatabaseUsers of database", "database", db.Namespace+"/"+db.Name)
			continue
		}
		requests = append(requests, users...)
	}
	return requests
}
