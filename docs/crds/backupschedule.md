# BackupSchedule

Represents a scheduled backup policy for automatic database backups.

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `BackupSchedule`  
**Scope:** Namespaced

## Example

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupSchedule
metadata:
  name: orders-nightly
  namespace: orders-team
spec:
  databaseRef:
    name: orders-db
  storageRef:
    name: company-s3
  schedule: "0 2 * * *"  # 2 AM daily
  retention:
    keepLast: 7
    keepDaily: 30
    keepWeekly: 12
    keepMonthly: 12
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `databaseRef.name` | string | ✅ | — | Name of the Database resource |
| `databaseRef.namespace` | string | ❌ | same as Schedule | Namespace of the Database |
| `storageRef.name` | string | ✅ | — | Name of the BackupStorage resource |
| `schedule` | string | ✅ | — | Cron schedule (5 fields) |
| `filenameTemplate` | string | ❌ | `{{ .Timestamp }}.sql.gz` | Backup filename template |
| `retention` | object | ❌ | — | Retention policy for cleanup |
| `suspend` | bool | ❌ | `false` | Pause scheduling |

## schedule (Cron Format)

Standard 5-field cron format: `minute hour day month weekday`

| Field | Values | Special |
|-------|--------|---------|
| minute | 0-59 | `*` = every |
| hour | 0-23 | `*/N` = every N |
| day | 1-31 | |
| month | 1-12 | |
| weekday | 0-6 (Sun=0) | |

**Examples:**

| Schedule | Description |
|----------|-------------|
| `0 2 * * *` | Daily at 2:00 AM |
| `0 */6 * * *` | Every 6 hours |
| `0 0 * * 0` | Weekly on Sunday at midnight |
| `0 3 1 * *` | Monthly on 1st at 3:00 AM |
| `30 4 * * 1-5` | Weekdays at 4:30 AM |

## retention

Defines how long to keep backups. Files not matching any retention rule are deleted.

| Field | Type | Description |
|-------|------|-------------|
| `keepLast` | int | Keep the N most recent backups |
| `keepDaily` | int | Keep one backup per day for N days |
| `keepWeekly` | int | Keep one backup per week for N weeks |
| `keepMonthly` | int | Keep one backup per month for N months |

### How Retention Works

1. **List** all backup files in the database's S3 path
2. **Parse** timestamps from filenames (`YYYYMMDD-HHMMSS*.sql.gz`)
3. **Calculate** which files match retention rules:
   - `keepLast`: newest N files
   - `keepDaily`: newest file per day within N days
   - `keepWeekly`: newest file per week within N weeks
   - `keepMonthly`: newest file per month within N months
4. **Delete** files that don't match any rule

**Overlap example:**

```yaml
retention:
  keepLast: 3      # Always keep 3 newest
  keepDaily: 7     # Plus daily for 7 days
```

If you have 10 backups from the last 7 days:
- `keepLast` protects: 3 newest
- `keepDaily` protects: 1 per day = up to 7 files
- Result: keeps ~7-10 files (union of both rules)

### Retention applies to ALL files

Retention operates on all `.sql.gz` files in the database's S3 path, regardless of whether they were created by this schedule or manually. This keeps storage management simple and predictable.

## filenameTemplate

Same template variables as [Backup](backup.md#filenametemplate):

| Variable | Description | Example |
|----------|-------------|---------|
| `.DatabaseName` | PostgreSQL database name | `orders_db` |
| `.Timestamp` | `YYYYMMDD-HHMMSS` format | `20260120-020000` |
| `.RunID` | Unique 8-char identifier | `a1b2c3d4` |

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | Current state (`Active`, `Suspended`, `Failed`) |
| `message` | string | Detailed message |
| `lastBackupTime` | time | When last backup was triggered |
| `lastSuccessfulBackup` | string | Name of last successful Backup resource |
| `nextScheduledTime` | time | When next backup will run |
| `managedBackups` | int | Number of backups in S3 |
| `observedGeneration` | int64 | Processed spec version |

### Status Phases

| Phase | Description |
|-------|-------------|
| `Active` | Schedule is running normally |
| `Suspended` | Schedule is paused (`spec.suspend: true`) |
| `Failed` | Error (see `message`) |

## Behavior

### Backup Creation

When scheduled time arrives:
1. Controller creates a `Backup` resource: `{schedule-name}-{runID}`
2. Backup is labeled with `dbtether.io/schedule: {schedule-name}`
3. Backup follows normal backup flow (creates Job, uploads to S3)
4. Schedule status updated with last/next times

### Ownership

BackupSchedule owns its Backup resources. When you delete a schedule:
- All associated Backup CRDs are deleted
- S3 files are NOT automatically deleted (run retention cleanup first)

### Suspension

Set `suspend: true` to pause scheduling:
- No new backups are created
- Running backups continue to completion
- Retention cleanup is paused
- Status shows `Suspended`

## kubectl Commands

```bash
# List all schedules
kubectl get backupschedules -A
kubectl get bks -A  # short name

# Schedule details
kubectl describe bks orders-nightly -n orders-team

# Check next backup time
kubectl get bks orders-nightly -n orders-team \
  -o jsonpath='{.status.nextScheduledTime}'

# List backups created by this schedule
kubectl get backups -n orders-team -l dbtether.io/schedule=orders-nightly

# Suspend a schedule
kubectl patch bks orders-nightly -n orders-team \
  --type=merge -p '{"spec":{"suspend":true}}'

# Resume
kubectl patch bks orders-nightly -n orders-team \
  --type=merge -p '{"spec":{"suspend":false}}'
```

## Examples

### Production: Daily with Full Retention

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupSchedule
metadata:
  name: prod-daily
  namespace: production
spec:
  databaseRef:
    name: main-db
  storageRef:
    name: prod-backups
  schedule: "0 2 * * *"  # 2 AM daily
  retention:
    keepLast: 7
    keepDaily: 30
    keepWeekly: 12
    keepMonthly: 12
```

### Dev: Hourly with Minimal Retention

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupSchedule
metadata:
  name: dev-hourly
  namespace: development
spec:
  databaseRef:
    name: dev-db
  storageRef:
    name: dev-backups
  schedule: "0 * * * *"  # Every hour
  retention:
    keepLast: 24  # Keep last 24 hours only
```

### Analytics: Weekly

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupSchedule
metadata:
  name: analytics-weekly
  namespace: data-team
spec:
  databaseRef:
    name: analytics-db
  storageRef:
    name: company-s3
  schedule: "0 3 * * 0"  # Sunday 3 AM
  retention:
    keepLast: 8  # 2 months of weekly backups
```

## Troubleshooting

### Phase: Failed, message: "Invalid cron schedule"

Check cron format is exactly 5 fields:
```
# Wrong (6 fields - includes seconds)
"0 0 2 * * *"

# Correct (5 fields)
"0 2 * * *"
```

### Backups not being created

1. Check schedule is not suspended:
   ```bash
   kubectl get bks my-schedule -o jsonpath='{.spec.suspend}'
   ```

2. Check operator logs:
   ```bash
   kubectl logs -n dbtether deployment/dbtether -f | grep BackupSchedule
   ```

3. Verify Database and BackupStorage exist and are Ready

### Retention not deleting files

1. Ensure timestamps in filenames match format `YYYYMMDD-HHMMSS`
2. Check operator has `s3:DeleteObject` permission
3. Check operator logs for retention errors

### S3 files remain after schedule deletion

By design, S3 files are not deleted when schedule is removed. To clean up:
1. Keep schedule running until retention removes old files
2. Or manually delete files from S3

