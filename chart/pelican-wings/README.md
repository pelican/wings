# Pelican Wings Helm Chart

Deploys [Pelican Wings](https://github.com/pelican/wings) — the game server
management daemon — into a Kubernetes cluster with full RBAC, storage, and
networking support.

## Prerequisites

- Kubernetes 1.34+
- Helm 3.x
- A running [Pelican Panel](https://github.com/pelican/panel) instance

## Quick Start

Put your node credentials from the Panel in a local values file (kept out of
version control) rather than on the command line, where `--set` would leak them
into shell history and process listings:

```yaml
# values.local.yaml (do not commit)
wings:
  panelUrl: https://panel.example.com
  token: YOUR_TOKEN
  tokenId: YOUR_TOKEN_ID
  uuid: YOUR_NODE_UUID
```

```bash
helm install wings ./chart/pelican-wings -f values.local.yaml
```

## Configuration

See [values.yaml](values.yaml) for the full list of configurable values.

### Key Values

| Parameter | Description | Default |
|-----------|-------------|---------|
| `wings.panelUrl` | Panel URL | `https://panel.example.com` |
| `wings.token` | Panel authentication token | `""` |
| `wings.tokenId` | Panel token ID | `""` |
| `wings.uuid` | Node UUID from Panel | `""` |
| `wings.kubernetes.networkMode` | Port exposure: `hostport`, `nodeport`, or `loadbalancer` | `nodeport` |
| `wings.kubernetes.storageMode` | Storage: `hostpath` or `pvc` | `pvc` |
| `wings.kubernetes.storageClass` | StorageClass for PVCs | `""` (cluster default) |
| `wings.kubernetes.storageSize` | Default PVC size | `10Gi` |
| `wings.kubernetes.imagePullPolicy` | Pull policy for game server Pods/install Jobs: `Always`, `IfNotPresent`, `Never` | `""` (smart default) |
| `gameNamespace` | Namespace for game server resources | `pelican` |
| `rbac.create` | Create RBAC resources | `true` |
| `rbac.kubeletMetricsFallback` | Grant cluster-wide `nodes/proxy` for the kubelet stats fallback (broad permission; prefer metrics-server) | `false` |
| `serviceAccount.create` | Create ServiceAccount | `true` |
| `serviceAccount.name` | ServiceAccount name (**required** when `serviceAccount.create=false`) | `""` |

> **Namespace:** Wings schedules game-server workloads into `gameNamespace`, and
> the chart creates the namespaced RBAC there. `wings.kubernetes.namespace` is
> therefore derived from `gameNamespace`; if you set it explicitly it must match
> `gameNamespace` or the chart will fail to render.

> **Credentials:** `wings.token`, `wings.tokenId`, and `wings.uuid` are rendered
> into a Kubernetes **Secret** (not a ConfigMap). Supply them via a private
> values file or `--set`, e.g. `helm install ... -f my-creds.yaml`, and keep that
> file out of version control.

### Storage

By default, the chart uses PVC-based storage (`storageMode: pvc`). This creates
a PersistentVolumeClaim per game server, enabling proper data lifecycle
management.

For single-node setups or testing, you can use HostPath:

```yaml
wings:
  kubernetes:
    storageMode: hostpath
```

### Networking

NodePort mode (default) creates a Kubernetes Service per game server, exposing
ports via cluster-assigned NodePorts:

```yaml
wings:
  kubernetes:
    networkMode: nodeport
    nodeportPreserve: true  # Try to use game port as NodePort
```

HostPort mode binds game server ports directly to the node:

```yaml
wings:
  kubernetes:
    networkMode: hostport
```

LoadBalancer mode provisions a `Service` of type `LoadBalancer` per game server
(for use with MetalLB, Cilium LB-IPAM, or a cloud LB). LB IP/sharing-key
annotations can be auto-populated from the allocation IP:

```yaml
wings:
  kubernetes:
    networkMode: loadbalancer
```

### Image pulling

Game server Pods and installation Jobs default to `imagePullPolicy: Always`
for remote images, so updated tags are re-pulled rather than reusing a stale
copy cached on the node (matching the Docker backend). `~`-prefixed local
images are never pulled. Override this for air-gapped clusters:

```yaml
wings:
  kubernetes:
    imagePullPolicy: IfNotPresent  # or "Never"
```

This is independent of `image.pullPolicy`, which applies to the Wings daemon
image itself.

## What Gets Created

- **Namespace** — `pelican` (configurable)
- **ServiceAccount** — For Wings and game server Pods
- **Role + RoleBinding** — Namespace-scoped permissions (Pods, Services, Jobs, PVCs)
- **ClusterRole + ClusterRoleBinding** — Metrics API access (`nodes/proxy` only when `rbac.kubeletMetricsFallback=true`)
- **Secret** — Wings configuration file (contains Panel token)
- **Deployment** — Wings daemon with health probes
- **Service** — Exposes Wings API within the cluster

## Uninstalling

```bash
helm uninstall wings
```

Note: PVCs created for game servers are NOT automatically deleted when
uninstalling the chart. Delete them manually if you want to remove all data:

```bash
kubectl delete pvc -n pelican -l app.kubernetes.io/managed-by=pelican-wings
```
