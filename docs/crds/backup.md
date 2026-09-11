# Backup

Represents a one-time database backup operation.

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `Backup`  
**Short name:** `bkp`  
**Scope:** Namespaced

## Example

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Backup
metadata:
  name: orders-backup-20260120
  namespace: my-team
spec:
  databaseRef:
    name: orders-db
    namespace: my-team
  storageRef:
    name: production-backups
  filenameTemplate: "{{ .Timestamp }}-{{ .RunID }}.sql.gz"
  ttlAfterCompletion: 24h  # Optional: how long to keep the finished Job
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `databaseRef.name` | string | ✅ | — | Name of the Database resource |
| `databaseRef.namespace` | string | ❌ | same as Backup | Namespace of the Database |
| `storageRef.name` | string | ✅ | — | Name of the BackupStorage resource |
| `filenameTemplate` | string | ❌ | `{{ .Timestamp }}.sql.gz` | Backup filename template |
| `trigger` | string | ❌ | — | Opaque value that only feeds the spec hash; change it to run the backup again |
| `ttlAfterCompletion` | duration | ❌ | `1h` | How long Kubernetes keeps the finished backup Job |
| `jobConfig` | object | ❌ | — | Kubernetes Job configuration |

### jobConfig

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `backoffLimit` | int | `3` | Number of retries before marking backup failed (0-10) |
| `activeDeadlineSeconds` | int | — | Hard timeout for the entire backup (seconds, min 60) |
| `ttlSecondsAfterFailed` | int | `43200` (12h) | Keep failed Job for debugging (seconds) |

```yaml
spec:
  jobConfig:
    backoffLimit: 5              # Retry up to 5 times
    activeDeadlineSeconds: 7200  # 2 hour timeout
    ttlSecondsAfterFailed: 86400 # Keep failed Job 24h for debugging
```

## filenameTemplate

| Variable | Description | Example |
|----------|-------------|---------|
| `.DatabaseName` | PostgreSQL database name | `orders_db` |
| `.Timestamp` | UTC timestamp in `YYYYMMDD-HHMMSS` format | `20260120-143022` |
| `.RunID` | Unique 8-character lowercase alphanumeric ID | `a1b2c3d4` |

| Template | Result |
|----------|--------|
| `{{ .Timestamp }}.sql.gz` | `20260120-143022.sql.gz` |
| `{{ .DatabaseName }}-{{ .Timestamp }}.sql.gz` | `orders_db-20260120-143022.sql.gz` |
| `{{ .Timestamp }}-{{ .RunID }}.sql.gz` | `20260120-143022-a1b2c3d4.sql.gz` |
| `backup-{{ .RunID }}.sql.gz` | `backup-a1b2c3d4.sql.gz` |

A fresh `RunID` is generated per run and appears in the Job name, the filename and `status.runId`, which is what ties the three together when several runs of the same Backup are in flight.

