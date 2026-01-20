package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lib/pq"
)

type Config struct {
	Host     string
	Port     int
	Username string
	Password string
	Database string
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
	SetConnectionLimit(ctx context.Context, username string, limit int) error
	DropUser(ctx context.Context, username string) error
	RevokeAllDatabaseAccess(ctx context.Context, username string) error
	GrantDatabaseAccess(ctx context.Context, username, database string) error
	ApplyPrivileges(ctx context.Context, username, database, preset string, additionalGrants []TableGrant) error
	VerifyDatabaseIsolation(ctx context.Context, username, allowedDatabase string) ([]string, error)
	RevokePrivilegesInDatabase(ctx context.Context, username, database string) error
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
	client, ok := c.clients[clusterName]
	c.mu.RUnlock()

	if ok {
		if err := client.Ping(ctx); err == nil {
			return client, nil
		}
		c.Remove(clusterName)
	}

	client, err := NewClient(ctx, config)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.clients[clusterName] = client
	c.mu.Unlock()

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

	connString := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=require",
		config.Host, config.Port, config.Username, config.Password, config.Database,
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

func (c *Client) RevokePublicConnect(ctx context.Context, name string) error {
	query := fmt.Sprintf("REVOKE CONNECT ON DATABASE %s FROM PUBLIC", pq.QuoteIdentifier(name))
	_, err := c.pool.Exec(ctx, query)
	if err != nil {
		return fmt.Errorf("failed to revoke public connect on %s: %w", name, err)
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
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=require",
		c.config.Host, c.config.Port, c.config.Username, c.config.Password, dbName,
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

func (c *Client) applyTableGrant(ctx context.Context, conn *pgx.Conn, quotedUser string, grant TableGrant) error {
	for _, table := range grant.Tables {
		privs := ""
		for i, p := range grant.Privileges {
			if i > 0 {
				privs += ", "
			}
			privs += p
		}
		query := fmt.Sprintf("GRANT %s ON TABLE %s TO %s", privs, pq.QuoteIdentifier(table), quotedUser)
		if _, err := conn.Exec(ctx, query); err != nil {
			return fmt.Errorf("failed to apply table grant on %s: %w", table, err)
		}
	}
	return nil
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

type TableGrant struct {
	Tables     []string
	Privileges []string
}
