# Troubleshooting Guide — IBM VPC File Pool CSI Driver

## Quick Diagnostic Checklist

Run these five commands first when investigating any issue:

```bash
# 1. Are the driver pods running?
kubectl get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller
kubectl get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=node

# 2. What state are the pools in?
kubectl get filesharepools

# 3. What does the PVC say?
kubectl describe pvc <pvc-name>

# 4. Controller logs (last 100 lines)
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller --tail=100

# 5. Node agent logs on the affected node
NODE=$(kubectl get pod <pod-name> -o jsonpath='{.spec.nodeName}')
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=node --field-selector spec.nodeName=${NODE} -c csi-node --tail=100
```

---

## PVC Issues

### PVC Pending: "pool not found"

**Symptom:** PVC stays in `Pending` with event message `pool "xyz" not found`.

**Cause:** The `pool` parameter in the StorageClass doesn't match any `FileSharePool` CR name.

**Fix:**
```bash
# List available pools
kubectl get filesharepools

# Check the StorageClass pool parameter
kubectl get storageclass <sc-name> -o jsonpath='{.parameters.pool}'
```

Ensure the pool name in the StorageClass's `parameters.pool` exactly matches the `metadata.name` of a `FileSharePool` CR. Names are case-sensitive.

---

### PVC Pending: "pool has no available capacity"

**Symptom:** PVC stays in `Pending` with event message `pool has no available capacity`.

**Cause:** All shares in the pool are fully allocated and the pool cannot auto-expand (either `autoExpand: false` or `maxShares` reached).

**Fix:**
```bash
# Check pool utilization
kubectl get filesharepools
kubectl get filesharepool <pool-name> -o jsonpath='{.status.totalAllocatedGB}/{.status.totalCapacityGB}'

# Option 1: Increase maxShares to allow more shares
kubectl patch filesharepool <pool-name> --type merge -p '{"spec":{"maxShares":20}}'

# Option 2: Increase share size (triggers VPC API call, takes 30-90s)
kubectl patch filesharepool <pool-name> --type merge -p '{"spec":{"shareSizeGB":4000}}'

# Option 3: Delete unused PVCs to free capacity
kubectl get subvolumes -l storage.ibmcloud.io/pool=<pool-name> -o wide
```

---

### PVC Pending: "pool is expanding, retry shortly"

**Symptom:** PVC stays in `Pending` with event message `pool is expanding, retry shortly`.

**Cause:** A new VPC file share is being created. This is normal — VPC share creation takes 30-90 seconds. The PVC will bind once the share becomes stable.

**Fix:** Wait 2-3 minutes. If still pending after 5 minutes, check controller logs:

```bash
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller --tail=100 | grep -i "error\|fail\|share"
```

