# Help and Support

Start here when something does not work, or when you do not know where to look. This guide is for the cluster administrator who runs the driver.

## 1. Where to start

Open the **IBM VPC File Pools** page in the OpenShift console. Each tab has one job:

| Console tab | What it is for | Guide |
|-------------|----------------|-------|
| Overview | Pool capacity, latency and alerts at a glance | [MONITORING.md](MONITORING.md) |
| Pools | Make and change pools of shared storage | [TUTORIAL-01-POOL-CREATION.md](TUTORIAL-01-POOL-CREATION.md), [CAPACITY-PLANNING.md](CAPACITY-PLANNING.md) |
| SubVolumes | The volumes (PVCs) held in the pools | [USER-GUIDE.md](USER-GUIDE.md) |
| Snapshots | Snapshots of volumes and of VM disks, and restore | [TUTORIAL-02-SNAPSHOTS-AND-CLONES.md](TUTORIAL-02-SNAPSHOTS-AND-CLONES.md), [VM-INSTANT-SNAPSHOTS.md](VM-INSTANT-SNAPSHOTS.md) |
| Replication | Copies of volumes and running VMs to a second cluster | [TUTORIAL-06-REPLICATION-AND-FAILOVER.md](TUTORIAL-06-REPLICATION-AND-FAILOVER.md), [CROSS-REGION-DR.md](CROSS-REGION-DR.md) |
| Monitoring | Graphs of the driver's metrics | [MONITORING.md](MONITORING.md) |

Backups of volumes and VMs are set up with `BackupPolicy` and used with the `kubectl backup` command: see [BACKUP-RECOVERY.md](BACKUP-RECOVERY.md).

## 2. Common problems

| What you see | Go to |
|--------------|-------|
| A PVC stays `Pending` | [PVC Issues](TROUBLESHOOTING.md#pvc-issues) |
| A pool is full | [Pool Full — maxShares reached](TROUBLESHOOTING.md#pool-full--maxshares-reached) |
| A VM does not start | [VM Issues](TROUBLESHOOTING.md#vm-issues) |
| A VM does not start after a snapshot | [VM with instant snapshots does not start](TROUBLESHOOTING.md#vm-with-instant-snapshots-does-not-start) |
| A pod cannot mount its volume | [Mount Issues](TROUBLESHOOTING.md#mount-issues) |
| Replication is late or failing | [Replication is late or failing](TROUBLESHOOTING.md#replication-is-late-or-failing) |
| Backups stopped | [Backups stopped: the snapshot pool is above 85% full](TROUBLESHOOTING.md#backups-stopped-the-snapshot-pool-is-above-85-full) |
| A volume has no recent backup | [A volume has no recent backup](TROUBLESHOOTING.md#a-volume-has-no-recent-backup) |
| The driver itself will not start | [Controller Issues](TROUBLESHOOTING.md#controller-issues) |
| VPC errors (403, quota, rate limit) | [VPC API Issues](TROUBLESHOOTING.md#vpc-api-issues) |

Five commands to run first are in the [Quick Diagnostic Checklist](TROUBLESHOOTING.md#quick-diagnostic-checklist).

## 3. Collect diagnostics for a support case

```bash
bash scripts/collect-diagnostics.sh
```

This writes `csi-diagnostics-<time>.txt` in the current directory. It holds:
- the driver version and the cluster version,
- the pools, volumes, snapshots, backup policies, backup restores and replication policies,
- the driver pods, the controller log (500 lines), the node logs (200 lines per node) and recent events.

It reads no Secret, so it holds no API key. It also removes values that look like keys, tokens or passwords. **Read the file before you send it.** Set `NS=<namespace>` if the driver is not in `kube-system`.

## 4. Get support

Open an issue in the project repository and attach the diagnostics file:
<https://github.com/neil1taylor/ibm-vpc-file-pool-csi/issues>

Say in the issue:
- the cluster type (ROKS or ROVS) and its version,
- the driver version (the image tag),
- what you did, what you expected and what happened,
- the diagnostics file.

## 5. Known limits

- [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md) lists what the driver cannot do.
- [FEATURES.md](FEATURES.md) shows the maturity of each feature. A feature marked **Experimental** can change, and you should test it before you rely on it.
