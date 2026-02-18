# Known Limitations

Explicit list of what the IBM VPC File Pool CSI Driver does not support, why, and available workarounds.

## Quota Enforcement

**What:** Per-PVC quotas are advisory only. The driver records a requested size in the SubVolume CR and reports it via `NodeGetVolumeStats`, but does not enforce a hard capacity limit per subdirectory.

**Why:** VPC file shares expose a single NFS filesystem. NFS does not support per-directory quotas, and the VPC API provides no mechanism for subdirectory-level enforcement. Linux project quotas (`xfs_quota`) are not available on NFS mounts.

**Workaround:** Monitor per-PVC usage via Prometheus metrics (`pool_csi_subvolume_used_bytes`) and configure soft quota alerts at the desired threshold. The `metrics.alerts.utilizationWarning` and `metrics.alerts.utilizationCritical` Helm values control alert thresholds at the pool level.

**Roadmap:** No change planned — this is a fundamental NFS/VPC limitation.

## Snapshots and Clones

**What:** Volume snapshots (`VolumeSnapshot`) and volume cloning (`dataSource` in PVC) are implemented via directory-level copies on the NFS share. Snapshots are created using `cp -a` from the source subdirectory to a `.snapshots/` directory on the same share. Clones use a similar copy mechanism with two paths:

- **Synchronous clones:** For small volumes (configurable threshold, default 10 GB) on the same share, the copy completes inline during `CreateVolume`.
- **Asynchronous clones:** For large volumes or cross-share clones, the SubVolume CR is created with `cloneStatus=Pending` and the background worker completes the copy. `NodePublishVolume` gates mount until the clone is `Complete`.

**Limitations:**
- Snapshots and clones are not instantaneous — they are full data copies proportional to the source volume size.
- Cross-share clones require the async path (no same-share optimization).
- Per-PVC quota enforcement limitations (see above) also apply to cloned volumes.
- No incremental or COW snapshots — each snapshot is a full copy.

**Workaround:** For very large volumes where copy time is unacceptable, consider application-level backup/restore tools.

**Roadmap:** Phase 4a (snapshots) and Phase 4b (clones) are complete.

## Volume Group Snapshots

**What:** Volume group snapshots (`VolumeGroupSnapshot`) are implemented. The driver supports coordinated multi-PVC snapshots via `CreateVolumeGroupSnapshot` and `DeleteVolumeGroupSnapshot` CSI RPCs.

**How it works:** Group snapshots iterate through the source PVCs sequentially, creating individual snapshots for each member. The order can be controlled via the `csi.ibm.com/copy-order` parameter (comma-separated PVC names). Two failure policies are supported:

- **Abort (default):** If any member snapshot fails, all previously completed snapshots are rolled back and the operation returns an error.
- **Continue:** If a member snapshot fails, the operation continues with remaining members and returns a `PartialFailure` status. Successfully completed snapshots are preserved.

**Limitations:**
- Snapshots are sequential, not atomic — there is an inconsistency window between the first and last member snapshot (tracked in `status.inconsistencyWindow`).
- Each member snapshot is a full directory copy (same as single-volume snapshots), so total time is proportional to the combined size of all source volumes.
- All source PVCs must belong to the same pool.
- Requires Kubernetes 1.32+ for the external VolumeGroupSnapshot API (the CSI-level RPCs work with any Kubernetes version).

**Workaround:** For applications requiring point-in-time consistency across volumes, quiesce writes at the application level before triggering the group snapshot.

**Roadmap:** Phase 4c — complete.

## Cross-Region Replication and Disaster Recovery

**What:** The driver includes a built-in replication controller (Phase 4d) for cross-region disaster recovery. A `ReplicationPolicy` CRD defines which SubVolumes to replicate, the destination NFS server (reachable over Transit Gateway), and a schedule interval. The controller periodically copies SubVolume data from the source pool to the destination using `cp -a` (CopyDir).

**How it works:** The replication controller runs as a background goroutine alongside the CSI controller. For each active `ReplicationPolicy` on schedule:
1. Lists SubVolumes in the source pool (optionally filtered by label selector)
2. Copies each matched SubVolume's directory to the destination NFS server
3. Updates per-SubVolume and per-policy replication status
4. Tracks consecutive failures and pauses the policy after exceeding `maxRetries`

