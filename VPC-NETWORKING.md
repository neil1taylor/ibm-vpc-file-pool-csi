# VPC Networking and NFS Connectivity

This document explains how NFS traffic flows between Kubernetes worker nodes and IBM Cloud VPC file shares, what VPC resources are involved, and what prerequisites must be in place.

## VPC Resource Topology

```
IBM Cloud Account
└── Resource Group (billing + IAM boundary)
    └── VPC
        ├── File Share (NFS backend, created in home zone)
        │   └── Mount Target (1 per VPC per share)
        │       └── FQDN resolves to zone-optimal IP via VPC DNS
        │
        ├── Zone: us-south-1 (home)
        │   ├── Subnet 0717-aaaa...
        │   └── Worker Nodes → DNS resolves FQDN → 10.240.1.x
        │
        ├── Zone: us-south-2 (accessor)
        │   ├── Subnet 0717-bbbb...
        │   └── Worker Nodes → DNS resolves FQDN → 10.241.1.x
        │
        └── Zone: us-south-3 (accessor)
            ├── Subnet 0717-cccc...
            └── Worker Nodes → DNS resolves FQDN → 10.242.1.x
```

Every file share in a pool belongs to the same VPC. Cross-VPC sharing is not supported.

## Mount Target Access Mode

The driver uses **VPC access mode** for all mount targets. This is the key design choice that simplifies cross-zone connectivity.

### VPC Mode vs Security Group Mode

| Aspect | VPC Mode (used) | Security Group Mode (not used) |
|--------|-----------------|-------------------------------|
| **Addressing** | FQDN in `MountPath` | Direct IP in `PrimaryIP.Address` |
| **Scope** | One mount target per VPC per share | One mount target per subnet per share |
| **Cross-zone** | Automatic via VPC DNS | Requires explicit mount targets per zone |
| **Security** | VPC-level access control | Security group rules per mount target |

### Why VPC Mode

VPC mode mount targets return a Fully Qualified Domain Name (FQDN) instead of a bare IP address. The VPC DNS service resolves this FQDN to **zone-optimal IP addresses** automatically — a node in `us-south-1` gets a different IP than a node in `us-south-2`, both from the same FQDN. This means:

- A single mount target per share serves all zones
- No per-zone mount target management by the driver
- Cross-zone traffic is handled transparently by IBM's infrastructure

### Mount Target Creation

Mount targets are created **inline** with the file share in a single VPC API call:

```go
// VPC access mode: bound to VPC, not a specific subnet
mountTarget := &vpcv1.ShareMountTargetPrototype{
    Name:              core.StringPtr(input.Name + "-mt"),
    VPC:               &vpcv1.VPCIdentityByID{ID: core.StringPtr(input.VPCId)},
    AccessProtocol:    core.StringPtr("nfs4"),
    TransitEncryption: core.StringPtr(transitEncryption),
}
```

The mount target FQDN/IP may not be available immediately after the share reaches `stable`. The driver polls every 10 seconds for up to 2 minutes until the address is populated.

## Network Path: End to End

```
┌─────────────────────────────────────────────────┐
│ Pod                                             │
│  /data/ ← bind-mount of subdirectory           │
└──────────────────────┬──────────────────────────┘
                       │ bind mount
┌──────────────────────▼──────────────────────────┐
│ Node Agent (DaemonSet)                          │
│  /staging/{share-id}/pvcs/pvc-abc123/           │
│       ▲                                         │
│       │ NFS mount (nfsvers=4.1, soft)           │
│       │ mount -t nfs4 <FQDN>:/ /staging/...    │
└───────┼─────────────────────────────────────────┘
        │
        │ VPC DNS resolves FQDN → zone-local IP
        │
        │ TCP 2049 (NFS)
        │ within VPC private network
        │
┌───────▼─────────────────────────────────────────┐
│ Mount Target                                    │
│  FQDN: share-xxxx.vpc-file.appdomain.cloud     │
│  Zone-local IP: 10.240.x.x                     │
└───────┬─────────────────────────────────────────┘
        │
┌───────▼─────────────────────────────────────────┐
│ VPC File Share (NFS export)                     │
│  /                                              │
│  └── pvcs/                                      │
│      ├── pvc-aaa/  (50 GB allocated)            │
│      ├── pvc-bbb/  (100 GB allocated)           │
│      └── pvc-ccc/  (25 GB allocated)            │
└─────────────────────────────────────────────────┘
```

All NFS traffic stays within the VPC private network. No traffic traverses the public internet.

