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
  ttlAfterCompletion: 24h  # Optional: auto-delete completed backup
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `databaseRef.name` | string | ✅ | — | Name of the Database resource |
| `databaseRef.namespace` | string | ❌ | same as Backup | Namespace of the Database |
| `storageRef.name` | string | ✅ | — | Name of the BackupStorage resource |
| `filenameTemplate` | string | ❌ | `{{ .Timestamp }}.sql.gz` | Backup filename template |
| `ttlAfterCompletion` | duration | ❌ | — | Auto-delete Backup CRD after completion |

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

**⚠️ Use with caution in GitOps environments!**

When set, the Backup CRD will be automatically deleted after the specified duration from completion.

| Value | Behavior |
|-------|----------|
| Not set | Backup CRD is retained indefinitely |
| `1h` | Deleted 1 hour after completion |
| `24h` | Deleted 24 hours after completion |

**Warning for ArgoCD/GitOps:** If the Backup manifest is in your Git repo and TTL deletes the resource, ArgoCD will recreate it, potentially triggering another backup. Consider:
- Using `specHash` (automatic) for idempotency
- Storing Backup manifests outside ArgoCD app
- Using BackupSchedule instead for recurring backups

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | Current state (`Pending`, `Running`, `Completed`, `Failed`) |
| `message` | string | Detailed message or error |
| `specHash` | string | Hash of spec (prevents re-runs on same config) |
| `jobName` | string | Name of the Kubernetes Job |
| `runId` | string | Unique run identifier |
| `path` | string | Full path to the backup file in storage |
| `size` | string | Backup file size (human-readable, e.g., `15.2 MiB`) |
| `duration` | string | Time taken to complete backup (e.g., `12s`) |
| `startedAt` | time | When backup started |
| `completedAt` | time | When backup completed |
| `observedGeneration` | int64 | Which spec version has been processed |

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
6. **Optional Cleanup:** If TTL set, Backup CRD auto-deletes after completion

### Idempotency

The controller computes a `specHash` of your spec. If you apply the same Backup twice:
- Same spec → no new backup (idempotent)
- Changed spec → new backup runs

This prevents accidental duplicate backups in GitOps workflows.

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

### Temporary Backup with TTL

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
  ttlAfterCompletion: 1h  # Auto-delete after 1 hour
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

### Phase: Failed, message: "Job already exists"

This can happen on race conditions. The controller will:
1. Find existing Job by labels
2. Update Backup status from Job status
3. Resolve automatically on next reconcile

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
```

