# Database

Represents a database within a DBCluster.

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `Database`  
**Scope:** Namespaced

## Example

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: my-app-db          # creates "my_app_db" in PostgreSQL
  namespace: my-team
spec:
  clusterRef:
    name: my-cluster
  # databaseName: optional, only needed if different from metadata.name
  extensions:
    - uuid-ossp
    - pg_trgm
  deletionPolicy: Retain
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `clusterRef.name` | string | ✅ | — | Name of the DBCluster resource |
| `databaseName` | string | ❌ | `metadata.name` | Database name in PostgreSQL (see below) |
| `extensions` | []string | ❌ | `[]` | List of PostgreSQL extensions to install |
| `deletionPolicy` | enum | ❌ | `Retain` | What to do with the database when resource is deleted |
| `revokePublicConnect` | bool | ❌ | `false` | Revoke CONNECT from PUBLIC role for isolation |

## databaseName

**Optional.** If not specified, the database name is derived from `metadata.name` with dashes (`-`) converted to underscores (`_`).

| Resource Name | databaseName (spec) | PostgreSQL Name |
|---------------|---------------------|-----------------|
| `my-app-db` | (not set) | `my_app_db` |
| `my-app-db` | `custom_name` | `custom_name` |

Use explicit `databaseName` only when you need a different name than the resource name.

Database name must follow PostgreSQL naming rules:

| Constraint | Value |
|------------|-------|
| Pattern | `^[a-z_][a-z0-9_]*$` |
| Max length | 63 |

**Examples:**
- ✅ Valid: `my_app`, `users_v2`, `_internal`, `app123`
- ❌ Invalid: `My-App` (uppercase, hyphen), `123db` (starts with number), `user@db` (special char)

## deletionPolicy

Determines what happens to the PostgreSQL database when the Kubernetes resource is deleted.

| Policy | Behavior on `kubectl delete database` |
|--------|-------------------------------|
| `Retain` | Database **stays** in PostgreSQL. Resource deleted, data preserved. |
| `Delete` | Database **is dropped** from PostgreSQL (`DROP DATABASE`). **Data is lost!** |

### When to use Retain (default)
- Production databases
- Data that cannot be lost
- Importing existing databases

### When to use Delete
- Feature branch databases
- Test environments
- Temporary databases

**⚠️ Warning:** Before deleting with `deletionPolicy: Delete`:
1. Ensure there are no active connections
2. Create a backup if needed
3. Operator will execute `DROP DATABASE IF EXISTS`

## revokePublicConnect

Controls database isolation by revoking `CONNECT` privilege from the `PUBLIC` role.

| Value | Behavior |
|-------|----------|
| `false` (default) | All PostgreSQL users can connect (standard PostgreSQL/AWS behavior) |
| `true` | Only users with explicit `GRANT CONNECT` can access the database |

**SQL executed when `true`:**
```sql
REVOKE CONNECT ON DATABASE <dbname> FROM PUBLIC;
```

**Why this matters:** In PostgreSQL, the `PUBLIC` role (which all users inherit) has `CONNECT` privilege on all databases by default. This means any user can connect to any database, even without explicit grants. Setting `revokePublicConnect: true` ensures only `DatabaseUser` resources with explicit access can connect.

**When to use `true`:**
- New databases that need strict isolation
- Multi-tenant environments
- Security-sensitive applications

**When to keep `false` (default):**
- Adopting existing databases where applications rely on PUBLIC access
- Shared databases accessed by many users without explicit grants
- Legacy systems

## extensions

Operator creates extensions inside the database:

```sql
CREATE EXTENSION IF NOT EXISTS uuid-ossp;
CREATE EXTENSION IF NOT EXISTS pg_trgm;
```

### Popular extensions

| Extension | Description |
|-----------|-------------|
| `uuid-ossp` | UUID generation (`uuid_generate_v4()`) |
| `pg_trgm` | Trigram matching for fuzzy search |
| `postgis` | Geospatial data |
| `hstore` | Key-value storage |
| `btree_gin` | GIN indexes for scalar types |
| `btree_gist` | GiST indexes for scalar types |
| `tablefunc` | Crosstab and other functions |
| `pgcrypto` | Cryptographic functions |

**Important:** Extension must be available in PostgreSQL. For Aurora/RDS — check supported extensions in AWS docs.

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | Current resource state |
| `message` | string | Detailed message |
| `observedGeneration` | int64 | Which spec version has been processed |

## Status Phases

| Phase | Description |
|-------|-------------|
| `Pending` | Initial state |
| `Waiting` | Waiting for DBCluster to become `Connected` |
| `Creating` | Creating the database |
| `Ready` | Database is ready for use |
| `Failed` | Error (see `message`) |
| `Deleting` | Deleting database (when `deletionPolicy: Delete`) |

## Behavior

### Idempotency
If database already exists in PostgreSQL:
- Operator does **not** try to recreate it
- Status becomes `Ready`
- Extensions are applied (if not already installed)

This allows:
- Importing existing databases
- Safe retry on errors
- GitOps workflow

### Finalizers
Operator adds finalizer `dbtether.io/finalizer`:
- Ensures `DROP DATABASE` executes before resource deletion
- Prevents "orphaned" databases when `deletionPolicy: Delete`

### Fail-fast
If DBCluster is not in `Connected` status:
- Database resource transitions to `Waiting`
- Retry every 10 seconds
- Does not attempt to create database until cluster is available

### Transient Errors
On temporary errors (network, timeout):
- Status `Failed` with message "(will retry)"
- Retry after 30 seconds

## kubectl Commands

```bash
# List all databases
kubectl get databases -A
kubectl get database -n my-namespace

# Database details
kubectl describe database my-app-db -n my-namespace

# Check status
kubectl get database my-app-db -n my-namespace -o jsonpath='{.status.phase}'
```

## Examples

### Import existing database

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: legacy-db
  namespace: default
spec:
  clusterRef:
    name: production-cluster
  databaseName: existing_app_db   # already exists
  deletionPolicy: Retain          # never delete!
```

### Development database (auto-delete)

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: feature-xyz-db
  namespace: dev
spec:
  clusterRef:
    name: dev-cluster
  databaseName: feature_xyz
  extensions:
    - uuid-ossp
  deletionPolicy: Delete   # will be deleted with namespace/PR
```

### Full-featured database

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: analytics-db
  namespace: data-team
  labels:
    team: data
    environment: production
spec:
  clusterRef:
    name: analytics-cluster
  databaseName: analytics
  extensions:
    - uuid-ossp
    - pg_trgm
    - btree_gin
    - tablefunc
    - hstore
  deletionPolicy: Retain
```

## Troubleshooting

### Phase: Waiting

DBCluster is not ready:
```bash
kubectl get dbcluster <cluster-name> -o jsonpath='{.status.phase}'
```

### Phase: Failed, message: "DBCluster not found"

Check that `clusterRef.name` matches a DBCluster name:
```bash
kubectl get dbcluster
```

### Phase: Failed, message: "failed to create database"

1. Check that database name is valid (lowercase, no special chars)
2. Check user privileges (`CREATEDB`)
3. Check operator logs:
   ```bash
   kubectl logs -n postgres-db-operator deployment/postgres-db-operator -f
   ```

### Phase: Failed, message: "failed to create extensions"

1. Check that extension is available in PostgreSQL
2. For Aurora — check supported extensions in AWS docs
3. Some extensions require `rds_superuser` role
