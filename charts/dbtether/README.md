# dbtether Helm chart

Installs the dbtether operator and its CRDs. Usage and resource reference: [repository README](https://github.com/certainty3452/dbtether#readme).

## Install

```bash
helm upgrade -i dbtether oci://ghcr.io/certainty3452/charts/dbtether -n dbtether --create-namespace
```

## Upgrade

Helm installs `crds/` once and never upgrades it. Apply the new version's CRDs first, then upgrade the release:

```bash
helm show crds oci://ghcr.io/certainty3452/charts/dbtether --version <version> \
  | kubectl apply --server-side --force-conflicts -f -
helm upgrade dbtether oci://ghcr.io/certainty3452/charts/dbtether --version <version> -n dbtether
```

`--force-conflicts` is needed because Helm owns the CRD fields from the first install.

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | Operator replicas. Above 1 requires `leaderElection.enabled` |
| `revisionHistoryLimit` | `""` | Old ReplicaSets to keep (Kubernetes default when empty) |
| `image.repository` | `ghcr.io/certainty3452/dbtether` | Operator image, also used for backup and restore Jobs |
| `image.tag` | `""` | Defaults to the chart `appVersion` |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `nameOverride` / `fullnameOverride` | `""` | |
| `serviceAccount.create` | `true` | |
| `serviceAccount.name` | `""` | Generated from the release name when empty |
| `serviceAccount.annotations` | `{}` | Where the IRSA or Workload Identity binding goes |
| `podAnnotations` / `podLabels` | `{}` | Extra metadata on the operator pod |
| `podSecurityContext` | non-root, `RuntimeDefault` seccomp | |
| `securityContext` | no privilege escalation, drop all capabilities, read-only root filesystem | |
| `resources` | requests `100m` / `128Mi`, limits `500m` / `256Mi` | Operator pod |
| `nodeSelector` / `tolerations` / `affinity` | `{}` / `[]` / `{}` | |
| `leaderElection.enabled` | `true` | Lease-based election; standby replicas take over if the leader dies |
| `metrics.enabled` | `true` | Serve the metrics endpoint; `false` passes `--metrics-bind-address=0` and drops the container port |
| `metrics.port` | `8080` | Metrics endpoint port |
| `health.port` | `8081` | Probe endpoint port |
| `startupProbe` / `livenessProbe` / `readinessProbe` | `/healthz`, `/healthz`, `/readyz` | Full probe specs. The startup probe allows 5 minutes |
| `logging.level` | `info` | `debug`, `info` or `error` |
| `logging.format` | `json` | `json` or `console` |
| `backup.maxConcurrentPerCluster` | `3` | Concurrent backup and restore Jobs per DBCluster |
| `backup.resources` | requests `100m` / `384Mi`, limit memory `1Gi` | Backup and restore Job pods. Memory follows upload buffers, not database size; raise it for schemas with 100k+ objects |
| `backup.podAnnotations` | `{}` | Job pod annotations, e.g. `karpenter.sh/do-not-disrupt: "true"` |
| `backup.podLabels` | `{}` | Job pod labels |
| `backup.jobLabels` | `{}` | Job object labels |
| `extraEnv` | `[]` | Extra operator env vars: sources for DBCluster `credentialsFromEnv`, and `DB_SSLMODE` |

`DB_SSLMODE` (default `require`) sets the TLS mode of the operator's connections and of backup and restore Jobs:

```yaml
extraEnv:
  - name: DB_SSLMODE
    value: verify-full
```

## Cloud identity for storage

The operator ServiceAccount's identity is used for BackupStorage access by the operator and by backup and restore Jobs, so one identity must reach every configured bucket. For per-tenant isolation, install one operator per tenant.

```yaml
# AWS (IRSA)
serviceAccount:
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789:role/dbtether-backup-role
```

```yaml
# GCP (Workload Identity)
serviceAccount:
  annotations:
    iam.gke.io/gcp-service-account: dbtether@project.iam.gserviceaccount.com
```

```yaml
# Azure (Workload Identity)
serviceAccount:
  annotations:
    azure.workload.identity/client-id: <client-id>
podLabels:
  azure.workload.identity/use: "true"
backup:
  podLabels:
    azure.workload.identity/use: "true"
```

## Uninstall

Delete the Database, DatabaseUser, Backup, BackupSchedule and Restore resources first: each has a finalizer only the operator removes, so deleting them afterwards leaves them in `Terminating`.

```bash
helm uninstall dbtether -n dbtether
kubectl delete crd dbclusters.dbtether.io databases.dbtether.io databaseusers.dbtether.io \
  backupstorages.dbtether.io backups.dbtether.io backupschedules.dbtether.io restores.dbtether.io
```

Helm does not delete CRDs; the last command does, and removes every remaining resource of these kinds.