## VPC Configuration Sources

The driver discovers VPC configuration from secrets and ConfigMaps that already exist on managed ROKS/IKS clusters:

| Config | Source | Example |
|--------|--------|---------|
| VPC ID | ConfigMap `ibm-cloud-provider-data` | `r006-a1b2c3d4-...` |
| Subnet ID | ConfigMap `ibm-cloud-provider-data` | `0717-12345678-...` |
| Resource Group | Pool spec or secret provider default | UUID |
| Region | Derived from pool spec zone or API endpoint | `us-south` |

These are passed to the reconciler at startup via `SetVPCConfig(vpcID, subnetID, defaultResourceGroup)`.

## VPC API Endpoint Selection

The driver prefers **private endpoints** so VPC API traffic stays on the IBM Cloud backbone:

| Priority | Endpoint Pattern | When Used |
|----------|-----------------|-----------|
| 1st | `https://{region}.private.iaas.cloud.ibm.com/v1` | Clusters with private service endpoints (VPE/CSE) |
| 2nd | `https://{region}.iaas.cloud.ibm.com/v1` | Clusters with public endpoints |
| 3rd | Constructed from region name | Fallback |

On managed ROKS/IKS clusters with private service endpoints enabled, all VPC API calls stay within the IBM network.

## Security Group Requirements

Worker node security groups **must allow TCP port 2049 outbound** to the VPC file share mount target. On managed ROKS/IKS clusters, this is typically pre-configured.

```
Worker Node Security Group
  ├── Outbound: TCP 2049 → VPC file share mount target  (required)
  └── (other existing rules)
```

!!! warning "Common troubleshooting issue"
    If pods hang during mount with `nfs: server not responding` errors, verify that the worker node security group allows outbound TCP 2049 to the subnet where the mount target resides.

## Cross-Zone Access

### Single-Zone (Default)

A pool with only `spec.zone` creates shares and mount targets in one availability zone. All worker nodes and file shares are co-located — NFS traffic never leaves the zone.

```
Zone 1 (us-south-1)
┌──────────────────────────────────────────────────────────────┐
│                                                              │
│  Worker Node A          Worker Node B          VPC File Share│
│  ┌──────────────┐      ┌──────────────┐      ┌─────────────┐│
│  │ Pod          │      │ Pod          │      │ NFS backend  ││
│  │ /data/ ←─────┼──┐   │ /data/ ←─────┼──┐   │             ││
│  └──────────────┘  │   └──────────────┘  │   │ /pvcs/      ││
│                    │                     │   │  ├─ pvc-aaa/ ││
│                    │   NFS (TCP 2049)    │   │  └─ pvc-bbb/ ││
│                    └─────────────────────┼──▶│             ││
│                        zone-local        └──▶│ Mount Target ││
│                        ~0.2ms latency        │ FQDN → IP   ││
│                                              └─────────────┘│
└──────────────────────────────────────────────────────────────┘
```

```yaml
apiVersion: storage.ibm.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-purpose
spec:
  zone: us-south-1
  # no accessorZones — single-zone only
```

Worker nodes in **other zones cannot access the share** unless accessor zones are configured. This is the simplest and lowest-latency configuration.

### Multi-Zone with Accessor Zones

```yaml
apiVersion: storage.ibm.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-purpose
spec:
  zone: us-south-1
  accessorZones:
    - zone: us-south-2
      subnetID: "0717-12345678"
    - zone: us-south-3
      subnetID: "0717-87654321"
  # ...
```

Because VPC mode uses DNS-based routing, the same FQDN is recorded for all zones in the pool status. VPC DNS automatically resolves it to the zone-optimal NFS server IP for each requesting node.

### Cross-Zone Traffic Flow

In a cross-zone pool, the file share lives in the home zone but pods on worker nodes in accessor zones access it over the VPC backbone:

```
Zone 1 (us-south-1)                      Zone 2 (us-south-2)
┌─────────────────────────┐              ┌─────────────────────────┐
│                         │              │                         │
│  VPC File Share         │              │  Worker Node / VM       │
│  (NFS backend)          │◄────NFS──────│  running Pod            │
│                         │   TCP 2049   │                         │
│  /pvcs/pvc-aaa/  ◄──┐  │  cross-zone  │  /data/ ← bind-mount   │
│  /pvcs/pvc-bbb/     │  │  VPC backbone │       of pvc-aaa/      │
│                     │  │              │                         │
│  Mount Target       │  │              │  DNS resolves share     │
│  FQDN: share-xxx.. │  │              │  FQDN → zone-2 IP      │
│                     │  │              │  → routes to zone-1     │
└─────────────────────┘  │              └─────────────────────────┘
                         │
                         │              ┌─────────────────────────┐
                         │              │  Zone 3 (us-south-3)    │
                         └──────NFS─────│  Worker Node / VM       │
                             TCP 2049   │  running Pod            │
                             cross-zone │  /data/ ← pvc-bbb/     │
                                        └─────────────────────────┘
```

