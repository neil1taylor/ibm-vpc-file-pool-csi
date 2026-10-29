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
| `image.tag` | string | `""` | Image tag. Empty means `v` plus the chart `appVersion` (never `latest`). Pin to a specific version or digest in production |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy (`Always`, `IfNotPresent`, `Never`) |
| `imagePullSecrets` | list | `[]` | List of image pull secret names for private registries |

## Config

VPC infrastructure configuration. On managed ROKS/ROVS/IKS clusters, all four values are auto-discovered. Only set these for self-managed clusters or to override auto-discovery.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `config.region` | string | `""` | IBM Cloud region (e.g., `us-south`). Auto-discovered from secret provider endpoint |
| `config.vpcID` | string | `""` | VPC ID. Auto-discovered from `ibm-cloud-provider-data` ConfigMap |
| `config.subnetID` | string | `""` | Worker subnet IDs for mount targets, comma-separated. Each share's mount target goes in the subnet in the pool's zone. Auto-discovered from `ibm-cloud-provider-data` ConfigMap |
| `config.resourceGroupID` | string | `""` | Resource group ID for VPC file share creation |

## Controller

Settings for the CSI controller Deployment (runs the Pool Manager and CSI controller server).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `controller.replicas` | int | `1` | Number of controller replicas. Only 1 is active (leader election) |
| `controller.healthPort` | int | `8081` | Port for `/healthz` and `/readyz` endpoints |
| `controller.podSecurityContext` | object | `{seccompProfile: {type: RuntimeDefault}}` | Pod security context of the controller. Off OpenShift the chart adds `fsGroup: 65532` unless you set one, so the non-root user can read the webhook key |
| `controller.securityContext` | object | no escalation, `runAsNonRoot`, drop ALL | Container security context of `csi-controller`. The image runs as user 65532 |
| `controller.sidecarSecurityContext` | object | no escalation, drop ALL | Container security context of the controller sidecars (provisioner, resizer, snapshotter, liveness probe, secret provider). Their user is not changed |
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
| `node.kubeletDir` | string | `/var/data/kubelet` | Kubelet root directory. ROKS/ROVS/IKS uses `/var/data/kubelet` (the real path). Standard Kubernetes uses `/var/lib/kubelet` |
| `node.metricsPort` | int | `9810` | Port of the node agent's HTTPS metrics endpoint (per-share used bytes, NFS latency and IOPS). Its certificate follows `webhook.certProvider` (`manual` needs the secret `<release>-ibm-vpc-file-pool-csi-node-metrics-certs`). The node agent uses the host network, so the port must be free on the node. `0` turns the endpoint, its Service and its ServiceMonitor off |
| `node.shareStatsInterval` | string | `30s` | How often the node agent reads the NFS statistics of its shares |
| `node.registrarHealthPort` | int | `9809` | Port for the node-driver-registrar health endpoint. Must not conflict with other hostNetwork services. Stock IBM VPC file CSI uses 9808 |
| `node.securityContext` | object | `{privileged: true, runAsUser: 0}` | Container security context of `csi-node`. It must stay privileged and root to mount NFS on the host |
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
| `secretProvider.managed` | bool | `true` | Use the storage-secret-sidecar for automatic API key injection (managed ROKS/ROVS/IKS). Set to `false` for self-managed clusters |
| `secretProvider.sidecar.image` | string | `icr.io/obs/armada-storage-secret:v1.2.75` | Container image for the storage-secret-sidecar |

## Clone Worker

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `cloneWorker.interval` | string | `10s` | Background clone worker poll interval (Go duration) |

## Golden Image Syncer

