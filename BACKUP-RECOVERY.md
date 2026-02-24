# Backup & Recovery Guide — IBM VPC File Pool CSI Driver

Data protection strategy using the driver's snapshot, clone, group snapshot, and replication features.

---

## Data Protection Options

| Method | RPO | RTO | Scope | Best For |
|--------|-----|-----|-------|----------|
| [Volume Snapshots](#volume-snapshots) | Point-in-time | Minutes (copy speed) | Single PVC | Regular backups of individual volumes |
| [Volume Clones](#volume-clones) | Point-in-time | Seconds (sync) to minutes (async) | Single PVC | Dev/test copies, pre-migration backup |
| [Group Snapshots](#group-snapshots) | Coordinated point-in-time | Minutes | Multiple PVCs | Multi-volume application backups |
| [Cross-Region Replication](#cross-region-replication) | Schedule-based (minutes to hours) | Manual failover | Full pool or label-selected | Disaster recovery |
| [Application-Level Backup](#application-level-backup) | Varies | Varies | Application-specific | Databases, VMs, crash-consistent workloads |

---

## Volume Snapshots

Directory-level point-in-time copies on the same NFS share. See [USER-GUIDE.md — Snapshots](USER-GUIDE.md#snapshots) for basic usage.

### Scheduled Snapshots with CronJob

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: snapshot-myapp-data
  namespace: default
spec:
  schedule: "0 */6 * * *"  # Every 6 hours
  concurrencyPolicy: Forbid
  jobTemplate:
    spec:
      template:
        spec:
          serviceAccountName: snapshot-creator
          containers:
            - name: snapshot
              image: bitnami/kubectl:latest
              command:
                - /bin/sh
                - -c
                - |
                  TIMESTAMP=$(date +%Y%m%d-%H%M%S)
                  cat <<SNAP | kubectl apply -f -
                  apiVersion: snapshot.storage.k8s.io/v1
                  kind: VolumeSnapshot
                  metadata:
                    name: myapp-data-${TIMESTAMP}
                    namespace: default
                    labels:
                      app: myapp
                      snapshot-type: scheduled
                  spec:
                    volumeSnapshotClassName: ibm-vpc-file-pool-snapclass
                    source:
                      persistentVolumeClaimName: myapp-data
                  SNAP
                  echo "Snapshot myapp-data-${TIMESTAMP} created"
          restartPolicy: OnFailure
---
# RBAC for the CronJob
apiVersion: v1
kind: ServiceAccount
metadata:
  name: snapshot-creator
  namespace: default
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: snapshot-creator
  namespace: default
rules:
  - apiGroups: ["snapshot.storage.k8s.io"]
    resources: ["volumesnapshots"]
    verbs: ["create", "get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: snapshot-creator
  namespace: default
subjects:
  - kind: ServiceAccount
    name: snapshot-creator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: snapshot-creator
```

### Snapshot Retention Script

Delete snapshots older than a retention period:

```bash
#!/bin/bash
# delete-old-snapshots.sh
# Usage: ./delete-old-snapshots.sh <namespace> <label-selector> <max-age-days>

NAMESPACE=${1:-default}
SELECTOR=${2:-snapshot-type=scheduled}
MAX_AGE_DAYS=${3:-7}

CUTOFF=$(date -u -d "${MAX_AGE_DAYS} days ago" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || \
         date -u -v-${MAX_AGE_DAYS}d +%Y-%m-%dT%H:%M:%SZ)

echo "Deleting snapshots older than ${MAX_AGE_DAYS} days (before ${CUTOFF})"

kubectl get volumesnapshots -n "$NAMESPACE" -l "$SELECTOR" \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.creationTimestamp}{"\n"}{end}' | \
while read name timestamp; do
  if [[ "$timestamp" < "$CUTOFF" ]]; then
    echo "Deleting snapshot: $name (created $timestamp)"
    kubectl delete volumesnapshot -n "$NAMESPACE" "$name"
  fi
done
```

Run as a CronJob for automated retention:

```yaml
apiVersion: batch/v1
kind: CronJob
metadata:
  name: snapshot-retention
  namespace: default
spec:
  schedule: "0 2 * * *"  # Daily at 2 AM
  jobTemplate:
    spec:
      template:
        spec:
          serviceAccountName: snapshot-creator
          containers:
            - name: cleanup
              image: bitnami/kubectl:latest
              command:
                - /bin/sh
                - -c
                - |
                  # Keep snapshots for 7 days
                  CUTOFF=$(date -u -d "7 days ago" +%Y-%m-%dT%H:%M:%SZ)
                  kubectl get volumesnapshots -n default -l snapshot-type=scheduled \
                    -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.creationTimestamp}{"\n"}{end}' | \
                  while read name ts; do
                    if [ "$ts" \< "$CUTOFF" ]; then
                      kubectl delete volumesnapshot -n default "$name"
                      echo "Deleted: $name"
                    fi
                  done
          restartPolicy: OnFailure
```

### Snapshot Performance

Snapshots are full directory copies (`cp -a`). Performance depends on data size:

| Data Size | Approximate Snapshot Time |
|-----------|--------------------------|
| 1 GB | 5-15 seconds |
| 10 GB | 30-90 seconds |
| 100 GB | 5-15 minutes |
| 1 TB | 1-2 hours |

Snapshot creation consumes share capacity equal to the source data size. Monitor pool capacity to ensure room for snapshots.

---

## Volume Clones

Full copies of existing PVCs. See [USER-GUIDE.md — Volume Cloning](USER-GUIDE.md#volume-cloning) and [VOLUME-CLONING.md](VOLUME-CLONING.md) for design details.

### Pre-Change Backup via Clone

Before a risky operation (upgrade, migration, schema change):

```bash
# 1. Create a clone of the data PVC
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: myapp-data-backup
  namespace: default
  labels:
    backup-reason: pre-upgrade
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 50Gi
  dataSource:
    kind: PersistentVolumeClaim
    name: myapp-data
EOF

# 2. Wait for clone to complete
kubectl get subvolumes myapp-data-backup -w
# Wait for cloneStatus to show "Complete"

# 3. Proceed with the risky operation
# ...

# 4. If rollback needed, swap PVC references in your deployment
# 5. Clean up the backup clone when no longer needed
kubectl delete pvc myapp-data-backup
```

### Clone from Snapshot (Point-in-Time Restore)

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: myapp-data-restored
  namespace: default
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 50Gi
  dataSource:
    name: myapp-data-20260221-060000   # Snapshot name
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
```

---

## Group Snapshots

Coordinated multi-PVC snapshots for applications with related volumes. See [USER-GUIDE.md — Volume Group Snapshots](USER-GUIDE.md#volume-group-snapshots) and [VOLUME-GROUP-SNAPSHOTS.md](VOLUME-GROUP-SNAPSHOTS.md) for design details.

### Application-Consistent Group Snapshot with Hooks

For applications that support quiesce/thaw operations:

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: mydb-consistent-snap
spec:
  poolName: general-purpose
  sourcePVCs:
    - pvcName: mydb-data
      pvcNamespace: default
    - pvcName: mydb-wal
      pvcNamespace: default
  failurePolicy: Abort
  preSnapshotHooks:
    - name: quiesce-db
      type: exec
      exec:
        podSelector:
          matchLabels:
            app: mydb
        container: db
        command: ["pg_ctl", "stop", "-m", "fast"]
      timeout: 30s
      onError: Abort
  postSnapshotHooks:
    - name: resume-db
      type: exec
      exec:
        podSelector:
          matchLabels:
            app: mydb
        container: db
        command: ["pg_ctl", "start"]
      timeout: 30s
      onError: Continue
```

### Best-Effort Group Snapshot (No Hooks)

For workloads where brief inconsistency is acceptable:

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: myapp-group-snap
spec:
  poolName: general-purpose
  sourcePVCs:
    - pvcName: myapp-data
      pvcNamespace: default
    - pvcName: myapp-config
      pvcNamespace: default
    - pvcName: myapp-logs
      pvcNamespace: default
  failurePolicy: Continue
```

The inconsistency window (time between first and last member copy) is tracked in `status.inconsistencyWindow`. For the smallest window, order PVCs from largest to smallest so the longest copy runs first.

---

## Cross-Region Replication

Schedule-based replication for disaster recovery. See [CROSS-REGION-DR.md](CROSS-REGION-DR.md) for architecture and consistency analysis.

### Setting Up DR

**Prerequisites:**
1. Transit Gateway connecting source and destination VPC regions
2. A `FileSharePool` in the destination region with shares mounted and accessible
3. The destination NFS server IP reachable from the source cluster

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: ReplicationPolicy
metadata:
  name: prod-to-dr
spec:
  sourcePool: production
  schedule: "15m"                    # Sync every 15 minutes
  incrementalSync: true              # Use rsync for delta-only transfer
  maxRetries: 3                      # Pause after 3 consecutive failures
  destination:
    nfsServer: "10.245.0.100"        # DR region mount target IP
    basePath: "/pvcs"                # Base path on destination share
  selector:                          # Optional: replicate only labeled SubVolumes
    matchLabels:
      backup: required
  preSyncHooks:                      # Optional: quiesce before sync
    - name: quiesce-apps
      type: http
      http:
        url: "http://myapp:8080/quiesce"
        method: POST
      timeout: 60s
      onError: Abort
  postSyncHooks:
    - name: resume-apps
      type: http
      http:
        url: "http://myapp:8080/resume"
        method: POST
      timeout: 60s
      onError: Continue
```

### Monitoring Replication Health

```bash
# Check replication status
kubectl get replicationpolicies -o wide

# Detailed status
kubectl get replicationpolicy prod-to-dr -o yaml

# PromQL: Current lag
# pool_csi_replication_lag_seconds{policy="prod-to-dr"}

# PromQL: Sync success rate
# rate(pool_csi_replication_sync_total{policy="prod-to-dr",result="success"}[1h])
```

### DR Failover Drill

Validate your DR readiness periodically:

```bash
# 1. Verify replication is current
kubectl get replicationpolicy prod-to-dr -o jsonpath='{.status.lastSyncTime}'

# 2. On the DR cluster, verify data exists
kubectl debug node/<dr-node> -it --image=busybox -- sh -c \
  "ls /var/data/kubelet/plugins/vpc-file-pool.csi.ibm.io/staging/<dr-share-id>/pvcs/"

# 3. Create test PVCs on the DR cluster pointing to replicated data
# 4. Deploy a test workload and verify data integrity
# 5. Clean up the test deployment
```

### Failover Procedure

Failover is manual by design (see [CROSS-REGION-DR.md](CROSS-REGION-DR.md) for why). Use the `kubectl failover` CLI plugin for structured DR promotion:

```bash
# 1. Stop replication on the source cluster to prevent partial syncs

# 2. Review what will be failed over and check RPO
kubectl failover plan --nfs-mount-path /repl/dst/10.245.3.8

# 3. Dry-run to preview resources that will be created
kubectl failover execute --nfs-mount-path /repl/dst/10.245.3.8 \
  --dr-pool dr-production --dr-share-ip 10.245.3.8 --dry-run

# 4. Execute failover — creates SubVolume CRs, PVs, and PVCs on the DR cluster
kubectl failover execute --nfs-mount-path /repl/dst/10.245.3.8 \
  --dr-pool dr-production --dr-share-ip 10.245.3.8

# 5. Verify PVC binding
kubectl failover status

# 6. Deploy workloads on the DR cluster
# 7. Update DNS/load balancer to point to the DR cluster
```

See [CROSS-REGION-DR.md — Failover CLI](CROSS-REGION-DR.md#failover-cli) for full flag reference and details.

---

## Application-Level Backup

For workloads that require crash consistency beyond what NFS directory copies provide.

### Databases

| Database | Recommended Backup Method |
|----------|--------------------------|
| PostgreSQL | `pg_dump` or `pg_basebackup` + WAL archiving |
| MySQL | `mysqldump` or `xtrabackup` |
| MongoDB | `mongodump` or filesystem snapshots with `db.fsyncLock()` |

Pool CSI snapshots can supplement application-level backups for non-critical data (configs, logs).

### KubeVirt VMs

**Do not snapshot or replicate running VM disk images.** The disk image will be in an inconsistent state.

For VM backup:
1. **Stop the VM** with `virtctl stop <vm-name>`
2. **Create a volume snapshot** of the disk PVC
3. **Restart the VM** with `virtctl start <vm-name>`

For ongoing VM protection, use application-level backup tools (e.g., Velero with KubeVirt plugin, Trilio) or shut down VMs before replication cycles via pre-sync hooks.

---

## Strategy Recommendations by Workload Type

| Workload | Primary Protection | Secondary Protection | Notes |
|----------|-------------------|---------------------|-------|
| **Stateless apps** | None needed | — | Redeploy from source |
| **Config data** | Scheduled snapshots (daily) | Cross-region replication | Low RPO tolerance |
| **Application data** | Scheduled snapshots (6h) + clone before changes | Cross-region replication | Label-select critical PVCs |
| **Databases** | Application-level backup (pg_dump, etc.) | Pool snapshots as secondary | Never rely solely on NFS snapshots |
| **KubeVirt VMs** | Stop VM + snapshot | Application-level backup | Never snapshot running VMs |
| **ML training data** | Group snapshots (model + checkpoints) | Cross-region for final models | Intermediate checkpoints are expendable |
| **CI/CD artifacts** | None or short-lived snapshots | — | Easily reproducible |

---

## Recovery Runbooks

### Single PVC Recovery

```bash
# 1. Find available snapshots
kubectl get volumesnapshots -n <namespace> -l app=<app-name>

# 2. Restore from snapshot
kubectl apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: <pvc-name>-restored
  namespace: <namespace>
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: <original-size>
  dataSource:
    name: <snapshot-name>
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
EOF

# 3. Update application to use the restored PVC
# 4. Verify data integrity
# 5. Delete the old PVC if the restored one is confirmed good
```

### Full Pool Recovery

If an entire pool is lost (all shares destroyed):

```bash
# 1. Recreate the FileSharePool CR
kubectl apply -f pool-backup.yaml

# 2. Wait for shares to be provisioned
kubectl get filesharepools -w

# 3. Restore SubVolumes from the CRD backup
#    (This restores tracking metadata only — data must come from DR or backups)

# 4. If cross-region replication was configured:
#    - Switch to DR cluster (see "Failover Procedure" above)
#    OR
#    - Reverse-replicate from DR back to the recovered pool
```

### Cross-Region Failover Recovery

See the [DR Failover Procedure](#failover-procedure) section above.

---

## See Also

- [USER-GUIDE.md](USER-GUIDE.md) — Snapshot, clone, and group snapshot usage
- [VOLUME-CLONING.md](VOLUME-CLONING.md) — Clone design and sync/async modes
- [VOLUME-GROUP-SNAPSHOTS.md](VOLUME-GROUP-SNAPSHOTS.md) — Group snapshot consistency model
- [CROSS-REGION-DR.md](CROSS-REGION-DR.md) — Replication architecture and constraints
- [CONSISTENCY-MODEL.md](CONSISTENCY-MODEL.md) — NFS consistency guarantees
- [MONITORING.md](MONITORING.md) — Snapshot, clone, and replication metrics
