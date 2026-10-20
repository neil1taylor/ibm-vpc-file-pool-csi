# Spec: Replicate and back up running VMs safely

Date: 2026-10-20. Status: draft for review.

## Goal

Replication copies the disks of running VMs to the other region, and each copy is safe to boot. Safe means: every disk of a VM shows the same single moment. A copy never mixes data from before and after that moment.

"Done" means:
- A running VM's disks replicate on the policy's schedule, and the VM keeps running.
- After failover, the copy boots and its file systems mount without errors.
- If the VM has a guest agent, the copy is file-system consistent. If it does not, the copy is crash consistent, the same as a power cut. The status says which.
- A VM with several disks gets all its disks from the same moment.
- Only changed blocks cross between regions in normal operation.
- The driver never copies a running VM disk with rsync again.
- Optional backup: on a schedule, the driver keeps local snapshots in a separate snapshot pool, and can also export them to IBM Cloud Object Storage (COS). Both use the same safe copy as replication.

## What exists today

Replication copies files with rsync, on a schedule. For VM disks this is unsafe. rsync reads `disk.img` while the VM writes to it, so the copy mixes blocks from different moments. `CROSS-REGION-DR.md` already says VM disks are not supported, but the controller copies them anyway, without a warning.

OpenShift Virtualization will get a supported backup feature with change tracking (KubeVirt v1.8; Veeam says it is a Tech Preview in OpenShift 4.22). rovslab runs 4.21 (KubeVirt v1.7.4), so we cannot use it yet.

Snapshots have the same problem. A snapshot copies the volume's files into `/pvcs/.snapshots/` on the same share. For a running VM, that copy is also unsafe. Because it is on the same share, it is also lost if the share is lost. The driver has no backup to COS.

### Test on rovslab, 20 Oct

The controller can ask QEMU, the program that runs each VM, for a copy. Every VM pod has libvirt 10.10 and QEMU 9.1. I tested this with a CentOS Stream 9 VM on a pool volume. During the test the VM wrote about 200 MB/s without a pause.

| Check | Result |
|---|---|
| Freeze the guest file systems, start the copy, thaw | Guest paused for 0.1 s |
| Boot the full copy | Booted. All 150 checked files matched. The last file is the last one written before the freeze |
| Change tracking through libvirt | Refused: "checkpoint for disk vda unsupported for storage type raw" |
| Change tracking in QEMU memory | Works on our raw disks |
| Copy of changes only, applied to the full copy, then boot | 3 GB in 16 s, compared with 224 s for the full copy. Booted, all 150 files matched |
| Live migration | The change list is gone after the migration |
| Guest write speed during the copy | 20 times slower during the full copy, 10 times slower during the change-only copy. The copy was written to the same file share as the VM disk |
| Size of a full copy | 40 GB for a 40 GB disk with 5 GB of data, because the copy was not sparse |

Red Hat does not support this method. You accepted that on 20 Oct, until the supported feature arrives.

## Design

### Which volumes take the VM path

On each cycle, the controller checks every volume in the policy. A volume takes the VM path if a running VM uses it. The controller finds this from the VM pod that mounts the volume.

- **Running VM:** QEMU makes the copy (below).
- **Stopped VM, or no VM:** rsync is safe, because nothing writes to the disk. The controller checks that the VM is still stopped after the copy. If the VM started during the copy, the controller discards the result and tries again on the next cycle.
- **Running VM, but the QEMU path fails:** the controller skips the volume and reports an error. It never falls back to rsync.

### Take a copy at one moment

The controller runs commands in the VM pod. It already has permission to do this, for hooks.

1. Freeze the guest file systems through the guest agent. If there is no guest agent, skip this step and mark the copy "crash consistent".
2. In one QEMU step, for all disks of the VM: start a new change list, and open a read-only view of each disk at this moment.
3. Thaw the guest. The guest is paused for about 0.1 s.
4. A copy Job reads the disk views and sends the data to the other region (below).
5. When the transfer is complete, close the views.

The disk view uses QEMU's "pull" backup. QEMU serves the frozen view over the network, and the copy Job reads it. If the VM writes a block that the Job has not read yet, QEMU first saves the old block to a small scratch file. So, unlike the tested method, no full-size copy of the disk is written to the share. This removes the 40 GB staging copy and half of the extra disk writes.

