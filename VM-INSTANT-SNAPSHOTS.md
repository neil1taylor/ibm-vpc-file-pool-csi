# Instant VM snapshots

Status: **design, not built yet.** The method was tested by hand on rovslab on 21 October 2026 (results below). The build plan is `docs/specs/2026-10-21-instant-vm-snapshots.md`.

## In one paragraph

A VM disk on a pool is one file, `disk.img`, in the volume's folder on a VPC file share. To take a snapshot, the driver tells QEMU (the program that runs the VM) to stop writing to that file and to send all new writes to a new, small file next to it. The old file is now the snapshot. Nothing is copied, so this takes seconds for any disk size. This is the same method VMware uses for its snapshots.

## Why the driver does it this way

- **A plain copy cannot be instant.** Today the driver copies the whole disk for each snapshot, at 20 MB/s by default: about 7 hours for 500 GB.
- **The file share cannot clone a file instantly.** The shares accept NFS 4.2, but they refuse a file clone (`cp --reflink`: "Operation not supported", rovslab, 21 Oct).
- **Share-level VPC snapshots are not used.** They cover every volume on the share, not one disk.

## How it works

### The files

Before any snapshot:
```
/pvcs/pvc-aaa/disk.img            ← the VM reads and writes here
```

After two snapshots:
```
/pvcs/pvc-aaa/disk.img            ← snapshot 1 (frozen, read-only)
/pvcs/pvc-aaa/snap-1.qcow2        ← changes after snapshot 1; frozen at snapshot 2 (read-only)
/pvcs/pvc-aaa/snap-2.qcow2        ← changes after snapshot 2; the VM writes here now
```

This set of files is a **chain**. Each file holds only the parts of the disk that changed after the file before it.

### Writes and reads

- **Writes** always go to the newest file.
- **Reads:** QEMU looks in the newest file first. If that part of the disk was not written since the snapshot, it looks in the file before, and so on down to `disk.img`. Each file has a small map of which parts it holds, so this is fast. A long chain makes reads a little slower, so the driver allows at most 16 snapshot files per disk.

### Taking a snapshot

1. The guest is frozen for a moment, so the guest writes its pending data to disk. OpenShift does this itself when you take a VM snapshot from the console. Otherwise the driver asks the guest agent to freeze.
2. The driver runs one command in the VM's pod: `virsh snapshot-create-as ... --disk-only`. QEMU starts the new file and switches to it.
3. The guest is thawed. The VM never stops.
4. The driver makes the frozen file read-only and records the chain.

### Deleting a snapshot (merge)

A file in the chain cannot just be deleted, because the newer files depend on it. Instead, the driver merges the next file up into the deleted snapshot's file (`virsh blockcommit`), then deletes the next file. Only the changed parts are copied, so a merge takes about as long as copying the changes, not the whole disk. QEMU merges while the VM runs. If the VM is stopped, a Job does the same with `qemu-img commit`.

### Restoring a snapshot

OpenShift restores a snapshot by making a new disk. The driver makes that disk with one sparse copy of the snapshot's view (`qemu-img convert`). This is a copy of the disk, so it is not instant.

### Restarts and moves: the hook

KubeVirt, which starts the VM, always opens `disk.img` as a plain file. On its own, it does not know about the chain. If it opened `disk.img` after a restart, every write since the first snapshot would be missing.

So the driver adds a **hook** to each VM with a pool disk. A hook is a small extra container in the VM's pod. KubeVirt calls it just before the VM starts, and it changes how the VM opens its disks: "open the newest file in the chain, as a qcow2 file". QEMU then follows the chain down to `disk.img`.

- The driver adds the hook by itself when the VM starts. You make VMs the normal way, from the standard catalog, with the pool's StorageClass.
- The hook needs the KubeVirt **`Sidecar` feature gate**. It is off by default. On OpenShift you turn it on with an annotation on the `HyperConverged` resource. **Red Hat does not support this setting**, and the cluster then reports `TaintedConfiguration`. On ROVS, IBM's add-on manages that resource.

### Safety: frozen files are read-only

A write to a frozen file would damage every snapshot built on it. So the driver makes each frozen file read-only. If a VM with a chain ever starts without the hook, QEMU cannot open `disk.img` for writing, and the VM does not start. It fails safe instead of starting on old data.

## What was tested (rovslab-prod-eude, ROVS 4.21, OpenShift Virtualization 4.21.17, 21 Oct 2026)

All tests were by hand, with `virsh` in the VM's pod and the hook from a ConfigMap. The driver code did not change.

| Test | Linux (CentOS Stream 9) | Windows Server 2022 |
|---|---|---|
| Snapshot of a running VM, guest frozen | 2 s | 4.6 s (VSS) |
| One snapshot of 2 disks at once, frozen | 2 s | not tested |
| Restart with the chain | data kept | data kept |
| Live migration with the chain | succeeded | succeeded |
| Merge a middle snapshot, VM running | 2 s | not tested |
| Merge the newest file, VM running | 4 s | 2 s |
| Restart after all merges, plain `disk.img` | data kept | data kept |
| Restore 2 frozen disks to a new VM | booted; counters on both disks matched (106 and 106); copy of a 30 GB disk with 2 GB used: 165 s | not tested |

Also seen:
- An OpenShift VM snapshot with today's driver: OpenShift froze the guest for 1 second, then the driver froze it again 3 seconds later for one disk only. That is the wrong-moment defect this design removes.
- libvirt writes absolute paths for the chain (`/var/run/kubevirt-private/vmi-disks/<volume>/...`). That path is the same in every VM pod. Driver Jobs that work on a chain mount the volume at the same path.
- libvirt does not delete files after a merge. The driver must.

Not tested yet: the five lab checks in the spec, and Windows with two disks.
