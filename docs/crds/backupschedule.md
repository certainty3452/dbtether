# BackupSchedule

Represents a scheduled backup policy for automatic database backups.

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `BackupSchedule`  
**Short name:** `bks`  
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
| `schedule` | string | ✅ | — | Cron schedule; admission enforces exactly 5 whitespace-separated fields |
| `filenameTemplate` | string | ❌ | `{{ .Timestamp }}.sql.gz` | Backup filename template, inherited by every Backup it creates |
| `retention` | object | ❌ | — | Retention policy for cleanup |
| `suspend` | bool | ❌ | `false` | Pause scheduling |
| `jobConfig` | object | ❌ | — | Job configuration inherited by all Backups |

### jobConfig

Copied verbatim onto every Backup this schedule creates — see [Backup jobConfig](backup.md#jobconfig).

```yaml
spec:
  schedule: "0 2 * * *"
  jobConfig:
    backoffLimit: 5              # All scheduled backups get 5 retries
    activeDeadlineSeconds: 14400 # 4 hour timeout for large DBs
```

## schedule (Cron Format)

Standard 5-field cron: `minute hour day month weekday`, evaluated in the operator's timezone.

| Schedule | Description |
|----------|-------------|
| `0 2 * * *` | Daily at 2:00 AM |
| `0 */6 * * *` | Every 6 hours |
| `0 0 * * 0` | Weekly on Sunday at midnight |
| `0 3 1 * *` | Monthly on 1st at 3:00 AM |
| `30 4 * * 1-5` | Weekdays at 4:30 AM |

The next run is computed from `status.lastBackupTime`, falling back to the resource's creation time. A schedule created after today's slot has passed therefore fires its first backup immediately.

## retention

At least one of the four fields must be a positive number — admission rejects an empty or all-zero policy, because that would mark every backup for deletion.

| Field | Type | Description |
|-------|------|-------------|
| `keepLast` | int | Keep the N most recent backups |
| `keepDaily` | int | Keep the newest backup of each day, for days within the last N |
| `keepWeekly` | int | Keep the newest backup of each ISO week, for weeks within the last N |
| `keepMonthly` | int | Keep the newest backup of each month, for months within the last N |

The rules union: a file survives if any rule keeps it. With `keepLast: 3` plus `keepDaily: 7` over 10 backups from the last week, the 3 newest and one per day both survive — roughly 7-10 files.

### What retention deletes

Cleanup runs after each scheduled backup and covers two things:

- **Objects** under the storage prefix whose name matches this schedule's `filenameTemplate`, with the template variables turned back into patterns (`{{ .Timestamp }}` → `\d{8}-\d{6}`, `{{ .RunID }}` → `[a-z0-9]{8}`). Files written by another schedule or by hand under the same prefix are left alone. Change `filenameTemplate` on a live schedule and the older files stop matching — they are then never cleaned up.
- **Backup resources** labelled with this schedule, once they are `Completed` or `Failed`, using the same policy. Runs still in flight are never deleted.

Ordering uses the `YYYYMMDD-HHMMSS` timestamp in the object key; an object without one falls back to its storage last-modified time, and is skipped if that is unavailable too.

Retention rebuilds the storage prefix from `.ClusterName` and `.DatabaseName` only, so a [`pathTemplate`](backupstorage.md#pathtemplate) containing `.Year`, `.Month` or `.Day` yields a prefix that matches no object and silently deletes nothing.

## filenameTemplate

Same variables as [Backup](backup.md#filenametemplate): `.DatabaseName`, `.Timestamp`, `.RunID`.

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | `Active`, `Suspended`, `Failed` |
| `message` | string | Error text; empty while `Active` |
| `lastBackupTime` | time | When the last backup was triggered |
| `nextScheduledTime` | time | When the next backup will run |
| `observedGeneration` | int64 | Processed spec version |

## Behavior

### Backup Creation

At each slot the controller creates a Backup named `<schedule-name>-<YYYYMMDD-HHMM>` from the slot's UTC time — deterministic, so a duplicate reconcile finds the existing object instead of running twice. It carries the labels `dbtether.io/schedule` and `dbtether.io/schedule-namespace`, and inherits `databaseRef`, `storageRef`, `filenameTemplate` and `jobConfig`. From there it follows the normal [Backup](backup.md) flow.

### Ownership

Each Backup is created with the schedule as its controller reference, so deleting the schedule deletes its Backup resources. Objects already in storage are never deleted with the schedule.

### Suspension

`suspend: true` stops the scheduler and retention cleanup; already-running backups finish. Status shows `Suspended`.

## kubectl Commands

```bash
kubectl get bks -A
kubectl describe bks orders-nightly -n orders-team
kubectl get bks orders-nightly -n orders-team -o jsonpath='{.status.nextScheduledTime}'

# Backups created by this schedule
kubectl get backups -n orders-team -l dbtether.io/schedule=orders-nightly

kubectl patch bks orders-nightly -n orders-team --type=merge -p '{"spec":{"suspend":true}}'
kubectl patch bks orders-nightly -n orders-team --type=merge -p '{"spec":{"suspend":false}}'
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

### Large Database: Custom Timeouts

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupSchedule
metadata:
  name: large-db-nightly
  namespace: data-team
spec:
  databaseRef:
    name: large-analytics-db
  storageRef:
    name: company-s3
  schedule: "0 1 * * *"  # 1 AM daily
  retention:
    keepLast: 7
  jobConfig:
    backoffLimit: 5              # More retries for large DBs
    activeDeadlineSeconds: 14400 # 4 hour timeout
    ttlSecondsAfterFailed: 86400 # Keep failed Jobs for debugging
```

## Troubleshooting

### Rejected on apply: `spec.schedule in body should match '^(\S+\s+){4}\S+$'`

The expression does not have exactly 5 fields. Six-field cron (with seconds) is the usual cause:

```
# Wrong (6 fields - includes seconds)
"0 0 2 * * *"

# Correct (5 fields)
"0 2 * * *"
```

### Phase: Failed, message: "Invalid cron schedule: ..."

Five fields, but one of them is not a valid cron value — `0 25 * * *`, `0 2 * * 8`, a typo in a range. The quoted parser error names the offending field.

### Backups not being created

Check `spec.suspend`, then that the Database and BackupStorage are both ready — a schedule creates the Backup regardless, and the Backup itself parks in `Pending` naming the dependency:

```bash
kubectl get bks my-schedule -o jsonpath='{.spec.suspend}'
kubectl get bkp -n <namespace> -l dbtether.io/schedule=my-schedule
```

### Retention not deleting files

1. The filenames must match this schedule's `filenameTemplate` and contain a `YYYYMMDD-HHMMSS` timestamp. Files written under an earlier template are ignored.
2. The operator needs `s3:DeleteObject` (or the equivalent) on the bucket.
3. Retention failures never fail a backup; they are logged as `retention cleanup: ...` and nothing else.

### Objects remain after the schedule is deleted

By design — deleting a schedule removes its Backup resources, never the files. Let retention drain them before deleting the schedule, or clean the prefix by hand.
