# PostgreSQL Database Operator Documentation

## CRD Reference

| CRD | Scope | Description |
|-----|-------|-------------|
| [DBCluster](crds/dbcluster.md) | Cluster | External PostgreSQL cluster (Aurora, RDS, self-hosted) |
| [Database](crds/database.md) | Namespaced | Database within a DBCluster |
| [DatabaseUser](crds/databaseuser.md) | Namespaced | PostgreSQL user with specific privileges |

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
┌─────────────────────────────────────────────────────────────────┐
│                      Kubernetes Cluster                         │
│                                                                  │
│  ┌──────────────────┐     ┌──────────────────────────────────┐  │
│  │   DBCluster      │     │        Database                  │  │
│  │   (cluster-wide) │     │        (namespaced)              │  │
│  │                  │     │                                   │  │
│  │  name: prod      │◄────│  clusterRef: prod                │  │
│  │  endpoint: ...   │     │  databaseName: my_app            │  │
│  │  credentials: ...│     │  extensions: [uuid-ossp]         │  │
│  └────────┬─────────┘     │  deletionPolicy: Retain          │  │
│           │               └──────────────────────────────────┘  │
│           │                                                      │
│  ┌────────▼─────────┐                                           │
│  │  Operator Pod    │                                           │
│  │                  │                                           │
│  │  - DBCluster     │                                           │
│  │    Controller    │                                           │
│  │  - Database      │                                           │
│  │    Controller    │                                           │
│  │  - Connection    │                                           │
│  │    Pool          │                                           │
│  └────────┬─────────┘                                           │
│           │                                                      │
└───────────┼──────────────────────────────────────────────────────┘
            │
            │ TCP/5432 (TLS)
            │
┌───────────▼──────────────────────────────────────────────────────┐
│                    External PostgreSQL                            │
│                    (Aurora, RDS, self-hosted)                     │
│                                                                   │
│   ┌─────────────┐  ┌─────────────┐  ┌─────────────┐              │
│   │  my_app     │  │  other_db   │  │  postgres   │              │
│   │  (created)  │  │             │  │  (system)   │              │
│   └─────────────┘  └─────────────┘  └─────────────┘              │
│                                                                   │
└───────────────────────────────────────────────────────────────────┘
```

## Deletion Policies

| Policy | Behavior | Use Case |
|--------|----------|----------|
| `Retain` | Database stays in PostgreSQL | Production, important data |
| `Delete` | `DROP DATABASE` is executed | Dev/test, feature branches |

## Roadmap

- [ ] **DatabaseUser** — create users with different privileges
- [ ] **Access Control** — team-based access via Validating Webhook
- [ ] **Backup/Restore** — pg_dump to S3, scheduled backups
- [ ] **ESO Integration** — push credentials to AWS Secrets Manager