Common sub-causes for prolonged expansion:
- VPC API rate limiting (429 responses) — the controller retries automatically
- VPC file share quota exceeded (300 shares per account) — see [VPC API Issues](#vpc-api-issues)
- Invalid profile or zone in the pool spec

---

### PVC Pending: "tier not found"

**Symptom:** PVC stays in `Pending` with event message referencing an unknown tier.

**Cause:** The StorageClass references a `tier` parameter that doesn't match any tier defined in the pool's `spec.tiers` list.

**Fix:**
```bash
# Check available tiers in the pool
kubectl get filesharepool <pool-name> -o jsonpath='{.spec.tiers[*].name}'

# Check the StorageClass tier parameter
kubectl get storageclass <sc-name> -o jsonpath='{.parameters.tier}'
```

Ensure the tier name in the StorageClass matches a tier defined in the `FileSharePool` spec.

---

### PVC Pending: "VPC API authentication failed"

**Symptom:** PVC stays in `Pending` and controller logs show `authentication failed` or `401 Unauthorized`.

**Cause:** The API key secret is missing, invalid, or expired.

**Fix:**
```bash
# Verify the secret exists
kubectl get secret -n kube-system storage-secret-store
kubectl get secret -n kube-system ibm-cloud-credentials

# Verify the configmap exists
kubectl get configmap -n kube-system ibm-cloud-provider-data

# Test the API key (if you have access to it)
ibmcloud login --apikey <key> --no-region
ibmcloud is shares --output json | jq length
```

On managed clusters (ROKS/ROVS/IKS), the `storage-secret-store` secret is maintained by the platform. If it's missing, contact IBM Cloud support. On self-managed clusters, recreate the secret per the instructions in [API-KEY-SETUP.md](API-KEY-SETUP.md).

---

## VM Issues

### VM shows "No bootable device"

**Symptom:** VM starts and reaches `Running` status, but the console shows "No bootable device" or a UEFI shell instead of booting the OS.

**Cause:** The boot disk image is in **qcow2 format** instead of raw. KubeVirt passes filesystem-PVC images to QEMU with `"driver":"file"` (raw protocol, no qcow2 format layer). QEMU reads the qcow2 headers (`QFI\xfb` magic) as raw MBR/GPT data and finds no valid boot sector.

This happens when cloud images (distributed as qcow2) are written directly to the PVC without converting to raw. CDI converts the image automatically, and the pool's golden images are made by CDI.

**Diagnosis:**
```bash
# Check the disk format from the virt-launcher pod
LAUNCHER=$(kubectl get pods -n <namespace> -l kubevirt.io/domain=<vm-name> -o name)
kubectl exec -n <namespace> ${LAUNCHER} -c compute -- \
  dd if=/var/run/kubevirt-private/vmi-disks/rootdisk/disk.img bs=1 count=4 2>/dev/null | od -A x -t x1
# If output shows: 51 46 49 fb → qcow2 format (QFI magic). Should be raw.
```

**Fix:** If the image came from CDI or from the pool's golden images, check the DataVolume with `kubectl describe dv`. If you wrote the image yourself, stop the VM, re-run your downloader pod with qcow2→raw conversion, then restart:
```bash
virtctl stop <vm-name> -n <namespace>
# Run a pod with qemu-img to convert (see TUTORIAL.md Step 8b):
# qemu-img convert -f qcow2 -O raw /boot/disk.qcow2 /boot/disk.img
virtctl start <vm-name> -n <namespace>
```

See [TUTORIAL.md](TUTORIAL.md) Step 8b and [VM-DISK-FORMATS.md](VM-DISK-FORMATS.md) for details.

---

### VM with instant snapshots does not start

**Symptoms and causes:**
- `admission webhook "vm-hook.storage.ibmcloud.io" denied the request: ... need the KubeVirt Sidecar feature gate`: the VM's disks have instant snapshots and the `Sidecar` gate is off. Turn it on (see [VM-INSTANT-SNAPSHOTS.md](VM-INSTANT-SNAPSHOTS.md)), or remove the snapshots with `kubectl pool-snapshots flatten <vm> -n <namespace>`.
- `... a snapshot Job changes the disk now`: a Job adds or merges a snapshot file of the stopped VM. KubeVirt starts the VM when it is done.
- `sidecar feature gate is not enabled in kubevirt-config, invalid entry metadata.annotations.hooks.kubevirt.io/hookSidecars`: the gate was turned off after the VM got its hook. As above.
- `Could not open '/var/run/kubevirt-private/vmi-disks/<volume>/disk.img': Permission denied` (VMI event `SyncFailed`): the VM started without the hook, so it tried to write a frozen snapshot file. This is the safety stop: nothing was written. Check that the controller and its webhook run (`kubectl get mutatingwebhookconfiguration | grep vm-hook`), and that the ConfigMap `ibm-vpc-file-pool-vm-hook` exists in the VM's namespace, then restart the VM.

**Diagnosis:**
```bash
kubectl get subvolume <pv-name> -o jsonpath='{.status.diskChain}{"\n"}{.status.chainOperation}{"\n"}'
kubectl get vmi <vm> -n <ns> -o jsonpath='{.metadata.annotations.storage\.ibmcloud\.io/disk-tops}{"\n"}'
```

---

### OpenShift Virtualization UI: "no accessMode specified in StorageProfile"

**Symptom:** When creating a VM through the OpenShift Virtualization UI using the `ibm-vpc-file-pool` StorageClass, the UI shows:
> "no accessMode specified in StorageProfile ibm-vpc-file-pool"

**Cause:** KubeVirt's CDI auto-creates a `StorageProfile` CR for each StorageClass. For known CSI drivers, CDI auto-populates `claimPropertySets` (access modes + volume mode). For our custom driver, CDI can't determine the correct settings, so the field is empty and the UI doesn't know what access mode to use.

**Automatic fix (v0.11.0+):** The controller detects CDI at startup and automatically patches the StorageProfile with `claimPropertySets: [{accessModes: ["ReadWriteMany"], volumeMode: Filesystem}]` during reconciliation. No manual step required.

**Manual workaround (pre-v0.11.0):**
```bash
kubectl patch storageprofile ibm-vpc-file-pool --type merge \
  -p '{"spec":{"claimPropertySets":[{"accessModes":["ReadWriteMany"],"volumeMode":"Filesystem"}]}}'
```

**Verification:**
```bash
kubectl get storageprofile ibm-vpc-file-pool -o jsonpath='{.spec.claimPropertySets}'
# Should output: [{"accessModes":["ReadWriteMany"],"volumeMode":"Filesystem"}]
```

---

### OpenShift Virtualization UI: "no accessMode specified in StorageProfile" (other StorageClasses)

**Symptom:** When creating a VM that references a non-pool StorageClass (e.g. `ibmc-vpc-file-1000-iops`, `ibmc-vpc-file-5-iops`), the UI shows:
> "no accessMode specified in StorageProfile ibmc-vpc-file-1000-iops"

**Cause:** CDI creates a StorageProfile for every StorageClass, but can't auto-detect access modes for all CSI drivers. The stock IBM VPC File CSI StorageClasses may also be missing `claimPropertySets`.

**Note:** The pool CSI controller only auto-patches StorageProfiles for its own pool StorageClasses. Third-party or stock StorageClasses must be patched manually.

**Fix:**
```bash
# Patch the stock IBM VPC File CSI StorageProfiles
kubectl patch storageprofile ibmc-vpc-file-1000-iops --type merge \
  -p '{"spec":{"claimPropertySets":[{"accessModes":["ReadWriteMany"],"volumeMode":"Filesystem"}]}}'

# Repeat for other stock StorageClasses as needed
kubectl patch storageprofile ibmc-vpc-file-5-iops --type merge \
  -p '{"spec":{"claimPropertySets":[{"accessModes":["ReadWriteMany"],"volumeMode":"Filesystem"}]}}'
```

**Verification:**
```bash
kubectl get storageprofile <name> -o jsonpath='{.spec.claimPropertySets}'
# Should output: [{"accessModes":["ReadWriteMany"],"volumeMode":"Filesystem"}]
```

### CDI copy pod OOMKilled when a VM disk is copied onto a pool

**Symptom:** A VM made from a catalog image with its boot disk on a pool does not start. The DataVolume keeps restarting. The CDI copy pod shows `OOMKilled`. The progress goes back to zero each time.

**Cause:** CDI's copy pods have a default memory limit of 600 MB. When they write onto NFS, the file cache counts against that limit, so the pod is killed. The controller keeps the limit at least 8Gi through `HyperConverged` `spec.resourceRequirements.storageWorkloads`. The limit is too low if the check is off (`cdi.copyPodMemoryLimit` is empty), if the controller cannot patch `HyperConverged`, or if something put the old value back, such as an update of IBM's add-on on ROVS.

**Check:**
```bash
# The limit CDI uses (should show limits.memory of 8Gi or more)
oc get cdi -o jsonpath='{.items[0].spec.config.podResourceRequirements}'

# The HyperConverged setting the controller manages
oc get hyperconverged -n openshift-cnv kubevirt-hyperconverged \
  -o jsonpath='{.spec.resourceRequirements.storageWorkloads}'

# Did the controller raise the limit? Look for a warning in its log, and check the metric
# vpc_file_pool_cdi_copy_limit_raised_total
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller | grep -i "CDI copy pod memory limit"
```

**Fix:**
1. Check that `cdi.copyPodMemoryLimit` is not empty (default `8Gi`).
2. Check the controller's ClusterRole allows `get`, `list` and `patch` on `hco.kubevirt.io` `hyperconvergeds`.
3. The controller corrects the limit within 5 minutes. Restart the controller to make it check at once.
4. If a raise happens again and again, something resets the value. On ROVS, IBM says the add-on cannot be modified, so ask IBM support.
5. Delete the failed DataVolume and create the VM again. The copy starts from zero.

---

## Share Issues

### Share has no mount target IP

**Symptom:** A share in pool status shows `ipAddress: null` or empty `mountTargetIP`. PVCs allocated to this share have PVs with an empty `server` field, causing NFS mount failures.

**Cause:** The share's mount target was never created, failed, or does not yet have a mount path. Before this release, shares created by auto-expansion had no mount target at all. The pool shows phase `Degraded`, and its `SharesReady` condition has reason `NoMountTarget` and names the shares. Volume requests get a retriable error and the pool does not add shares.

**Automatic recovery:** The reconciler finds stable shares with no mount target and creates one in security group access mode, in the worker subnet of the pool's zone. If that fails, it tries again on the next reconcile and logs `Failed to create mount target for share`.

**Manual check:**
```bash
# Shares and their mount target addresses
kubectl get fsp <pool-name> -o jsonpath='{range .status.shares[*]}{.shareID}{"\t"}{.mountTargetIP}{"\t"}{.exportPath}{"\t"}{.state}{"\n"}{end}'
kubectl get fsp <pool-name> -o jsonpath='{.status.conditions[?(@.type=="SharesReady")].message}'

# The share's access mode and mount targets
ibmcloud is share <share-id> --output json | jq '.access_control_mode, .mount_targets'
```

**Common causes of a failed mount target:**
- No worker subnet in the pool's zone. The controller log says `no cluster subnet in the pool's zone`. Set `spec.zone` to a zone that has workers.
- The cluster security group cannot be found (`security group "kube-<id>" not found`). Set `spec.securityGroupIDs` on the pool.
- The share uses **VPC access mode** (`access_control_mode` is `vpc`). IBM no longer allows new VPC-mode mount targets. The share must be converted to security group mode.

---

### PV has empty NFS server field

**Symptom:** Pod stuck in `ContainerCreating` with mount error. The PV's `spec.csi.volumeAttributes.server` is empty.

**Cause (pre-v0.11.0):** `selectShare` did not filter by `MountTargetIP`. A share with no mount target was selected for allocation.

**Fix:** Upgrade to v0.11.0+ (adds the mount target IP guard). For existing broken PVs, delete the PVC and recreate it — the new allocation will only pick shares with populated mount target IPs.

```bash
# Check if a PV has an empty server
kubectl get pv <pv-name> -o jsonpath='{.spec.csi.volumeAttributes.server}'

# If empty, delete and recreate the PVC
kubectl delete pvc <pvc-name> -n <namespace>
# Recreate the PVC YAML — the new allocation picks a healthy share
```

---

## Mount Issues

### NFS mount failed

**Symptom:** Pod stuck in `ContainerCreating` with event `MountVolume.SetUp failed: mount failed: exit status 32`.

**Cause:** The node agent cannot mount the NFS share. Almost always a networking issue.

**Fix:**

1. **Check security groups** — TCP port 2049 must be allowed between worker nodes and VPC file share mount targets:
```bash
# Find the security group attached to your worker nodes
ibmcloud ks worker ls --cluster <cluster-name> --output json | jq '.[0].networkInterfaces[0].securityGroups'

# Check rules on that security group
ibmcloud is security-group-rules <sg-id> --output json | jq '.[] | select(.port_min <= 2049 and .port_max >= 2049)'
```

2. **Test connectivity from the node**:
```bash
# Get the mount target IP for the share
kubectl get subvolume <pvc-name> -o jsonpath='{.spec.mountTargetIP}'

# Debug from the worker node
kubectl debug node/<node-name> -it --image=busybox -- sh -c "nc -zv <mount-target-ip> 2049"
```

3. **Check VPC network ACLs** — subnet-level ACLs can also block NFS traffic:
```bash
ibmcloud is subnet <subnet-id> --output json | jq '.network_acl'
```

---

### Permission denied creating subdirectory (mkdir .../pvcs: permission denied)

**Symptom:** Pod stuck in `ContainerCreating` with event `MountVolume.SetUp failed ... mkdir .../globalmount/pvcs: permission denied`.

**Cause (pre-v0.9.0, and security group mode before this release):** The driver was mounting the NFS server root (`server:/`) instead of the share-specific export path (`server:/share_abc123`). The export path is specific to each share in both access modes. The NFS server root is owned by UID 99, mode 0711, so the CSI node (mapped to UID 65534 by root_squash) cannot create directories there.

**Fix:** Upgrade to v0.9.0+. The driver now correctly parses and uses the export path from the VPC API's `MountPath` field. Existing shares are automatically backfilled during health checks.

**Verification:**
```bash
# Check the PV volume context — "share" should NOT be "/"
kubectl get pv <pv-name> -o jsonpath='{.spec.csi.volumeAttributes.share}'
# Should output something like: /7a5c6b10_6c24_48a5_9eff_d58978cabd4b

# Check the pool share status
kubectl get filesharepool <pool-name> -o jsonpath='{.status.shares[0].exportPath}'
```

---

### Permission denied on mount

**Symptom:** Pod starts but gets `permission denied` when writing to the volume.

**Cause:** The UID/GID in the pod's `securityContext` doesn't match the subdirectory ownership set by the StorageClass.

**Fix:**
```bash
# Check what UID/GID the subdirectory was created with
kubectl get subvolume <pvc-name> -o jsonpath='uid={.spec.uid} gid={.spec.gid}'

# Check the pod's security context
kubectl get pod <pod-name> -o jsonpath='{.spec.containers[0].securityContext}'
```

Solutions:
- Set `runAsUser`/`runAsGroup` in the pod spec to match the StorageClass `uid`/`gid`
- Use `fsGroup` in the pod's security context to have Kubernetes chown the mount
- Create a StorageClass with `uid` and `gid` matching your application's requirements

---

### Stale NFS mount after share recovery

**Symptom:** Pod gets `Stale file handle` errors after a VPC file share was temporarily unavailable.

**Cause:** The NFS client kernel handle is stale. Since the driver uses `soft` mount options, the mount should eventually time out, but the kernel can cache the stale handle.

**Fix:**
```bash
# Identify the node where the pod runs
NODE=$(kubectl get pod <pod-name> -o jsonpath='{.spec.nodeName}')

# Check the node agent logs for the mount state
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=node --field-selector spec.nodeName=${NODE} -c csi-node --tail=50

# The simplest fix is to delete the pod — the kubelet will re-mount on restart
kubectl delete pod <pod-name>
```

If many pods on the same node are affected, the node agent will detect the stale mount during the next staging operation and remount.

---

### NFS mount fails with nsenter/namespace errors

**Symptom:** Pod stuck in `ContainerCreating` and node agent logs show errors related to `nsenter`, `mount namespace`, or `/proc/1/ns/mnt`.

**Cause:** The node DaemonSet is not running with `hostPID: true`, so the nsenter mount wrapper cannot access the host's mount namespace via `/proc/1`.

**Fix:**
```bash
# Verify hostPID is enabled on the DaemonSet
kubectl get daemonset -n kube-system ibm-vpc-file-pool-csi-node -o jsonpath='{.spec.template.spec.hostPID}'
# Should output: true

# If false, patch it:
kubectl patch daemonset -n kube-system ibm-vpc-file-pool-csi-node --type merge \
  -p '{"spec":{"template":{"spec":{"hostPID":true}}}}'
```

The nsenter wrapper at `/usr/local/bin/mount` routes NFS mounts through `nsenter --mount=/proc/1/ns/mnt --root=/proc/1/root` (host namespace) and bind mounts through the container's local `/usr/bin/mount`.

---

### Mount stuck — pod hangs on I/O

**Symptom:** Pod hangs indefinitely on file operations. `kubectl exec` into the pod also hangs.

**Cause:** If the NFS mount was configured with `hard` instead of `soft`, the kernel retries indefinitely when the NFS server is unreachable.

**Fix:** The driver defaults to `soft` mounts to prevent this scenario. Custom mount flags from the StorageClass are merged with the safe defaults, so `soft` is always present unless the StorageClass explicitly includes `hard`. If you see this:

1. Verify the StorageClass `mountOptions` do not include `hard`:
```bash
kubectl get storageclass <sc-name> -o jsonpath='{.mountOptions}'
```

2. If `hard` was specified, update the StorageClass to remove it (or replace with `soft`) and recreate affected PVCs
3. For immediate relief, force-delete the stuck pod:
```bash
kubectl delete pod <pod-name> --force --grace-period=0
```

---

## Pool Issues

### Pool stuck in Initializing

**Symptom:** `kubectl get filesharepools` shows phase `Initializing` for more than 5 minutes.

**Cause:** The initial VPC file share creation is failing.

**Fix:**
```bash
# Check pool events
kubectl describe filesharepool <pool-name>

# Check controller logs for VPC API errors
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller --tail=100 | grep -i "error\|fail"
```

Common causes:
| Log Message | Cause | Fix |
|-------------|-------|-----|
| `authentication failed` | Invalid API key | See [PVC Pending: "VPC API authentication failed"](#pvc-pending-vpc-api-authentication-failed) |
| `quota exceeded` | 300 share account limit | Delete unused shares in VPC console or via `ibmcloud is shares` |
| `zone not found` | Invalid zone in pool spec | Verify zone matches your worker nodes: `kubectl get nodes -o jsonpath='{.items[*].metadata.labels.topology\.kubernetes\.io/zone}'` |
| `profile not found` | Invalid storage profile | Use `ibmcloud is share-profiles` to list valid profiles |
| `security group` or `2049` | NFS port blocked | Add TCP 2049 inbound rule to worker node security groups |

---

### Pool in Degraded phase

**Symptom:** `kubectl get filesharepools` shows phase `Degraded`.

**Cause:** One or more VPC file shares in the pool are unhealthy (the VPC API reports their lifecycle state as something other than `stable`).

**Fix:**
```bash
# Identify the unhealthy share(s)
kubectl get filesharepool <pool-name> -o jsonpath='{range .status.shares[*]}{.shareID}{"\t"}{.state}{"\n"}{end}'

# Check the share in VPC
ibmcloud is share <share-id> --output json | jq '{lifecycle_state, health_state, health_reasons}'
```

The pool manager automatically stops allocating new PVCs to degraded shares. Existing PVCs on the share continue to work if the NFS mount is still functional. Once the VPC API reports the share as `stable` again, the pool phase returns to `Ready`.

If the share is permanently failed, the pool manager will eventually need to migrate affected SubVolumes. This is currently a manual process — delete the affected SubVolumes and let the PVCs rebind.

---

### Pool Expanding timeout

**Symptom:** Pool stays in `Expanding` phase for more than 10 minutes.

**Cause:** A new VPC file share is being created but the creation is taking longer than expected, or the VPC API call failed silently.

**Fix:**
```bash
# Check controller logs for the expansion status
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller --tail=200 | grep -i "expand\|share\|create"

# Check if the share was created in VPC but not yet detected
ibmcloud is shares --output json | jq '.[] | select(.name | contains("<pool-name>"))'
```

The controller retries share creation automatically. If it's stuck, check VPC API quotas and rate limits.

---

### Pool Full — maxShares reached

**Symptom:** Pool phase is `Full`. New PVCs fail with `pool has no available capacity`.

**Cause:** The pool has created the maximum number of shares (`maxShares`) and all are fully allocated.

**Options:**
```bash
# Option 1: Increase maxShares (check your VPC quota first)
ibmcloud is shares --output json | jq length  # Current total shares
kubectl patch filesharepool <pool-name> --type merge -p '{"spec":{"maxShares":20}}'

# Option 2: Increase share size (existing shares grow)
kubectl patch filesharepool <pool-name> --type merge -p '{"spec":{"shareSizeGB":4000}}'

# Option 3: Identify and remove unused PVCs
kubectl get subvolumes -l storage.ibmcloud.io/pool=<pool-name> --sort-by=.metadata.creationTimestamp
```

---

### Pool deletion blocked by active SubVolumes or snapshots

**Symptom:** `kubectl delete filesharepool <name>` hangs or the pool stays in `Terminating`. The pool's `DeletionBlocked` condition says why: `ActiveSubVolumes` or `SnapshotsExist`.

**Cause:** The pool has a finalizer that prevents deletion while SubVolumes still reference its shares, or while snapshots keep their data on them. Deleting the shares would delete that data.

If the reason is `SnapshotsExist`, delete the pool's VolumeSnapshots first:
```bash
kubectl get snapshots.storage.ibmcloud.io -o json |
  jq -r '.items[] | select(.spec.poolName=="<pool-name>") | .metadata.name'
# Find the VolumeSnapshot of each one through its VolumeSnapshotContent, and delete it.
```

**Fix:**
```bash
# List SubVolumes in the pool
kubectl get subvolumes -l storage.ibmcloud.io/pool=<pool-name>

# Delete all PVCs backed by this pool first
kubectl delete pvc -l storageClassName=<sc-name> --all-namespaces

# Wait for SubVolumes to be cleaned up
kubectl get subvolumes -l storage.ibmcloud.io/pool=<pool-name> -w

# Then retry pool deletion
kubectl delete filesharepool <pool-name>
```

If SubVolumes are stuck in a finalizer loop, check the controller logs for errors during cleanup.

---

## Controller Issues

### Controller CrashLoopBackOff

**Symptom:** Controller pod is in `CrashLoopBackOff`.

**Fix:**
```bash
# Check the previous crash logs
kubectl logs -n kube-system <controller-pod> -c csi-controller --previous

# Check all containers in the pod
kubectl describe pod -n kube-system <controller-pod>
```

Common causes:

| Log Message | Cause | Fix |
|-------------|-------|-----|
| `failed to load credentials` | Missing API key secret | Verify `storage-secret-store` or `ibm-cloud-credentials` secret exists in `kube-system` |
| `dial unix /var/lib/csi/sockets/...` | CSI socket not ready | Restart the pod; check that volumes are mounted correctly |
| `panic: assignment to entry in nil map` | Bug in initialization | File an issue with full stack trace |
| `OOMKilled` | Insufficient memory | Increase controller memory limit in Helm values or deployment manifest |

---

### Leader election issues

**Symptom:** Multiple controller replicas running but no PVCs being processed.

**Cause:** Leader election failed or is stuck. Only the leader processes requests.

**Fix:**
```bash
# Check which pod holds the leader lease
kubectl get lease -n kube-system vpc-file-pool-csi-leader -o yaml

# Check controller logs for leader election messages
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller --tail=50 | grep -i "leader"
```

If the leader lease is held by a dead pod, delete the lease to force re-election:
```bash
kubectl delete lease -n kube-system vpc-file-pool-csi-leader
```

---

### CSI sidecar connectivity

**Symptom:** PVCs stay in `Pending` with no events from the provisioner.

**Cause:** The CSI provisioner sidecar can't communicate with the driver's gRPC socket.

**Fix:**
```bash
# Check sidecar logs
kubectl logs -n kube-system <controller-pod> -c csi-provisioner --tail=50

# Verify the CSI driver is registered
kubectl get csidriver vpc-file-pool.csi.ibm.io

# Check all containers are running in the controller pod
kubectl get pod -n kube-system <controller-pod> -o jsonpath='{range .status.containerStatuses[*]}{.name}{"\t"}{.ready}{"\n"}{end}'
```

---

## Data Issues

### Cross-PVC data visibility

**Concern:** Can pods access data from other PVCs on the same share?

**Answer:** No, under normal operation. Each PVC is bind-mounted at its own subdirectory (`/pvcs/pvc-<uuid>`). The kernel enforces bind-mount boundaries — a process cannot traverse above the mount point.

**However:** A privileged pod (`privileged: true`) with host filesystem access could theoretically access the NFS staging mount and see all subdirectories. Mitigations:
- Use `PodSecurityAdmission` (Kubernetes) or `SecurityContextConstraints` (OpenShift) to prevent privileged pods in tenant namespaces
- Use separate pools for workloads with different trust levels
- Use the `spread` allocation strategy to minimize how many PVCs share a single share

---

### Orphaned SubVolume CRs

**Symptom:** `kubectl get subvolumes` shows SubVolume CRs whose corresponding PVC no longer exists.

**Cause:** The PVC deletion event was missed by the controller (e.g., controller was down during deletion), or the `DeleteVolume` gRPC call failed.

**Fix:**
```bash
# Find orphaned SubVolumes (SubVolume exists but PVC doesn't)
for sv in $(kubectl get subvolumes -o jsonpath='{.items[*].metadata.name}'); do
  ns=$(kubectl get subvolume $sv -o jsonpath='{.spec.pvcNamespace}')
  pvc=$(kubectl get subvolume $sv -o jsonpath='{.spec.pvcName}')
  if ! kubectl get pvc -n $ns $pvc &>/dev/null; then
    echo "Orphaned: $sv (PVC $ns/$pvc not found)"
  fi
done

# Delete orphaned SubVolumes (this frees pool capacity but does NOT delete the subdirectory data)
kubectl delete subvolume <name>

# To also clean up the subdirectory data, note the share and path before deleting:
kubectl get subvolume <name> -o jsonpath='Share: {.spec.shareID}, Path: {.spec.subPath}'
```

---

### Orphaned subdirectories on NFS shares

**Symptom:** Subdirectories exist on the NFS share but no corresponding SubVolume or PVC exists.

**Cause:** The SubVolume CR was deleted (perhaps manually) but the subdirectory cleanup failed or was skipped.

**Detection:**
```bash
# Mount the share and list subdirectories (from a debug pod)
kubectl debug node/<node-name> -it --image=busybox -- sh
ls /var/lib/kubelet/plugins/vpc-file-pool.csi.ibm.io/staging/<share-id>/pvcs/

# Compare with known SubVolumes
kubectl get subvolumes -l storage.ibmcloud.io/share-id=<share-id> -o jsonpath='{.items[*].spec.subPath}'
```

**Cleanup:** Orphaned subdirectories consume share capacity but don't affect pool tracking (the pool counts capacity from SubVolume CRs, not filesystem usage). To reclaim space, delete the orphaned directories from a debug pod or via direct NFS access.

---

## VPC API Issues

### 403 Forbidden (IAM permissions)

**Symptom:** Controller logs show `403 Forbidden` or `not authorized` from VPC API calls.

**Cause:** The API key's IAM role doesn't have sufficient VPC Infrastructure permissions.

**Fix:**

Required IAM roles:
- **VPC Infrastructure Services: Editor** — minimum for creating/managing file shares
- **VPC Infrastructure Services: Administrator** — needed if managing security groups

```bash
# Check the service ID's policies
ibmcloud iam service-policies <service-id>

# Add Editor role for VPC Infrastructure
ibmcloud iam service-policy-create <service-id> --roles Editor --service-name is
```

---

### Quota exceeded (300 shares)

**Symptom:** Controller logs show `quota exceeded` or `shares_per_account limit reached`.

**Cause:** VPC accounts have a default limit of 300 file shares. This includes shares from all sources (pool CSI, stock CSI, manually created).

**Fix:**
```bash
# Check current share count
ibmcloud is shares --output json | jq length

# List shares to find candidates for cleanup
ibmcloud is shares --output json | jq '.[] | {name, id, size, created_at, lifecycle_state}'

# Delete unused shares
ibmcloud is share-delete <share-id> --force
```

To request a quota increase, open an IBM Cloud support ticket.

**Prevention:** Budget your 300-share quota across pools. If you have 3 pools with `maxShares: 10` each, that's 30 shares for the pool CSI. Leave room for the stock CSI driver and any manually created shares.

---

### Rate limiting (429 Too Many Requests)

**Symptom:** Controller logs show `429` responses or `rate limit exceeded` from VPC API calls.

**Cause:** The VPC API has per-account rate limits. Multiple controllers or frequent reconciliation can hit these limits.

**Fix:** The driver includes built-in rate limiting and automatic retry with exponential backoff. If you're still hitting limits:

1. Check if other VPC API consumers are using the same account (Terraform, other controllers)
2. Reduce the number of pools or increase the reconciliation interval
3. Set `initialShares` high enough to avoid burst creation during initial setup

---

## Backup and Replication Issues

### Backups stopped: the snapshot pool is above 85% full

**Symptom:** `kubectl get backuppolicies` shows phase `Error`, and no new backups appear.

```bash
kubectl get backuppolicy <name> -o jsonpath='{.status.phase}{"\n"}{.status.message}{"\n"}'
```

The message says how full the share is. At 85% the policy stops new backups. It never deletes backups to make room.

**Fix:** make room, and the backups resume by themselves when use falls to 85% or less.
- Shorten `spec.keepFor` on the policy, or remove the backups of a volume you no longer need (`kubectl backup delete`, see [BACKUP-RECOVERY.md](BACKUP-RECOVERY.md)).
- Or add shares to the snapshot pool.

The alert `VPCFilePoolSnapshotPoolNearlyFull` fires at 75% so you can act first.

### A volume has no recent backup

```bash
kubectl backup list --policy <name>
kubectl get backuppolicy <name> -o yaml    # status.volumes[].lastError is the last error of each volume
```

Check `status.phase` first (a `Suspended` policy makes no backups). Then read `status.volumes[].lastError` for the volume. The controller log has the line `Failed to create replication resources` when a copy could not start.

### Replication is late or failing

```bash
kubectl get replicationpolicies
kubectl get replicationpolicy <name> -o jsonpath='{.status.phase}{"\n"}{.status.consecutiveFailures}{"\n"}{.status.lastError}{"\n"}'
```

- Phase `Paused` with the annotation `storage.ibmcloud.io/paused: "true"` was paused by a person. Remove it with `kubectl annotate replicationpolicy <name> storage.ibmcloud.io/paused-`.
- A high `consecutiveFailures` with a `lastError` is a failing copy. The error names the cause (for example the destination is not reachable).
- Check the destination, then look at the controller log for the policy name.

---

## Metrics Issues

### Prometheus not scraping metrics

**Symptom:** No `vpc_file_pool_*` metrics in Prometheus.

**Fix:**

1. **Verify the metrics endpoint is up:**
```bash
kubectl port-forward -n kube-system svc/vpc-file-pool-csi-controller 8080:8080
curl http://localhost:8080/metrics | grep vpc_file_pool
```

2. **Check ServiceMonitor** (if using Prometheus Operator):
```bash
kubectl get servicemonitor -n kube-system vpc-file-pool-csi
```

3. **Check Prometheus targets** — look for the controller's metrics endpoint in the Prometheus UI under Status > Targets

4. **Manual scrape config** (if not using Prometheus Operator):
```yaml
- job_name: 'vpc-file-pool-csi'
  kubernetes_sd_configs:
    - role: pod
      namespaces:
        names: ['kube-system']
  relabel_configs:
    - source_labels: [__meta_kubernetes_pod_label_app]
      regex: vpc-file-pool-csi-controller
      action: keep
    - source_labels: [__meta_kubernetes_pod_container_port_number]
      regex: "8080"
      action: keep
```

---

### Stale metrics

**Symptom:** Metrics show values that don't match reality (e.g., `vpc_file_pool_pvc_count` disagrees with `kubectl get subvolumes | wc -l`).

**Cause:** Metrics are updated during the reconciliation loop. If reconciliation is paused or failing, metrics become stale.

**Fix:**
```bash
# Check the last reconciliation time
kubectl get filesharepool <pool-name> -o jsonpath='{.status.lastReconcileTime}'

# Force reconciliation by annotating the pool
kubectl annotate filesharepool <pool-name> reconcile-trigger=$(date +%s) --overwrite
```

---

## Log Reference

### klog V-levels

The driver uses `klog/v2` structured logging. Control verbosity with the `--v` flag or Helm value `logLevel`.

| Level | What It Logs | When to Use |
|-------|-------------|-------------|
| `V(0)` | Errors and critical warnings | Always on (default) |
| `V(2)` | Normal operations: share selected, subdir created, PVC allocated | Default for production |
| `V(4)` | Detailed: full request/response, capacity calculations, share scoring | Debugging allocation issues |
| `V(6)` | Trace: function entry/exit, lock acquisition, every API call | Debugging timing/concurrency |

### Enabling verbose logging

**Helm:**
```bash
helm upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi/ \
  --namespace kube-system \
  --set logLevel=4
```

**Raw manifests:**
```bash
# Edit the controller deployment
kubectl edit deployment -n kube-system vpc-file-pool-csi-controller
# Add --v=4 to the container args
```

**Temporarily (no restart):**
```bash
# Not supported — klog doesn't support runtime level changes.
# You must restart the pod with the new --v flag.
```

### Log filtering tips

```bash
# Allocation events
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller | grep "Allocated\|Deallocated"

# VPC API calls
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller | grep "vpc\|api\|share"

# Errors only
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller | grep -i "error\|fail\|panic"

# Specific PVC
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller -c csi-controller | grep "pvc-<uuid>"

# Node mount operations
kubectl logs -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=node -c csi-node | grep "mount\|stage\|publish"
```

---

## Getting Help

If you cannot resolve the issue:

1. Collect diagnostics with the script from the repository:
```bash
bash scripts/collect-diagnostics.sh
```
It writes `csi-diagnostics-<time>.txt`. It holds driver versions, pools, volumes, snapshots, backup and replication policies, driver logs and recent events. It never reads a Secret and it removes values that look like keys or tokens. Read the file before you send it.

2. Open a GitHub issue with:
   - Cluster type (IKS/ROKS/ROVS) and version
   - Driver version (image tag)
   - The `csi-diagnostics-<time>.txt` file
   - Steps to reproduce

See [SUPPORT.md](SUPPORT.md) for where to start.
