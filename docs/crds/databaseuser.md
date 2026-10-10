# DatabaseUser

A PostgreSQL role with a privilege preset on one or more [Databases](database.md), plus a generated password in a Kubernetes Secret. Namespaced.

## Examples

Minimal (Secret `my-app-readonly-credentials`, role `my_app_readonly`):

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: my-app-readonly
  namespace: my-team
spec:
  database:
    name: my-app-db
```

Full:

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
    - name: orders-db
      namespace: orders        # orders-db must list "platform" in allowedNamespaces
  privileges: readonly          # default for entries without their own
  additionalGrants:
    - tables: [audit_log]
      privileges: [INSERT]
  password:
    length: 32
  rotation:
    days: 30
  connectionLimit: 20
  idleInTransactionTimeout: 30s
  deletionPolicy: Delete
  secretGeneration: perDatabase
  secret:
    template: POSTGRES
```

## Spec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `database` | object | | One database: `name`, optional `namespace`, optional `privileges`. Exactly one of `database` / `databases` |
| `databases` | []object | | Several databases, same fields, at least one, each listed once, all on one DBCluster |
| `username` | string | `metadata.name` with `-` as `_` | `^[a-z_][a-z0-9_]*$`, max 63. Immutable |
| `privileges` | enum | `readonly` | `readonly`, `readwrite`, `admin`, `owner` (see below) |
| `additionalGrants[].tables` | []string | | Table names, `^[a-zA-Z_][a-zA-Z0-9_]*$`, max 63 |
| `additionalGrants[].privileges` | []string | | `SELECT`, `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, `REFERENCES`, `TRIGGER`, `USAGE` |
| `password.length` | int | `16` | 12-64 |
| `rotation.days` | int | unset | Rotate every N days, 1-365 |
| `connectionLimit` | int | `-1` | `-1` unlimited, `>= 1` caps; `0` is rejected |
| `idleInTransactionTimeout` | duration | unset | Sets `idle_in_transaction_session_timeout` on the role, min `1ms`. Unset clears it |
| `deletionPolicy` | enum | `Delete` | `Delete` drops the role; `Retain` keeps the role and its Secret |
| `secretGeneration` | enum | `primary` | `primary` or `perDatabase` |
| `secret.name` | string | `<name>-credentials` | Not allowed with `perDatabase` |
| `secret.template` | enum | `raw` | `raw`, `DB`, `DATABASE`, `POSTGRES`, `custom`, `dsn` |
| `secret.keys` | object | | Key names `host`, `port`, `database`, `username`, `password` for `template: custom` |
| `secret.onConflict` | enum | `Fail` | `Fail`, `Adopt`, `Merge`; only `Fail` with `perDatabase` |

## Status

| Phase | Meaning |
|-------|---------|
| `Pending` | A referenced Database or DBCluster is missing or not ready; `Failed` after 10 minutes |
| `Creating` | Creating the role or applying grants |
| `Ready` | Role, grants and Secret are in place |
| `Failed` | See `status.message`; `status.databases[]` has per-database phase and message |

A `Ready` user is re-checked every 10 minutes and when its DBCluster changes: a role dropped outside the operator is recreated with the Secret's password and privileges are re-applied.

## Privileges

Presets apply to the `public` schema, including default privileges for objects created later by the operator role and the database owner.

| Preset | Permissions |
|--------|-------------|
| `readonly` | `USAGE` on schema, `SELECT` on tables |
| `readwrite` | readonly + `INSERT`, `UPDATE`, `DELETE`, sequence `USAGE`/`SELECT` |
| `admin` | readwrite + `CREATE` on schema, `TRUNCATE`, `REFERENCES`, `TRIGGER` |
| `owner` | admin + ownership of the objects in `public` (`ALTER TABLE`, `pg_restore`) |

Every reconcile converges the role to exactly the preset plus `additionalGrants`; hand-made grants in `public` are revoked, so put them in `additionalGrants`. Objects the user owns are untouched.

## Secret

| Template | host | port | database | username | password |
|----------|------|------|----------|----------|----------|
| `raw` | `host` | `port` | `database` | `username` | `password` |
| `DB` | `DB_HOST` | `DB_PORT` | `DB_NAME` | `DB_USER` | `DB_PASSWORD` |
| `DATABASE` | `DATABASE_HOST` | `DATABASE_PORT` | `DATABASE_NAME` | `DATABASE_USER` | `DATABASE_PASSWORD` |
| `POSTGRES` | `POSTGRES_HOST` | `POSTGRES_PORT` | `POSTGRES_DATABASE` | `POSTGRES_USER` | `POSTGRES_PASSWORD` |
| `custom` | from `keys`, unset ones fall back to `raw` | | | | |
| `dsn` | one key `dsn` = `postgres://user:password@host:port/database` | | | | |

- `primary` writes one Secret for the first database; with `raw` and several databases it adds an informational `databases` key (comma-separated). `perDatabase` writes `<name>-<database>-credentials` per database, all with one password. It needs distinct Database names across namespaces.
- Secret contents follow the spec on every reconcile. Renaming the `password` key generates a new password, because the old value cannot be found; consumers must re-read the Secret.
- `onConflict` applies when the Secret exists and is not owned by this user: `Fail` touches nothing, `Adopt` takes it over and overwrites its data, `Merge` takes it over and keeps foreign keys. `Merge` sticks to the Secret through the `dbtether.io/conflict-policy: Merge` annotation, even if the field is later removed; to reset, delete the Secret or remove the annotation.
- Force a new password: delete the Secret; the operator regenerates it and applies it in PostgreSQL.

## Notes

- **Isolation.** Each reconcile grants `CONNECT` on the listed databases and revokes it on the others, so removing an entry removes access. A role still reaches databases through `PUBLIC` unless the Database sets [`revokePublicConnect`](database.md).
- **Cross-namespace.** A Database in another namespace must list this namespace in its `allowedNamespaces`; otherwise the entry is not granted (or is revoked) and the user goes `Failed`.
- **One `owner` per Database.** The oldest claimant wins (ties: namespace, then name). The others get `admin`, report `Failed` for that database, and keep working otherwise. Ownership transfer covers tables, sequences, views, types and routines in `public` that the user does not own. The operator grants itself membership in the role for this; on deletion or removal from the list, ownership returns to the operator role.

## Troubleshooting

| Message | Cause and fix |
|---------|---------------|
| Rejected: `exactly one of database or databases must be set` | Set one of the two |
| Rejected: `spec.secret.name cannot be set when secretGeneration=perDatabase` | Remove `secret.name` (and any `onConflict` other than `Fail`) |
| `waiting for Database '<name>'` (or `... to be ready`) | The Database is missing or not `Ready` in the resolved namespace; clears on its own |
| `database <ns>/<name> is listed more than once` | An entry without `namespace` means your own namespace; remove the duplicate |
| `per-database secrets need distinct Database names` | Two Databases share a name across namespaces; use `primary` or drop one |
| `all databases must be on the same cluster` | One user is one role on one cluster; create a DatabaseUser per cluster |
| `owner conflict: ... only one owner per database` | Another user holds `owner`. Lower this one to `admin` or delete the other |
| `secret error: ... already exists and is not owned by this DatabaseUser` | Rename with `secret.name`, delete the Secret, or set `onConflict` to `Adopt`/`Merge` |
| Access to unexpected databases | Inherited from `PUBLIC`; set `revokePublicConnect` on the Database |
