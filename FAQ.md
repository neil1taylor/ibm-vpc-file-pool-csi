# Frequently Asked Questions — IBM VPC File Pool CSI Driver

Quick answers with links to authoritative documentation for deeper reading.

---

## General

### How is this different from the stock IBM VPC File CSI driver?

The stock driver creates one VPC file share per PVC (1:1 mapping). This driver pools many PVCs as subdirectories within a small number of large shares. PVC creation drops from 30-90 seconds to under 1 second, you use a fraction of your 300-share quota, and small PVCs don't each waste a 10 GB minimum allocation. See [FEATURES.md — Comparison Table](FEATURES.md#comparison-table) for a full feature-by-feature comparison.

### Can I run both drivers at the same time?

Yes. The pool driver uses provisioner name `vpc-file-pool.csi.ibm.io` while the stock driver uses `vpc.file.csi.ibm.io`. They run independently with separate StorageClasses. Use the stock driver for workloads needing dedicated IOPS isolation (e.g., databases), and the pool driver for everything else. See [USER-GUIDE.md — Coexistence](USER-GUIDE.md#coexistence-with-the-stock-ibm-csi-driver).

### Does this work on OpenShift (ROKS)?

Yes, fully supported. The driver is tested on ROKS 4.14 through 4.17. The node agent requires the `privileged` SCC (standard for CSI drivers), and the Helm chart includes the necessary configuration. See [USER-GUIDE.md — Compatibility Matrix](USER-GUIDE.md#compatibility-matrix).

### Does this work on IBM Cloud Satellite?

No. The driver requires IBM Cloud VPC infrastructure (it calls the VPC file share API directly). For Satellite locations, consider the community `csi-driver-nfs` with your own NFS server. See [KNOWN-LIMITATIONS.md — IBM Cloud Satellite](KNOWN-LIMITATIONS.md#ibm-cloud-satellite).

---

## Storage & Capacity

### How many PVCs can fit on a single share?

There is no hard limit in the driver. The practical limit depends on share capacity and the NFS server's metadata handling. In testing, hundreds of PVCs per share work well. Target 50-200 PVCs per share for general workloads. See [CAPACITY-PLANNING.md](CAPACITY-PLANNING.md) for sizing guidance.

### Is per-PVC quota enforced?

Quotas are advisory (soft enforcement). The driver tracks requested vs. actual usage and exposes it via Prometheus metrics, but NFS does not support per-directory hard quotas. Monitor usage via `NodeGetVolumeStats` or Prometheus alerts. See [KNOWN-LIMITATIONS.md — Quota Enforcement](KNOWN-LIMITATIONS.md#quota-enforcement).

### What happens when a pool is full?

New PVC requests fail with "pool has no available capacity." Options: increase `maxShares`, increase `shareSizeGB`, or delete unused PVCs. If `autoExpand` is enabled and `maxShares` is not reached, the pool creates new shares automatically. See [TROUBLESHOOTING.md — Pool Full](TROUBLESHOOTING.md#pool-full--maxshares-reached).

### Can I shrink a PVC?

No. Kubernetes does not support PVC shrinking (this is a platform-wide constraint, not specific to this driver). You can only expand PVCs.

### What happens to data when I delete a PVC?

It depends on the StorageClass reclaim policy: `Delete` removes the subdirectory and data, `Retain` keeps data for manual recovery, and `Archive` moves data to `.archived/` on the share. See [USER-GUIDE.md — Deleting a PVC](USER-GUIDE.md#deleting-a-pvc).

---

## Performance

### Is NFS slower than block storage?

For sequential I/O, NFS throughput is comparable to block storage (100-500 MB/s depending on share IOPS profile). Random I/O latency is slightly higher (1-5 ms vs <1 ms for block). NFS is well-suited for most workloads except latency-sensitive databases requiring sub-millisecond I/O. See [PERFORMANCE-TUNING.md — Expected Performance Ranges](PERFORMANCE-TUNING.md#expected-performance-ranges).

### Do PVCs on the same share compete for IOPS?

Yes. All PVCs share the share's total IOPS budget with no per-PVC isolation. One PVC doing heavy I/O can affect others on the same share. Mitigations: use tiered pools for IOPS-sensitive workloads, use `spread` strategy to distribute load, or use the stock IBM CSI driver for workloads needing dedicated IOPS. See [PERFORMANCE-TUNING.md — Noisy Neighbor Risk](PERFORMANCE-TUNING.md#noisy-neighbor-risk).

### How do I benchmark storage performance?

Deploy an `fio` pod with a PVC from the pool and run sequential and random I/O tests. See [PERFORMANCE-TUNING.md — Benchmarking](PERFORMANCE-TUNING.md#benchmarking) for ready-to-use fio pod YAML and interpretation guidance.

---

## KubeVirt

### How do I set up VMs with pool storage?

Configure the pool with `defaultUID: 107`, `defaultGID: 107`, and `defaultPermissions: "0777"`. Create PVCs directly (bypass CDI), download cloud images, convert from qcow2 to raw format, and reference PVCs in the VM spec using `persistentVolumeClaim`. See [KNOWN-LIMITATIONS.md — CDI Compatibility](KNOWN-LIMITATIONS.md#cdi-containerized-data-importer-compatibility) for the full workflow.

### My VM shows "No bootable device" — what's wrong?

The boot disk is in qcow2 format instead of raw. KubeVirt requires raw format for filesystem-mode PVCs. Convert with `qemu-img convert -f qcow2 -O raw`. See [TROUBLESHOOTING.md — No bootable device](TROUBLESHOOTING.md#vm-shows-no-bootable-device).

### Why doesn't CDI work with this driver?

CDI's VolumePopulator mechanism is incompatible with subdirectory-based CSI provisioning. CDI sets a `dataSource` that the `csi-provisioner` doesn't recognize, causing it to back off. The driver includes a golden image syncer (v0.11.0+) that provides an alternative workflow. See [KNOWN-LIMITATIONS.md — CDI Compatibility](KNOWN-LIMITATIONS.md#cdi-containerized-data-importer-compatibility).

---

## Security

### Can one PVC access another PVC's data on the same share?

Under normal operation, no. Each PVC is bind-mounted at its own subdirectory, and the kernel enforces bind-mount boundaries. However, a privileged pod with host filesystem access could theoretically access the staging mount. Use `PodSecurityAdmission` or `SecurityContextConstraints` to prevent privileged pods in tenant namespaces, and use separate pools for different trust levels. See [TROUBLESHOOTING.md — Cross-PVC data visibility](TROUBLESHOOTING.md#cross-pvc-data-visibility).

### Is NFS traffic encrypted?

NFS traffic is not encrypted in transit on ROKS clusters because IBM Cloud's encryption-in-transit (IPsec) is not supported on RHCOS-based worker nodes. VPC network traffic is already isolated at the hypervisor level, and data at rest is always encrypted (provider-managed or customer-managed keys). See [KNOWN-LIMITATIONS.md — NFS Encryption in Transit](KNOWN-LIMITATIONS.md#nfs-encryption-in-transit) and [SECURITY.md](SECURITY.md).

---

## Data Protection

### How fast are snapshots?

Snapshots are full directory copies (`cp -a`), not instantaneous COW snapshots. Speed is proportional to data size: ~1 GB takes 5-15 seconds, ~100 GB takes 5-15 minutes. Snapshots consume share capacity equal to the source data. See [BACKUP-RECOVERY.md](BACKUP-RECOVERY.md) for scheduling and retention strategies.

### What's the best backup strategy?

It depends on your workload. For application data, use scheduled snapshots + cross-region replication. For databases, use application-level backup (pg_dump, etc.) with pool snapshots as a secondary layer. For VMs, stop the VM before snapshotting. See [BACKUP-RECOVERY.md — Strategy Recommendations](BACKUP-RECOVERY.md#strategy-recommendations-by-workload-type).

---

## Troubleshooting

### My PVC is stuck in Pending

Check the PVC events (`kubectl describe pvc <name>`) for the specific error message: "pool not found" (StorageClass misconfigured), "no available capacity" (pool full), or "expanding, retry shortly" (normal — wait 2-3 minutes). See [TROUBLESHOOTING.md — PVC Issues](TROUBLESHOOTING.md#pvc-issues) for all cases.

### I'm getting "permission denied" errors

UID/GID mismatch between the pod's `securityContext` and the StorageClass's `uid`/`gid` parameters. Set `runAsUser`/`runAsGroup` to match, or use `fsGroup`. See [TROUBLESHOOTING.md — Permission denied](TROUBLESHOOTING.md#permission-denied-on-mount).

### How do I upgrade the driver?

Follow the [UPGRADE-GUIDE.md](UPGRADE-GUIDE.md) — it covers pre-flight checks, version-specific notes for every release, CRD upgrades, and rollback procedures.

---

## See Also

- [USER-GUIDE.md](USER-GUIDE.md) — Comprehensive end-user guide
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — Full troubleshooting runbook
- [FEATURES.md](FEATURES.md) — Complete feature catalog
- [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md) — Platform constraints and workarounds
