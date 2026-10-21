# Spec: snapshots of a running VM with several disks show one moment

Date: 2026-10-21. Status: draft for review. Issue #14. This extends `2026-10-21-vm-safe-snapshots.md`, which left several disks out.

## Goal

When this is done:
- A KubeVirt snapshot of a running VM with two or more pool disks gives disks that fit each other. Every file that one disk names is on the other disk. A VM restored from the snapshot boots with all its disks from one moment.
- Each disk's Snapshot shows the same copy moment.
- Snapshots of VMs with one disk, and of everything else, work as they do now.

## What the lab showed (21 Oct 2026, rovslab-dr-eude)

A running VM with a system disk and a data disk. A writer puts file n on the data disk, then logs it on the system disk.
- KubeVirt made one `VolumeSnapshot` per disk and called the VM snapshot done after 11 s. It does not wait for our copies, and it never calls the group snapshot API.
- Our driver copied the disks one after the other: the data disk at 13:16:25 and the system disk at 13:17:45, 80 s apart. `startVMSnapshotCopy` makes snapshots of one VM take turns.
- In the restored VM, 725 of 1,154 logged files were missing on the data disk. Each disk was sound alone.

Making the group snapshot API work would not help: KubeVirt does not use it.

## Design

The driver copies all pool disks of the VM at one moment, when the first disk's snapshot request arrives. Later requests for the same VM use that copy.

1. **Find the disks.** When a `CreateSnapshot` for a running VM disk starts its copy, the controller lists every pool disk the VM pod mounts (replication already does this). It starts one QEMU copy for all of them (`vmcopy.Copier.Start` already takes a list of volumes and gives one moment).
2. **The first Snapshot owns the copy.** Its `status.vmCopy` lists every disk with its staging file. It watches the QEMU copy as it does today.
3. **Other Snapshots join.** When a request for another disk of the same VM arrives while a copy set exists for that VM, the new Snapshot records the set (same copy id and moment, its own staging file) and does not start a QEMU copy. When the QEMU copy is done, each Snapshot starts its own apply Job and becomes ready.
4. **A set that nobody claims is dropped.** KubeVirt makes all `VolumeSnapshot` objects within seconds. A staging file that no Snapshot claims within 10 minutes of the QEMU copy is deleted. A request that arrives after that starts a new set.
5. **One disk, or no sibling.** A VM with one pool disk behaves as today. A lone `VolumeSnapshot` of one disk of a multi-disk VM also copies every disk, and drops the others after the wait.
6. **Failure.** A migration cancels the whole set and every joined Snapshot tries again, up to 3 times as now. A Snapshot that is deleted before its apply Job leaves the set; the set ends when none is left.
7. **The apply Jobs** write each disk's `disk.img` into its own snapshot folder, as today. Restore is unchanged.

## What it costs

- **Time and space for a lone snapshot.** A snapshot of one disk of a VM with a 500 GB data disk still copies the data disk to a staging file, and drops it after 10 minutes. The staging files sit in each disk's folder on its share, so the share needs room for them. This is the price of one moment for all disks.
- **More code in the part that moves the copy forward.** The controller must keep one copy set per VM across several Snapshot resources. The tests below cover the cases where this breaks.

## Tests

- Unit tests with the fake copier: two Snapshots of one VM share one copy and one moment; a third request after 10 minutes starts a new set; a migration cancels the set and both Snapshots retry; delete of one Snapshot while the other finishes; a controller restart in the middle; a one-disk VM is unchanged.
- Lab test: `test/e2e/test-vm-snapshot-2disk.sh` (PR #86) is the check. It must report `MISSING=0` and copy moments within 1 s. Stage 06 of the release test runs it.

## Decisions I made for you

- All pool disks of the VM are copied at one moment, even when only one disk was asked for. A guess at which disks belong together would be less safe than the extra copy.
- The wait for sibling requests is 10 minutes. KubeVirt needs seconds. A longer wait only costs staging space.
- A disk of the VM that is not on a pool is not copied. A VM that mixes pool disks and other storage does not get one moment across both; the Snapshot says so in its status.
- Restore and the group snapshot API do not change. KubeVirt does not use the group API, so the API stays as a separate item.
- The 2-disk test stays in stage 06 and fails until this is built. A failing test is the honest state.
