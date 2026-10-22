# Part 11: Instant VM Snapshots

> **Tutorial Series:** [Index](TUTORIAL.md) | [Part 1: Pool Creation](TUTORIAL-01-POOL-CREATION.md) | **Part 11**

Take a snapshot of a running VM in seconds, whatever the disk size. Delete a snapshot while the VM runs. Restore a snapshot to a new VM. Then remove all snapshots of the VM. The words instant snapshot, chain and Sidecar gate are defined in [CONTEXT.md](CONTEXT.md#instant-snapshot).

**Time:** about 45 minutes. The restore is a copy, so it takes longer for a large disk.
**Status:** Experimental. Read [VM-INSTANT-SNAPSHOTS.md](VM-INSTANT-SNAPSHOTS.md) and the limits in [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md#instant-vm-disk-snapshots-experimental) first.

> **Red Hat does not support the KubeVirt `Sidecar` feature gate** that this feature needs. OpenShift Virtualization then reports `TaintedConfiguration=True`. Use a test cluster. On ROVS, IBM's add-on manages the `HyperConverged` object; the driver changes one field of it.

---

## Prerequisites

- OpenShift Virtualization on your cluster, and `virtctl`.
- A running VM whose disks are PVCs on a pool StorageClass. Its guest runs the guest agent (`qemu-guest-agent`). This tutorial calls the VM `my-vm` in the namespace `vms`. Change the names to match yours.
- Each disk file (`disk.img`) is owned by the QEMU user (UID 107). The driver changes the file mode from inside the VM pod, as UID 107. If another user owns the file, the first snapshot fails with `chmod: changing permissions of '.../disk.img': Operation not permitted`. A disk that you made by hand as root on NFS (as in [Part 1](TUTORIAL-01-POOL-CREATION.md)) is owned by `nobody` unless you run `chown 107:107` on it.
- A VolumeSnapshotClass for the driver exists. The chart makes `ibm-vpc-file-pool` (see `volumeSnapshotClass` in [HELM-VALUES.md](HELM-VALUES.md)).
- The driver runs with the admission webhooks on, and `vmSnapshots.method=auto` (the chart default). See [HELM-VALUES.md](HELM-VALUES.md).

```bash
export NS=vms
export VM=my-vm
oc get vmi -n ${NS} ${VM} -o jsonpath='{.status.phase}{"\n"}'
```

**Expected:** `Running`.

---

## Step 1: Turn on the Sidecar gate

Check the gate first:

```bash
oc get kubevirt -A -o jsonpath='{.items[0].spec.configuration.developerConfiguration.featureGates}{"\n"}'
```

If the list contains `"Sidecar"`, go to step 2. Otherwise turn it on. Choose one way.

**With the chart.** Upgrade the release with `vmSnapshots.enableSidecarGate=true`. A Job turns the gate on after each install and upgrade. It adds one entry to the `kubevirt.kubevirt.io/jsonpatch` annotation of `HyperConverged` and keeps the other entries.

```bash
helm upgrade <release> charts/ibm-vpc-file-pool-csi -n kube-system --reuse-values \
  --set vmSnapshots.enableSidecarGate=true
```

**By hand.** Use the script in the repository. It edits only its own entry:

```bash
hack/sidecar-gate.sh on <cluster name> <kubeconfig>
```

Check again. The gate shows after a minute or two, because KubeVirt reconciles it.

**Expected:** the list now contains `"Sidecar"`.

The chart never turns the gate off. Turning it off is always a step you take (see [Step 7](#step-7-remove-all-snapshots-flatten)).

---

## Step 2: Find the disk files

The VM has one volume for each disk. Each volume is a PVC, and each PVC has a SubVolume. Before any snapshot, a disk is one file, `disk.img`:

```bash
export CLAIM=$(oc get vm -n ${NS} ${VM} -o jsonpath='{.spec.template.spec.volumes[0].persistentVolumeClaim.claimName}')
export PV=$(oc get pvc -n ${NS} ${CLAIM} -o jsonpath='{.spec.volumeName}')
echo "claim ${CLAIM}, volume ${PV}"
oc get subvolume ${PV} -o jsonpath='{.status.diskChain}{"\n"}'
```

**Expected:** an empty line. `status.diskChain` is empty for a volume with no snapshots. If your VM boots from a `dataVolume` volume, use `.dataVolume.name` in place of `.persistentVolumeClaim.claimName`.

---

## Step 3: Take a snapshot of the running VM

A `VirtualMachineSnapshot` snapshots all disks of the VM at the same moment. OpenShift freezes the guest for that moment. You can also use the VM Snapshots tab in the console.

```bash
cat <<EOF | oc apply -n ${NS} -f -
apiVersion: snapshot.kubevirt.io/v1beta1
kind: VirtualMachineSnapshot
metadata:
  name: ${VM}-snap1
spec:
  failureDeadline: 1h
  source:
    apiGroup: kubevirt.io
    kind: VirtualMachine
    name: ${VM}
EOF

oc wait -n ${NS} vmsnapshot/${VM}-snap1 --for=jsonpath='{.status.phase}'=Succeeded --timeout=300s
```

**Expected:** the snapshot is `Succeeded` within a few seconds. The VM keeps running.

Look at the disk now:

```bash
oc get subvolume ${PV} -o jsonpath='{.status.diskChain}{"\n"}'
```

**Expected:** a list with two files, for example `["snap-a1b2c3.qcow2","disk.img"]`. The newest file is first. The VM now writes to `snap-a1b2c3.qcow2`. `disk.img` is the snapshot and is read-only. Nothing was copied.

To see how each disk snapshot was made, find the `Snapshot` resource of the first disk:

```bash
VSC=$(oc get vmsnapshot -n ${NS} ${VM}-snap1 -o jsonpath='{.status.virtualMachineSnapshotContentName}')
VS=$(oc get vmsnapshotcontent -n ${NS} ${VSC} -o json | jq -r '.spec.volumeBackups[0].volumeSnapshotName')
CONTENT=$(oc get volumesnapshot -n ${NS} ${VS} -o jsonpath='{.status.boundVolumeSnapshotContentName}')
HANDLE=$(oc get volumesnapshotcontent ${CONTENT} -o jsonpath='{.status.snapshotHandle}')
oc get snapshots.storage.ibmcloud.io ${HANDLE##*/} -o jsonpath='{.metadata.annotations.storage\.ibmcloud\.io/snapshot-method}{"\n"}'
```

**Expected:** `instant`. If it says `vm` or `file`, the snapshot was a copy. Read `status.methodReason` of the `Snapshot`. The usual reason is that the Sidecar gate is off.

---

## Step 4: Take a second snapshot

Make a change in the guest first, so the snapshots differ. Then snapshot again.

```bash
virtctl ssh -n ${NS} cloud-user@vmi/${VM} -c 'echo two | sudo tee /var/tmp/marker-two && sync'

cat <<EOF | oc apply -n ${NS} -f -
apiVersion: snapshot.kubevirt.io/v1beta1
kind: VirtualMachineSnapshot
metadata:
  name: ${VM}-snap2
spec:
  failureDeadline: 1h
  source:
    apiGroup: kubevirt.io
    kind: VirtualMachine
    name: ${VM}
EOF

oc wait -n ${NS} vmsnapshot/${VM}-snap2 --for=jsonpath='{.status.phase}'=Succeeded --timeout=300s
oc get subvolume ${PV} -o jsonpath='{.status.diskChain}{"\n"}'
```

Change the user name in `virtctl ssh` to the user of your guest image. `virtctl ssh` needs an SSH key in the guest. A VM that has only a password (like the one in Part 1) cannot use it: log in on the console and make the file there.

**Expected:** a chain of three files. A disk may have at most 16 snapshots.

---

## Step 5: Delete the first snapshot while the VM runs

A file in the chain cannot just be deleted, because newer files depend on it. The driver merges the next newer file into it, and QEMU does this while the VM runs.

```bash
oc delete vmsnapshot -n ${NS} ${VM}-snap1
oc get subvolume ${PV} -o jsonpath='{.status.diskChain}{" "}{.status.chainOperation}{"\n"}'
```

While the merge runs, `status.chainOperation` names it. No other snapshot or merge of the disk starts then. A merge takes about as long as copying the changed parts, not the whole disk. Repeat the command until `chainOperation` is empty.

**Expected:** the chain has two files, and the VM was never stopped.

---

## Step 6: Restore a snapshot to a new VM

OpenShift restores a snapshot by making new PVCs. The driver writes each with one sparse copy of the snapshot (`qemu-img convert`). The restore is a copy, so it takes time in proportion to the data.

```bash
cat <<EOF | oc apply -n ${NS} -f -
apiVersion: snapshot.kubevirt.io/v1beta1
kind: VirtualMachineRestore
metadata:
  name: ${VM}-restore
spec:
  target:
    apiGroup: kubevirt.io
    kind: VirtualMachine
    name: ${VM}-copy
  virtualMachineSnapshotName: ${VM}-snap2
  patches:
    - '{"op":"remove","path":"/spec/template/spec/domain/devices/interfaces/0/macAddress"}'
EOF

oc wait -n ${NS} vmrestore/${VM}-restore --for=jsonpath='{.status.complete}'=true --timeout=3600s
virtctl start ${VM}-copy -n ${NS}
```

The `patches` line removes the MAC address, so the copy does not clash with the original. Remove that line if your VM has no network interface with a fixed MAC address.

**Expected:** the new VM starts and has the file `/var/tmp/marker-two`.

A snapshot of a VM disk cannot be cloned. Restore the snapshot instead.

---

## Step 7: Remove all snapshots (flatten)

To go back to plain disks, delete all snapshots of the VM. The `flatten` command does it and waits until every disk is one `disk.img` again.

```bash
kubectl pool-snapshots flatten ${VM} -n ${NS}
```

**Expected:** the command lists the snapshots and asks you to type the VM name. Then it deletes them and prints `Done: every disk of vms/my-vm is a plain disk.img.` Use `--yes` to skip the question.

A VM with snapshots does not start while the Sidecar gate is off. Flatten first, then remove the `Sidecar` entry from the `kubevirt.kubevirt.io/jsonpatch` annotation, or run `hack/sidecar-gate.sh off`.

---

## Clean up

```bash
oc delete vm -n ${NS} ${VM}-copy --ignore-not-found
oc delete vmrestore -n ${NS} ${VM}-restore --ignore-not-found
oc delete vmsnapshot -n ${NS} ${VM}-snap1 ${VM}-snap2 --ignore-not-found
```

---

## Things to know

- Backups and replication of a disk with instant snapshots make a full copy after each snapshot. A stopped VM's disk with snapshots is not copied until the VM runs or the disk is flattened.
- A VM with snapshots gets a small extra container (the VM hook) when it starts. The driver adds it by itself. The hook needs the Sidecar gate.
- Hot-plugged disks are not supported.

## Next

- [Part 12: Replicating running VMs](TUTORIAL-12-REPLICATING-RUNNING-VMS.md)
- [Part 10: Backup and restore](TUTORIAL-10-BACKUP-AND-RESTORE.md)
