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

The controller runs commands in the VM pod. On OpenShift, a user may run commands in a pod only if it could also create that pod. VM pods run under the `kubevirt-controller` security policy (SCC), so the controller's account gets `use` on that one SCC. Kasten and IBM Backup and Recovery have the same permission for the same reason. This gives the account no new power to start pods. It cannot create pods directly. A pod can run as this account only in `kube-system`, where OpenShift applies no SCC at all, so the Jobs the controller already creates there are unrestricted. Found in the end-to-end test on 20 Oct: without it, every copy failed with "exec operation is not allowed because the pod's security context exceeds your permissions".

1. Freeze the guest file systems through the guest agent, with KubeVirt's own freeze tool in the VM pod.
   - The tool thaws the guest by itself after a timeout, even if the controller never sends the thaw. The controller sets 30 s. So a lost connection cannot leave the guest frozen.
   - If there is no guest agent, skip this step.
   - If the freeze fails, thaw and go on without it. Example: on a Linux guest with SELinux, one file system with a missing label makes the whole freeze fail.
   - In both cases the copy is marked "crash consistent", and the status shows the guest's error.
2. In one QEMU step, for all disks of the VM: start a new change list, and start a copy of each disk as it is at this moment.
3. Thaw the guest. The guest is paused for about 0.2 s.
4. QEMU writes the copy to a staging file in the VM's own volume, at a speed limit.
5. A copy Job sends the staging file to the other region (below), then deletes it. The Job runs as the same user as QEMU, because only that user can delete the file.

**If the VM must move during a copy.** A VM cannot live-migrate while QEMU copies its disks: the move fails, and the VM stays where it is (tested 20 Oct). A worker drain or a cluster update would wait for the copy. So the controller watches for a requested move. When it sees one, it cancels the copy and deletes the staging files. The move then works, and the next cycle starts a new copy. A move waits at most one controller cycle, 30 s by default.

**Reading the staging file.** The staging file is in QEMU's qcow2 format. The driver image cannot install `qemu-img`, so the driver reads the format itself. It accepts only what QEMU makes for a copy, and it refuses anything else. A staging file lies in a volume that other pods in the namespace can write, so the reader checks every table against the file size.

The staging file is sparse: it holds only the data the disk really uses (a full copy) or the blocks that changed (a change copy). In the step 1 test, a full copy of a 40 GB disk with 5 GB of data was 5.1 GB.

**Speed limit.** Without a limit, the copy uses all the share's bandwidth, and the VM's writes were 16 to 19 times slower. At 20 MB/s per disk, the VM kept 93% of its normal write speed. The policy has a speed setting, 20 MB/s per disk by default. A higher limit gives a shorter copy and a lower RPO, but slows the VM more.

The limit counts empty disk space too. A full copy of a 20 GB disk with 2 GB of data took about 18 minutes at 20 MB/s (tested 20 Oct). Change copies are not affected. Full copies happen only on the first copy and after a VM moves or restarts.

**Why not read the disk over the network.** The earlier draft read the disk from QEMU over the network, with no staging file. Step 1 showed that this cannot work. The VM pod forwards every incoming connection to the guest, so nothing outside the pod can reach QEMU. The only other way is through the node, with a privileged reader. That stays a possible later change, because the node agent is already privileged.

### Send only changed blocks

- **Change list.** QEMU keeps a change list (a dirty bitmap) for each disk, in memory. Each copy starts a new list at its moment. The list of the last copy that reached the other region stays until the next copy has reached it too. So if a copy fails, the next one still includes every block that changed since the last good copy.
- **Normal cycle:** QEMU writes only the blocks in that list to the staging file.
- **First copy, or after the change list is lost:** a live migration or a VM restart erases the lists. The controller sees that the list is gone and asks QEMU for a full copy. The staging file of a full copy holds only the data the disk really uses.
- **Compare pass.** When a full copy reaches a far-site disk that already exists, only the blocks that differ are written, and only those go into the undo file (below). In direct NFS mode the Job compares the blocks itself, so it reads the far-site data once over the network; that costs about as much as sending it. Receiver mode will compare at the far site, from checksums, so only the blocks that differ cross the network.
- The Job never reads the VM's live disk file. It reads only staging files, which do not change while it reads them.

