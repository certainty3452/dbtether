# BackupStorage

A bucket or container for backups: S3 (or S3-compatible), GCS or Azure Blob. Cluster-scoped; short name `bs`.

## Examples

Minimal (authenticates with the operator ServiceAccount's cloud identity):

```yaml
apiVersion: dbtether.io/v1alpha1
kind: BackupStorage
metadata:
  name: production-backups
spec:
  s3:
    bucket: my-backup-bucket
    region: eu-central-1
```

S3-compatible storage with static credentials:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: minio-credentials
  namespace: dbtether
stringData:
  AWS_ACCESS_KEY_ID: minio
  AWS_SECRET_ACCESS_KEY: minio-secret
---
apiVersion: dbtether.io/v1alpha1
kind: BackupStorage
metadata:
  name: minio-backups
spec:
  s3:
    bucket: backups
    region: us-east-1
    endpoint: https://minio.internal:9000
  pathTemplate: "{{ .ClusterName }}/{{ .DatabaseName }}"
  credentialsSecretRef:
    name: minio-credentials
    namespace: dbtether
```

GCS and Azure use the workload identity of the operator ServiceAccount:

```yaml
spec:
  gcs:
    bucket: company-pg-backups
    project: my-gcp-project
```

```yaml
spec:
  azure:
    container: pg-backups
    storageAccount: companybackups
```

## Spec

Exactly one of `s3`, `gcs`, `azure`. Admission rejects none of them and `credentialsSecretRef` together with `gcs` or `azure`; two providers fail in the controller.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `s3.bucket` | string | required | Bucket name |
| `s3.region` | string | required | e.g. `eu-central-1` |
| `s3.endpoint` | string | | Custom endpoint for S3-compatible storage |
| `gcs.bucket` | string | required | Bucket name |
| `gcs.project` | string | required | GCP project ID |
| `azure.container` | string | required | Blob container |
| `azure.storageAccount` | string | required | Storage account |
| `pathTemplate` | string | `{{ .ClusterName }}/{{ .DatabaseName }}` | Directory part of every object key |
| `credentialsSecretRef` | object | | S3 only. `name` and `namespace` required; Secret keys `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` |

`pathTemplate` variables: `.ClusterName`, `.DatabaseName`, and the UTC date `.Year`, `.Month`, `.Day` (zero-padded). The filename comes from the Backup's [`filenameTemplate`](backup.md#spec). Example: `backups/{{ .Year }}/{{ .ClusterName }}` gives `backups/2026/production/`.

## Status

| Phase | Meaning |
|-------|---------|
| `Ready` | The probe succeeded; `status.provider` is `s3`, `gcs` or `azure` |
| `Failed` | See `status.message`. New backup Jobs are blocked; running Jobs are not |

## Notes

- **Authentication.** Without `credentialsSecretRef` the operator and its Jobs use the ServiceAccount identity: IRSA or EKS Pod Identity (AWS), Workload Identity (GCP), Managed or Workload Identity (Azure). Annotate it through the chart's `serviceAccount.annotations`.
- **Credentials Secrets live in the operator namespace.** Backup and restore Jobs run there and read Secrets only from it, and they cannot use `credentialsFromEnv`. So the DBCluster behind a Backup or Restore needs `credentialsSecretRef`, and that Secret and this storage's `credentialsSecretRef` Secret must both be in the operator namespace.
- **Probe.** On every reconcile and at least every 30 minutes, one call checks auth and the bucket (15 s budget); `Failed` is retried after 60 s. It does not test write access: a role with list but no write reports `Ready` and fails at the first backup.
- **Dates in `pathTemplate` disable retention.** Keep them out on storage used by a schedule with `retention`; see [BackupSchedule](backupschedule.md#notes).
- **One identity for all storages.** See [Cloud identity for storage](../../charts/dbtether/README.md#cloud-identity-for-storage).

## Required permissions

| Provider | Probe call | Probe permission | Backup, retention, `deletionPolicy: Delete` and restore |
|----------|------------|------------------|---------------------------------------------------------|
| AWS S3 | `HeadBucket` | `s3:ListBucket` | `s3:PutObject`, `s3:GetObject`, `s3:DeleteObject`, `s3:PutObjectTagging`, `s3:AbortMultipartUpload` |
| GCS | `Bucket.Attrs` | `storage.buckets.get` | `storage.objects.create`; get, list and delete objects for restore, retention and `deletionPolicy: Delete`; overwriting an existing object (fixed `filenameTemplate`) also needs `storage.objects.delete` |
| Azure | `Container.GetProperties` | container Read | `Storage Blob Data Contributor` |

Uploads carry object tags, so S3 needs `s3:PutObjectTagging` or every upload fails.

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": [
      "s3:ListBucket", "s3:PutObject", "s3:GetObject", "s3:DeleteObject",
      "s3:PutObjectTagging", "s3:AbortMultipartUpload"
    ],
    "Resource": ["arn:aws:s3:::my-backup-bucket", "arn:aws:s3:::my-backup-bucket/*"]
  }]
}
```

## Troubleshooting

| Message | Cause and fix |
|---------|---------------|
| Rejected: `one of s3, gcs, or azure must be specified` | Add a provider block |
| Rejected: `credentialsSecretRef is only supported with S3` | Remove it and use workload identity for GCS and Azure |
| `only one of s3, gcs, or azure can be specified` | Split into two BackupStorages |
| `s3 bucket "<name>" not reachable: ...` | `403`: identity lacks `s3:ListBucket`; `404`: wrong bucket; `301`: wrong `region`. GCS and Azure report the same shape |
| `failed to build storage client: credentialsSecretRef ...` | Secret not found, missing a key, or `namespace` not set |
| `Ready`, but backups fail with `AccessDenied` | Add the write permissions above |
