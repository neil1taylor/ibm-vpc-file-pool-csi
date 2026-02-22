# Upgrade Guide — IBM VPC File Pool CSI Driver

This guide covers upgrading the driver between versions without data loss or downtime.

---

## Pre-Flight Checklist

Before upgrading, complete every item:

- [ ] **Read the [CHANGELOG.md](CHANGELOG.md)** entry for the target version — note breaking changes, new CRD fields, and required actions
- [ ] **Verify pool health** — all pools should be in `Ready` phase:
  ```bash
  kubectl get filesharepools
  # All pools should show Phase: Ready
  ```
- [ ] **Back up CRD resources** (pools and subvolumes are the source of truth for all state):
  ```bash
  kubectl get filesharepools -o yaml > backup-pools.yaml
  kubectl get subvolumes -o yaml > backup-subvolumes.yaml
  kubectl get snapshots.storage.ibmcloud.io -o yaml > backup-snapshots.yaml 2>/dev/null
  kubectl get replicationpolicies -o yaml > backup-replication.yaml 2>/dev/null
  kubectl get volumegroupsnapshots -o yaml > backup-groupsnapshots.yaml 2>/dev/null
  ```
- [ ] **Check for in-flight operations** — no PVCs should be in `Pending` state and no clones should be in progress:
  ```bash
  kubectl get pvc --all-namespaces | grep Pending
  kubectl get subvolumes -o jsonpath='{range .items[?(@.spec.cloneStatus=="InProgress")]}{.metadata.name}{"\n"}{end}'
  ```
- [ ] **Verify you have the correct target version** of the Helm chart or manifests
- [ ] **Confirm cluster meets the compatibility requirements** for the target version (see below)

---

## Compatibility Matrix

| Driver Version | Kubernetes | OpenShift (ROKS) | Helm | Go | Key Dependencies |
|---------------|-----------|-----------------|------|-----|-----------------|
| v0.1.0–v0.3.0 | 1.28+ | 4.14+ | v3.10+ | 1.25+ | controller-runtime v0.18+ |
| v0.4.0–v0.5.0 | 1.28+ | 4.14+ | v3.10+ | 1.25+ | controller-runtime v0.18+, VolumeSnapshot CRDs |
| v0.6.0–v0.8.0 | 1.28+ | 4.14+ | v3.10+ | 1.25+ | controller-runtime v0.23+, cert-manager v1.12+ |
| v0.9.0–v0.10.0 | 1.28+ | 4.14+ | v3.10+ | 1.25+ | controller-runtime v0.23+, cert-manager v1.12+ |
| v0.11.0 | 1.28+ | 4.14+ | v3.10+ | 1.25+ | controller-runtime v0.23+, cert-manager v1.12+, CDI (optional) |

---

## Upgrade Methods

### Helm Upgrade (Recommended)

```bash
# 1. Update the chart repository (if using a remote repo)
helm repo update

# 2. Preview changes
helm diff upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --values my-values.yaml

# 3. Apply the upgrade
helm upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --values my-values.yaml \
  --timeout 5m

# 4. Watch rollout
kubectl rollout status deployment/vpc-file-pool-csi-controller -n kube-system
kubectl rollout status daemonset/ibm-vpc-file-pool-csi-node -n kube-system
```

### Raw Manifest Upgrade

```bash
# 1. Apply updated CRDs first (CRDs are not managed by kubectl apply on deployments)
kubectl apply -f config/crd/

# 2. Apply updated RBAC
kubectl apply -f config/rbac/

# 3. Apply updated deployments and daemonsets
kubectl apply -f config/controller/
kubectl apply -f config/node/

# 4. Watch rollout
kubectl rollout status deployment/vpc-file-pool-csi-controller -n kube-system
kubectl rollout status daemonset/ibm-vpc-file-pool-csi-node -n kube-system
```

---

## CRD Upgrade Procedures