### Apply the copy in the other region

The far-site disk must never be left half updated, and a rewind must be possible. One method serves both, and it works in direct NFS mode, where nothing of ours runs in the other region:

1. Before it changes any block, the Job reads the far-site disk's current contents of every block it will change, and writes them to an undo file next to the disk. Then it marks the undo file complete.
2. It writes the new blocks into the disk.
3. It marks the undo file applied. The undo file is now a restore point: it can take the disk back to the moment before this copy.

If the Job stops during step 2, the disk is half updated, but the complete undo file is at the far site. The next Job, or failover, first rolls the disk back from that undo file to the previous good moment. This needs nothing from the source region, so it also works after the source region is lost. An undo file that is not complete is deleted; the disk was not touched yet.

In direct NFS mode each changed block crosses the network about three times: the old block is read and written to the undo file, and the new block is written. Receiver mode will do steps 1 to 3 at the far site, so each block crosses once.

Failover rolls back any applied-but-not-marked copy first. It refuses to use a disk while a Job is applying a copy.

**Full copies in step 3.** Step 3 sent whole disks and swapped each one in only when complete. Step 4 changes this to the method above, so the first copy after the change gives the first restore point.

### Restore points in the other region

The far site keeps the copies of the last 24 hours, so you can rewind if bad data reached the VM, for example ransomware or a broken update. Each policy has a keep time, 24 hours by default.

- Each copy's undo files form one restore point. A restore point covers all disks of a VM at the same moment.
- The disk itself always holds the latest copy. So a failover to the latest copy is as fast as today.
- To rewind, failover makes a new disk from the latest one, then applies the undo files from newest to oldest, back to the chosen moment. The latest disk is not changed, so you can rewind again to a different moment.
- When an undo file is older than the keep time, the next Job deletes it. Undo files are deleted oldest first, so the remaining ones always form a complete chain back from the latest copy.
- After a compare pass, the undo file holds only the blocks that differed. So a VM move does not cost a full disk of storage.
- The policy status lists the restore points.

### What the status shows

For each VM volume, the policy status shows:
- the moment the last good copy shows
- "file-system consistent" or "crash consistent"
- whether the last cycle was a change-only cycle or a compare pass, and the bytes sent
- the oldest and newest restore points in the other region

New metrics: copy type, guest pause time, and how long the VM was slowed by a copy.

### Optional backup: snapshot pool and COS

A new `BackupPolicy` resource turns backup on. It has a schedule, a snapshot pool, how long to keep local snapshots, and optional COS settings. Backup is off unless you create a policy.

A policy selects VMs by label, and covers all disks of each VM, taken at one moment. It can also select normal PVCs by label. One VM belongs to at most one backup policy. If two policies match a VM, the webhook refuses the second one.

**Local snapshots in their own pool.** A snapshot pool is a normal pool that holds only snapshots. It uses its own VPC file shares, so a lost or full data share does not take the snapshots with it.

- Each scheduled snapshot uses the safe copy above: QEMU for running VMs, a file copy for everything else.
- VM disks are kept as a chain: one full copy, then one change file for each later snapshot. This uses the same change list as replication. The 20 Oct test showed that a change file applied to its full copy boots.
- Other volumes are kept with rsync and hard links. A file that did not change takes no extra space.
- When a snapshot passes its keep time, the driver merges it into the next one in the chain, so the chain stays short.
- Restore makes a new PVC from any snapshot, as restore does today.

**Export to COS (optional).** After each local snapshot, the driver uploads it to a COS bucket.

- It reads from the snapshot pool, not from the VM, so the export does not slow the VM.
- It uses Kopia, an open-source Go backup library. Kopia splits data into chunks, uploads only chunks that COS does not already have, and encrypts the data before upload. Writing this ourselves would take months. Kopia is also what Velero and Kasten use.
- It has its own keep time, usually longer than the local one.
- Each policy has its own area in the bucket. So the locks of one policy cannot affect another. The cost: two policies do not share identical chunks, so storage is a little higher.

**Setup command for COS.** You run it once, with your own IBM Cloud login. It is in the console plugin and in the CLI. It:

