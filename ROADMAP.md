# Roadmap

Planned, not implemented.

**Database**
- `spec.owner` (a DatabaseUser), `spec.template` (encoding/collation), `spec.schemas`
- `spec.deletionProtection` for Database and DatabaseUser
- Explicit adoption mode (`spec.adopt`)
- Periodic drift detection for Database (DatabaseUser is re-verified every 10 minutes)

**Multi-tenancy**
- Namespace restrictions on DBCluster and BackupStorage (explicit list or label selector)
- Validating webhook enforcing them

**Observability**
- Prometheus metrics for backup duration and size, role sync results and database state

**Authentication**
- IAM / managed-identity login to the database: AWS RDS and Aurora, Azure AD, GCP Cloud SQL

**Credentials in external stores**
- Write DatabaseUser credentials to AWS Secrets Manager, GCP Secret Manager, Azure Key Vault or Vault
- External Secrets Operator integration (`PushSecret`)