CRDs require special handling because Helm does not upgrade CRDs on `helm upgrade` (by design).

### Additive Schema Changes (Safe)

Most upgrades add new optional fields to existing CRDs. These are backward-compatible — existing CRs continue to work without modification. Apply the new CRDs before upgrading the controller:

```bash
kubectl apply -f config/crd/
```

### Breaking Schema Changes

No versions to date have introduced breaking CRD schema changes. All field additions have been optional with backward-compatible defaults. If a future version requires a breaking change, it will be called out explicitly in the CHANGELOG and this guide will be updated with migration steps.

### Verifying CRD Updates

```bash
# Check CRD versions
kubectl get crd filesharepools.storage.ibmcloud.io -o jsonpath='{.spec.versions[*].name}'
kubectl get crd subvolumes.storage.ibmcloud.io -o jsonpath='{.spec.versions[*].name}'

# Verify new fields are present (example: check for ExportPath added in v0.9.0)
kubectl get crd filesharepools.storage.ibmcloud.io -o json | \
  python3 -c "import sys,json; schema=json.load(sys.stdin); print('exportPath' in str(schema))"
```

---

## Version-Specific Upgrade Notes

### Upgrading to v0.11.0 (from v0.10.0)

**New features:** Golden image syncer, mount target recovery, `selectShare` IP guard.

**CRD changes (additive):**
- `FileSharePool.spec.goldenImages` — new optional field for golden image syncer config
- `FileSharePool.status.goldenImages` — new status field for per-image sync state

**RBAC changes:** ClusterRole extended with `cdi.kubevirt.io` (dataimportcrons, datasources), `batch/jobs`, and `template.openshift.io/templates`. Apply updated RBAC before upgrading.

**Action required:**
1. Apply updated CRDs: `kubectl apply -f config/crd/`
2. Apply updated RBAC: `kubectl apply -f config/rbac/`
3. Upgrade controller — the new mount target recovery and IP guard activate automatically
4. (Optional) Configure `spec.goldenImages` on FileSharePool CRs if using KubeVirt

**No data migration required.** Shares with missing mount targets are automatically recovered or drained.

---

### Upgrading to v0.10.0 (from v0.9.0)

**New features:** NFS `sec=sys` default mount option, CDI StorageProfile auto-patching.

**Action required:**
1. Upgrade controller and node DaemonSet
2. Existing NFS mounts continue with their current `sec=` negotiation. New mounts get `sec=sys` automatically.
3. To apply `sec=sys` to existing mounts, restart the node DaemonSet pods (rolling restart happens automatically during upgrade)

**KubeVirt users:** This upgrade fixes the "chown operation not permitted" issue. After upgrade, restart affected VMs.

---

### Upgrading to v0.9.0 (from v0.8.0)

**Critical fix:** NFS mount used server root instead of share export path.

**CRD changes (additive):**
- `PoolShareStatus.ExportPath` — new field for NFS export path
- `ZoneMountTarget.ExportPath` — new field
- `SubVolumeSpec.ShareExportPath` — new field

**Action required:**
1. Apply updated CRDs: `kubectl apply -f config/crd/`
2. Upgrade controller — existing shares without export paths are **automatically backfilled** during the reconciler's health check
3. Upgrade node DaemonSet — new mounts use the correct export path

**No manual data migration required.** The health check backfill handles existing shares.

---

### Upgrading to v0.8.0 (from v0.7.0)

**Fixes:** Hardcoded staging path, mount flags override, resource requests/limits, probes, webhook cert permissions.

**Action required:**
1. Apply updated CRDs
2. Upgrade controller — now includes `--kubelet-dir` flag wired through Helm
3. Upgrade node DaemonSet — gains resource limits, probes, and readiness checks

**ROKS users:** Verify `node.kubeletDir` is set to `/var/data/kubelet` in your Helm values. The controller's staging path now respects this setting.

---

### Upgrading to v0.7.0 (from v0.6.0)