- creates the COS instance, if you do not give an existing one
- creates the bucket, with object lock turned on and no fixed keep time, so each policy decides
- sets your Key Protect root key for the bucket
- creates a key for the driver, with a custom role on this one bucket only. The role can read, write, and extend locks. It cannot delete a version of an object, skip a lock, or change the bucket settings. IBM's standard Writer role is not used, because it can skip Governance locks.
- gives the driver the Reader role on the root key in Key Protect. Reader can encrypt and unlock the backup key, but cannot delete the root key.
- creates the backup key (below), has Key Protect encrypt it, and shows the readable key to you once

The driver itself never creates or deletes a COS instance or bucket. If someone takes over the cluster, they get only the driver's key, and that key cannot remove a lock. An uninstall or a clean-up of the driver never touches the bucket.

**Encryption.** Backups are encrypted in three places:

- on the way to COS, with HTTPS
- inside COS, with COS encryption and your Key Protect root key. While object lock is on, COS stops anyone from deleting that root key, so nobody can make the backups unreadable by deleting it
- before upload, by Kopia with the backup key, so COS only ever holds encrypted data

**The backup key is protected by Key Protect.** Key Protect encrypts the backup key with the same root key as the bucket. Only that encrypted copy is stored: in a Kubernetes Secret, and next to the backups in COS. The readable key is never stored. When the driver needs the key, it asks Key Protect to unlock it, and keeps it only in memory.

- Because it is the bucket's root key, object lock also protects it from deletion.
- If the cluster is lost, a restore in another cluster reads the encrypted copy from COS and asks Key Protect to unlock it.
- If you remove the driver's access in Key Protect, the driver can no longer read or write backups.

**Emergency copy.** Key Protect runs in one region. If that region is down, Key Protect cannot unlock the key. So the setup command shows the readable key once, and you keep a copy outside IBM Cloud, for example in a password manager. A restore can use it instead of Key Protect. Without either Key Protect or this copy, nobody can read the backups, including IBM and us.

**Immutable backups.** Each policy sets a keep time for its COS backups, and either Compliance lock or no lock:

- **Compliance:** nobody can delete or change a backup before its keep time ends. That includes you, your account admins and IBM. This is the default for a new policy, with a keep time of 30 days.
- **None:** no lock.

A change to a policy affects only new backups. A backup in Compliance mode keeps its lock to the end, even if you change or delete the policy. COS bills a locked backup until its lock ends. IBM requires the account to stay in good standing while any lock is active.

**What the step 1 test showed (Kopia 0.23.1, IBM COS, 20 Oct):**

- Kopia locks its data, its indexes and its repository files in COS. Restore matched the checksums. No readable text from the backup was found in any object.
- No role could shorten a Compliance lock.
- **Governance mode does not protect against a stolen driver key.** IBM's Writer role can skip Governance locks. A custom role without that permission stops the skip. But the driver also needs permission to extend locks, and with it the driver can shorten a Governance lock. So Governance is not offered.
- **Locks must be extended.** A chunk that a new backup still uses keeps the lock from its first upload. Kopia extends the locks of chunks that are still in use during its maintenance run. This needs the "extend locks" permission, which is safe in Compliance mode. Kopia requires the keep time to be at least the maintenance interval plus 24 hours. The driver sets the maintenance interval to fit.
- **A delete by the driver's key only hides data.** COS adds a delete marker, and the data stays. But Kopia cannot read hidden data. So the restore command, run with your own login, first removes any delete markers. The driver's key cannot remove them.
- Restore from COS makes a new PVC in any cluster that runs the driver, including a cluster in another region. This is the last line of defence if both regions lose their pools.

**Fix for today's snapshots.** A snapshot of a running VM volume also uses the safe copy, not a file copy.

### Later: switch to the supported feature

The part that takes the copy is one replaceable piece. When the clusters run the OpenShift version with the supported backup feature, we replace that piece. The transfer, the receiver and failover stay the same.

## Plan

