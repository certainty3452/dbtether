# dbtether

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/certainty3452/dbtether)](https://go.dev/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.31+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io/)

> Kubernetes operator for external PostgreSQL databases - manage AWS Aurora, RDS, and self-hosted databases and users declaratively via CRDs.

Tether your databases to Kubernetes. Create databases and users in existing PostgreSQL clusters through GitOps workflows. Perfect for platform teams building self-service developer experiences.

## Why This Operator?

When building a platform on Kubernetes, I faced a common dilemma with database provisioning:

- **CloudNativePG** creates databases inside the Kubernetes cluster, requiring PV/PVC management and adding operational complexity
- **Crossplane** provisions separate database instances per resource, which becomes expensive when you just need multiple databases in a shared cluster

Both are great tools designed for **isolated environments** — separate clusters or instances per team. But I was building a platform where **separation of concerns** mattered more than isolation: infrastructure team provisions shared Aurora clusters via Terraform, developers manage their own databases and users via GitOps.

I needed **manageability**, not isolation. A simple way for developers to self-serve databases without tickets or manual SQL, while infrastructure controls the underlying clusters.

This operator fills that gap. It connects to existing PostgreSQL-compatible clusters (AWS Aurora, RDS, or self-hosted) and manages databases and users declaratively through CRDs. Perfect for Helm charts that need a database, Backstage templates for self-service portals, or ArgoCD workflows where databases are provisioned via pull requests.

As a GitOps enthusiast, this operator fits perfectly into my workflow. I hope it helps others facing the same challenge.

## Use Cases

- **Manage RDS/Aurora from Kubernetes** - connect to existing AWS database clusters and create databases via CRDs
- **Self-service database provisioning** - developers request databases via pull requests, platform team approves, GitOps applies
- **Multi-tenant database management** - one Aurora cluster, multiple databases with isolated users per team/namespace
- **Database-as-Code with ArgoCD / Flux** - declarative database and user management synced from Git
- **Ephemeral environments** - spin up isolated databases for preview/feature branches via Helm charts, auto-cleanup on teardown

## Features

- **Declarative management** - manage databases and users via Kubernetes CRDs
- **GitOps-friendly** - works seamlessly with ArgoCD, Flux, and other GitOps tools
- **Auto-generated credentials** - secure passwords stored in Kubernetes Secrets
- **Password rotation** - automatic credential rotation with configurable schedule
- **Database isolation** - users are granted access only to their assigned database (cannot query other databases)
- **Configurable deletion policies** - choose between Retain (keep data) or Delete on resource removal
- **Database backups** - one-time and scheduled backups with `pg_dump` → gzip → cloud storage
- **Multi-cloud storage** - backup to AWS S3, Google Cloud Storage, or Azure Blob Storage
- **Retention policies** - automatic cleanup with `keepLast`, `keepDaily`, `keepWeekly`, `keepMonthly`
- **Cloud-native auth** - IRSA, Workload Identity, Managed Identity for secure storage access

## Installation

### Using Helm

```bash
helm install dbtether ./charts/dbtether \
  -n postgres-operator-system \
  --create-namespace
```

### Using kubectl

```bash
# Install CRDs
kubectl apply -f config/crd/bases/

# Install RBAC and operator
kubectl apply -f config/rbac/
kubectl apply -f config/manager/
```

## Usage

### 1. Create admin credentials secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aurora-admin-credentials
  namespace: postgres-operator-system
type: Opaque
stringData:
  username: postgres
  password: your-admin-password
```

### 2. Create a DBCluster

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DBCluster
metadata:
  name: my-aurora-cluster
spec:
  endpoint: my-cluster.cluster-xxx.eu-west-1.rds.amazonaws.com
  port: 5432
  credentialsSecretRef:
    name: aurora-admin-credentials
    namespace: postgres-operator-system
```

### 3. Create a Database

```yaml
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: my-app-db
  namespace: default
spec:
  clusterRef:
    name: my-aurora-cluster
  databaseName: my_app
  extensions:
    - uuid-ossp
    - pg_trgm
  deletionPolicy: Retain  # or Delete
```

### 4. Create a DatabaseUser

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: my-app-readonly
  namespace: default
spec:
  databaseRef:
    name: my-app-db
  privileges: readonly
```

### 5. Check status

```bash
# Check cluster connection
kubectl get dbclusters
NAME               ENDPOINT                                    PHASE      VERSION   AGE
my-aurora-cluster  my-cluster.xxx.rds.amazonaws.com            Connected  15.4      5m

# Check databases
kubectl get databases -A
NAMESPACE   NAME        CLUSTER            DATABASE   PHASE   AGE
default     my-app-db   my-aurora-cluster  my_app     Ready   2m

# Check users
kubectl get databaseusers -A
NAMESPACE   NAME             DATABASE    USERNAME         PRIVILEGES   PHASE   AGE
default     my-app-readonly  my-app-db   my-app-readonly  readonly     Ready   1m

# Get generated credentials
kubectl get secret my-app-readonly-credentials -o jsonpath='{.data.password}' | base64 -d
```

## CRD Reference

See full documentation in [docs/](docs/README.md):

| CRD | Scope | Description |
|-----|-------|-------------|
| [DBCluster](docs/crds/dbcluster.md) | Cluster | External PostgreSQL cluster connection |
| [Database](docs/crds/database.md) | Namespaced | Database within a DBCluster |
| [DatabaseUser](docs/crds/databaseuser.md) | Namespaced | PostgreSQL user with privileges |
| BackupStorage | Cluster | S3/GCS/Azure storage configuration |
| Backup | Namespaced | One-time database backup |
| BackupSchedule | Namespaced | Scheduled backups with retention policy |

### Quick Reference

**DBCluster:**
- `spec.endpoint` - PostgreSQL hostname (required)
- `spec.port` - Port, default 5432
- `spec.credentialsSecretRef` - Reference to Secret with username/password

**Database:**
- `spec.clusterRef.name` - Name of DBCluster (required)
- `spec.databaseName` - Database name in PostgreSQL (required)
- `spec.extensions` - List of PostgreSQL extensions
- `spec.deletionPolicy` - `Retain` (default) or `Delete`

**DatabaseUser:**
- `spec.databaseRef.name` - Name of Database (required)
- `spec.privileges` - `readonly`, `readwrite`, or `admin` (required)
- `spec.username` - PostgreSQL username (defaults to metadata.name)
- `spec.password.length` - Password length (default 16, range 12-64)

**BackupStorage:**
- `spec.s3.bucket` - S3 bucket name (required for S3)
- `spec.s3.region` - AWS region (required for S3)
- `spec.pathTemplate` - Path template (default: `{{ .ClusterName }}/{{ .DatabaseName }}`)
- `spec.credentialsSecretRef` - Optional, uses IRSA/Pod Identity if omitted

**Backup:**
- `spec.databaseRef.name` - Name of Database to backup (required)
- `spec.storageRef.name` - Name of BackupStorage (required)
- `spec.filenameTemplate` - Filename template (default: `{{ .Timestamp }}.sql.gz`)
- `spec.ttlAfterCompletion` - Job auto-cleanup duration (default: 1h)

**BackupSchedule:**
- `spec.databaseRef.name` - Name of Database to backup (required)
- `spec.storageRef.name` - Name of BackupStorage (required)
- `spec.schedule` - Cron schedule, e.g., `0 2 * * *` for 2 AM daily (required)
- `spec.retention.keepLast` - Keep N most recent backups
- `spec.retention.keepDaily` - Keep daily backups for N days
- `spec.suspend` - Pause scheduling

## Development

```bash
# Build
make build

# Run tests (unit)
make test

# Lint and security checks
make check

# Build multi-arch Docker image
make docker-buildx
```

### Testing with envtest

Controller tests use [envtest](https://book.kubebuilder.io/reference/envtest.html) which provides a real Kubernetes API server without requiring a full cluster:

```bash
# Run all tests including envtest
make test

# Run only controller tests with envtest
make test-envtest
```

**Requirements:** `setup-envtest` (installed automatically via `go run`)

## Roadmap

See [ROADMAP.md](ROADMAP.md) for planned features:

- **Restore** — restore from backup with conflict handling
- **User & Password Management** — customizable secrets, multi-database access
- **Database Features** — owner, templates, schemas, deletion protection
- **Access Control** — namespace isolation, validating webhook, IAM authentication
- **Secret Management** — AWS Secrets Manager, Vault, ESO integration

## Contributing

Contributions are welcome! Whether it's bug reports, feature requests, documentation improvements, or code contributions - I appreciate any help from the community.

Feel free to:
- Open an issue to report bugs or suggest features
- Submit a pull request with improvements
- Share your use cases and feedback

## License

Apache 2.0

