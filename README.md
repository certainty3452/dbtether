# dbtether

[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/dbtether)](https://artifacthub.io/packages/helm/dbtether/dbtether)

A Kubernetes operator that manages databases, users, backups and restores inside existing PostgreSQL servers (AWS Aurora, RDS, self-hosted) through CRDs. The platform team provisions the shared server; developers request databases in a pull request.

- Databases and users as resources, with `Retain` or `Delete` on removal
- Generated passwords in Kubernetes Secrets, with scheduled rotation
- `CONNECT` only on the databases a user is assigned
- One-time and scheduled backups (`pg_dump`, gzip) to S3, GCS or Azure Blob, with retention
- Restores with conflict handling and automatic re-granting

Unlike CloudNativePG (PostgreSQL inside the cluster) or Crossplane (one instance per resource), dbtether manages many databases in one shared server.

## Install

```bash
helm upgrade -i dbtether oci://ghcr.io/certainty3452/charts/dbtether -n dbtether --create-namespace
```

Upgrades (CRDs first) and chart values: [charts/dbtether/README.md](charts/dbtether/README.md).

## Quick example

The login needs `CREATEDB`; on Aurora/RDS use the master user.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: aurora-admin
  namespace: dbtether
stringData:
  username: postgres
  password: your-admin-password
---
apiVersion: dbtether.io/v1alpha1
kind: DBCluster
metadata:
  name: my-aurora
spec:
  endpoint: my-cluster.cluster-xxx.eu-west-1.rds.amazonaws.com
  credentialsSecretRef:
    name: aurora-admin
    namespace: dbtether
---
apiVersion: dbtether.io/v1alpha1
kind: Database
metadata:
  name: my-app-db
  namespace: default
spec:
  clusterRef:
    name: my-aurora
  extensions: [uuid-ossp]
---
apiVersion: dbtether.io/v1alpha1
kind: DatabaseUser
metadata:
  name: my-app
  namespace: default
spec:
  database:
    name: my-app-db
  privileges: readwrite
```

```bash
kubectl get dbclusters,databases,databaseusers -A
kubectl get secret my-app-credentials -o jsonpath='{.data.password}' | base64 -d
```

## Resources

Seven CRDs, from DBCluster to Restore: [docs/README.md](docs/README.md#resources).

## Security and multi-tenancy

- **Secrets.** The ClusterRole grants `get`, `list`, `watch`, `create`, `update`, `delete` on Secrets in all namespaces: it reads DBCluster and BackupStorage credentials and writes DatabaseUser Secrets into any namespace. A compromised operator token exposes every Secret, so treat the operator namespace as high-trust: restrict it by admission policy, keep untrusted workloads out, limit pod egress to your database endpoints.
- **Cluster-scoped references.** Any namespace can point a Database or DatabaseUser at any DBCluster, and a Backup, BackupSchedule or Restore at any BackupStorage. A Restore's `source.path` reads any object in that storage. On a shared platform gate `clusterRef` and `storageRef` with admission policy (Kyverno, Gatekeeper). Cross-namespace references to a Database are denied unless it lists the namespace in `spec.allowedNamespaces`.
- **Storage identity.** Backup and restore Jobs share the operator's cloud identity: [Cloud identity for storage](charts/dbtether/README.md#cloud-identity-for-storage).
- **Storage permissions** are listed in [BackupStorage](docs/crds/backupstorage.md#required-permissions).

## Development

```bash
make build             # bin/manager
make test              # unit and envtest suites
make test-unit         # ./pkg/... only
make test-integration  # needs DBTETHER_TEST_DSN, a superuser DSN of a scratch PostgreSQL 16+
make check             # fmt, vet, lint, gosec, govulncheck
make manifests         # regenerate CRDs into charts/dbtether/crds
make generate          # regenerate deepcopy code
make docker-build      # local image
```

`make test` downloads the envtest binaries into `bin/` on first run; no install step.

See [ROADMAP.md](ROADMAP.md) for planned work.

## License

Apache 2.0 — see [LICENSE](LICENSE).
