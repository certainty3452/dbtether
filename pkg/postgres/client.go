package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"
)

// Typed aliases force callsites to cast; allowlists below enforce the type at runtime.
type RoleParam string
type TablePrivilege string

const (
	RoleParamIdleInTransactionTimeout RoleParam = "idle_in_transaction_session_timeout"
)

var allowedRoleParams = map[RoleParam]struct{}{
	RoleParamIdleInTransactionTimeout: {},
}

var allowedTablePrivileges = map[TablePrivilege]struct{}{
	"SELECT":     {},
	"INSERT":     {},
	"UPDATE":     {},
	"DELETE":     {},
	"TRUNCATE":   {},
	"REFERENCES": {},
	"TRIGGER":    {},
	"USAGE":      {},
}

var (
	ErrInvalidTablePrivilege = errors.New("invalid table privilege")
	ErrInvalidRoleParam      = errors.New("invalid role parameter")
)

type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	Database string
	SSLMode  string
}

type Client struct {
	config Config
	pool   *pgxpool.Pool
}

// ClientInterface defines all PostgreSQL operations for mocking in tests
type ClientInterface interface {
	Close()
	Ping(ctx context.Context) error
	GetVersion(ctx context.Context) (string, error)
	DatabaseExists(ctx context.Context, name string) (bool, error)
	CreateDatabaseWithOwner(ctx context.Context, name, ownerNamespace, ownerName string) error
	EnsureDatabaseWithOwner(ctx context.Context, name, ownerNamespace, ownerName string, forceAdopt bool) (ownershipTracked bool, err error)
	GetDatabaseOwner(ctx context.Context, name string) (namespace, resourceName string, err error)
	ClearDatabaseOwner(ctx context.Context, name string) error
	DropDatabase(ctx context.Context, name string) error
	RevokePublicConnect(ctx context.Context, name string) error
	CreateExtension(ctx context.Context, dbName, extensionName string) error
	EnsureExtensions(ctx context.Context, dbName string, extensions []string) error
	UserExists(ctx context.Context, username string) (bool, error)
	CreateUser(ctx context.Context, username, password string) error
	SetPassword(ctx context.Context, username, password string) error
	GetConnectionLimit(ctx context.Context, username string) (int, error)
	SetConnectionLimit(ctx context.Context, username string, limit int) error
	GetRoleParameter(ctx context.Context, username string, key RoleParam) (value string, exists bool, err error)
	SetRoleParameter(ctx context.Context, username string, key RoleParam, value string) error
	ResetRoleParameter(ctx context.Context, username string, key RoleParam) error
	DropUser(ctx context.Context, username string) error
	RevokeAllDatabaseAccess(ctx context.Context, username string) error
	GrantDatabaseAccess(ctx context.Context, username, database string) error
	RevokeDatabaseAccess(ctx context.Context, username, database string) error
	GetUserDatabaseAccess(ctx context.Context, username string) ([]string, error)
	SyncDatabaseAccess(ctx context.Context, username string, allowedDatabases []string) error
	ApplyPrivileges(ctx context.Context, username, database, preset string, additionalGrants []TableGrant) error
	VerifyDatabaseIsolation(ctx context.Context, username, allowedDatabase string) ([]string, error)
	RevokePrivilegesInDatabase(ctx context.Context, username, database string) error
	ReassignOwnership(ctx context.Context, fromUser, database string) error
	EnsureRoleMembership(ctx context.Context, username string) error
}

// Ensure Client implements ClientInterface
var _ ClientInterface = (*Client)(nil)

// ClientCacheInterface allows mocking the client cache in tests
type ClientCacheInterface interface {
	Get(ctx context.Context, clusterName string, config Config) (ClientInterface, error)
	Remove(clusterName string)
	Close()
}

type ClientCache struct {
	clients map[string]*Client
	mu      sync.RWMutex
}

// Ensure ClientCache implements ClientCacheInterface
var _ ClientCacheInterface = (*ClientCache)(nil)

func NewClientCache() *ClientCache {
	return &ClientCache{
		clients: make(map[string]*Client),
	}
}

