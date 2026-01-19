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

I was looking for a middle ground: delegate **cluster provisioning** to infrastructure (Terraform, AWS Console, etc.) while giving **developers self-service** for databases and users via GitOps.

This operator fills that gap. It connects to existing PostgreSQL-compatible clusters (AWS Aurora, RDS, or self-hosted) and manages databases and users declaratively through CRDs. Combined with ArgoCD, developers can provision databases via pull requests - or even expose it through a Helm chart if you're feeling adventurous.

As a GitOps enthusiast, this operator fits perfectly into my workflow. I hope it helps others facing the same challenge.

## Use Cases

- **Manage RDS/Aurora from Kubernetes** - connect to existing AWS database clusters and create databases via CRDs
- **Self-service database provisioning** - developers request databases via pull requests, platform team approves, GitOps applies
- **Multi-tenant database management** - one Aurora cluster, multiple databases with isolated users per team/namespace
- **Database-as-Code with ArgoCD** - declarative database and user management synced from Git

## Features

- **Declarative management** - manage databases and users via Kubernetes CRDs
- **GitOps-friendly** - works seamlessly with ArgoCD, Flux, and other GitOps tools
- **Auto-generated credentials** - secure passwords stored in Kubernetes Secrets
- **Password rotation** - automatic credential rotation with configurable schedule
- **Database isolation** - users are granted access only to their assigned database (cannot query other databases)
- **Configurable deletion policies** - choose between Retain (keep data) or Delete on resource removal

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

### User & Password Management
- [ ] Customizable secret keys via `spec.secretTemplate` (choose between `DATABASE_*`, `DB_*`, or custom keys)

### Database Features
- [ ] Database owner via `spec.owner` (reference to DatabaseUser)
- [ ] Database templates via `spec.template` (for encoding/collation)
- [ ] Schema management via `spec.schemas` (create additional schemas beyond public)
- [ ] Deletion protection via `spec.deletionProtection`

### Observability
- [ ] Periodic drift detection for Database/DatabaseUser - detect if resources were deleted externally and update status

### Access Control & Security

**Namespace Isolation (recommended)**
- [ ] `spec.allowedNamespaces` on DBCluster - explicit list of namespaces that can reference this cluster
- [ ] `spec.namespaceSelector` on DBCluster - label selector for allowed namespaces (e.g., `team=backend`)
- [ ] Validating Webhook to enforce namespace restrictions when creating Database/DatabaseUser

**Authentication Improvements (optional, for sensitive data)**
- [ ] AWS IAM Authentication for RDS/Aurora - use AWS IAM roles instead of long-lived passwords. Operator gets temporary credentials via IRSA (IAM Roles for Service Accounts). Eliminates secret sprawl, enables CloudTrail audit.
- [ ] Azure AD Authentication for Azure Database for PostgreSQL - use managed identities
- [ ] GCP IAM Authentication for Cloud SQL - use Workload Identity

### Database Import
- [ ] Adopt existing databases via `spec.adopt: true` - safely take over management of existing databases without recreating them

### Backup/Restore

Architecture: separate image for backup runner (resource isolation, pg_dump tools)

```
dbtether              postgres-db-backup (separate image)
├── Backup Controller             └── pg_dump + S3 upload
│   └── creates Job/CronJob ──────► runs backup
```

- [ ] DatabaseBackup CRD for on-demand and scheduled backups
- [ ] `postgres-db-backup` image with pg_dump and aws cli
- [ ] S3 upload with configurable retention
- [ ] DatabaseRestore CRD for point-in-time recovery

### Secret Management Integrations

Goal: Allow storing credentials in external secret stores instead of (or in addition to) Kubernetes Secrets.

**Option A: Direct write to secret store**
- [ ] AWS Secrets Manager integration via `spec.secretStore.aws`
- [ ] Google Cloud Secret Manager integration via `spec.secretStore.gcp`
- [ ] Azure Key Vault integration via `spec.secretStore.azure`
- [ ] HashiCorp Vault integration via `spec.secretStore.vault`

**Option B: External Secrets Operator (ESO) integration**
- [ ] Create `PushSecret` resource for ESO to sync to external store
- [ ] Support `ExternalSecret` pattern (operator creates secret in store, ESO syncs back to K8s)

```yaml
# Example future API
spec:
  secretStore:
    type: aws-secretsmanager  # or vault, kubernetes (default)
    aws:
      secretName: /myapp/db-credentials
      region: eu-west-1
```

### Future Ideas
- [ ] DatabaseSession CRD - temporary proxy pods for local database access with TTL

## Contributing

Contributions are welcome! Whether it's bug reports, feature requests, documentation improvements, or code contributions - I appreciate any help from the community.

Feel free to:
- Open an issue to report bugs or suggest features
- Submit a pull request with improvements
- Share your use cases and feedback

## License

Apache 2.0

