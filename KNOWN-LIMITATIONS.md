# Known Limitations

Explicit list of what the IBM VPC File Pool CSI Driver does not support, why, and available workarounds.

## Quota Enforcement

**What:** Per-PVC quotas are advisory only. The driver records a requested size in the SubVolume CR and reports it via `NodeGetVolumeStats`, but does not enforce a hard capacity limit per subdirectory.

**Why:** VPC file shares expose a single NFS filesystem. NFS does not support per-directory quotas, and the VPC API provides no mechanism for subdirectory-level enforcement. Linux project quotas (`xfs_quota`) are not available on NFS mounts.

**Workaround:** Monitor per-PVC usage via Prometheus metrics (`pool_csi_subvolume_used_bytes`) and configure soft quota alerts at the desired threshold. The `metrics.alerts.utilizationWarning` and `metrics.alerts.utilizationCritical` Helm values control alert thresholds at the pool level.

**Roadmap:** No change planned — this is a fundamental NFS/VPC limitation.

## Snapshots and Clones

**What:** Volume snapshots (`VolumeSnapshot`) and volume cloning (`dataSource` in PVC) are not implemented.

**Why:** Snapshotting a subdirectory within a shared NFS mount requires either filesystem-level snapshots (not exposed by VPC file shares) or a copy-on-write mechanism at the CSI layer.

**Workaround:** Use `rsync` or application-level backup tools to copy data from one PVC to another.

**Roadmap:** Phase 4a — planned for a future release.

## Volume Group Snapshots

**What:** Volume group snapshots (`VolumeGroupSnapshot`) are not implemented.

**Why:** Requires Kubernetes 1.32+ and depends on single-volume snapshot support (Phase 4a) as a prerequisite.

**Workaround:** None. Coordinate application-level quiesce and backup across PVCs manually.

**Roadmap:** Phase 4c — planned after single-volume snapshots.

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

## Manual Share Draining

**What:** There is no user-facing API or CLI command to drain all SubVolumes off a specific share (e.g., before maintenance or decommissioning).

**Why:** Draining requires migrating data between shares, which involves application-level coordination. The Pool Manager does not implement live data migration.

**Workaround:** Cordon the pool (set `maxShares: 0` or reduce capacity), wait for applications to be redeployed with new PVCs, then manually clean up old SubVolume CRs and delete the share.

**Roadmap:** Under consideration for a future release.

## Hard NFS Mounts

**What:** Deliberately not supported. The driver enforces `soft` NFS mounts.

**Why:** Hard NFS mounts cause pods to hang indefinitely if the NFS server becomes unreachable. With pooled storage, a single unresponsive share could freeze many pods simultaneously. Soft mounts return errors after the retry timeout, allowing applications to handle failures gracefully.

**Workaround:** None — this is a deliberate safety decision. Configure `timeo` and `retrans` mount options to adjust timeout behavior. The defaults (`timeo=600,retrans=3`) provide a 60-second timeout with 3 retries.

**Roadmap:** No change planned.

## VPC Account Quota

**What:** IBM Cloud VPC accounts are limited to 300 file shares per account. This quota is shared with the standard IBM VPC File CSI driver.

**Why:** This is an IBM Cloud platform limit, not a driver limitation.

**Workaround:** The pool driver significantly reduces share consumption by hosting many PVCs per share. A single pool with 5 shares can serve hundreds of PVCs. Request a quota increase via IBM Cloud support if needed.

**Roadmap:** No change planned — platform quota.
