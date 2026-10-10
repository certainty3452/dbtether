# dbtether documentation

Install and a first example: [README](../README.md). Chart values: [chart README](../charts/dbtether/README.md).

## Resources

| Resource | Scope | Purpose |
|----------|-------|---------|
| [DBCluster](crds/dbcluster.md) | Cluster | Connection to an external PostgreSQL server |
| [Database](crds/database.md) | Namespaced | A database inside a DBCluster |
| [DatabaseUser](crds/databaseuser.md) | Namespaced | A role with privileges and a generated Secret |
| [BackupStorage](crds/backupstorage.md) | Cluster | S3, GCS or Azure Blob destination |
| [Backup](crds/backup.md) | Namespaced | One-time backup |
| [BackupSchedule](crds/backupschedule.md) | Namespaced | Cron backups with retention |
| [Restore](crds/restore.md) | Namespaced | Restore a database from a backup |

## How it fits together

The operator holds a connection pool per DBCluster and manages databases and roles over SQL. Backups and restores run as Jobs in the operator namespace, streaming between PostgreSQL and object storage.

```
Database, DatabaseUser ──► DBCluster ──► PostgreSQL (Aurora, RDS, self-hosted)
Backup, BackupSchedule ──► Database + BackupStorage ──► Job: pg_dump | gzip ──► bucket
Restore                ──► Backup or path ──► Job: bucket | gunzip | psql ──► Database
```
