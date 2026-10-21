# Spec: instant VM snapshots (overlay files)

Date: 2026-10-21. Status: approved ("spec agreed continue"); updated with the lab check results the same day. It replaces the VM path of `2026-10-21-vm-safe-snapshots.md` (step 7) for clusters that allow it. How the method works, with the lab evidence, is in `VM-INSTANT-SNAPSHOTS.md`. Read that first.

## Goal

When this is done:
- A snapshot of a running VM's disk takes a few seconds, whatever the disk size. No disk data is copied.
- An OpenShift VM snapshot (`VirtualMachineSnapshot`) of a VM with several disks gives disks from one moment: the moment OpenShift froze the guest. The snapshot reports that moment truthfully.
- Deleting a snapshot merges its file away while the VM runs. The merge copies only the data that changed.
- A VM with snapshots restarts and live-migrates without losing data.
- If anything that this method needs is missing, the VM refuses to start rather than start on the wrong data.
- Restoring a snapshot to a new disk is one copy pass.
- VMs are made the normal way: from the standard OpenShift catalog, with the pool's StorageClass. Nothing is added to the VM. The driver adds its hook to each running instance by itself.

Gap against the goal (VMware-like): in VMware, "revert to snapshot" is instant. Here, an OpenShift restore always makes new PVCs, so a restore is a copy of the disk. A 500 GB disk with 50 GB used takes minutes. Making restore instant is part 2 at the end; it is not in this build.

## What exists today

- A snapshot of a running VM disk is a full QEMU copy at 20 MB/s by default, then a second full copy by a Job. A 500 GB disk takes about 7 hours.
- The copy starts after `CreateSnapshot` has returned, so the reported moment is wrong. On the lab cluster, OpenShift froze the guest for 1 second, then our driver froze it again 3 seconds later, for one disk only (21 Oct, `VM-INSTANT-SNAPSHOTS.md`). Disks of one VM come from different moments. This is the root cause of issue #14.
- The driver can already run commands in a VM's `virt-launcher` pod (`pkg/vmcopy`, using `pods/exec`).

## Design

### When the instant method is used

`CreateSnapshot` uses the instant method when all of these are true:
1. The volume is the disk of a KubeVirt VM (a `virt-launcher` pod mounts the claim, or the VM is stopped and its spec names the claim).
2. The KubeVirt `Sidecar` feature gate is on. The driver reads it from the `KubeVirt` resource on each call.
3. The VM is running or stopped. A running VM does not need the hook for its first snapshot. The hook is only needed when a VM with a chain starts.
4. The disk is not a hot-plugged disk.
5. The disk's chain has fewer than 16 snapshot files.

Otherwise, it uses the existing copy method, and the Snapshot status says why (`status.method: copy`, `status.methodReason`).

### Turning it on

- The cluster admin turns on the KubeVirt `Sidecar` feature gate. On OpenShift this is a `jsonpatch` annotation on the `HyperConverged` resource. It marks the cluster configuration as `TaintedConfiguration`, and Red Hat does not support it. `INSTALL.md` gets the steps and the support warning.
- Helm value `vmSnapshots.method`: `auto` (instant when possible, the default) or `copy` (never instant).

### The hook

- A new mutating webhook on `VirtualMachineInstance` create adds the `hooks.kubevirt.io/hookSidecars` annotation, only when both are true:
  - at least one of the VM's disks is a pool claim with a chain (a snapshot exists), and
  - the `Sidecar` feature gate is on. With the gate off, KubeVirt refuses any VMI with the hook annotation ("sidecar feature gate is not enabled"), so adding it would stop the VM from starting.
- A VM without snapshots is never changed.
- The hook uses KubeVirt's own `sidecar-shim` image, with a script from a ConfigMap. This is the setup tested in the lab. The controller keeps the ConfigMap `ibm-vpc-file-pool-vm-hook` in every namespace that has a pool claim.
- The webhook also writes the current top file of each disk into a VMI annotation, `storage.ibmcloud.io/disk-top-<volume>`, read from the SubVolume status. The hook script opens that file as qcow2 instead of `disk.img`. With no annotation, the disk is opened as plain `disk.img`, as today.
- After every snapshot and merge, the controller updates that annotation on the running VMI. A live migration does not need it: the target takes the chain from the running VM (lab check 2). The annotation is for the next start.
- The webhook is set to `failurePolicy: Ignore`. Missing the hook is made safe by the read-only rule below, not by blocking all VMs.

### The read-only rule (safety)

- Once a file is frozen as a snapshot, the driver makes it read-only (`chmod 0444`). QEMU only reads frozen files, so this does not affect a VM that runs with its chain.
- If a VM with a chain starts without the hook, QEMU tries to open `disk.img` for writing, fails, and the VM does not start. It never writes to a frozen file. A write to a frozen file would damage every snapshot built on it.
- The driver raises an event and an alert metric when a VM with a chain is found running or starting without the hook.

