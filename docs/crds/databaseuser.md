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
| `privileges` | enum | ❌ | `readonly` | Default privilege preset: `readonly`, `readwrite`, `admin` |
| `additionalGrants` | array | ❌ | `[]` | Additional table-level grants |
| `password.length` | int | ❌ | `16` | Password length (12-64) |
| `rotation.days` | int | ❌ | — | Password rotation interval in days (1-365) |
| `connectionLimit` | int | ❌ | `-1` | Max concurrent connections (`-1` = unlimited) |
| `deletionPolicy` | enum | ❌ | `Delete` | What to do with user when resource is deleted |
| `secret` | object | ❌ | — | Secret configuration (see below) |
| `secretGeneration` | enum | ❌ | `primary` | How to generate secrets: `primary` or `perDatabase` |

\* One of `database` or `databases` is required.

### database / databases

You must specify either `database` (for single database) or `databases` (for multiple databases).

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

All databases must be on the same DBCluster.

## username

**Optional.** If not specified, derived from `metadata.name` with dashes (`-`) converted to underscores (`_`).

| Resource Name | username (spec) | PostgreSQL Name |
|---------------|-----------------|-----------------|
| `my-app-user` | (not set) | `my_app_user` |
| `my-app-user` | `custom_user` | `custom_user` |

| Constraint | Value |
|------------|-------|
| Pattern | `^[a-z_][a-z0-9_]*$` |
| Max length | 63 |

## privileges

Preset privilege levels applied to the `public` schema:

| Preset | Permissions |
|--------|-------------|
| `readonly` | `SELECT` on all tables, `USAGE` on schema |
| `readwrite` | readonly + `INSERT`, `UPDATE`, `DELETE`, sequence usage |
| `admin` | readwrite + `CREATE` on schema, `TRUNCATE`, `REFERENCES`, `TRIGGER` |

Can be set at spec level (default for all databases) or per-database.

## secretGeneration

Controls how secrets are created for multiple databases:

| Mode | Behavior |
|------|----------|
| `primary` (default) | Single secret with first database as primary, includes `databases` field listing all |
| `perDatabase` | Separate secret for each database (same password, different `database` field) |

### primary mode (default)

Creates one secret with:
- `database`: first database name
- `databases`: comma-separated list of all databases (only for `template: raw` with >1 database)

```yaml
# Secret with raw template and multiple databases:
# airbyte-service-credentials
data:
  host: cluster.endpoint
  port: "5432"
  database: airbyte_db         # first database
  databases: airbyte_db,temporal_db,temporal_visibility_db  # informational
  user: airbyte_service
  password: <generated>

# Secret with POSTGRES template (no databases field):
data:
  POSTGRES_HOST: cluster.endpoint
  POSTGRES_PORT: "5432"
  POSTGRES_DATABASE: airbyte_db
  POSTGRES_USER: airbyte_service
  POSTGRES_PASSWORD: <generated>
```

### perDatabase mode

Creates one secret per database:

```yaml
spec:
  databases:
    - name: airbyte-db
    - name: temporal-db
  secretGeneration: perDatabase
  secret:
    template: POSTGRES
```

Creates secrets:
- `airbyte-service-airbyte-db-credentials` with `POSTGRES_DATABASE: airbyte_db`
- `airbyte-service-temporal-db-credentials` with `POSTGRES_DATABASE: temporal_db`

All secrets share the same password.

## secret

Secret configuration for customizing the generated credentials:

```yaml
secret:
  name: my-custom-secret      # Custom secret name (default: {name}-credentials)
  template: DATABASE          # Key format: raw, DB, DATABASE, POSTGRES, custom
  keys:                       # Custom key names (only when template: custom)
    host: PGHOST
    port: PGPORT
    database: PGDATABASE
    user: PGUSER
    password: PGPASSWORD
  onConflict: Merge           # Fail, Adopt, Merge
```

### Key Templates

| Template | host | port | database | user | password |
|----------|------|------|----------|------|----------|
| `raw` (default) | `host` | `port` | `database` | `user` | `password` |
| `DB` | `DB_HOST` | `DB_PORT` | `DB_NAME` | `DB_USER` | `DB_PASSWORD` |
| `DATABASE` | `DATABASE_HOST` | `DATABASE_PORT` | `DATABASE_NAME` | `DATABASE_USER` | `DATABASE_PASSWORD` |
| `POSTGRES` | `POSTGRES_HOST` | `POSTGRES_PORT` | `POSTGRES_DATABASE` | `POSTGRES_USER` | `POSTGRES_PASSWORD` |
| `custom` | custom | custom | custom | custom | custom |

**Note:** The `databases` field (comma-separated list of all databases) is only added when:
- `secretGeneration` is `primary` (default)
- `template` is `raw` or not specified
- User has access to more than 1 database

This field is always named `databases` (not template-specific) and is informational only.

### onConflict

Controls behavior when a secret with the specified name already exists:

| Policy | Behavior |
|--------|----------|
| `Fail` (default) | Error if secret exists and is not owned by this DatabaseUser |
| `Adopt` | Take ownership, regenerate credentials, overwrite secret data |
| `Merge` | Take ownership, add/update our keys while keeping existing keys |

## Database Isolation

**Critical security feature:** Users can ONLY connect to their assigned databases.

For each reconcile:
1. Get list of databases user currently has access to
2. Revoke access from databases NOT in the spec
3. Grant access to databases IN the spec
4. Apply privileges per database

This ensures users cannot access databases they shouldn't, even if database list changes.

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | `Pending`, `Creating`, `Ready`, `Failed` |
| `message` | string | Detailed status message |
| `clusterName` | string | DBCluster this user belongs to |
| `username` | string | PostgreSQL username |
| `databases` | array | Per-database access status |
| `secretName` | string | Primary secret name |
| `passwordUpdatedAt` | timestamp | When password was last created or rotated |
| `observedGeneration` | int64 | Which spec version has been processed |

### databases status

Each database has its own status:

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

### Simple single database user

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: orders-api
  namespace: team-alpha
spec:
  database:
    name: orders-db
  privileges: readwrite
```

### Multiple databases with different privileges

```yaml
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
  privileges: readonly
  password:
    length: 24
```

### Multiple databases with separate secrets per database

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

Creates three secrets:
- `airbyte-service-airbyte-db-credentials`
- `airbyte-service-temporal-db-credentials`
- `airbyte-service-temporal-visibility-db-credentials`

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
# List all users
kubectl get databaseusers -A

# User details
kubectl describe databaseuser my-app-readonly -n my-team

# Get credentials
kubectl get secret my-app-readonly-credentials -n my-team -o yaml

# Decode password
kubectl get secret my-app-readonly-credentials -n my-team -o jsonpath='{.data.password}' | base64 -d

# Trigger password rotation (delete secret)
kubectl delete secret my-app-readonly-credentials -n my-team
```

## Troubleshooting

### Phase: Failed, message: "validation error: cannot specify both 'database' and 'databases'"

Use either `database` OR `databases`, not both:

```yaml
# Wrong
spec:
  database:
    name: db1
  databases:
    - name: db2

# Correct
spec:
  database:
    name: db1
```

### Phase: Failed, message: "all databases must be on the same cluster"

All databases in `databases` must reference the same DBCluster. Create separate DatabaseUser resources for databases on different clusters.

### Phase: Failed, message: "Database not found"

Check that database resource exists:
```bash
kubectl get database -A
```

### User has access to unexpected databases

Check operator logs for isolation warnings:
```bash
kubectl logs -n dbtether-system deployment/dbtether-controller | grep "unexpected database"
```
