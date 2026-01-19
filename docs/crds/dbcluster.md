# DBCluster

Represents an external PostgreSQL cluster (Aurora, RDS, self-hosted).

**API Version:** `dbtether.io/v1alpha1`  
**Kind:** `DBCluster`  
**Scope:** Cluster (not namespaced)  
**Short name:** `dbc`

## Example

### Option A: Credentials from Kubernetes Secret

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

### Option B: Credentials from Environment Variables

```yaml
apiVersion: dbtether.io/v1alpha1
kind: DBCluster
metadata:
  name: my-cluster
spec:
  endpoint: my-cluster.xxx.rds.amazonaws.com
  port: 5432
  credentialsFromEnv:
    username: MY_CLUSTER_USERNAME  # ENV variable name, not the value
    password: MY_CLUSTER_PASSWORD  # ENV variable name, not the value
```

## Spec

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `endpoint` | string | ✅ | — | PostgreSQL cluster hostname (without port) |
| `port` | int | ❌ | `5432` | PostgreSQL port (1-65535) |
| `credentialsSecretRef` | object | ❌* | — | Reference to K8s Secret with credentials |
| `credentialsFromEnv` | object | ❌* | — | ENV variable names for credentials |

\* One of `credentialsSecretRef` or `credentialsFromEnv` must be specified.

### credentialsSecretRef

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | string | ✅ | Name of the Secret containing credentials |
| `namespace` | string | ✅ | Namespace where the Secret is located |

### credentialsFromEnv

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `username` | string | ✅ | Name of ENV variable containing username |
| `password` | string | ✅ | Name of ENV variable containing password |

## Credentials

### Option A: Kubernetes Secret

The Secret must contain two required keys:

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

### Option B: Environment Variables

The operator reads credentials from its own pod environment. This is useful when:
- Using External Secrets Operator to inject secrets as ENV vars
- Using Vault Agent sidecar
- Mounting secrets via CSI driver

Configure in Helm values:

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

**Important:**
- User must have `CREATEDB` privileges to create databases
- For Aurora/RDS this is typically the master user

## Status

| Field | Type | Description |
|-------|------|-------------|
| `phase` | enum | Current state: `Pending`, `Connected`, `Failed` |
| `message` | string | Detailed status message |
| `postgresVersion` | string | PostgreSQL version (e.g., `PostgreSQL 16.11 on x86_64-pc-linux-gnu`) |
| `lastCheckTime` | timestamp | Time of last connection check |
| `observedGeneration` | int64 | Which spec version has been processed |

## Status Phases

| Phase | Description |
|-------|-------------|
| `Pending` | Initial state, waiting for first reconcile |
| `Connected` | Successfully connected to the cluster |
| `Failed` | Connection error (wrong credentials, unreachable endpoint) |

## Behavior

### Health Checks
- Operator checks connection every **5 minutes**
- On error — retry after **30 seconds**

### Connection Pooling
- Operator maintains a connection pool for each DBCluster
- When DBCluster is deleted — connections are closed

### Deletion
- When DBCluster resource is deleted, connections are closed
- Databases in PostgreSQL are **not deleted**
- Database resources referencing deleted DBCluster will transition to `Failed` status

## kubectl Commands

```bash
# List all clusters
kubectl get dbclusters
kubectl get dbc

# Cluster details
kubectl describe dbcluster my-cluster

# Check PostgreSQL version
kubectl get dbc my-cluster -o jsonpath='{.status.postgresVersion}'
```

## Troubleshooting

### Phase: Failed, message: "connection failed"

1. Check endpoint accessibility:
   ```bash
   nc -zv my-cluster.xxx.rds.amazonaws.com 5432
   ```

2. Verify credentials in Secret:
   ```bash
   kubectl get secret my-credentials -n postgres-db-operator -o yaml
   ```

3. Ensure operator pod can reach PostgreSQL (security groups, network policies)

### Phase: Failed, message: "credentials error"

Secret not found or missing required keys:
```bash
kubectl get secret my-credentials -n postgres-db-operator -o jsonpath='{.data.username}' | base64 -d
kubectl get secret my-credentials -n postgres-db-operator -o jsonpath='{.data.password}' | base64 -d
```
