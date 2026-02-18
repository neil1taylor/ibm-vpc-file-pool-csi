# How the Driver Works

This page explains what each component does in plain language. For the full technical reference, see [Architecture](ARCHITECTURE.md).

## The Two Main Pieces

The driver runs two things on your cluster:

1. **Controller** (one instance) — decides *where* each PVC goes
2. **Node Agent** (one per worker node) — does the actual *mounting*

```
                    PVC created
                        │
                        ▼
              ┌──────────────────┐
              │    Controller    │   "Put this PVC on share #2"
              │  (1 per cluster) │
              └────────┬─────────┘
                       │ records a SubVolume CR
                       ▼
              ┌──────────────────┐
              │   Node Agent     │   "Mount share #2, bind-mount
              │  (1 per node)    │    the subdirectory into the pod"
              └──────────────────┘
```

## Controller

The controller is a Deployment (single replica with leader election). It handles volume lifecycle at the cluster level.

**When a PVC is created**, the controller:

1. Looks at the pool of pre-created VPC file shares
2. Picks a share that has enough free space (using a spread or binpack strategy)
3. Creates a **SubVolume CR** to record the allocation — "PVC X lives on share Y at path `/pvcs/pvc-xyz`"
4. Returns the share's NFS IP and subdirectory path back to Kubernetes

This takes less than a second because it's just picking an existing share and writing a CR — no VPC API calls.

**When a PVC is deleted**, the controller removes the SubVolume CR and updates capacity tracking.

### Pool Manager (runs inside the controller)

The Pool Manager is a background reconciler that handles the slow VPC API work so the controller doesn't have to:

- Creates new VPC file shares when the pool is running low on space (auto-expansion)
- Monitors share health via the VPC API
- Marks unhealthy shares as draining (no new PVCs, existing ones keep working)
- Updates pool status and Prometheus metrics

VPC API calls take 30-90 seconds each, which is why they happen here in the background — never during a PVC create request.

For the Pool Manager interface and reconciliation data flows, see [Architecture > Pool Manager](ARCHITECTURE.md#3-pool-manager).

## Node Agent

The node agent is a DaemonSet (one pod per worker node). It handles the actual NFS mounts and bind-mounts.

**When a pod starts** that uses a pooled PVC, the node agent on that node:

1. **Mounts the NFS share** — if it isn't already mounted on this node. Uses `soft,timeo=600,retrans=3` so pods get errors instead of hanging if NFS goes down.
2. **Creates the subdirectory** — `/pvcs/pvc-xyz` on the share, if it doesn't already exist (with the configured UID/GID/permissions).
3. **Bind-mounts the subdirectory** into the pod's volume path. The pod only sees its own subdirectory, not the whole share.

**When a pod stops**, the node agent removes the bind-mount. The NFS share stays mounted as long as other PVCs on that node still use it.

### Mount Caching

The node agent keeps one NFS mount per share per node. If 10 PVCs on the same node all use the same share, there's still just one NFS connection — each PVC gets its own bind-mount of its subdirectory.

```
NFS mount (1 per share per node):
  10.240.1.5:/ → /staging/{share-id}/

Bind-mounts (1 per PVC):
  /staging/{share-id}/pvcs/pvc-aaa/ → Pod A /data/
  /staging/{share-id}/pvcs/pvc-bbb/ → Pod B /data/
  /staging/{share-id}/pvcs/pvc-ccc/ → Pod C /data/
```

For cross-zone IP selection and the nsenter mount wrapper, see [Architecture > CSI Node Agent](ARCHITECTURE.md#2-csi-node-agent-daemonset).

## Other Components

These run inside the controller pod but handle specialized tasks:

| Component | What it does | More detail |
|-----------|-------------|-------------|
| **IBM VPC Client** | Thin wrapper around the VPC file share API. All VPC calls go through here. | [IBM VPC API](IBM-VPC-API.md) |
| **Clone Worker** | Copies data for volume clones in the background. Small clones finish inline; large ones run async. | [Volume Cloning](VOLUME-CLONING.md) |
| **Replication Controller** | Cross-region disaster recovery — rsyncs SubVolume data between pools on a schedule. | [Cross-Region DR](CROSS-REGION-DR.md) |
| **Hook Orchestrator** | Runs pre/post lifecycle hooks (exec commands in pods, or HTTP webhooks) for replication and group snapshots. | [Architecture > Hook Orchestrator](ARCHITECTURE.md#8-hook-orchestrator) |
| **Admission Webhooks** | Validates CRD fields at admission time so bad configs are rejected immediately. | [Architecture > Admission Webhooks](ARCHITECTURE.md#9-admission-webhooks) |

## What Lives Where

All state is stored in Kubernetes CRDs — no external database, no ConfigMaps for state.

| CRD | Purpose |
|-----|---------|
| **FileSharePool** | Defines a pool: zone, share size, max shares, allocation strategy, auto-expand settings |
| **SubVolume** | Tracks one PVC's allocation: which share, subdirectory path, requested size |
| **Snapshot** | Point-in-time copy of a SubVolume's subdirectory |
| **VolumeGroupSnapshot** | Coordinates snapshots across multiple PVCs with lifecycle hooks |
| **ReplicationPolicy** | Cross-region replication schedule and configuration |

For full CRD definitions, see [CRD Specification](CRD-SPEC.md).

## Summary

| | Controller | Node Agent |
|---|---|---|
| **Runs as** | Deployment (1 replica) | DaemonSet (1 per node) |
| **Job** | Pick shares, record allocations, manage pool | Mount NFS, bind-mount subdirs into pods |
| **Calls VPC API?** | Yes (via Pool Manager, in background) | No |
| **CRD access** | Read/write FileSharePool + SubVolume | Read-only (gets info from CSI RPCs) |
| **Speed** | CreateVolume < 1 second | NFS mount 1-5 seconds, bind-mount < 1 second |