### Taking a snapshot

For a running VM, inside `CreateSnapshot`:
1. If OpenShift has frozen the guest (`VMI status.fsFreezeStatus` is `frozen`), do not freeze again. Otherwise ask for a freeze (`--quiesce`). If the guest agent cannot freeze, take the snapshot anyway and mark it crash-consistent.
2. Run `virsh snapshot-create-as <domain> --disk-only --atomic --no-metadata` in the compute container, for this disk only. The new file is `snap-<n>.qcow2` in the disk's folder.
3. Read the real chain back from QEMU (`virsh domblklist`, `qemu-img info --backing-chain`). Save it in the SubVolume status (`status.diskChain`).
4. Make the frozen file read-only. Update the VMI annotation.
5. Copy replication's change list to the new top file, so incremental replication keeps working.
6. Return `ReadyToUse: true` with the creation time from step 2.

For a stopped VM: a Job runs `qemu-img create -f qcow2 -b <top> -F <format> snap-<n>.qcow2`, then steps 3 to 6. The next start uses the new file through the hook.

A snapshot request that is not from OpenShift (a plain `VolumeSnapshot`) for one disk of a multi-disk VM covers only that disk. To cover all disks at one moment, use an OpenShift VM snapshot. It freezes the guest first, and each disk's snapshot lands inside that freeze. On the lab cluster, the freeze lasted 1 second and each instant snapshot took about 2 seconds, so the freeze timeout matters. The default KubeVirt timeout is longer than this, but this needs a lab check (see below).

### Deleting a snapshot (merge)

Rule: the snapshot's frozen file F has one child C (the next file up the chain). Merge C into F. Then F takes C's place, and the snapshot that owned C (or the live disk) now owns F.
- Running VM, C is the live top: `virsh blockcommit --active --pivot`.
- Running VM, C is in the middle: `virsh blockcommit --top C --base F`.
- Stopped VM: a Job runs `qemu-img commit`.
- F is read-only, and a merge into a read-only file fails. So the driver makes F writable just before the merge. If the merge fails, it makes F read-only again. If C was the live top, F is now the live top and stays writable.
- Then delete C's file, update the chain in the SubVolume status and the VMI annotation.
- A merge and a live migration cannot run at the same time. The driver waits and retries for up to 30 minutes, then returns an error that the snapshotter retries.

### Restore to a new disk

`CreateVolume` from a snapshot runs `qemu-img convert -O raw` from the snapshot's frozen file (which reads its own backing files) into the new folder's `disk.img`. This is one pass and keeps the disk sparse. It uses the existing share-data Job runner and the async restore flow.

A restore and a merge on the same disk never run at the same time. A per-volume lock enforces this.

### Deleting a disk that has snapshots

When a claim is deleted and its disk still has snapshots, the driver keeps the folder as retained, until the last snapshot is deleted. Then it removes the folder. This matches the CSI rule that a snapshot outlives its source.

### Space

- An overlay file starts small and grows with the changes written after the snapshot. In the worst case it grows to the disk size.
- Each snapshot is charged 10 % of the disk size against pool capacity. Real use is measured by the node agent and shown in the Snapshot status and a metric.
- The existing alert on share usage covers a share that fills. A full share pauses the VM's writes; it does not lose data.

### When the Sidecar gate is turned off

- New snapshots fall back to the copy method.
- VMs with chains will not start (read-only rule). The driver raises an alert at once.
- A new `kubectl pool-snapshots flatten <vm>` command merges all of a VM's snapshots back into `disk.img`. It works on a running VM, or by Job on a stopped VM. After that, the VM starts without the hook. Its snapshots are gone; the command says so and asks for confirmation.

### Other features that touch VM disks

