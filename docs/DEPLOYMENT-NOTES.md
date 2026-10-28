# Deployment Notes: IBM VPC File Pool CSI Driver on ROVS

One page for a deployment on Red Hat OpenShift Virtualization Service (ROVS), and the test that follows it. It says what is ready, what is not ready, and where to look. It links to the full documents instead of repeating them.

Status: draft. Fill in the version after the release is tagged.

## What the driver does

It pools many volumes (PVCs) as folders on a few shared VPC file shares. A volume is made in under a second, because it is a folder on a share that already exists. See [OVERVIEW.md](OVERVIEW.md) and [FEATURES.md](FEATURES.md).

## What is ready

| Area | Maturity | Notes |
|------|----------|-------|
| Pools and volumes (RWX, KubeVirt disks) | GA | The core of the driver. |
| Growth: a pool adds shares when it fills | GA | `autoExpand`. Pools are in one zone. |
| Snapshots, restore, clones | Beta | Every copy is made by a Job in the driver's namespace. Time grows with the size of the data. |
| VMs from the standard OpenShift catalog | Beta | The driver keeps CDI's copy memory limit at 8Gi, and CDI clones onto a pool by the driver (`csi-clone`). See [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md#cdi-containerized-data-importer-compatibility). |
| Moving a volume from another storage class into a pool | Beta | `kubectl migrate`, or the console page "Migrate volumes into a pool". |
| OpenShift console pages | Beta | Pools, volumes, snapshots, replication, monitoring, restore, clone, migrate. |
| Metrics and alerts | GA | The chart ships 21 alerts, on by default. They are made only when the cluster has the Prometheus Operator API (OpenShift has it). Limits are under `metrics.alerts`. On OpenShift the alerts live in the namespace `ibm-vpc-file-pool-csi-monitoring` and only work when an admin has turned on user workload monitoring (`enableUserWorkload: true`). See [MONITORING.md](MONITORING.md#where-alerts-run-on-openshift). |
| Replication to another cluster | Beta | Direct NFS mode, or receiver mode (HTTPS) with a connection check in the console. See [CROSS-REGION-DR.md](CROSS-REGION-DR.md). |
| Snapshot of a running VM with several disks | Beta | KubeVirt asks for one snapshot per disk. The driver copies all the pool disks of the VM at one moment, so the disks fit each other (tested on 21 October 2026). See [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md). |
| **Replicating a running VM** | **Experimental** | One consistent moment for all disks of a VM, restore points, failover with `kubectl failover`. Tested on two ROVS clusters. Treat it as a trial. |
| **Backup to a snapshot pool** | **Experimental** | A `BackupPolicy` backs up volumes and VMs into a pool that holds only backups. Restore with `kubectl backup restore`. See [BACKUP-RECOVERY.md](BACKUP-RECOVERY.md). |
| **Backup to IBM Cloud Object Storage (COS)** | **Experimental** | A second copy in a locked bucket. Read "Backup to COS: what is proven" below before you rely on it. |

## What does not work yet

- **Group snapshots (`VolumeGroupSnapshot`) are not active.** The code and the record type exist, but nothing acts on them and the console does not show them. This is a decision, not a gap: there is no plan to turn them on (issue #100). You do not need them for VMs, because a KubeVirt snapshot of a VM with several disks already shows one moment (see the table). See [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md#volume-group-snapshots).
- **By default a snapshot is a full copy, not an instant one.** (Instant VM snapshots exist, but they are Experimental, need the KubeVirt `Sidecar` feature gate, which Red Hat does not support, and are off unless you turn them on: [VM-INSTANT-SNAPSHOTS.md](VM-INSTANT-SNAPSHOTS.md).) A copy takes minutes for a large disk and uses as much space as the disk holds. KubeVirt marks a `VirtualMachineSnapshot` done after a few seconds, but each disk's `VolumeSnapshot` is ready only when its copy ends. Wait for every `VolumeSnapshot` before you restore. A snapshot of one disk of a multi-disk VM also copies the other disks to temporary files, which are deleted after 10 minutes, so each share needs free room for them.
- **A snapshot of a running VM's disk is consistent only when the driver can freeze the guest** (the guest agent must answer). The snapshot's `status.consistency` says which kind it is.
- **Golden images** are hidden. Use the standard catalog.

## Backup to COS: what is proven and what is not

Built: `spec.export` on a `BackupPolicy` (Compliance object lock, a backup key wrapped by Key Protect), `kubectl backup cos-setup` to make the bucket and keys, `kubectl backup restore --from-cos`, `kubectl backup cos-list`, and [TUTORIAL-09-COS-BACKUP-BUCKET.md](TUTORIAL-09-COS-BACKUP-BUCKET.md).

- **Proven (stage 18, quick run 1022-1215, image v0.13.2-109-gefdbbb2, lab cluster rovslab-prod-eude, a real COS bucket and Key Protect root key in eu-de):**
  - Export of VM disks and a pod volume to COS, with Key Protect unwrapping and Kopia.
  - `kubectl backup cos-list` shows one export per volume. The run found a fault here, fixed in PR #183.
  - Restore from COS into the same cluster. The VM boots with no XFS errors and all logged checksums match. The pod volume restored at the first moment holds the first content, and at the latest moment holds the changed content. The cluster was left clean.
  - `cos-setup` works on real IBM Cloud, after the fix for the extra instance-wide Writer policy that IBM adds (PR #149, issue #138).
  - Each run leaves Compliance-locked objects for 25 hours. The cost is small.
  - The backup to a snapshot pool also ran in the earlier full run 1022-0847, including the local (non-COS) restores. Quick mode skips the local restores.
- **Not proven:**
  - A restore from COS in the other (DR) cluster.
  - Emergency restore with `--backup-key-file`. The readable backup key from the `cos-setup` run was not saved (lab only).
  - Long-run behaviour of the 5-minute schedule at full size. Quick mode uses a 1-minute schedule and a 1 GB disk.
- Until the three items above pass, do not treat the COS copy as your only backup.

## Install

1. ROVS has no cert-manager. Set `webhook.certProvider=openshift`. See [INSTALL.md](INSTALL.md).
2. Copy the `all-icr-io` pull secret into `kube-system` if it is not there. See [INSTALL.md](INSTALL.md).
3. Give the driver an API key: [API-KEY-SETUP.md](API-KEY-SETUP.md).
4. Install the chart, then make one pool: [TUTORIAL-01-POOL-CREATION.md](TUTORIAL-01-POOL-CREATION.md).
5. Shares use security group access mode. Check that the cluster's worker subnets are listed in `vpc_subnet_ids`: [VPC-NETWORKING.md](VPC-NETWORKING.md).
6. Turn on the console plugin: [TUTORIAL-08-MIGRATION-AND-CONSOLE.md](TUTORIAL-08-MIGRATION-AND-CONSOLE.md).

## Size a pool before you create it

IBM only accepts some share size and IOPS pairs (for example, 3,000 IOPS needs at least 80 GB). The driver refuses a pool that does not fit, and says which range is allowed. See [CAPACITY-PLANNING.md](CAPACITY-PLANNING.md) and [PERFORMANCE-TUNING.md](PERFORMANCE-TUNING.md).

## If something goes wrong

- Start with the support guide: [SUPPORT.md](SUPPORT.md). It maps what you see to the right section.
- Check the pool: `kubectl get filesharepools` (the phase should be `Ready`).
- Common problems: [TROUBLESHOOTING.md](TROUBLESHOOTING.md).
- Collect the diagnostics with `bash scripts/collect-diagnostics.sh` and read the file before you send it.
- **Support is GitHub issues only.** Open an issue at <https://github.com/neil1taylor/ibm-vpc-file-pool-csi/issues> and attach the diagnostics. There is no other support contact.
- Never send the API key. Share IDs and mount target addresses are fine to send.

## During the test

- Record the cluster, the pool settings, and what the workload does (VMs, files, both).
- Keep to the Beta and GA rows for anything that holds important data. Use the Experimental rows only for copies you can lose.
- Note anything that surprised you, including docs that were unclear.

## Decisions I made for you

- The page links to the full documents and does not repeat them, so it cannot drift from them.
- The maturity column copies the labels in [FEATURES.md](FEATURES.md). I did not promote anything.
- Backups are listed as Experimental, with a plain list of what is and is not proven for COS.
- Group snapshots are listed as not active, as decided, with no plan.
- Replicating a running VM stays Experimental, with its own row and a plain warning.
