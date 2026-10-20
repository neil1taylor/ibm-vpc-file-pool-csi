# Golden Image Workflow for KubeVirt VMs

This document explains how to configure golden OS images for one-click VM creation from the OpenShift Virtualization UI or CLI.

## Concepts

The OpenShift Virtualization catalog offers two ways to create a VM, each backed by a different Kubernetes resource:

| | DataSource (Image) | Template |
|---|---|---|
| **What it is** | A pointer to a bootable disk image (PVC or VolumeSnapshot) | A complete VM blueprint (CPU, memory, disks, network, cloud-init) |
| **Catalog tab** | **InstanceTypes** — user picks an image + an instance type (CPU/RAM preset) | **Templates** — user clicks and gets a fully configured VM |
| **API** | `cdi.kubevirt.io/v1beta1 DataSource` | `template.openshift.io/v1 Template` |
| **Flexibility** | High — any image can be paired with any instance type | Low — opinionated, ready-to-go |
| **Use case** | Power users who want to choose their own sizing | Quick-start VMs with sensible defaults |

The golden image syncer creates **both** so that pool-backed images appear in both catalog tabs.

## Background

CDI (Containerized Data Importer) manages golden OS images for OpenShift Virtualization. On clusters where the default StorageClass is ODF/Ceph, CDI imports and snapshots golden images on Ceph. These Ceph-backed golden images cannot be cloned to NFS pool storage, which causes the "Create VM" UI flow to fail with `UnrecognizedDataSourceKind`.

The driver supports two modes for golden image availability, selected automatically based on whether the pool StorageClass is the cluster default.

## Mode 1: Native CDI (Recommended)

**When:** The pool StorageClass is annotated as the cluster default.

In this mode, CDI handles everything natively:
- CDI `DataImportCron` imports OS images directly onto pool storage
- CDI creates `VolumeSnapshot` via the CSI snapshotter sidecar
- VM creation uses CDI clone (CSI `CreateVolumeFromSnapshot`)
- Cross-namespace cloning works via CDI host-assisted cloning

### Setup

```bash
# 1. Make the pool StorageClass the default
kubectl annotate sc ibm-vpc-file-pool \
  storageclass.kubernetes.io/is-default-class=true

# 2. Remove default from old StorageClass (only one default allowed)
kubectl annotate sc ocs-storagecluster-cephfs \
  storageclass.kubernetes.io/is-default-class-

# 3. Delete existing CDI DataVolumes so DataImportCrons re-create them
#    on the new default StorageClass
kubectl delete dv --all -n openshift-virtualization-os-images
```

### Verification

```bash
# Wait ~5 minutes per image for CDI to re-import
kubectl get pvc -n openshift-virtualization-os-images
kubectl get volumesnapshots -n openshift-virtualization-os-images

# Check controller logs
kubectl logs deploy/ibm-vpc-file-pool-csi-controller -c controller | grep golden
```

Then test from the UI: **Virtualization > Catalog > Select OS > Create VirtualMachine**.

## Mode 2: Custom Syncer

**When:** Another StorageClass (e.g., ODF/Ceph) is the cluster default.

The controller runs a background syncer that:
1. Discovers OS images from CDI `DataImportCron` resources
2. Creates golden image PVCs on the pool StorageClass
3. Runs converter Jobs to download and convert qcow2 images to raw format
4. Creates OpenShift Templates for VM creation from the UI catalog

### Configuration

Add `goldenImages` to your `FileSharePool` spec:

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-purpose
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 1000
  maxShares: 10
  defaultPermissions: "0777"
  defaultUID: 107
  defaultGID: 107
  goldenImages:
    enabled: true
    targetNamespaces:
      - pool-tutorial
      - default
    # Optional: only sync specific images (substring match)
    # imageFilter:
    #   - centos
    #   - fedora
    # Optional: converter image (default: quay.io/centos/centos:stream9)
    # converterImage: quay.io/centos/centos:stream9
    # Optional: golden PVC size (default: 15 GB)
    # pvcSizeGB: 15
```

### What the Syncer Creates

For each discovered image and each target namespace:

| Resource | Where | Name | Purpose |
|----------|-------|------|---------|
| PVC | target namespace | `golden-{imageName}` | Holds the raw disk image |
| Job | driver namespace (`kube-system`) | `share-data-golden-{namespace}-golden-{imageName}` (shortened with a hash when long) | Downloads, decompresses, and converts to raw. Removed when it finishes. |
| DataSource | `openshift-virtualization-os-images` | `{imageName}-{namespace}-nfs-pool` | CDI DataSource pointing to the golden PVC (enables InstanceTypes tab) |
| Template | target namespace | `{imageName}-nfs-pool` | OpenShift VM template for UI catalog |

The converter Job runs in the driver's namespace and mounts the share directly, so the target namespaces need no `anyuid` grant.

When you delete the pool, the driver deletes its golden PVCs, Templates and DataSources first, then the pool. Running converter Jobs are stopped before their PVC is deleted. The pool still waits for every other volume on it, such as VM disks, to be deleted.

Two pools must not use the same target namespace: the golden PVC and Template names do not include the pool name, so the second pool reuses the first pool's objects.

The converter Job handles the full OCI-to-raw pipeline:
1. Downloads OCI image from the container registry via `skopeo`
2. Decompresses gzip-compressed OCI layers (standard OCI format)
3. Detects disk format via `qemu-img info` (qcow2 or raw)
4. Converts qcow2 to raw if needed, or moves raw images directly
5. Writes `disk.img` to the golden PVC with `chmod 0666`

Decompression writes directly to the golden PVC's folder on the share to avoid OOM — emptyDir staging (tmpfs) uses pod memory and large images exceed the memory limit.

### Verification

```bash
# Check converter Jobs
kubectl get jobs -n pool-tutorial -l pool.storage.ibmcloud.io/golden-image=true

