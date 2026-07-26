# dbtether

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/certainty3452/dbtether)](https://go.dev/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.31+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io/)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/dbtether)](https://artifacthub.io/packages/helm/dbtether/dbtether)

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
- **Database restore** - restore from backups with conflict handling (fail, drop, overwrite)
- **Multi-cloud storage** - backup to AWS S3, Google Cloud Storage, or Azure Blob Storage
- **Retention policies** - automatic cleanup with `keepLast`, `keepDaily`, `keepWeekly`, `keepMonthly`
- **Cloud-native auth** - IRSA, Workload Identity, Managed Identity for secure storage access

## Installation

### Using Helm (recommended)

```bash
helm upgrade -i dbtether oci://ghcr.io/certainty3452/charts/dbtether -n dbtether --create-namespace
```

### Using kubectl (from source)

```bash
# Install CRDs
kubectl apply -f config/crd/bases/

# Install RBAC and operator
kubectl apply -f config/rbac/
kubectl apply -f config/manager/
```

### Docker Image

```bash
docker pull ghcr.io/certainty3452/dbtether:latest
# Multi-arch: linux/amd64, linux/arm64
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
  database:
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
| [BackupStorage](docs/crds/backupstorage.md) | Cluster | S3/GCS/Azure storage configuration |
| [Backup](docs/crds/backup.md) | Namespaced | One-time database backup |
| [BackupSchedule](docs/crds/backupschedule.md) | Namespaced | Scheduled backups with retention policy |

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
- `spec.database.name` - Name of Database (for single database)
- `spec.databases[]` - List of databases (for multi-database access)
- `spec.privileges` - `readonly` (default), `readwrite`, `admin`, or `owner`
- `spec.username` - PostgreSQL username (defaults to metadata.name)
- `spec.password.length` - Password length (default 16, range 12-64)
- `spec.secretGeneration` - `primary` (default) or `perDatabase`
- `spec.secret.name` - Custom secret name (default: `{name}-credentials`)
- `spec.secret.template` - Key format: `raw` (default), `DB`, `DATABASE`, `POSTGRES`, `custom`, `dsn`
- `spec.secret.onConflict` - If secret exists: `Fail` (default), `Adopt`, `Merge`

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

**Restore:**
- `spec.source.latestFrom.databaseRef.name` - Auto-find latest backup for a database (recommended)
- `spec.source.latestFrom.namespace` - Namespace to search for backups (optional)
- `spec.source.backupRef.name` - Reference to a specific Backup CRD
- `spec.source.path` - Direct path to backup file (requires `storageRef`)
- `spec.source.storageRef.name` - BackupStorage for direct path
- `spec.target.databaseRef.name` - Target Database to restore into (required)
- `spec.onConflict` - `fail` (default), `drop`, or `overwrite`
- `spec.ttlAfterCompletion` - Auto-cleanup duration

## Required permissions and security implications

The operator runs with cluster-scoped RBAC. Two grants are worth understanding before installing into a multi-tenant cluster.

### Secrets (cluster-wide, full CRUD)

The operator's ClusterRole grants `get, list, watch, create, update, patch, delete` on `secrets` across **all namespaces**. This is required to:

- Read `DBCluster.spec.credentialsSecretRef` from any namespace (clusters are cluster-scoped, but their master credentials usually live in a platform namespace).
- Write generated `DatabaseUser` credentials into the user's namespace (which is arbitrary).
- Manage cross-namespace storage credentials referenced by `BackupStorage`.

**Blast radius:** compromise of the operator ServiceAccount token = read/write of every Secret in the cluster. Treat the operator namespace as a high-trust zone. Concretely:

- Pin the operator namespace as restricted in your admission policy (PSA/OPA).
- Do not co-locate untrusted workloads in the operator's namespace.
- Apply NetworkPolicies to limit egress from the operator pod to your DB endpoints only.
- Rotate the operator's ServiceAccount token if you suspect compromise; cluster-wide Secret access is what an attacker would target.

A namespace-scoped variant (operator only reads/writes Secrets in an allowlisted set of namespaces) is on the roadmap.

### Cluster-scoped CRs

`DBCluster` and `BackupStorage` are cluster-scoped. Any namespace can today create a `Database` or `DatabaseUser` referencing any `DBCluster`. If you run a shared platform with multiple tenants, see `spec.allowedNamespaces` on the roadmap — until it lands, gate `clusterRef` usage with admission policy (Kyverno / Gatekeeper / Validating Webhook).

### Backup pod identity

Backup and restore Jobs run under the same ServiceAccount as the operator. The IRSA / Workload Identity / Managed Identity role attached to it has access to **every** bucket configured via `BackupStorage`. If you need per-tenant storage isolation, use a separate operator install per tenant (each with its own ServiceAccount and cloud-IAM binding) rather than one operator with cluster-wide buckets.

### Storage probe IAM requirements

Since `0.6.2` the operator probes each `BackupStorage` on reconcile (every 30 minutes by default, and on every spec change). The probe issues one cheap call against the bucket/container to surface misconfiguration immediately instead of at first backup. This is a strict superset of what `0.6.1` required:

| Provider | Probe call | Required permission |
|----------|------------|---------------------|
| AWS S3 | `HeadBucket` | `s3:ListBucket` on the bucket |
| GCS | `Bucket.Attrs` | `storage.buckets.get` on the bucket |
| Azure Blob | `Container.GetProperties` | container-level Read (covered by `Storage Blob Data Reader` / `Contributor`) |

The probe verifies **auth and bucket existence**, not write access — if the role can list the bucket but lacks `s3:PutObject` / `storage.objects.create` / `Storage Blob Data Contributor`, the probe will report Ready and the first backup will fail. Treat the IAM policies below in the BackupStorage docs as the canonical write-path grants.

Probe failures continuously block *new* backup jobs (existing jobs continue normally). Transient cloud errors will briefly flip the storage to `Failed`; it auto-recovers within 60 seconds once the probe succeeds again.

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

