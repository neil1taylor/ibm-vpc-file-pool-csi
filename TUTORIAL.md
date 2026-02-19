# Tutorial: File Share Pool CSI Driver on ROKS with OpenShift Virtualization

Step-by-step guide to create a file share pool, inspect it via IBM Cloud CLI, and attach NFS-backed storage to a KubeVirt VM as a shared data disk.

**Cluster:** ROKS eu-de (OpenShift Virtualization enabled)
**Zone:** eu-de-1

---

## Prerequisites

Verify the CSI driver is running:

```bash
# Controller pod (6/6 Running)
oc get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi -l app.kubernetes.io/component=controller

# Node pods (3/3 Running on each schedulable node)
oc get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi -l app.kubernetes.io/component=node

# CSI Driver registered
oc get csidriver vpc-file-pool.csi.ibm.io
```

---

## Step 1: Clean Up Any Previous Test Resources

```bash
# Delete the tutorial namespace (removes all PVCs, pods, VMs inside it)
oc delete namespace pool-tutorial --ignore-not-found

# Delete any existing test pool (WARNING: deletes underlying VPC file share)
# Replace the pool name with whatever you used previously
oc delete filesharepool --all --ignore-not-found

# Wait for shares to be deleted
ibmcloud is shares
```

---

## Step 2: Set Up Variables and Namespace

Choose a name for your pool. The controller automatically creates a StorageClass named after the pool when it reaches `Ready`.

```bash
# Set your pool name — used throughout the tutorial
export POOL_NAME=my-pool

# Create a namespace for tutorial resources
export TUTORIAL_NS=pool-tutorial
oc create namespace ${TUTORIAL_NS}
```

Use `ibmcloud is shares` to see existing VPC file shares before creating the pool.

---

## Step 3: Create a FileSharePool

```bash
cat <<EOF | oc apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: ${POOL_NAME}
spec:
  zone: eu-de-1
  profile: dp2
  shareSizeGB: 100
  iops: 100
  maxShares: 3
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultPermissions: "0777"
  defaultUID: 107
  defaultGID: 107
EOF
```

