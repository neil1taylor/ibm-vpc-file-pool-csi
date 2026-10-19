---
name: pool-status
description: Run health checks on the pool CSI driver — pod status, pool phases, pending PVCs, and recent errors
allowed-tools: Bash(kubectl *)
---

Run the daily health check from DAY2-OPERATIONS.md and summarize the results:

```bash
# 1. Driver pods
kubectl get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -o wide
kubectl get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=node -o wide

# 2. Pool phases
kubectl get filesharepools -o custom-columns='NAME:.metadata.name,CAPACITY:.status.totalCapacityGB,ALLOCATED:.status.totalAllocatedGB,SHARES:.status.shareCount,PVCS:.status.totalPVCCount,PHASE:.status.phase'

# 3. Pending PVCs using pool StorageClasses
kubectl get pvc --all-namespaces | grep -E "Pending.*ibm-vpc-file-pool"

# 4. Per-share health
for pool in $(kubectl get filesharepools -o jsonpath='{.items[*].metadata.name}'); do
  echo "=== Pool: $pool ==="
  kubectl get fsp "$pool" -o jsonpath='{range .status.shares[*]}  {.shareID}  state={.state}  ip={.mountTargetIP}{"\n"}{end}'
done

# 5. Replication health (if configured)
kubectl get replicationpolicies -o custom-columns='NAME:.metadata.name,LAST_SYNC:.status.lastSyncTime,FAILURES:.status.consecutiveFailures,PHASE:.status.phase' 2>/dev/null

# 6. Recent controller errors (last hour)
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller --since=1h 2>/dev/null | grep -i "error\|fail" | tail -20
```

After running all commands, provide a brief summary:
- Are all pods running?
- Are all pools Ready?
- Any pending PVCs?
- Any share health issues?
- Any controller errors?

Flag anything that needs attention.
