# Performance Tuning Guide — IBM VPC File Pool CSI Driver

This guide covers NFS mount optimization, share sizing, allocation strategies, IOPS planning, capacity management, and benchmarking for the pool CSI driver.

---

## NFS Mount Options

### Why `soft` Not `hard`

The driver defaults to `soft` NFS mounts. This is a deliberate safety choice:

| Option | Behavior | Risk |
|--------|----------|------|
| `hard` | Retries NFS operations indefinitely until the server responds | Pods hang forever if the NFS server is unreachable. `kubectl exec`, `kubectl logs`, and even `kubectl delete pod` can all block. The only fix is a node reboot. |
| `soft` | Returns an error after `timeo × retrans` timeout | Pods get I/O errors, but remain responsive. The application can handle the error, and the pod can be deleted and rescheduled. |

**Always use `soft` for pool-based storage.** A single NFS share serves many PVCs — a hard-mount hang on one share blocks every pod on that node that uses that share.

### timeo and retrans

```yaml
mountOptions:
  - soft
  - timeo=600     # 60 seconds (in deciseconds) per NFS RPC attempt
  - retrans=3     # Retry 3 times before returning error
```

- **Total timeout:** `timeo × retrans = 600 × 3 = 1800 deciseconds = 180 seconds`
- For latency-sensitive workloads, lower `timeo` to 100 (10 seconds) with `retrans=5`
- For batch/background workloads, keep the defaults — they handle transient NFS blips without spurious errors

### rsize and wsize

The NFS read/write buffer sizes default to negotiation between client and server. On IBM VPC File Storage, the server typically negotiates 1 MB (1048576 bytes).

```yaml
mountOptions:
  - rsize=1048576
  - wsize=1048576
```

Explicitly setting these is usually unnecessary, but can help if:
- You see unexpectedly low throughput on large sequential reads/writes
- Your workload does many small random I/Os (smaller buffers like 32768 can reduce latency)

### NFSv4.1

Always use NFSv4.1:

```yaml
mountOptions:
  - nfsvers=4.1
```

NFSv4.1 adds:
- **Session trunking** — multiple TCP connections for higher throughput
- **Directory delegation** — reduces round trips for metadata operations
- **pNFS support** — parallel data access (if the server supports it)

IBM VPC File Storage supports NFSv4.1 natively. Do not use NFSv3 — it lacks these features and requires additional ports (portmapper, mountd).

### Encryption in Transit

IBM VPC File Storage supports NFS over TLS (encryption in transit). Enable it in the pool spec or mount options:

```yaml
mountOptions:
  - nfsvers=4.1
  - sec=sys          # Default: no encryption (fastest)
  # - sec=krb5p      # Kerberos with privacy (encrypted) — requires setup
```

**Throughput impact:** Encryption in transit reduces throughput by approximately 20-30% due to TLS overhead. Use it when:
- Data traverses untrusted network segments
- Compliance requirements mandate encryption in transit
- The workload is not throughput-sensitive

For most in-VPC workloads, `sec=sys` is sufficient — VPC network traffic is already isolated at the hypervisor level.

---

## Share Sizing Strategy

### Sizing by Workload Type

| Workload | Share Size | PVC Size (typical) | PVCs per Share | Notes |
|----------|-----------|-------------------|----------------|-------|
| General (configs, logs, small apps) | 1000-2000 GB | 1-10 GB | 100-2000 | Most common. Optimize for PVC count. |
| Databases, analytics | 2000-4000 GB | 50-500 GB | 4-80 | Fewer, larger PVCs. Consider IOPS isolation. |
| CI/CD ephemeral | 500-1000 GB | 1-5 GB | 100-1000 | High churn. Use `binpack` to consolidate. |
| ML/AI training data | 4000-16000 GB | 500-4000 GB | 1-30 | Large datasets. Maximize throughput. |
| Shared team storage | 4000+ GB | N/A (1 large PVC) | 1-10 | Few large PVCs shared via RWX. |

### PVC Density Guidelines

- **Target 50-200 PVCs per share** for general workloads. This keeps metadata operations fast and limits blast radius.
- **Under 50 PVCs per share** for database workloads where IOPS isolation matters.
- **Over 200 PVCs per share** is fine for configs and logs where individual PVCs do minimal I/O.

### Large vs. Small Shares

| Approach | Pros | Cons |
|----------|------|------|
| Fewer large shares (4+ TB) | Fewer VPC quota entries, less management overhead, more room for growth | Higher blast radius (share failure affects more PVCs), longer share creation time |
| Many small shares (500 GB-1 TB) | Lower blast radius, faster creation, finer-grained capacity control | Uses more VPC quota, more management overhead |

