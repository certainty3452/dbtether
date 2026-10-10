//go:build integration

package postgres

import (
	"context"
	"testing"
)

func TestApplyPrivileges_DowngradeRevokesWritePrivileges(t *testing.T) {
	ctx := context.Background()

	for _, from := range []string{"readwrite", "admin"} {
		t.Run(from+" to readonly", func(t *testing.T) {
			e := newPGEnv(t)
			db := e.uniqueName("it_downgrade")
			user := e.uniqueName("it_user")
			e.createDatabase(t, db, e.master)
			if err := e.client.CreateUser(ctx, user, envPassword); err != nil {
				t.Fatal(err)
			}
			conn := e.connectAs(t, e.master, envPassword, db)
			mustExec(t, conn, "CREATE TABLE before_downgrade (id int)")

			if err := e.client.ApplyPrivileges(ctx, user, db, from, "", nil); err != nil {
				t.Fatalf("ApplyPrivileges(%s): %v", from, err)
			}
			if err := e.client.ApplyPrivileges(ctx, user, db, "readonly", "", nil); err != nil {
				t.Fatalf("ApplyPrivileges(readonly): %v", err)
			}
			mustExec(t, conn, "CREATE TABLE after_downgrade (id int)")

			for _, table := range []string{"before_downgrade", "after_downgrade"} {
				if !hasTablePrivilege(t, conn, user, table, "SELECT") {
					t.Errorf("%s: SELECT missing after downgrade to readonly", table)
				}
				for _, privilege := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE"} {
					if hasTablePrivilege(t, conn, user, table, privilege) {
						t.Errorf("%s: %s still granted after downgrade from %s to readonly", table, privilege, from)
					}
				}
			}
		})
	}
}

func TestApplyPrivileges_RemovedAdditionalGrantIsRevoked(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	db := e.uniqueName("it_extra")
	user := e.uniqueName("it_user")
	e.createDatabase(t, db, e.master)
	if err := e.client.CreateUser(ctx, user, envPassword); err != nil {
		t.Fatal(err)
	}
	conn := e.connectAs(t, e.master, envPassword, db)
	mustExec(t, conn, "CREATE TABLE granted_table (id int)")

	grants := []TableGrant{{Tables: []string{"granted_table"}, Privileges: []TablePrivilege{"INSERT", "DELETE"}}}
	if err := e.client.ApplyPrivileges(ctx, user, db, "readonly", "", grants); err != nil {
		t.Fatalf("ApplyPrivileges with grants: %v", err)
	}
	if !hasTablePrivilege(t, conn, user, "granted_table", "INSERT") {
		t.Fatal("INSERT not granted by the additional grant; the revoke assertion below proves nothing")
	}

	if err := e.client.ApplyPrivileges(ctx, user, db, "readonly", "", nil); err != nil {
		t.Fatalf("ApplyPrivileges without grants: %v", err)
	}
	for _, privilege := range []string{"INSERT", "DELETE"} {
		if hasTablePrivilege(t, conn, user, "granted_table", privilege) {
			t.Errorf("%s still granted after the entry left additionalGrants", privilege)
		}
	}
}

func TestApplyPrivileges_ReadonlyUserSeesTablesCreatedByOwnerUser(t *testing.T) {
	ctx := context.Background()
	e := newPGEnv(t)
	db := e.uniqueName("it_defpriv")
	app := e.uniqueName("it_app")
	ro := e.uniqueName("it_ro")
	e.createDatabase(t, db, e.master)
	for _, user := range []string{app, ro} {
		if err := e.client.CreateUser(ctx, user, envPassword); err != nil {
			t.Fatal(err)
		}
		if err := e.client.GrantDatabaseAccess(ctx, user, db); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.client.ApplyPrivileges(ctx, app, db, "owner", app, nil); err != nil {
		t.Fatalf("ApplyPrivileges(owner): %v", err)
	}
	if err := e.client.ApplyPrivileges(ctx, ro, db, "readonly", app, nil); err != nil {
		t.Fatalf("ApplyPrivileges(readonly): %v", err)
	}

	appConn := e.connectAs(t, app, envPassword, db)
	mustExec(t, appConn, "CREATE TABLE created_by_app (id int)")

	if !hasTablePrivilege(t, appConn, ro, "created_by_app", "SELECT") {
		t.Error("readonly user has no SELECT on a table created later by the owner user")
	}
}
