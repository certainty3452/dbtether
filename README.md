# dbtether

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/certainty3452/dbtether)](https://go.dev/)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.31+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io/)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/dbtether)](https://artifacthub.io/packages/helm/dbtether/dbtether)

> Kubernetes operator for external PostgreSQL databases - manage AWS Aurora, RDS, and self-hosted databases and users declaratively via CRDs.

Tether your databases to Kubernetes. Create databases and users in existing PostgreSQL clusters through GitOps workflows. Built for platform teams where the infrastructure team provisions shared Aurora/RDS clusters and developers self-serve databases inside them.

## Why This Operator?

Existing tools solve a different problem:

- **CloudNativePG** runs PostgreSQL inside the Kubernetes cluster, which means PV/PVC management and an in-cluster database to operate.
- **Crossplane** provisions a separate database instance per resource, which gets expensive when what you need is several databases in one shared cluster.

Both are built for **isolation** — a cluster or instance per team. dbtether is built for **manageability** in a shared cluster: it connects to an existing PostgreSQL-compatible server and manages databases and users inside it via CRDs, so a database can be requested in a pull request instead of a ticket.

Typical uses: Helm charts that need a database, Backstage self-service templates, ArgoCD-driven provisioning, and ephemeral per-branch databases that clean themselves up on teardown.

## Features

- **Declarative management** - databases and users as Kubernetes CRDs
- **Auto-generated credentials** - passwords generated into Kubernetes Secrets, with scheduled rotation
- **Database isolation** - users are granted `CONNECT` only on their assigned databases
- **Configurable deletion policies** - `Retain` or `Delete` per database and per user
- **Database backups** - one-time and scheduled, `pg_dump` → gzip → cloud storage
- **Database restore** - transactional, with conflict handling (`fail`, `drop`) and automatic re-granting
- **Multi-cloud storage** - AWS S3, Google Cloud Storage, Azure Blob Storage
- **Retention policies** - `keepLast`, `keepDaily`, `keepWeekly`, `keepMonthly`
- **Cloud-native auth** - IRSA, Workload Identity, Managed Identity for storage access

## Installation

### Using Helm (recommended)

```bash
helm upgrade -i dbtether oci://ghcr.io/certainty3452/charts/dbtether -n dbtether --create-namespace
```

### Using kubectl (from source)

```bash
kubectl apply -f config/crd/bases/
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
  namespace: dbtether
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
    namespace: dbtether
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
kubectl get dbclusters
NAME               ENDPOINT                                    PHASE      VERSION   AGE
my-aurora-cluster  my-cluster.xxx.rds.amazonaws.com            Connected  15.4      5m

kubectl get databases -A
NAMESPACE   NAME        CLUSTER            DATABASE   PHASE   AGE
default     my-app-db   my-aurora-cluster  my_app     Ready   2m

kubectl get databaseusers -A
NAMESPACE   NAME             CLUSTER            DATABASES   USERNAME          PRIVILEGES   PHASE   AGE
default     my-app-readonly  my-aurora-cluster  my_app      my_app_readonly   readonly     Ready   1m

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
| [Restore](docs/crds/restore.md) | Namespaced | Restore a database from a backup |

### Quick Reference

**DBCluster:**
- `spec.endpoint` - PostgreSQL hostname (required)
- `spec.port` - Port, default 5432
- `spec.credentialsSecretRef` - Secret with `username`/`password`; required for anything beyond this resource's own health check

**Database:**
- `spec.clusterRef.name` - Name of DBCluster (required)
- `spec.databaseName` - Database name in PostgreSQL; defaults to `metadata.name` with `-` replaced by `_`
- `spec.extensions` - List of PostgreSQL extensions
- `spec.deletionPolicy` - `Retain` (default) or `Delete`
- `spec.revokePublicConnect` - Revoke `CONNECT` from `PUBLIC`, default `false`

**DatabaseUser:**
- `spec.database.name` - Name of Database (for single database)
- `spec.databases[]` - List of databases (for multi-database access)
- `spec.privileges` - `readonly` (default), `readwrite`, `admin`, or `owner`
- `spec.username` - PostgreSQL username; defaults to `metadata.name` with `-` replaced by `_`
- `spec.password.length` - Password length (default 16, range 12-64)
- `spec.secretGeneration` - `primary` (default) or `perDatabase`
- `spec.secret.name` - Custom secret name (default: `{name}-credentials`)
- `spec.secret.template` - Key format: `raw` (default), `DB`, `DATABASE`, `POSTGRES`, `custom`, `dsn`
- `spec.secret.onConflict` - If secret exists: `Fail` (default), `Adopt`, `Merge`

**BackupStorage:**
- `spec.s3.bucket` - S3 bucket name (required for S3)
- `spec.s3.region` - AWS region (required for S3)
- `spec.pathTemplate` - Path template (default: `{{ .ClusterName }}/{{ .DatabaseName }}`)
- `spec.credentialsSecretRef` - S3 only; uses IRSA/Pod Identity if omitted

**Backup:**
- `spec.databaseRef.name` - Name of Database to backup (required)
- `spec.storageRef.name` - Name of BackupStorage (required)
- `spec.filenameTemplate` - Filename template (default: `{{ .Timestamp }}.sql.gz`)
- `spec.trigger` - Opaque value that only feeds the spec hash; change it to run the backup again
- `spec.ttlAfterCompletion` - Job auto-cleanup duration (default: 1h)

**BackupSchedule:**
- `spec.databaseRef.name` - Name of Database to backup (required)
- `spec.storageRef.name` - Name of BackupStorage (required)
- `spec.schedule` - 5-field cron, e.g. `0 2 * * *` for 2 AM daily (required)
- `spec.retention.keepLast` / `keepDaily` / `keepWeekly` / `keepMonthly` - at least one must be positive
- `spec.suspend` - Pause scheduling and retention

**Restore:**
- `spec.source.latestFrom.databaseRef.name` - Auto-find latest backup for a database (recommended)
- `spec.source.latestFrom.namespace` - Namespace to search for backups (optional)
- `spec.source.backupRef.name` - Reference to a specific Backup CRD
- `spec.source.path` - Direct path to backup file (requires `storageRef`)
- `spec.source.storageRef.name` - BackupStorage for direct path
- `spec.target.databaseRef.name` - Target Database, in the Restore's own namespace (required)
- `spec.onConflict` - `fail` (default) or `drop`
- `spec.ttlAfterCompletion` - Job auto-cleanup duration (default: 1h)

## Required permissions and security implications

The operator runs with cluster-scoped RBAC. Two grants are worth understanding before installing into a multi-tenant cluster.

### Secrets (cluster-wide, full CRUD)

The ClusterRole grants `get, list, watch, create, update, patch, delete` on `secrets` in **all namespaces**, because the operator must:

- read `DBCluster.spec.credentialsSecretRef` from any namespace (clusters are cluster-scoped, but their master credentials usually live in a platform namespace);
- write generated `DatabaseUser` credentials into the user's namespace, which is arbitrary;
- read storage credentials referenced by `BackupStorage`.

**Blast radius:** compromise of the operator ServiceAccount token means read/write of every Secret in the cluster. Treat the operator namespace as a high-trust zone:

- mark it restricted in your admission policy (PSA/OPA);
- do not co-locate untrusted workloads there;
- limit egress from the operator pod to your database endpoints;
- rotate the ServiceAccount token if you suspect compromise.

### Cluster-scoped CRs

`DBCluster` and `BackupStorage` are cluster-scoped, and any namespace can create a `Database` or `DatabaseUser` referencing any `DBCluster`. On a shared platform, gate `clusterRef` usage with admission policy (Kyverno / Gatekeeper / a validating webhook).

### Backup pod identity

Backup and restore Jobs run under the operator's ServiceAccount, so its cloud identity can reach **every** bucket configured through `BackupStorage`. For per-tenant storage isolation, run a separate operator install per tenant, each with its own ServiceAccount and IAM binding.

### Storage probe IAM requirements

The operator probes each `BackupStorage` on reconcile, at least every 30 minutes, with one cheap call so misconfiguration surfaces immediately instead of at the first backup:

| Provider | Probe call | Required permission |
|----------|------------|---------------------|
| AWS S3 | `HeadBucket` | `s3:ListBucket` on the bucket |
| GCS | `Bucket.Attrs` | `storage.buckets.get` on the bucket |
| Azure Blob | `Container.GetProperties` | container-level Read (covered by `Storage Blob Data Reader` / `Contributor`) |

The probe verifies auth and bucket existence, not write access: a role that can list the bucket but lacks `s3:PutObject` / `storage.objects.create` / `Storage Blob Data Contributor` reports `Ready` and fails at the first backup. The IAM policies in the [BackupStorage docs](docs/crds/backupstorage.md#iam-policy-aws-s3) are the canonical write-path grants.

A `Failed` probe blocks *new* backup Jobs; running Jobs are unaffected. Failed storages are retried every 60 seconds.

## Development

```bash
make build            # compile bin/manager
make test             # full suite, including envtest (alias for test-envtest)
make test-unit        # ./pkg/... only
make check            # fmt, vet, lint, gosec, govulncheck
make docker-buildx    # multi-arch image
```

### Testing with envtest

Controller tests use [envtest](https://book.kubebuilder.io/reference/envtest.html), a real Kubernetes API server without a full cluster. `make test-envtest` resolves the binaries through `setup-envtest`, so install it once:

```bash
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
```

The PostgreSQL integration tests need a superuser DSN to a scratch server (PostgreSQL 16+) and are skipped when it is unset:

```bash
DBTETHER_TEST_DSN=postgres://postgres:it@localhost:55432/postgres?sslmode=disable make test-integration
```

## Roadmap

See [ROADMAP.md](ROADMAP.md).

## Contributing

Bug reports, feature requests, documentation fixes and pull requests are all welcome — open an issue or a PR.

## License

Apache 2.0
