# Backup

Represents a one-time database backup operation.

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `Backup`  
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

Configure Kubernetes Job parameters for backup execution.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `backoffLimit` | int | `3` | Number of retries before marking backup failed (0-10) |
| `activeDeadlineSeconds` | int | — | Hard timeout for the entire backup (seconds, min 60) |
| `ttlSecondsAfterFailed` | int | `43200` (12h) | Keep failed Job for debugging (seconds) |

**Example:**

```yaml
spec:
  jobConfig:
    backoffLimit: 5              # Retry up to 5 times
    activeDeadlineSeconds: 7200  # 2 hour timeout
    ttlSecondsAfterFailed: 86400 # Keep failed Job 24h for debugging
```

## filenameTemplate

Template for the backup filename.

**Available variables:**

| Variable | Description | Example |
|----------|-------------|---------|
| `.DatabaseName` | PostgreSQL database name | `orders_db` |
| `.Timestamp` | Timestamp in `YYYYMMDD-HHMMSS` format | `20260120-143022` |
| `.RunID` | Unique 8-character alphanumeric ID | `a1b2c3d4` |

**Examples:**

| Template | Result |
|----------|--------|
| `{{ .Timestamp }}.sql.gz` | `20260120-143022.sql.gz` |
| `{{ .DatabaseName }}-{{ .Timestamp }}.sql.gz` | `orders_db-20260120-143022.sql.gz` |
| `{{ .Timestamp }}-{{ .RunID }}.sql.gz` | `20260120-143022-a1b2c3d4.sql.gz` |
| `backup-{{ .RunID }}.sql.gz` | `backup-a1b2c3d4.sql.gz` |

### RunID

`RunID` is a unique 8-character alphanumeric identifier generated for each backup run. It provides:

- **Uniqueness:** Guarantees unique filenames even if timestamp collides
- **Traceability:** Same RunID appears in Job name, filename, and status
- **Correlation:** Easy to find Job by RunID: `kubectl get jobs -l dbtether.io/backup-name=<name>`

## ttlAfterCompletion

Sets `ttlSecondsAfterFinished` on the Kubernetes Job that runs the backup. Kubernetes deletes the Job and its pods that long after the Job finishes; the Backup resource and its status are untouched, and the uploaded backup file stays in storage.

| Value | Behavior |
|-------|----------|
| Not set | Job is deleted 1 hour after it finishes |
| `1h` | Job is deleted 1 hour after it finishes |
| `24h` | Job is deleted 24 hours after it finishes |

Raise it when you need more time to inspect Job logs after a run. Failed Jobs are kept on their own schedule; see `jobConfig.ttlSecondsAfterFailed`, which defaults to 12 hours.

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
| `phase` | enum | Current state (`Pending`, `Running`, `Completed`, `Failed`) |
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
| `failureMessage` | string | Detailed failure message |
| `failedAttempts` | int | Number of failed Job attempts |
| `lastPodName` | string | Name of the last Pod (for log retrieval) |

### Status Phases

| Phase | Description |
|-------|-------------|
| `Pending` | Backup created, waiting to start |
| `Running` | Backup job is executing |
| `Completed` | Backup finished successfully |
| `Failed` | Backup failed (see `message`) |

## How It Works

1. **Create Backup:** You create a Backup resource
2. **Generate RunID:** Controller generates unique 8-char RunID
3. **Create Job:** Controller creates a Kubernetes Job with name `backup-<name>-<runID>`
4. **Execute pg_dump:** Job runs `pg_dump` → compresses → uploads to storage
5. **Update Status:** Controller updates Backup status with path, size, duration
6. **Job Cleanup:** Kubernetes deletes the finished Job once `ttlAfterCompletion` elapses

### Idempotency

The controller computes a `specHash` of your spec. If you apply the same Backup twice:
- Same spec → no new backup (idempotent)
- Changed spec → new backup runs

