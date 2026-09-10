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
| `source` | object | ✅ | — | Where to restore from. Exactly one of `backupRef`, `latestFrom`, or (`path` + `storageRef`) must be set |
| `target.databaseRef.name` | string | ✅ | — | Name of the Database to restore into |
| `target.databaseRef.namespace` | string | ❌ | same as Restore | Namespace of the target Database |
| `onConflict` | enum | ❌ | `fail` | What to do if the target database is not empty: `fail`, `drop` |
| `ttlAfterCompletion` | duration | ❌ | `1h` | How long Kubernetes keeps the finished restore Job |

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

The controller scans `Backup` resources in `latestFrom.namespace` (or the Restore's namespace) and picks the most recent `Completed` one referencing that database.

#### Mode C: direct path

Restore from a known object path in storage. Useful when the original `Backup` CRD is gone (TTL expired, deleted from cluster) but the file still exists.

```yaml
source:
  path: "production/orders_db/20260120-143022-a1b2c3d4.sql.gz"
  storageRef:
    name: production-backups
```

`storageRef` is required when `path` is used.

### onConflict

Behavior when the target database already exists or contains data.

| Policy | Behavior |
|--------|----------|
| `fail` (default) | Abort if the database is not empty. Safest. |
| `drop` | `DROP DATABASE` and recreate before restoring. Destroys whatever is there. |

### ttlAfterCompletion

Sets `ttlSecondsAfterFinished` on the Kubernetes Job that runs the restore. Same semantics as on [`Backup`](backup.md#ttlaftercompletion): Kubernetes deletes the Job and its pods that long after the Job finishes; the Restore resource and its status are untouched.

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | `Pending`, `Running`, `Completed`, `Failed` |
| `message` | string | Human-readable status / error |
| `specHash` | string | Hash of spec — prevents re-runs on the same config |
| `jobName` | string | Kubernetes Job created for this restore |
| `sourcePath` | string | Resolved storage path of the backup being restored |
| `duration` | string | Time taken (e.g., `47s`) |
| `runId` | string | Unique 8-character identifier for this run |
| `startedAt` | time | When the restore started |
| `completedAt` | time | When the restore completed |
| `observedGeneration` | int64 | Which spec version has been processed |

### Phases

| Phase | Description |
|-------|-------------|
| `Pending` | Restore created, source resolution in progress |
| `Running` | Restore job is executing |
| `Completed` | Restore finished successfully |
| `Failed` | Restore failed; see `message` |

## How it works

1. **Resolve source.** Controller resolves the source into a concrete `(storage, path)` pair:
   - `backupRef` → looks up the `Backup` CRD and reads its `status.path`
   - `latestFrom` → lists `Backup` resources for the database, picks latest `Completed`
   - `path` → uses the value directly with the referenced `BackupStorage`
2. **Generate RunID.** Controller generates an 8-char alphanumeric `runId`.
3. **Resolve target.** Reads the target `Database` to learn cluster connection + PG database name.
4. **Apply onConflict.** If `drop`, the database is dropped + recreated. If `fail`, restore aborts when the database is non-empty.
5. **Create Job.** Controller creates a Kubernetes Job named `restore-<name>-<runId>` that streams the object from storage → `gunzip` → `psql`. The whole dump runs as one transaction, so any error rolls everything back. `COMMENT ON EXTENSION` and `SET transaction_timeout` statements are skipped.
6. **Update status.** Phase, duration, and any error are recorded back onto the Restore CRD.

### Client version

Restore Jobs run the bundled `psql` closest to the target server's major version — the matching one, the oldest above it, or the newest available. The image bundles the PostgreSQL 16, 17 and 18 clients.

### Idempotency

The controller computes `specHash` of the spec. Re-applying the same Restore manifest does not re-trigger the job. To re-run, change something material (e.g., a label/annotation in spec, or the source).

## kubectl commands

```bash
# List restores
kubectl get restores -A
kubectl get rst -A  # short name

# Restore details
kubectl describe restore restore-orders -n my-team

# Watch progress
kubectl get rst -n my-team -w

# Find the Job
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

Drop and recreate the staging database from a specific pre-deploy backup.

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
```

### Short Job Retention

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Restore
metadata:
  name: throwaway-restore
  namespace: dev
spec:
  source:
    latestFrom:
      databaseRef:
        name: orders-db
  target:
    databaseRef:
      name: orders-db-scratch
  onConflict: drop
  ttlAfterCompletion: 1h  # Keep the finished Job for 1 hour
```

## Troubleshooting

### Phase: Failed, message: "no completed backup found for database"

`latestFrom` could not find a `Completed` Backup. Check:

```bash
kubectl get bkp -n <namespace> -l dbtether.io/database=<db-name>
```

If you expect a backup elsewhere, set `source.latestFrom.namespace` explicitly.

### Phase: Failed, message: "database is not empty and onConflict=fail"

`onConflict: fail` (default) refuses to restore into a database that holds any table, view, materialized view, sequence or foreign table outside the system schemas. Either:
- Set `onConflict: drop` to wipe and recreate, or
- Pick a fresh target Database.

### Phase: Failed, message: "failed to download backup: failed to download from S3: ..."

The operator's IRSA/Pod Identity role lacks read access to the backup object. See [BackupStorage troubleshooting](backupstorage.md#troubleshooting).

### Phase: Failed, message: "backup contains no SQL statements"

The resolved object holds nothing to run — an empty object, an S3 folder marker, or a `source.path` typo. The target is untouched, `drop` included. Check which object was picked:

```bash
kubectl get rst <name> -n <ns> -o jsonpath='{.status.sourcePath}'
```

### Phase: Failed, message: "backup is a pg_dump custom-format archive; only plain-format SQL dumps can be restored"

The object is a `pg_dump -Fc` archive, and only plain-format SQL dumps restore; `backup is not a plain-format SQL dump` says the same for any other binary content. The target is untouched — re-run against a dbtether backup or a `pg_dump --format=plain` file.

### Phase: Failed, message: "restore failed: psql failed: exit status 3: `psql:<stdin>:12`: ERROR: ..."

A real restore error, quoted from psql with the dump's line number; the `ERROR:` line can carry dump data to anyone who can read the Restore. The database is left as it was before the restore — empty after `onConflict: drop`. Pull logs:

```bash
JOB=$(kubectl get rst <name> -n <ns> -o jsonpath='{.status.jobName}')
kubectl logs -n dbtether job/$JOB
```

Common causes: schema-version mismatch, extension missing in target cluster, target user lacks `owner` privilege. Restored objects are created by the cluster admin. A DatabaseUser with `privileges: owner` owns the tables, sequences, views, types and routines in `public`; the restore runs that grant pass itself before it reports Completed. A DatabaseUser that loses the owner election gets `admin` instead ([DatabaseUser](databaseuser.md#privileges)).

### Phase: Failed, message: "restore failed: psql failed: exit status 3: `psql:<stdin>:812`: ERROR: out of shared memory"

The single transaction holds a lock per relation, and this dump has more relations than the cluster's lock table holds. Raise `max_locks_per_transaction` in the cluster's parameter group (RDS needs a reboot) and re-run the Restore.