func (c *ClientCache) Get(ctx context.Context, clusterName string, config Config) (ClientInterface, error) {
	c.mu.RLock()
	cached, ok := c.clients[clusterName]
	c.mu.RUnlock()

	if ok {
		if err := cached.Ping(ctx); err == nil {
			return cached, nil
		}
		c.Remove(clusterName)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Re-check under the write lock: a concurrent Get on a cold cache would
	// otherwise store its own pool over ours and leak the loser's connections.
	if cached, ok := c.clients[clusterName]; ok {
		return cached, nil
	}

	client, err := NewClient(ctx, config)
	if err != nil {
		return nil, err
	}
	c.clients[clusterName] = client

	return client, nil
}

func (c *ClientCache) Remove(clusterName string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if client, ok := c.clients[clusterName]; ok {
		client.Close()
		delete(c.clients, clusterName)
	}
}

func (c *ClientCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	for name, client := range c.clients {
		client.Close()
		delete(c.clients, name)
	}
}

func NewClient(ctx context.Context, config Config) (*Client, error) {
	if config.Database == "" {
		config.Database = "postgres"
	}
	if config.Port == 0 {
		config.Port = 5432
	}
	if config.SSLMode == "" {
		config.SSLMode = "require"
	}

	connString := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		config.Host, config.Port, config.Username, config.Password, config.Database, config.SSLMode,
	)

	poolConfig, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection string: %w", err)
	}

	poolConfig.MaxConns = 5
	poolConfig.MinConns = 1
	poolConfig.MaxConnLifetime = 30 * time.Minute
	poolConfig.MaxConnIdleTime = 5 * time.Minute
	poolConfig.HealthCheckPeriod = 1 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	return &Client{config: config, pool: pool}, nil
}

func (c *Client) Close() {
	if c.pool != nil {
		c.pool.Close()
	}
}

func (c *Client) Ping(ctx context.Context) error {
	return c.pool.Ping(ctx)
}

func (c *Client) GetVersion(ctx context.Context) (string, error) {
	var version string
	err := c.pool.QueryRow(ctx, "SELECT version()").Scan(&version)
	if err != nil {
		return "", fmt.Errorf("failed to get version: %w", err)
	}
	return version, nil
}

func (c *Client) DatabaseExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	err := c.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)",
		name,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check database existence: %w", err)
	}
	return exists, nil
}

// ownerComment formats the ownership comment for database metadata
func ownerComment(namespace, name string) string {
	return fmt.Sprintf("dbtether:%s/%s", namespace, name)
}

// parseOwnerComment extracts namespace and name from ownership comment
func parseOwnerComment(comment string) (namespace, name string, ok bool) {
	if !strings.HasPrefix(comment, "dbtether:") {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(comment, "dbtether:"), "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (c *Client) CreateDatabaseWithOwner(ctx context.Context, name, ownerNamespace, ownerName string) error {
	query := fmt.Sprintf("CREATE DATABASE %s", pq.QuoteIdentifier(name))
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to create database %s: %w", name, err)
	}

	// Set ownership comment (best-effort — should work for newly created databases)
	comment := ownerComment(ownerNamespace, ownerName)
	commentQuery := fmt.Sprintf("COMMENT ON DATABASE %s IS %s",
		pq.QuoteIdentifier(name), pq.QuoteLiteral(comment))
	// Ignore error — ownership tracking is optional enhancement
	_, _ = c.pool.Exec(ctx, commentQuery)

	return nil
}

func (c *Client) GetDatabaseOwner(ctx context.Context, name string) (namespace, resourceName string, err error) {
	query := `SELECT COALESCE(description, '') FROM pg_database d 
		LEFT JOIN pg_shdescription s ON d.oid = s.objoid 
		WHERE datname = $1`
	var comment string
	if err := c.pool.QueryRow(ctx, query, name).Scan(&comment); err != nil {
		return "", "", fmt.Errorf("failed to get database comment: %w", err)
	}
	if comment == "" {
		return "", "", nil // no owner set (legacy database)
	}
	ns, n, ok := parseOwnerComment(comment)
	if !ok {
		return "", "", nil // comment exists but not in our format
	}
	return ns, n, nil
}

// A REVOKE by a role that does not own the database answers with a warning and no error, and a NULL datacl is the default ACL, which grants PUBLIC CONNECT.
const publicConnectGrantedQuery = `
	SELECT EXISTS (
		SELECT 1 FROM pg_database d
		WHERE d.datname = $1
		  AND (d.datacl IS NULL
		       OR EXISTS (SELECT 1 FROM aclexplode(d.datacl) a
		                  WHERE a.grantee = 0 AND a.privilege_type = 'CONNECT'))
	)`

