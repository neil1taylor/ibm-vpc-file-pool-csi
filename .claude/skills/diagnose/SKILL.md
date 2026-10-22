---
name: diagnose
description: Collect full diagnostics bundle for the pool CSI driver and save to a timestamped file
allowed-tools: Bash(kubectl *), Bash(date *), Write
---

Collect the full diagnostics bundle from DAY2-OPERATIONS.md and save it to `csi-diagnostics-<timestamp>.txt` in the current directory.

Run these commands and capture all output into a single file:

1. Collection timestamp
2. Driver image version: `kubectl get deployment -n kube-system ibm-vpc-file-pool-csi-controller -o jsonpath='{.spec.template.spec.containers[0].image}'`
3. Cluster version: `kubectl version`
4. Node info: `kubectl get nodes -o wide`
5. CSI driver registration: `kubectl get csidriver vpc-file-pool.csi.ibm.io -o yaml`
6. Pool status: `kubectl get filesharepools -o yaml`
7. SubVolumes: `kubectl get subvolumes -o wide`
8. Pending PVCs: `kubectl get pvc --all-namespaces | grep Pending`
9. Controller pods: `kubectl get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -o wide`
10. Node agent pods: `kubectl get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=node -o wide`
11. Controller logs (last 500 lines): `kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller --tail=500`
12. Node agent logs (last 200 lines per pod)
13. Replication policies: `kubectl get replicationpolicies -o yaml`
14. Leader lease: `kubectl get lease -n kube-system ibm-vpc-file-pool-csi-leader -o yaml`
15. Recent events (last hour): `kubectl get events --all-namespaces --sort-by='.lastTimestamp' | tail -100`

After saving, print the file path and remind the user to **redact any API keys or secrets** before sharing.
