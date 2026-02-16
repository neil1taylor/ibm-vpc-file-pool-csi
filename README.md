# IBM VPC File Pool CSI Driver

A Kubernetes [CSI](https://kubernetes-csi.github.io/docs/) driver that pools multiple PVCs as subdirectories within shared IBM Cloud VPC file shares, instead of the traditional 1:1 PVC-to-share mapping.

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
| Pool Manager | `pkg/pool/manager.go` | Core brain: allocation, deallocation, expansion |
| Share Selection | `pkg/pool/share.go` | Spread (most free) or binpack (least free) strategy |
| IBM VPC Client | `pkg/ibmcloud/client.go` | Thin wrapper around VPC file share API |
| CRD Types | `api/v1alpha1/` | FileSharePool and SubVolume definitions |

## CRDs

- **FileSharePool** (cluster-scoped) — defines a pool of VPC file shares with allocation strategy, auto-expansion, and capacity limits
- **SubVolume** (cluster-scoped) — tracks individual PVC allocations: which share, subdirectory path, and requested size

## Glossary

| Term | Definition |
|------|-----------|
| **Pool** (FileSharePool) | A group of VPC file shares managed as a unit, defined by a FileSharePool CR |
| **Share** (VPC File Share) | An NFS-exported block of cloud storage created via the IBM VPC API; each pool contains one or more shares |
| **SubVolume** | A subdirectory on a share that backs a single PVC, tracked by a SubVolume CR |
| **Subdirectory** | The physical directory on the NFS share (e.g., `/pvcs/pvc-abc123`), created by the node agent during publish |
| **Mount Target** | The VPC-managed NFS endpoint (IP address) used to mount a share on worker nodes |
| **Allocation** | The process of selecting a share, recording a SubVolume CR, and creating a subdirectory for a new PVC |
| **Staging** | Per-node NFS mount of a share at a staging directory; done once per share per node |

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

132 tests total, all passing with `-race`.

## Documentation

| Document | Description |
|----------|-------------|
| [ARCHITECTURE.md](ARCHITECTURE.md) | System design and data flows |
| [CRD-SPEC.md](CRD-SPEC.md) | FileSharePool and SubVolume CRD definitions |
| [CSI-INTERFACE.md](CSI-INTERFACE.md) | CSI gRPC method implementations |
| [IBM-VPC-API.md](IBM-VPC-API.md) | VPC API client wrapper design |
| [CODING-GUIDELINES.md](CODING-GUIDELINES.md) | Go conventions and patterns |
| [TESTING.md](TESTING.md) | Testing strategy and coverage targets |
| [USER-GUIDE.md](USER-GUIDE.md) | End-user guide |
| [INSTALL.md](INSTALL.md) | Build, deploy, and Helm chart |

## License

[Apache License 2.0](LICENSE)
