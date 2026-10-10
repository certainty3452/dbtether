# DBCluster

A connection to an external PostgreSQL server (Aurora, RDS, self-hosted). Cluster-scoped; short name `dbc`. Every Database and DatabaseUser points at one.

## Examples

Minimal:

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DBCluster
metadata:
  name: my-cluster
spec:
  endpoint: my-cluster.xxx.rds.amazonaws.com
  credentialsSecretRef:
    name: my-credentials
    namespace: dbtether
```

With the Secret and an explicit port:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-credentials
  namespace: dbtether
stringData:
  username: postgres_admin
  password: super-secret
---
apiVersion: dbtether.io/v1alpha1
kind: DBCluster
metadata:
  name: production
spec:
  endpoint: production.cluster-xxx.eu-central-1.rds.amazonaws.com
  port: 5432
  credentialsSecretRef:
    name: my-credentials
    namespace: dbtether
```

## Spec

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `endpoint` | string | required | Server hostname, without port |
| `port` | int | `5432` | 1-65535 |
| `credentialsSecretRef.name` | string | | Secret with keys `username` and `password` (name and namespace both required) |
| `credentialsSecretRef.namespace` | string | | Namespace of that Secret |
| `credentialsFromEnv.username` | string | | Name of an operator-pod env var holding the username |
| `credentialsFromEnv.password` | string | | Name of an operator-pod env var holding the password |

Set at least one credentials source. If both are set, `credentialsFromEnv` wins.

## Status

| Phase | Meaning |
|-------|---------|
| `Pending` | Waiting for the first reconcile |
| `Connected` | Connection works; `status.postgresVersion` is set |
| `Failed` | See `status.message` |

## Notes

- The role needs `CREATEDB`; on Aurora/RDS use the master user. It must own the databases it creates, so it can revoke `PUBLIC` access and write the ownership marker.
- `credentialsFromEnv` suits credentials injected as env vars (External Secrets, Vault Agent). Pass them with the chart's `extraEnv`:

  ```yaml
  extraEnv:
    - name: MY_CLUSTER_PASSWORD
      valueFrom:
        secretKeyRef: {name: external-secret, key: password}
  ```

  ```yaml
  spec:
    credentialsFromEnv:
      username: MY_CLUSTER_USERNAME  # variable name, not the value
      password: MY_CLUSTER_PASSWORD
  ```

- Backup and Restore Jobs read only `credentialsSecretRef`, see [BackupStorage](backupstorage.md#notes).
- A connected cluster is re-checked every 5 minutes, a failed one every 30 seconds.
- Deleting a DBCluster does not touch PostgreSQL. Its Databases fall back to `Pending` and reach `Failed` after 10 minutes.

## Troubleshooting

| Message | Cause and fix |
|---------|---------------|
| `connection failed: ...` | Endpoint unreachable or login rejected. The driver error says which. Check security groups and NetworkPolicies for operator pod egress: `nc -zv <endpoint> 5432` |
| `ping failed: ...` | The pool opened but the server did not answer: a failover or the connection limit. Retried automatically |
| `credentials error: ...` | The Secret is missing or lacks `username`/`password`, or an env var named in `credentialsFromEnv` is unset |
