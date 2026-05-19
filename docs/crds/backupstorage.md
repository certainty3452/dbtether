# BackupStorage

Defines a storage destination for database backups (S3, GCS, Azure).

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `BackupStorage`  
**Scope:** Cluster

## Example

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupStorage
metadata:
  name: production-backups
spec:
  s3:
    bucket: my-backup-bucket
    region: eu-central-1
  pathTemplate: "{{ .ClusterName }}/{{ .DatabaseName }}"
  # Optional: use explicit credentials instead of IRSA/Pod Identity
  # credentialsSecretRef:
  #   name: s3-credentials
  #   namespace: dbtether
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `s3` | object | ❌* | — | S3 storage configuration |
| `gcs` | object | ❌* | — | GCS storage configuration |
| `azure` | object | ❌* | — | Azure Blob storage configuration |
| `pathTemplate` | string | ❌ | `{{ .ClusterName }}/{{ .DatabaseName }}` | Directory path template |
| `credentialsSecretRef` | object | ❌ | — | Secret with storage credentials |

**\* Note:** Exactly one of `s3`, `gcs`, or `azure` must be specified.

### S3 Configuration

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `s3.bucket` | string | ✅ | S3 bucket name |
| `s3.region` | string | ✅ | AWS region (e.g., `eu-central-1`) |
| `s3.endpoint` | string | ❌ | Custom endpoint (for S3-compatible storage) |

### GCS Configuration

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `gcs.bucket` | string | ✅ | GCS bucket name |
| `gcs.project` | string | ✅ | GCP project ID |

### Azure Configuration

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `azure.container` | string | ✅ | Azure Blob container name |
| `azure.storageAccount` | string | ✅ | Azure storage account name |

## pathTemplate

Template for the directory structure where backups are stored.

**Available variables:**

| Variable | Description | Example |
|----------|-------------|---------|
| `.ClusterName` | Name of the DBCluster | `production` |
| `.DatabaseName` | PostgreSQL database name | `orders_db` |
| `.Year` | Current year (4 digits) | `2026` |
| `.Month` | Current month (2 digits) | `01` |
| `.Day` | Current day (2 digits) | `20` |

**Examples:**

| Template | Result |
|----------|--------|
| `{{ .ClusterName }}/{{ .DatabaseName }}` | `production/orders_db/` |
| `backups/{{ .Year }}/{{ .Month }}/{{ .ClusterName }}` | `backups/2026/01/production/` |
| `{{ .ClusterName }}/{{ .DatabaseName }}/{{ .Year }}-{{ .Month }}-{{ .Day }}` | `production/orders_db/2026-01-20/` |

## Authentication

### Cloud-Native Auth (Recommended)

If `credentialsSecretRef` is not specified, the operator uses cloud-native authentication:

| Provider | Method |
|----------|--------|
| AWS S3 | IRSA (IAM Roles for Service Accounts) or EKS Pod Identity |
| GCP GCS | Workload Identity |
| Azure | Managed Identity or Workload Identity |

**AWS IRSA example:**
```yaml
# ServiceAccount annotation
eks.amazonaws.com/role-arn: arn:aws:iam::123456789:role/backup-role
```

### Secret-based Auth

For explicit credentials, create a Secret and reference it:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: s3-credentials
  namespace: dbtether
type: Opaque
stringData:
  AWS_ACCESS_KEY_ID: "AKIAIOSFODNN7EXAMPLE"
  AWS_SECRET_ACCESS_KEY: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
---
apiVersion: dbtether.io/v1alpha1
kind: BackupStorage
metadata:
  name: external-backups
spec:
  s3:
    bucket: external-bucket
    region: us-west-2
  credentialsSecretRef:
    name: s3-credentials
    namespace: dbtether