**Recommendation:** Start with 2 TB shares. This balances quota efficiency with blast radius. Increase to 4+ TB only if you're hitting the 300-share quota limit.

### maxShares Budget Planning

VPC accounts have a 300 file share quota. Budget across all consumers:

```
Total VPC shares available:     300
Stock IBM CSI driver shares:   - 50  (existing 1:1 PVCs)
Manually created shares:       - 10
Safety buffer:                 - 40
Available for pool CSI:         200

Pool allocations:
  general-purpose (maxShares):   50
  high-iops (maxShares):         20
  ci-cd (maxShares):             30
  Total pool CSI:               100  (within budget)
```

Check your current usage: `ibmcloud is shares --output json | jq length`

---

## Allocation Strategy Analysis

The pool manager supports two share selection strategies: `spread` and `binpack`. The strategy is set per pool in `spec.allocationStrategy`.

### Spread Strategy

**How it works:** When a new PVC is allocated, the manager selects the share with the most free capacity. PVCs are distributed evenly across all shares.

```
Share-1: [PVC-1] [PVC-4] [PVC-7]     ← 30% used
Share-2: [PVC-2] [PVC-5] [PVC-8]     ← 30% used
Share-3: [PVC-3] [PVC-6] [PVC-9]     ← 30% used
```

**Pros:**
- Lower blast radius — if one share fails, only ~1/N of PVCs are affected
- More even IOPS distribution across shares
- Better for long-lived workloads

**Cons:**
- More shares used at any given time
- Auto-expansion triggers earlier (all shares fill up together)

### Binpack Strategy

**How it works:** When a new PVC is allocated, the manager selects the share with the least free capacity that still fits the request. Shares are filled sequentially.

```
Share-1: [PVC-1] [PVC-2] [PVC-3] [PVC-4] [PVC-5]  ← 90% used
Share-2: [PVC-6] [PVC-7]                             ← 25% used
Share-3: (empty)                                      ← 0% used
```

**Pros:**
- Fewer shares in active use — saves VPC quota
- Easier to drain and decommission individual shares
- Better for ephemeral/CI workloads with high churn

**Cons:**
- Higher blast radius — one share holds most of the PVCs
- IOPS contention on the active share
- If the active share fails, many PVCs are affected

### When to Switch

| Signal | Action |
|--------|--------|
| Hitting 300-share quota | Switch to `binpack` to consolidate |
| Share failure affected too many PVCs | Switch to `spread` for isolation |
| CI/CD workloads with short PVC lifetimes | Use `binpack` — shares naturally drain |
| Production databases | Use `spread` — isolate blast radius |
| Mixed workloads | Create separate pools: `spread` for prod, `binpack` for CI |

### Monitoring Allocation Balance

```promql
# Share utilization variance (high variance = uneven, low = evenly spread)
stddev by (pool) (vpc_file_pool_share_allocated_gb) / avg by (pool) (vpc_file_pool_share_allocated_gb)

# PVC count per share (should be roughly equal for spread)
vpc_file_pool_share_pvc_count
```

---

## IOPS Planning

### VPC File Storage IOPS Model

IBM VPC file shares use the `dp2` profile by default, which provides IOPS based on share size:

| Share Size | Base IOPS (dp2) | Max IOPS (custom) |
|-----------|----------------|-------------------|
| 10-39 GB | 100 | 1000 |
| 40-79 GB | 100 | 2000 |
| 80-99 GB | 100 | 4000 |
| 100-499 GB | 100-1000 | 6000 |
| 500-999 GB | 500-3000 | 10000 |
| 1000-1999 GB | 1000-6000 | 20000 |
| 2000-3999 GB | 2000-12000 | 40000 |
| 4000-7999 GB | 4000-24000 | 48000 |
| 8000-15999 GB | 8000-48000 | 48000 |
| 16000-32000 GB | 16000-96000 | 96000 |

### IOPS Sharing Model

All PVCs on a share compete for the share's total IOPS. There is no per-PVC IOPS isolation.

**Example:** A 2 TB `dp2` share provides ~6000 IOPS. If 100 PVCs are on that share, each PVC gets an average of 60 IOPS — enough for configs and logs, but not enough for a database.

### Noisy Neighbor Risk

One PVC doing heavy sequential I/O can starve other PVCs on the same share.

**Mitigations:**
1. **Tiered pools:** Create separate pools for different IOPS requirements
2. **Spread strategy:** Distributes PVCs more evenly, reducing per-share contention
3. **Dedicated shares:** For truly IOPS-sensitive workloads, use the stock IBM CSI driver (1:1 PVC-to-share)

