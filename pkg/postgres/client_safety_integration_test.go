//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestReassignOwnership_BlockedByLockFailsWithinBound(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	db := e.uniqueName("it_lock")
	app := e.uniqueName("it_app")
	e.createDatabase(t, db, e.master)
	if err := e.client.CreateUser(ctx, app, envPassword); err != nil {
		t.Fatal(err)
	}
	if err := e.client.ApplyPrivileges(ctx, app, db, "owner", "", nil); err != nil {
		t.Fatalf("ApplyPrivileges(owner): %v", err)
	}

	super := e.connectSuperuser(t, db)
	mustExec(t, super, "CREATE TABLE locked_table (id int)")
	mustExec(t, super, "ALTER TABLE locked_table OWNER TO "+app)

	holder := e.connectSuperuser(t, db)
	mustExec(t, holder, "BEGIN")
	mustExec(t, holder, "LOCK TABLE locked_table IN ACCESS SHARE MODE")
	t.Cleanup(func() { _, _ = holder.Exec(context.Background(), "ROLLBACK") })

	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.client.ReassignOwnership(callCtx, app, db) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error while another session holds a lock on a table to reassign")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("ReassignOwnership blocked for 15s on a held lock with a context without deadline")
	}
}

func TestClientCache_GetReturnsClientForChangedConfig(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	other := e.uniqueName("it_other")
	e.createRole(t, other, "LOGIN", envPassword)

	cache := NewClientCache()
	t.Cleanup(cache.Close)

	cfgA := e.config
	cfgB := e.config
	cfgB.Username = other

	first, err := cache.Get(ctx, "c", cfgA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Get(ctx, "c", cfgB)
	if err != nil {
		t.Fatal(err)
	}

	var current string
	if err := second.(*Client).pool.QueryRow(ctx, "SELECT current_user").Scan(&current); err != nil {
		t.Fatalf("query through the second client: %v", err)
	}
	if current != other {
		t.Errorf("Get with a changed config returned a client connected as %q (cached: %v), want %q", current, first == second, other)
	}
}

func TestClient_AdminCredentialsWithSpecialCharactersConnect(t *testing.T) {
	ctx := context.Background()
	passwords := []string{"pass word", "'quote", `back\slash`, "x host=evil.example"}

	for _, password := range passwords {
		t.Run(password, func(t *testing.T) {
			e := newPGEnv(t)
			role := e.uniqueName("it_special")
			e.createRole(t, role, "LOGIN", password)
			cfg := e.config
			cfg.Username, cfg.Password = role, password

			client, err := NewClient(ctx, cfg)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			t.Cleanup(client.Close)
			if err := client.Ping(ctx); err != nil {
				t.Errorf("Ping: %v", err)
			}
			conn, err := client.connectToDatabase(ctx, "postgres")
			if err != nil {
				t.Errorf("connectToDatabase: %v", err)
			} else {
				_ = conn.Close(ctx)
			}
		})
	}
}

func TestClient_ConnectionErrorsDoNotLeakPassword(t *testing.T) {
	ctx := context.Background()
	passwords := []string{"pass word", "'quote", `back\slash`, "x host=evil.example", "plain-secret-value"}

	for _, password := range passwords {
		t.Run(password, func(t *testing.T) {
			e := newPGEnv(t)
			role := e.uniqueName("it_leak")
			e.createRole(t, role, "LOGIN", "the-real-password")
			cfg := e.config
			cfg.Username, cfg.Password = role, password

			var errs []error
			client, err := NewClient(ctx, cfg)
			if err != nil {
				errs = append(errs, err)
			} else {
				t.Cleanup(client.Close)
				errs = append(errs, client.Ping(ctx))
				_, connErr := client.connectToDatabase(ctx, "postgres")
				errs = append(errs, connErr)
			}

			fragments := []string{password}
			for _, token := range strings.Fields(password) {
				if len(token) >= 6 {
					fragments = append(fragments, token)
				}
			}
			failures := 0
			for _, err := range errs {
				if err == nil {
					continue
				}
				failures++
				for _, fragment := range fragments {
					if strings.Contains(err.Error(), fragment) {
						t.Errorf("error leaks password fragment %q: %v", fragment, err)
					}
				}
			}
			if failures == 0 {
				t.Fatal("expected the wrong password to fail the connection")
			}
		})
	}
}

func TestDropDatabase_FailedDropLeavesApplicationSessionsAlive(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	db := e.uniqueName("it_drop")
	owner := e.uniqueName("it_owner")
	operator := e.uniqueName("it_operator")
	e.createRole(t, owner, "LOGIN CREATEDB", envPassword)
	e.createRole(t, operator, "LOGIN", envPassword)
	mustExec(t, e.admin, "GRANT pg_signal_backend TO "+operator)
	e.createDatabase(t, db, owner)

	session := e.connectAs(t, owner, envPassword, db)

	cfg := e.config
	cfg.Username, cfg.Password = operator, envPassword
	client, err := NewClient(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)

	if err := client.DropDatabase(ctx, db); err == nil {
		t.Fatal("expected DropDatabase to fail for a role that does not own the database")
	}

	var one int
	if err := session.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Errorf("application session was terminated by a drop that could not succeed: %v", err)
	}
}

func TestEnsureDatabaseWithOwner_ForeignCommentIsNotClaimed(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	db := e.uniqueName("it_comment")
	e.createDatabase(t, db, e.master)
	const foreign = "managed by the platform team"
	mustExec(t, e.admin, "COMMENT ON DATABASE "+db+" IS '"+foreign+"'")

	if _, err := e.client.EnsureDatabaseWithOwner(ctx, db, "ns", "claimant", false); err == nil {
		t.Error("expected an error for a database whose comment is not in dbtether format")
	}
	if got := databaseComment(t, e.admin, db); got != foreign {
		t.Errorf("comment = %q, want it left as %q", got, foreign)
	}
}
