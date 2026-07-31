# Changelog

All notable changes to the dbtether Helm chart will be documented in this file.

## [0.7.0] - 2026-07-31

### Breaking
- A restore target in another namespace is now rejected

### Added
- Restores bring back database user access before reporting completion

### Fixed
- Users lost access to a database after it was restored from a backup
- A restore could get stuck forever when user access could not be restored; it now completes with a warning
- Rare connection leak on first-time connections to a cluster

## [0.6.4] - 2026-07-26

### Fixed
- An empty retention policy no longer deletes every backup — it is now rejected outright
- Backups no longer fail permanently when a dependency is briefly unavailable
- Restores work with static S3 credentials and can no longer run twice in parallel
- `deletionPolicy: Delete` retries until the database/user is actually dropped, instead of silently leaving it behind; deletion can now stay pending while the cluster is unreachable or the role still owns objects elsewhere
- Password rotation never fired after initial setup; it does now
- Failed privilege grants are surfaced and retried instead of reported as Ready
- From-source RBAC manifests were missing required permissions (Helm installs unaffected)

## [0.6.3] - 2026-05-20

### Fixed
- DatabaseUser secret is rebuilt when `spec.secret.template` or `spec.secret.keys` changes. Previously the secret stayed in the old shape — mixed legacy + new keys, password unreadable by consumers — until manually deleted.

## [0.6.2] - 2026-05-19

### Breaking
- BackupStorage probe requires additional read permission on the bucket/container.
  After upgrade, existing BackupStorage CRs may flip to Failed if IAM lacks:
  - AWS S3:  `s3:ListBucket` (used by HeadBucket)
  - GCS:     `storage.buckets.get` (used by Bucket.Attrs)
  - Azure:   container-level Read (used by GetProperties — covered by `Storage Blob Data Reader` / `Contributor`, but not by all custom roles)
  Backups will be blocked until IAM is updated, or the BackupStorage spec is corrected.
- `credentialsSecretRef` is now rejected at admission for GCS and Azure (was silently ignored). Use Workload Identity / Managed Identity instead.
- A BackupStorage CR with no provider configured is now rejected at admission (was caught later in reconcile). Diagnostic surfaces immediately on `kubectl apply`.

### Added
- BackupStorage reconciler probes the bucket before flipping to Ready
- Restore CRD reference documentation
- `StorageClient.Reachable(ctx) error` on the internal storage interface

### Changed
- Backup job memory default lowered to match real streaming usage
- Release pipeline skips `:latest` on pre-release tags
- CI verifies chart README image tag and CHANGELOG entry
- Storage probe failures are evaluated continuously; transient cloud errors temporarily block *new* backup jobs (existing jobs continue normally). Failed status auto-recovers within 60s.

### Removed
- Deprecated populateBackupResults helper

## [0.6.1] - 2026-05-19

### Changed
- DatabaseUser ConnectionLimit now read-before-write
- DatabaseUser finalizers added via Patch instead of Update
- Backup/Restore Jobs inherit operator's DB_SSLMODE env (was silently locked to compile-time default)
- Azure Upload no longer buffers the full payload in memory

### Fixed
- Retention now applies to GCS and Azure backups (was S3-only)
- BackupSchedule finalizer Update error was swallowed
- Backup goroutine leak when upload aborts mid-stream
- countActiveJobsForCluster now uses server-side label filter

### Security
- DatabaseUser.additionalGrants.tables restricted to standard PostgreSQL identifier pattern
- DatabaseUser.connectionLimit=0 rejected at admission (was silently mapped to unlimited)

## [0.6.0] - 2026-05-19

### Added
- DatabaseUser supports idleInTransactionTimeout to abort hanging transactions per role

### Changed
- DatabaseUser transitions to Failed (was Ready) when ConnectionLimit cannot be applied, matching the new idle_in_transaction policy

### Security
- DatabaseUser.additionalGrants.privileges is now restricted to a fixed allowlist; arbitrary strings could previously be composed into GRANT statements by anyone with create rights on the CR
- Restore no longer passes the database password on the psql argv (was visible via /proc/cmdline)
- Restore no longer interpolates the database name into a SQL literal unescaped

## [0.5.8] - 2026-04-24

### Fixed
- Retention cleanup no longer deletes manually-created backups with non-standard naming

## [0.5.7] - 2026-04-10

### Added
- New `dsn` secret template — generates a single `dsn` key with full PostgreSQL connection string

### Fixed
- Password generation now uses only URL-safe special characters (`._-`)

## [0.5.6] - 2026-02-06

### Fixed
- Backup CRD retention now uses full policy (keepDaily/Weekly/Monthly) instead of only keepLast
- DatabaseUser deletion now gracefully handles cleanup when database is already deleted
- Optimistic locking conflicts during cascading deletion (all controllers now use Patch instead of Update for finalizer removal)

## [0.5.5] - 2026-02-03

### Fixed
- Race condition causing "backup job not found" errors immediately after job creation
- Added retry logic for recently started backups to allow Kubernetes cache propagation

### Changed
- Improved secret key naming consistency: default template now uses `username` instead of `user`

## [0.5.4] - 2026-02-02

### Changed
- Streaming backup: constant memory usage regardless of database size

### Fixed
- Track pod name during Running phase for debugging failed jobs

### Added
- Configurable resources for backup pods via `backup.resources`

## [0.5.3] - 2026-02-02

### Fixed
- Backup/Restore stuck in Running when Job completes
- Added Pod read permissions to ClusterRole

## [0.5.2] - 2026-01-30

### Fixed
- Backup stuck in Running phase when underlying Job fails

### Added
- Configurable pod annotations/labels for backup jobs via Helm values (`backup.podAnnotations`, `backup.podLabels`, `backup.jobLabels`)
- Kubernetes Events for backup lifecycle (`BackupStarted`, `BackupCompleted`, `BackupFailed`)
- Configurable job parameters via `jobConfig` in Backup and BackupSchedule CRDs (`backoffLimit`, `activeDeadlineSeconds`, `ttlSecondsAfterFailed`)
- Detailed failure reporting in Backup status (`failureReason`, `failureMessage`, `failedAttempts`)

## [0.5.1] - 2026-01-29

### Added
- `owner` privilege level for DatabaseUser

## [0.5.0] - 2026-01-28

### Added
- Multi-database support for DatabaseUser
- Per-database secret generation option

### Breaking Changes
- `spec.databaseRef` → `spec.database` or `spec.databases[]`

## [0.4.3] - 2025-01-26

### Fixed
- Old secret is now deleted when `spec.secret.name` changes

## [0.4.2] - 2026-01-26

### Fixed
- Race condition in DBCluster/BackupStorage/DatabaseUser controllers causing double reconciliations

## [0.4.1] - 2026-01-26

### Fixed
- Image tag in values.yaml now correctly matches chart appVersion
- Release pipeline now auto-updates image.tag and syncs CRDs

### Added
- CI check for chart version consistency (image.tag matches appVersion)
- CI check for CRD synchronization between config/crd/bases and charts/dbtether/crds

## [0.4.0] - 2026-01-26

### Added
- Custom secret name for DatabaseUser via `spec.secret.name`
- Secret key templates: `raw`, `DB`, `DATABASE`, `POSTGRES`, `custom`
- Custom secret keys support via `spec.secret.keys` for legacy app compatibility

### Changed
- **BREAKING**: Default secret key format changed from `DATABASE_*` to `raw` (host, port, database, user, password)

## [0.3.1] - Previous

### Added
- Restore CRD with latestFrom support
- Multi-cloud backup storage (S3, GCS, Azure)
- Retention policies for backups
