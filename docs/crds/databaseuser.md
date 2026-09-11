# DatabaseUser

Represents a PostgreSQL user with specific privileges for one or more databases.

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `DatabaseUser`  
**Scope:** Namespaced

## Example

```yaml
# Simple case - single database
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: my-app-readonly
  namespace: my-team
spec:
  database:
    name: my-app-db
  privileges: readonly
```

```yaml
# Multiple databases - same user, different privileges
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: airbyte-service
  namespace: platform
spec:
  databases:
    - name: airbyte-db
      privileges: readwrite
    - name: temporal-db
      privileges: readwrite
    - name: temporal-visibility-db
      privileges: readonly
  privileges: readonly  # default for databases without explicit privileges
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `database` | object | ❌* | — | Single database reference (mutually exclusive with `databases`) |
| `databases` | array | ❌* | — | Multiple database references (mutually exclusive with `database`) |
| `username` | string | ❌ | metadata.name | PostgreSQL username (see below) |
| `privileges` | enum | ❌ | `readonly` | Default privilege preset: `readonly`, `readwrite`, `admin`, `owner` |
| `additionalGrants` | array | ❌ | `[]` | Additional table-level grants |
| `password.length` | int | ❌ | `16` | Password length (12-64) |
| `rotation.days` | int | ❌ | — | Password rotation interval in days (1-365) |
| `connectionLimit` | int | ❌ | `-1` | Max concurrent connections (`-1` = unlimited, `>= 1` to cap; `0` is rejected) |
| `idleInTransactionTimeout` | duration | ❌ | — | Abort sessions idle inside a transaction for longer than this (min `1ms`). Unset clears the role-level override |
| `deletionPolicy` | enum | ❌ | `Delete` | `Delete` drops the PostgreSQL role with the resource; `Retain` leaves it |
| `secret` | object | ❌ | — | Secret configuration (see below) |
| `secretGeneration` | enum | ❌ | `primary` | How to generate secrets: `primary` or `perDatabase` |

\* Exactly one of `database` or `databases` is required.

### database / databases

Specify exactly one of `database` (a single database) or `databases` (several). Setting both or neither is rejected on apply.

**Single database:**
```yaml
spec:
  database:
    name: my-app-db
    namespace: other-namespace  # optional, defaults to user's namespace
    privileges: readwrite       # optional, overrides spec.privileges
```

**Multiple databases:**
```yaml
spec:
  databases:
    - name: db1
      privileges: readwrite
    - name: db2
      privileges: readonly
    - name: db3
      # uses spec.privileges (default: readonly)