All NFS traffic stays within the VPC private network — it never traverses the public internet.

### Cross-Zone Trade-offs

| Factor | Same-Zone | Cross-Zone |
|--------|-----------|------------|
| **NFS latency** | ~0.2ms | ~1-2ms |
| **Bandwidth cost** | Included | Metered cross-zone traffic |
| **Reliability** | Zone-local only | Inter-zone link dependency |
| **Failure mode** | `soft` mount returns error | `soft` mount returns error (no hang) |

**Good fit for cross-zone:** log storage, batch processing, model artifacts, config data — workloads that are read-heavy or latency-tolerant.

**Prefer same-zone for:** databases, real-time applications, latency-sensitive workloads — create a separate pool per zone instead.

### Alternative: One Pool Per Zone

For latency-sensitive workloads, avoid cross-zone NFS entirely by creating a **separate pool in each zone**. Each pool's shares and consumers stay zone-local.

```
Zone 1 (us-south-1)                      Zone 2 (us-south-2)
┌───────────────────────────┐            ┌───────────────────────────┐
│                           │            │                           │
│  Pool: db-storage-z1      │            │  Pool: db-storage-z2      │
│                           │            │                           │
│  Worker Node              │            │  Worker Node              │
│  ┌─────────────────┐     │            │  ┌─────────────────┐     │
│  │ DB Pod          │     │            │  │ DB Pod          │     │
│  │ /data/ ←────────┼──┐  │            │  │ /data/ ←────────┼──┐  │
│  └─────────────────┘  │  │            │  └─────────────────┘  │  │
│                       │  │            │                       │  │
│  VPC File Share    ◄──┘  │            │  VPC File Share    ◄──┘  │
│  zone-local NFS          │            │  zone-local NFS          │
│  ~0.2ms latency          │            │  ~0.2ms latency          │
│                           │            │                           │
└───────────────────────────┘            └───────────────────────────┘

              No cross-zone NFS traffic
```

```yaml
# Pool for zone 1
apiVersion: storage.ibm.io/v1alpha1
kind: FileSharePool
metadata:
  name: db-storage-z1
spec:
  zone: us-south-1
---
# Pool for zone 2
apiVersion: storage.ibm.io/v1alpha1
kind: FileSharePool
metadata:
  name: db-storage-z2
spec:
  zone: us-south-2
```

Use Kubernetes topology constraints (node affinity or `allowedTopologies` on the StorageClass) to ensure pods land in the same zone as their pool. This trades operational simplicity for guaranteed low latency.

### How the Node Agent Selects the Right IP

During `NodeStageVolume`, the node agent:

1. Reads its zone from the node's `topology.kubernetes.io/zone` label
2. Checks the VolumeContext for a zone-specific key (e.g., `server.us-south-2`)
3. If found, uses the zone-specific IP; otherwise falls back to the primary IP

This keeps cross-zone latency low and avoids metered cross-zone bandwidth charges.

## File Share Lifecycle and VPC API Timing

VPC file share operations are **slow** (30-90 seconds). The driver isolates these from the CSI hot path:

| Operation | Typical Duration | Where It Runs |
|-----------|-----------------|---------------|
| Create share + inline mount target | 30-90s | Pool reconciler (background) |
| Wait for mount target IP | 10-120s | Pool reconciler (background) |
| Expand share | 30-60s | Pool reconciler (background) |
| Delete share | 10-30s | Pool reconciler (background) |
| `CreateVolume` (pick existing share) | < 1s | CSI controller (hot path) |
| `NodeStageVolume` (NFS mount) | 1-5s | CSI node agent |
| `NodePublishVolume` (bind-mount) | < 1s | CSI node agent |

The CSI `CreateVolume` call **never creates VPC resources**. It picks an existing share from the pool and records a SubVolume CR. All VPC API calls happen in the pool reconciler's background loop.

## NFS Mount Options

```
nfsvers=4.1,soft,timeo=600,retrans=3
```

