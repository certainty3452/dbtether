# Restore

Restores a dbtether backup into a [Database](database.md) with a Job in the operator namespace, then re-applies the database's settings and its users' grants. Namespaced; short name `rst`. A Restore runs once and its `spec` is immutable; to restore again, create a new Restore.

## Examples

Minimal (latest backup of `orders-db` into an empty `orders-db-clone`):

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Restore
metadata:
  name: orders-clone
  namespace: my-team
spec:
  source:
    latestFrom:
      databaseRef:
        name: orders-db
  target:
    databaseRef:
      name: orders-db-clone
```

Full (specific backup from another namespace, replacing the target):

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Restore
metadata:
  name: refresh-staging
  namespace: staging
spec:
  source:
    backupRef:
      name: pre-deploy-20260120
      namespace: production     # its Database lists "staging" in allowedNamespaces
  target:
    databaseRef:
      name: orders-db
  onConflict: drop
  ttlAfterCompletion: 24h
```

From a file when the Backup resource is gone:

```yaml
spec:
  source:
    path: "production/orders_db/20260118-022000-9f3e1c7d.sql.gz"
    storageRef:
      name: production-backups
  target:
    databaseRef:
      name: forensics-copy
```

## Spec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `source.backupRef.name` | string | | A specific Backup. The Restore waits in `Pending` while it runs, fails if it `Failed` or has no `status.path` |
| `source.backupRef.namespace` | string | Restore's namespace | Namespace of that Backup |
| `source.latestFrom.databaseRef` | object | | The newest `Completed` Backup of that Database (by `completedAt`). Backups already pruned by retention are invisible |
| `source.latestFrom.namespace` | string | Restore's namespace | Where to search for Backups; also the default namespace of `databaseRef` |
| `source.path` | string | | Object key in storage. Requires `storageRef` |
| `source.storageRef.name` | string | | BackupStorage for `path` |
| `target.databaseRef.name` | string | required | Database to restore into; must be in the Restore's namespace |
| `onConflict` | enum | `fail` | `fail` or `drop` |
| `ttlAfterCompletion` | duration | `1h` | How long Kubernetes keeps the finished Job |

Set exactly one source: `backupRef`, `latestFrom`, or `path` with `storageRef`; admission rejects any other combination. A source Database in another namespace must list the Restore's namespace in its [`allowedNamespaces`](database.md); until it does the Restore stays `Pending`.

`onConflict`: `fail` aborts if the target holds any table, view, materialized view, sequence or foreign table outside the system schemas. `drop` runs `DROP DATABASE` and recreates it, destroying what is there. Both checks run in the Job against the live database.

## Status

| Phase | Meaning |
|-------|---------|
| `Pending` | Waiting for the source Backup, the target Database to be `Ready`, or another dependency; see `message` |
| `Running` | The Job is executing |
| `Granting` | Data is in; re-applying the Database spec and users' grants |
| `Completed` | Done; check `message` for whether grants came back too |
| `Failed` | See `message` |

Other fields: `sourcePath`, `jobName`, `runId`, `duration`, `jobSucceededAt`, `grantAttempts`, `startedAt`, `completedAt`.

## Notes

- **Transaction.** The Job streams storage, `gunzip`, `psql` as one transaction with `ON_ERROR_STOP=1`; any error rolls everything back. After a failed `drop` restore the target is empty. Only plain-format SQL dumps restore (what Backup produces); custom-format archives are rejected.
- **Grants after the restore.** Dumps are taken with `--no-owner --no-acl`, so the controller re-applies the target Database's ownership marker, `revokePublicConnect` and extensions, and for every DatabaseUser of that Database `CONNECT`, the privilege preset and `additionalGrants`. Users that are invalid, being deleted or without a role yet are skipped; their own controller grants them later. Rounds are retried every 30 seconds, up to 5.
- **After 5 failed rounds the Restore still reports `Completed`** because the data landed, with `message: restore completed; grants not re-applied for: <users>` (or `restore completed; grants not re-applied: <reason>`) and a `RestoreGrantsSkipped` warning event. Gate automation on the message or event, not on `phase` alone.
- **Credentials.** The DBCluster needs `credentialsSecretRef` in the operator namespace, see [BackupStorage](backupstorage.md#notes).
- **Throttling.** Restores share the per-cluster job limit with backups, see [Backup](backup.md#notes).
- **Client version.** See [Backup](backup.md#notes).
- **Error text.** `psql` errors in `message` can quote dump data to anyone who can read the Restore.

## Troubleshooting

| Message | Cause and fix |
|---------|---------------|
| Pending: `waiting for source: backup <name> is not completed yet` | Not an error; it starts when the Backup completes |
| Pending: `waiting for target database <name> ...` | The target is missing or not `Ready` in the Restore's namespace |
| Pending: `... must be in the operator namespace ...` or `DBCluster <name> has no credentialsSecretRef` | Use a `credentialsSecretRef` Secret in the operator namespace, see [BackupStorage](backupstorage.md#notes) |
| `no completed backup found for database <name>` | No `Completed` Backup with a path in the searched namespace. Set `latestFrom.namespace` or use `source.path` |
| `backup has no path in status` | The Backup's Job vanished before reporting. Use `source.path` or take a new backup |
| `database is not empty and onConflict=fail` | Use `onConflict: drop` or a fresh target |
| `failed to download backup: ...` | The operator identity cannot read the object, see [BackupStorage](backupstorage.md#required-permissions) |
| `backup contains no SQL statements` | Empty object, folder marker or a `source.path` typo. The target is untouched. `kubectl get rst <name> -o jsonpath='{.status.sourcePath}'` shows what was picked |
| `restore failed: psql failed: ... ERROR: ...` | A real restore error with the dump's line number: schema mismatch or an extension missing on the target. Logs: `kubectl logs -n dbtether job/$(kubectl get rst <name> -n <ns> -o jsonpath='{.status.jobName}')` |
| `ERROR: out of shared memory` | One transaction holds a lock per relation. Raise `max_locks_per_transaction` (RDS needs a reboot) and re-run |
| `restore job <name> disappeared without recording a result` | The Job was deleted or expired. Check the target database before creating a new Restore |
| Granting: `restore succeeded, retrying grants: ...` | The cluster is unreachable or a role was dropped mid-restore; see the 5-round rule above |