### Calculating Required IOPS

```
Total IOPS needed = Sum of (PVC_count × avg_IOPS_per_PVC)

Example:
  50 app configs at 5 IOPS each     =   250 IOPS
  20 logging PVCs at 50 IOPS each   = 1,000 IOPS
  5 database PVCs at 500 IOPS each  = 2,500 IOPS
  Total                              = 3,750 IOPS

→ A 2 TB dp2 share (6,000 IOPS) handles this with headroom
→ Put database PVCs on a separate high-IOPS pool for isolation
```

### Tiered Pools for IOPS Isolation

```yaml
# Standard pool — general workloads
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: standard-pool
spec:
  profile: dp2
  shareSizeGB: 2000          # ~6,000 IOPS per share
  allocationStrategy: spread
  # ...

---
# High-IOPS pool — databases, analytics
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: high-iops-pool
spec:
  profile: dp2
  shareSizeGB: 4000          # ~12,000 IOPS per share
  iops: 24000                # Custom IOPS (costs more)
  maxShares: 5               # Limit quota usage
  allocationStrategy: spread
  # ...
```

See `examples/tiered/` for complete examples.

---

## Capacity Planning

### expandThresholdPercent Tuning

This setting controls when the pool manager creates a new VPC file share. The tradeoff is safety vs. cost:

| Threshold | Behavior | Tradeoff |
|-----------|----------|----------|
| 60% | New share created when 60% allocated | Safer: more buffer for burst workloads. More expensive: shares sit partially empty. |
| 80% (default) | New share created when 80% allocated | Balanced: enough buffer for typical growth (30-90s to create a share). |
| 95% | New share created when 95% allocated | Cheaper: shares are well-utilized. Riskier: PVCs may fail if burst exceeds the 5% buffer during share creation. |

**Recommendation:**
- **80%** for production workloads with steady growth
- **70%** for workloads with unpredictable bursts (CI/CD, batch jobs)
- **90%** for stable workloads where PVC creation rate is slow and predictable

### initialShares for Burst Workloads

If you deploy many PVCs at once (e.g., a batch job that creates 50 PVCs), set `initialShares` high enough to absorb the burst without triggering expansion:

```yaml
spec:
  initialShares: 3            # Pre-create 3 shares
  shareSizeGB: 2000           # 6 TB total capacity from the start
  autoExpand: true
  expandThresholdPercent: 80
```

**Math:** If each PVC requests 10 GB and you expect an initial burst of 200 PVCs:
- Total burst: 200 × 10 GB = 2000 GB
- At 80% threshold, each 2 TB share absorbs ~1600 GB
- You need `initialShares: 2` minimum (1600 × 2 = 3200 GB > 2000 GB)
- Use `initialShares: 3` for safety

### Monitoring Queries for Capacity Planning

```promql
# Current pool utilization (should stay below expandThresholdPercent)
vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb * 100

# Allocation rate (PVCs per hour) — use for growth projection
rate(vpc_file_pool_allocations_total{status="success"}[1h]) * 3600

# Projected time to full (hours) at current allocation rate
(vpc_file_pool_capacity_gb - vpc_file_pool_allocated_gb)
/ (rate(vpc_file_pool_allocated_gb[1h]) * 3600)

# Share creation rate (should be rare — <1 per hour in steady state)
rate(vpc_file_pool_api_calls_total{operation="create_share"}[1h]) * 3600
```

---

## Node-Level Optimization

### Mount Caching

The CSI node agent maintains one NFS mount per share per node. Multiple PVCs on the same share reuse this single NFS mount via bind-mounts. This means:

- **First PVC on a node** from a given share: NFS mount (1-2 seconds)
- **Subsequent PVCs on the same node** from the same share: bind-mount only (~10 ms)

This is why the `spread` strategy can increase mount latency on large clusters — more shares means more NFS mounts per node. With `binpack`, a single share serves many PVCs, and most nodes only need one NFS mount.

### Worker Node NFS Client Tuning

For high-throughput workloads, increase the NFS client's TCP slot table on worker nodes:

```bash
# Check current value (default is 2)
cat /proc/sys/sunrpc/tcp_slot_table_entries

# Increase to 128 for higher concurrency (requires remount to take effect)
echo 128 > /proc/sys/sunrpc/tcp_slot_table_entries
```

This allows more concurrent NFS RPC calls per mount, which improves throughput for workloads with many parallel readers/writers.

**On ROKS/IKS:** Worker node tuning requires a DaemonSet with privileged access to modify sysctl settings. Example:

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: nfs-tuning
  namespace: kube-system