**What this does:**
- Creates a pool named `${POOL_NAME}` in `eu-de-1`
- Uses the `dp2` profile with 100 IOPS
- Each share is 100 GB; up to 3 shares allowed
- 1 share is created immediately
- Auto-expands when 80% of capacity is allocated
- `defaultPermissions: "0777"` — required for KubeVirt VMs (see [KubeVirt NFS Permissions](#kubevirt-nfs-permissions) below)
- `defaultUID: 107` / `defaultGID: 107` — the CSI driver creates PVC subdirectories as UID 107 (QEMU user), bypassing NFS root_squash so virt-handler skips chown

Watch the pool initialize (takes 30-90 seconds for VPC share creation):

```bash
oc get filesharepools -w
```

Wait until `PHASE` shows `Ready`:

```
NAME       ZONE      PROFILE   SHARES   CAPACITY   ALLOCATED   PVCS   PHASE
my-pool    eu-de-1   dp2       1        100        0           0      Ready
```

Use `ibmcloud is shares` to see the new VPC file share that was created.

---

## Step 4: Verify the Auto-Created StorageClass

When a FileSharePool reaches `Ready`, the controller **automatically creates** a matching StorageClass. You do not need to create one manually.

The auto-created StorageClass:
- Is named after the pool (e.g., `${POOL_NAME}`)
- Uses `vpc-file-pool.csi.ibm.io` as the provisioner
- Sets `parameters.pool` to the pool name
- Includes default NFS mount options (`nfsvers=4.1,soft,timeo=600,retrans=3`)
- Enables `allowVolumeExpansion: true` and `reclaimPolicy: Delete`
- Has an OwnerReference to the pool (deleted automatically when the pool is deleted)

For tiered pools, one StorageClass per tier is created (e.g., `${POOL_NAME}-standard`, `${POOL_NAME}-premium`), each with the `tier` parameter set.

Verify:

```bash
oc get sc ${POOL_NAME}
```

**Opt-out:** If you prefer to manage StorageClasses manually, add the annotation `storage.ibmcloud.io/skip-storageclass: "true"` to the FileSharePool before creating it, then create the StorageClass yourself:

```bash
cat <<EOF | oc apply -f -
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ibm-vpc-file-pool
provisioner: vpc-file-pool.csi.ibm.io
parameters:
  pool: ${POOL_NAME}
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions:
  - nfsvers=4.1
  - soft
  - timeo=600
  - retrans=3
EOF
```

---

## Step 5: Inspect the Pool via Kubernetes

```bash
# Summary view
oc get filesharepools

# Detailed status — shows shares, mount target IPs, capacity
oc describe filesharepool ${POOL_NAME}

# YAML view — full status with share IDs
oc get filesharepool ${POOL_NAME} -o yaml
```

Key things to note in the output:
- `status.phase: Ready` — pool is operational
- `status.shares[0].shareID` — the VPC file share ID (e.g., `r010-xxxxxxxx-...`)
- `status.shares[0].mountTargetIP` — the NFS server hostname
- `status.shares[0].state: stable` — share is healthy
- `status.totalCapacityGB` / `status.totalAllocatedGB` — capacity tracking

Copy the **shareID** from the output — you'll use it in the next step.

---

## Step 6: Verify the VPC File Share via IBM Cloud CLI

Use the share ID from Step 5 (replace `<SHARE_ID>` with your actual one):

```bash
# List all file shares in the account
ibmcloud is shares

# Get detailed info for the pool's share
ibmcloud is share <SHARE_ID>
```

Expected output shows:
- **Name:** `${POOL_NAME}-share-1`
- **Zone:** `eu-de-1`
- **Size:** `100` GB
- **Profile:** `dp2`
- **Lifecycle state:** `stable`

Now inspect the mount target (the NFS endpoint):

```bash
# List mount targets for the share
ibmcloud is share-mount-targets <SHARE_ID>

# Get mount target details (shows the NFS mount path)
ibmcloud is share-mount-target <SHARE_ID> <MOUNT_TARGET_ID>
```

The `mount_path` field shows the NFS server and export path, e.g.:
```
fsf-fra0251a-byok-fz.adn.networklayer.com:/953e01e5_f4c2_4c0c_b715_99ac35879a58
```

This is the NFS server that worker nodes connect to when pods use PVCs from this pool.

---

## Step 7: Test PVC Provisioning with a Simple Pod

Create a PVC and a pod to verify NFS storage works end-to-end:

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vm-shared-data
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 10Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: pool-test-writer
  namespace: ${TUTORIAL_NS}
spec:
  containers:
    - name: writer
      image: busybox
      command: ["sh", "-c", "echo 'Pool CSI NFS storage is working!' > /data/hello.txt && ls -la /data/ && cat /data/hello.txt && sleep 3600"]
      volumeMounts:
        - name: shared-data
          mountPath: /data
  volumes:
    - name: shared-data
      persistentVolumeClaim:
        claimName: vm-shared-data
EOF
```

Watch the PVC bind and pod start:

```bash
oc get pvc vm-shared-data -n ${TUTORIAL_NS} -w
# Should show STATUS=Bound within seconds

oc get pod pool-test-writer -n ${TUTORIAL_NS} -w
# Wait for STATUS=Running, then:
oc logs pool-test-writer -n ${TUTORIAL_NS}
```

Expected output:
```
Pool CSI NFS storage is working!
```

Verify the SubVolume and pool capacity:

```bash
oc get subvolumes -o wide
oc get filesharepools
# ALLOCATED should show 10, PVCS should show 1
```

Clean up the test pod (keep the PVC for now):

```bash
oc delete pod pool-test-writer -n ${TUTORIAL_NS}
```

---

## Step 8: Create a VM with a Pool-Backed Data Disk

Create a KubeVirt VM with **both disks entirely on NFS pool storage**. This is a 3-part process: create PVCs, download the OS image, then create the VM.

> **Note:** We bypass CDI's `dataVolumeTemplate` because CDI's VolumePopulator mechanism conflicts with our CSI provisioner. Instead, we create regular PVCs on the pool and download the OS image directly.

### Step 8a: Create the Boot and Data PVCs

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: pool-test-vm-boot
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 15Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: pool-test-vm-data
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 10Gi
EOF
```

Both PVCs should bind immediately:

```bash
oc get pvc pool-test-vm-boot pool-test-vm-data -n ${TUTORIAL_NS}
# Both should show STATUS=Bound, STORAGECLASS=${POOL_NAME}
```

### Step 8b: Download the OS Image to the Boot PVC

KubeVirt expects a disk image file named `disk.img` in a filesystem-mode PVC. The pod runs as **UID 107** (the QEMU user) so that `disk.img` is owned by the correct user — VPC file shares enforce NFS root_squash, which prevents virt-launcher from chowning files at runtime (see [KubeVirt NFS Permissions](#kubevirt-nfs-permissions)):

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: image-downloader
  namespace: ${TUTORIAL_NS}
spec:
  restartPolicy: Never
  securityContext:
    runAsUser: 107
    runAsGroup: 107
    fsGroup: 107
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: downloader
      image: registry.access.redhat.com/ubi9/ubi-minimal
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: ["ALL"]
      command:
        - sh
        - -c
        - |
          echo "Downloading CentOS Stream 9 cloud image..."
          curl -L -o /boot/disk.img \
            https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2
          ls -lh /boot/disk.img
          echo "Done!"
      volumeMounts:
        - name: boot-disk
          mountPath: /boot
  volumes:
    - name: boot-disk
      persistentVolumeClaim:
        claimName: pool-test-vm-boot
EOF
```

Wait for the download to complete (~1.8 GB, takes 1-3 minutes depending on bandwidth):

```bash
# Watch pod status
oc get pod image-downloader -n ${TUTORIAL_NS} -w

# Check progress (while running)
oc logs image-downloader -n ${TUTORIAL_NS} -f

# When STATUS=Completed, verify the image
oc logs image-downloader -n ${TUTORIAL_NS}
```

Expected output:
```
Downloading CentOS Stream 9 cloud image...
-rw-r--r--. 1 99 99 1.8G ... /boot/disk.img
Done!
```

> **Note:** The file shows UID 99 (not 107) because VPC NFS maps non-root UIDs to the anonymous user. This is fine — virt-launcher's UID 107 is mapped to the same anonymous UID, so it retains read/write access.

Clean up the downloader pod:

```bash
oc delete pod image-downloader -n ${TUTORIAL_NS}
```

### Step 8c: Create the VM

```bash
cat <<EOF | oc apply -f -
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: pool-test-vm
  namespace: ${TUTORIAL_NS}
spec:
  instancetype:
    kind: virtualmachineclusterinstancetype
    name: u1.medium
  preference:
    kind: virtualmachineclusterpreference
    name: centos.stream9
  runStrategy: Always
  template:
    metadata:
      labels:
        network.kubevirt.io/headlessService: headless
    spec:
      domain:
        devices:
          disks:
            - name: rootdisk
              bootOrder: 1
            - name: datadisk
              serial: DATA0001
          interfaces:
            - masquerade: {}
              name: default
      networks:
        - name: default
          pod: {}
      subdomain: headless
      volumes:
        - persistentVolumeClaim:
            claimName: pool-test-vm-boot
          name: rootdisk
        - persistentVolumeClaim:
            claimName: pool-test-vm-data
          name: datadisk
        - cloudInitNoCloud:
            userData: |
              #cloud-config
              user: centos
              password: pooltest123
              chpasswd:
                expire: false
          name: cloudinitdisk
EOF
```

**What this creates:**
- **Boot disk** (`rootdisk`): CentOS Stream 9 image on the NFS pool (downloaded in Step 8b)
- **Data disk** (`datadisk`): 10 Gi blank disk on the NFS pool
- **Both disks on NFS** — no ODF/Ceph dependency, no CDI dependency
- **Instance type**: `u1.medium` (1 vCPU, 4 Gi RAM)
- **Login**: user `centos`, password `pooltest123`

---

## Step 9: Wait for the VM to Start

```bash
# Watch VM status
oc get vm pool-test-vm -n ${TUTORIAL_NS} -w

# Watch the VMI (VirtualMachineInstance) start
oc get vmi pool-test-vm -n ${TUTORIAL_NS} -w
```

Both PVCs are already provisioned, so the VM should start quickly (30-60 seconds):

```bash
# Verify both PVCs are Bound on ${POOL_NAME}
oc get pvc pool-test-vm-boot pool-test-vm-data -n ${TUTORIAL_NS}
```

Wait until `VM STATUS` shows `Running` and `VMI PHASE` shows `Running`.

---

## Step 10: Access the VM and Use the Data Disk

Connect to the VM console:

```bash
virtctl console pool-test-vm -n ${TUTORIAL_NS}
```

Login with `centos` / `pooltest123`, then:

```bash
# See all disks — the data disk should appear as /dev/vdb or similar
lsblk

# Format and mount the data disk
sudo mkfs.ext4 /dev/vdb
sudo mkdir -p /mnt/shared-data
sudo mount /dev/vdb /mnt/shared-data

# Write test data
echo "Hello from VM on pool CSI storage!" | sudo tee /mnt/shared-data/vm-test.txt
cat /mnt/shared-data/vm-test.txt

# Check disk space
df -h /mnt/shared-data
```

Press `Ctrl+]` to exit the console.

---

## Step 11: Verify Pool State After Allocations

Both the manually created PVC (Step 7) and the VM's data disk PVC should appear:

```bash
# Pool shows total allocated capacity and PVC count
oc get filesharepools
# Expected: ALLOCATED=20 (10 from Step 7 + 10 from VM), PVCS=2

# Both SubVolumes point to the same underlying VPC file share
oc get subvolumes -o wide

# VPC share is unchanged (still stable, same size)
ibmcloud is shares
ibmcloud is share <SHARE_ID>
```

This demonstrates the pooling model: **2 PVCs share 1 VPC file share**, saving VPC quota and provisioning time.

---

## Step 12: Create a Second Manual PVC to See Capacity Grow

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vm-shared-data-2
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 20Gi
EOF

# Check it bound
oc get pvc vm-shared-data-2 -n ${TUTORIAL_NS}

# Pool now shows 40 GB allocated, 3 PVCs — all on the same single VPC share
oc get filesharepools

# Three SubVolumes, all pointing to the same shareID
oc get subvolumes -o wide
```

---

## Cleanup

```bash
# 1. Delete the tutorial namespace — removes all PVCs, pods, and VMs inside it
oc delete namespace ${TUTORIAL_NS}

# 2. Verify pool capacity is reclaimed
oc get filesharepools
# Expected: ALLOCATED=0, PVCS=0

# 3. (Optional) Delete the pool — this deletes the VPC file share
oc delete filesharepool ${POOL_NAME}
ibmcloud is shares  # Verify share is gone

# 4. (Optional) Delete the StorageClass — auto-deleted with the pool (OwnerReference),
#    but if you created one manually:
# oc delete sc ${POOL_NAME}
```

---

## Quick Reference: Useful Commands

| What | Command |
|------|---------|
| List pools | `oc get filesharepools` |
| Pool details | `oc describe filesharepool <name>` |
| List SubVolumes | `oc get subvolumes -o wide` |
| List VPC shares | `ibmcloud is shares` |
| Share details | `ibmcloud is share <id>` |
| Mount targets | `ibmcloud is share-mount-targets <share-id>` |
| CSI driver status | `oc get csidriver vpc-file-pool.csi.ibm.io` |
| Controller logs | `oc logs -n kube-system -l app.kubernetes.io/component=controller -c csi-controller --tail=50` |
| Node agent logs | `oc logs -n kube-system -l app.kubernetes.io/component=node -c csi-node --tail=50` |
| Webhook test | `echo '{"apiVersion":"storage.ibmcloud.io/v1alpha1","kind":"FileSharePool","metadata":{"name":"bad"},"spec":{}}' \| oc apply -f -` |

---

## KubeVirt NFS Permissions

KubeVirt's virt-handler calls `chown(mountDir, 107, 107)` on every filesystem-mode PVC mount. IBM Cloud VPC file shares enforce NFS `root_squash` (cannot be disabled), which maps root (UID 0) to nobody (UID 65534) and blocks all chown operations. Without mitigation, VMs fail with:

```
preparing host-disks failed: chown /proc/self/fd/26: operation not permitted
```

The CSI driver solves this with two pool settings:

1. **`defaultUID: 107` / `defaultGID: 107` on the pool** — the CSI node agent creates PVC subdirectories by spawning `mkdir` as UID 107 (using `setuid`). Since NFS root_squash only affects UID 0, the directory is created already owned by UID 107 on the NFS server. When virt-handler checks `stat.Uid == 107`, it finds the directory already owned correctly and **skips chown entirely**.
2. **`defaultPermissions: "0777"` on the pool** — ensures the QEMU user (UID 107) can access subdirectories.
3. **Image downloader runs as UID 107** — `disk.img` is created with the correct ownership from the start, so virt-launcher doesn't need to chown it.

KubeVirt [PR #15037](https://github.com/kubevirt/kubevirt/pull/15037) (merged July 2025, backported to v1.5/v1.6) made virt-launcher skip chown on pre-existing files. OpenShift Virtualization 4.17+ includes this fix. On older versions, VMs on NFS with root_squash may not start regardless of these workarounds.

See also: [Known Limitations — KubeVirt NFS Permissions](KNOWN-LIMITATIONS.md#kubevirt-nfs-permissions-root_squash).

---

## How StorageClasses Link to Pools

When a FileSharePool reaches `Ready`, the controller **auto-creates** a StorageClass named after the pool. You can also create StorageClasses manually if you opt out of auto-creation.

```
StorageClass (auto-created)     FileSharePool                VPC File Share
┌─────────────────────┐        ┌──────────────────┐        ┌──────────────────┐
│ my-pool             │        │ my-pool          │        │ my-pool-share-1  │
│                     │        │                  │        │                  │
│ parameters:         │───────>│ zone: eu-de-1    │───────>│ 100 GB, dp2      │
│   pool: my-pool     │        │ profile: dp2     │        │ NFS mount target │
│                     │        │ shareSizeGB: 100 │        │                  │
└─────────────────────┘        └──────────────────┘        └──────────────────┘
         │                              │
         │ PVC requests                 │ Allocates subdirectories
         ▼                              ▼
┌─────────────────────┐        ┌──────────────────┐
│ PVC: my-app-data    │        │ SubVolume CR     │
│ 10Gi RWX            │───────>│ shareID: r010-.. │
│                     │        │ subPath: /pvcs/  │
└─────────────────────┘        │   pvc-xxxx       │
                               └──────────────────┘
```

- **One StorageClass** references **one pool** (via `parameters.pool`) — auto-created by the controller
- **One pool** manages **one or more VPC file shares** (auto-expands as needed)
- **Each PVC** creates a **SubVolume** (subdirectory on an existing share)
- **Multiple PVCs** share the **same VPC file share** — that's the pooling benefit
