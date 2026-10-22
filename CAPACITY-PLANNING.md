# Capacity Planning Guide — IBM VPC File Pool CSI Driver

Decision framework for sizing pools, selecting profiles, and planning growth. This guide wraps the raw performance data in [PERFORMANCE-TUNING.md](PERFORMANCE-TUNING.md) into actionable sizing guidance.

---

## Decision Flowchart

Use this to determine your pool configuration:

```
1. What workload type?
   ├── General (configs, logs, small apps) ──→ Profile: dp2, Share: 1-2 TB, Strategy: spread
   ├── Database / analytics ──→ Profile: dp2 (set spec.iops), Share: 2-4 TB, Strategy: spread
   ├── CI/CD ephemeral ──→ Profile: dp2, Share: 500 GB-1 TB, Strategy: binpack
   ├── ML/AI training ──→ Profile: dp2, Share: 4-16 TB, Strategy: spread
   └── KubeVirt VMs ──→ Profile: dp2, Share: 2-4 TB, Strategy: spread, UID: 107

2. How many trust boundaries?
   ├── Single team / single tenant ──→ 1 pool
   ├── Multiple teams, same trust level ──→ 1 pool with tiers
   └── Multiple teams, different trust levels ──→ Separate pools per trust boundary

3. How many zones?
   ├── Pods in one zone ──→ 1 pool in that zone
   ├── Pods in many zones, shared data ──→ 1 pool (its one mount target serves every zone)
   └── Zone-isolated data ──→ 1 pool per zone

4. Need DR?
   ├── No ──→ Done
   └── Yes ──→ Add ReplicationPolicy + destination pool in DR region
```

---

## How Many Pools?

Create separate pools for each distinct combination of:

| Dimension | When to Separate | Example |
|-----------|-----------------|---------|
| **Trust boundary** | Different teams that should not share NFS storage | `team-a-pool`, `team-b-pool` |
| **Performance tier** | Workloads needing different IOPS levels | `standard-pool` (dp2), `high-iops-pool` (dp2 with a higher `spec.iops`) |
| **Zone** | Zone-local data residency requirements | `us-south-1-pool`, `us-south-2-pool` |
| **Lifecycle** | Different retention or backup policies | `persistent-pool`, `ephemeral-pool` |
| **Workload class** | VMs need different UID/GID than app workloads | `app-pool` (UID 1000), `vm-pool` (UID 107) |

**Rule of thumb:** Start with fewer pools and split only when you have a concrete reason. Each pool consumes VPC share quota.

---

## Share Size Selection

### Formula

```
Minimum share size = (target PVCs per share) × (average PVC size) / (target utilization)

Example:
  Target: 100 PVCs per share
  Average PVC size: 10 GB
  Target utilization: 80%
  Minimum share size = 100 × 10 / 0.8 = 1250 GB → round up to 2000 GB
```

### Trade-offs

| Share Size | Pros | Cons |
|-----------|------|------|
| 500 GB - 1 TB | Lower blast radius, faster creation, finer control | Uses more VPC quota, more management overhead |
| 2 TB (recommended start) | Good balance of quota efficiency and blast radius | — |
| 4+ TB | Fewer shares, more room for growth | Higher blast radius, longer creation time |