spec:
  selector:
    matchLabels:
      app: nfs-tuning
  template:
    metadata:
      labels:
        app: nfs-tuning
    spec:
      hostPID: true
      containers:
        - name: tuner
          image: busybox:latest
          command:
            - sh
            - -c
            - |
              sysctl -w sunrpc.tcp_slot_table_entries=128
              sleep infinity
          securityContext:
            privileged: true
```

### Security Group Optimization

Each worker node connects to NFS shares via the mount target IP. The VPC security group must allow:

| Direction | Protocol | Port | Source/Destination |
|-----------|----------|------|--------------------|
| Outbound | TCP | 2049 | Mount target IPs (or 0.0.0.0/0 within VPC) |
| Inbound | TCP | 2049 | Mount target IPs (for NFS callbacks) |

**Tip:** If you have many shares, create a single security group rule with the VPC CIDR (e.g., `10.240.0.0/16`) instead of individual IPs. This avoids hitting the security group rule limit (50 rules per group).

### Node Count and Share Fanout

Each share can have up to 64 mount targets. Each node that accesses a share creates one NFS connection to the mount target. Consider the math:

```
Nodes: 20
Shares per pool: 5
Strategy: spread

Each node potentially mounts all 5 shares = 100 mount target connections total
Each share has 20 node connections (well under the 64 limit)
```

With `binpack`, most PVCs are on 1-2 shares, so most nodes only connect to 1-2 mount targets — fewer connections, less NFS server overhead.

---

## Benchmarking

### fio Pod for Sequential I/O

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: fio-sequential
  namespace: default
spec:
  containers:
    - name: fio
      image: nixery.dev/fio
      command:
        - fio
        - --name=seqwrite
        - --rw=write
        - --bs=1M
        - --size=1G
        - --numjobs=1
        - --time_based
        - --runtime=60
        - --group_reporting
        - --filename=/data/testfile
        - --output-format=json
      volumeMounts:
        - name: data
          mountPath: /data
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
  restartPolicy: Never
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: bench-pvc
```

### fio Pod for Random I/O

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: fio-random
  namespace: default
spec:
  containers:
    - name: fio
      image: nixery.dev/fio
      command:
        - fio
        - --name=randread
        - --rw=randread
        - --bs=4K
        - --size=1G
        - --numjobs=4
        - --time_based
        - --runtime=60
        - --group_reporting
        - --iodepth=32
        - --filename=/data/testfile
        - --output-format=json
      volumeMounts:
        - name: data
          mountPath: /data
      securityContext:
        runAsUser: 1000
        runAsGroup: 1000
  restartPolicy: Never
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: bench-pvc
```

### Expected Performance Ranges

These ranges are for IBM VPC File Storage with the `dp2` profile, measured in-zone (same AZ as the share):

| Metric | Expected Range | Notes |
|--------|---------------|-------|
| Sequential read (1M blocks) | 100-500 MB/s | Depends on share size and IOPS profile |
| Sequential write (1M blocks) | 100-400 MB/s | Slightly lower than read due to NFS write semantics |
| Random read IOPS (4K blocks) | 1000-48000 | Depends on share IOPS allocation |
| Random write IOPS (4K blocks) | 1000-48000 | Similar to random read |
| Latency (random 4K read) | 1-5 ms | In-zone latency; cross-zone adds 1-3 ms |
| Metadata ops (create/stat/delete) | 1000-5000 ops/s | NFS metadata operations per mount |

### Interpreting Results

- **Below expected range:** Check security group rules, node NFS client settings, and share IOPS allocation
- **High latency variance:** May indicate noisy neighbors on the same share — check PVC count and IOPS usage per share
- **Throughput plateau:** You may be hitting the share's IOPS limit — check `ibmcloud is share <id>` for current IOPS

### Benchmarking Tips

1. **Warm up the mount** before benchmarking — the first I/O after mount is slower due to NFS connection setup
2. **Test with realistic block sizes** — 4K for databases, 1M for log/backup workloads
3. **Run from the same zone** as the share — cross-zone adds 1-3 ms latency
4. **Test with multiple jobs** (`--numjobs=4`) to measure concurrency limits
5. **Compare with stock CSI** — run the same fio test on a dedicated 1:1 PVC to establish the NFS baseline; the pool CSI should match since it uses the same NFS shares

### Benchmark PVC Setup

```bash
# Create a PVC for benchmarking
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: bench-pvc
  namespace: default
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 100Gi
EOF

# Wait for binding
kubectl get pvc bench-pvc -w

# Run the benchmark
kubectl apply -f fio-sequential.yaml
kubectl logs fio-sequential --follow

# Clean up
kubectl delete pod fio-sequential fio-random
kubectl delete pvc bench-pvc
```
