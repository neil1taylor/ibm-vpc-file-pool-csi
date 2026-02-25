# Helm Values Reference

Complete reference for all configurable parameters in the `ibm-vpc-file-pool-csi` Helm chart.

## General

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `nameOverride` | string | `""` | Override the chart name used in resource names |
| `fullnameOverride` | string | `""` | Override the full resource name prefix |

## Image

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `image.repository` | string | `de.icr.io/ibm-vpc-file-pool-csi/driver` | Container image repository for the CSI driver |
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
| `controller.healthPort` | int | `8081` | Port for `/healthz` and `/readyz` endpoints |
| `controller.resources.requests.cpu` | string | `50m` | Controller CPU request |
| `controller.resources.requests.memory` | string | `128Mi` | Controller memory request |
| `controller.resources.limits.memory` | string | `512Mi` | Controller memory limit |
| `controller.tolerations` | list | `[]` | Tolerations for controller pod scheduling |
| `controller.nodeSelector` | object | `{}` | Node selector for controller pod placement |
| `controller.affinity` | object | `{}` | Affinity rules for controller pod scheduling |

## Node

Settings for the CSI node agent DaemonSet (runs on every worker node).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `node.kubeletDir` | string | `/var/data/kubelet` | Kubelet root directory. ROKS/IKS uses `/var/data/kubelet` (the real path). Standard Kubernetes uses `/var/lib/kubelet` |
| `node.registrarHealthPort` | int | `9809` | Port for the node-driver-registrar health endpoint. Must not conflict with other hostNetwork services. Stock IBM VPC file CSI uses 9808 |
| `node.resources.requests.cpu` | string | `50m` | Node agent CPU request |
| `node.resources.requests.memory` | string | `64Mi` | Node agent memory request |
| `node.resources.limits.memory` | string | `256Mi` | Node agent memory limit |
| `node.tolerations` | list | `[{operator: Exists}]` | Tolerations for node agent pods. Default tolerates all taints so the agent runs on every node |
| `node.nodeSelector` | object | `{}` | Node selector for node agent pod placement |
| `node.affinity` | object | `{}` | Affinity rules for node agent pod scheduling |

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

## Clone Worker

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `cloneWorker.interval` | string | `10s` | Background clone worker poll interval (Go duration) |

## Golden Image Syncer

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `goldenImageSyncer.interval` | string | `5m` | Background golden image syncer poll interval (Go duration). Controls how often the syncer checks for new CDI DataImportCrons and updates golden images |

## Replication Receiver

Configuration for the driver-to-driver cross-region replication receiver. Deploy this on the **destination** cluster. Source cluster sync-client Jobs upload data via HTTPS.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `replicationReceiver.enabled` | bool | `false` | Enable the replication receiver Deployment, Service, and Route |
| `replicationReceiver.pvcName` | string | `""` | Pre-created PVC name for the destination NFS mount |
| `replicationReceiver.authSecretName` | string | `""` | Secret name containing a `token` key for bearer token authentication |
| `replicationReceiver.resources.requests.cpu` | string | `100m` | Receiver CPU request |
| `replicationReceiver.resources.requests.memory` | string | `256Mi` | Receiver memory request |
| `replicationReceiver.resources.limits.memory` | string | `512Mi` | Receiver memory limit |

## Replication

Controller-side replication Job configuration.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `replication.image` | string | `""` | Override container image for replication Jobs. When empty, the driver image is used for receiver-mode (sync-client) Jobs; CentOS Stream 9 is used for direct-NFS (rsync) Jobs |

## Webhook

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `webhook.enabled` | bool | `true` | Enable validating admission webhooks for CRD resources |
| `webhook.port` | int | `9443` | Webhook server port |
| `webhook.certProvider` | string | `cert-manager` | Certificate provisioning method: `cert-manager` (auto-provisions via cert-manager) or `manual` (you create the TLS secret) |
| `webhook.certManager.issuerKind` | string | `Issuer` | cert-manager issuer kind. Only used when `certProvider=cert-manager` |
| `webhook.certManager.issuerName` | string | `""` | cert-manager issuer name. Leave empty to create a self-signed Issuer automatically |

