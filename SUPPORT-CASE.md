# Support Case: Worker Nodes Stuck on Reload — "Unable to talk to IKS servers"

## Cluster Details

| Field | Value |
|-------|-------|
| Cluster Name | `ocp-virt-420-cluster` |
| Cluster ID | `d62edtlf05426pegte60` |
| Region / Zone | `eu-de` / `eu-de-1` |
| Master Version | 4.20.12_1536_openshift |
| Worker Version | 4.20.11_1535_openshift |
| Worker Flavor | `bx2d.metal.96x384` (bare metal) |
| VPC ID | `r010-c1ad3bf9-8b94-49c9-9aa2-bfb88ede7367` |
| VPE Gateway | `d62edtlf05426pegte60.vpe.private.eu-de.containers.cloud.ibm.com:30456` |
| Private Service Endpoint | `c114.private.eu-de.containers.cloud.ibm.com:30456` |

## Affected Workers

| Worker ID | IP | Current State | Status |
|-----------|----|---------------|--------|
| `kube-...-000001d7` | 10.67.67.14 | deploying | Worker unable to talk to IKS servers |
| `kube-...-00000311` | 10.67.67.15 | deploying | Worker unable to talk to IKS servers |
| `kube-...-000002b1` | 10.67.67.13 | normal | Ready (not reloaded) |

## Problem Summary

Worker nodes fail to complete reload. After reimaging, both affected nodes reach the `deploying` phase but then get stuck with the error:

> Worker unable to talk to IKS servers. Please verify your firewall setup is allowing traffic from this worker

The reload then cycles — the system retries automatically (stop → reinitialize → deploy → firewall error → retry). Node `00000311` has been through this cycle at least 3 times over ~2 hours. Node `000001d7` has hit the same error on its first reload attempt.

The one node that was **not** reloaded (`000002b1`) remains healthy and Ready.

## Timeline (2026-02-18, all times UTC)

| Time | Action / Event |
|------|---------------|
| ~00:35 | Observed node `00000311` in `critical / Unknown` state (was already unhealthy before reload) |
| ~00:38 | Ran `ibmcloud ks worker reload` on `00000311` |
| ~00:45 | `00000311` entered `reload_pending`, then `reloading / reinitializing` |
| ~01:10 | `00000311` reached `deploying` phase |
| ~01:30 | `00000311` showed "Worker unable to talk to IKS servers" for the first time |
| ~01:40 | System auto-retried reload of `00000311` (attempt 2) |
| ~01:50 | Node `000001d7` also went `critical / Unknown` on its own (not reloaded yet) |
| ~01:52 | Ran `ibmcloud ks worker reload` on `000001d7` |
| ~02:10 | `000001d7` entered `deploying` phase |
| ~02:20 | `00000311` still cycling — attempt 3 |
| ~02:45 | Both `000001d7` and `00000311` stuck in `deploying` with "unable to talk to IKS servers" |

## What We Checked

### Security Groups — No Issues

The cluster security group (`kube-d62edtlf05426pegte60`) has:
- **Outbound all** to `0.0.0.0/0` (unrestricted)
- **Outbound all** to `161.26.0.0/16` (IBM private services)
- **Outbound all** to `166.8.0.0/14` (IBM CSE endpoints)
- **Outbound TCP 443** to specific IKS API endpoints (`149.81.10.228`, `149.81.33.182`)
- **Outbound all** to VPE gateway SG (`kube-vpegw-d62edtlf05426pegte60`)
- **Outbound all** to cluster SG (self-referencing)

The additional SG (`ocp-virt-420-sg`) also has outbound all to `0.0.0.0/0`.

There are no inbound or outbound rules blocking traffic to IKS servers.

### Cluster Master — Healthy

The master is `deployed`, `Ready`, health `normal`. It was not affected.

### Other Observations

- The cluster state shows `pending` / "worker nodes are being provisioned" which reflects the two nodes being reloaded.
- Ingress status is `critical` — likely because 2 of 3 nodes are down.
- `Outbound Traffic Protection: disabled` — so no additional egress restrictions.
- The only healthy node (`000002b1`) has never been reloaded during this session.
- Before the reload, pods on all nodes were experiencing very slow container creation (5-10 min for a single busybox container) and frequent `sd-bus` CRI-O errors. This was the reason for attempting the reload.

## Why We Reloaded

We were deploying a CSI driver and observed extremely slow pod startup on all three nodes:
- OVN/multus network interface attachment: 2-5 minutes per pod
- CRI-O container creation: 5+ minutes per container with `sd-bus call: Connection timed out` errors
- A plain `busybox` pod with no special configuration took ~10 minutes to reach Running

We created a minimal busybox pod as a baseline test to confirm the slowness was infrastructure-level, not caused by our workload. Both busybox and our driver image exhibited identical startup delays.

Node `00000311` was already in `critical / Unknown` state before we attempted the reload.

## Request

Please investigate why the two reloaded worker nodes cannot reach the IKS control plane after reimaging. The security groups allow all outbound traffic, so the block appears to be elsewhere (VPE gateway, private service endpoint routing, or infrastructure-level networking).
