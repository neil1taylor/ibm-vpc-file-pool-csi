# Tutorial Series — IBM VPC File Pool CSI Driver

Hands-on tutorials covering every major capability of the pool CSI driver. Each part is self-contained with its own setup and cleanup — follow them in any order after Part 1.

**Cluster:** ROKS (OpenShift Virtualization enabled)

---

## Tutorials

| Part | Tutorial | What You'll Learn |
|------|----------|-------------------|
| 1 | [Pool Creation, PVCs, and KubeVirt VMs](TUTORIAL-01-POOL-CREATION.md) | Create a pool, provision PVCs, launch a VM with NFS-backed boot and data disks |
| 2 | [Snapshots and Clones](TUTORIAL-02-SNAPSHOTS-AND-CLONES.md) | Point-in-time snapshots, restore from snapshot, sync clones (<10 GB) and async clones (>10 GB) |
| 3 | [Group Snapshots and Hooks](TUTORIAL-03-GROUP-SNAPSHOTS-AND-HOOKS.md) | Coordinated multi-PVC snapshots, exec and HTTP lifecycle hooks, failure policies |
| 4 | [Pool Configuration](TUTORIAL-04-POOL-CONFIGURATION.md) | Multi-tier pools, spread vs binpack strategies, auto-expansion, share draining |
| 5 | [Golden Images](TUTORIAL-05-GOLDEN-IMAGES.md) | CDI native mode and custom syncer for automatic KubeVirt golden image provisioning |
| 6 | [Replication and Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md) | Direct NFS and driver-to-driver cross-region replication, pause/resume, failover CLI |
| 7 | [Monitoring](TUTORIAL-07-MONITORING.md) | Prometheus metrics (21 metrics), ServiceMonitor, alerting rules, Grafana dashboard |
| 8 | [Migration and Console Plugin](TUTORIAL-08-MIGRATION-AND-CONSOLE.md) | PVC migration from stock driver, OpenShift console plugin walkthrough |

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
└── Part 8 (migration/console)
```

Parts 2-8 are independent of each other — pick whichever is relevant to your use case.

---

## Prerequisites (All Parts)

Every tutorial assumes:

1. **ROKS cluster** with OpenShift Virtualization enabled (for VM tutorials)
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
| 3 | `pool-tutorial-groupsnap` |
| 4 | `pool-tutorial-config` |
| 5 | `pool-tutorial-golden` |
| 6 | `pool-tutorial-repl` |
| 7 | `pool-tutorial-monitoring` |
| 8 | `pool-tutorial-ops` |

Tutorials can be run concurrently on the same cluster without interference.

---

## Quick Links

- [Architecture](ARCHITECTURE.md) — system design and component diagram
- [CRD Spec](CRD-SPEC.md) — all CRD definitions
- [Features](FEATURES.md) — complete feature catalog
- [Troubleshooting](TROUBLESHOOTING.md) — common issues and solutions
- [Install Guide](INSTALL.md) — build, deploy, Helm chart