func (c *Client) RevokePublicConnect(ctx context.Context, name string) error {
	query := fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", pq.QuoteIdentifier(name))
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to revoke public connect on %s: %w", name, err)
	}

	var stillGranted bool
	if err := c.pool.QueryRow(ctx, publicConnectGrantedQuery, name).Scan(&stillGranted); err != nil {
		return fmt.Errorf("failed to verify public connect on %s: %w", name, err)
	}
	if stillGranted {
		return fmt.Errorf("public connect on database %s could not be revoked (not owner)", name)
	}
	return nil
}

func (c *Client) EnsureDatabaseWithOwner(ctx context.Context, name, ownerNamespace, ownerName string, forceAdopt bool) (ownershipTracked bool, err error) {
	exists, err := c.DatabaseExists(ctx, name)
	if err != nil {
		return false, err
	}
	if !exists {
		err := c.CreateDatabaseWithOwner(ctx, name, ownerNamespace, ownerName)
		return err == nil, err // new DB = ownership tracked if created successfully
	}

	// Database exists — check ownership
	ns, n, err := c.GetDatabaseOwner(ctx, name)
	if err != nil {
		return false, err
	}

	expectedOwner := ownerComment(ownerNamespace, ownerName)

	// No owner set (legacy) or forceAdopt — try to claim it (best-effort)
	if ns == "" && n == "" || forceAdopt {
		commentQuery := fmt.Sprintf("COMMENT ON DATABASE %s IS %s",
			pq.QuoteIdentifier(name), pq.QuoteLiteral(expectedOwner))
		if _, err := c.pool.Exec(ctx, commentQuery); err != nil {
			// COMMENT requires being PostgreSQL owner of the database
			// For legacy databases created by other users, this will fail — that's OK
			// We continue without ownership tracking for such databases
			return false, nil // best-effort: skip ownership tracking for legacy databases
		}
		return true, nil // ownership claimed successfully
	}

	// Check if we are the owner
	if ns != ownerNamespace || n != ownerName {
		return false, fmt.Errorf("database %s is owned by %s/%s, cannot be claimed by %s/%s (use annotation dbtether.io/force-adopt to override)",
			name, ns, n, ownerNamespace, ownerName)
	}

	return true, nil // already owned by us
}

func (c *Client) ClearDatabaseOwner(ctx context.Context, name string) error {
	// Set comment to NULL to release ownership
	query := fmt.Sprintf("COMMENT ON DATABASE %s IS NULL", pq.QuoteIdentifier(name))
	if _, err := c.pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("failed to clear ownership of database %s: %w", name, err)
	}
	return nil
}

func (c *Client) DropDatabase(ctx context.Context, name string) error {
	// Terminate active connections before dropping
	terminateQuery := fmt.Sprintf(`
		SELECT pg_terminate_backend(pid) 
		FROM pg_stat_activity 
		WHERE datname = %s AND pid <> pg_backend_pid()
	`, pq.QuoteLiteral(name))
	_, _ = c.pool.Exec(ctx, terminateQuery) // best-effort: ignore errors

	query := fmt.Sprintf("DROP DATABASE IF EXISTS %s", pq.QuoteIdentifier(name))
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to drop database %s: %w", name, err)
	}
	return nil
}

func (c *Client) CreateExtension(ctx context.Context, dbName, extensionName string) error {
	conn, err := c.connectToDatabase(ctx, dbName)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }() // error on close is not actionable

	query := fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s", pq.QuoteIdentifier(extensionName))
	_, err = conn.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to create extension %s: %w", extensionName, err)
	}
	return nil
}

func (c *Client) EnsureExtensions(ctx context.Context, dbName string, extensions []string) error {
	if len(extensions) == 0 {
		return nil
	}

	conn, err := c.connectToDatabase(ctx, dbName)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }() // error on close is not actionable

	for _, ext := range extensions {
		query := fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s", pq.QuoteIdentifier(ext))
		if _, err = conn.Exec(ctx, query); err != nil {
			return fmt.Errorf("failed to create extension %s: %w", ext, err)
		}
	}
	return nil
}

func (c *Client) connectToDatabase(ctx context.Context, dbName string) (*pgx.Conn, error) {
	connString := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=%s",
		c.config.Host, c.config.Port, c.config.Username, c.config.Password, dbName, c.config.SSLMode,
	)
	conn, err := pgx.Connect(ctx, connString)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database %s: %w", dbName, err)
	}
	return conn, nil
}

func IsTransientError(err error) bool {
	// All connection errors are considered transient for retry purposes
	return err != nil
}

