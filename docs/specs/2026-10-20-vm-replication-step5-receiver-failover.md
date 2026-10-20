# Spec: VM disk replication in receiver mode, and failover to a chosen moment

Date: 2026-10-20. Status: draft for review. This is step 5 of `2026-10-20-vm-safe-replication.md`. Read that spec first.

## Goal

When this step is done:
- In receiver mode (`destinationEndpoint` set), running VM disks replicate safely, with the same guarantees as direct NFS mode. Today receiver mode skips them.
- Each changed block crosses the network once, not three times.
- After a full copy to a disk that already exists, only the blocks that differ cross the network.
- Failover of a VM with several disks always gives disks from the same moment, even if a copy stopped half way, and even if the source region is gone.
- You can fail over to an earlier restore point, for example before ransomware.

## What exists today

- Direct NFS mode (step 4): the apply Job writes into the far-site disk over NFS. It saves old blocks in undo files first. Each changed block crosses the network about three times.
- Receiver mode: sync-client Jobs send a tar stream to an HTTPS receiver. The receiver extracts files. It does not know about VM disks, and the controller skips VM volumes in this mode.
- Failover (`cmd/failover`): reads one metadata file per SubVolume and creates the SubVolume, PV and PVC. It treats every PVC alone. It does not know which PVCs belong to one VM, and it does not look at undo files.
- `pkg/replication/undo.go` already has apply, roll back, rewind and prune. It works on a local folder, so the receiver can call it unchanged.

## Design

### A new receiver call for VM disks

The sync client sends the staging file in three calls. All use the existing token and TLS.

1. **Compare** `POST /api/v1/vmcopy/{subvolume}/compare`. The client sends a SHA-256 hash for each 64 KiB block of the staging file's data. It only sends this for a full copy. The receiver compares the hashes with its own disk and answers with the list of blocks that differ. For a change-only copy, this call is skipped, because every listed block already changed.
2. **Upload** `PUT /api/v1/vmcopy/{subvolume}/{copyID}`. The client sends only the needed blocks, as a stream of offset, length and data. The receiver writes them to a sparse staging file in an `.repl-incoming` folder next to the disk. The call can be repeated with the same `copyID`; the receiver keeps what it has and asks only for the rest.
3. **Commit** `POST /api/v1/vmcopy/{subvolume}/{copyID}/commit`. The client sends the disk size, the moments `from` and `to`, and a hash of the whole upload. The receiver checks the hash. Then it calls `ApplyCopy`, the same function direct NFS mode uses, on its local copy of the folder. So the undo file is written at the far site, and each changed block crosses the network once.

**The `from` check.** The receiver compares `from` with the moment its disk shows now. If they differ (a copy was lost, or someone else changed the disk), it answers 409. The client then does a full copy with the compare call. No copy is applied on top of the wrong base.

**Safety rules for the receiver:**
- The receiver uses `os.Root` on the volume folder, as `undo.go` does. A link in the folder cannot send a write outside it.
- It refuses a block whose offset plus length is beyond the declared disk size. It refuses a declared size above a limit in the policy (default 2 TiB).
- It allows one call per disk at a time. A second commit for the same disk waits or answers 429.
- It deletes `.repl-incoming` files older than 24 hours.
- The subvolume name must match `^pvc-[a-f0-9-]{36}$`, as today.

### Several disks of one VM commit together

A VM with two disks must not end with disk 1 at moment B and disk 2 at moment A for long. The client works in two phases:

1. Upload all disks of the VM (steps 1 and 2 above). Nothing is applied yet.
2. Send commit for each disk, one after the other.

If the client stops between two commits, the disks show different moments. That is allowed, and failover handles it (next section). The next cycle finishes the commits, because the uploaded files are still there.

### The VM group and the moment, in the metadata

Failover can only choose a common moment if it knows which volumes belong to one VM, and what moment each disk shows. The metadata file of each SubVolume gets two new fields:
- `vmGroup`: namespace and VM name, taken from the source VM pod when the copy is made.
- `vmMoment`: the moment the disk shows. The receiver writes it in the same commit step, after the disk is marked applied.

The moment of a disk is also known without the metadata: it is the `to` of the newest applied undo file. For a disk with no undo file yet, the metadata is the only record. If the two disagree, the undo file wins, because it is written first.

### Failover picks the latest common moment

For a VM, failover now does this:

