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

**What:** Volume group snapshots (`VolumeGroupSnapshot`) are not implemented.

**Why:** Requires Kubernetes 1.32+ and the VolumeGroupSnapshot API. Single-volume snapshot support (Phase 4a) is now complete as a prerequisite.

**Workaround:** None. Coordinate application-level quiesce and backup across PVCs manually.

**Roadmap:** Phase 4c — planned for a future release. Prerequisites (Phase 4a/4b) are complete.

## Cross-Region Replication and Disaster Recovery

**What:** No built-in disaster recovery or cross-region replication for pooled volumes.

**Why:** VPC file shares do not currently support cross-region replication. DR would require an external data sync mechanism.

**Workaround:** Use application-level replication (e.g., database replication) or external tools like `rsync` with cron jobs across regions.

**Roadmap:** Phase 4d — planned for investigation in a future release.

## statfs Reports Share-Level Stats

**What:** `df` and `statfs` calls inside a pod report the total capacity and usage of the entire NFS share, not the individual subdirectory.

**Why:** This is an NFS protocol limitation. NFS `statfs` returns filesystem-level statistics; there is no per-directory equivalent.

**Workaround:** Use `NodeGetVolumeStats` (exposed via `kubectl describe pvc`) for accurate per-PVC usage based on `du`. Prometheus metrics also report per-SubVolume usage.

**Roadmap:** No change planned — NFS protocol limitation.

## No Migration Tool from Stock CSI Driver

**What:** There is no automated migration path from the standard IBM VPC File CSI driver (1:1 PVC-to-share mapping) to the pool CSI driver.

**Why:** The two drivers use fundamentally different storage models. Migrating requires moving data from standalone shares into subdirectories on pooled shares.

**Workaround:** Create new PVCs on the pool driver and `rsync` data from old PVCs:

```bash
# From a pod with both PVCs mounted:
rsync -avz /old-pvc/ /new-pvc/
```

**Roadmap:** A migration guide is planned; automated tooling is not on the current roadmap.

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

**What:** Deliberately not supported. The driver enforces `soft` NFS mounts.

**Why:** Hard NFS mounts cause pods to hang indefinitely if the NFS server becomes unreachable. With pooled storage, a single unresponsive share could freeze many pods simultaneously. Soft mounts return errors after the retry timeout, allowing applications to handle failures gracefully.

**Workaround:** None — this is a deliberate safety decision. Configure `timeo` and `retrans` mount options to adjust timeout behavior. The defaults (`timeo=600,retrans=3`) provide a 60-second timeout with 3 retries.

**Roadmap:** No change planned.

## Cross-Zone Node Failover

**What:** Cross-zone accessor bindings (`spec.accessorZones`) create mount targets in multiple VPC zones, but the node agent always selects the mount target IP matching the node's own zone. If all nodes in a zone are lost, pods rescheduled to a different zone will use that zone's mount target IP (if configured). There is no automatic failover to a different zone's mount target for an already-mounted share.

**Why:** NFS mount targets are zone-local. A mount target in `us-south-1` is only reachable from nodes in `us-south-1`. The driver selects the correct zone IP at mount time, not dynamically at runtime.

**Workaround:** Configure `accessorZones` for all zones where worker nodes may be scheduled. This ensures mount targets exist in every zone before pods arrive.

**Roadmap:** No change planned — this is by design. Zone-local mount targets are a VPC networking constraint.

## VPC Account Quota

**What:** IBM Cloud VPC accounts are limited to 300 file shares per account. This quota is shared with the standard IBM VPC File CSI driver.

**Why:** This is an IBM Cloud platform limit, not a driver limitation.

**Workaround:** The pool driver significantly reduces share consumption by hosting many PVCs per share. A single pool with 5 shares can serve hundreds of PVCs. Request a quota increase via IBM Cloud support if needed.

**Roadmap:** No change planned — platform quota.
