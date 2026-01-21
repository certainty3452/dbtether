# dbtether Roadmap

This document outlines planned features and improvements for the dbtether operator.

## User & Password Management

- [ ] **Customizable secret keys** via `spec.secretTemplate`
  - Choose between `DATABASE_*`, `DB_*`, or custom keys

- [ ] **Multi-database access** — decouple user from single database binding:
  - `spec.databaseRefs[]` instead of `spec.databaseRef` — grant access to multiple databases
  - Phase 1: same-namespace only
  - Phase 2: cross-namespace with opt-in via `dbtether.io/allow-users-from` annotation or team labels on namespaces

## Database Features

- [ ] **Database owner** via `spec.owner` (reference to DatabaseUser)
- [ ] **Database templates** via `spec.template` (for encoding/collation)
- [ ] **Schema management** via `spec.schemas` (create additional schemas beyond public)
- [ ] **Deletion protection** via `spec.deletionProtection`
- [ ] **Explicit adoption mode** via `spec.adopt: true` for existing databases

## Observability

- [ ] **Periodic drift detection** for Database/DatabaseUser
  - Detect if resources were deleted externally and update status

## Access Control & Security

### Namespace Isolation (recommended)

- [ ] `spec.allowedNamespaces` on DBCluster — explicit list of namespaces that can reference this cluster
- [ ] `spec.namespaceSelector` on DBCluster — label selector for allowed namespaces (e.g., `team=backend`)
- [ ] **Validating Webhook** to enforce namespace restrictions when creating Database/DatabaseUser

### Authentication Improvements (optional, for sensitive data)

- [ ] **AWS IAM Authentication** for RDS/Aurora
  - Use AWS IAM roles instead of long-lived passwords
  - Operator gets temporary credentials via IRSA (IAM Roles for Service Accounts)
  - Eliminates secret sprawl, enables CloudTrail audit

- [ ] **Azure AD Authentication** for Azure Database for PostgreSQL
  - Use managed identities

- [ ] **GCP IAM Authentication** for Cloud SQL
  - Use Workload Identity

## Secret Management Integrations

Goal: Allow storing credentials in external secret stores instead of (or in addition to) Kubernetes Secrets.

### Option A: Direct write to secret store

- [ ] AWS Secrets Manager integration via `spec.secretStore.aws`
- [ ] Google Cloud Secret Manager integration via `spec.secretStore.gcp`
- [ ] Azure Key Vault integration via `spec.secretStore.azure`
- [ ] HashiCorp Vault integration via `spec.secretStore.vault`

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
