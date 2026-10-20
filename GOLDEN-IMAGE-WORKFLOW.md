# Golden Image Workflow for KubeVirt VMs

> **Hidden feature.** Golden images are hidden. The code is kept for possible later use, and a pool that sets `spec.goldenImages` in YAML still works. The console pool form no longer offers golden images. The recommended way to make VMs is the standard OpenShift Virtualization catalog: pick a catalog image and choose the pool's StorageClass for the boot disk. See [Create a VM with its boot disk on a pool](USER-GUIDE.md#create-a-vm-with-its-boot-disk-on-a-pool) and [CDI compatibility](KNOWN-LIMITATIONS.md#cdi-containerized-data-importer-compatibility).

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
2. Creates a CDI DataVolume for each image on the pool StorageClass. CDI downloads the image and converts it to raw format.
3. Creates a DataSource and an OpenShift Template for VM creation from the UI catalog

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
    # Optional: golden PVC size (default: 15 GB)
    # pvcSizeGB: 15
```

### What the Syncer Creates

For each discovered image and each target namespace:

| Resource | Name | Purpose |
|----------|------|---------|
| DataVolume | `golden-{imageName}` | CDI imports the image from the registry onto the pool StorageClass. The source is the DataImportCron's registry URL with `pullMethod: node`. |
| PVC | `golden-{imageName}` | Created by CDI for the DataVolume. Holds the raw disk image. |
| DataSource | `{imageName}-{namespace}-nfs-pool` | CDI DataSource pointing to the golden PVC (enables InstanceTypes tab). One per golden claim. |
| Template | `{imageName}-nfs-pool` | OpenShift VM template for UI catalog, in the target namespace |

For images whose DataImportCron uses an ImageStream (the RHEL images), the syncer uses the external address that the ImageStream imports from. An example is `registry.redhat.io/rhel9/rhel-guest-image`.

A golden image is ready when the DataVolume phase is `Succeeded`. If the phase is `Failed`, the pool status reports `Failed` and the syncer publishes no catalog entry.

On rovslab (20 October 2026), CentOS Stream 9 imported in about 40 seconds. RHEL 9 took about 2.5 minutes, with one automatic retry while the node pulled the image. VMs booted from both, and the guest agent connected.

Target namespaces need no extra permissions (no `anyuid` grant).

Two pools must not send golden images to the same namespace. The names collide. The driver does not block this.

When you delete a pool, the driver first deletes its golden DataVolumes, PVCs and Templates in its target namespaces, and its DataSources. The syncer skips pools that are being deleted.

> **Note:** Creating a VM from the console catalog (InstanceTypes tab) with its boot disk on the pool is being tested. It is not yet verified.

### Verification

```bash
# Check golden DataVolumes
kubectl get dv -n pool-tutorial

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
4. The boot PVC is cloned from the golden image via CSI clone (the console catalog path with the boot disk on the pool is being tested)

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

The test creates a pool with `goldenImages.enabled=true` and lets the syncer handle image discovery, DataVolume creation, and template creation. It then creates a VM from the syncer-generated template, waits for the CSI clone Job to complete, and verifies the VM boots with connectivity checks.

Runtime: ~13-14 minutes.

The test handles cleanup automatically (before and after), but if you use `--keep` or the script crashes, see [TESTING.md — Manual Cleanup](TESTING.md#cleanup) for the 8-step teardown procedure and stuck-namespace recovery.

## Troubleshooting

### Golden Image Shows Failed (DataVolume)

**Symptom:** The pool status shows a golden image in phase `Failed`.

```bash
# Check the DataVolume phase
oc get dv -n pool-tutorial

# See the events and the reason
oc describe dv golden-centos-stream9 -n pool-tutorial
```

CDI retries some failures by itself. A DataVolume that stays `Failed` needs a retry. To retry, delete the DataVolume. The syncer makes a new one on the next cycle:

```bash
oc delete dv golden-centos-stream9 -n pool-tutorial
```

### DataVolume "was not made by pool"

**Symptom:** The pool status shows `Failed` with a message like `DataVolume ns/golden-<image> was not made by pool <pool> ...; delete it to let the pool make it`.

**Cause:** A DataVolume named `golden-<image>` already exists in the target namespace, and the pool did not make it. It has no pool labels, or its source URL or StorageClass is different. The syncer refuses to use it, and publishes no catalog entry.

**Fix:** Check the DataVolume. If you do not need it, delete it. The syncer then makes its own on the next cycle.

```bash
oc get dv golden-centos-stream9 -n pool-tutorial -o yaml
oc delete dv golden-centos-stream9 -n pool-tutorial
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