1. **Lab test (done, 20 Oct).** Results:
   - Reading the disk over the network: cannot work, see above. The plan uses the staging method.
   - Two disks: one QEMU step copies both at the same moment. The far-site disks booted, and both showed the same last write.
   - Speed limit: at 20 MB/s per disk, the VM kept 93% of its write speed.
   - Sparse staging files: 5.1 GB for a 40 GB disk with 5 GB of data.
   - A full copy plus two change copies, applied at the far site, booted with all files intact.
   - Linux freeze fails if a file system has no SELinux label. The design now falls back to a crash-consistent copy.
   - Windows freeze: not tested. The lab's only Windows disk is in use by other lab work. It moves to step 6.
   - COS and Kopia: see "Immutable backups" below.
2. **Stop unsafe copies.** The controller detects running VMs and skips their volumes with a clear error. This ships first and by itself, because today's behaviour makes copies that cannot boot.
3. **Copy at one moment.** Freeze, take the view, thaw, read the full disk, and send it. Cancel the copy when the VM must move. Lab test of the parts, 20 Oct:
   - KubeVirt's freeze tool is in the VM pod. Freeze took 0.11 s, thaw 0.03 s, and the automatic thaw worked.
   - A live migration during a copy fails, and the VM keeps running where it was. After the copy is cancelled, the migration took 8 s.
   - The speed limit counts empty disk space (above).
   - The staging file is owned by the QEMU user, and only that user can delete it.
   - End-to-end test, 20 Oct (`test/e2e/test-vm-replication.sh`, direct NFS mode, two pools on the driver): a CentOS VM with a 20 GB root disk and a 10 GB data disk wrote a file every 0.1 s. Both disks showed the same moment and were file-system consistent. The far-site copy booted with no XFS errors, and all 1,579 files written before the freeze matched their checksums; the last one was written at the moment of the freeze. Data sent: 1.98 GB and 118 MB. The test pool's shares had 100 IOPS at first, and the copy ran at about 3 MB/s per disk; at 3000 IOPS it ran at the speed limit. So the copy time depends on the share's IOPS as well as the speed limit.
4. **Change-only cycles, safe apply with undo files, and restore points, in direct NFS mode.** Parts: change lists in the copy piece; the undo-file apply and roll-back; the controller and a keep time per policy. This took over the safe-apply part of step 5, because direct NFS mode has no far-site receiver to do it.
5. **Receiver mode for VM disks, and failover changes.** The receiver does the same apply at the far site, and compares checksums for the compare pass. Failover rolls back an unfinished copy and can rewind to any restore point.
6. **Test across two regions,** with a failover and a boot of a Linux VM and a Windows VM. This includes the first test of Windows freeze and thaw. Then update `CROSS-REGION-DR.md`, `FEATURES.md` and the tutorial.
7. **Safe snapshots of running VMs,** using the copy from step 3.
8. **Backup to a snapshot pool:** the `BackupPolicy` resource, the chain of change files, hard-link snapshots, merging, and restore.
9. **Export to COS:** the setup command, Kopia upload, encryption, keep time, lock modes per policy, and restore from COS into another cluster. Then add a backup stage to the release test run.

About 5 PRs for replication after the lab test, and 3 more for backup.

## Tests in the release test run

Each part of this plan adds its checks to the unattended release test run, in the same PR as the code. The checks are listed in `2026-10-19-release-test-run.md`: stage 5 (snapshot of a running VM), stage 10 (VM replication) and stage 18 (backup). Each check looks at the result itself: the copy boots, the files match their checksums, and the bytes sent. It does not trust a status field.

The run needs one COS bucket for the backup stage. I will ask you to create it when backup is built.

## Cost and risk