See [PERFORMANCE-TUNING.md — Share Sizing Strategy](PERFORMANCE-TUNING.md#share-sizing-strategy) for workload-specific sizing tables.

---

## IOPS Tier Selection

### dp2 (the only profile)

`dp2` is the only profile VPC file shares offer. It accepts 100 to 96,000 IOPS and 10 to 32,000 GB per share. If you do not set `spec.iops`, the VPC gives each share 100 IOPS, whatever its size. IOPS does not grow with share size, so set `spec.iops` for any workload that needs more.

The VPC limits the maximum IOPS by share size and refuses values out of range. For example, 10,000 IOPS needs a share of at least 500 GB, and a 300 GB share allowed about 6,000. For the exact limits, see IBM's [About file storage for VPC](https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-profiles).

### Setting IOPS

Set `spec.iops` when you need more than 100 IOPS per share. Higher IOPS costs more.

```yaml
spec:
  profile: dp2
  shareSizeGB: 2000
  iops: 24000        # IOPS per share (costs more)
```

### When to Set Higher IOPS

| Scenario | Recommendation |
|----------|---------------|
| PVCs mostly idle (configs, static assets) | Leave `spec.iops` unset — don't pay for unused IOPS |
| Mixed workloads | Set `spec.iops` for the busiest expected load; there is no burst IOPS |
| Database PVCs with sustained random I/O | Set a high `spec.iops` |
| ML training with sequential large reads | Leave `spec.iops` unset — throughput is more relevant than IOPS |

See [PERFORMANCE-TUNING.md — IOPS Planning](PERFORMANCE-TUNING.md#iops-planning) for the full IOPS table and calculation examples.

---

## Binpack vs Spread

| Factor | Spread | Binpack |
|--------|--------|---------|
| **Blast radius** | Low — PVCs distributed evenly | High — one share holds most PVCs |
| **VPC quota usage** | Higher — all shares active | Lower — fewer shares in use |
| **IOPS distribution** | Even across shares | Concentrated on active share |
| **Best for** | Production, databases, long-lived PVCs | CI/CD, ephemeral, high-churn PVCs |
| **Auto-expansion** | Triggers when all shares fill together | Triggers when active share is full |

**Decision rule:** Use `spread` unless you're hitting the 300-share quota limit or running ephemeral CI/CD workloads.

---

## VPC Quota Budget

VPC accounts have a 300 file share limit shared across all consumers. Plan your budget:

```
VPC Share Budget Planner
========================
Account share limit:                    300
  - Stock IBM CSI driver shares:       - ___  (check: ibmcloud is shares | grep ibm-vpc-file)
  - Manually created shares:           - ___
  - Other teams/projects:              - ___
  - Safety buffer (10%):               - 30
  = Available for pool CSI:              ___

Pool allocations:
  Pool: ____________  maxShares: ___
  Pool: ____________  maxShares: ___
  Pool: ____________  maxShares: ___
  Total pool CSI shares:          ___
```

Check current usage:
```bash
ibmcloud is shares --output json | jq length
```

If approaching the limit, consider:
1. Consolidate pools (fewer, larger pools)
2. Switch to `binpack` strategy (fewer active shares)
3. Increase share size to host more PVCs per share
4. Request a quota increase via IBM Cloud support ticket

---

## Expansion Strategy Tuning

### expandThresholdPercent

Controls when new shares are created:

| Threshold | Risk Profile | Cost Impact | Recommended For |
|-----------|-------------|-------------|-----------------|
| 60-70% | Conservative — large buffer for burst | Higher — shares sit partially empty | Unpredictable burst workloads (CI/CD, batch) |
| 80% (default) | Balanced | Moderate | Most production workloads |
| 90-95% | Aggressive — minimal waste | Lower — but PVCs may fail during expansion | Stable, predictable workloads |

### initialShares Formula

Pre-create enough shares to absorb your initial PVC burst without triggering expansion:

```
initialShares = ceil(initial_burst_pvc_count × avg_pvc_size_gb / (share_size_gb × threshold / 100))

Example:
  Initial burst: 200 PVCs at 10 GB each = 2000 GB
  Share size: 2000 GB, threshold: 80%
  Usable per share: 2000 × 0.80 = 1600 GB
  initialShares = ceil(2000 / 1600) = 2

  Use initialShares: 3 for safety margin
```

---

## Growth Projection

Use Prometheus metrics to project when you'll need more capacity:

```promql
# Current utilization
vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb * 100

# Allocation rate (GB per day)
deriv(vpc_file_pool_allocated_gb[7d]) * 86400

# Projected days to 90% utilization
(vpc_file_pool_capacity_gb * 0.9 - vpc_file_pool_allocated_gb)
/ (deriv(vpc_file_pool_allocated_gb[7d]) * 86400 + 0.001)

# PVC creation rate (per day)
increase(vpc_file_pool_allocations_total{status="success"}[7d]) / 7
```

Set alerts for proactive capacity management — see [MONITORING.md](MONITORING.md) for alerting rules.

---

## Cost Estimation

### Pool CSI vs Stock Driver Savings

The pool CSI driver reduces costs by consolidating PVCs onto shared shares:

```
Stock driver cost (1:1 mapping):
  PVCs: 200
  Average PVC size: 5 GB
  Minimum share size: 10 GB (VPC minimum)
  Shares needed: 200
  Total billed capacity: 200 × 10 GB = 2,000 GB
  VPC shares used: 200

Pool driver cost:
  PVCs: 200
  Average PVC size: 5 GB
  Total data: 200 × 5 GB = 1,000 GB
  Share size: 2,000 GB
  Shares needed: 1 (1,000 GB fits in a single 2 TB share at 50% util)
  Total billed capacity: 2,000 GB
  VPC shares used: 1

Savings:
  Capacity billed: Same (2,000 GB) — but pool uses 1 share vs 200
  Share quota freed: 199 shares
  Provisioning speed: <1s vs 30-90s per PVC
```

The savings increase as PVC sizes decrease below the 10 GB VPC minimum.

### Cost Variables

| Factor | Impact |
|--------|--------|
| Share IOPS profile | Higher IOPS = higher cost per GB |
| Snapshot storage | Snapshots consume share capacity (billed). A snapshot is kept on its volume's share and is charged the volume's size (a copy) or a tenth of it (an instant VM snapshot). A share without that much free space refuses the snapshot (`ResourceExhausted`). Snapshots do not count as PVCs. |
| Replication storage | DR destination pool is billed separately |

---

## Example Configurations

### Small Cluster (10 nodes, <100 PVCs)

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 1000
  maxShares: 3
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultUID: 1000
  defaultGID: 1000
  defaultPermissions: "0755"
```

**Budget:** 3 VPC shares max. Capacity: up to 3 TB for ~300 PVCs at 10 GB average.

### Medium Cluster (50 nodes, 100-1000 PVCs)

```yaml
# General purpose pool
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 2000
  maxShares: 10
  initialShares: 2
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultUID: 1000
  defaultGID: 1000
  defaultPermissions: "0755"
---
# CI/CD ephemeral pool
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: ci-cd
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 500
  maxShares: 5
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 90
  allocationStrategy: binpack
  defaultUID: 1000
  defaultGID: 1000
  defaultPermissions: "0777"
```

**Budget:** 15 VPC shares max. General: 20 TB for ~1000 PVCs. CI/CD: 2.5 TB for high-churn workloads.

### Large Cluster (100+ nodes, 1000+ PVCs)

```yaml
# Production workloads
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: production
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 4000
  maxShares: 25
  initialShares: 5
  autoExpand: true
  expandThresholdPercent: 75
  allocationStrategy: spread
  defaultUID: 1000
  defaultGID: 1000
  defaultPermissions: "0755"
---
# High-IOPS databases
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: high-iops
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 4000
  iops: 24000
  maxShares: 10
  initialShares: 2
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultUID: 1000
  defaultGID: 1000
  defaultPermissions: "0755"
---
# CI/CD
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: ci-cd
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 1000
  maxShares: 10
  initialShares: 2
  autoExpand: true
  expandThresholdPercent: 90
  allocationStrategy: binpack
  defaultUID: 1000
  defaultGID: 1000
  defaultPermissions: "0777"
```

**Budget:** 45 VPC shares max. Production: 100 TB. High-IOPS: 40 TB. CI/CD: 10 TB.

### KubeVirt VM Cluster

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: vm-storage
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 4000
  maxShares: 10
  initialShares: 2
  autoExpand: true
  expandThresholdPercent: 75
  allocationStrategy: spread
  defaultUID: 107          # QEMU user
  defaultGID: 107
  defaultPermissions: "0777"
  goldenImages:            # Auto-provision golden images for KubeVirt
    enabled: true
    targetNamespaces:
      - openshift-virtualization-os-images
    pvcSizeGB: 30
```

**Budget:** 10 VPC shares max. Capacity: 40 TB for ~40 VMs at 30 GB boot disk + data.

---

## See Also

- [PERFORMANCE-TUNING.md](PERFORMANCE-TUNING.md) — NFS tuning, IOPS planning, benchmarking
- [USER-GUIDE.md](USER-GUIDE.md) — Pool creation and management
- [MONITORING.md](MONITORING.md) — Capacity metrics and alerting
- [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md) — VPC quota and platform constraints
- [examples/](examples/) — Ready-to-use pool configurations
