# Spec: safe snapshots of running VMs

Date: 2026-10-21. Status: draft for review. This is step 7 of `2026-10-20-vm-safe-replication.md`. Read that spec first.

## Goal

When this step is done:
- A snapshot of a PVC that a running VM uses shows one moment, and the guest file system is consistent. A VM restored from it boots.
- The snapshot does not stop the VM for more than a fraction of a second.
- Snapshots of everything else (stopped VMs, pods, databases) work as they do today.
- You see in the Snapshot's status whether it is file-system consistent or crash consistent.

## What exists today

- `CreateSnapshot` makes a Snapshot resource and starts a Job that copies the volume's folder to `/pvcs/.snapshots/snap-<name>` on the same share. For a running VM this is an unsafe copy: the Job reads the disk file while the VM writes to it, so the snapshot mixes moments. This is the same defect that step 2 removed from replication.
- Replication already has the safe copy (`pkg/vmcopy`): freeze the guest, start a QEMU copy of the disk, thaw, wait, then an `apply-vm-disk` Job turns the staged file into a raw `disk.img`. Lab results are in the main spec. Linux and Windows both passed on 21 Oct.
- Restore (`RestoreSnapshot`) copies the snapshot folder into a new volume. It does not care how the folder was made.

## Design

### Which snapshots take the new path

`CreateSnapshot` looks for a KubeVirt pod (`virt-launcher` or `hotplug-disk`) that runs or starts and mounts the source claim. This is the same check replication uses, and it moves to a shared place so both call it.
- No such pod: the file copy, as today.
- A pod exists: the VM path below.

### The VM path

1. The Snapshot gets `status.vmCopy`, a small record of the copy: the VM pod, the staging file, the copy id and the phase. It has the same shape as replication's `vmCopy` state.
2. The controller freezes the guest, tells QEMU to copy this one disk, and thaws. The copy goes to a staging file in the volume's own folder.
3. When QEMU has finished, the controller starts an `apply-vm-disk` Job. It writes a raw `disk.img` into the snapshot folder on the same share. It has no undo file and no change list, because a snapshot is always a full copy. The Job deletes the staging file.
4. The Snapshot becomes ready. Its status shows the copy moment and `consistency`: `FileSystemConsistent`, or `CrashConsistent` if the guest could not freeze.

A background loop in the controller moves the copy forward every 10 seconds. Snapshots do not depend on the snapshotter calling `CreateSnapshot` again, because a copy takes minutes.

### Failure cases

- **Live migration.** A migration cancels the copy, as in replication. The controller removes the staging file and tries again, up to 3 times, one minute apart. After that the Snapshot fails with a message that names the migration.
- **The VM stops during the copy.** If the QEMU copy fails because of this, the controller starts again with the file copy, because the disk is now at rest.
- **A VM starts during a file copy.** When a file copy ends, the controller checks again for a VM pod. If one is there, it deletes the snapshot folder and starts again on the VM path.
- **Delete during a copy.** The controller cancels the QEMU copy, removes the staging file and the snapshot folder, and then removes the Snapshot.
- **A copy that runs too long.** QEMU does not report how far a copy is, so progress cannot be measured. A copy that runs longer than 2 hours plus twice the time the disk needs at the speed limit is cancelled and counts as one try. (Changed during the build: the first draft said "no progress for 2 hours".)
- **Controller restart.** The record in `status.vmCopy` lets the new leader carry on. If the VM pod is gone, the try is cancelled.

### Speed limit

A VolumeSnapshotClass parameter `vmCopySpeedLimitMBps` limits how much the copy slows the VM. The default is 20, the same as for replication. A 20 GB disk takes about 20 minutes at this limit (lab, 20 Oct), because empty space counts. A higher limit is faster and slows the VM more.

### What this step does not do

- It does not make several disks match. A snapshot is one PVC. (Matching several disks was added later: see `2026-10-21-multi-disk-vm-snapshots.md`.)
- No chain of change files and no merging. Every snapshot is a full copy. The chain belongs to the backup step (step 8).
- No change to restore, or to group snapshots.

### Changes to code and docs

- `api/v1alpha1`: `SnapshotStatus` gets `vmCopy`, `consistency` and `consistencyReason`. Run `make generate`. Update `CRD-SPEC.md`.
- New: the copy loop for Snapshots, in `pkg/pool`. The Job builder shares code with replication's.
- Docs: `FEATURES.md`, `KNOWN-LIMITATIONS.md` (one disk per snapshot), `BACKUP-RECOVERY.md`, `CHANGELOG.md`.
- Metrics: the existing snapshot metrics count VM snapshots too. No new metric.

## Tests

- Unit tests with the fake copier: the VM path starts, advances and finishes; a migration retries three times and then fails; a VM starts during a file copy; delete during a copy; a restart in the middle.
- Lab test on rovslab (the release test stage 5): a CentOS VM writes a file every 0.1 s with its checksum in a log. Take a VolumeSnapshot while it runs. Restore it to a new PVC and boot a VM from it. The file system has no errors and every logged file matches its checksum. Repeat with the Windows VM.

## Cost and risk

- **Slower snapshots of running VMs.** The old copy was fast and wrong. A safe snapshot takes as long as a full copy: about 20 minutes for a 20 GB disk at the default limit.
- **Space.** A snapshot of a running VM is a full copy of the allocated blocks, on the same share as the disk. Hard-link or chain snapshots come with step 8.
- **Red Hat does not support the QEMU commands this uses.** The same risk as for replication. The step is easy to replace later with the supported feature.
- **A snapshot on the same share is lost with the share.** This step does not change that. Step 8 puts snapshots in their own pool.

## Decisions I made for you

- A VM disk snapshot is a full copy every time. Chains wait for the backup step.
- The snapshot path is chosen by looking for a running VM pod. There is no new setting to turn on.
- One PVC per snapshot. Matching several disks stays with group snapshots (issue #14).
- The default speed limit is 20 MB/s, the same as replication. A class parameter can change it.
- A migration retries the copy three times, one minute apart, and then fails.
- A copy that runs longer than 2 hours plus twice the expected time is cancelled. Progress cannot be measured, so a time limit replaces "no progress".
- A snapshot never removes the change lists that replication keeps. It only checks that no other copy runs in the VM, and waits if one does. Replication waits the same way.
- The status says whether the snapshot is file-system consistent or crash consistent. A crash-consistent snapshot is still ready to use; it is not an error.
- If the VM stops during the copy, the controller falls back to the file copy.
- The copy runs in a loop in the controller. It does not wait for the snapshotter to ask again.