The pull backup is not tested yet. Step 1 of the plan tests it. If it does not work, the fallback is the tested method: QEMU writes a staging copy to the share, and the Job sends that copy.

The copy Job runs in the VM's namespace. So it reaches the VM pod on the VM's own network, also when the VM uses a user-defined primary network. The view is protected with TLS and a certificate made for that one copy.

### Send only changed blocks

- **Normal cycle:** the Job reads only the blocks in the change list, and sends them over HTTPS to the receiver in the other region. This uses the existing driver-to-driver replication path.
- **First copy, or after the change list is lost:** a live migration or a VM restart erases the change list. The controller sees that the list is gone, and does a compare pass. The Job reads the whole disk, but sends only a checksum for each 4 MB block. The receiver compares the checksums with its own copy and asks only for the blocks that differ. So a node drain costs a full read in the source region, but not a full transfer between regions.

### Apply the copy in the other region

The far-site disk must never be left half updated.

1. The receiver writes the incoming blocks to a change file next to the disk, not into the disk.
2. When all blocks have arrived, the receiver marks the change file complete.
3. Then the receiver writes the blocks into the disk, and deletes the change file.

If the receiver stops during step 3, it applies the same complete change file again when it restarts. This is safe, because each block goes to a fixed place in the disk. An incomplete change file is deleted, and the disk keeps the previous good copy.

Failover finishes any complete change file first. It refuses to use a disk while a change file is being applied.

### What the status shows

For each VM volume, the policy status shows:
- the moment the last good copy shows
- "file-system consistent" or "crash consistent"
- whether the last cycle was a change-only cycle or a compare pass, and the bytes sent

New metrics: copy type, guest pause time, and how long the VM was slowed by a copy.

### Optional backup: snapshot pool and COS

A new `BackupPolicy` resource turns backup on for a set of volumes. It has a schedule, a snapshot pool, how long to keep local snapshots, and optional COS settings. Backup is off unless you create a policy.

**Local snapshots in their own pool.** A snapshot pool is a normal pool that holds only snapshots. It uses its own VPC file shares, so a lost or full data share does not take the snapshots with it.

- Each scheduled snapshot uses the safe copy above: QEMU for running VMs, a file copy for everything else.
- VM disks are kept as a chain: one full copy, then one change file for each later snapshot. This uses the same change list as replication. The 20 Oct test showed that a change file applied to its full copy boots.
- Other volumes are kept with rsync and hard links. A file that did not change takes no extra space.
- When a snapshot passes its keep time, the driver merges it into the next one in the chain, so the chain stays short.
- Restore makes a new PVC from any snapshot, as restore does today.

**Export to COS (optional).** After each local snapshot, the driver uploads it to a COS bucket.

- It reads from the snapshot pool, not from the VM, so the export does not slow the VM.
- It uses Kopia, an open-source Go backup library. Kopia splits data into chunks, uploads only chunks that COS does not already have, and encrypts the data before upload. The key is in a Kubernetes Secret that you keep. Writing this ourselves would take months. Kopia is also what Velero and Kasten use.
- It has its own keep time, usually longer than the local one.
- It can use COS object lock, if the bucket was created with it. Then nobody can delete or change a backup before its keep time ends, which protects against ransomware. This is off by default, because the bucket must be created with it.
- Restore from COS makes a new PVC in any cluster that runs the driver, including a cluster in another region. This is the last line of defence if both regions lose their pools.

**Fix for today's snapshots.** A snapshot of a running VM volume also uses the safe copy, not a file copy.

### Later: switch to the supported feature

The part that takes the copy is one replaceable piece. When the clusters run the OpenShift version with the supported backup feature, we replace that piece. The transfer, the receiver and failover stay the same.

## Plan

1. **Lab test of the pull backup (no product code).** On rovslab, check that:
   - a Job in the VM namespace can read the frozen view over TLS
   - the change list can be read through it
   - the VM slows down less than in the 20 Oct test, measured with the same load
   - a Windows VM freezes and thaws through its guest agent
   - it works on a VM with two disks

   If the pull backup fails, the plan uses the tested staging method, and this spec gets an update.