Directories come from the BackupStorage's [`pathTemplate`](backupstorage.md#pathtemplate); `filenameTemplate` is only the last segment. A schedule's retention only deletes files matching its own `filenameTemplate` — see [BackupSchedule retention](backupschedule.md#retention).

## ttlAfterCompletion

Sets `ttlSecondsAfterFinished` on the Kubernetes Job that runs the backup: Kubernetes deletes the Job and its pods that long after the Job finishes. The Backup resource, its status and the uploaded file are untouched.

Each attempt runs in its own Pod and failed Pods are kept until the TTL, so their logs survive. Raise the value when you need more time to read them. A Job that ends in failure switches to `jobConfig.ttlSecondsAfterFailed` instead, which defaults to 12 hours.

## trigger

An opaque string that only feeds the spec hash. Change it to run the backup again from the same manifest; the operator gives it no other meaning.

Use it when one Backup manifest lives in git and must run before every deployment:

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Backup
metadata:
  name: orders-pre-deploy
  namespace: orders-team
spec:
  databaseRef:
    name: orders-db
  storageRef:
    name: production-backups
  filenameTemplate: pre-deploy.sql.gz  # stable name: every run overwrites the same object
  trigger: "v4.6.1"                    # the image tag being deployed
```

| Event | What the operator does |
|-------|------------------------|
| Manifest re-applied, `trigger` unchanged | Nothing, however often it is synced |
| `trigger` changed after the last run finished | A new run starts |
| `trigger` changed while a run is active | The active run finishes first, then a new run starts; runs never overlap |
| `trigger` set back to an earlier value | A new run; any change counts, not the value itself |

The operator writes `status.observedGeneration` when a run starts, not when it finishes. A health check must require both `observedGeneration == metadata.generation` and `phase == Completed`; `observedGeneration` alone turns true while the run is still `Running`.

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | `Pending`, `Running`, `Completed`, `Failed` |
| `message` | string | Detailed message or error |
| `specHash` | string | Hash of spec (prevents re-runs on same config) |
| `jobName` | string | Name of the Kubernetes Job |
| `runId` | string | Unique run identifier |
| `path` | string | Full path to the backup file produced by the current run |
| `size` | string | Backup file size (human-readable, e.g., `15.2 MiB`) |
| `duration` | string | Time taken to complete backup (e.g., `12s`) |
| `startedAt` | time | When backup started |
| `completedAt` | time | When backup completed |
| `observedGeneration` | int64 | The spec generation the current run started with |
| `failureReason` | string | Machine-readable failure reason (e.g., `BackoffLimitExceeded`) |
| `failureMessage` | string | What the backup Job reported, or the Job's condition message |
| `failedAttempts` | int | Number of failed Job attempts |
| `lastPodName` | string | Name of the last Pod (for log retrieval) |

### Status Phases

| Phase | Description |
|-------|-------------|
| `Pending` | Waiting on a dependency, a free job slot, or Job creation |
| `Running` | Backup job is executing |
| `Completed` | Backup finished successfully |
| `Failed` | Backup failed (see `message` and `failureMessage`) |

## How It Works

1. Controller resolves `databaseRef`, its DBCluster and `storageRef`, and requires all three to be ready.
2. It generates an 8-character `runId` and creates a Job named `backup-<name>-<runId>` in the operator's namespace.
3. The Job streams `pg_dump` → `gzip` → object storage without staging the dump on disk.
4. The Job reports path, size and duration back through its own annotations; the controller copies them into the Backup status.
5. Kubernetes deletes the finished Job once `ttlAfterCompletion` elapses.

### Idempotency

The controller hashes the spec. Applying the same Backup again does nothing, no matter how often a GitOps tool syncs it; changing any spec field starts a new run. To re-run from an unchanged manifest, change [`trigger`](#trigger).

### Throttling

At most 3 backup Jobs per DBCluster run at once (`backup.maxConcurrentPerCluster` in the chart). Over the limit the Backup sits in `Pending` and is retried every 30 seconds.

## kubectl Commands

```bash
kubectl get bkp -A
kubectl describe backup orders-backup-20260120 -n my-team
kubectl get bkp -n my-team -w

# Path and size of the last run
kubectl get bkp orders-backup-20260120 -n my-team \
  -o jsonpath='{.status.path}{"\n"}{.status.size}'

# The Job lives in the operator's namespace, not the Backup's
kubectl get jobs -n dbtether -l dbtether.io/backup=orders-backup-20260120
```

## Examples

### Simple Backup

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Backup
metadata:
  name: daily-orders-20260120
  namespace: orders-team
spec:
  databaseRef:
    name: orders-db
  storageRef:
    name: production-backups
```

### Backup with Custom Filename

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Backup
metadata:
  name: pre-migration-backup
  namespace: orders-team
spec:
  databaseRef:
    name: orders-db
  storageRef:
    name: production-backups
  filenameTemplate: "pre-migration-{{ .Timestamp }}-{{ .RunID }}.sql.gz"
```

### Cross-namespace Database Reference

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Backup
metadata:
  name: shared-db-backup
  namespace: backup-team
spec:
  databaseRef:
    name: shared-db
    namespace: shared-resources  # Different namespace
  storageRef:
    name: central-backups
```

### Large Database with Custom Timeouts

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Backup
metadata:
  name: large-db-backup
  namespace: data-team
spec:
  databaseRef:
    name: large-analytics-db
  storageRef:
    name: company-s3
  jobConfig:
    backoffLimit: 5              # Retry up to 5 times on failure
    activeDeadlineSeconds: 7200  # 2 hour timeout
    ttlSecondsAfterFailed: 86400 # Keep failed Job 24h for debugging
```

## Backup File Details

### Format

`pg_dump --format=plain --no-owner --no-acl`, piped through gzip. `--no-owner --no-acl` is why a restore has to re-apply ownership and grants itself — see [Grants after the restore](restore.md#grants-after-the-restore).

### Client version

Backup Jobs run the bundled `pg_dump` matching the source server's major version, or the oldest bundled one above it. The image bundles the PostgreSQL 16, 17 and 18 clients; a newer server fails the Job with `no pg_dump for a server on major <N>` naming the bundled majors.

### S3 Object Tags

Uploads to S3 carry these tags, best-effort:

| Tag | Value |
|-----|-------|
| `database` | PostgreSQL database name |
| `cluster` | DBCluster name |
| `backup-name` | Backup resource name |
| `namespace` | Backup namespace |
| `timestamp` | Run timestamp, `YYYYMMDD-HHMMSS` |
| `created-by` | `dbtether` |

Tagging needs `s3:PutObjectTagging`. Without it the upload is retried untagged and the backup still succeeds.

## Troubleshooting

### Phase: Pending, message: "database &lt;name&gt; is not ready (phase: &lt;phase&gt;)"

A dependency is not usable yet. The same shape covers `database <name> not found`, `cluster <name> not found`, `cluster <name> is not connected`, `backup storage <name> not found` and `backup storage <name> is not ready (phase: <phase>)`. Fix the named resource; the Backup starts on its own.

```bash
kubectl get database orders-db -n my-team
kubectl get backupstorage production-backups
```

### Phase: Pending, message: "waiting for other backups to complete (active: 3/3)"

The DBCluster is at its concurrent-job limit. Retried every 30 seconds; raise `backup.maxConcurrentPerCluster` if the queue never drains.

### Phase: Pending, message: "failed to create job: ..."

The API server rejected the Job — usually a quota, an admission policy, or invalid pod labels/annotations supplied through the chart's `backup.podLabels` / `backup.podAnnotations`.

### Phase: Failed, message: "backup job failed: ..."

The Job itself failed; `status.failureMessage` holds what the container reported and `status.failureReason` the Kubernetes condition (`BackoffLimitExceeded`, `DeadlineExceeded`). `upload failed: failed to upload to S3: ...` here means IAM — see [BackupStorage troubleshooting](backupstorage.md#troubleshooting). Pull the logs:

```bash
POD=$(kubectl get bkp my-backup -n my-team -o jsonpath='{.status.lastPodName}')
kubectl logs -n dbtether $POD
```

### Phase: Failed, message: "backup job not found"

The Job was deleted before its result was recorded — a TTL that elapsed while the operator was down, or a manual delete. Whether the file reached storage is unknown; check the bucket, then re-run by changing [`trigger`](#trigger).

### Finding the Backup File

```bash
kubectl get bkp my-backup -n my-team -o jsonpath='{.status.path}'
# s3://my-bucket/production/orders_db/20260120-143022-a1b2c3d4.sql.gz

aws s3 cp "$(kubectl get bkp my-backup -n my-team -o jsonpath='{.status.path}')" ./backup.sql.gz
```

### Events

```bash
kubectl get events -n my-team --field-selector involvedObject.name=my-backup
```

| Type | Reason | Meaning |
|------|--------|---------|
| Normal | `BackupStarted` | Backup job created |
| Normal | `BackupCompleted` | Backup finished successfully |
| Warning | `BackupFailed` | Backup failed (carries reason and message) |
