# Restore

Restores a database from a backup created by dbtether.

**API Version:** `dbtether.io/v1alpha1`
**Kind:** `Restore`
**Short name:** `rst`
**Scope:** Namespaced

## Example

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Restore
metadata:
  name: restore-orders
  namespace: my-team
spec:
  source:
    latestFrom:
      databaseRef:
        name: orders-db
  target:
    databaseRef:
      name: orders-db-restored
  onConflict: drop
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `source` | object | ✅ | — | Where to restore from. Exactly one of `backupRef`, `latestFrom`, or (`path` + `storageRef`) |
| `target.databaseRef.name` | string | ✅ | — | Name of the Database to restore into |
| `onConflict` | enum | ❌ | `fail` | What to do if the target database is not empty: `fail`, `drop` |
| `ttlAfterCompletion` | duration | ❌ | `1h` | How long Kubernetes keeps the finished restore Job |

The target Database must live in the Restore's own namespace; admission rejects `target.databaseRef.namespace`. The source may point anywhere.

### source

Exactly one mode must be chosen.

#### Mode A: explicit backup

Point at a specific `Backup` resource. Useful when you want a known-good snapshot.

```yaml
source:
  backupRef:
    name: orders-backup-20260120
    namespace: my-team  # optional, defaults to Restore's namespace
```

Resolves `status.path` on the referenced `Backup`. While that Backup is `Pending` or `Running`, the Restore waits in `Pending` instead of guessing at a result, and retries automatically once the Backup finishes. If the Backup is `Failed`, or `Completed` with no `status.path`, the Restore fails.

#### Mode B: latest from database

Automatically find the latest `Completed` backup for a database. Useful for "give me a fresh restore of prod" workflows.

```yaml
source:
  latestFrom:
    databaseRef:
      name: orders-db
    namespace: production  # optional, where to search for Backup resources
```

