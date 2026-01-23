# DBTether Helm Chart

A Kubernetes operator for managing PostgreSQL databases in external clusters (AWS RDS, Aurora, Azure Database, GCP Cloud SQL, or any PostgreSQL-compatible database).

## Features

- **Database Lifecycle Management** — Create, update, and delete databases via Kubernetes CRDs
- **User Management** — Automatic credential generation with rotation support
- **Backup & Restore** — Scheduled backups with retention policies, point-in-time restore
- **Multi-Cloud Storage** — S3, GCS, and Azure Blob Storage support with cloud-native authentication
- **GitOps Ready** — Declarative configuration, idempotent operations

## Installation

```bash
helm upgrade -i dbtether oci://ghcr.io/certainty3452/charts/dbtether -n dbtether --create-namespace
```

## CRDs Overview

| CRD | Description |
|-----|-------------|
| `DBCluster` | Connection to external PostgreSQL cluster |
| `Database` | PostgreSQL database managed by the operator |
| `DatabaseUser` | PostgreSQL user with automatic password management |
| `BackupStorage` | S3/GCS/Azure storage configuration |
| `Backup` | One-time database backup |
| `BackupSchedule` | Scheduled backups with retention policy |
| `Restore` | Database restoration from backup |

## Quick Start

### 1. Create a DBCluster (connection to your PostgreSQL)

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DBCluster
metadata:
  name: my-aurora
spec:
  endpoint: my-cluster.cluster-xxx.eu-central-1.rds.amazonaws.com
  port: 5432
  credentialsSecretRef:
    name: aurora-master-credentials
    namespace: default
```

### 2. Create a Database

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: myapp-db
  namespace: default
spec:
  clusterRef:
    name: my-aurora
  databaseName: myapp
  deletionPolicy: Retain  # Keep database when CRD is deleted
```

### 3. Create a User

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: myapp-user
  namespace: default
spec:
  clusterRef:
    name: my-aurora
  databaseRef:
    name: myapp-db
  username: myapp
  passwordRotation:
    enabled: true
    interval: 720h  # 30 days
```

### 4. Setup Backups

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupStorage
metadata:
  name: s3-backups
spec:
  s3:
    bucket: my-backups
    region: eu-central-1
    # Uses IRSA by default — no credentials needed!
---
apiVersion: dbtether.io/v1alpha1
kind: BackupSchedule
metadata:
  name: daily-backup
  namespace: default
spec:
  databaseRef:
    name: myapp-db
  storageRef:
    name: s3-backups
  schedule: "0 2 * * *"  # Daily at 2 AM
  retention:
    keepLast: 7
    keepDaily: 7
    keepWeekly: 4
```

### 5. Restore from Backup

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Restore
metadata:
  name: restore-myapp
  namespace: default
spec:
  source:
    latestFrom:
      databaseRef:
        name: myapp-db  # Auto-find latest backup
  target:
    databaseRef:
      name: myapp-db-restored
  onConflict: drop  # drop existing database before restore
```

## Configuration

### Values

| Parameter | Description | Default |
|-----------|-------------|---------|
| `replicaCount` | Number of operator replicas | `1` |
| `image.repository` | Operator image | `ghcr.io/certainty3452/dbtether` |
| `image.tag` | Image tag | `0.3.1` |
| `resources.requests.cpu` | CPU request | `100m` |
| `resources.requests.memory` | Memory request | `128Mi` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `256Mi` |
| `leaderElection.enabled` | Enable leader election | `true` |
| `logging.level` | Log level (debug, info, warn, error) | `info` |
| `logging.format` | Log format (json, console) | `json` |
| `backup.maxConcurrentPerCluster` | Max concurrent backups per cluster | `3` |

### Cloud Authentication

#### AWS (IRSA)

```yaml
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789:role/dbtether-backup-role
```

#### GCP (Workload Identity)

```yaml
serviceAccount:
  annotations:
    iam.gke.io/gcp-service-account: dbtether@project.iam.gserviceaccount.com
```

#### Azure (Workload Identity)

```yaml
serviceAccount:
  annotations:
    azure.workload.identity/client-id: <client-id>
podAnnotations:
  azure.workload.identity/use: "true"
```

## Uninstallation

```bash
helm uninstall dbtether -n dbtether
kubectl delete namespace dbtether
```

> **Note:** CRDs are not deleted automatically. To remove them:
> ```bash
> kubectl delete crd dbclusters.dbtether.io databases.dbtether.io \
>   databaseusers.dbtether.io backupstorages.dbtether.io \
>   backups.dbtether.io backupschedules.dbtether.io restores.dbtether.io
> ```

## Links

- [GitHub Repository](https://github.com/certainty3452/dbtether)
- [Documentation](https://github.com/certainty3452/dbtether#readme)
- [Issues](https://github.com/certainty3452/dbtether/issues)

