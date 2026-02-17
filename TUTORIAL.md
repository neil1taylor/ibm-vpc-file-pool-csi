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
# Delete leftover test pods, PVCs, and VMs from earlier testing
oc delete pod pool-test-writer pool-data-reader --ignore-not-found
oc delete pvc vm-shared-data vm-shared-data-2 --ignore-not-found
oc delete vm pool-test-vm --ignore-not-found
oc delete dv pool-test-vm-boot pool-test-vm-data --ignore-not-found

# Delete any existing test pool (WARNING: deletes underlying VPC file share)
# Replace the pool name with whatever you used previously
oc delete filesharepool --all --ignore-not-found

# Wait for shares to be deleted
ibmcloud is shares
```

---

## Step 2: Create a FileSharePool

Choose a name for your pool. The controller automatically creates a StorageClass named after the pool when it reaches `Ready`.

```bash
# Set your pool name — used throughout the tutorial
export POOL_NAME=my-pool
```

Use `ibmcloud is shares` to see existing VPC file shares before creating the pool.

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
EOF
```

**What this does:**
- Creates a pool named `${POOL_NAME}` in `eu-de-1`
- Uses the `dp2` profile with 100 IOPS
- Each share is 100 GB; up to 3 shares allowed
- 1 share is created immediately
- Auto-expands when 80% of capacity is allocated

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

## Step 3: Verify the Auto-Created StorageClass

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

## Step 4: Inspect the Pool via Kubernetes

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

## Step 5: Verify the VPC File Share via IBM Cloud CLI

Use the share ID from Step 4 (replace `<SHARE_ID>` with your actual one):

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

## Step 6: Test PVC Provisioning with a Simple Pod

Create a PVC and a pod to verify NFS storage works end-to-end:

```bash
cat <<'EOF' | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vm-shared-data
  namespace: default
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
  namespace: default
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
oc get pvc vm-shared-data -w
# Should show STATUS=Bound within seconds

oc get pod pool-test-writer -w
# Wait for STATUS=Running, then:
oc logs pool-test-writer
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
oc delete pod pool-test-writer
```

---

## Step 7: Create a VM with a Pool-Backed Data Disk

Create a KubeVirt VM with **both disks entirely on NFS pool storage**. This is a 3-part process: create PVCs, download the OS image, then create the VM.

> **Note:** We bypass CDI's `dataVolumeTemplate` because CDI's VolumePopulator mechanism conflicts with our CSI provisioner. Instead, we create regular PVCs on the pool and download the OS image directly.

### Step 7a: Create the Boot and Data PVCs

```bash
cat <<'EOF' | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: pool-test-vm-boot
  namespace: default
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
  namespace: default
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
oc get pvc pool-test-vm-boot pool-test-vm-data
# Both should show STATUS=Bound, STORAGECLASS=${POOL_NAME}
```

### Step 7b: Download the OS Image to the Boot PVC

KubeVirt expects a disk image file named `disk.img` in a filesystem-mode PVC. Run a pod that downloads the CentOS Stream 9 cloud image:

```bash
cat <<'EOF' | oc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: image-downloader
  namespace: default
spec:
  restartPolicy: Never
  containers:
    - name: downloader
      image: registry.access.redhat.com/ubi9/ubi-minimal
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

Wait for the download to complete (~1 GB, takes 1-3 minutes depending on bandwidth):

```bash
# Watch pod status
oc get pod image-downloader -w

# Check progress (while running)
oc logs image-downloader -f

# When STATUS=Completed, verify the image
oc logs image-downloader
```

Expected output:
```
Downloading CentOS Stream 9 cloud image...
-rw-r--r--. 1 root root 1.2G ... /boot/disk.img
Done!
```

Clean up the downloader pod:

```bash
oc delete pod image-downloader
```

### Step 7c: Create the VM

```bash
cat <<'EOF' | oc apply -f -
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: pool-test-vm
  namespace: default
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
- **Boot disk** (`rootdisk`): CentOS Stream 9 image on the NFS pool (downloaded in Step 7b)
- **Data disk** (`datadisk`): 10 Gi blank disk on the NFS pool
- **Both disks on NFS** — no ODF/Ceph dependency, no CDI dependency
- **Instance type**: `u1.medium` (1 vCPU, 4 Gi RAM)
- **Login**: user `centos`, password `pooltest123`

---

## Step 8: Wait for the VM to Start

```bash
# Watch VM status
oc get vm pool-test-vm -w

# Watch the VMI (VirtualMachineInstance) start
oc get vmi pool-test-vm -w
```

Both PVCs are already provisioned, so the VM should start quickly (30-60 seconds):

```bash
# Verify both PVCs are Bound on ${POOL_NAME}
oc get pvc pool-test-vm-boot pool-test-vm-data
```

Wait until `VM STATUS` shows `Running` and `VMI PHASE` shows `Running`.

---

## Step 9: Access the VM and Use the Data Disk

Connect to the VM console:

```bash
virtctl console pool-test-vm
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

## Step 10: Verify Pool State After Allocations

Both the manually created PVC (Step 6) and the VM's data disk PVC should appear:

```bash
# Pool shows total allocated capacity and PVC count
oc get filesharepools
# Expected: ALLOCATED=20 (10 from Step 6 + 10 from VM), PVCS=2

# Both SubVolumes point to the same underlying VPC file share
oc get subvolumes -o wide

# VPC share is unchanged (still stable, same size)
ibmcloud is shares
ibmcloud is share <SHARE_ID>
```

This demonstrates the pooling model: **2 PVCs share 1 VPC file share**, saving VPC quota and provisioning time.

---

## Step 11: Create a Second Manual PVC to See Capacity Grow

```bash
cat <<'EOF' | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vm-shared-data-2
  namespace: default
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 20Gi
EOF

# Check it bound
oc get pvc vm-shared-data-2

# Pool now shows 40 GB allocated, 3 PVCs — all on the same single VPC share
oc get filesharepools

# Three SubVolumes, all pointing to the same shareID
oc get subvolumes -o wide
```

---

## Cleanup

```bash
# 1. Delete the VM
oc delete vm pool-test-vm

# 2. Delete all PVCs (boot disk, data disk, manual test PVCs)
oc delete pvc pool-test-vm-boot pool-test-vm-data vm-shared-data vm-shared-data-2 --ignore-not-found

# 3. Delete test pods
oc delete pod pool-test-writer pool-data-reader --ignore-not-found

# 4. Delete downloader pod if still around
oc delete pod image-downloader --ignore-not-found

# 5. Verify pool capacity is reclaimed
oc get filesharepools
# Expected: ALLOCATED=0, PVCS=0

# 6. (Optional) Delete the pool — this deletes the VPC file share
oc delete filesharepool ${POOL_NAME}
ibmcloud is shares  # Verify share is gone

# 7. (Optional) Delete the StorageClass — auto-deleted with the pool (OwnerReference),
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
