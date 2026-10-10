package postgres

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"
)

// Distinct types force callers to cast; the allowlists below validate at runtime.
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
	ApplyPrivileges(ctx context.Context, username, database, preset, ownerRole string, additionalGrants []TableGrant) error
	VerifyDatabaseIsolation(ctx context.Context, username, allowedDatabase string) ([]string, error)
	RevokePrivilegesInDatabase(ctx context.Context, username, database string) error
	ReassignOwnership(ctx context.Context, fromUser, database string) error
	EnsureRoleMembership(ctx context.Context, username string) error
}

var _ ClientInterface = (*Client)(nil)

type ClientCacheInterface interface {
	Get(ctx context.Context, clusterName string, config Config) (ClientInterface, error)
	Remove(clusterName string)
	Close()
}

// Callers hold a client for a whole reconcile without releasing it, so a replaced pool stays open this long.
const retiredClientGrace = 5 * time.Minute

const connectTimeout = 10 * time.Second

type cachedClient struct {
	client *Client
	config Config
}

type ClientCache struct {
	mu      sync.Mutex
	clients map[string]cachedClient
	retired map[*Client]*time.Timer
}

var _ ClientCacheInterface = (*ClientCache)(nil)

func NewClientCache() *ClientCache {
	return &ClientCache{
		clients: make(map[string]cachedClient),
		retired: make(map[*Client]*time.Timer),
	}
}

func (c *ClientCache) Get(ctx context.Context, clusterName string, config Config) (ClientInterface, error) {
	if cached := c.lookup(clusterName, config); cached != nil {
		if err := cached.Ping(ctx); err == nil {
			return cached, nil
		}
		c.evict(clusterName, cached)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// A concurrent cold Get may have stored a pool while we pinged; a second one would leak.
	entry, ok := c.clients[clusterName]
	if ok && entry.config == config {
		return entry.client, nil
	}

	fresh, err := NewClient(ctx, config)
	if err != nil {
		return nil, err
	}
	c.clients[clusterName] = cachedClient{client: fresh, config: config}
	if ok {
		c.retireLocked(entry.client)
	}
	return fresh, nil
}

func (c *ClientCache) lookup(clusterName string, config Config) *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.clients[clusterName]
	if !ok || entry.config != config {
		return nil
	}
	return entry.client
}

// Only the client that failed the ping goes: another Get may already have replaced it with a healthy one.
func (c *ClientCache) evict(clusterName string, client *Client) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.clients[clusterName]; ok && entry.client == client {
		delete(c.clients, clusterName)
		c.retireLocked(client)
	}
}

func (c *ClientCache) retireLocked(client *Client) {
	c.retired[client] = time.AfterFunc(retiredClientGrace, func() {
		c.mu.Lock()
		_, pending := c.retired[client]
		delete(c.retired, client)
		c.mu.Unlock()
		if pending {
			client.Close()
		}
	})
}

func (c *ClientCache) Remove(clusterName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.clients[clusterName]; ok {
		delete(c.clients, clusterName)
		c.retireLocked(entry.client)
	}
}