**Limitations:**
- **File-level consistency only** (without hooks). Each individual file is consistent (NFS close-to-open semantics), but cross-file consistency is NOT guaranteed unless pre/post-sync hooks are configured to quiesce application writes. See `CROSS-REGION-DR.md` for detailed consistency analysis.
- **Not suitable for VMs, databases, or workloads requiring crash consistency.** Actively written disk images (qcow2, vmdk, raw) and databases with WALs will produce corrupted replicas. Use IBM VPC block storage replication or application-level replication for these workloads.
- **Requires pre-configured cross-region network connectivity** (Transit Gateway or VPN) and a pre-existing destination FileSharePool.
- **Failover is manual.** Automated failover is explicitly out of scope due to limited consistency guarantees.

**Best suited for:** Static assets, model weights, log aggregation, workloads with non-zero RPO tolerance, and workloads that support quiesce hooks for application-consistent replication.

**Workaround for unsupported workloads:** Use application-level replication (PostgreSQL streaming replication, MySQL Group Replication, etc.) or IBM VPC block storage snapshots for crash-consistent DR.

**Roadmap:** Phase 4d (replication) and Phase 5 (hooks + incremental rsync) — implemented. Future enhancements: bandwidth limiting, parallel rsync.

## statfs Reports Share-Level Stats

**What:** `df` and `statfs` calls inside a pod report the total capacity and usage of the entire NFS share, not the individual subdirectory.

**Why:** This is an NFS protocol limitation. NFS `statfs` returns filesystem-level statistics; there is no per-directory equivalent.

**Workaround:** Use `NodeGetVolumeStats` (exposed via `kubectl describe pvc`) for accurate per-PVC usage based on `du`. Prometheus metrics also report per-SubVolume usage.

**Roadmap:** No change planned — NFS protocol limitation.

## Migration from Stock CSI Driver

**What:** Migrating from the standard IBM VPC File CSI driver (1:1 PVC-to-share mapping) to the pool CSI driver requires data copying because the two drivers use fundamentally different storage models.

**Why:** Kubernetes does not support in-place changes to a PV's CSI driver. Migration requires creating new PVCs on the pool driver and copying data from the old PVCs.

**Tool:** The `kubectl-migrate` CLI tool automates the migration workflow:

```bash
# Build the migration tool
make build-migrate

# 1. Plan: discover PVCs to migrate
kubectl migrate plan --namespace default --storage-class ibm-vpc-file-5iops --target-pool general-purpose

# 2. Execute: migrate a specific PVC (creates new PVC, rsync data, verify)
kubectl migrate execute --namespace default --pvc my-data --target-pool general-purpose --target-storage-class ibm-vpc-file-pool

# 3. Status: check migration progress
kubectl migrate status --namespace default
```

The tool creates a new PVC on the pool driver, launches a temporary pod that rsyncs data from the old PVC to the new one, and verifies file counts. The old PVC is NOT deleted automatically — you switch your workloads to the new PVC and delete the old one when ready.

**Limitations:**
- Migration speed depends on data volume and NFS throughput.
- Applications must tolerate brief downtime or support live migration at the application layer.
- The tool does not handle StatefulSet ordinal PVC naming automatically — migrate each PVC individually.

## IBM Cloud Satellite

**What:** Not supported. The driver requires IBM Cloud VPC infrastructure.

**Why:** Cloud Satellite locations do not have access to the VPC file share API. Satellite uses different storage backends (local disks, NetApp, etc.).

**Workaround:** Use the storage drivers provided by IBM Cloud Satellite for your location's infrastructure.

**Roadmap:** No support planned.

## VPC Classic Infrastructure

**What:** Not supported. The driver requires VPC Gen2 infrastructure.

**Why:** VPC Classic (Gen1) uses a different file storage API that is not compatible with the `vpc-go-sdk` file share operations used by this driver.

**Workaround:** Migrate to VPC Gen2 infrastructure. IBM has deprecated VPC Classic.

**Roadmap:** No support planned — VPC Classic is deprecated.

## Share Draining (Graceful Evacuation)

**What:** The driver supports graceful share draining via `spec.drainShares`. Adding a share ID to this list marks it as "draining," which excludes it from new allocations. The reconciler tracks drain progress in `status.drainStatus`, reporting remaining SubVolumes per share. Once all SubVolumes are removed (via application redeployment with new PVCs or manual PVC deletion), the share is reported as fully drained.

**Note:** Draining prevents new allocations to the share but does not automatically migrate existing data. Applications must be redeployed or PVCs manually deleted to complete the drain.