1. Check that no commit is running on the disks (a lease file with a time limit, written by the receiver). If one is running, stop and say so. With `--force`, wait for its limit to pass.
2. Roll back any unfinished copy on each disk (`Recover`). This needs nothing from the source region.
3. List, for each disk, the moments it can show: its current moment, and the `from` of each applied undo file.
4. Choose the newest moment that every disk can show. If you gave `--rewind-to`, choose the newest common moment at or before that time.
5. Disks that already show that moment are used in place, as today. Other disks are rewound into a new disk (`Rewind` writes a new file and leaves the latest disk unchanged). The new disk gets its own folder, and the PV points to it.
6. Print the moment chosen, for each disk the moment it started at, and the RPO. If a disk is rewound, say how much extra space it needs.

If no moment is common to all disks (the 24 hour history was pruned on one disk only), failover stops and lists the moments each disk has. It never mixes moments.

New flags: `--rewind-to <RFC3339 time>` and `--list-moments` (print the common moments and exit).

### Remove the receiver-mode skip

The controller stops skipping VM volumes in receiver mode and uses the QEMU path with the three calls above. Hot-plugged disks are still skipped, as in direct NFS mode, because the volume list does not include them.

### Metrics and status

Status adds, for each VM volume: the mode used (`receiver` or `direct`), whether the last cycle was a compare pass, and bytes sent. New metrics: bytes skipped by the compare pass, and commit failures by reason (409, hash mismatch, size limit).

## Plan

This step is 3 PRs. Each adds its release test checks in the same PR.

1. **Receiver calls and the VM group.** The three calls, the `from` check, the incoming folder and its clean-up, the safety rules, and the new metadata fields. Tests: unit tests with a fake disk for each refusal; a test that a stopped upload resumes; a test that a wrong `from` gives 409.
2. **Controller and client in receiver mode.** The sync client sends the three calls. The controller removes the skip. The compare call for full copies. Two-disk commit order. Test: extend `test/e2e/test-vm-replication.sh` with a receiver-mode run on rovslab (two pools; the receiver runs in the same cluster, as in the existing receiver tests). It checks bytes sent are near the changed bytes, not three times that.
3. **Failover.** Latest common moment, `--rewind-to`, `--list-moments`, the lease check, the rewind into a new disk. Test: stop a commit between two disks (a test hook), run failover, boot the VM, and check that both disks show one moment. Then rewind to an older restore point, boot, and check that the checksums match that moment. Add this to release test stage 10.

Not in this step: the two-region test with Windows (step 6), and any change to snapshots or backup.

## Cost and risk

- **Hashing.** The compare call makes the client read the whole staging file once and hash it. This costs CPU and local read time, but no network. A 100 GB disk takes a few minutes. It happens only after a VM move or restart.
- **Space at the far site.** Unchanged from step 4: up to one more disk for 24 hours of restore points. A rewind needs one more full disk until you delete it.
- **Receiver is a new write path to far-site data.** It writes disk files, so a bug can corrupt a disk. The undo file protects the previous moment. The safety rules above and the shared `ApplyCopy` code limit the risk. Tests for refusals are part of PR 1.
- **Failover with a long wait.** If a commit hangs, failover waits for the lease to end, or you use `--force`. During a real disaster, `--force` is the right choice, because `Recover` makes it safe.
- **Rewind is slower than a normal failover.** It copies the whole disk, then applies undo files. A 100 GB disk takes minutes. A failover to the latest moment is as fast as today.

## Decisions I made for you

- Receiver mode uses the same apply code as direct NFS mode (`ApplyCopy`), run at the far site. One method, tested once.
- The client sends block hashes first, so a compare pass sends only the blocks that differ.
- A copy is applied only if the far-site disk shows the moment the copy starts from. If not, the client does a full copy with the compare pass.
- A VM's disks are uploaded first and committed one after the other. A stop between commits is allowed; failover fixes it.
- Failover chooses the newest moment that all disks of a VM can show. It never mixes moments, and it stops if there is no common one.
- Failover on a disk with an unfinished copy rolls it back first. It needs nothing from the source region.
- A rewind makes a new disk and leaves the latest one unchanged, so you can try another moment.
- Failover learns which volumes belong to one VM from a `vmGroup` field in the metadata.
- A running commit blocks failover until its lease ends; `--force` skips the wait.
- The default size limit for one disk in a receiver call is 2 TiB, set per policy.
- Hot-plugged disks stay skipped in receiver mode, as in direct NFS mode.
- This step does not change snapshots, backup or the Windows test. Those stay in steps 6 to 9.