| Option | Value | Why |
|--------|-------|-----|
| `nfsvers` | `4.1` | Modern NFS protocol with better performance |
| `soft` | - | Returns errors instead of hanging indefinitely on NFS failure |
| `timeo` | `600` (60s) | Timeout in deciseconds before first retransmission |
| `retrans` | `3` | Maximum retransmissions before giving up |

!!! danger "Never use `hard` mount option"
    Hard mounts cause pods to hang **indefinitely** when the NFS server is unreachable. With `soft`, pods receive an error after ~3 minutes (60s x 3 retries) and can fail gracefully.

Optional encryption in transit adds `sec=krb5p` when `spec.encryptInTransit: true` is set on the pool.

## Mount Caching: One NFS Mount Serves Many PVCs

The node agent maintains a mount cache to avoid redundant NFS mounts:

```
Node with 3 PVCs from the same share:

  NFS mount (1 total):
    10.240.1.5:/ → /staging/{share-id}/

  Bind mounts (3 total):
    /staging/{share-id}/pvcs/pvc-aaa/ → Pod A /data/
    /staging/{share-id}/pvcs/pvc-bbb/ → Pod B /data/
    /staging/{share-id}/pvcs/pvc-ccc/ → Pod C /data/
```

When `NodeStageVolume` is called for a share that's already mounted on the node, it's a no-op. The NFS mount is only removed when the last PVC using that share on the node is unpublished.

## Rate Limiting

The VPC API client enforces a client-side rate limit of **5 requests per second** to stay within IBM Cloud API rate limits. All operations go through `withAuth()` which combines rate limiting with IAM token refresh.

| HTTP Status | Error | Behavior |
|-------------|-------|----------|
| 404 | `ErrShareNotFound` | Treated as success for deletes (idempotent) |
| 429 | `ErrAPIRateLimit` | Retry with backoff |
| 401/403 | `ErrAuthentication` | Credential issue, check secrets |
| 500+ | Server error | Retry with backoff |

## Resource Group

All shares in a pool belong to the same IBM Cloud resource group, set via `spec.resourceGroup` on the FileSharePool CR. If not specified, the default resource group from the cluster's secret provider configuration is used. This controls:

- **IAM access control** — who can manage the shares
- **Billing** — cost tracking and quota enforcement

## Encryption in Transit

When `spec.encryptInTransit: true` is set on the pool, the mount target is created with `TransitEncryption: "user_managed"`. This adds Kerberos encryption (`sec=krb5p`) to the NFS mount, encrypting all data on the wire between the worker node and the file share.

```
Without encryption (default):

  Worker Node                        VPC File Share
  ┌──────────────┐    NFS v4.1      ┌──────────────┐
  │ Pod /data/   │─────────────────▶│ /pvcs/...    │
  └──────────────┘   plaintext       └──────────────┘
                     TCP 2049
                     (within VPC)


With encryption (encryptInTransit: true):

  Worker Node                        VPC File Share
  ┌──────────────┐    NFS v4.1      ┌──────────────┐
  │ Pod /data/   │──── krb5p ──────▶│ /pvcs/...    │
  └──────────────┘   encrypted       └──────────────┘
                     TCP 2049
                     (within VPC)

  Mount options: nfsvers=4.1,soft,timeo=600,retrans=3,sec=krb5p
  Mount target:  TransitEncryption: "user_managed"
```

```yaml
apiVersion: storage.ibm.io/v1alpha1
kind: FileSharePool
metadata:
  name: compliant-storage
spec:
  zone: us-south-1
  encryptInTransit: true
  # ...
```

!!! note
    Encryption in transit adds CPU overhead on both the client (worker node) and server (file share backend). Use it when compliance requirements (HIPAA, PCI-DSS, etc.) mandate encrypted storage traffic. For workloads within a single VPC where the network is already trusted, the default plaintext NFS is sufficient.

## Summary: What Must Be in Place

Before deploying the driver, ensure these VPC resources and configurations exist:

| Prerequisite | How to Verify |
|-------------|---------------|
| VPC exists | `ibmcloud is vpcs` |
| Worker node subnet exists in target zone(s) | `ibmcloud is subnets --vpc <vpc-id>` |
| Security group allows TCP 2049 outbound | `ibmcloud is security-group-rules <sg-id>` |
| IAM credentials available (API key or pod identity) | Check `ibm-cloud-credentials` or `storage-secret-store` secret in `kube-system` |
| `ibm-cloud-provider-data` ConfigMap exists | `kubectl get cm ibm-cloud-provider-data -n kube-system` |
| File share quota sufficient | `ibmcloud is share-profiles` and account quotas |