func (c *ClientCache) Close() {
	c.mu.Lock()
	clients := make([]*Client, 0, len(c.clients)+len(c.retired))
	for name, entry := range c.clients {
		clients = append(clients, entry.client)
		delete(c.clients, name)
	}
	for client, timer := range c.retired {
		timer.Stop()
		clients = append(clients, client)
		delete(c.retired, client)
	}
	c.mu.Unlock()

	for _, client := range clients {
		client.Close()
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

	poolConfig, err := pgxpool.ParseConfig(config.serverURL())
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection settings: %w", err)
	}
	config.applyCredentials(&poolConfig.ConnConfig.Config, config.Database)

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

// Credentials bypass connection strings so no character is parsed as syntax or echoed in errors.
func (c *Config) serverURL() string {
	return (&url.URL{
		Scheme:   "postgres",
		Host:     net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		RawQuery: url.Values{"sslmode": {c.SSLMode}}.Encode(),
	}).String()
}

func (c *Config) applyCredentials(cfg *pgconn.Config, database string) {
	cfg.User = c.Username
	cfg.Password = c.Password
	cfg.Database = database
	cfg.ConnectTimeout = connectTimeout
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

func ownerComment(namespace, name string) string {
	return fmt.Sprintf("dbtether:%s/%s", namespace, name)
}

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

	comment := ownerComment(ownerNamespace, ownerName)
	commentQuery := fmt.Sprintf("COMMENT ON DATABASE %s IS %s",
		pq.QuoteIdentifier(name), pq.QuoteLiteral(comment))
	_, _ = c.pool.Exec(ctx, commentQuery)

	return nil
}

func (c *Client) GetDatabaseOwner(ctx context.Context, name string) (namespace, resourceName string, err error) {
	comment, err := c.databaseComment(ctx, name)
	if err != nil {
		return "", "", err
	}
	ns, n, ok := parseOwnerComment(comment)
	if !ok {
		return "", "", nil
	}
	return ns, n, nil
}

func (c *Client) databaseComment(ctx context.Context, name string) (string, error) {
	var comment string
	err := c.pool.QueryRow(ctx,
		`SELECT COALESCE(shobj_description(oid, 'pg_database'), '') FROM pg_database WHERE datname = $1`,
		name,
	).Scan(&comment)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to get database comment: %w", err)
	}
	return comment, nil
}

// REVOKE by a non-owner only warns, and a NULL datacl grants PUBLIC CONNECT.
const publicConnectGrantedQuery = `
	SELECT EXISTS (
		SELECT 1 FROM pg_database d
		WHERE d.datname = $1
		  AND (d.datacl IS NULL
		       OR EXISTS (SELECT 1 FROM aclexplode(d.datacl) a
		                  WHERE a.grantee = 0 AND a.privilege_type = 'CONNECT'))
	)`

func (c *Client) RevokePublicConnect(ctx context.Context, name string) error {
	var granted bool
	if err := c.pool.QueryRow(ctx, publicConnectGrantedQuery, name).Scan(&granted); err != nil {
		return fmt.Errorf("failed to check public connect on %s: %w", name, err)
	}
	if !granted {
		return nil
	}

	query := fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", pq.QuoteIdentifier(name))
	if _, err := c.pool.Exec(ctx, query); err != nil {
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
		return err == nil, err
	}

	comment, err := c.databaseComment(ctx, name)
	if err != nil {
		return false, err
	}
	ns, n, marked := parseOwnerComment(comment)
	if comment != "" && !marked {
		return false, fmt.Errorf("database %s has a foreign comment %q; dbtether only claims a database without a comment or with its own ownership marker",
			name, comment)
	}

	desiredComment := ownerComment(ownerNamespace, ownerName)
	if comment == desiredComment {
		return true, nil
	}

	if !marked || forceAdopt {
		commentQuery := fmt.Sprintf("COMMENT ON DATABASE %s IS %s",
			pq.QuoteIdentifier(name), pq.QuoteLiteral(desiredComment))
		// COMMENT needs PostgreSQL ownership; a database owned by another role stays untracked.
		if _, err := c.pool.Exec(ctx, commentQuery); err != nil {
			return false, nil
		}
		return true, nil
	}

	if ns != ownerNamespace || n != ownerName {
		return false, fmt.Errorf("database %s is owned by %s/%s, cannot be claimed by %s/%s (use annotation dbtether.io/force-adopt to override)",
			name, ns, n, ownerNamespace, ownerName)
	}

	return true, nil
}

func (c *Client) ClearDatabaseOwner(ctx context.Context, name string) error {
	query := fmt.Sprintf("COMMENT ON DATABASE %s IS NULL", pq.QuoteIdentifier(name))
	if _, err := c.pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("failed to clear ownership of database %s: %w", name, err)
	}
	return nil
}

