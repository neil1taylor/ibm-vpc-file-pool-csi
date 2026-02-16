# Helm Values Reference

Complete reference for all configurable parameters in the `ibm-vpc-file-pool-csi` Helm chart.

## Image

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `image.repository` | string | `icr.io/ibm-vpc-file-pool-csi/driver` | Container image repository for the CSI driver |
| `image.tag` | string | `latest` | Image tag. Pin to a specific version or digest in production |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy (`Always`, `IfNotPresent`, `Never`) |
| `imagePullSecrets` | list | `[]` | List of image pull secret names for private registries |

## Config

VPC infrastructure configuration. On managed ROKS/IKS clusters, all four values are auto-discovered. Only set these for self-managed clusters or to override auto-discovery.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `config.region` | string | `""` | IBM Cloud region (e.g., `us-south`). Auto-discovered from secret provider endpoint |
| `config.vpcID` | string | `""` | VPC ID. Auto-discovered from `ibm-cloud-provider-data` ConfigMap |
| `config.subnetID` | string | `""` | Subnet ID for mount targets. Auto-discovered from `ibm-cloud-provider-data` ConfigMap |
| `config.resourceGroupID` | string | `""` | Resource group ID for VPC file share creation |

## Controller

Settings for the CSI controller Deployment (runs the Pool Manager and CSI controller server).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.replicas` | int | `1` | Number of controller replicas. Only 1 is active (leader election) |
| `controller.resources` | object | `{}` | CPU/memory resource requests and limits |
| `controller.tolerations` | list | `[]` | Tolerations for controller pod scheduling |
| `controller.nodeSelector` | object | `{}` | Node selector for controller pod placement |
| `controller.affinity` | object | `{}` | Affinity rules for controller pod scheduling |

## Node

Settings for the CSI node agent DaemonSet (runs on every worker node).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `node.tolerations` | list | `[{operator: Exists}]` | Tolerations for node agent pods. Default tolerates all taints so the agent runs on every node |

## Logging

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `logLevel` | int | `4` | klog verbosity level. `2` = normal ops, `4` = detailed, `6` = trace |

## Secret Provider

Controls how the IBM Cloud API key is injected into the controller.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `secretProvider.managed` | bool | `true` | Use the storage-secret-sidecar for automatic API key injection (managed ROKS/IKS). Set to `false` for self-managed clusters |
| `secretProvider.sidecar.image` | string | `icr.io/obs/armada-storage-secret:v1.2.75` | Container image for the storage-secret-sidecar |

## Sidecars

Standard CSI sidecar container images. Update these when upgrading Kubernetes CSI components.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `sidecars.provisioner.image` | string | `registry.k8s.io/sig-storage/csi-provisioner:v5.1.0` | CSI external-provisioner image |
| `sidecars.resizer.image` | string | `registry.k8s.io/sig-storage/csi-resizer:v1.12.0` | CSI external-resizer image |
| `sidecars.registrar.image` | string | `registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.12.0` | CSI node-driver-registrar image |
| `sidecars.liveness.image` | string | `registry.k8s.io/sig-storage/livenessprobe:v2.14.0` | CSI liveness probe image |

## Metrics

Prometheus metrics and alerting configuration.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `metrics.port` | int | `8080` | Port for the `/metrics` endpoint |
| `metrics.serviceMonitor.enabled` | bool | `false` | Create a Prometheus ServiceMonitor resource |
| `metrics.serviceMonitor.interval` | string | `30s` | Scrape interval for the ServiceMonitor |
| `metrics.alerts.enabled` | bool | `false` | Create PrometheusRule alert resources |
| `metrics.alerts.utilizationWarning` | int | `80` | Pool utilization percentage that triggers a warning alert |
| `metrics.alerts.utilizationCritical` | int | `95` | Pool utilization percentage that triggers a critical alert |

## StorageClass

Configuration for the default StorageClass created by the chart.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `storageClass.create` | bool | `true` | Create a StorageClass resource |
| `storageClass.name` | string | `ibm-vpc-file-pool` | StorageClass name |
| `storageClass.pool` | string | `general-purpose` | Name of the FileSharePool CR to allocate from |
| `storageClass.tier` | string | `""` | Tier name within the pool. Empty uses the default (no tier). Set when the pool defines tiers |
| `storageClass.reclaimPolicy` | string | `Delete` | Reclaim policy (`Delete` or `Retain`) |
| `storageClass.volumeBindingMode` | string | `Immediate` | Volume binding mode (`Immediate` or `WaitForFirstConsumer`) |
| `storageClass.allowVolumeExpansion` | bool | `true` | Allow PVC resize requests |
| `storageClass.mountOptions` | list | `[nfsvers=4.1, soft, timeo=600, retrans=3]` | NFS mount options. Do not change `soft` to `hard` — see [Known Limitations](KNOWN-LIMITATIONS.md) |

## Example Overrides

### Air-Gapped Registry

Mirror all images to an internal registry:

```yaml
image:
  repository: registry.internal.example.com/ibm-vpc-file-pool-csi/driver
  tag: v0.2.0

imagePullSecrets:
  - name: internal-registry-creds

secretProvider:
  sidecar:
    image: registry.internal.example.com/obs/armada-storage-secret:v1.2.75

sidecars:
  provisioner:
    image: registry.internal.example.com/sig-storage/csi-provisioner:v5.1.0
  resizer:
    image: registry.internal.example.com/sig-storage/csi-resizer:v1.12.0
  registrar:
    image: registry.internal.example.com/sig-storage/csi-node-driver-registrar:v2.12.0
  liveness:
    image: registry.internal.example.com/sig-storage/livenessprobe:v2.14.0
```

### Self-Managed Cluster

Disable the managed secret sidecar and provide VPC config explicitly:

```yaml
config:
  region: us-south
  vpcID: r006-abc12345-6789-def0-1234-567890abcdef
  subnetID: 0717-abcdef01-2345-6789-abcd-ef0123456789
  resourceGroupID: a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6

secretProvider:
  managed: false
```

Then create a Secret with the IBM Cloud API key:

```bash
kubectl create secret generic ibm-cloud-credentials \
  --namespace kube-system \
  --from-literal=api-key=<your-api-key>
```

### Custom Metrics

Enable Prometheus monitoring with tighter alert thresholds:

```yaml
metrics:
  port: 9090
  serviceMonitor:
    enabled: true
    interval: 15s
  alerts:
    enabled: true
    utilizationWarning: 70
    utilizationCritical: 90
```