- **Replication and backup (`pkg/vmcopy`):** they copy the live disk through QEMU, which reads through the chain. They must keep working. Their change list moves to the new top file at each snapshot (step 5 above).
- **Restore of a backup into a disk (`restore-vm-disk`, PR #68):** refuses a disk that has a chain. The user deletes or flattens snapshots first.
- **Clone of a VM disk:** a clone flattens the chain (`qemu-img convert`), the same as a restore.
- **Golden images:** not affected; they are plain raw files.
- **Group snapshots:** unchanged by this spec. An OpenShift VM snapshot gives one moment for all disks without them.

### Code and docs that change

- `pkg/vmsnap` (new): take, merge and flatten, using the `Execer` from `pkg/vmcopy`.
- `pkg/pool/manager.go`: choose the method; call `pkg/vmsnap`; keep retained folders.
- `pkg/webhook`: new mutating webhook for VMIs.
- `api/v1alpha1`: `SubVolumeStatus.DiskChain`; `SnapshotStatus.Method`, `MethodReason`, `ChainFile`; then `make generate` and `CRD-SPEC.md`.
- Helm chart: the webhook, the ConfigMap, `vmSnapshots.method`, RBAC to patch VMIs and read the `KubeVirt` resource; `HELM-VALUES.md`.
- `cmd/` : the `flatten` command.
- Docs: `VM-INSTANT-SNAPSHOTS.md` (from design to built), `FEATURES.md`, `KNOWN-LIMITATIONS.md`, `CONSISTENCY-MODEL.md`, `INSTALL.md`, `TROUBLESHOOTING.md`, `MONITORING.md`, `CHANGELOG.md`.

## Lab checks (done, rovslab, 21 Oct)

1. **KubeVirt keeps the read-only bits.** A VM with a chain started with the hook, and `disk.img` stayed `0444`. Without the hook, the VM did not start: QEMU said "Could not open .../disk.img: Permission denied". The read-only rule works, so the webhook keeps `failurePolicy: Ignore`.
2. **A live migration straight after a snapshot keeps the chain**, even for a VM started with no hook at all. The target takes the chain from the running VM.
3. **With the gate off, KubeVirt refuses a VMI that has the hook annotation.** So the webhook adds the hook only when the gate is on and the VM has a chain (see "The hook").
4. **Time limits:** a `VirtualMachineSnapshot` fails after 5 minutes by default, and the chart runs `csi-snapshotter` with `--timeout=300s`. OpenShift held the freeze for 1 second with a fast `CreateSnapshot`. An instant snapshot takes 2 to 5 seconds per disk.
5. **The IBM add-on did not undo the `jsonpatch` annotation**, nor the driver's own `HyperConverged` change from 20 Oct. Only KubeVirt's operator wrote to the resource since then.

New finding: a merge into a read-only file fails. The merge rule above handles it.

## Tests

- Unit tests, written first, with a fake `Execer`: method choice, the chain record, the merge rule for each position in the chain, the read-only rule, the per-volume lock, retained folders, and the fallback when the gate is off.
- E2E on rovslab, Linux and Windows:
  - Make the VM from the standard catalog with the pool StorageClass, in the console. Take a snapshot from the console's Snapshots tab. It must be instant.
  - A 2-disk VM with a counter writer on both disks. Take an OpenShift VM snapshot. Restore it. The counters must match.
  - Snapshot time for a 100 GB disk must be under 10 seconds.
  - Restart, live migration and live migration right after a snapshot.
  - Delete the oldest, a middle and the newest snapshot; restart; check marker files.
  - Remove the hook from a VM with a chain: the VM must not start.
  - Turn the gate off: snapshots fall back to copy; `flatten` works.

## Cost and risk

- **Support:** the `Sidecar` gate is not supported by Red Hat, and it changes a resource that IBM's add-on manages. Every customer cluster needs it. This is the main risk, and it is a product decision, not a technical one.
- **Data safety:** a write to a frozen file damages every later snapshot. The read-only rule is the guard, and lab check 1 must pass.
- **Build effort:** about two to three weeks for code, tests and docs, plus lab time. It reuses the exec, Job and restore code that exists.

## Part 2 (not in this build): instant restore

A restored disk could be a new overlay file in the new folder, on top of the snapshot's frozen file in the old folder. That makes a restore instant, like VMware's revert, but it ties the new disk to the old folder. It needs its own spec.

## Decisions I made for you

- The instant method is the default wherever the cluster allows it. The copy method stays for clusters without the `Sidecar` gate.
- The hook uses KubeVirt's own shim image and a ConfigMap, as tested. No new image to pull into user namespaces.
- The webhook adds the hook only to VMs that have snapshots, and only while the gate is on. It does not block VM starts if it is down. Safety comes from the read-only rule.
- Chain state lives in the SubVolume status. After each change the driver reads the real chain from QEMU, and the real chain wins.
- At most 16 snapshot files per disk. More are refused with a clear message, because long chains slow reads.
- A plain `VolumeSnapshot` covers one disk. All disks at one moment comes from an OpenShift VM snapshot. No new group snapshot work.
- Deleting a snapshot is a merge into its own file, so other snapshots never move.
- A restore is a full, sparse copy. Instant restore is part 2.
- A claim with snapshots keeps its folder until the last snapshot goes.
- Each snapshot is charged 10 % of the disk size; real use is measured and shown.
- Turning the gate off never deletes data. VMs with chains refuse to start until you run `flatten`.
- The wrong-moment bug in the copy method and the other snapshot bugs found on 21 Oct are fixed in separate issues, not in this build. The exception is the restore size check (#98), which the restore path needed.
- Changed while building: replication makes a full copy after each snapshot instead of moving its change list to the new file. Moving it safely would need the change list and the snapshot in one QEMU step, which libvirt does not offer; a full copy is always correct.
- Changed while building: a clone of a disk with snapshots is refused instead of flattened. Restoring a snapshot does the same job.
- Changed while building: new files name their backing file by file name only, and the stopped-VM and restore Jobs run in KubeVirt's virt-launcher image (it has `qemu-img`), mount only the folders they need, and check the chain before they open it.