// Superusers always pass; others need the database owner's privileges.
const dropPermittedQuery = `SELECT pg_has_role(current_user, datdba, 'USAGE') FROM pg_database WHERE datname = $1`

func (c *Client) DropDatabase(ctx context.Context, name string) error {
	var permitted bool
	err := c.pool.QueryRow(ctx, dropPermittedQuery, name).Scan(&permitted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to check permission to drop database %s: %w", name, err)
	}
	if !permitted {
		return fmt.Errorf("cannot drop database %s: the operator role neither owns it nor has the privileges of its owner", name)
	}

	_, _ = c.pool.Exec(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
		name) // best-effort: DROP DATABASE reports the sessions that survive

	query := fmt.Sprintf("DROP DATABASE IF EXISTS %s", pq.QuoteIdentifier(name))
	if _, err := c.pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("failed to drop database %s: %w", name, err)
	}
	return nil
}

func (c *Client) CreateExtension(ctx context.Context, dbName, extensionName string) error {
	conn, err := c.connectToDatabase(ctx, dbName)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

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
	defer func() { _ = conn.Close(ctx) }()

	for _, ext := range extensions {
		query := fmt.Sprintf("CREATE EXTENSION IF NOT EXISTS %s", pq.QuoteIdentifier(ext))
		if _, err = conn.Exec(ctx, query); err != nil {
			return fmt.Errorf("failed to create extension %s: %w", ext, err)
		}
	}
	return nil
}

func (c *Client) connectToDatabase(ctx context.Context, dbName string) (*pgx.Conn, error) {
	connConfig, err := pgx.ParseConfig(c.config.serverURL())
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection settings: %w", err)
	}
	c.config.applyCredentials(&connConfig.Config, dbName)
	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database %s: %w", dbName, err)
	}
	return conn, nil
}

func IsTransientError(err error) bool {
	return err != nil
}

func isDatabaseNotExistError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "3D000"
	}
	return false
}

func isLockNotAvailableError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "55P03"
	}
	return false
}

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

// Only a direct ACL entry counts: CONNECT inherited from PUBLIC or a role is not the grant dbtether manages.
const directConnectQuery = `
	SELECT d.datname FROM pg_database d, aclexplode(d.datacl) a
	WHERE d.datistemplate = false
	  AND a.privilege_type = 'CONNECT'
	  AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = $1)`

