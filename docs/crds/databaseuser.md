# DatabaseUser

Represents a PostgreSQL user with specific privileges for a database.

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `DatabaseUser`  
**Scope:** Namespaced

## Example

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: my-app-readonly
  namespace: my-team
spec:
  databaseRef:
    name: my-app-db
  privileges: readonly
  password:
    length: 16
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `databaseRef.name` | string | ✅ | — | Name of the Database resource |
| `databaseRef.namespace` | string | ❌ | same as user | Namespace of the Database resource |
| `username` | string | ❌ | metadata.name | PostgreSQL username (see below) |
| `privileges` | enum | ✅ | — | Privilege preset: `readonly`, `readwrite`, `admin` |
| `additionalGrants` | array | ❌ | `[]` | Additional table-level grants |
| `password.length` | int | ❌ | `16` | Password length (12-64) |
| `rotation.days` | int | ❌ | — | Password rotation interval in days (1-365) |
| `connectionLimit` | int | ❌ | `-1` | Max concurrent connections (`-1` = unlimited) |
| `deletionPolicy` | enum | ❌ | `Delete` | What to do with user when resource is deleted |

## username

**Optional.** If not specified, derived from `metadata.name` with dashes (`-`) converted to underscores (`_`).

| Resource Name | username (spec) | PostgreSQL Name |
|---------------|-----------------|-----------------|
| `my-app-user` | (not set) | `my_app_user` |
| `my-app-user` | `custom_user` | `custom_user` |

Use explicit `username` only when you need a different name than the resource name.

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

### SQL Executed

**readonly:**
```sql
GRANT USAGE ON SCHEMA public TO <user>;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO <user>;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO <user>;
```

**readwrite:**
```sql
-- includes readonly +
GRANT INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO <user>;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO <user>;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT INSERT, UPDATE, DELETE ON TABLES TO <user>;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO <user>;
```

**admin:**
```sql
-- includes readwrite +
GRANT CREATE ON SCHEMA public TO <user>;
GRANT TRUNCATE, REFERENCES, TRIGGER ON ALL TABLES IN SCHEMA public TO <user>;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT TRUNCATE, REFERENCES, TRIGGER ON TABLES TO <user>;
```

## additionalGrants

Additional grants on specific tables (on top of preset):

```yaml
additionalGrants:
  - tables: ["audit_log", "events"]
    privileges: [INSERT]
```

Available privileges: `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER`

## password

Password configuration:

```yaml
password:
  length: 24  # default: 16, range: 12-64
```

- Auto-generated using cryptographically secure random
- Characters: lowercase, uppercase, digits, and safe special chars (`._-,^`)
- Guarantees at least 3 of each character type
- Stored only in Kubernetes Secret
- **Regeneration**: Deleting the Secret triggers automatic password regeneration

## deletionPolicy

Determines what happens to the PostgreSQL user when the Kubernetes resource is deleted.

| Policy | Behavior on delete |
|--------|--------------------|
| `Delete` (default) | User is dropped from PostgreSQL (`DROP USER`) |
| `Retain` | User remains in PostgreSQL, only Kubernetes resource is deleted |

**Note:** Unlike Database CRD, default is `Delete` since users are typically ephemeral.

## connectionLimit

Limit concurrent PostgreSQL connections per user:

```yaml
connectionLimit: 10  # max 10 concurrent connections
```

| Value | Meaning |
|-------|---------|
| `-1` | Unlimited (PostgreSQL default) |
| `0` | Not set (uses PostgreSQL default) |
| `>0` | Maximum concurrent connections |

**SQL Executed:**
```sql
ALTER USER <username> CONNECTION LIMIT <limit>;
```

## rotation

Automatic password rotation based on age:

```yaml
rotation:
  days: 30  # rotate every 30 days
```

| Value | Meaning |
|-------|---------|
| `1-365` | Rotate password after specified number of days |
| not set | No automatic rotation |

**How it works:**
1. Operator tracks when password was last set (`status.passwordUpdatedAt`)
2. On each reconcile, checks if age exceeds `rotation.days`
3. If expired: deletes Secret → regenerates password → updates PostgreSQL
4. Schedules next check via `RequeueAfter`

**Manual rotation:** Delete the generated Secret, operator will create new one with new password.

## Generated Secret

Operator creates a Secret with connection details:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: <databaseuser-name>-credentials
  namespace: <databaseuser-namespace>
  ownerReferences:
    - kind: DatabaseUser
      name: <databaseuser-name>
data:
  username: <base64>
  password: <base64>
  host: <base64>
  port: <base64>
  database: <base64>
```

**Lifecycle:**
- Secret deleted manually → operator recreates with NEW password
- DatabaseUser deleted → Secret deleted via ownerReference

## Database Isolation

**Critical security feature:** Users can ONLY connect to their assigned database.

When creating user, operator executes:
```sql
CREATE USER <username> WITH PASSWORD 'xxx' NOCREATEDB NOCREATEROLE NOINHERIT;
REVOKE CONNECT ON DATABASE postgres FROM <username>;
GRANT CONNECT ON DATABASE <target_db> TO <username>;
```

Operator verifies isolation on each reconcile and logs warning if user has unexpected access.

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | `Pending`, `Creating`, `Ready`, `Failed` |
| `message` | string | Detailed status message |
| `secretName` | string | Name of generated credentials Secret |
| `passwordUpdatedAt` | timestamp | When password was last created or rotated |
| `observedGeneration` | int64 | Which spec version has been processed |

## Existing User Handling

If PostgreSQL user already exists:
- **Adopt**: reset password, apply privileges
- Log warning: "adopting existing user"
- Secret is created/updated with new password

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

## Examples

### Readonly user for analytics

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: analytics-reader
  namespace: analytics
spec:
  databaseRef:
    name: main-db
    namespace: production
  privileges: readonly
```

### Application user with write access

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: my-app
  namespace: my-team
spec:
  databaseRef:
    name: my-app-db
  username: my_app_user
  privileges: readwrite
  password:
    length: 32
```

### Admin for migrations

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: my-app-migrations
  namespace: my-team
spec:
  databaseRef:
    name: my-app-db
  username: my_app_admin
  privileges: admin
```

### Readonly with specific table write access

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: audit-writer
  namespace: my-team
spec:
  databaseRef:
    name: my-app-db
  privileges: readonly
  additionalGrants:
    - tables: ["audit_log"]
      privileges: [INSERT]
```

### User with connection limit

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: limited-user
  namespace: my-team
spec:
  databaseRef:
    name: my-app-db
  privileges: readwrite
  connectionLimit: 5
```

### User with password rotation

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: rotating-user
  namespace: my-team
spec:
  databaseRef:
    name: my-app-db
  privileges: readwrite
  password:
    length: 32
  rotation:
    days: 30  # rotate every 30 days
```

## Troubleshooting

### Phase: Failed, message: "Database not found"

Check that `databaseRef.name` matches a Database resource:
```bash
kubectl get database -A
```

### Phase: Failed, message: "failed to create user"

1. Check that admin credentials in DBCluster have `CREATEUSER` privilege
2. Check operator logs for detailed error

### Phase: Failed, message: "failed to apply privileges"

1. Verify database exists and is Ready
2. Check that admin user has `GRANT OPTION` on target database

### User has access to unexpected databases

Check operator logs for isolation warnings:
```bash
kubectl logs -n postgres-db-operator deployment/postgres-db-operator | grep "unexpected databases"
```

