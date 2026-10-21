# Known Limitations

Explicit list of what the IBM VPC File Pool CSI Driver does not support, why, and available workarounds.

## Quota Enforcement

**What:** Per-PVC quotas are advisory only. The driver records a requested size in the SubVolume CR and reports it via `NodeGetVolumeStats`, but does not enforce a hard capacity limit per subdirectory.

**Why:** VPC file shares expose a single NFS filesystem. NFS does not support per-directory quotas, and the VPC API provides no mechanism for subdirectory-level enforcement. Linux project quotas (`xfs_quota`) are not available on NFS mounts.

**Workaround:** Monitor per-PVC usage via Prometheus metrics (`pool_csi_subvolume_used_bytes`) and configure soft quota alerts at the desired threshold. The `metrics.alerts.utilizationWarning` and `metrics.alerts.utilizationCritical` Helm values control alert thresholds at the pool level.

**Roadmap:** No change planned — this is a fundamental NFS/VPC limitation.

## Snapshots and Clones

**What:** Volume snapshots (`VolumeSnapshot`) and volume cloning (`dataSource` in PVC) are implemented via directory-level copies on the NFS share. The controller has no NFS access, so a Kubernetes Job in the driver's namespace does each copy: `cp -a` from the source subdirectory to a `.snapshots/` directory on the same share, and back again for a restore. A restored claim stays Pending until its copy has finished. Clones use a similar copy mechanism, and every clone is asynchronous: the SubVolume CR is created with `cloneStatus=Pending`, and the clone worker runs a Kubernetes Job that completes the copy. `NodePublishVolume` gates mount until the clone is `Complete`.

**Limitations:**
- Snapshots and clones are not instantaneous — they are full data copies proportional to the source volume size.
- A snapshot of a volume that is being written is not one point in time: files can change while the copy runs. Stop or freeze the workload before the snapshot if you need a consistent copy. The exception is a disk that a running KubeVirt VM uses: that snapshot is a QEMU copy at one moment, with the guest frozen for a moment (see below).
- A snapshot of a running VM is made for each disk (each claim) as KubeVirt asks for it, but the driver copies all the pool disks of the VM at one moment, so the snapshots of the disks fit each other. A snapshot of one disk of a multi-disk VM therefore also copies the other disks to temporary files; the files of a disk that no snapshot claims within 10 minutes are deleted, and each share needs room for them. A disk that is not on a pool is not part of the moment. The copy runs at 20 MB/s by default, so a 20 GB disk takes about 20 minutes; set `vmCopySpeedLimitMBps` on the VolumeSnapshotClass to change that. Red Hat does not support the QEMU commands this uses.
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

**Not working on ROVS (tested 19 Oct 2026, ROVS 4.21):** nothing calls these RPCs. The chart runs `csi-snapshotter` without group snapshots turned on, and that sidecar only knows the upstream `groupsnapshot.storage.k8s.io` API. OpenShift has its own `groupsnapshot.storage.openshift.io` API, and only on clusters with a non-default feature set. See GitHub issue #14.

**Roadmap:** Phase 4c code is complete; making it reachable on OpenShift is issue #14.

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
kubectl migrate execute --namespace default --pvc my-data --target-pool general-purpose --target-storage-class general-purpose