The controller scans every `Backup` in `latestFrom.namespace` (or the Restore's namespace), keeps those whose `spec.databaseRef.name` matches and that are `Completed` with a path, and picks the one with the newest `status.completedAt`. A backup whose CR has already been pruned by retention is invisible here — use Mode C for those.

#### Mode C: direct path

Restore from a known object path in storage. Useful when the original `Backup` CRD is gone (retention pruned it, TTL expired, deleted from cluster) but the file still exists.

```yaml
source:
  path: "production/orders_db/20260120-143022-a1b2c3d4.sql.gz"
  storageRef:
    name: production-backups
```

`storageRef` is required when `path` is used.

### onConflict

| Policy | Behavior |
|--------|----------|
| `fail` (default) | Abort if the database holds any table, view, materialized view, sequence or foreign table outside the system schemas. Safest. |
| `drop` | `DROP DATABASE` and recreate before restoring. Destroys whatever is there. |

Both checks run inside the restore Job, against the live database, not against the Database resource's status.

### ttlAfterCompletion

Sets `ttlSecondsAfterFinished` on the Kubernetes Job that runs the restore. Same semantics as on [`Backup`](backup.md#ttlaftercompletion): Kubernetes deletes the Job and its pods that long after the Job finishes; the Restore resource and its status are untouched.

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | `Pending`, `Running`, `Granting`, `Completed`, `Failed` |
| `message` | string | Human-readable status / error |
| `specHash` | string | Hash of spec — prevents re-runs on the same config |
| `jobName` | string | Kubernetes Job created for this restore |
| `sourcePath` | string | Resolved storage path of the backup being restored |
| `duration` | string | Time taken (e.g., `47s`) |
| `runId` | string | Unique 8-character identifier for this run |
| `grantAttempts` | int | Grant rounds spent so far, out of 5 |
| `startedAt` | time | When the restore started |
| `completedAt` | time | When the restore completed |
| `observedGeneration` | int64 | Which spec version has been processed |

### Phases

| Phase | Description |
|-------|-------------|
| `Pending` | Source not resolved yet — typically waiting on the referenced Backup |
| `Running` | Restore job is executing |
| `Granting` | Data is in; re-applying the Database spec and the users' grants |
| `Completed` | Restore finished; check `message` for whether grants came back too |
| `Failed` | Restore failed; see `message` |

## How it works

1. **Resolve source** into a `(storage, path)` pair, per the mode above, and **resolve target** by reading the target `Database` for its cluster and PostgreSQL name.
2. **Create Job** named `restore-<name>-<runId>` in the operator's namespace. Its `backoffLimit` is 0, so a failed restore is never silently retried by Kubernetes.
3. **The Job applies `onConflict`,** then streams the object from storage → `gunzip` → `psql`. The whole dump runs as one transaction under `ON_ERROR_STOP=1`, so any error rolls everything back — including the `BEGIN`/`COMMIT` pg_dump puts around large objects, which are stripped so they cannot commit early. `COMMENT ON EXTENSION` and `SET transaction_timeout` statements are skipped.
4. **Re-apply access** in the `Granting` phase, then report `Completed`.

### Grants after the restore

The dump is taken with `--no-owner --no-acl` and `onConflict: drop` recreates the database, so nothing that ran before the restore keeps its privileges. Before reporting `Completed` the controller therefore re-applies, against the live cluster:

- the target Database's own spec — ownership marker, `revokePublicConnect`, extensions;
- for every `DatabaseUser` referencing that Database: `CONNECT`, the privilege preset and any `additionalGrants`.

A user whose spec is invalid, that is being deleted, or whose PostgreSQL role does not exist yet is skipped — its own controller grants it on the next reconcile. Only one user can hold `owner`; the loser of that election gets `admin` here, same as anywhere else ([DatabaseUser](databaseuser.md#privileges)).

The whole round is bounded at 5 minutes and retried every 30 seconds, up to 5 rounds. **After 5 failed rounds the Restore still reports `Completed`** — the data did land — with `message: restore completed; grants not re-applied for: <users>` and a `RestoreGrantsSkipped` warning event. Automation that gates on `phase == Completed` alone will treat a half-restored database as good; gate on the message or the event too.

### Client version

Restore Jobs run the bundled `psql` closest to the target server's major version — the matching one, the oldest above it, or the newest available. The image bundles the PostgreSQL 16, 17 and 18 clients.

### Idempotency

The controller computes `specHash` of the spec. Re-applying the same Restore manifest does not re-trigger the job. To re-run, change something material (e.g., a label/annotation in spec, or the source).

## kubectl commands

```bash
kubectl get rst -A
kubectl describe restore restore-orders -n my-team
kubectl get rst -n my-team -w

# The Job lives in the operator's namespace
JOB=$(kubectl get rst restore-orders -n my-team -o jsonpath='{.status.jobName}')
kubectl logs -n dbtether job/$JOB
```

## Examples

### Restore the latest backup into a new database

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
  onConflict: fail  # target must be empty
```

### Pre-deploy snapshot restore into a staging copy

Drop and recreate the staging database from a specific pre-deploy backup taken in another namespace.

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
      namespace: production
  target:
    databaseRef:
      name: orders-db
  onConflict: drop
```

### Restore from a known path

When the `Backup` CRD is gone but the file still lives in S3.

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Restore
metadata:
  name: ad-hoc-restore
  namespace: incident-response
spec:
  source:
    path: "production/orders_db/20260118-022000-9f3e1c7d.sql.gz"
    storageRef:
      name: production-backups
  target:
    databaseRef:
      name: forensics-copy
  onConflict: fail
  ttlAfterCompletion: 24h  # keep the Job around to read its logs
```

## Troubleshooting

### Phase: Pending, message: "waiting for source: backup &lt;name&gt; is not completed yet (phase: &lt;phase&gt;)"

`backupRef` points at a Backup that has not finished. Not an error — the Restore starts by itself when the Backup completes.

### Phase: Failed, message: "failed to resolve source: no completed backup found for database &lt;name&gt;"

`latestFrom` found no `Completed` Backup with a path in the namespace it searched. Backups are matched on `spec.databaseRef.name` in that namespace only:

```bash
kubectl get bkp -n <namespace> -o custom-columns=NAME:.metadata.name,DB:.spec.databaseRef.name,PHASE:.status.phase,PATH:.status.path
```

Set `source.latestFrom.namespace` if the backups live elsewhere, or switch to `source.path` if retention already pruned the CR.

### Phase: Failed, message: "failed to resolve source: backup has no path in status"

The referenced Backup is `Completed` but never recorded where it wrote — its Job disappeared before reporting. Use `source.path`, or take a new backup.

### Phase: Failed, message: "target database not found: ..."

`target.databaseRef.name` names no Database in the Restore's namespace. Cross-namespace targets are rejected at admission, so this is always a name or namespace mistake.

### Phase: Failed, message: "database is not empty and onConflict=fail"

The target holds a table, view, materialized view, sequence or foreign table outside the system schemas. Either set `onConflict: drop` to wipe and recreate, or pick a fresh target Database.

### Phase: Failed, message: "failed to download backup: failed to download from S3: ..."

The operator's IRSA/Pod Identity role lacks read access to the backup object. See [BackupStorage troubleshooting](backupstorage.md#troubleshooting).

### Phase: Failed, message: "backup contains no SQL statements"

The resolved object holds nothing to run — an empty object, an S3 folder marker, or a `source.path` typo. The target is untouched, `drop` included. Check which object was picked:

```bash
kubectl get rst <name> -n <ns> -o jsonpath='{.status.sourcePath}'
```

### Phase: Failed, message: "backup is a pg_dump custom-format archive; only plain-format SQL dumps can be restored"

The object is a `pg_dump -Fc` archive, and only plain-format SQL dumps restore; `backup is not a plain-format SQL dump` says the same for any other binary content. The target is untouched — re-run against a dbtether backup or a `pg_dump --format=plain` file.

### Phase: Failed, message: "restore failed: psql failed: exit status 3: `psql:&lt;stdin&gt;:12`: ERROR: ..."

A real restore error, quoted from psql with the dump's line number; the `ERROR:` line can carry dump data to anyone who can read the Restore. The database is left as it was before the restore — empty after `onConflict: drop`. Common causes: a schema-version mismatch, or an extension the dump needs that is not installable on the target cluster. Pull the logs:

```bash
JOB=$(kubectl get rst <name> -n <ns> -o jsonpath='{.status.jobName}')
kubectl logs -n dbtether job/$JOB
```

### Phase: Failed, message: "restore failed: psql failed: exit status 3: `psql:&lt;stdin&gt;:812`: ERROR: out of shared memory"

The single transaction holds a lock per relation, and this dump has more relations than the cluster's lock table holds. Raise `max_locks_per_transaction` in the cluster's parameter group (RDS needs a reboot) and re-run the Restore.

### Phase: Failed, message: "restore job was deleted"

The Job vanished mid-run — its TTL elapsed while the operator was down, or someone deleted it. Whether the data landed is unknown; check the target database before re-running.

### Phase: Granting, message: "restore succeeded, retrying grants: ..."

The data is in but at least one user's grants failed; `status.grantAttempts` counts the rounds. The quoted error names each subject. Usually the cluster is unreachable or a role was dropped mid-restore. After 5 rounds the Restore completes anyway — see [Grants after the restore](#grants-after-the-restore).

### Phase: Completed, message: "restore completed; grants not re-applied: ..."

The target Database or its DBCluster was deleted while the restore was in flight, so there was nothing left to grant on. The data is in the database; re-apply the Database and its DatabaseUsers to restore access.
