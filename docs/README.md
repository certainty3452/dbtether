# PostgreSQL Database Operator Documentation

## CRD Reference

| CRD | Scope | Description |
|-----|-------|-------------|
| [DBCluster](crds/dbcluster.md) | Cluster | External PostgreSQL cluster (Aurora, RDS, self-hosted) |
| [Database](crds/database.md) | Namespaced | Database within a DBCluster |
| [DatabaseUser](crds/databaseuser.md) | Namespaced | PostgreSQL user with specific privileges |
| [BackupStorage](crds/backupstorage.md) | Cluster | Storage destination for backups (S3, GCS, Azure) |
| [Backup](crds/backup.md) | Namespaced | One-time database backup operation |
| [BackupSchedule](crds/backupschedule.md) | Namespaced | Scheduled backups with retention policy |

## Quick Start

### 1. Install the operator

```bash
helm install postgres-db-operator ./charts/postgres-db-operator \
  -n postgres-db-operator \
  --create-namespace
```

### 2. Create credentials

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-cluster-credentials
  namespace: postgres-db-operator
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
    namespace: postgres-db-operator
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

```
┌──────────────────────────────────────────────────────────────────────────────┐
│                              Kubernetes Cluster                               │
│                                                                               │
│  ┌────────────────────┐  ┌─────────────────────┐  ┌───────────────────────┐  │
│  │ DBCluster (cluster)│  │ BackupStorage       │  │ Backup (namespaced)   │  │
│  │                    │  │ (cluster)           │  │                       │  │
│  │ name: prod         │  │                     │  │ databaseRef: my-app   │  │
│  │ endpoint: ...      │  │ s3:                 │  │ storageRef: s3-backup │  │
│  │ credentials: ...   │  │   bucket: backups   │  └───────────┬───────────┘  │
│  └────────┬───────────┘  │   region: eu-ctr-1  │              │              │
│           │              └─────────┬───────────┘              │              │
│           │                        │                          │              │
│  ┌────────▼────────────────────────▼──────────────────────────▼───────────┐  │
│  │                         Operator Pod                                    │  │
│  │  ┌──────────────┐ ┌──────────────┐ ┌────────────┐ ┌─────────────────┐  │  │
│  │  │ DBCluster    │ │ Database     │ │ Backup     │ │ BackupStorage   │  │  │
│  │  │ Controller   │ │ Controller   │ │ Controller │ │ Controller      │  │  │
│  │  └──────────────┘ └──────────────┘ └─────┬──────┘ └─────────────────┘  │  │
│  └───────────┬───────────────────────────────┼────────────────────────────┘  │
│              │                               │                                │
│              │                      ┌────────▼────────┐                      │
│              │                      │   Backup Job    │                      │
│              │                      │   (pg_dump →    │                      │
│              │                      │    gzip → S3)   │                      │
│              │                      └────────┬────────┘                      │
└──────────────┼───────────────────────────────┼───────────────────────────────┘
               │                               │
               │ TCP/5432 (TLS)                │ HTTPS (S3 API)
               │                               │
┌──────────────▼─────────────────┐   ┌─────────▼─────────────────────────────┐
│   External PostgreSQL          │   │           Cloud Storage               │
│   (Aurora, RDS, self-hosted)   │   │        (S3, GCS, Azure Blob)          │
│                                │   │                                        │
│   ┌──────────┐  ┌──────────┐   │   │  ┌────────────────────────────────┐   │
│   │  my_app  │  │ postgres │   │   │  │ prod/my_app/20260120-143022.gz │   │
│   │          │  │ (system) │   │   │  └────────────────────────────────┘   │
│   └──────────┘  └──────────┘   │   │                                        │
└────────────────────────────────┘   └────────────────────────────────────────┘
```

## Deletion Policies

| Policy | Behavior | Use Case |
|--------|----------|----------|
| `Retain` | Database stays in PostgreSQL | Production, important data |
| `Delete` | `DROP DATABASE` is executed | Dev/test, feature branches |

## Roadmap

See [ROADMAP.md](../ROADMAP.md) for full roadmap.

**Next up:**
- [ ] Restore — restore from backup with conflict handling
- [ ] Multi-database user access