// isDatabaseNotExistError checks if error is PostgreSQL "database does not exist" (SQLSTATE 3D000)
func isDatabaseNotExistError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "3D000"
	}
	return false
}

// isLockNotAvailableError checks if error is PostgreSQL "lock not available" (SQLSTATE 55P03)
func isLockNotAvailableError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "55P03"
	}
	return false
}

// User management methods

func (c *Client) UserExists(ctx context.Context, username string) (bool, error) {
	var exists bool
	err := c.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)",
		username,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check user existence: %w", err)
	}
	return exists, nil
}

func (c *Client) CreateUser(ctx context.Context, username, password string) error {
	query := fmt.Sprintf(
		"CREATE USER %s WITH PASSWORD %s NOCREATEDB NOCREATEROLE NOINHERIT",
		pq.QuoteIdentifier(username),
		pq.QuoteLiteral(password),
	)
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to create user %s: %w", username, err)
	}
	return nil
}

func (c *Client) SetPassword(ctx context.Context, username, password string) error {
	query := fmt.Sprintf(
		"ALTER USER %s WITH PASSWORD %s",
		pq.QuoteIdentifier(username),
		pq.QuoteLiteral(password),
	)
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to set password for user %s: %w", username, err)
	}
	return nil
}

func (c *Client) GetConnectionLimit(ctx context.Context, username string) (int, error) {
	var limit int
	if err := c.pool.QueryRow(ctx,
		`SELECT rolconnlimit FROM pg_roles WHERE rolname = $1`,
		username,
	).Scan(&limit); err != nil {
		return 0, fmt.Errorf("failed to read connection limit for %s: %w", username, err)
	}
	return limit, nil
}

func (c *Client) SetConnectionLimit(ctx context.Context, username string, limit int) error {
	query := fmt.Sprintf(
		"ALTER USER %s CONNECTION LIMIT %d",
		pq.QuoteIdentifier(username),
		limit,
	)
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to set connection limit for user %s: %w", username, err)
	}
	return nil
}

func (c *Client) GetRoleParameter(ctx context.Context, username string, key RoleParam) (value string, exists bool, err error) {
	if _, ok := allowedRoleParams[key]; !ok {
		return "", false, fmt.Errorf("%w: %q", ErrInvalidRoleParam, key)
	}
	var rolconfig []string
	if scanErr := c.pool.QueryRow(ctx,
		`SELECT COALESCE(rolconfig, '{}'::text[]) FROM pg_roles WHERE rolname = $1`,
		username,
	).Scan(&rolconfig); scanErr != nil {
		return "", false, fmt.Errorf("failed to read role config for %s: %w", username, scanErr)
	}
	for _, entry := range rolconfig {
		k, v, ok := strings.Cut(entry, "=")
		if ok && k == string(key) {
			return v, true, nil
		}
	}
	return "", false, nil
}

func (c *Client) SetRoleParameter(ctx context.Context, username string, key RoleParam, value string) error {
	if _, ok := allowedRoleParams[key]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidRoleParam, key)
	}
	query := fmt.Sprintf(
		"ALTER ROLE %s SET %s = %s",
		pq.QuoteIdentifier(username),
		string(key),
		pq.QuoteLiteral(value),
	)
	if _, err := c.pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("failed to set role parameter %s on %s: %w", key, username, err)
	}
	return nil
}

func (c *Client) ResetRoleParameter(ctx context.Context, username string, key RoleParam) error {
	if _, ok := allowedRoleParams[key]; !ok {
		return fmt.Errorf("%w: %q", ErrInvalidRoleParam, key)
	}
	query := fmt.Sprintf(
		"ALTER ROLE %s RESET %s",
		pq.QuoteIdentifier(username),
		string(key),
	)
	if _, err := c.pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("failed to reset role parameter %s on %s: %w", key, username, err)
	}
	return nil
}

func (c *Client) DropUser(ctx context.Context, username string) error {
	query := fmt.Sprintf("DROP USER IF EXISTS %s", pq.QuoteIdentifier(username))
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to drop user %s: %w", username, err)
	}
	return nil
}

func (c *Client) RevokeAllDatabaseAccess(ctx context.Context, username string) error {
	rows, err := c.pool.Query(ctx, "SELECT datname FROM pg_database WHERE datistemplate = false")
	if err != nil {
		return fmt.Errorf("failed to list databases: %w", err)
	}
	defer rows.Close()

	quotedUser := pq.QuoteIdentifier(username)
	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			continue
		}
		query := fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM %s",
			pq.QuoteIdentifier(dbName), quotedUser)
		_, _ = c.pool.Exec(ctx, query) // best-effort: some DBs may not allow revoke
	}
	return nil
}

