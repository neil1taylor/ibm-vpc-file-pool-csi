# Pilot Notes: IBM VPC File Pool CSI Driver on ROVS

One page for a customer pilot on Red Hat OpenShift Virtualization Service (ROVS). It says what the pilot covers, what is not ready, and where to look. It links to the full documents instead of repeating them.

Status: draft for the pilot. Fill in the version after the release is tagged.

## What the driver does

It pools many volumes (PVCs) as folders on a few shared VPC file shares. A volume is made in under a second, because it is a folder on a share that already exists. See [OVERVIEW.md](OVERVIEW.md) and [FEATURES.md](FEATURES.md).

## What the pilot covers

| Area | Maturity | Notes |
|------|----------|-------|
| Pools and volumes (RWX, KubeVirt disks) | GA | The core of the driver. |
| Growth: a pool adds shares when it fills | GA | `autoExpand`. Pools are in one zone. |
| Snapshots, restore, clones | Beta | Every copy is made by a Job in the driver's namespace. Time grows with the size of the data. |
| VMs from the standard OpenShift catalog | Beta | The driver keeps CDI's copy memory limit at 8Gi, and CDI clones onto a pool by the driver (`csi-clone`). See [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md#cdi-containerized-data-importer-compatibility). |
| Moving a volume from another storage class into a pool | Beta | `kubectl migrate`, or the console page "Migrate volumes into a pool". |
| OpenShift console pages | Beta | Pools, volumes, snapshots, replication, monitoring, restore, clone, migrate. |
| Metrics and alerts | GA | See [MONITORING.md](MONITORING.md). |
| Replication to another cluster | Beta | Direct NFS mode, or receiver mode (HTTPS) with a connection check in the console. See [CROSS-REGION-DR.md](CROSS-REGION-DR.md). |
| **Replicating a running VM** | **Experimental** | One consistent moment for all disks of a VM, restore points, failover with `kubectl failover`. Tested on two ROVS clusters. Treat it as a trial. |

## What does not work yet

- **Group snapshots on ROVS** do not work: nothing on ROVS calls the driver for them (issue #14). Multi-disk VMs cannot use them. Use a snapshot per volume, or stop the application first. See [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md#volume-group-snapshots).
- **A snapshot of a running VM's disk is consistent only when the driver can freeze the guest** (the guest agent must answer). The snapshot's `status.consistency` says which kind it is.
- **Golden images** are hidden. Use the standard catalog.
- Backups are not part of this list until they are released.

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

- Check the pool: `kubectl get filesharepools` (the phase should be `Ready`).
- Common problems: [TROUBLESHOOTING.md](TROUBLESHOOTING.md).
- Before you open a ticket, collect the diagnostics: [DAY2-OPERATIONS.md](DAY2-OPERATIONS.md#log-collection-for-support).
- Never send the API key. Share IDs and mount target addresses are fine to send.

## What we ask of the pilot

- Tell us the cluster, the pool settings, and what the workload does (VMs, files, both).
- Keep to the Beta and GA rows for anything that holds important data. Use the Experimental row only for copies you can lose.
- Send us what surprised you, including docs that were unclear.

## Decisions I made for you

- The page links to the full documents and does not repeat them, so it cannot drift from them.
- The maturity column copies the labels in [FEATURES.md](FEATURES.md). I did not promote anything.
- Backups are left out until they are released.
- Replicating a running VM stays Experimental, with its own row and a plain warning.
