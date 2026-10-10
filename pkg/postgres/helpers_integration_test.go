//go:build integration

package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/lib/pq"
)

const envPassword = "integration"

type pgEnv struct {
	dsn    string
	admin  *pgx.Conn
	master string
	config Config
	client *Client
}

func newPGEnv(t *testing.T) *pgEnv {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("%s is not set", testDSNEnv)
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect to %s: %v", testDSNEnv, err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })

	master := fmt.Sprintf("dbtether_it_master_%d", time.Now().UnixNano())
	e := &pgEnv{dsn: dsn, admin: admin, master: master}
	e.createRole(t, master, "NOSUPERUSER CREATEROLE CREATEDB LOGIN", envPassword)

	e.config = configFromDSN(t, dsn)
	e.config.Username = master
	e.config.Password = envPassword
	client, err := NewClient(ctx, e.config)
	if err != nil {
		t.Fatalf("failed to create client as %s: %v", master, err)
	}
	t.Cleanup(client.Close)
	e.client = client
	return e
}

func (e *pgEnv) createRole(t *testing.T, name, options, password string) {
	t.Helper()
	mustExec(t, e.admin, fmt.Sprintf("CREATE ROLE %s %s PASSWORD %s",
		pq.QuoteIdentifier(name), options, pq.QuoteLiteral(password)))
	t.Cleanup(func() {
		_, _ = e.admin.Exec(context.Background(), fmt.Sprintf("DROP OWNED BY %s", pq.QuoteIdentifier(name)))
		_, _ = e.admin.Exec(context.Background(), fmt.Sprintf("DROP ROLE IF EXISTS %s", pq.QuoteIdentifier(name)))
	})
}

func (e *pgEnv) createDatabase(t *testing.T, name, owner string) {
	t.Helper()
	mustExec(t, e.admin, fmt.Sprintf("CREATE DATABASE %s OWNER %s",
		pq.QuoteIdentifier(name), pq.QuoteIdentifier(owner)))
	t.Cleanup(func() {
		_, _ = e.admin.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", pq.QuoteIdentifier(name)))
	})
}

func (e *pgEnv) uniqueName(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

func (e *pgEnv) connectAs(t *testing.T, user, password, database string) *pgx.Conn {
	t.Helper()
	cfg := e.config
	cfg.Username, cfg.Password = user, password
	c := &Client{config: cfg}
	conn, err := c.connectToDatabase(context.Background(), database)
	if err != nil {
		t.Fatalf("connect as %s to %s: %v", user, database, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func (e *pgEnv) connectSuperuser(t *testing.T, database string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = database
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("superuser connect to %s: %v", database, err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

func hasTablePrivilege(t *testing.T, conn *pgx.Conn, role, table, privilege string) bool {
	t.Helper()
	var granted bool
	if err := conn.QueryRow(context.Background(),
		"SELECT has_table_privilege($1, $2, $3)", role, table, privilege,
	).Scan(&granted); err != nil {
		t.Fatalf("has_table_privilege(%s, %s, %s): %v", role, table, privilege, err)
	}
	return granted
}

func databaseComment(t *testing.T, conn *pgx.Conn, name string) string {
	t.Helper()
	return queryString(t, conn,
		"SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1", name)
}