func (c *Client) hasDirectConnect(ctx context.Context, username, database string) (bool, error) {
	var granted bool
	err := c.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM (`+directConnectQuery+`) x WHERE x.datname = $2)`,
		username, database,
	).Scan(&granted)
	if err != nil {
		return false, fmt.Errorf("failed to read database access of %s on %s: %w", username, database, err)
	}
	return granted, nil
}

func (c *Client) GrantDatabaseAccess(ctx context.Context, username, database string) error {
	granted, err := c.hasDirectConnect(ctx, username, database)
	if err != nil {
		return err
	}
	if granted {
		return nil
	}
	query := fmt.Sprintf(
		"GRANT CONNECT ON DATABASE %s TO %s",
		pq.QuoteIdentifier(database),
		pq.QuoteIdentifier(username),
	)
	if _, err = c.pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("failed to grant database access: %w", err)
	}
	return nil
}

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

func (c *Client) directConnectDatabases(ctx context.Context, username string) ([]string, error) {
	rows, err := c.pool.Query(ctx, directConnectQuery, username)
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
	return databases, rows.Err()
}

func (c *Client) SyncDatabaseAccess(ctx context.Context, username string, allowedDatabases []string) error {
	currentAccess, err := c.directConnectDatabases(ctx, username)
	if err != nil {
		return err
	}

	allowed := make(map[string]bool, len(allowedDatabases))
	for _, db := range allowedDatabases {
		allowed[db] = true
	}

	for _, db := range currentAccess {
		if !allowed[db] {
			if err := c.RevokeDatabaseAccess(ctx, username, db); err != nil {
				return err
			}
		}
	}

	for _, db := range allowedDatabases {
		if err := c.GrantDatabaseAccess(ctx, username, db); err != nil {
			return fmt.Errorf("failed to grant access to %s: %w", db, err)
		}
	}

	return nil
}

type presetPrivileges struct {
	schema    []string
	tables    []string
	sequences []string
}

var adminPrivileges = presetPrivileges{
	schema:    []string{"USAGE", "CREATE"},
	tables:    []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"},
	sequences: []string{"USAGE", "SELECT"},
}

var privilegePresets = map[string]presetPrivileges{
	"readonly":  {schema: []string{"USAGE"}, tables: []string{"SELECT"}},
	"readwrite": {schema: []string{"USAGE"}, tables: []string{"SELECT", "INSERT", "UPDATE", "DELETE"}, sequences: []string{"USAGE", "SELECT"}},
	"admin":     adminPrivileges,
	"owner":     adminPrivileges,
}

// GRANT ... ON TABLE on a sequence drops any other privilege with a warning, so desiring one would never converge.
var sequencePrivileges = map[string]bool{"USAGE": true, "SELECT": true, "UPDATE": true}

// Objects the owner role creates get the preset's default privileges too.
func (c *Client) ApplyPrivileges(ctx context.Context, username, database, preset, ownerRole string, additionalGrants []TableGrant) error {
	desired, err := newDesiredPrivileges(username, preset, ownerRole, additionalGrants)
	if err != nil {
		return err
	}

	conn, err := c.connectToDatabase(ctx, database)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	if desired.ownerRole != "" {
		if err := ensureRoleMembership(ctx, conn, desired.ownerRole); err != nil {
			return err
		}
	}

	if err := convergePrivileges(ctx, conn, username, &desired); err != nil {
		return fmt.Errorf("failed to apply %s privileges: %w", preset, err)
	}

	if preset != "owner" {
		return nil
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

type desiredPrivileges struct {
	preset    presetPrivileges
	ownerRole string
	tables    map[string][]string
}

func newDesiredPrivileges(username, preset, ownerRole string, additionalGrants []TableGrant) (desiredPrivileges, error) {
	privileges, ok := privilegePresets[preset]
	if !ok {
		return desiredPrivileges{}, fmt.Errorf("unknown privileges preset %q", preset)
	}
	if ownerRole == username {
		ownerRole = ""
	}
	tables, err := additionalTablePrivileges(additionalGrants)
	if err != nil {
		return desiredPrivileges{}, err
	}
	return desiredPrivileges{preset: privileges, ownerRole: ownerRole, tables: tables}, nil
}

func additionalTablePrivileges(grants []TableGrant) (map[string][]string, error) {
	tables := make(map[string][]string)
	for _, grant := range grants {
		privileges, err := allowedPrivileges(grant.Privileges)
		if err != nil {
			return nil, err
		}
		for _, table := range grant.Tables {
			tables[table] = append(tables[table], privileges...)
		}
	}
	return tables, nil
}

func (d *desiredPrivileges) relation(sequence, inPublic bool, names []string) []string {
	var privileges []string
	if inPublic && sequence {
		privileges = append(privileges, d.preset.sequences...)
	} else if inPublic {
		privileges = append(privileges, d.preset.tables...)
	}
	for _, name := range names {
		privileges = append(privileges, d.tables[name]...)
	}
	if !sequence {
		return privileges
	}
	valid := privileges[:0]
	for _, privilege := range privileges {
		if sequencePrivileges[privilege] {
			valid = append(valid, privilege)
		}
	}
	return valid
}

type defaultACLKey struct {
	role    string
	objects string
}

func (d *desiredPrivileges) defaults(operator string) map[defaultACLKey][]string {
	defaults := make(map[defaultACLKey][]string)
	for _, creator := range []string{operator, d.ownerRole} {
		if creator == "" {
			continue
		}
		defaults[defaultACLKey{creator, "TABLES"}] = d.preset.tables
		if len(d.preset.sequences) > 0 {
			defaults[defaultACLKey{creator, "SEQUENCES"}] = d.preset.sequences
		}
	}
	return defaults
}

// Privileges the user holds in public from a grantor the operator acts for, plus every relation a grant may target.
const heldPrivilegesQuery = `
WITH target AS (SELECT (SELECT oid FROM pg_roles WHERE rolname = $1) AS oid),
named AS (SELECT name, to_regclass(quote_ident(name)) AS oid FROM unnest($2::text[]) AS name),
objects AS (
	SELECT 'SCHEMA' AS kind, quote_ident(n.nspname) AS object, '' AS role, true AS in_public, false AS owned,
		'{}'::text[] AS named, n.nspacl AS acl
	FROM pg_namespace n
	WHERE n.nspname = 'public'
	UNION ALL
	SELECT CASE c.relkind WHEN 'S' THEN 'SEQUENCE' ELSE 'TABLE' END, format('%I.%I', n.nspname, c.relname), '',
		n.nspname = 'public', (c.relowner = target.oid) IS TRUE,
		ARRAY(SELECT named.name FROM named WHERE named.oid = c.oid ORDER BY named.name), c.relacl
	FROM pg_class c
	JOIN pg_namespace n ON n.oid = c.relnamespace
	CROSS JOIN target
	WHERE c.relkind IN ('r', 'p', 'v', 'm', 'f', 'S')
		AND (n.nspname = 'public' OR c.oid IN (SELECT named.oid FROM named))
	UNION ALL
	SELECT 'ROUTINE', format('%I.%I(%s)', n.nspname, p.proname, pg_get_function_identity_arguments(p.oid)), '',
		true, (p.proowner = target.oid) IS TRUE, '{}', p.proacl
	FROM pg_proc p
	JOIN pg_namespace n ON n.oid = p.pronamespace
	CROSS JOIN target
	WHERE n.nspname = 'public' AND p.proacl IS NOT NULL
	UNION ALL
	SELECT 'DEFAULT',
		CASE d.defaclobjtype WHEN 'r' THEN 'TABLES' WHEN 'S' THEN 'SEQUENCES' WHEN 'f' THEN 'FUNCTIONS' ELSE 'TYPES' END,
		pg_get_userbyid(d.defaclrole), true, false, '{}', d.defaclacl
	FROM pg_default_acl d
	JOIN pg_namespace n ON n.oid = d.defaclnamespace
	WHERE n.nspname = 'public'
		AND d.defaclobjtype IN ('r', 'S', 'f', 'T')
		AND pg_has_role(d.defaclrole, 'USAGE')
)
SELECT o.kind, o.object, o.role, o.in_public, o.owned, o.named, held.privileges, held.grantable
FROM objects o
CROSS JOIN target
CROSS JOIN LATERAL (
	SELECT coalesce(array_agg(a.privilege_type), '{}'),
		coalesce(array_agg(a.privilege_type) FILTER (WHERE a.is_grantable), '{}')
	FROM aclexplode(o.acl) a
	WHERE a.grantee = target.oid AND pg_has_role(a.grantor, 'USAGE')
) held(privileges, grantable)
ORDER BY o.kind, o.role, o.object`

type heldPrivileges struct {
	kind       string
	object     string
	role       string
	inPublic   bool
	owned      bool
	named      []string
	privileges []string
	grantable  []string
}

// One transaction so other sessions never see a partial privilege set.
func convergePrivileges(ctx context.Context, conn *pgx.Conn, username string, desired *desiredPrivileges) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after commit

	statements, err := privilegeStatements(ctx, tx, username, desired)
	if err != nil {
		return fmt.Errorf("failed to read current privileges: %w", err)
	}
	for _, statement := range statements {
		if _, err := tx.Exec(ctx, statement); err != nil {
			return fmt.Errorf("%s: %w", statement, err)
		}
	}
	return tx.Commit(ctx)
}

func privilegeStatements(ctx context.Context, tx pgx.Tx, username string, desired *desiredPrivileges) ([]string, error) {
	var operator string
	if err := tx.QueryRow(ctx, "SELECT current_user").Scan(&operator); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, heldPrivilegesQuery, username, slices.Collect(maps.Keys(desired.tables)))
	if err != nil {
		return nil, err
	}
	held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (heldPrivileges, error) {
		var h heldPrivileges
		err := row.Scan(&h.kind, &h.object, &h.role, &h.inPublic, &h.owned, &h.named, &h.privileges, &h.grantable)
		return h, err
	})
	if err != nil {
		return nil, err
	}

	plan := newPrivilegePlan(desired, operator)
	for i := range held {
		plan.reconcile(&held[i])
	}
	return plan.statements(pq.QuoteIdentifier(username)), nil
}

type privilegePlan struct {
	desired      *desiredPrivileges
	defaults     map[defaultACLKey][]string
	unresolved   map[string]bool
	schemaListed bool
	targets      []aclTarget
}

func newPrivilegePlan(desired *desiredPrivileges, operator string) *privilegePlan {
	unresolved := make(map[string]bool, len(desired.tables))
	for name := range desired.tables {
		unresolved[name] = true
	}
	return &privilegePlan{desired: desired, defaults: desired.defaults(operator), unresolved: unresolved}
}

func (p *privilegePlan) reconcile(h *heldPrivileges) {
	target := aclTarget{object: h.kind + " " + h.object, held: heldMap(h.privileges, h.grantable)}
	switch h.kind {
	case "SCHEMA":
		p.schemaListed = true
		target.desired = p.desired.preset.schema
	case "TABLE", "SEQUENCE":
		for _, name := range h.named {
			delete(p.unresolved, name)
		}
		if h.owned {
			return
		}
		target.desired = p.desired.relation(h.kind == "SEQUENCE", h.inPublic, h.named)
		target.keepUndesired = !h.inPublic
	case "ROUTINE":
		if h.owned {
			return
		}
	case "DEFAULT":
		key := defaultACLKey{h.role, h.object}
		target.prefix, target.object, target.desired = defaultACLPrefix(h.role), h.object, p.defaults[key]
		delete(p.defaults, key)
	}
	p.targets = append(p.targets, target)
}

func (p *privilegePlan) statements(grantee string) []string {
	targets := p.targets
	if !p.schemaListed {
		targets = append(targets, aclTarget{object: "SCHEMA public", desired: p.desired.preset.schema})
	}
	keys := slices.SortedFunc(maps.Keys(p.defaults), func(a, b defaultACLKey) int {
		return cmp.Or(strings.Compare(a.role, b.role), strings.Compare(a.objects, b.objects))
	})
	for _, key := range keys {
		targets = append(targets, aclTarget{prefix: defaultACLPrefix(key.role), object: key.objects, desired: p.defaults[key]})
	}
	// Left to the server so a missing or non-relation table fails exactly as GRANT reports it.
	for _, name := range slices.Sorted(maps.Keys(p.unresolved)) {
		targets = append(targets, aclTarget{object: "TABLE " + pq.QuoteIdentifier(name), desired: p.desired.tables[name]})
	}

	statements := make([]string, 0, len(targets))
	for i := range targets {
		statements = append(statements, targets[i].statements(grantee)...)
	}
	return statements
}

func defaultACLPrefix(role string) string {
	return "ALTER DEFAULT PRIVILEGES FOR ROLE " + pq.QuoteIdentifier(role) + " IN SCHEMA public "
}

type aclTarget struct {
	prefix        string
	object        string
	desired       []string
	held          map[string]bool
	keepUndesired bool
}

func heldMap(privileges, grantable []string) map[string]bool {
	held := make(map[string]bool, len(privileges))
	for _, privilege := range privileges {
		held[privilege] = held[privilege] || slices.Contains(grantable, privilege)
	}
	return held
}

func (t aclTarget) statements(grantee string) []string {
	want := make(map[string]bool, len(t.desired))
	for _, privilege := range t.desired {
		want[privilege] = true
	}
	var grant, revoke, revokeOption []string
	for privilege := range want {
		grantable, held := t.held[privilege]
		if !held {
			grant = append(grant, privilege)
		} else if grantable {
			revokeOption = append(revokeOption, privilege)
		}
	}
	if !t.keepUndesired {
		for privilege := range t.held {
			if !want[privilege] {
				revoke = append(revoke, privilege)
			}
		}
	}

	var statements []string
	add := func(format string, privileges []string) {
		if len(privileges) == 0 {
			return
		}
		slices.Sort(privileges)
		statements = append(statements,
			t.prefix+fmt.Sprintf(format, strings.Join(privileges, ", "), t.object, grantee))
	}
	add("REVOKE %s ON %s FROM %s", revoke)
	add("REVOKE GRANT OPTION FOR %s ON %s FROM %s", revokeOption)
	add("GRANT %s ON %s TO %s", grant)
	return statements
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

// Upper-case is hygiene for non-CRD callers (CRD enum is case-sensitive). Empty
// slice fails loudly so admission bypass can't produce "GRANT  ON TABLE …".
func allowedPrivileges(privileges []TablePrivilege) ([]string, error) {
	if len(privileges) == 0 {
		return nil, fmt.Errorf("%w: no privileges provided", ErrInvalidTablePrivilege)
	}
	parts := make([]string, 0, len(privileges))
	for _, p := range privileges {
		up := TablePrivilege(strings.ToUpper(strings.TrimSpace(string(p))))
		if _, ok := allowedTablePrivileges[up]; !ok {
			return nil, fmt.Errorf("%w: %q", ErrInvalidTablePrivilege, p)
		}
		parts = append(parts, string(up))
	}
	return parts, nil
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
	defer func() { _ = conn.Close(ctx) }()

	quotedUser := pq.QuoteIdentifier(username)
	queries := []string{
		fmt.Sprintf("REVOKE ALL ON ALL TABLES IN SCHEMA public FROM %s", quotedUser),
		fmt.Sprintf("REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM %s", quotedUser),
		fmt.Sprintf("REVOKE ALL ON SCHEMA public FROM %s", quotedUser),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM %s", quotedUser),
		fmt.Sprintf("ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON SEQUENCES FROM %s", quotedUser),
	}
	for _, q := range queries {
		_, _ = conn.Exec(ctx, q)
	}

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

// Must run before dropping a user that owns objects.
func (c *Client) ReassignOwnership(ctx context.Context, fromUser, database string) error {
	conn, err := c.connectToDatabase(ctx, database)
	if err != nil {
		if isDatabaseNotExistError(err) {
			return nil // database already deleted, nothing to reassign
		}
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	// Both statements wait for ACCESS EXCLUSIVE behind application sessions; the caller retries timeouts.
	if _, err := conn.Exec(ctx, transferLockTimeout); err != nil {
		return fmt.Errorf("failed to set the ownership transfer lock timeout: %w", err)
	}

	quotedFromUser := pq.QuoteIdentifier(fromUser)
	if _, err := conn.Exec(ctx, fmt.Sprintf("REASSIGN OWNED BY %s TO CURRENT_USER", quotedFromUser)); err != nil {
		return fmt.Errorf("failed to reassign ownership from %s: %w", fromUser, err)
	}
	if _, err := conn.Exec(ctx, fmt.Sprintf("DROP OWNED BY %s", quotedFromUser)); err != nil {
		return fmt.Errorf("failed to drop privileges of %s: %w", fromUser, err)
	}
	return nil
}

type TableGrant struct {
	Tables     []string
	Privileges []TablePrivilege
}