- **VM slowdown during a copy.** Without a limit, a VM that writes nonstop was 16 to 19 times slower during a copy. The default limit of 20 MB/s per disk kept 93% of its speed. A slower copy makes a longer copy window, so this is a trade-off between RPO and VM speed. The policy has a setting for it.
- **Restore point space.** The far site keeps the old contents of every block that changed in the last 24 hours. A VM that rewrites much of its disk each day needs up to one more disk of space at the far site.
- **VM moves wait for the copy to cancel.** A move waits up to one controller cycle, 30 s by default. A full copy that is cancelled starts again from the beginning.
- **Staging space.** Each copy needs free space in the VM's volume for its staging file, until the Job has sent it: the used data for a full copy, the changed blocks for a change copy.
- **Red Hat does not support it.** An OpenShift Virtualization update can change the VM pod and break this method. Each release test run will include a VM replication stage, so we find a break before you do. If it breaks, VM volumes are skipped with an error. They are never copied unsafely.
- **Security.** The controller runs commands in VM pods, which needs `use` on the `kubevirt-controller` SCC (above). The staging files stay in the VM's own volume; the apply Job mounts only that volume's folder and refuses links.
- **Backup cost.** A snapshot pool is extra VPC file shares, paid for as normal shares. COS is paid for each GB stored, and only changed chunks are added after the first upload. A cost estimate goes into this spec once you choose the keep times.
- **New dependency.** Kopia, for the COS export only.

## Decisions I made for you

- Running VM disks are never copied with rsync. If the safe method fails, the volume is skipped and reported.
- Step 2 (stop unsafe copies) ships first, before the new copy method exists.
- A VM without a guest agent still replicates, as a crash-consistent copy. The status says so.
- Stopped VMs still use rsync, with a check that the VM stayed stopped.
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
- A backup policy selects whole VMs by label. A VM can be in only one backup policy.
- A setup command that you run creates the COS instance, the bucket and the driver's key. The driver never creates or deletes buckets.
- The driver's COS key can write to one bucket only, and cannot remove locks.
- Backups in COS are always encrypted. Key Protect protects the backup key with the bucket's root key, so the readable key is never stored.
- The setup command shows the readable key once, as an emergency copy for when Key Protect is down. You keep it outside IBM Cloud.
- Key Protect is required for COS export.
- Each policy sets a keep time, and either Compliance lock or no lock. A new policy defaults to Compliance for 30 days.
- Governance lock is not offered. Tested on 20 Oct: it cannot stop a stolen driver key from shortening the lock, while still letting the driver extend locks.
- The driver's COS key uses a custom role: read, write and extend locks. It cannot delete versions, skip locks or change the bucket.
- The restore command removes delete markers first, with your login.
- When a Linux or Windows guest cannot freeze, the copy goes ahead as crash consistent, and the status shows why.
- The copy uses a sparse staging file in the VM's own volume, at 20 MB/s per disk by default. Reading the disk over the network is not possible (tested 20 Oct).
- Each policy has its own area in the bucket, so storage is slightly higher than one shared area.
- Snapshots of running VMs are fixed to use the safe copy, as part of this work.
- Backup is built after replication, because it reuses the safe copy and the change list.
- Multi-disk VMs are copied at one moment as part of this work. Group snapshots (issue #14) stay a separate issue.
- The far site keeps restore points for 24 hours by default, set per policy. You chose 24 hours on 20 Oct.
- Safe apply and restore points use one method: undo files written at the far site before the disk changes. It works in direct NFS mode, which has nothing of ours at the far site. The cost there is about three network crossings per changed block; receiver mode will need one.
- Direct NFS mode has no checksum compare pass. After a VM move, the Job reads the far-site data once to compare, which costs about as much as sending it.
- A failed copy loses nothing: the change list of the last good copy is kept until the next copy reaches the far site.
- Step 4 builds this for direct NFS mode first, because that is the mode that copies VMs today. Receiver mode follows in step 5.
- Restore points are undo files: the disk always holds the latest copy, so failover to the latest copy stays fast. A rewind is slower, because it must apply undo files.
- A rewind makes a new disk and leaves the latest copy unchanged.
- Step 3 keeps no restore points. They start in step 5.
- The guest freeze uses KubeVirt's own freeze tool, which thaws the guest by itself after 30 s.
- When a VM must move, the controller cancels the copy. A move is more important than a copy.
- The default speed limit stays at 20 MB/s per disk, even though full copies are slow, because full copies are rare.
- The driver reads QEMU's staging file format itself, and refuses any part of the format that QEMU does not use for a copy.
- The copy Job runs as the QEMU user, so it can delete the staging file.
- The driver's controller account may use the `kubevirt-controller` security policy, as Kasten and IBM Backup and Recovery do. OpenShift requires it to run commands in VM pods. It adds no new power to start pods, because the controller's Jobs in `kube-system` already skip SCC checks.
