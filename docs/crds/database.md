# Database

A PostgreSQL database inside a [DBCluster](dbcluster.md). Namespaced. An existing database is adopted, not recreated, so re-applying from Git is safe.

## Examples

Minimal (creates `my_app_db`):

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: my-app-db
  namespace: my-team
spec:
  clusterRef:
    name: my-cluster
```

Full:

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: orders-db
  namespace: orders
spec:
  clusterRef:
    name: my-cluster
  databaseName: orders
  extensions:
    - uuid-ossp
    - pg_trgm
  deletionPolicy: Retain
  revokePublicConnect: true
  allowedNamespaces:
    - analytics
```

Entries are names or `*` patterns (e.g. `box-*`); `allowedNamespaces: ["*"]` allows any namespace.

## Spec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `clusterRef.name` | string | required | DBCluster name. Immutable |
| `databaseName` | string | `metadata.name` with `-` as `_` | PostgreSQL name, `^[a-z_][a-z0-9_]*$`, max 63. Immutable once set; reported in `status.databaseName` |
| `extensions` | []string | `[]` | Extensions to `CREATE EXTENSION IF NOT EXISTS` |
| `deletionPolicy` | enum | `Retain` | `Retain` or `Delete` |
| `revokePublicConnect` | bool | `false` | Run `REVOKE CONNECT ... FROM PUBLIC` |
| `allowedNamespaces` | []string | `[]` | Other namespaces whose DatabaseUsers, Backups, BackupSchedules and Restores may reference this Database. Entries are names or `*` patterns (e.g. `box-*`); its own namespace is always allowed; `["*"]` allows any namespace |

The derived name is not validated, so an uppercase `metadata.name` reaches PostgreSQL as is.

## Status

| Phase | Meaning |
|-------|---------|
| `Pending` | The DBCluster does not exist |
| `Waiting` | The DBCluster is not `Connected` |
| `Creating` | Creating the database |
| `Ready` | Usable |
| `Failed` | See `status.message` |
| `Deleting` | Being deleted |

`Pending` or `Waiting` for over 10 minutes becomes `Failed` (`timeout: ... (pending for over 10 minutes)`). Every failure is retried after 60 seconds.

## Notes

- **Deletion.** `Retain` leaves the database in PostgreSQL and clears its ownership marker. `Delete` terminates open connections and runs `DROP DATABASE` (data is lost); the resource stays until the drop succeeds. Both act only on a database whose marker names this resource; otherwise nothing is touched and the finalizer is removed.
- **Ownership.** The operator stores `namespace/name` in the database comment and refuses a database claimed by another Database resource. Annotate with `dbtether.io/force-adopt: "true"` to take it over. A comment that is not a dbtether marker is never overwritten; clear it with `COMMENT ON DATABASE <name> IS NULL`.
- **Writing the marker requires owning the database.** For an adopted database owned by another role, `status.ownershipTracked` is `false` and two resources can point at the same database unnoticed. Fix: `ALTER DATABASE <name> OWNER TO <operator_user>`.
- **`revokePublicConnect`.** Without it every role can connect through `PUBLIC`, so a DatabaseUser's `CONNECT` grant isolates nothing. The operator verifies the revoke; it fails (`not owner`) on databases the operator does not own. Leave it off when adopting a database whose applications rely on `PUBLIC`.
- **Cross-namespace access.** Removing a namespace from `allowedNamespaces` revokes the access its DatabaseUsers had.

  ```yaml
  # DatabaseUser in namespace analytics
  spec:
    database:
      name: orders-db
      namespace: orders
  ```

- **Extensions** must be installable on the server. On Aurora/RDS some need `rds_superuser`.

## Troubleshooting

| Message | Cause and fix |
|---------|---------------|
| `waiting for DBCluster '<name>'` | No DBCluster with that name: typo or not applied yet |
| `waiting for DBCluster '<name>' to be connected` | Fix the cluster first, see [DBCluster](dbcluster.md#troubleshooting) |
| `connection error: ...` | Credentials Secret missing or lacking `username`/`password`, or an env var of `credentialsFromEnv` is unset |
| `failed to create database: ...` | The operator role lacks `CREATEDB`, or the name is invalid |
| `... is owned by <ns>/<name>, cannot be claimed by ...` | Another Database claims it. Use a different `databaseName` or add `force-adopt` to the winner |
| `... has a foreign comment ...` | Clear the comment (see Ownership) |
| `failed to create extensions: ...` | Extension unavailable, or needs `rds_superuser`. The database exists; only the extension step retries |
| `failed to revoke public connect: ... (not owner)` | The operator does not own the database. Hand it over with `ALTER DATABASE ... OWNER TO` or drop the field |