func (c *Client) GrantDatabaseAccess(ctx context.Context, username, database string) error {
	query := fmt.Sprintf(
		"GRANT CONNECT ON DATABASE %s TO %s",
		pq.QuoteIdentifier(database),
		pq.QuoteIdentifier(username),
	)
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to grant database access: %w", err)
	}
	return nil
}

// RevokeDatabaseAccess revokes CONNECT privilege on a single database
func (c *Client) RevokeDatabaseAccess(ctx context.Context, username, database string) error {
	query := fmt.Sprintf(
		"REVOKE CONNECT ON DATABASE %s FROM %s",
		pq.QuoteIdentifier(database),
		pq.QuoteIdentifier(username),
	)
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to revoke database access on %s: %w", database, err)
	}
	return nil
}

// GetUserDatabaseAccess returns list of databases where user has CONNECT privilege
func (c *Client) GetUserDatabaseAccess(ctx context.Context, username string) ([]string, error) {
	query := `
		SELECT datname FROM pg_database 
		WHERE datistemplate = false 
		AND has_database_privilege($1, datname, 'CONNECT')
	`
	rows, err := c.pool.Query(ctx, query, username)
	if err != nil {
		return nil, fmt.Errorf("failed to get user database access: %w", err)
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var db string
		if err := rows.Scan(&db); err != nil {
			return nil, err
		}
		databases = append(databases, db)
	}
	return databases, nil
}

// SyncDatabaseAccess ensures user has access ONLY to the specified databases.
// It grants access to allowed databases and revokes access from all others.
func (c *Client) SyncDatabaseAccess(ctx context.Context, username string, allowedDatabases []string) error {
	// Get all databases where user currently has CONNECT
	currentAccess, err := c.GetUserDatabaseAccess(ctx, username)
	if err != nil {
		return err
	}

	// Build allowed set for O(1) lookup
	allowed := make(map[string]bool, len(allowedDatabases))
	for _, db := range allowedDatabases {
		allowed[db] = true
	}

	// Revoke access to databases NOT in allowed list (best-effort)
	for _, db := range currentAccess {
		if !allowed[db] {
			// Best-effort: log but continue on error
			_ = c.RevokeDatabaseAccess(ctx, username, db)
		}
	}

	// Grant access to allowed databases
	for _, db := range allowedDatabases {
		if err := c.GrantDatabaseAccess(ctx, username, db); err != nil {
			return fmt.Errorf("failed to grant access to %s: %w", db, err)
		}
	}

	return nil
}

func (c *Client) ApplyPrivileges(ctx context.Context, username, database, preset string, additionalGrants []TableGrant) error {
	conn, err := c.connectToDatabase(ctx, database)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }() // error on close is not actionable

	quotedUser := pq.QuoteIdentifier(username)

	// Revoke all first for clean state (best-effort)
	_, _ = conn.Exec(ctx, fmt.Sprintf("REVOKE ALL ON SCHEMA public FROM %s", quotedUser)) // may fail if no grants exist

	// Grant USAGE on schema
	if _, err = conn.Exec(ctx, fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", quotedUser)); err != nil {
		return fmt.Errorf("failed to grant schema usage: %w", err)
	}

	switch preset {
	case "readonly":
		if err := c.applyReadonlyPrivileges(ctx, conn, quotedUser); err != nil {
			return err
		}
	case "readwrite":
		if err := c.applyReadwritePrivileges(ctx, conn, quotedUser); err != nil {
			return err
		}
	case "admin":
		if err := c.applyAdminPrivileges(ctx, conn, quotedUser); err != nil {
			return err
		}
	case "owner":
		if err := c.applyOwnerPrivileges(ctx, conn, username); err != nil {
			return err
		}
	}

	// Apply additional grants
	for _, grant := range additionalGrants {
		if err := c.applyTableGrant(ctx, conn, quotedUser, grant); err != nil {
			return err
		}
	}

	return nil
}

func (c *Client) applyReadonlyPrivileges(ctx context.Context, conn *pgx.Conn, quotedUser string) error {
	queries := []string{
		fmt.Sprintf("GRANT SELECT ON ALL TABLES IN SCHEMA public TO %s", quotedUser),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO %s", quotedUser),
	}
	for _, q := range queries {
		if _, err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("failed to apply readonly privileges: %w", err)
		}
	}
	return nil
}

