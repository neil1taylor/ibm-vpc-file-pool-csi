# Examples — IBM VPC File Pool CSI Driver

Ready-to-use examples for common deployment patterns. Each example can be applied directly with `kubectl apply -f`.

## Prerequisites

Before using these examples, ensure:
1. The CSI driver is installed ([INSTALL.md](../INSTALL.md))
2. At least one `FileSharePool` is `Ready` (the `basic/` example creates one)
3. A StorageClass exists for the pool (the `basic/` example creates one)

## Examples

| Directory | Description |
|-----------|-------------|
| [basic/](basic/) | Minimal setup: pool, StorageClass, PVC, and pod |
| [multi-zone/](multi-zone/) | Pools across availability zones with WaitForFirstConsumer |
| [statefulset/](statefulset/) | StatefulSet with volumeClaimTemplates |
| [shared-rwx/](shared-rwx/) | ReadWriteMany PVC shared across deployment replicas |
| [tiered/](tiered/) | Performance tiers: standard and high-IOPS pools |
| [custom-permissions/](custom-permissions/) | Restricted UID/GID/permissions via StorageClass |
| [retain-archive/](retain-archive/) | Retain or archive data on PVC deletion |

## Quick Start

```bash
# 1. Create the pool and StorageClass
kubectl apply -f examples/basic/pool.yaml
kubectl apply -f examples/basic/storageclass.yaml

# 2. Wait for the pool to be Ready (30-90 seconds for initial share creation)
kubectl get filesharepools -w

# 3. Create a PVC and pod
kubectl apply -f examples/basic/pvc.yaml
kubectl apply -f examples/basic/pod.yaml

# 4. Verify
kubectl get pvc my-app-data          # Should be Bound
kubectl logs my-app-pod              # Should show "Hello from pool CSI"
```

## Cleanup

```bash
# Delete examples in reverse order
kubectl delete -f examples/basic/pod.yaml
kubectl delete -f examples/basic/pvc.yaml
kubectl delete -f examples/basic/storageclass.yaml
kubectl delete -f examples/basic/pool.yaml
```

## Notes

- Replace `us-south-1` / `us-south-2` zones with your actual VPC zones
- Replace `your-resource-group-id` with your IBM Cloud resource group ID
- Pool creation triggers a VPC file share creation (30-90 seconds)
- PVC binding is near-instant once the pool is Ready
