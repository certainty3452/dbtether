# Changelog

All notable changes to the dbtether Helm chart will be documented in this file.

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