func (c *Client) applyReadwritePrivileges(ctx context.Context, conn *pgx.Conn, quotedUser string) error {
	if err := c.applyReadonlyPrivileges(ctx, conn, quotedUser); err != nil {
		return err
	}
	queries := []string{
		fmt.Sprintf("GRANT INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s", quotedUser),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT INSERT, UPDATE, DELETE ON TABLES TO %s", quotedUser),
		fmt.Sprintf("GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s", quotedUser),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s", quotedUser),
	}
	for _, q := range queries {
		if _, err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("failed to apply readwrite privileges: %w", err)
		}
	}
	return nil
}

func (c *Client) applyAdminPrivileges(ctx context.Context, conn *pgx.Conn, quotedUser string) error {
	if err := c.applyReadwritePrivileges(ctx, conn, quotedUser); err != nil {
		return err
	}
	queries := []string{
		fmt.Sprintf("GRANT CREATE ON SCHEMA public TO %s", quotedUser),
		fmt.Sprintf("GRANT TRUNCATE, REFERENCES, TRIGGER ON ALL TABLES IN SCHEMA public TO %s", quotedUser),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT TRUNCATE, REFERENCES, TRIGGER ON TABLES TO %s", quotedUser),
	}
	for _, q := range queries {
		if _, err := conn.Exec(ctx, q); err != nil {
			return fmt.Errorf("failed to apply admin privileges: %w", err)
		}
	}
	return nil
}

func (c *Client) applyOwnerPrivileges(ctx context.Context, conn *pgx.Conn, username string) error {
	if err := c.applyAdminPrivileges(ctx, conn, pq.QuoteIdentifier(username)); err != nil {
		return err
	}

	// After the admin grants so a refused membership leaves the user degraded to admin instead of stripped of everything.
	if err := ensureRoleMembership(ctx, conn, username); err != nil {
		return err
	}

	statements, err := pendingOwnershipStatements(ctx, conn, username)
	if err != nil {
		return fmt.Errorf("failed to list objects to transfer to %s: %w", username, err)
	}

	return transferOwnership(ctx, conn, statements)
}

type sqlExecutor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