**New feature:** Automatic StorageClass creation.

**RBAC changes:** Controller ClusterRole needs `create` verb for `storageclasses`.

**Action required:**
1. Apply updated RBAC: `kubectl apply -f config/rbac/`
2. Apply updated CRDs
3. Upgrade controller — auto-creates StorageClasses for existing pools

**Note:** If you already have manually created StorageClasses, they are not overwritten. To opt out of auto-creation, annotate the pool:
```bash
kubectl annotate filesharespool <pool-name> storage.ibmcloud.io/skip-storageclass="true"
```

---

### Upgrading to v0.6.0 (from v0.5.0)

**New features:** Validating admission webhooks, lifecycle hooks, incremental rsync.

**Prerequisites:**
- cert-manager v1.12+ must be installed (for webhook TLS certificates)

**CRD changes (additive):**
- `ReplicationPolicy.spec.preSyncHooks` / `postSyncHooks` — new hook fields
- `ReplicationPolicy.spec.incrementalSync` — new field (default `true`)
- `VolumeGroupSnapshot.spec.preSnapshotHooks` / `postSnapshotHooks` — new hook fields
- New CRD types: `Hook`, `ExecHookSpec`, `HTTPHookSpec`, `HookResult`

**Action required:**
1. Install cert-manager if not already present: `kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml`
2. Apply updated CRDs: `kubectl apply -f config/crd/`
3. Apply webhook configurations: `kubectl apply -f config/webhook/`
4. Upgrade controller

**Known issue fixed:** VPC auto-discovery silent failure — if you experienced pools without mount targets, this version fixes it by using `kubernetes.Clientset` for pre-start ConfigMap reads.

---

### Upgrading to v0.5.0 (from v0.4.0)

**New feature:** Cross-region disaster recovery (ReplicationPolicy CRD).

**CRD changes (additive):**
- New CRD: `ReplicationPolicy`

**Action required:**
1. Apply new CRD: `kubectl apply -f config/crd/`
2. Upgrade controller
3. (Optional) Create `ReplicationPolicy` CRs for DR — see [CROSS-REGION-DR.md](CROSS-REGION-DR.md)

---

### Upgrading to v0.4.0 (from v0.3.0)

**New features:** Volume snapshots, volume cloning, volume group snapshots, share draining, migration CLI.

**CRD changes (additive):**
- New CRD: `Snapshot`
- New CRD: `VolumeGroupSnapshot`
- `SubVolume` gains clone fields: `sourceVolume`, `sourceShareID`, `cloneStatus`, `cloneProgress`
- `FileSharePool.spec.drainShares` — new field for graceful share draining

**Prerequisites:**
- VolumeSnapshot CRDs must be installed (external-snapshotter):
  ```bash
  kubectl apply -f https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/client/config/crd/
  ```

**Action required:**
1. Install VolumeSnapshot CRDs (if not already present)
2. Apply updated CRDs: `kubectl apply -f config/crd/`
3. Upgrade controller and node DaemonSet
4. (Optional) Create `VolumeSnapshotClass` for the pool driver

---

### Upgrading to v0.3.0 (from v0.2.0)

**New features:** Cross-zone accessor binding, idempotent CreateFileShare, nsenter mount wrapper.

**CRD changes (additive):**
- `FileSharePool.spec.accessorZones` — new field for multi-zone mount targets
- `PoolShareStatus.mountTargets` — new status field (ZoneMountTarget array)

**Action required:**
1. Apply updated CRDs: `kubectl apply -f config/crd/`
2. Upgrade controller and node DaemonSet
3. **Node DaemonSet now requires `hostPID: true`** for the nsenter mount wrapper — verify your DaemonSet spec includes this

---

### Upgrading to v0.2.0 (from v0.1.0)

**New features:** Share tiering, per-PVC usage reporting, Prometheus metrics, auto-discovery.