> **Hidden feature.** The code is kept for possible later use. The recommended way to make VMs is the standard OpenShift Virtualization catalog.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `cdi.copyPodMemoryLimit` | string | `8Gi` | The controller keeps the memory limit of CDI's copy pods at least this high, through the OpenShift Virtualization `HyperConverged` setting `spec.resourceRequirements.storageWorkloads`. CDI's default of 600M is too small for copies onto NFS: the pod is killed. On rovslab, a 25 GB copy failed with 2Gi and finished with 8Gi. Empty turns the check off |
| `goldenImageSyncer.interval` | string | `5m` | Background golden image syncer poll interval (Go duration). Controls how often the syncer checks for new CDI DataImportCrons and updates golden images |

## Backup Browse

Browse a backup and restore single files (issue #92). Added in phase 1 of `docs/specs/2026-10-22-backup-file-restore.md`.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `backupBrowse.enabled` | bool | `true` | Turns browse sessions on. When off, the controller gets no `--browse-image` and the chart renders no browse Role or NetworkPolicy |
| `backupBrowse.namespace` | string | `ibm-vpc-file-pool-csi-browse` | The namespace of browse sessions: their helper pods and one Secret for each. The chart makes it. It holds nothing else, so the gateway's right to read Secrets there reaches nothing else. It is labelled for the Pod Security level `baseline`, because the helper runs as root with every capability dropped (it reads backups made by other users); `restricted` would refuse it. On OpenShift the chart adds one SecurityContextConstraints that allows exactly that for the namespace's default account |
| `backupBrowse.image.repository` | string | `de.icr.io/ibm-vpc-file-pool-csi/browse` | The browse helper image (`Dockerfile.browse`) |
| `backupBrowse.image.tag` | string | `""` | Empty means the tag of the driver image |
| `backupBrowse.idle` | string | `15m` | A session ends after this long without use |
| `backupBrowse.maxLife` | string | `2h` | A session ends after this long, even if in use |
| `backupBrowse.maxSessions` | int | `5` | Open sessions in the cluster. More wait |
| `backupBrowse.maxSessionsPerNamespace` | int | `2` | Open sessions for the claims of one namespace. More wait |
| `backupBrowse.restoreMaxBytes` | int | `214748364800` | The most one file restore (`BackupFileRestore`) writes, 200 GiB. A restore of more is refused before it copies. It is a controller setting and works even when `backupBrowse.enabled` is `false` |
| `backupBrowse.restoreMaxRunning` | int | `5` | File restores that run at once in the cluster. More wait, oldest first. Each running restore holds a Job and two share mounts. Works even when `backupBrowse.enabled` is `false` |
| `backupBrowse.cos.enabled` | bool | `true` | Browse and restore files from backups that a policy exported to COS (`spec.export`). The helper pods that read COS run in the release namespace, next to the COS and Key Protect Secrets (a pod mounts only Secrets of its own namespace), so those keys are never copied to the browse namespace where the gateway may read Secrets. The chart adds a Role there (the controller may create and delete Secrets, for the copy of each session's token and certificate) and a NetworkPolicy that lets only the gateway reach those pods. Off: no such pods, no Role, no NetworkPolicy, and a session or restore from COS fails with "not turned on" |
| `backupBrowse.cos.cpuLimit` | string | `""` | CPU limit of the helper pod that reads COS. Empty means `1` |
| `backupBrowse.cos.memoryLimit` | string | `""` | Memory limit of the helper pod that reads COS. Empty means `1Gi` (Kopia and its cache need more than a helper that reads a folder) |
| `backupBrowse.cos.startTimeout` | string | `5m` | A session on COS ends if its helper is not ready after this long, usually because a Secret named by the export block is missing or lacks a key; the message names the Secrets to check |

The chart also makes, in the browse namespace, a Role that lets the controller make, change and delete Secrets there (the session Secrets, and copies of the image pull secrets, which the controller copies from the release namespace), and a NetworkPolicy that lets only the backup gateway pods of the release namespace reach the helper pods, on port 8443. **Moving from the earlier part:** sessions that were open in the release namespace are not followed; they end by themselves within 2 hours and their objects are removed with them.

## Backup Gateway

The one door to browse sessions (issue #92). Users talk to it with their own Kubernetes token (`kubectl backup ls/get/browse`). It needs `backupBrowse.enabled`.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `backupGateway.enabled` | bool | `true` | Turns the gateway on |
| `backupGateway.replicas` | int | `1` | The gateway keeps no state |
| `backupGateway.url` | string | `""` | The address that goes in the ConfigMap `backup-gateway-info`, which `kubectl backup` reads. Empty uses `https://<route.host>`, the host of the existing Route, or on a first install on OpenShift the host the router will give the Route (`<release>-gateway-<namespace>.<ingress domain>`); if none is known, users pass `--gateway` |
| `backupGateway.tls.secretName` | string | `""` | Empty: on OpenShift the service CA makes the serving certificate. Elsewhere you must name a Secret (`tls.crt`, `tls.key`) in the release namespace, or the gateway pod cannot start |
| `backupGateway.route.enabled` | bool | `true` | A reencrypt Route, made only where the OpenShift Route API exists. The router answers with the cluster's ingress certificate for the host, then encrypts again to the gateway (the service CA certificate, which the router trusts). Traffic is encrypted on both legs |
| `backupGateway.route.host` | string | `""` | Empty lets OpenShift choose the host |
| `backupGateway.sessionWait` | string | `20s` | How long a request that asks to wait (`?wait=true`) holds for the helper pod to be ready. Keep it under your proxy's timeout (the OpenShift router cuts a request after 30 seconds). `kubectl backup` does not ask to wait: it polls |
| `backupGateway.maxDownloadBytes` | int | `53687091200` | The largest single file or tar a user may download (50 GiB) |
| `backupGateway.maxOpenRestores` | int | `10` | File restores one user may have that have not ended. A request over the limit is refused (HTTP 429) |
| `backupGateway.resources` | object | small | Requests and limits |

When the gateway runs on OpenShift, the chart also adds a `backup-gateway` entry to the ConsolePlugin proxy (user token, the gateway Service on port 8443) and sets `fileBrowse: "true"` in the settings ConfigMap, so the Backups pages show **Browse files**. There is no value for either.

The chart also makes the ClusterRoles `backup-file-reader` and `backup-file-restorer` (see BACKUP-RECOVERY.md), a ServiceAccount for the gateway with only the rights it needs, and the ConfigMap `backup-gateway-info` that every signed-in user may read (that one ConfigMap and no other).

## PoolBench

The storage performance test service (`docs/specs/2026-10-25-poolbench.md`). Off by default. When on, the chart renders one Deployment (`--mode=poolbench`, the driver image), its own service account, a cluster role, a run role, the collector Service, and the results volume.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `poolBench.enabled` | bool | `false` | Turns the PoolBench service on |
| `poolBench.interval` | string | `15s` | Time between two passes of the runner |
| `poolBench.results.size` | string | `5Gi` | Size of the volume that keeps the test VMs' raw results until a run is deleted |
| `poolBench.results.storageClass` | string | `""` | Class of the results volume; empty uses the cluster's default class |
| `poolBench.podSecurityContext` | object | seccomp `RuntimeDefault` | Pod security context. Where the user ID is fixed (off OpenShift, or in `kube-system`) the chart adds `fsGroup: 65532` unless you set one, so the service can write the results volume |
| `poolBench.securityContext` | object | non-root, read-only root, no privilege escalation, all capabilities dropped | Container security context |
| `poolBench.resources` | object | 50m CPU, 64Mi memory requested, 256Mi limit | Container resources |

Rights: cluster-wide the service may make and delete namespaces, pools and backup policies (the backup event), list nodes and pods (for the room check), read images, read and update `PoolBench` objects, and bind one role, `<release>-poolbench-run`. It binds that role to itself in each run's own namespace; the role gives it secrets, claims (it also labels them), VMs, disks, pods and pod exec there and nowhere else. RBAC cannot limit namespace deletes or role bindings by name, so where the cluster has `ValidatingAdmissionPolicy` (Kubernetes 1.30 and later, OpenShift 4.17 and later) the chart adds a policy that lets the service make and delete only namespaces, pools and backup policies named `poolbench-*`, delete only a namespace, pool or policy that carries the run label, make only a backup policy for one `poolbench-*` namespace that copies to that run's backup pool and exports nowhere, and bind only the run role, only in such a namespace. It reads the node agents' metrics for the share's counters, trusting their certificate as the ServiceMonitors do (the service CA, or the `ca.crt` of `<release>-node-metrics-certs`). Its own metrics (port 8443) are scraped like the controller's; their certificate is the secret `<release>-poolbench-certs`, made by the OpenShift service CA or cert-manager, or by you with `webhook.certProvider=manual`. Without that secret the pod does not start. When the console plugin is on, the chart also adds a proxy `poolbench` to the ConsolePlugin: the console passes the person's own token to port 8444 of the service, which checks with a TokenReview and an access review that the person may get the run before it returns its raw results. The console trusts the service's certificate only when it comes from the OpenShift service CA (`webhook.certProvider=openshift`).

## Replication Receiver

Configuration for the driver-to-driver cross-region replication receiver. Deploy this on the **destination** cluster. Source cluster sync-client Jobs upload data via HTTPS.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `replicationReceiver.enabled` | bool | `false` | Enable the replication receiver Deployment, Service, and Route |
| `replicationReceiver.pvcName` | string | `""` | Pre-created PVC name for the destination NFS mount. It is also the claim a `Failover` reads when it names none |
| `replicationReceiver.authSecretName` | string | `""` | Secret name containing a `token` key for bearer token authentication |
| `replicationReceiver.runAsUser` | int | `107` | User the receiver runs as. It writes destination VM disks, and KubeVirt runs QEMU as 107 |
| `replicationReceiver.runAsGroup` | int | `107` | Group the receiver runs as |
| `replicationReceiver.securityContext` | object | no escalation, `runAsNonRoot`, drop ALL, seccomp `RuntimeDefault` | Container security context of the receiver |
| `replicationReceiver.resources.requests.cpu` | string | `100m` | Receiver CPU request |
| `replicationReceiver.resources.requests.memory` | string | `256Mi` | Receiver memory request |
| `replicationReceiver.resources.limits.memory` | string | `1Gi` | Receiver memory limit. The receiver needs about 100 MiB itself. Pages of files on NFS count against the limit and the kernel drops them as it needs room, so the memory shown for a busy receiver stays near the limit; that is normal. The receiver forces its data out every 32 MiB and drops cached pages. A 512Mi limit survived an 11 GiB VM disk send in a lab test; the default adds room |
| `replicationReceiver.tls.enabled` | bool | `false` | Enable native TLS on the receiver (certificates provisioned by cert-manager) |
| `replicationReceiver.tls.additionalDNSNames` | list | `[]` | Extra DNS SANs for the receiver TLS certificate (e.g., custom ingress hostnames). On OpenShift, the Route hostname is added automatically |
| `replicationReceiver.tls.certManager.issuerKind` | string | `Issuer` | cert-manager issuer kind: `Issuer` (namespace-scoped) or `ClusterIssuer` |
| `replicationReceiver.tls.certManager.issuerName` | string | `""` | cert-manager issuer name. Leave empty to create a self-signed Issuer automatically |

## Replication

Controller-side replication Job configuration.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `replication.image` | string | `""` | Override container image for replication Jobs. When empty, the driver image is used for receiver-mode (sync-client) Jobs; CentOS Stream 9 is used for direct-NFS (rsync) Jobs |

## VM Snapshots

See [VM-INSTANT-SNAPSHOTS.md](VM-INSTANT-SNAPSHOTS.md).

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `vmSnapshots.method` | string | `auto` | `auto`: a snapshot of a KubeVirt VM disk is instant (a new overlay file, nothing copied) where the KubeVirt `Sidecar` feature gate is on, and a copy elsewhere. It adds the VM hook webhook. `copy`: every VM disk snapshot is a copy, and there is no VM hook webhook. Red Hat does not support the `Sidecar` feature gate |
| `vmSnapshots.enableSidecarGate` | bool | `false` | Turn on the KubeVirt `Sidecar` feature gate, which instant snapshots need. A hook Job adds one entry to the `HyperConverged` `kubevirt.kubevirt.io/jsonpatch` annotation after each install and upgrade, and keeps the other entries. Only that Job's service account gets rights on `HyperConverged`: get and update on the object named `kubevirt-hyperconverged`, nothing else. The gate is cluster-wide: once it is on, anyone who can create or edit a VM can add a hook sidecar container to that VM's pod (see SECURITY.md). Setting it back to `false` or uninstalling does **not** turn the gate off, because VMs with instant snapshots cannot start without it. Needs `vmSnapshots.method=auto`. Red Hat does not support settings made through that annotation |
| `vmSnapshots.launcherImage` | string | `""` | KubeVirt's virt-launcher image, which has `qemu-img`, for the Jobs that change a stopped VM's snapshot files and that restore instant snapshots. Empty reads it from the virt-operator Deployment in the namespace of the `KubeVirt` resource |

The chart also renders a ConfigMap `ibm-vpc-file-pool-csi-settings` in the release namespace. It holds `vmSnapshotMethod`, the value of `vmSnapshots.method`, and no secret. A Role lets every signed-in user `get` that one ConfigMap by name, and nothing else in the namespace. The console reads it to show whether snapshots are instant or copies.

## Webhook

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `webhook.enabled` | bool | `true` | Enable validating admission webhooks for CRD resources |
| `webhook.port` | int | `9443` | Webhook server port |
| `webhook.certProvider` | string | `cert-manager` | Certificate provisioning method: `cert-manager` (auto-provisions via cert-manager), `openshift` (uses the built-in OpenShift service CA; no extra operator) or `manual` (you create the TLS secret and set `webhook.caBundle`) |
| `webhook.caBundle` | string | `""` | Base64-encoded PEM CA bundle for the webhook configuration. Required when `certProvider=manual` |
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
| `metrics.serviceMonitor.enabled` | bool | `true` | Create a Prometheus ServiceMonitor. The chart creates it only when the cluster has the `monitoring.coreos.com/v1` API. The console plugin's charts need it |
| `metrics.serviceMonitor.interval` | string | `30s` | Scrape interval for the ServiceMonitor |
| `metrics.monitoring.mode` | string | `auto` | Where scraping and alerts live on OpenShift. `auto`: in the monitoring namespace when the cluster has the Prometheus Operator API and the OpenShift console and `webhook.certProvider=openshift`; `enabled`: always (needs `certProvider=openshift`, else the render fails); `disabled`: in the release namespace. Needs user workload monitoring on: see [MONITORING.md](MONITORING.md#where-alerts-run-on-openshift) |
| `metrics.monitoring.namespace` | string | `ibm-vpc-file-pool-csi-monitoring` | Namespace for the `ServiceMonitor` objects, the `PrometheusRule` and the metrics Services. The chart creates it, without a platform monitoring label |
| `metrics.scraper.serviceAccount` | string | `prometheus-k8s` | Service account bound to the chart's `metrics-reader` ClusterRole (`get` on `/metrics`) so it can scrape the node agents. Empty creates no binding |
| `metrics.scraper.namespace` | string | `openshift-monitoring` | Namespace of that service account |
| `metrics.alerts.shareUsedWarning` | int | `85` | Percent of one share's size written (real used bytes) that triggers a warning |
| `metrics.alerts.shareUsedCritical` | int | `95` | Percent of one share's size written that triggers a critical alert |
| `metrics.alerts.nfsLatencySeconds` | number | `0.1` | Average seconds per NFS read or write request on one share that triggers a warning |
| `metrics.alerts.nfsMinOpsPerSecond` | number | `1` | The latency alert is judged only while the share does at least this many read or write requests a second |
| `metrics.alerts.shareIOPSWarning` | int | `0` | Read plus write operations a second on one share (all nodes) that trigger a warning. `0` turns the alert off |
| `metrics.alerts.enabled` | bool | `true` | Create the PrometheusRule (only when the cluster has the Prometheus Operator API) |
| `metrics.alerts.allocationFailuresPerSecond` | number | `0.1` | Failed volume allocations per second that trigger a critical alert |
| `metrics.alerts.allocationP99Seconds` | number | `5` | P99 allocation time in seconds that triggers a warning |
| `metrics.alerts.apiErrorsPerSecond` | number | `0.05` | Failed VPC API calls per second that trigger a warning |
| `metrics.alerts.replicationLagSeconds` | int | `7200` | Replication lag in seconds that triggers a warning |
| `metrics.alerts.replicationFailures` | int | `3` | Replication failures in a row that trigger a critical alert |
| `metrics.alerts.backupLateSeconds` | int | `43200` | A volume with no backup for this long triggers a warning (use twice the schedule) |
| `metrics.alerts.exportLateSeconds` | int | `43200` | An export to COS, or a moment waiting, this old triggers a warning |
| `metrics.alerts.snapshotPoolWarning` | int | `75` | Snapshot pool share use (percent) that triggers a warning |
| `metrics.alerts.snapshotPoolCritical` | int | `85` | Snapshot pool share use (percent) that triggers a critical alert; new backups stop at 85 |
| `metrics.alerts.shareUtilizationWarning` | int | `90` | Percent of one share's size allocated to PVCs that triggers a warning |
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

OpenShift Console dynamic plugin for visual pool management. On by default, and installed only on clusters with the OpenShift console (ConsolePlugin v1 API, OpenShift 4.14+). On other clusters the chart leaves it out.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `consolePlugin.enabled` | bool | `true` | Install the OpenShift Console dynamic plugin and turn it on in the console. Set `false` to leave it out |
| `consolePlugin.image.repository` | string | `de.icr.io/ibm-vpc-file-pool-csi/console-plugin` | Plugin container image repository |
| `consolePlugin.image.tag` | string | `""` | Plugin image tag. Empty means `v` plus the chart `appVersion` |
| `consolePlugin.securityContext` | object | no escalation, `runAsNonRoot`, drop ALL, seccomp `RuntimeDefault` | Container security context of the plugin (its image already runs as user 1001) |
| `consolePlugin.replicas` | int | `2` | Number of plugin pod replicas |
| `consolePlugin.port` | int | `9443` | HTTPS port for the plugin Service. TLS certs are auto-provisioned by the OpenShift service-ca operator |
| `consolePlugin.resources.requests.cpu` | string | `10m` | CPU request |
| `consolePlugin.resources.requests.memory` | string | `50Mi` | Memory request |
| `consolePlugin.resources.limits.memory` | string | `128Mi` | Memory limit |

The chart turns the plugin on in the console for you. A hook Job runs after each install and upgrade and adds `ibm-vpc-file-pool-csi` to the console's plugin list (`console.operator.openshift.io/cluster`, `spec.plugins`). It keeps every other plugin. Before `helm uninstall`, a second hook Job removes only this plugin from the list. If you upgrade with `consolePlugin.enabled=false`, the same Job removes it after the upgrade. The Jobs use the driver image and a service account that can only read and update that one object.

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

Pin the OpenShift Console plugin image (the plugin is on by default):

```yaml
consolePlugin:
  image:
    repository: de.icr.io/ibm-vpc-file-pool-csi/console-plugin
    tag: v0.12.0
  replicas: 2
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