## Volume Snapshot Class

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `volumeSnapshotClass.create` | bool | `true` | Create a VolumeSnapshotClass resource |
| `volumeSnapshotClass.name` | string | `ibm-vpc-file-pool` | VolumeSnapshotClass name |
| `volumeSnapshotClass.deletionPolicy` | string | `Delete` | Snapshot deletion policy (`Delete` or `Retain`) |

## Sidecars

Standard CSI sidecar container images and resources. Update images when upgrading Kubernetes CSI components. All sidecars have default resource requests/limits; override per-sidecar via `sidecars.<name>.resources`.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `sidecars.provisioner.image` | string | `registry.k8s.io/sig-storage/csi-provisioner:v5.1.0` | CSI external-provisioner image |
| `sidecars.provisioner.resources` | object | `{requests: {cpu: 10m, memory: 32Mi}, limits: {memory: 128Mi}}` | Provisioner resource requests/limits |
| `sidecars.resizer.image` | string | `registry.k8s.io/sig-storage/csi-resizer:v1.12.0` | CSI external-resizer image |
| `sidecars.resizer.resources` | object | `{requests: {cpu: 10m, memory: 32Mi}, limits: {memory: 128Mi}}` | Resizer resource requests/limits |
| `sidecars.snapshotter.image` | string | `registry.k8s.io/sig-storage/csi-snapshotter:v8.2.0` | CSI external-snapshotter image |
| `sidecars.snapshotter.resources` | object | `{requests: {cpu: 10m, memory: 32Mi}, limits: {memory: 128Mi}}` | Snapshotter resource requests/limits |
| `sidecars.registrar.image` | string | `registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.12.0` | CSI node-driver-registrar image |
| `sidecars.registrar.resources` | object | `{requests: {cpu: 10m, memory: 16Mi}, limits: {memory: 64Mi}}` | Registrar resource requests/limits |
| `sidecars.liveness.image` | string | `registry.k8s.io/sig-storage/livenessprobe:v2.14.0` | CSI liveness probe image |
| `sidecars.liveness.resources` | object | `{requests: {cpu: 10m, memory: 16Mi}, limits: {memory: 64Mi}}` | Liveness probe resource requests/limits |

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

## Console Plugin

OpenShift Console dynamic plugin for visual pool management. Requires OpenShift 4.14+ with the ConsolePlugin v1 API. Disabled by default.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `consolePlugin.enabled` | bool | `false` | Enable the OpenShift Console dynamic plugin |
| `consolePlugin.image.repository` | string | `de.icr.io/ibm-vpc-file-pool-csi/console-plugin` | Plugin container image repository |
| `consolePlugin.image.tag` | string | `latest` | Plugin image tag |
| `consolePlugin.replicas` | int | `2` | Number of plugin pod replicas |
| `consolePlugin.port` | int | `9443` | HTTPS port for the plugin Service. TLS certs are auto-provisioned by the OpenShift service-ca operator |
| `consolePlugin.resources.requests.cpu` | string | `10m` | CPU request |
| `consolePlugin.resources.requests.memory` | string | `50Mi` | Memory request |
| `consolePlugin.resources.limits.memory` | string | `128Mi` | Memory limit |

After installing with `consolePlugin.enabled=true`, register the plugin with the OpenShift console:

```bash
oc patch console.operator.openshift.io cluster \
  --patch '{"spec":{"plugins":["ibm-vpc-file-pool-csi"]}}' \
  --type=merge
```

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

### Console Plugin

Enable the OpenShift Console dynamic plugin:

```yaml
consolePlugin:
  enabled: true
  image:
    repository: de.icr.io/ibm-vpc-file-pool-csi/console-plugin
    tag: v0.12.0
  replicas: 2
```

After install, register the plugin:

```bash
oc patch console.operator.openshift.io cluster \
  --patch '{"spec":{"plugins":["ibm-vpc-file-pool-csi"]}}' \
  --type=merge
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
