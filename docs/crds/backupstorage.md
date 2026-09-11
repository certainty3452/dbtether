# BackupStorage

Defines a storage destination for database backups (S3, GCS, Azure).

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `BackupStorage`  
**Short name:** `bs`  
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
| `credentialsSecretRef` | object | ❌ | — | Secret with S3 credentials; `name` and `namespace` are both required |

\* Exactly one provider. Admission rejects a spec with none of the three, and with `credentialsSecretRef` alongside `gcs` or `azure`; more than one provider is caught by the controller instead.

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `s3.bucket` | string | ✅ | S3 bucket name |
| `s3.region` | string | ✅ | AWS region (e.g., `eu-central-1`) |
| `s3.endpoint` | string | ❌ | Custom endpoint (for S3-compatible storage) |
| `gcs.bucket` | string | ✅ | GCS bucket name |
| `gcs.project` | string | ✅ | GCP project ID |
| `azure.container` | string | ✅ | Azure Blob container name |
| `azure.storageAccount` | string | ✅ | Azure storage account name |

## pathTemplate

The directory part of every object key; the filename comes from the Backup's [`filenameTemplate`](backup.md#filenametemplate).

| Variable | Description | Example |
|----------|-------------|---------|
| `.ClusterName` | Name of the DBCluster | `production` |
| `.DatabaseName` | PostgreSQL database name | `orders_db` |
| `.Year` `.Month` `.Day` | UTC date at backup time, zero-padded | `2026` `01` `20` |

| Template | Result |
|----------|--------|
| `{{ .ClusterName }}/{{ .DatabaseName }}` | `production/orders_db/` |
| `backups/{{ .Year }}/{{ .Month }}/{{ .ClusterName }}` | `backups/2026/01/production/` |
| `{{ .ClusterName }}/{{ .DatabaseName }}/{{ .Year }}-{{ .Month }}-{{ .Day }}` | `production/orders_db/2026-01-20/` |

**A date variable disables BackupSchedule retention.** Backup Jobs substitute `.Year` / `.Month` / `.Day`, but the retention pass rebuilds the prefix knowing only `.ClusterName` and `.DatabaseName`, so it lists `backups/<no value>/<no value>/production`, matches nothing, and deletes nothing — without reporting an error. Keep dates out of `pathTemplate` on any storage a schedule with `retention` writes to; put them in the Backup's `filenameTemplate` instead.

## Authentication

Without `credentialsSecretRef` the operator and its Jobs use the ServiceAccount's cloud identity:

| Provider | Method |
|----------|--------|
| AWS S3 | IRSA (IAM Roles for Service Accounts) or EKS Pod Identity |
| GCP GCS | Workload Identity |
| Azure | Managed Identity or Workload Identity |

```yaml
# ServiceAccount annotation for IRSA
eks.amazonaws.com/role-arn: arn:aws:iam::123456789:role/backup-role
```

`credentialsSecretRef` is S3-only, and the Secret must carry `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`. BackupStorage is cluster-scoped, so the reference needs an explicit `namespace`.

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
| `phase` | enum | `Ready` or `Failed` |
| `message` | string | `storage reachable`, or the validation/probe error |
| `provider` | string | Detected provider (`s3`, `gcs`, `azure`) |
| `lastValidation` | time | When the status was last written |
| `observedGeneration` | int64 | Which spec version has been processed |

## Reachability probe

On every reconcile, and at least every 30 minutes, the operator issues one low-cost call against the bucket or container, using the same auth path the backup Jobs will use. It surfaces bad credentials, a wrong region, a missing bucket or revoked IAM at `kubectl apply` time instead of an hour later in a backup Job's logs. The call has a 15-second budget; `Ready` requeues after 30 minutes, `Failed` after 60 seconds.

A `Failed` BackupStorage blocks *new* backup Jobs; Jobs already running are untouched.

### Required permissions

| Provider | Probe call | Required permission |
|----------|------------|---------------------|
| AWS S3 | `HeadBucket` | `s3:ListBucket` on the bucket |
| GCS | `Bucket.Attrs` | `storage.buckets.get` on the bucket |
| Azure Blob | `Container.GetProperties` | container-level Read (covered by `Storage Blob Data Reader` / `Contributor`) |

The probe checks auth and bucket existence, not write access: a role with `ListBucket` and no `PutObject` reports `Ready` and fails at the first backup. The IAM policy below is the full set.

## kubectl Commands

```bash
kubectl get bs
kubectl describe backupstorage production-backups
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
  pathTemplate: "{{ .ClusterName }}/{{ .DatabaseName }}"
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

Backups need write and list; retention needs delete; restore needs read.

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

Object tagging is optional and best-effort — without it the upload is retried untagged:

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

## Troubleshooting

### Rejected on apply: "one of s3, gcs, or azure must be specified"

No provider block. Add exactly one:

```yaml
# Wrong
spec:
  pathTemplate: "{{ .ClusterName }}/{{ .DatabaseName }}"

# Correct
spec:
  s3:
    bucket: my-bucket
    region: eu-central-1
```

### Rejected on apply: "credentialsSecretRef is only supported with S3; GCS and Azure must use Workload Identity / Managed Identity"

Drop `credentialsSecretRef` and give the operator's ServiceAccount the cloud identity instead.

### Phase: Failed, message: "only one of s3, gcs, or azure can be specified"

Two provider blocks are set. Split them into two BackupStorage resources.

### Phase: Failed, message: "s3 bucket \"&lt;name&gt;\" not reachable: ..."

The probe's `HeadBucket` failed. `403` means the identity lacks `s3:ListBucket` or a bucket policy denies it; `404`/`NoSuchBucket` means a wrong name; `301`/`PermanentRedirect` means a wrong `region`. GCS and Azure report the same shape (`gcs bucket ... not reachable`, `azure container ... not reachable`), with `does not exist` when the provider says so outright.

### Phase: Failed, message: "failed to build storage client: credentialsSecretRef ..."

The Secret could not be read. The three variants name the cause: `credentialsSecretRef.namespace is required for cluster-scoped BackupStorage`, `credentialsSecretRef <ns>/<name> not found`, `credentialsSecretRef <ns>/<name> missing AWS_ACCESS_KEY_ID or AWS_SECRET_ACCESS_KEY`.

### Storage is Ready but backups fail with AccessDenied

The probe only proves list access. Add the write-path permission from the IAM policy above — `s3:PutObject`, `storage.objects.create`, or `Storage Blob Data Contributor` — to the same identity.
