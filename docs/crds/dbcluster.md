# DBCluster

Represents an external PostgreSQL cluster (Aurora, RDS, self-hosted).

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `DBCluster`  
**Scope:** Cluster (not namespaced)  
**Short name:** `dbc`

## Example

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DBCluster
metadata:
  name: my-cluster
spec:
  endpoint: my-cluster.xxx.rds.amazonaws.com
  port: 5432
  credentialsSecretRef:
    name: my-credentials
    namespace: dbtether
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `endpoint` | string | ✅ | — | PostgreSQL cluster hostname (without port) |
| `port` | int | ❌ | `5432` | PostgreSQL port (1-65535) |
| `credentialsSecretRef` | object | ❌* | — | Reference to K8s Secret with credentials; `name` and `namespace` are both required |
| `credentialsFromEnv` | object | ❌* | — | Names of ENV variables holding the credentials; `username` and `password` are both required |

\* At least one must be set. If both are, `credentialsFromEnv` wins and the operator logs that it ignored the Secret.

## Credentials

The Secret must carry two keys:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: my-credentials
  namespace: dbtether
type: Opaque
stringData:
  username: postgres_admin
  password: super-secret
```

The role needs `CREATEDB` to create databases — on Aurora/RDS this is normally the master user. Databases it creates are owned by it, which is what lets the operator write its ownership marker and revoke PUBLIC access.

### credentialsFromEnv

`credentialsFromEnv` names environment variables that the operator reads from its own pod — useful when External Secrets, a Vault Agent sidecar, or a CSI driver injects credentials as ENV rather than into a Secret the operator can read.

```yaml
spec:
  endpoint: my-cluster.xxx.rds.amazonaws.com
  credentialsFromEnv:
    username: MY_CLUSTER_USERNAME  # ENV variable name, not the value
    password: MY_CLUSTER_PASSWORD
```

Set the variables through Helm:

```yaml
extraEnv:
  - name: MY_CLUSTER_USERNAME
    valueFrom:
      secretKeyRef:
        name: external-secret
        key: username
  - name: MY_CLUSTER_PASSWORD
    valueFrom:
      secretKeyRef:
        name: external-secret
        key: password
```

**It only covers this resource's own health check.** Database, DatabaseUser, Backup and Restore all read `credentialsSecretRef` directly, so a cluster with `credentialsFromEnv` alone reports `Connected` while everything pointing at it fails. Set `credentialsSecretRef` if the cluster hosts anything.

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | `Pending`, `Connected`, or `Failed` |
| `message` | string | Detailed status message |
| `postgresVersion` | string | `SELECT version()` output, e.g. `PostgreSQL 16.11 on x86_64-pc-linux-gnu` |
| `lastCheckTime` | timestamp | Time of last connection check |
| `observedGeneration` | int64 | Which spec version has been processed |

## Status Phases

| Phase | Description |
|-------|-------------|
| `Pending` | Initial state, waiting for first reconcile |
| `Connected` | Successfully connected to the cluster |
| `Failed` | Connection error (wrong credentials, unreachable endpoint) |

## Behavior

A connected cluster is re-checked every 5 minutes; a failed one retries every 30 seconds. The operator keeps one connection pool per DBCluster and closes it when the resource is deleted or a ping fails.

Deleting a DBCluster does not touch any database in PostgreSQL. Database resources that referenced it fall back to `Pending` with `waiting for DBCluster '<name>'`, and reach `Failed` after 10 minutes of that.

## kubectl Commands

```bash
kubectl get dbc
kubectl describe dbcluster my-cluster
kubectl get dbc my-cluster -o jsonpath='{.status.postgresVersion}'
```

## Troubleshooting

### Phase: Failed, message: "connection failed: ..."

The operator could not open a pool. Check reachability and that the operator pod's egress (security groups, NetworkPolicies) allows the endpoint:

```bash
nc -zv my-cluster.xxx.rds.amazonaws.com 5432
```

The quoted driver error distinguishes DNS failures, timeouts and rejected passwords.

### Phase: Failed, message: "ping failed: ..."

The pool opened but the server did not answer. The cached client is dropped, so the next reconcile reconnects from scratch. Persistent pings failures usually mean a failover in progress or a connection limit reached on the server.

### Phase: Failed, message: "credentials error: ..."

Either the Secret is missing (`secrets "my-credentials" not found`), it lacks a key (`secret must contain 'username' and 'password' keys`), or an ENV variable named by `credentialsFromEnv` is unset (`environment variable MY_CLUSTER_USERNAME not set or empty`).

```bash
kubectl get secret my-credentials -n dbtether -o jsonpath='{.data.username}' | base64 -d
```
