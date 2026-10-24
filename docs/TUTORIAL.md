# Tutorial Series — IBM VPC File Pool CSI Driver

Hands-on tutorials covering every major capability of the pool CSI driver. Each part is self-contained with its own setup and cleanup — follow them in any order after Part 1. The words used here are defined in [CONTEXT.md](CONTEXT.md).

**Cluster:** ROKS (OpenShift Virtualization enabled)

---

## Tutorials

| Part | Tutorial | What You'll Learn |
|------|----------|-------------------|
| 1 | [Pool Creation, PVCs, and KubeVirt VMs](TUTORIAL-01-POOL-CREATION.md) | Create a pool, provision PVCs, launch a VM with NFS-backed boot and data disks |
| 2 | [Snapshots and Clones](TUTORIAL-02-SNAPSHOTS-AND-CLONES.md) | Point-in-time snapshots, restore from snapshot, clones (asynchronous, Job-based) |
| 3 | [Group Snapshots and Hooks](TUTORIAL-03-GROUP-SNAPSHOTS-AND-HOOKS.md) | Group snapshots are not active; what to use instead, and lifecycle hooks on replication |
| 4 | [Pool Configuration](TUTORIAL-04-POOL-CONFIGURATION.md) | Multi-tier pools, spread vs binpack strategies, auto-expansion, share draining |
| 5 | [Golden Images](TUTORIAL-05-GOLDEN-IMAGES.md) | CDI native mode and custom syncer for automatic KubeVirt golden image provisioning |
| 6 | [Replication and Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md) | Direct NFS and driver-to-driver cross-region replication, pause/resume, failover CLI |
| 7 | [Monitoring](TUTORIAL-07-MONITORING.md) | Prometheus metrics (37 metrics), ServiceMonitor, alerting rules, Grafana dashboard |
| 8 | [Migration and Console Plugin](TUTORIAL-08-MIGRATION-AND-CONSOLE.md) | PVC migration from stock driver, OpenShift console plugin walkthrough |
| 9 | [COS Bucket for Backups](TUTORIAL-09-COS-BACKUP-BUCKET.md) | Locked COS bucket, a key that cannot delete or unlock, a CBR rule that limits it to the cluster |
| 10 | [Backup and Restore](TUTORIAL-10-BACKUP-AND-RESTORE.md) | Back up a volume on a schedule to a snapshot pool, list the moments, restore one, delete backups |
| 11 | [Instant VM Snapshots](TUTORIAL-11-INSTANT-VM-SNAPSHOTS.md) | Turn on the Sidecar gate, snapshot a running VM in seconds, merge, restore, flatten |
| 12 | [Replicating Running VMs](TUTORIAL-12-REPLICATING-RUNNING-VMS.md) | Copy a running VM with all disks at one moment to a second cluster through a receiver |
| 13 | [Failover of Running VMs](TUTORIAL-13-FAILOVER-OF-RUNNING-VMS.md) | Plan, rewind and fail over replicated VMs with `kubectl failover` |
| 14 | [Restore a Snapshot in the Console](TUTORIAL-14-CONSOLE-RESTORE.md) | Make a snapshot and restore it to a new PVC in the OpenShift console |

---

## Reading Order

```
Part 1 (foundation — do this first)
├── Part 2 (snapshots/clones)
│   └── Part 3 (group snapshots/hooks)
├── Part 4 (pool configuration)
│   └── Part 5 (golden images)
├── Part 6 (replication/failover)
├── Part 7 (monitoring)
├── Part 8 (migration/console)
├── Part 9 (COS bucket for backups)
├── Part 10 (backup and restore)
├── Part 11 (instant VM snapshots)
├── Part 12 (replicating running VMs)
│   └── Part 13 (failover of running VMs)
└── Part 14 (restore a snapshot in the console)
```

Parts 2-14 are independent of each other, except that Part 13 follows Part 12. Pick whichever is relevant to your use case.

---

## Prerequisites (All Parts)

Every tutorial assumes:

1. **ROKS or ROVS cluster** with OpenShift Virtualization enabled (for VM tutorials)
2. **CSI driver deployed** — controller and node pods running:
   ```bash
   oc get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi
   ```
3. **IBM Cloud CLI** with VPC infrastructure plugin (`ibmcloud is`)
4. **`oc` and `virtctl`** CLI tools installed

---

## Namespace Convention

Each tutorial uses a unique namespace to avoid conflicts:

| Part | Namespace |
|------|-----------|
| 1 | `pool-tutorial` |
| 2 | `pool-tutorial-snapshots` |
| 3 | none (Part 3 has no steps to run) |
| 4 | `pool-tutorial-config` |
| 5 | `pool-tutorial-golden` |
| 6 | `pool-tutorial-repl` |
| 7 | `pool-tutorial-monitoring` |
| 8 | `pool-tutorial-ops` |
| 10 | `pool-tutorial-backup` |
| 11 | `vms` (your own VM) |
| 12 | `vms` (your own VM) |
| 13 | `vms` (your own VM) |
| 14 | `pool-tutorial-console` |

Tutorials can be run concurrently on the same cluster without interference.

---

## Quick Links

- [Architecture](ARCHITECTURE.md) — system design and component diagram
- [CRD Spec](CRD-SPEC.md) — all CRD definitions
- [Features](FEATURES.md) — complete feature catalog
- [Troubleshooting](TROUBLESHOOTING.md) — common issues and solutions
- [Install Guide](INSTALL.md) — build, deploy, Helm chart
