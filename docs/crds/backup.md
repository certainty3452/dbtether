# Backup

A one-time backup of a [Database](database.md) to a [BackupStorage](backupstorage.md): `pg_dump` piped through gzip into object storage by a Job in the operator namespace. Namespaced; short name `bkp`.

## Examples

Minimal:

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Backup
metadata:
  name: orders-backup-20260120
  namespace: my-team
spec:
  databaseRef:
    name: orders-db
  storageRef:
    name: production-backups
```

Full:

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Backup
metadata:
  name: orders-pre-deploy
  namespace: my-team
spec:
  databaseRef:
    name: shared-db
    namespace: shared-resources   # shared-db lists my-team in allowedNamespaces
  storageRef:
    name: production-backups
  filenameTemplate: "pre-deploy-{{ .Timestamp }}-{{ .RunID }}.sql.gz"
  trigger: "v4.6.1"               # change to run again from the same manifest
  ttlAfterCompletion: 24h
  jobConfig:
    backoffLimit: 5
    activeDeadlineSeconds: 7200
    ttlSecondsAfterFailed: 86400
```

## Spec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `databaseRef.name` | string | required | Database to back up |
| `databaseRef.namespace` | string | Backup's namespace | Another namespace must list the Backup's namespace in the Database's [`allowedNamespaces`](database.md), otherwise the Backup stays `Pending` |
| `storageRef.name` | string | required | BackupStorage |
| `filenameTemplate` | string | `{{ .Timestamp }}.sql.gz` | Last path segment. Variables: `.ClusterName`, `.DatabaseName`, `.Timestamp` (UTC `YYYYMMDD-HHMMSS`), `.Year`, `.Month`, `.Day`, `.RunID` (8 chars) |
| `trigger` | string | unset | Max 253 chars. Changing it starts a new run |
| `ttlAfterCompletion` | duration | `1h` | How long Kubernetes keeps the finished Job and its pods |
| `deletionPolicy` | enum | `Retain` | `Delete` removes the backup file from storage when the Backup is deleted, see [Notes](#notes) |
| `jobConfig.backoffLimit` | int | `3` | Retries, 0-10 |
| `jobConfig.activeDeadlineSeconds` | int | unset | Hard timeout, min 60 |
| `jobConfig.ttlSecondsAfterFailed` | int | `43200` | How long a failed Job is kept |

Directories come from the storage's [`pathTemplate`](backupstorage.md#spec). Example: `{{ .Timestamp }}-{{ .RunID }}.sql.gz` gives `20260120-143022-a1b2c3d4.sql.gz`.

## Status

| Phase | Meaning |
|-------|---------|
| `Pending` | Waiting for a dependency, a free job slot, or Job creation |
| `Running` | The Job is executing |
| `Completed` | Uploaded; `status.path`, `size`, `duration` are set |
| `Failed` | See `message`, `failureReason`, `failureMessage` |

Other status fields: `jobName`, `runId`, `startedAt`, `completedAt`, `failedAttempts`, `lastPodName` (for logs).

## Notes

- **Idempotency.** The spec is hashed; re-applying an unchanged Backup does nothing, however often GitOps syncs it. Any spec change other than `deletionPolicy` starts a new run. If a run is active, it finishes first; runs never overlap.
- **`trigger`** only feeds that hash. Use it to re-run a Backup kept in Git, e.g. before each deployment. Use a fixed `filenameTemplate` if every run should overwrite one object.
- **Health checks.** `status.observedGeneration` is written when a run starts. Check `observedGeneration == metadata.generation` and `phase == Completed`; the first alone is true while the run is still `Running`.
- **Credentials.** The DBCluster needs `credentialsSecretRef` in the operator namespace, see [BackupStorage](backupstorage.md#notes).
- **Throttling.** At most `backup.maxConcurrentPerCluster` (default 3) backup and restore Jobs per DBCluster; the rest wait in `Pending`, retried every 30 seconds.
- **Job.** Named `backup-<name>-<runId>` in the operator namespace, labelled `dbtether.io/backup=<name>`. Each attempt is a separate pod, kept until the TTL so logs survive.
- **Format.** `pg_dump --format=plain --no-owner --no-acl`, gzipped. A restore re-applies ownership and grants itself. The image bundles clients for PostgreSQL 16, 17 and 18: `pg_dump` is the smallest bundled major at or above the server's, `psql` (restores) the matching or nearest lower one. A server newer than 18 fails with `no pg_dump for a server on major <N>`.
- **S3 tags.** Uploads carry the tags `database`, `cluster`, `backup-name`, `namespace`, `timestamp`, `created-by`, so the identity needs `s3:PutObjectTagging`, see [BackupStorage](backupstorage.md#required-permissions).
- **`deletionPolicy: Delete`.** Deleting the Backup deletes the file in `status.path`. The operator deletes with the storage's credentials, the same as Jobs, and those credentials need the delete permission from [BackupStorage](backupstorage.md#required-permissions); a Backup deleted while its Job runs can leave that run's file behind. A storage error holds deletion and retries (`FileDeleteFailed`), a missing BackupStorage leaves the file with a `FileOrphaned` event. A re-run does not delete the previous run's file. A Restore already reading the file fails if the Backup is deleted. Backups sharing one file key (same storage, same database, same fixed `filenameTemplate`) share the file, so deleting one removes it for the other; put `.RunID` in the template to keep keys unique (`.Timestamp` has one-second resolution).
- **Schedules.** Backups created by a [BackupSchedule](backupschedule.md) keep `Retain`; the schedule's retention manages their files.
- **Events:** `BackupStarted`, `BackupCompleted`, `BackupFailed`, `ResultNotRecorded`, `FileDeleteFailed`, `FileOrphaned`.

## Troubleshooting

| Message | Cause and fix |
|---------|---------------|
| Pending: `database <name> is not ready` (also `not found`, `cluster ... not connected`, `backup storage ... not ready`) | Fix the named resource; the Backup starts on its own |
| Pending: `waiting for other backup/restore jobs on cluster <name> (active: 3/3)` | At the concurrency limit; raise `backup.maxConcurrentPerCluster` if it never drains |
| Pending: `failed to create job: ...` | API server rejected the Job: quota, admission policy, or bad `backup.podLabels` / `podAnnotations` |
| Pending: `... must be in the operator namespace ...` or `DBCluster <name> has no credentialsSecretRef` | Set `credentialsSecretRef` on the DBCluster with the Secret in the operator namespace, see [BackupStorage](backupstorage.md#notes) |
| Failed: `backup job failed: ...` | Read `status.failureMessage`, then the pod logs (below). `failed to upload to S3` is IAM, see [BackupStorage](backupstorage.md#required-permissions) |
| Failed: `backup job not found` | The Job was deleted before it recorded a result. Check the bucket, then change `trigger` to re-run |
| Failed: `result not recorded: ...` | Create a new Backup or change `spec.trigger` |
| Backup stuck deleting, event `FileDeleteFailed` | `deletionPolicy: Delete` cannot delete the file: fix the storage permissions or credentials, it retries on its own. Set `deletionPolicy: Retain` to let deletion finish |

```bash
kubectl logs -n dbtether "$(kubectl get bkp my-backup -n my-team -o jsonpath='{.status.lastPodName}')"
kubectl get bkp my-backup -n my-team -o jsonpath='{.status.path}'
```