This prevents accidental duplicate backups in GitOps workflows. Any field of the spec counts, so re-applying an unchanged manifest is free no matter how often your GitOps tool syncs it.

To re-run on purpose from an unchanged manifest, change [`trigger`](#trigger).

### Throttling

To prevent overloading the database, the operator limits concurrent backup jobs:
- **Default:** 3 concurrent backups per DBCluster
- Jobs are queued and retried with 30-second delay

## kubectl Commands

```bash
# List all backups
kubectl get backups -A
kubectl get bkp -A  # short name

# Backup details
kubectl describe backup orders-backup-20260120 -n my-team

# Watch backup progress
kubectl get bkp -n my-team -w

# Check backup path and size
kubectl get bkp orders-backup-20260120 -n my-team \
  -o jsonpath='{.status.path}{"\n"}{.status.size}'

# Find associated Job
kubectl get jobs -n dbtether -l dbtether.io/backup-name=orders-backup-20260120
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

### Short Job Retention

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Backup
metadata:
  name: test-backup
  namespace: dev-team
spec:
  databaseRef:
    name: test-db
  storageRef:
    name: dev-backups
  ttlAfterCompletion: 1h  # Keep the finished Job for 1 hour
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

Backups are created using `pg_dump` with the following settings:
- **Format:** Plain SQL
- **Compression:** gzip (`.sql.gz`)
- **Encoding:** UTF-8

### S3 Object Tags

When uploading to S3, the operator adds metadata tags (best-effort):

| Tag | Value |
|-----|-------|
| `dbtether.io/backup-name` | Backup resource name |
| `dbtether.io/backup-namespace` | Backup namespace |
| `dbtether.io/database` | Database name |
| `dbtether.io/cluster` | DBCluster name |
| `dbtether.io/created-by` | `dbtether-operator` |

> **Note:** Tags require `s3:PutObjectTagging` permission. If missing, backup succeeds without tags.

## Troubleshooting

### Phase: Pending (stuck)

1. Check if Database exists and is Ready:
   ```bash
   kubectl get database orders-db -n my-team
   ```

2. Check if BackupStorage exists and is Ready:
   ```bash
   kubectl get backupstorage production-backups
   ```

3. Check operator logs:
   ```bash
   kubectl logs -n dbtether deployment/dbtether -f
   ```

### Phase: Failed, message: "backup throttled"

Too many concurrent backups for this cluster. The backup will be automatically retried in 30 seconds.

### Phase: Failed, message: "S3 upload failed: AccessDenied"

Check IAM permissions. See [BackupStorage Troubleshooting](backupstorage.md#troubleshooting).

### Finding the Backup File

```bash
# Get the full path
kubectl get bkp my-backup -n my-team -o jsonpath='{.status.path}'
# Output: s3://my-bucket/production/orders_db/20260120-143022-a1b2c3d4.sql.gz

# Download from S3
aws s3 cp "$(kubectl get bkp my-backup -n my-team -o jsonpath='{.status.path}')" ./backup.sql.gz
```

### Checking Backup Job Logs

```bash
# Find the job
JOB=$(kubectl get bkp my-backup -n my-team -o jsonpath='{.status.jobName}')

# Get logs
kubectl logs -n dbtether job/$JOB

# Or use lastPodName from status
POD=$(kubectl get bkp my-backup -n my-team -o jsonpath='{.status.lastPodName}')
kubectl logs -n dbtether $POD
```

### Viewing Kubernetes Events

The operator emits events for backup lifecycle:

```bash
# Events for a specific backup
kubectl get events -n my-team --field-selector involvedObject.name=my-backup

# Events: BackupStarted, BackupCompleted, BackupFailed
```

| Event | Reason | Description |
|-------|--------|-------------|
| Normal | `BackupStarted` | Backup job created |
| Normal | `BackupCompleted` | Backup finished successfully |
| Warning | `BackupFailed` | Backup failed (includes reason and message) |