```

All databases must be on the same DBCluster, and each may be listed only once. An entry without `namespace` resolves to the DatabaseUser's own.

## username

If not specified, derived from `metadata.name` with dashes (`-`) converted to underscores (`_`).

| Resource Name | username (spec) | PostgreSQL Name |
|---------------|-----------------|-----------------|
| `my-app-user` | (not set) | `my_app_user` |
| `my-app-user` | `custom_user` | `custom_user` |

An explicit `username` must match `^[a-z_][a-z0-9_]*$` and be at most 63 characters.

## privileges

Presets applied to the `public` schema:

| Preset | Permissions |
|--------|-------------|
| `readonly` | `USAGE` on schema, `SELECT` on all tables |
| `readwrite` | readonly + `INSERT`, `UPDATE`, `DELETE`, sequence `USAGE`/`SELECT` |
| `admin` | readwrite + `CREATE` on schema, `TRUNCATE`, `REFERENCES`, `TRIGGER` |
| `owner` | admin + ownership of the objects in `public` (enables `ALTER TABLE`, constraints, `ALTER TYPE ... ADD VALUE`, `pg_restore`) |

Each preset also sets matching `ALTER DEFAULT PRIVILEGES`, so objects created later are covered without a re-grant.

Set at spec level (the default for all databases) or per-database.

Every reconcile resets the user's grants on the `public` schema itself (`REVOKE ALL ON SCHEMA public`) before re-applying the preset, so a hand-made `GRANT ... ON SCHEMA public` does not survive. Hand-made table grants are not revoked, but they are not tracked either and a [restore](restore.md#grants-after-the-restore) will not bring them back — put them in `additionalGrants`.

### `owner`

Only one DatabaseUser can hold `owner` on a Database. The oldest by creation time wins, ties broken by namespace then name; a user being deleted or with an invalid spec is not a candidate. Every other claimant gets `admin` on that database instead, marks it `Failed` in `status.databases`, and keeps reconciling its role, secret, rotation and other databases normally. The [grant pass after a restore](restore.md#grants-after-the-restore) runs the same election.

Ownership transfer covers the tables, sequences, views, materialized views, types and routines in `public` that the user does not already own; objects belonging to an extension stay with the extension owner. The `public` schema itself remains owned by the cluster admin — the user creates in it through the `CREATE` grant that comes with `admin`, and cannot `ALTER SCHEMA` or `DROP SCHEMA public`.

To transfer objects, the operator grants itself membership in the user's role. That membership is permanent and disappears only with the role. On deletion, or when a database is dropped from the access list, ownership is reassigned back to the operator's role.

## additionalGrants

Table-level grants on top of the preset. Useful for surgical access (e.g. read-only role that can also INSERT into a single audit table).

```yaml
spec:
  privileges: readonly
  additionalGrants:
    - tables: ["audit_log", "events"]
      privileges: ["INSERT"]
```

| Field | Constraints |
|-------|-------------|
| `tables` | At least one entry. Each must match `^[a-zA-Z_][a-zA-Z0-9_]*$`, max 63 chars. |
| `privileges` | At least one entry, from `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER`, `USAGE`. |

Both are enforced at admission and re-checked in the controller before any SQL is composed.

## connectionLimit

PostgreSQL `CONNECTION LIMIT` for the role: `-1` (default) for unlimited, `>= 1` to cap. `0` is rejected at admission — the integer zero-value cannot be told apart from "unset", so it would silently lock the role out.

## idleInTransactionTimeout

Sets `idle_in_transaction_session_timeout` on the role. Sessions idle inside an open transaction longer than this are aborted server-side — worth setting for pooled connections (PgBouncer transaction mode) where a stuck transaction holds locks indefinitely.

```yaml
spec:
  idleInTransactionTimeout: 30s
```

Minimum `1ms`. PostgreSQL's `0` (disabled) cannot be expressed; leave the field unset, which clears any role-level override.

## secretGeneration

| Mode | Behavior |
|------|----------|
| `primary` (default) | One secret, named `<name>-credentials`, carrying the first database |
| `perDatabase` | One secret per database, named `<name>-<database>-credentials`, all sharing one password |

In `primary` mode with `template: raw` and more than one database, the secret gains a `databases` key — a comma-separated list of every PostgreSQL database name. It is informational, always spelled `databases` regardless of template, and absent for every other template.

```yaml
# primary + raw, three databases
data:
  host: cluster.endpoint
  port: "5432"
  database: airbyte_db                                      # first database
  databases: airbyte_db,temporal_db,temporal_visibility_db  # informational
  username: airbyte_service
  password: <generated>
```

`perDatabase` names secrets from the Database resource name alone, so two Databases sharing a name across namespaces would collide — that is rejected. It also forbids `secret.name` and any `onConflict` other than `Fail`, both at admission.

## secret

```yaml
secret:
  name: my-custom-secret      # Custom secret name (default: {name}-credentials)
  template: DATABASE          # Key format: raw, DB, DATABASE, POSTGRES, custom, dsn
  keys:                       # Custom key names (only when template: custom)
    host: PGHOST
    port: PGPORT
    database: PGDATABASE
    username: PGUSER
    password: PGPASSWORD
  onConflict: Merge           # Fail, Adopt, Merge