type sqlQuerier interface {
	sqlExecutor
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type ownershipStatement struct {
	object    string
	statement string
}

const transferLockTimeout = `SET lock_timeout = '5s'`

const ownershipCatalogQuery = `
SELECT object, statement FROM (
	SELECT
		format('%I.%I', n.nspname, c.relname) AS object,
		format('ALTER %s %I.%I OWNER TO %I',
			CASE c.relkind
				WHEN 'f' THEN 'FOREIGN TABLE'
				WHEN 'S' THEN 'SEQUENCE'
				WHEN 'v' THEN 'VIEW'
				WHEN 'm' THEN 'MATERIALIZED VIEW'
				ELSE 'TABLE'
			END,
			n.nspname, c.relname, $1::text) AS statement
	FROM pg_class c
	JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE n.nspname = 'public'
		AND c.relkind IN ('r', 'p', 'f', 'S', 'v', 'm')
		AND pg_get_userbyid(c.relowner) <> $1::text
		AND NOT EXISTS (
			SELECT 1 FROM pg_depend d
			WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid
				AND (d.deptype = 'e' OR (c.relkind = 'S' AND d.deptype IN ('a', 'i'))))

	UNION ALL

	SELECT
		format('%I.%I', n.nspname, t.typname),
		format('ALTER %s %I.%I OWNER TO %I',
			CASE t.typtype WHEN 'd' THEN 'DOMAIN' ELSE 'TYPE' END,
			n.nspname, t.typname, $1::text)
	FROM pg_type t
	JOIN pg_namespace n ON n.oid = t.typnamespace
	WHERE n.nspname = 'public'
		AND t.typtype IN ('e', 'd', 'r', 'c')
		AND (t.typtype <> 'c' OR EXISTS (
			SELECT 1 FROM pg_class rc WHERE rc.oid = t.typrelid AND rc.relkind = 'c'))
		AND pg_get_userbyid(t.typowner) <> $1::text
		AND NOT EXISTS (
			SELECT 1 FROM pg_depend d
			WHERE d.deptype = 'e'
				AND ((d.classid = 'pg_type'::regclass AND d.objid = t.oid)
					OR (d.classid = 'pg_class'::regclass AND d.objid = t.typrelid)))

	UNION ALL

	SELECT
		routine.identity,
		format('ALTER %s %s OWNER TO %I',
			CASE p.prokind WHEN 'p' THEN 'PROCEDURE' WHEN 'a' THEN 'AGGREGATE' ELSE 'FUNCTION' END,
			routine.identity, $1::text)
	FROM pg_proc p
	JOIN pg_namespace n ON n.oid = p.pronamespace
	CROSS JOIN LATERAL (
		SELECT format('%I.%I(%s)', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid))
	) AS routine(identity)
	WHERE n.nspname = 'public'
		AND p.prokind IN ('f', 'p', 'a')
		AND pg_get_userbyid(p.proowner) <> $1::text
		AND NOT EXISTS (
			SELECT 1 FROM pg_depend d
			WHERE d.deptype IN ('e', 'i') AND d.classid = 'pg_proc'::regclass AND d.objid = p.oid)
) objects(object, statement)
ORDER BY object`

func pendingOwnershipStatements(ctx context.Context, conn *pgx.Conn, username string) ([]ownershipStatement, error) {
	rows, err := conn.Query(ctx, ownershipCatalogQuery, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var statements []ownershipStatement
	for rows.Next() {
		var statement ownershipStatement
		if err := rows.Scan(&statement.object, &statement.statement); err != nil {
			return nil, err
		}
		statements = append(statements, statement)
	}
	return statements, rows.Err()
}

func transferOwnership(ctx context.Context, exec sqlExecutor, statements []ownershipStatement) error {
	if len(statements) == 0 {
		return nil
	}

	// ALTER ... OWNER queues for ACCESS EXCLUSIVE, and everything arriving after it queues behind that — fail the object instead.
	if _, err := exec.Exec(ctx, transferLockTimeout); err != nil {
		return fmt.Errorf("failed to set the ownership transfer lock timeout: %w", err)
	}

	var failures []error
	for _, statement := range statements {
		_, err := exec.Exec(ctx, statement.statement)
		if err == nil {
			continue
		}
		failures = append(failures, fmt.Errorf("failed to transfer ownership of %s: %w", statement.object, err))
		// A locked database locks every remaining object too; waiting out one timeout each blocks the whole controller.
		if isLockNotAvailableError(err) {
			break
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return &ownershipTransferError{failures: failures}
}

const maxReportedTransferFailures = 10

// Every failure ends up in status.databases[].message, which a schema-wide outage would otherwise fill.
type ownershipTransferError struct {
	failures []error
}

func (e *ownershipTransferError) Error() string {
	reported := e.failures
	if len(reported) > maxReportedTransferFailures {
		reported = reported[:maxReportedTransferFailures]
	}
	messages := make([]string, 0, len(reported)+1)
	for _, failure := range reported {
		messages = append(messages, failure.Error())
	}
	if remaining := len(e.failures) - len(reported); remaining > 0 {
		messages = append(messages, fmt.Sprintf("and %d more", remaining))
	}
	return strings.Join(messages, "; ")
}

func (e *ownershipTransferError) Unwrap() []error {
	return e.failures
}

func (c *Client) applyTableGrant(ctx context.Context, conn *pgx.Conn, quotedUser string, grant TableGrant) error {
	privs, err := joinAllowedPrivileges(grant.Privileges)
	if err != nil {
		return err
	}
	for _, table := range grant.Tables {
		query := fmt.Sprintf("GRANT %s ON TABLE %s TO %s", privs, pq.QuoteIdentifier(table), quotedUser)
		if _, err := conn.Exec(ctx, query); err != nil {
			return fmt.Errorf("failed to apply table grant on %s: %w", table, err)
		}
	}
	return nil
}

// Upper-case is hygiene for non-CRD callers (CRD enum is case-sensitive). Empty
// slice fails loudly so admission bypass can't produce "GRANT  ON TABLE …".
func joinAllowedPrivileges(privileges []TablePrivilege) (string, error) {
	if len(privileges) == 0 {
		return "", fmt.Errorf("%w: no privileges provided", ErrInvalidTablePrivilege)
	}
	parts := make([]string, 0, len(privileges))
	for _, p := range privileges {
		up := TablePrivilege(strings.ToUpper(strings.TrimSpace(string(p))))
		if _, ok := allowedTablePrivileges[up]; !ok {
			return "", fmt.Errorf("%w: %q", ErrInvalidTablePrivilege, p)
		}
		parts = append(parts, string(up))
	}
	return strings.Join(parts, ", "), nil
}

func (c *Client) VerifyDatabaseIsolation(ctx context.Context, username, allowedDatabase string) ([]string, error) {
	query := `
		SELECT datname FROM pg_database 
		WHERE datistemplate = false 
		AND has_database_privilege($1, datname, 'CONNECT')
	`
	rows, err := c.pool.Query(ctx, query, username)
	if err != nil {
		return nil, fmt.Errorf("failed to verify database isolation: %w", err)
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var db string
		if err := rows.Scan(&db); err != nil {
			return nil, err
		}
		databases = append(databases, db)
	}
	return databases, nil
}

func (c *Client) RevokePrivilegesInDatabase(ctx context.Context, username, database string) error {
	conn, err := c.connectToDatabase(ctx, database)
	if err != nil {
		if isDatabaseNotExistError(err) {
			return nil // database already deleted, nothing to revoke
		}
		return err
	}
	defer func() { _ = conn.Close(ctx) }() // error on close is not actionable

	quotedUser := pq.QuoteIdentifier(username)
	queries := []string{
		fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %s", quotedUser),
		fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %s", quotedUser),
		fmt.Sprintf("REVOKE ALL ON SCHEMA public FROM %s", quotedUser),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM %s", quotedUser),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM %s", quotedUser),
	}
	for _, q := range queries {
		_, _ = conn.Exec(ctx, q) // best-effort cleanup
	}

	// Revoke connect on database level
	revokeConnect := fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM %s",
		pq.QuoteIdentifier(database), quotedUser)
	_, _ = c.pool.Exec(ctx, revokeConnect) // best-effort: may fail if not granted

	return nil
}

// EnsureRoleMembership makes the operator's role a member of username so it can reassign what that role owns.
func (c *Client) EnsureRoleMembership(ctx context.Context, username string) error {
	return ensureRoleMembership(ctx, c.pool, username)
}

func ensureRoleMembership(ctx context.Context, db sqlQuerier, username string) error {
	operator, inherited, err := roleMembership(ctx, db, username)
	if err != nil {
		return err
	}
	if inherited {
		return nil
	}

	query := fmt.Sprintf("GRANT %s TO CURRENT_USER", pq.QuoteIdentifier(username))
	if _, err := db.Exec(ctx, query); err != nil {
		return fmt.Errorf("failed to grant role membership in %s: %w", username, err)
	}

	// The grant takes INHERIT from the grantee's rolinherit, so a NOINHERIT operator gets SET without USAGE and would re-grant forever.
	if _, inherited, err = roleMembership(ctx, db, username); err != nil {
		return err
	}
	if !inherited {
		return fmt.Errorf("operator role %q does not inherit the privileges of roles it is a member of; "+
			"%s must be granted to it WITH INHERIT TRUE", operator, username)
	}
	return nil
}

func roleMembership(ctx context.Context, db sqlQuerier, username string) (operator string, inherited bool, err error) {
	err = db.QueryRow(ctx,
		"SELECT current_user, pg_has_role(current_user, $1, 'USAGE')", username,
	).Scan(&operator, &inherited)
	if err != nil {
		return "", false, fmt.Errorf("failed to check role membership in %s: %w", username, err)
	}
	return operator, inherited, nil
}

// ReassignOwnership transfers all objects owned by fromUser to the current connection user (master).
// This must be called before dropping a user who has owner privileges.
func (c *Client) ReassignOwnership(ctx context.Context, fromUser, database string) error {
	conn, err := c.connectToDatabase(ctx, database)
	if err != nil {
		if isDatabaseNotExistError(err) {
			return nil // database already deleted, nothing to reassign
		}
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	quotedFromUser := pq.QuoteIdentifier(fromUser)

	// REASSIGN OWNED transfers all objects owned by fromUser to the current user (master)
	// This is required before DROP USER if the user owns any objects
	query := fmt.Sprintf("REASSIGN OWNED BY %s TO CURRENT_USER", quotedFromUser)
	if _, err := conn.Exec(ctx, query); err != nil {
		return fmt.Errorf("failed to reassign ownership from %s: %w", fromUser, err)
	}

	// DROP OWNED removes any remaining privileges (grants) that fromUser has
	dropOwnedQuery := fmt.Sprintf("DROP OWNED BY %s", quotedFromUser)
	if _, err := conn.Exec(ctx, dropOwnedQuery); err != nil {
		// Best-effort: may fail if user has no remaining owned objects
		// This is expected after REASSIGN OWNED
		return nil
	}

	return nil
}

type TableGrant struct {
	Tables     []string
	Privileges []TablePrivilege
}