**CRD changes (additive):**
- `FileSharePool.spec.tiers` — new optional field for tiered storage
- Various status fields for metrics

**Action required:**
1. Apply updated CRDs: `kubectl apply -f config/crd/`
2. Upgrade controller — metrics endpoint becomes available on `:8080/metrics`
3. (Optional) Configure Prometheus scraping — see [MONITORING.md](MONITORING.md)
4. (Optional) Configure tiered storage if needed

---

### Fresh Install: v0.1.0

Initial release. See [INSTALL.md](INSTALL.md) for first-time installation.

---

## Rollback Procedures

### Helm Rollback

```bash
# List revision history
helm history ibm-vpc-file-pool-csi -n kube-system

# Rollback to previous revision
helm rollback ibm-vpc-file-pool-csi <revision-number> -n kube-system
```

### Manual Rollback

```bash
# Reapply the previous version's manifests
kubectl apply -f config/controller/  # previous version
kubectl apply -f config/node/        # previous version
```

### CRD Rollback Caveats

**Helm does not roll back CRDs.** If the upgrade added new CRD fields, those fields remain in the CRD schema after rollback. This is generally safe because:

- New fields are optional with defaults
- The older controller version ignores unknown fields
- Existing CR data is preserved

If you need to roll back a CRD, apply the previous version's CRD manifests manually:

```bash
kubectl apply -f config/crd/  # from the previous version's checkout
```

**Warning:** Rolling back CRDs that removed fields can cause data loss in those fields. Always back up CRs before rollback (see pre-flight checklist).

---

## Post-Upgrade Verification

Run this checklist after every upgrade:

```bash
# 1. Verify all pods are running
kubectl get pods -n kube-system -l app=vpc-file-pool-csi-controller
kubectl get pods -n kube-system -l app=vpc-file-pool-csi-node

# 2. Verify all pools are Ready
kubectl get filesharepools

# 3. Verify CSI driver is registered
kubectl get csidriver vpc-file-pool.csi.ibm.io

# 4. Test PVC creation (create a test PVC and verify it binds)
kubectl apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: upgrade-test-pvc
  namespace: default
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 1Gi
EOF
kubectl get pvc upgrade-test-pvc -w  # Should bind within seconds

# 5. Verify metrics endpoint (if applicable)
kubectl port-forward -n kube-system svc/vpc-file-pool-csi-controller 8080:8080 &
curl -s http://localhost:8080/metrics | grep vpc_file_pool | head -5
kill %1

# 6. Clean up test PVC
kubectl delete pvc upgrade-test-pvc

# 7. Check controller logs for errors
kubectl logs -n kube-system -l app=vpc-file-pool-csi-controller -c csi-controller --tail=50 | grep -i "error\|fail"
```

---

## Downtime Expectations

| Component | Downtime | Impact |
|-----------|----------|--------|
| **Controller** | ~30 seconds | Leader election gap. New PVC creation pauses during the gap. Existing mounted PVCs are unaffected. |
| **Node DaemonSet** | Rolling update, ~60s per node | No mount disruption for existing pods. New `NodePublishVolume` calls queue until the new pod is ready. |
| **CRD apply** | None | CRD updates are live. Existing CRs continue to work. |
| **Webhook update** | ~10 seconds | CRD creation/update requests may briefly fail validation. Retry resolves this. |

**Zero-downtime for existing workloads.** Pods with mounted PVCs are not affected by controller or node agent upgrades — NFS mounts are kernel-level and persist independently of the CSI driver pods.

---

## See Also

- [CHANGELOG.md](CHANGELOG.md) — Full version history
- [INSTALL.md](INSTALL.md) — First-time installation
- [HELM-VALUES.md](HELM-VALUES.md) — Helm chart configuration reference
- [CRD-SPEC.md](CRD-SPEC.md) — CRD field definitions
- [DAY2-OPERATIONS.md](DAY2-OPERATIONS.md) — Post-upgrade operational procedures
