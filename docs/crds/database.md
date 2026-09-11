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
| `deletionPolicy` | enum | ❌ | `Retain` | `Retain` or `Delete` — what to do with the database when the resource is deleted |
| `revokePublicConnect` | bool | ❌ | `false` | Revoke CONNECT from PUBLIC role for isolation |

The DBCluster must be reachable through `credentialsSecretRef`; a cluster that only sets `credentialsFromEnv` cannot back a Database.

## databaseName

If not specified, the database name is derived from `metadata.name` with dashes (`-`) converted to underscores (`_`). The resolved name is reported in `status.databaseName`.

| Resource Name | databaseName (spec) | PostgreSQL Name |
|---------------|---------------------|-----------------|
| `my-app-db` | (not set) | `my_app_db` |
| `my-app-db` | `custom_name` | `custom_name` |

An explicit `databaseName` must match `^[a-z_][a-z0-9_]*$` and be at most 63 characters; admission rejects anything else.

- ✅ Valid: `my_app`, `users_v2`, `_internal`, `app123`
- ❌ Invalid: `My-App` (uppercase, hyphen), `123db` (starts with number), `user@db` (special char)

The derived form is not checked against that pattern, so an uppercase `metadata.name` reaches PostgreSQL as-is.

## deletionPolicy

| Policy | Behavior on `kubectl delete database` |
|--------|---------------------------------------|
| `Retain` (default) | Database **stays** in PostgreSQL. The operator clears its ownership marker so another Database resource can adopt it. |
| `Delete` | Operator terminates open connections, then runs `DROP DATABASE IF EXISTS`. **Data is lost.** |

Use `Retain` for production data and when importing an existing database; use `Delete` for feature-branch and test databases that should disappear with their namespace.

With `Delete`, the finalizer holds the resource until the drop succeeds — if PostgreSQL is unreachable the Kubernetes object stays until it can be dropped, rather than leaving an orphaned database behind.

## revokePublicConnect

In PostgreSQL the `PUBLIC` role holds `CONNECT` on every database by default, so any role can connect to any database without an explicit grant. Setting this to `true` runs:

```sql
REVOKE CONNECT ON DATABASE <dbname> FROM PUBLIC;
```

after which only roles with an explicit `GRANT CONNECT` — i.e. the `DatabaseUser` resources pointing at this Database — can connect.

The operator verifies the revoke took effect. `REVOKE` is silently a no-op when the operator's role does not own the database, so a failure here means an adopted legacy database whose PostgreSQL owner is someone else.

Leave it `false` when adopting an existing database whose applications rely on PUBLIC access.

## extensions

Each entry is created inside the database as a quoted identifier:

```sql
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm";
```

Common choices: `uuid-ossp` (UUID generation), `pg_trgm` (fuzzy search), `postgis` (geospatial), `hstore` (key-value), `btree_gin` / `btree_gist` (index types for scalars), `tablefunc` (crosstab), `pgcrypto` (cryptographic functions).

The extension must be installable on the server. On Aurora/RDS check the supported-extensions list for your engine version; some need `rds_superuser`.

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | Current resource state |
| `message` | string | Detailed message |
| `databaseName` | string | Resolved PostgreSQL database name |
| `observedGeneration` | int64 | Which spec version has been processed |
| `ownershipTracked` | bool | `false` when the operator could not mark the database as its own — see [Ownership](#ownership) |

## Status Phases

| Phase | Description |
|-------|-------------|
| `Pending` | The referenced DBCluster resource does not exist |
| `Waiting` | DBCluster exists but is not `Connected` |
| `Creating` | Creating the database |
| `Ready` | Database is ready for use |
| `Failed` | Error (see `message`) |
| `Deleting` | Resource is being deleted |

A resource that stays in `Pending` or `Waiting` for more than 10 minutes flips to `Failed` with `timeout: <message> (pending for over 10 minutes)`.

## Behavior

### Idempotency
An existing database is adopted, not recreated: the phase goes to `Ready` and extensions are applied on top. This is what makes importing an existing database and re-syncing from Git safe.

### Ownership
The operator records `namespace/name` in the database's PostgreSQL comment and refuses to reconcile a database already claimed by a different Database resource:

```
database my_app is owned by team-a/my-app-db, cannot be claimed by team-b/my-app-db (use annotation dbtether.io/force-adopt to override)
```

Setting `dbtether.io/force-adopt: "true"` on the Database transfers the claim.

Writing the comment requires being the PostgreSQL owner of the database. For an adopted database owned by another role the marker cannot be written, `status.ownershipTracked` becomes `false`, and two Database resources can then point at the same PostgreSQL database without the operator noticing. To enable tracking: `ALTER DATABASE <name> OWNER TO <operator_user>`.

### Retries
A missing DBCluster requeues after 30 s, an unconnected one after 20 s. Every failure against PostgreSQL is reported as `Failed` with `transient error (will retry): <message>` and requeued after 60 s, so a Database recovers on its own once the cluster comes back.

## kubectl Commands

```bash
kubectl get databases -A
kubectl describe database my-app-db -n my-namespace
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

### Isolated database

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: analytics-db
  namespace: data-team
spec:
  clusterRef:
    name: analytics-cluster
  databaseName: analytics
  revokePublicConnect: true   # only its DatabaseUsers can connect
  extensions:
    - uuid-ossp
    - btree_gin
  deletionPolicy: Retain
```

## Troubleshooting

Operator logs for every case below:

```bash
kubectl logs -n dbtether deployment/dbtether -f
```

### Phase: Pending, message: "waiting for DBCluster '&lt;name&gt;'"

No DBCluster with that name exists — `clusterRef.name` is a typo, or the cluster has not been applied yet. DBCluster is cluster-scoped, so there is no namespace to get wrong:

```bash
kubectl get dbcluster
```

### Phase: Waiting, message: "waiting for DBCluster '&lt;name&gt;' to be connected"

The DBCluster exists but is not `Connected`. Fix the cluster first — see [DBCluster troubleshooting](dbcluster.md#troubleshooting).

### Phase: Failed, message: "connection error: ..."

The operator could not build a client for the cluster: the credentials Secret is missing, lacks `username`/`password`, or the DBCluster uses `credentialsFromEnv` instead of `credentialsSecretRef`.

### Phase: Failed, message: "transient error (will retry): failed to create database: ..."

`CREATE DATABASE` failed. Check that the operator's role has `CREATEDB`, and that the name is valid for PostgreSQL.

If the message ends in `is owned by <ns>/<name>, cannot be claimed by ...`, another Database resource already claims that PostgreSQL database. Point one of them at a different `databaseName`, or add `dbtether.io/force-adopt: "true"` to the resource that should win.

### Phase: Failed, message: "transient error (will retry): failed to create extensions: ..."

The extension is not available on the server, or creating it needs a role the operator does not have (`rds_superuser` on RDS/Aurora). The database itself is already created; only the extension step is retrying.

### Phase: Failed, message: "transient error (will retry): failed to revoke public connect: public connect on database &lt;name&gt; could not be revoked (not owner)"

`revokePublicConnect: true` on a database the operator's role does not own. PostgreSQL accepts the `REVOKE` and changes nothing. Either hand the database over — `ALTER DATABASE <name> OWNER TO <operator_user>` — or drop the field.

### Phase: Failed, message: "timeout: ... (pending for over 10 minutes)"

The resource waited more than 10 minutes for its DBCluster. The original wait message is kept in the text; fix that cause and the Database returns on the next reconcile.
