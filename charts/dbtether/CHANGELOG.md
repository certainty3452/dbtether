# Changelog

All notable changes to the dbtether Helm chart will be documented in this file.

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