# Check golden PVCs
kubectl get pvc -n pool-tutorial -l pool.storage.ibmcloud.io/golden-image=true

# Check Templates
oc get templates -n pool-tutorial

# Check pool status
kubectl get fsp general-purpose -o jsonpath='{.status.goldenImages}' | jq .
```

### Creating VMs

**From the UI:**
1. Navigate to **Virtualization > Catalog**
2. Find the template (e.g., `centos-stream9-nfs-pool`)
3. Click **Create VirtualMachine**
4. The boot PVC is cloned from the golden image via CSI clone

**From the CLI:**
```bash
oc process centos-stream9-nfs-pool -n pool-tutorial \
  -p NAME=my-vm \
  -p CLOUD_USER_PASSWORD=mypassword \
  | oc apply -n pool-tutorial -f -
```

## Testing

The E2E VM clone test validates the full syncer-driven golden image pipeline:

```bash
# Run the test (requires ROKS or ROVS + OpenShift Virtualization + CDI)
make test-vm

# With options
./test/e2e/test-vm-clone.sh --zone eu-de-1 --keep

# Available flags
#   --keep          Don't clean up on success
#   --namespace NS  Override namespace (default: e2e-vm-test)
#   --pool POOL     Override pool name (default: e2e-vm-pool)
#   --zone ZONE     Override zone (default: eu-de-1)
#   --image FILTER  Override image filter (default: centos-stream-9)
#   --timeout SECS  Override total timeout (default: 900)
```

The test creates a pool with `goldenImages.enabled=true` and lets the syncer handle image discovery, PVC creation, converter Jobs, and template creation. It then creates a VM from the syncer-generated template, waits for the CSI clone Job to complete, and verifies the VM boots with connectivity checks.

Runtime: ~13-14 minutes.

The test handles cleanup automatically (before and after), but if you use `--keep` or the script crashes, see [TESTING.md — Manual Cleanup](TESTING.md#cleanup) for the 8-step teardown procedure and stuck-namespace recovery.

## Troubleshooting

### Converter Job Fails

```bash
# Check Job logs (the Job runs in the driver's namespace)
kubectl get jobs -n kube-system -l storage.ibmcloud.io/share-data-job
kubectl logs -n kube-system job/<job name from the list above>

# Common issues:
# - Network access to container registry blocked
# - dnf install fails (network or repo issues)
# - qemu-img conversion error (corrupted download)
```

### Converter Job OOMKilled

**Symptom:** Converter pod shows `OOMKilled` status after running for several minutes.

**Cause (pre-v0.11.0):** Older converter scripts wrote decompressed OCI layers to the staging emptyDir, which uses tmpfs (pod memory). Large images (2+ GB decompressed) exceeded the 2Gi memory limit.

**Fix:** Upgrade to v0.11.0+. The converter now decompresses directly to the golden PVC's folder on the share, bypassing emptyDir memory usage. If you see OOMKilled on v0.11.0+, the OCI image download itself may be too large for staging — delete the job and let the syncer recreate it.

```bash
# Check for OOMKilled
kubectl get pods -n kube-system -l storage.ibmcloud.io/share-data-job -o jsonpath='{range .items[*]}{.metadata.name} {.status.containerStatuses[0].state}{"\n"}{end}'

# Delete the failed job to trigger a retry with the current converter script
kubectl delete job -n kube-system <job name>
```

### Converter Job Uses Old Script After Upgrade

**Symptom:** After deploying a new controller version, converter jobs still run the old script.

**Cause:** Jobs created by the previous controller version contain the old script. New versions only affect newly created jobs.

**Fix:** Delete existing converter jobs and remove the `golden-ready` annotation from golden PVCs so the syncer recreates them with the updated script:

```bash
kubectl delete jobs -n pool-tutorial -l pool.storage.ibmcloud.io/golden-image=true
kubectl annotate pvc -n pool-tutorial -l pool.storage.ibmcloud.io/golden-image=true pool.storage.ibmcloud.io/golden-ready-
```

### PVC Stuck in Pending

```bash
# Check if pool has capacity
kubectl get fsp general-purpose

# Check CSI provisioner logs
kubectl logs deploy/ibm-vpc-file-pool-csi-controller -c csi-provisioner
```

### Template Not Appearing in UI

Templates must have these labels to appear in the Virtualization catalog:
- `template.kubevirt.io/type: base`
- `os.template.kubevirt.io/{imageName}: true`
- `workload.template.kubevirt.io/server: true`

Verify:
```bash
oc get template centos-stream9-nfs-pool -n pool-tutorial -o jsonpath='{.metadata.labels}'
```

### Syncer Not Running

The syncer only runs in custom mode (when no pool SC is default). Check:
```bash
kubectl logs deploy/ibm-vpc-file-pool-csi-controller -c controller | grep "golden"
```

If you see `"Native CDI golden image mode active"`, your pool SC is the default and CDI handles everything.
