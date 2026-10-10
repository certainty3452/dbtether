# BackupSchedule

Creates [Backups](backup.md) of a [Database](database.md) on a cron schedule and prunes old ones by a retention policy. Namespaced; short name `bks`.

## Examples

Minimal:

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
  schedule: "0 2 * * *"
```

Full:

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
  schedule: "0 2 * * *"
  filenameTemplate: "{{ .Timestamp }}.sql.gz"
  retention:
    keepLast: 7
    keepDaily: 30
    keepWeekly: 12
    keepMonthly: 12
  suspend: false
  jobConfig:
    backoffLimit: 5
    activeDeadlineSeconds: 14400
```

## Spec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `databaseRef.name` | string | required | Database to back up |
| `databaseRef.namespace` | string | schedule's namespace | Another namespace must list the schedule's in the Database's [`allowedNamespaces`](database.md), otherwise the schedule stays `Pending` |
| `storageRef.name` | string | required | BackupStorage |
| `schedule` | string | required | Standard 5-field cron, evaluated in UTC |
| `filenameTemplate` | string | `{{ .Timestamp }}.sql.gz` | Inherited by every Backup; variables as in [Backup](backup.md#spec) |
| `retention.keepLast` | int | | Keep the N newest backups |
| `retention.keepDaily` | int | | Newest backup of each day, for N days |
| `retention.keepWeekly` | int | | Newest backup of each ISO week, for N weeks |
| `retention.keepMonthly` | int | | Newest backup of each month, for N months |
| `suspend` | bool | `false` | Stop scheduling and retention; running backups finish |
| `jobConfig` | object | | Copied onto every Backup, see [Backup](backup.md#spec) |

Examples for `schedule`: `0 */6 * * *` every 6 hours, `0 0 * * 0` Sundays at midnight, `30 4 * * 1-5` weekdays at 4:30. Admission requires exactly five fields.

`retention` needs at least one positive field; admission rejects an empty or all-zero policy because it would delete every backup.

## Status

| Phase | Meaning |
|-------|---------|
| `Active` | Scheduling |
| `Pending` | Waiting, e.g. for a cross-namespace Database reference to be allowed |
| `Suspended` | `suspend: true` |
| `Failed` | See `message`, e.g. an invalid cron value |

Other fields: `lastBackupTime`, `nextScheduledTime`, `lastSuccessfulBackup`, `managedBackups` (count of `Completed` Backups).

## Notes

- **Backups.** At each slot a Backup named `<schedule>-<YYYYMMDD-HHMM>` (UTC) is created, labelled `dbtether.io/schedule=<schedule>`, with the schedule as controller owner. Deleting the schedule deletes its Backup resources, never the files in storage.
- **First run.** The first cron slot after creation. A slot missed while the operator was down fires once on recovery.
- **Retention rules union.** A file survives if any rule keeps it. The day/week/month windows end at the newest backup, not at now, so a stalled schedule never ages out what it has; the newest backup is always kept.
- **What retention deletes**, at most once a minute while the schedule is `Active`:
  - Objects directly inside the database's directory whose names match this schedule's `filenameTemplate`. Hand-made files and other names are left alone. Two schedules on one database with the same template prune each other's files. After changing the template, older files no longer match and are never cleaned.
  - Completed or Failed Backup resources of this schedule. Running ones are never deleted.
- **Dates in `pathTemplate` disable retention.** It rebuilds the prefix from `.ClusterName` and `.DatabaseName` only, so a template with `.Year`/`.Month`/`.Day` matches nothing and deletes nothing, silently.
- **Credentials.** Retention uses the BackupStorage credentials. Secret placement: [BackupStorage](backupstorage.md#notes).

## Troubleshooting

| Symptom | Cause and fix |
|---------|---------------|
| Rejected: `spec.schedule ... should match '^(\S+\s+){4}\S+$'` | Not five fields; six-field cron with seconds is the usual cause. Use `0 2 * * *`, not `0 0 2 * * *` |
| `Failed`: `Invalid cron schedule: ...` | Five fields, one invalid (`0 25 * * *`). The message names it |
| No Backups appear | Check `spec.suspend` and the schedule's `message`; then the Backup `Pending` message names the missing Database or BackupStorage: `kubectl get bkp -l dbtether.io/schedule=<name>` |
| Retention deletes nothing | Filenames must match `filenameTemplate`; the identity needs delete permission; `pathTemplate` must not use dates. Failures are only logged as `retention cleanup: ...` |
| Files remain after deleting the schedule | By design. Let retention drain them first, or clean the prefix by hand |