# 3. Status: check migration progress
kubectl migrate status --namespace default
```

The tool creates a new PVC on the pool driver, launches a temporary pod that rsyncs data from the old PVC to the new one, and compares file counts. The new PVC is named `<old name>-pooled`. A pool's StorageClass has the same name as the pool, so `--target-storage-class` is the pool name. If the file counts differ, the pod fails and the tool reports the migration as failed. The old PVC is NOT deleted automatically — you switch your workloads to the new PVC and delete the old one when ready.

**Limitations:**
- The tool finds the cluster like `kubectl`: the `--kubeconfig` flag, then the `KUBECONFIG` variable, then `~/.kube/config`.
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

**What:** VMs come from the standard OpenShift Virtualization catalog. You pick a catalog image and choose the pool's StorageClass for the boot disk. CDI copies the image onto the pool. CDI registry import also works with the pool CSI driver.

**Measured on rovslab, 20 October 2026:**

- CDI's copy pods have a default memory limit of 600 MB. A copy of RHEL 9 from Ceph onto a pool was killed for lack of memory, 3 of 3 times (also onto IBM VPC File). With a 2 GB limit it finished in 77 s.
- A 25 GB copy from Ceph with a 2Gi limit was killed 6 times and never finished. With an 8Gi limit it finished in 134 s and the checksum matched. Memory sat at the limit, so a much larger disk is not proven.
- A VM from the standard RHEL 9 catalog image onto a pool had its guest agent after 103 s.
- A CDI DataVolume with a registry source, on a pool StorageClass, imported CentOS Stream 9 in about 40 seconds and RHEL 9 in about 2.5 minutes.

Earlier versions of this document said that CDI is incompatible with this driver, and later that a copy from Ceph onto a pool still fails. Both statements are out of date.

**Copy pod memory limit:** The controller keeps the memory limit of CDI's copy pods at least 8Gi, through `HyperConverged` `spec.resourceRequirements.storageWorkloads` (the setting Red Hat documents). It checks at start-up and every 5 minutes. If `storageWorkloads` is not set, it sets CDI's defaults with the memory raised (limits CPU 750m and memory 8Gi, requests CPU 100m and memory 60M). If it is set, the controller changes only `limits.memory`. It never lowers a limit. Each raise logs a warning and adds 1 to `vpc_file_pool_cdi_copy_limit_raised_total`. Helm value: `cdi.copyPodMemoryLimit`.

**ROVS caveat:** IBM says the virtualization add-on is pre-configured and cannot be modified, and IBM's add-on creates the `HyperConverged` object. The driver changes one field of it. After an add-on update, check the metric and the controller log for a new warning, and check the limit again (see [TROUBLESHOOTING.md](TROUBLESHOOTING.md#cdi-copy-pod-oomkilled-when-a-vm-disk-is-copied-onto-a-pool)). Where changing `HyperConverged` is not allowed, set `cdi.copyPodMemoryLimit=""`. Then catalog copies of large disks onto the pool fail.

**Golden images (hidden):** The pool's golden image syncer uses CDI registry import. It is kept for possible later use, and the console pool form no longer offers it. The standard catalog is the recommended way. See [GOLDEN-IMAGE-WORKFLOW.md](GOLDEN-IMAGE-WORKFLOW.md).

> **Important:** KubeVirt requires **raw format** disk images for filesystem-mode PVCs. It passes them to QEMU with `"driver":"file"` (no qcow2 format layer). If a qcow2 image is written to the PVC without conversion, the VM shows "No bootable device". CDI converts the image automatically. Convert by hand (`qemu-img convert -f qcow2 -O raw`) only if you write images to the pool yourself.

**StorageProfile:** The controller automatically patches CDI's `StorageProfile` CR for each pool StorageClass, setting `claimPropertySets: [{accessModes: ["ReadWriteMany"], volumeMode: Filesystem}]` and `cloneStrategy: csi-clone`, so CDI asks the driver to make clones. CDI's snapshot clone does not work with this driver, and CDI's host-assisted copy pods need a memory limit above their 600 MB default when they write onto NFS (see above). The patching is non-destructive (only sets fields if empty) and non-fatal (errors are logged but don't block reconciliation). CDI detection is cached — if CDI is not installed, the check runs once and is skipped on subsequent reconciles.

**Golden Image Automation (hidden):** The driver includes a golden image syncer that automates OS image provisioning for KubeVirt. Two modes are supported:

1. **Native CDI mode**: Make the pool StorageClass the cluster default. CDI then imports and snapshots golden images on pool storage.
2. **Custom syncer mode**: When another StorageClass is the default, enable `spec.goldenImages` on the FileSharePool. The controller discovers CDI `DataImportCron` images and imports each one with a CDI DataVolume on the pool StorageClass. It also creates CDI `DataSource` CRs (for the InstanceTypes catalog tab) and OpenShift `Template` CRs (for the Templates catalog tab). Creating a VM from the InstanceTypes tab with its boot disk on the pool is being tested.

See `GOLDEN-IMAGE-WORKFLOW.md` for setup instructions.

## KubeVirt NFS Permissions (root_squash)

**What:** KubeVirt's virt-handler calls `chown(mountDir, 107, 107)` on every filesystem-mode PVC mount. IBM Cloud VPC file shares enforce NFS `root_squash` — root (UID 0) is mapped to nobody (UID 65534). Without `sec=sys`, the NFS mount defaults to `sec=null` (anonymous auth) where chown fails for all UIDs and VMs cannot start.

**Why:** VPC file shares in security-group mode default-negotiate to `sec=null` unless the client explicitly requests `sec=sys` (AUTH_UNIX). With `sec=null`, all operations are anonymous (UID 99), chown is impossible, and virt-handler fails with `preparing host-disks failed: chown ... operation not permitted`.

**Fix:** The CSI driver mounts with `sec=sys` in its default NFS mount options (matching the stock IBM VPC File CSI driver). With `sec=sys`, standard Unix UID/GID credentials are sent in NFS RPCs, root_squash maps only UID 0, and chown works for non-root UIDs.

Configure the FileSharePool for KubeVirt:
```yaml
spec:
  defaultPermissions: "0777"
  defaultUID: 107
  defaultGID: 107
