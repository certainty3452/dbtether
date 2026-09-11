# dbtether Documentation

## CRD Reference

| CRD | Scope | Description |
|-----|-------|-------------|
| [DBCluster](crds/dbcluster.md) | Cluster | External PostgreSQL cluster (Aurora, RDS, self-hosted) |
| [Database](crds/database.md) | Namespaced | Database within a DBCluster |
| [DatabaseUser](crds/databaseuser.md) | Namespaced | PostgreSQL user with specific privileges |
| [BackupStorage](crds/backupstorage.md) | Cluster | Storage destination for backups (S3, GCS, Azure) |
| [Backup](crds/backup.md) | Namespaced | One-time database backup operation |
| [BackupSchedule](crds/backupschedule.md) | Namespaced | Scheduled backups with retention policy |
| [Restore](crds/restore.md) | Namespaced | Restore a database from a backup |

## Quick Start

### 1. Install the operator

```bash
helm upgrade -i dbtether oci://ghcr.io/certainty3452/charts/dbtether -n dbtether --create-namespace
```

### 2. Create credentials

The role needs `CREATEDB` on the target cluster — on Aurora/RDS, the master user.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-cluster-credentials
  namespace: dbtether
type: Opaque
stringData:
  username: postgres_admin
  password: your-password
```

### 3. Register a cluster

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DBCluster
metadata:
  name: my-cluster
spec:
  endpoint: my-cluster.xxx.rds.amazonaws.com
  port: 5432
  credentialsSecretRef:
    name: my-cluster-credentials
    namespace: dbtether
```

### 4. Create a database

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: my-app-db
  namespace: default
spec:
  clusterRef:
    name: my-cluster
  databaseName: my_app
  extensions:
    - uuid-ossp
  deletionPolicy: Retain
```

### 5. Check status

```bash
kubectl get dbcluster
kubectl get database -A
```

## Architecture

The operator never hosts a database. It holds a connection pool per DBCluster and drives an existing PostgreSQL server over SQL; backups and restores run as Jobs in the operator's namespace, streaming between that server and object storage.

```
┌──────────────────────────── Kubernetes cluster ────────────────────────────┐
│                                                                            │
│  DBCluster (cluster-scoped)     BackupStorage (cluster-scoped)             │
│  Database, DatabaseUser         Backup, BackupSchedule, Restore            │
│  (namespaced, per team)         (namespaced, per team)                     │
│                 │                             │                            │
│                 └────────────┬────────────────┘                            │
│                              ▼                                             │
│                      ┌───────────────┐                                     │
│                      │ Operator pod  │  one controller per CRD             │
│                      └───────┬───────┘                                     │
│                              │ creates                                     │
│                    ┌─────────▼──────────┐                                  │
│                    │ Backup / Restore   │  pg_dump | gzip → storage        │
│                    │ Jobs               │  storage → gunzip | psql         │
│                    └─────────┬──────────┘                                  │
└──────────────┬───────────────┴──────────────┬─────────────────────────────┘
               │ TCP/5432 (TLS)               │ HTTPS
               ▼                              ▼
┌──────────────────────────────┐   ┌────────────────────────────────────────┐
│ External PostgreSQL          │   │ Cloud storage (S3, GCS, Azure Blob)    │
│ (Aurora, RDS, self-hosted)   │   │ production/my_app/20260120-143022.sql.gz│
└──────────────────────────────┘   └────────────────────────────────────────┘
```
