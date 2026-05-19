# dbtether Roadmap

This document outlines planned features and improvements for the dbtether operator.

## Database Features

- [ ] **Database owner** via `spec.owner` (reference to DatabaseUser)
- [ ] **Database templates** via `spec.template` (for encoding/collation)
- [ ] **Schema management** via `spec.schemas` (create additional schemas beyond public)
- [ ] **Deletion protection** via `spec.deletionProtection` — prevents accidental `kubectl delete` of production databases and users
- [ ] **Explicit adoption mode** via `spec.adopt: true` for existing databases

## Multi-tenant Safety

- [ ] **Namespace isolation** on DBCluster via `spec.allowedNamespaces` (explicit list) and `spec.namespaceSelector` (label-based)
- [ ] **Validating Webhook** to enforce namespace restrictions on Database / DatabaseUser

## Observability

- [ ] **Custom Prometheus metrics** for backup duration, backup size, role sync results, and database state — for SRE dashboards
- [ ] **Periodic drift detection** for Database / DatabaseUser when resources are changed outside the operator

## Authentication

- [ ] **AWS IAM Authentication** for RDS / Aurora (IRSA, EKS Pod Identity)
- [ ] **Azure AD Authentication** for Azure Database for PostgreSQL (managed identities)
- [ ] **GCP IAM Authentication** for Cloud SQL (Workload Identity)

## Secret Management Integrations

Store credentials in external secret stores instead of (or alongside) Kubernetes Secrets.

### Option A: direct write to secret store

- [ ] AWS Secrets Manager via `spec.secretStore.aws`
- [ ] Google Cloud Secret Manager via `spec.secretStore.gcp`
- [ ] Azure Key Vault via `spec.secretStore.azure`
- [ ] HashiCorp Vault via `spec.secretStore.vault`

### Option B: External Secrets Operator (ESO) integration

- [ ] Create `PushSecret` resource for ESO to sync to external store
- [ ] Support `ExternalSecret` pattern (operator creates secret in store, ESO syncs back to K8s)

```yaml
spec:
  secretStore:
    type: aws-secretsmanager  # or vault, kubernetes (default)
    aws:
      secretName: /myapp/db-credentials
      region: eu-west-1
```

## Future Ideas

- [ ] **DatabaseSession CRD** — temporary proxy pods for local database access with TTL