```

If you write boot disk images to the pool yourself, convert qcow2 to raw format first (see [CDI Compatibility](#cdi-containerized-data-importer-compatibility) above). CDI does this for you when it imports an image. For a manual conversion, use `quay.io/centos/centos:stream9` which provides `qemu-img` via `dnf install`. The pod runs as root to install packages; due to NFS root_squash the raw file is created as UID 65534. Use `chmod 0666` so the QEMU user (UID 107) can read and write the image.

**Note:** If mounting with custom StorageClass `mountOptions`, always include `sec=sys`. Omitting it causes NFS to negotiate `sec=null`, breaking chown and KubeVirt.

**Roadmap:** Fixed in v0.10.0 (`sec=sys` mount option). See `TUTORIAL.md` for a complete worked example.

## NFS Encryption in Transit

**What:** IBM Cloud VPC File Storage supports optional encryption in transit (IPsec) for NFS connections, but this feature is **not available** on bare metal servers or RHCOS-based virtual server instances.

**Why:** IBM Cloud's encryption in transit implementation uses IPsec between the compute host and the file share mount target. This mechanism is not supported on:

- **Bare metal servers** — IPsec for file shares is not available on VPC bare metal.
- **RHCOS-based virtual server instances** — Red Hat Enterprise Linux CoreOS (used by ROKS, ROVS and other OpenShift worker nodes) is explicitly unsupported.
- **Only supported on** regular VPC virtual server instances running non-RHCOS Linux distributions.

Since ROKS and ROVS worker nodes run RHCOS, encryption in transit cannot be enabled for this driver on ROKS or ROVS clusters. NFS traffic travels unencrypted within the VPC network.

**Mitigations:**
- Data at rest is always encrypted (provider-managed or customer-managed keys via Key Protect / HPCS).
- VPC network traffic is isolated to your VPC — it does not traverse the public internet.
- Use VPC security groups to restrict NFS traffic (TCP 2049) to only your worker node subnets (see `SECURITY.md` hardening checklist).

**Roadmap:** No change planned — this is an IBM Cloud platform limitation. Monitor the [VPC file storage documentation](https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-vpc-eit) for updates to supported compute types.

## VPC Account Quota

**What:** IBM Cloud VPC accounts are limited to 300 file shares per account. This quota is shared with the standard IBM VPC File CSI driver.

**Why:** This is an IBM Cloud platform limit, not a driver limitation.

**Workaround:** The pool driver significantly reduces share consumption by hosting many PVCs per share. A single pool with 5 shares can serve hundreds of PVCs. Request a quota increase via IBM Cloud support if needed.

**Roadmap:** No change planned — platform quota.

## Backups to a Snapshot Pool

**What:** a `BackupPolicy` backs up selected PVCs and VMs into a snapshot pool (see `BACKUP-RECOVERY.md`).

**Limitations:**
- The snapshot pool must have exactly one share (issue #71).
- No check that the snapshot pool is full; backups fail when it is (issue #69).
- No command to remove the backups of a volume (issue #70). Backups of a deleted volume stay on the share, and can be restored, until the keep time has passed since the last backup; after that the files stay on the share until someone removes them.
- A backup of a volume that is not a VM disk is a file copy. It is as consistent as the files are.
- Backups are in the same VPC as the data, and are not encrypted or locked beyond what the file share does (export to COS is a later step).
- A restore needs the snapshot pool to be ready, and waits while a copy is being written to the backup of that volume.