```

### Key Templates

| Template | host | port | database | username | password |
|----------|------|------|----------|----------|----------|
| `raw` (default) | `host` | `port` | `database` | `username` | `password` |
| `DB` | `DB_HOST` | `DB_PORT` | `DB_NAME` | `DB_USER` | `DB_PASSWORD` |
| `DATABASE` | `DATABASE_HOST` | `DATABASE_PORT` | `DATABASE_NAME` | `DATABASE_USER` | `DATABASE_PASSWORD` |
| `POSTGRES` | `POSTGRES_HOST` | `POSTGRES_PORT` | `POSTGRES_DATABASE` | `POSTGRES_USER` | `POSTGRES_PASSWORD` |
| `custom` | from `keys`, falling back to the `raw` name | | | | |
| `dsn` | — single `dsn` key — | | | |

`dsn` produces one key holding `postgres://user:password@host:port/database`, for consumers like ORY Hydra that want a connection string.

Templates and `keys` are recomputed from the spec every reconcile, so changing them rewrites the secret on the next pass. Renaming the **password** key is the exception: the old value cannot be found under the new name, so a fresh password is generated and set in PostgreSQL. The role keeps working, but every consumer must re-read the secret.

### onConflict

Applies when a secret of that name already exists and is not owned by this DatabaseUser:

| Policy | Behavior |
|--------|----------|
| `Fail` (default) | Report an error, touch nothing |
| `Adopt` | Take ownership, regenerate credentials, overwrite the secret's data |
| `Merge` | Take ownership, overlay our keys, leave foreign keys in place |

`Merge` is recorded on the produced secret, so once adopted under `Merge` later reconciles keep overlaying even if `onConflict` is removed from the spec. To get back to full-replace semantics, switch to `Adopt` or delete the secret.

## Database Isolation

Each reconcile resolves the databases the user should reach, revokes `CONNECT` on every other database it currently holds, grants `CONNECT` on the listed ones and applies the per-database preset. Removing a database from the list therefore removes the access — no separate cleanup step.

A revoke that fails is ignored — the reconcile still reports `Ready`. And revoking the user's own `CONNECT` is not enough on its own: the role still reaches the database through `PUBLIC` unless that Database sets [`revokePublicConnect`](database.md#revokepublicconnect).

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | `Pending`, `Creating`, `Ready`, `Failed` |
| `message` | string | Detailed status message |
| `clusterName` | string | DBCluster this user belongs to |
| `username` | string | PostgreSQL username |
| `databases` | array | Per-database access status |
| `databasesSummary` | string | Printer-column form, e.g. `db1 (+2)` |
| `secretName` | string | Primary secret name |
| `passwordUpdatedAt` | timestamp | When password was last created or rotated |
| `observedGeneration` | int64 | Which spec version has been processed |

A user can be `Failed` overall while most of its databases are fine — `status.databases` is where the per-database truth lives:

```yaml
status:
  databases:
    - name: airbyte-db
      databaseName: airbyte_db
      phase: Ready
      privileges: readwrite
      secretName: airbyte-service-airbyte-db-credentials  # if perDatabase
    - name: temporal-db
      databaseName: temporal_db
      phase: Ready
      privileges: readonly
```

## Examples

### Separate secrets per database

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: airbyte-service
  namespace: platform
spec:
  databases:
    - name: airbyte-db
    - name: temporal-db
    - name: temporal-visibility-db
  privileges: readwrite
  secretGeneration: perDatabase
  secret:
    template: POSTGRES
```

Creates `airbyte-service-airbyte-db-credentials`, `airbyte-service-temporal-db-credentials` and `airbyte-service-temporal-visibility-db-credentials`, all with the same password.

### Cross-namespace database reference

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: analytics-reader
  namespace: analytics
spec:
  database:
    name: main-db
    namespace: production
  privileges: readonly
```

### User with password rotation

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: rotating-user
  namespace: my-team
spec:
  database:
    name: my-app-db
  privileges: readwrite
  password:
    length: 32
  rotation:
    days: 30
```

## kubectl Commands

```bash
kubectl get databaseusers -A
kubectl describe databaseuser my-app-readonly -n my-team

