# IBM VPC File Pool CSI Driver

A Kubernetes [CSI](https://kubernetes-csi.github.io/docs/) driver that pools multiple PVCs as subdirectories within shared IBM Cloud VPC file shares, instead of the traditional 1:1 PVC-to-share mapping.

## Why Pooling?

The standard IBM VPC file CSI driver creates one VPC file share per PVC — a 1:1 mapping. That hits three walls fast:

- **Quota** — VPC accounts cap at 300 file shares. A busy cluster with lots of small PVCs burns through that quickly.
- **Speed** — Creating a VPC file share takes 30-90 seconds. Pods sit waiting for storage.
- **Cost** — Each share has a 10 GB minimum and its own billing line. Hundreds of tiny PVCs mean hundreds of minimums.

Pooling flips the model: pre-provision a few large shares, then hand out subdirectories within them. Creating a subdirectory is instant, shares nothing with the VPC API, and lets many PVCs share the capacity of a single share. Think of it like a VMware datastore — one NFS export holds many volumes.

## How It Works

Traditional CSI drivers create one VPC file share per PVC. This driver takes a different approach: a **FileSharePool** CR defines a pool of VPC file shares, and each PVC gets a subdirectory on an existing share.

```
VPC File Share (1 TB)
├── /pvcs/pvc-aaa...    ← PVC-1 (10 GB quota)
├── /pvcs/pvc-bbb...    ← PVC-2 (50 GB quota)
└── /pvcs/pvc-ccc...    ← PVC-3 (20 GB quota)
```

This reduces VPC API calls, speeds up PVC provisioning (mkdir vs. 30-90s share creation), and lowers cost by consolidating storage.

## Architecture

```
Workload Pod
    │ bind-mount /data
    ▼
CSI Node Agent (DaemonSet)
    │ NFS mount per share, bind-mount per PVC
    │
CSI Controller (Deployment)
    │ CreateVolume / DeleteVolume
    ▼
Pool Manager
    │ share selection (spread/binpack), mkdir, capacity tracking
    ▼
IBM VPC Client ──► VPC File Share API
```

**Key components:**

| Component | Location | Role |
|-----------|----------|------|
| CSI Controller | `pkg/driver/controller.go` | gRPC handlers for volume lifecycle |
| CSI Node Agent | `pkg/driver/node.go` | NFS mount + bind-mount into pods |
| Pool Manager | `pkg/pool/manager.go` | Core brain: allocation, deallocation, expansion, snapshots, clones |
| Share Selection | `pkg/pool/share.go` | Spread (most free) or binpack (least free) strategy |
| Clone Worker | `pkg/pool/clone_worker.go` | Async clone operations for large volumes |
| Replication Controller | `pkg/pool/replication_controller.go` | Cross-region disaster recovery with incremental rsync |
| Hook Orchestrator | `pkg/hooks/` | Lifecycle hooks (exec + HTTP) for replication and group snapshots |
| Admission Webhooks | `pkg/webhook/` | Validating webhooks for all CRD types |
| Migration CLI | `pkg/migrate/` | Migrate PVCs from stock IBM CSI driver |
| IBM VPC Client | `pkg/ibmcloud/client.go` | Thin wrapper around VPC file share API |
| CRD Types | `api/v1alpha1/` | FileSharePool, SubVolume, Snapshot, VolumeGroupSnapshot, ReplicationPolicy, Hook |

## CRDs

- **FileSharePool** (cluster-scoped) — defines a pool of VPC file shares with allocation strategy, auto-expansion, and capacity limits
- **SubVolume** (cluster-scoped) — tracks individual PVC allocations: which share, subdirectory path, requested size, and clone/snapshot state
- **Snapshot** (cluster-scoped) — tracks point-in-time directory copies of SubVolumes
- **VolumeGroupSnapshot** (cluster-scoped) — coordinates multi-PVC consistent snapshots with pre/post-snapshot lifecycle hooks
- **ReplicationPolicy** (cluster-scoped) — defines cross-region replication relationships between pools with pre/post-sync lifecycle hooks and incremental rsync

## Glossary

### Core Concepts

| Term | Definition |
|------|-----------|
| **Pool** (FileSharePool) | A group of VPC file shares managed as a unit, defined by a FileSharePool CR |
| **Share** (VPC File Share) | An NFS-exported block of cloud storage created via the IBM VPC API; each pool contains one or more shares |
| **SubVolume** | A subdirectory on a share that backs a single PVC, tracked by a SubVolume CR |
| **Subdirectory** | The physical directory on the NFS share (e.g., `/pvcs/pvc-abc123`), created by the node agent during publish |
| **Mount Target** | The VPC-managed NFS endpoint (IP address) used to mount a share on worker nodes |
| **Allocation** | The process of selecting a share, recording a SubVolume CR, and creating a subdirectory for a new PVC |
| **Staging** | Per-node NFS mount of a share at a staging directory; done once per share per node |
| **Tier** | A named performance level within a pool (e.g., "standard", "premium"), each with its own VPC profile, share size, and StorageClass |
| **Spread** | Allocation strategy that places each new PVC on the share with the most free capacity, distributing PVCs evenly across shares |
| **Binpack** | Allocation strategy that places each new PVC on the share with the least free capacity that still fits, filling shares sequentially |
| **Draining** | Share state where no new PVCs are allocated but existing PVCs continue to work; used to decommission a share gracefully |
| **Expanding** | Pool state when a new VPC file share is being created because utilization crossed the `expandThresholdPercent` threshold |
| **Golden Image** | A pre-configured VM boot disk image (e.g., CentOS, Fedora) provisioned on pool storage for rapid VM creation via cloning |
| **Clone Worker** | Background goroutine that copies data for large (>10 GB) volume clones asynchronously, allowing `CreateVolume` to return immediately |
| **Hook Orchestrator** | Component that executes lifecycle hooks (exec commands or HTTP webhooks) before/after replication syncs and group snapshots |
| **Incremental Sync** | rsync-based replication that transfers only changed files between source and destination, instead of full directory copies |

### IBM Cloud VPC

| Term | Definition |
|------|-----------|
| **dp2** | The default VPC file share profile; IOPS scale automatically with share size (e.g., 2 TB = ~6,000 IOPS) |
| **Custom Profile** | A VPC file share profile where you specify a fixed IOPS value independent of the dp2 auto-scaling formula |
| **IOPS** | I/O Operations Per Second; the performance budget for a share, shared by all PVCs on that share |
| **VPC Access Mode** | File share access control mode where shares are exported under a shared FQDN and differentiated by their NFS export path |
| **Security Group Mode** | Alternative file share access control mode using VPC security groups; incompatible with VPC mount targets |
| **root_squash** | NFS server mapping of root (UID 0) to nobody (UID 65534); always enabled on VPC file shares, cannot be disabled |
| **initial_owner** | VPC API parameter setting the UID/GID ownership of the share's root directory at creation time |
| **Transit Gateway** | IBM Cloud service connecting VPCs across regions over the private backbone; required for cross-region replication |

### NFS

| Term | Definition |
|------|-----------|
| **NFSv4.1** | Network File System protocol version 4.1; adds session trunking and directory delegation over v4.0. Required by VPC file shares. |
| **sec=sys** | NFS security flavor using standard Unix UID/GID credentials (AUTH_UNIX); required for `chown` to work on VPC shares |
| **sec=null** | NFS security flavor using anonymous authentication; VPC shares negotiate this by default, making all files UID 99 |
| **soft mount** | NFS mount option that returns I/O errors after a timeout; prevents pods from hanging when the NFS server is unreachable |
| **hard mount** | NFS mount option that retries I/O indefinitely until the server responds; can cause pods to hang permanently |
| **timeo** | NFS mount option: timeout per RPC attempt in deciseconds (default 600 = 60 seconds) |
| **retrans** | NFS mount option: number of RPC retransmissions before a soft mount returns an error (default 3) |
| **rsize / wsize** | NFS mount options controlling read/write buffer sizes in bytes (typically negotiated to 1 MB) |
| **close-to-open** | NFS consistency model: changes to a file become visible to other clients only after the writer calls `close()` and the reader calls `open()` |
| **bind-mount** | Linux mount operation that makes a directory accessible at a second mount point; used to expose individual subdirectories into pod paths |

### CSI (Container Storage Interface)

| Term | Definition |
|------|-----------|
| **Provisioner** | The CSI driver identifier in a StorageClass (e.g., `vpc-file-pool.csi.ibm.io`); determines which driver handles PVC requests |
| **Sidecars** | Standard Kubernetes CSI helper containers (csi-provisioner, csi-resizer, csi-snapshotter, liveness-probe, node-driver-registrar) that translate Kubernetes events into CSI gRPC calls |
| **NodeStageVolume** | CSI RPC that mounts the entire NFS share to a staging directory on the node; called once per share per node |
| **NodePublishVolume** | CSI RPC that bind-mounts a SubVolume's subdirectory from the staging path into the pod's volume path |
| **VolumeContentSource** | Field in `CreateVolume` indicating the new volume should be populated from an existing snapshot or volume (clone) |
| **fsGroupPolicy** | CSI driver spec field controlling whether kubelet recursively `chown`s mounts to the pod's `fsGroup`; set to `None` to avoid failures with root_squash |
| **StorageClass** | Kubernetes resource that maps PVCs to a provisioner and its parameters (pool name, tier, UID/GID, mount options) |

### KubeVirt / OpenShift Virtualization

| Term | Definition |
|------|-----------|
| **CDI** | Containerized Data Importer — KubeVirt component that imports, clones, and uploads VM disk images into PVCs |
| **DataImportCron** | CDI resource that periodically imports OS images from a registry into golden image PVCs on a schedule |
| **DataSource** | CDI resource pointing to a bootable disk PVC; appears in the OpenShift Virtualization **InstanceTypes** catalog tab |
| **DataVolume** | CDI resource that combines PVC creation with data import/clone in a single object; incompatible with the pool CSI driver |
| **VolumePopulator** | Kubernetes mechanism CDI uses to populate PVCs; sets a `dataSource` that the pool CSI provisioner does not recognize |
| **StorageProfile** | CDI resource auto-created per StorageClass; defines access modes, volume mode, and clone strategy for CDI operations |
| **Template** | OpenShift resource defining a complete VM blueprint (CPU, memory, disks, network); appears in the **Templates** catalog tab |
| **InstanceType** | KubeVirt resource defining a CPU/RAM preset; paired with a DataSource (image) to create a VM from the InstanceTypes tab |
| **virt-handler** | KubeVirt DaemonSet component on each node that manages VM lifecycle; calls `chown(107, 107)` on filesystem-mode PVC mount directories |
| **virt-launcher** | KubeVirt pod that runs the QEMU process for a single VM |
| **QEMU** | Open-source machine emulator used by KubeVirt to run VMs; requires raw format disk images for filesystem-mode PVCs |
| **qcow2** | QEMU Copy-On-Write disk image format; cloud images are distributed as qcow2 but must be converted to raw for KubeVirt filesystem PVCs |

### Kubernetes

| Term | Definition |
|------|-----------|
| **Finalizer** | Kubernetes metadata that prevents a resource from being deleted until a controller removes it; used on pools to block deletion while SubVolumes exist |
| **Leader Election** | Pattern ensuring only one controller replica actively reconciles at a time; prevents concurrent pool state corruption |
| **OwnerReference** | Kubernetes metadata linking a child resource to its parent; enables automatic garbage collection (e.g., auto-created StorageClasses deleted with their pool) |
| **Admission Webhook** | Kubernetes extension that validates or mutates resources on create/update; the driver uses validating webhooks on all CRD types |
| **SCC** (SecurityContextConstraints) | OpenShift resource controlling pod security permissions; the node agent requires the `privileged` SCC (standard for CSI drivers) |

## Getting Started

### Prerequisites

- Kubernetes 1.28+
- IBM Cloud VPC infrastructure
- `kubectl` and `helm` (v3)

### Installation

```bash
# Apply CRDs
make deploy

# Or install via Helm
make helm-install
```

See [INSTALL.md](INSTALL.md) for full setup instructions including IBM Cloud API key configuration.

### Create a Pool

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: my-pool
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 1000
  maxShares: 5
  autoExpand: true
  allocationStrategy: spread
```

### Use a PVC

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-app-data
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 10Gi
```

## Development

```bash
make build          # Build binary
make test           # Unit tests with race detector
make test-coverage  # Tests with coverage report
make test-vm        # VM clone E2E test (requires ROKS + OCV)
make lint           # golangci-lint
make generate       # Regenerate CRD types and DeepCopy
```

Run a single test:

```bash
go test ./pkg/pool/ -v -race -run TestAllocate_SpreadStrategy
```

### Test Coverage

| Package | Coverage |
|---------|----------|
| `pkg/pool/` | 89.2% |
| `pkg/util/` | 88.0% |
| `pkg/driver/` | 81.2% |
| `pkg/ibmcloud/fake/` | 100% |
| `pkg/migrate/` | 78.5% |
| `test/integration/` | — (integration) |

510+ tests total, all passing with `-race`.

## Documentation

### Getting Started

| Document | Description |
|----------|-------------|
| [INSTALL.md](INSTALL.md) | Build, deploy, and Helm chart |
| [USER-GUIDE.md](USER-GUIDE.md) | End-user guide: pools, PVCs, snapshots, clones |
| [FAQ.md](FAQ.md) | Frequently asked questions |
| [examples/](examples/) | Ready-to-use YAML examples |

### Operations

| Document | Description |
|----------|-------------|
| [UPGRADE-GUIDE.md](UPGRADE-GUIDE.md) | Version upgrade procedures and compatibility matrix |
| [DAY2-OPERATIONS.md](DAY2-OPERATIONS.md) | Production runbook: health checks, draining, failover |
| [MONITORING.md](MONITORING.md) | Metrics reference, alerting rules, Grafana dashboard |
| [CAPACITY-PLANNING.md](CAPACITY-PLANNING.md) | Sizing guidance, cost estimation, example configurations |
| [BACKUP-RECOVERY.md](BACKUP-RECOVERY.md) | Snapshot scheduling, DR setup, recovery runbooks |
| [TROUBLESHOOTING.md](TROUBLESHOOTING.md) | Comprehensive troubleshooting guide |
| [PERFORMANCE-TUNING.md](PERFORMANCE-TUNING.md) | NFS tuning, IOPS planning, benchmarking |

### Configuration Reference

| Document | Description |
|----------|-------------|
| [HELM-VALUES.md](HELM-VALUES.md) | Complete Helm chart values reference |
| [CRD-SPEC.md](CRD-SPEC.md) | FileSharePool and SubVolume CRD definitions |
| [API-KEY-SETUP.md](API-KEY-SETUP.md) | Authentication via secret-common-lib |

### Architecture & Design

| Document | Description |
|----------|-------------|
| [FEATURES.md](FEATURES.md) | Complete feature catalog with comparison table |
| [ARCHITECTURE.md](ARCHITECTURE.md) | System design and data flows |
| [CSI-INTERFACE.md](CSI-INTERFACE.md) | CSI gRPC method implementations |
| [IBM-VPC-API.md](IBM-VPC-API.md) | VPC API client wrapper design |
| [CONSISTENCY-MODEL.md](CONSISTENCY-MODEL.md) | NFS consistency guarantees and limitations |
| [VOLUME-CLONING.md](VOLUME-CLONING.md) | Volume cloning design and usage |
| [VOLUME-GROUP-SNAPSHOTS.md](VOLUME-GROUP-SNAPSHOTS.md) | Multi-PVC coordinated snapshots |
| [CROSS-REGION-DR.md](CROSS-REGION-DR.md) | Cross-region disaster recovery |

### Platform-Specific

| Document | Description |
|----------|-------------|
| [VPC-NETWORKING.md](VPC-NETWORKING.md) | VPC network configuration for NFS |
| [VM-DISK-FORMATS.md](VM-DISK-FORMATS.md) | KubeVirt disk image format requirements |
| [GOLDEN-IMAGE-WORKFLOW.md](GOLDEN-IMAGE-WORKFLOW.md) | Automated VM golden image provisioning |
| [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md) | Known limitations and workarounds |
| [SECURITY.md](SECURITY.md) | Security policy and hardening guide |

### Development

| Document | Description |
|----------|-------------|
| [CODING-GUIDELINES.md](CODING-GUIDELINES.md) | Go conventions and patterns |
| [TESTING.md](TESTING.md) | Testing strategy and coverage targets |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Developer guide and contribution process |

### Release

| Document | Description |
|----------|-------------|
| [CHANGELOG.md](CHANGELOG.md) | Version history and release notes |

## License

[Apache License 2.0](LICENSE)