2. **Stop unsafe copies.** The controller detects running VMs and skips their volumes with a clear error. This ships first and by itself, because today's behaviour makes copies that cannot boot.
3. **Copy at one moment.** Freeze, take the view, thaw, read the full disk, and send it.
4. **Change-only cycles and the compare pass.**
5. **Safe apply in the other region, and failover changes.**
6. **Test across two regions,** with a failover and a boot of a Linux VM and a Windows VM. Then update `CROSS-REGION-DR.md`, `FEATURES.md` and the tutorial.
7. **Safe snapshots of running VMs,** using the copy from step 3.
8. **Backup to a snapshot pool:** the `BackupPolicy` resource, the chain of change files, hard-link snapshots, merging, and restore.
9. **Export to COS:** Kopia upload, encryption, keep time, object lock, and restore from COS into another cluster. Then add a backup stage to the release test run.

About 5 PRs for replication after the lab test, and 3 more for backup.

## Tests in the release test run

Each part of this plan adds its checks to the unattended release test run, in the same PR as the code. The checks are listed in `2026-10-19-release-test-run.md`: stage 5 (snapshot of a running VM), stage 10 (VM replication) and stage 18 (backup). Each check looks at the result itself: the copy boots, the files match their checksums, and the bytes sent. It does not trust a status field.

The run needs one COS bucket for the backup stage. I will ask you to create it when backup is built.

## Cost and risk

- **VM slowdown during a copy.** It was large in the test, with a VM that wrote nonstop. Step 1 measures it again with the pull backup. If it is still large, we add a speed limit for the copy Job. A slower copy makes a longer copy window, so this is a trade-off between RPO and VM speed. The policy will have a setting for it.
- **Red Hat does not support it.** An OpenShift Virtualization update can change the VM pod and break this method. Each release test run will include a VM replication stage, so we find a break before you do. If it breaks, VM volumes are skipped with an error. They are never copied unsafely.
- **Security.** The controller runs commands in VM pods. It already can, for hooks. The disk view is reachable only inside the VM's namespace, with a one-time certificate.
- **Backup cost.** A snapshot pool is extra VPC file shares, paid for as normal shares. COS is paid for each GB stored, and only changed chunks are added after the first upload. A cost estimate goes into this spec once you choose the keep times.
- **New dependency.** Kopia, for the COS export only.
- **Scratch space.** Blocks that the VM overwrites during a copy are saved in a scratch file on the VM's share until the copy ends.

## Decisions I made for you

- Running VM disks are never copied with rsync. If the safe method fails, the volume is skipped and reported.
- Step 2 (stop unsafe copies) ships first, before the new copy method exists.
- A VM without a guest agent still replicates, as a crash-consistent copy. The status says so.
- Stopped VMs still use rsync, with a check that the VM stayed stopped.
- The copy is read over the network from QEMU, not written to the share first. The tested staging method is the fallback.
- After a migration or restart, a compare pass sends only the blocks that differ, not the whole disk.
- The far-site disk is updated through a change file, so it is never left half written.
- Replication copies disks only. VM definitions are not replicated. After failover, you create the VMs from your own definitions, as today.
- Change tracking stays in QEMU memory, because libvirt refuses it on our raw disks. Changing the disk format is out of scope.
- The copy piece is built to be replaced by OpenShift's supported backup feature later.
- Every part adds its release test checks in the same PR as its code.
- Backup is optional and off by default. It is a separate `BackupPolicy`, not part of replication, so you can use either one without the other.
- Local snapshots go to their own pool, never to the data share.
- VM disk snapshots are a chain of change files, and old ones are merged. Other volumes use hard-link snapshots.
- The COS export reads from the snapshot pool, never from the running VM.
- The COS export uses Kopia (chunking, encryption, upload of new chunks only), instead of our own format.
- Backups in COS are always encrypted. You keep the key.
- Object lock is supported but off by default, because the bucket must be created with it.
- Snapshots of running VMs are fixed to use the safe copy, as part of this work.
- Backup is built after replication, because it reuses the safe copy and the change list.
- Multi-disk VMs are copied at one moment as part of this work. Group snapshots (issue #14) stay a separate issue.