kubectl get secret my-app-readonly-credentials -n my-team -o jsonpath='{.data.password}' | base64 -d

# Force a new password: the operator regenerates and re-applies it in PostgreSQL
kubectl delete secret my-app-readonly-credentials -n my-team
```

## Troubleshooting

### Rejected on apply: "exactly one of database or databases must be set"

Admission rejects the resource; it is never created. Set exactly one:

```yaml
# Wrong
spec:
  database:
    name: db1
  databases:
    - name: db2

# Wrong
spec:
  privileges: readonly

# Correct
spec:
  database:
    name: db1
```

### Rejected on apply: "spec.secret.name cannot be set when secretGeneration=perDatabase"

Per-database secret names are derived from the Database name, so a fixed name would make every database write to the same secret. The companion rule rejects `onConflict` other than `Fail` in that mode:

```yaml
# Wrong
spec:
  secretGeneration: perDatabase
  secret:
    name: shared-secret
    onConflict: Merge

# Correct
spec:
  secretGeneration: perDatabase
  secret:
    template: POSTGRES
```

### Phase: Pending, message: "waiting for Database '&lt;name&gt;'"

No Database with that name in the resolved namespace. `waiting for Database '<name>' to be ready` means it exists but is not `Ready`; `waiting for DBCluster '<name>'` and `waiting for DBCluster '<name>' to be connected` are the same shape one level up. All of them clear on their own once the dependency is fixed. After 10 minutes the phase flips to `Failed` with `timeout: ... (pending for over 10 minutes)`.

```bash
kubectl get database -A
```

### Phase: Failed, message: "validation error: database &lt;namespace&gt;/&lt;name&gt; is listed more than once"

`databases` names the same Database twice. An entry without `namespace` resolves to the DatabaseUser's own — `my-team` here — so both entries are one reference:

```yaml
# Wrong
spec:
  databases:
    - name: db1
    - name: db1
      namespace: my-team

# Correct
spec:
  databases:
    - name: db1
      privileges: readwrite
```

### Phase: Failed, message: "validation error: per-database secrets need distinct Database names: &lt;name&gt; is referenced from &lt;ns1&gt; and &lt;ns2&gt;"

With `secretGeneration: perDatabase` the secret name comes from the Database name alone, so two Databases sharing a name across namespaces write to one secret. Drop one, or use `secretGeneration: primary`:

```yaml
# Wrong
spec:
  secretGeneration: perDatabase
  databases:
    - name: orders
    - name: orders
      namespace: other-team

# Correct
spec:
  secretGeneration: perDatabase
  databases:
    - name: orders
```

### Phase: Failed, message: "all databases must be on the same cluster: 'db1' is on 'cluster-a', but 'db2' is on 'cluster-b'"

One user is one PostgreSQL role on one cluster. Split into a DatabaseUser per cluster.

### Phase: Failed, message: "owner conflict: DatabaseUser &lt;ns&gt;/&lt;name&gt; already holds privileges=owner on Database &lt;ns&gt;/&lt;name&gt;; only one owner per database"

Two DatabaseUsers claim `owner` on the same Database; the older one keeps it and the message names it. The reporting user already has `admin` there and works — to clear the status, either lower it:

```yaml
spec:
  privileges: admin
```

or delete the claimant that should not own the objects:

```bash
kubectl delete databaseuser <name> -n <namespace>
```

Either way the reporting DatabaseUser recovers on its own.

### Phase: Failed, message: "secret error: ... already exists and is not owned by this DatabaseUser"

A secret of that name exists and belongs to something else. Rename it with `secret.name`, delete the existing secret, or set `secret.onConflict` to `Adopt` or `Merge`.

### User has access to unexpected databases

Usually inherited from `PUBLIC`; set [`revokePublicConnect`](database.md#revokepublicconnect) on the Database. The operator logs each one it sees, at debug level — set `logging.level: debug` in the chart to get them:

```bash
kubectl logs -n dbtether deployment/dbtether | grep "unexpected database"
```