**Usage:**
```yaml
spec:
  drainShares:
    - "r006-share-to-drain"
```

Monitor progress via `status.drainStatus` or the `DrainComplete` condition.

## Hard NFS Mounts

**What:** The driver defaults to `soft` NFS mounts. Custom mount flags from the StorageClass are merged with the safe defaults (`nfsvers=4.1,soft,timeo=600,retrans=3`), so `soft` is always present unless the StorageClass explicitly includes `hard`.

**Why:** Hard NFS mounts cause pods to hang indefinitely if the NFS server becomes unreachable. With pooled storage, a single unresponsive share could freeze many pods simultaneously. Soft mounts return errors after the retry timeout, allowing applications to handle failures gracefully.

**Override:** If your workload requires `hard` mounts (e.g., applications that must not see transient I/O errors), add `hard` to the StorageClass `mountOptions`. This will replace the `soft` default. Use with caution — a VPC file share outage will cause all pods using hard-mounted shares to hang until the share recovers.

**Safe customization:** Adding options like `noatime`, `rsize=1048576`, or `timeo=300` to the StorageClass will merge with the defaults without removing `soft`. Only `hard` explicitly replaces `soft`.

**Roadmap:** No change planned — `soft` remains the recommended and default setting.

## Cross-Zone Node Failover

**What:** Cross-zone accessor bindings (`spec.accessorZones`) create mount targets in multiple VPC zones, but the node agent always selects the mount target IP matching the node's own zone. If all nodes in a zone are lost, pods rescheduled to a different zone will use that zone's mount target IP (if configured). There is no automatic failover to a different zone's mount target for an already-mounted share.

**Why:** NFS mount targets are zone-local. A mount target in `us-south-1` is only reachable from nodes in `us-south-1`. The driver selects the correct zone IP at mount time, not dynamically at runtime.

**Workaround:** Configure `accessorZones` for all zones where worker nodes may be scheduled. This ensures mount targets exist in every zone before pods arrive.

**Roadmap:** No change planned — this is by design. Zone-local mount targets are a VPC networking constraint.

## CDI (Containerized Data Importer) Compatibility

**What:** KubeVirt's CDI `dataVolumeTemplate` mechanism does not work with the pool CSI driver. VMs that use `dataVolumeTemplate` to clone OS images from golden image sources (DataSources, VolumeSnapshots) will fail with the boot PVC stuck in `Pending`.

**Why:** CDI's VolumePopulator sets a `dataSource` field (VolumeCloneSource or VolumeImportSource) on the PVC it creates. The `csi-provisioner` sidecar sees this unrecognized `dataSource` and backs off, assuming an external populator will handle provisioning. CDI's populator then cannot complete because it does not support the pool CSI driver's provisioning interface. The same conflict occurs for blank DataVolumes (`source: blank`). This affects both boot disks (OS image clones) and data disks (blank volumes) when created via `dataVolumeTemplate`.

On managed ROKS/IKS clusters, CDI's golden OS images are typically stored as ODF/Ceph VolumeSnapshots. CDI knows how to clone these to other Ceph-backed StorageClasses but not to NFS-backed pool storage.

**Workaround:** Bypass CDI entirely by creating regular PVCs on the pool StorageClass and populating them manually:

1. Create PVCs directly (not via `dataVolumeTemplate`)
2. For boot disks, download cloud images (qcow2) into the PVC using a simple pod:
   ```bash
   # Download CentOS Stream 9 cloud image into the boot PVC
   curl -L -o /boot/disk.img \
     https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2
   ```
3. Reference the PVCs in the VM spec using `persistentVolumeClaim` (not `dataVolume`)

KubeVirt supports filesystem-mode PVCs for VM disks — it reads the `disk.img` file from the mounted NFS share. See `TUTORIAL.md` for a complete worked example.

**Roadmap:** Could be addressed by implementing CSI VolumeSnapshot support (which would enable CDI's clone populator path), but this is not currently planned. The manual image download workaround is straightforward and avoids the CDI dependency entirely.

## VPC Account Quota

**What:** IBM Cloud VPC accounts are limited to 300 file shares per account. This quota is shared with the standard IBM VPC File CSI driver.

**Why:** This is an IBM Cloud platform limit, not a driver limitation.

**Workaround:** The pool driver significantly reduces share consumption by hosting many PVCs per share. A single pool with 5 shares can serve hundreds of PVCs. Request a quota increase via IBM Cloud support if needed.

**Roadmap:** No change planned — platform quota.