```

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | Current resource state (`Ready`, `Failed`) |
| `message` | string | Detailed message |
| `provider` | string | Detected provider (`s3`, `gcs`, `azure`) |
| `lastValidation` | time | Last time the storage was validated |
| `observedGeneration` | int64 | Which spec version has been processed |

## Status Phases

| Phase | Description |
|-------|-------------|
| `Ready` | Spec validated and the bucket/container is reachable with current credentials |
| `Failed` | Configuration error or reachability probe failed (see `message`) |

## Reachability probe

On every reconcile (and at least every 30 minutes) the operator issues a single low-cost call against the bucket / container — `HeadBucket` for S3, `Bucket.Attrs` for GCS, `Container.GetProperties` for Azure. The intent is to surface misconfigured credentials, wrong region, missing bucket, or revoked IAM **immediately** at `kubectl apply` time, not an hour later when the first backup job fails.

### Required permissions

| Provider | Probe call | Required permission |
|----------|------------|---------------------|
| AWS S3 | `HeadBucket` | `s3:ListBucket` on the bucket |
| GCS | `Bucket.Attrs` | `storage.buckets.get` on the bucket |
| Azure Blob | `Container.GetProperties` | container-level Read (covered by `Storage Blob Data Reader` / `Contributor`) |

These are in addition to the write-path grants (`s3:PutObject` / `storage.objects.create` / Blob Data Contributor) needed for actual backups — see [IAM Policy (AWS S3)](#iam-policy-aws-s3) below.

### What the probe does NOT verify

The probe checks **auth + bucket existence**, not **write access**. A role with `ListBucket` but no `PutObject` will pass the probe and fail at first backup. Keep the IAM policies in this doc as the source of truth for the full set of permissions.

### Transient failures

A probe failure flips status to `Failed` and blocks *new* backup jobs (existing jobs continue uninterrupted). Transient cloud errors auto-recover within 60 seconds. Persistent failures requeue every 60 seconds; successful state requeues every 30 minutes.

### credentialsSecretRef

Only honored when `s3` is configured. The CRD admission rule rejects `credentialsSecretRef` together with `gcs` or `azure` — those providers must authenticate via Workload Identity / Managed Identity. When using `credentialsSecretRef` on S3, the Secret must contain `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` keys; the operator reads them to build the probe client matching what subsequent jobs will use.

## kubectl Commands

```bash
# List all backup storages
kubectl get backupstorage
kubectl get bs  # short name

# Storage details
kubectl describe backupstorage production-backups

# Check status
kubectl get bs production-backups -o jsonpath='{.status.phase}'
```

## Examples

### S3 with IRSA (AWS)

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupStorage
metadata:
  name: aws-backups
spec:
  s3:
    bucket: company-pg-backups
    region: eu-central-1
  pathTemplate: "{{ .ClusterName }}/{{ .DatabaseName }}"
```

### GCS with Workload Identity

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupStorage
metadata:
  name: gcp-backups
spec:
  gcs:
    bucket: company-pg-backups
    project: my-gcp-project
  pathTemplate: "{{ .ClusterName }}/{{ .DatabaseName }}/{{ .Year }}"
```

### Azure with Managed Identity

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupStorage
metadata:
  name: azure-backups
spec:
  azure:
    container: pg-backups
    storageAccount: companybackups
```

### S3-compatible Storage (Custom Endpoint)

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupStorage
metadata:
  name: minio-backups
spec:
  s3:
    bucket: backups
    region: us-east-1
    endpoint: https://minio.internal:9000
  credentialsSecretRef:
    name: minio-credentials
    namespace: dbtether
```

## IAM Policy (AWS S3)

Minimum IAM permissions for IRSA:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:GetObject",
        "s3:DeleteObject",
        "s3:ListBucket"
      ],
      "Resource": [
        "arn:aws:s3:::my-backup-bucket",
        "arn:aws:s3:::my-backup-bucket/*"
      ]
    }
  ]
}
```

**Optional** (for object tagging):
```json
{
  "Effect": "Allow",
  "Action": [
    "s3:PutObjectTagging",
    "s3:GetObjectTagging"
  ],
  "Resource": "arn:aws:s3:::my-backup-bucket/*"
}
```

> **Note:** S3 tagging is best-effort. If `s3:PutObjectTagging` permission is missing, backup will succeed without tags.

## Troubleshooting

### Phase: Failed, message: "no storage provider specified"

Ensure exactly one of `s3`, `gcs`, or `azure` is configured:

```yaml
spec:
  s3:  # Must specify one provider
    bucket: my-bucket
    region: eu-central-1
```

### Phase: Failed, message: "multiple storage providers specified"

Only one provider can be active. Remove extra providers.

### Backup fails with "AccessDenied"

1. Check IAM role/policy permissions
2. Verify IRSA annotation on ServiceAccount
3. Check if bucket policy allows access
4. Verify `credentialsSecretRef` points to valid secret

