# Spec: backup to a snapshot pool

Date: 2026-10-21. Status: draft for review. This is step 8 of `2026-10-20-vm-safe-replication.md`. Read that spec first. Step 9 (export to COS) is a separate spec: `2026-10-21-backup-cos-export.md`.

## Goal

When this step is done:
- You create one `BackupPolicy`. On its schedule, the driver backs up the VMs and PVCs you selected, into a pool that holds only backups.
- A lost or full data share does not take the backups with it, because the backup pool uses its own VPC file shares.
- You can restore any backup moment of a volume to a new PVC, in the same cluster, with one command.
- A VM with several disks is backed up at one moment, and a restored VM boots.
- Backups older than the keep time go away by themselves.

## A change from the main spec

The main spec said: keep a VM disk as a chain, one full copy and then one change file per backup, and merge the oldest into the next one.

This spec keeps the **latest** backup as a whole disk, and keeps **older** moments as undo files. This is what replication already does at the far site (step 4). Reasons:
- Restoring the latest backup, the most common case, is a plain copy. In a chain, it must apply every change file.
- Dropping an old backup is deleting its undo file. In a chain, it is a merge that rewrites data.
- The code exists and is tested on two clusters: `ApplyCopy`, `Rewind`, and the prune of old undo files. A chain would be new code.

So a VM backup is a replication to a pool in the same cluster, with a longer keep time. The rest of this spec is what is new on top of that.

## What exists today

- A `ReplicationPolicy` can send a pool's volumes to a destination pool by NFS (direct mode). It copies running VMs safely, at one moment for all disks of a VM, and sends only changed blocks after the first copy.
- The destination keeps restore points (undo files) for `restorePointKeepHours`, and `kubectl failover execute --rewind-to` makes a disk as it was at a moment.
- Replication selects volumes with a label selector, and skips nothing silently.
- Snapshots (steps 1 to 7) live on the same share as the volume.

## Design

### The BackupPolicy resource

Cluster-scoped. Fields:
- `snapshotPool`: the pool that holds the backups. It must be a pool that no workload claim uses. The webhook refuses a policy that names a pool with claims, and refuses a data pool that also is a backup pool.
- `schedule`: an interval such as `6h`, like replication.
- `selector`: a label selector for PVCs. A VM is selected through its disks: if any disk matches, the whole VM is backed up. Namespaces can be limited with `namespaces`.
- `keepFor`: how long backups are kept, such as `30d`. The default is 7 days.
- `vmCopySpeedLimitMBps`: the same limit as for replication. Default 20.
- `suspend`: stops new backups. Existing backups stay.

The status lists each backed-up volume with its last backup moment, its oldest backup moment, the bytes written, the consistency, and the last error.

One volume belongs to at most one backup policy. The webhook refuses a second policy that selects the same labels in an overlapping namespace. (A volume can be both backed up and replicated; these are different policies.)

### How a backup runs

The controller makes and owns a `ReplicationPolicy` for each `BackupPolicy`. It points at the snapshot pool, uses the same schedule and selector, and sets `restorePointKeepHours` from `keepFor`. Replication does the copy: QEMU for running VM disks, at one moment for all disks of a VM, and changed blocks only after the first copy. The `BackupPolicy` status is read from the owned policy.

The owned policy is marked as owned. The webhook refuses edits by hand, and deleting the `BackupPolicy` deletes it. Deleting the policy does not delete the backups; a separate command does that (below).

### Volumes that are not VM disks

Replication keeps only the latest files of such a volume. Backups of them need old moments too. For these volumes the backup uses hard links:
- Each backup moment is a folder in the snapshot pool. A backup copies from the previous moment's folder with `rsync --link-dest`. A file that did not change takes no extra space.
- A backup folder is consistent only as much as the files are: it is a file copy of a live volume, as snapshots of pods are today. The policy status says `FileCopy`, not `FileSystemConsistent`. For a database, use a hook (the hooks of replication work on backup policies too).
- Folders older than `keepFor` are removed, oldest first.

### Restore

A new command, `kubectl backup`:
- `kubectl backup list --policy NAME` shows each volume and its backup moments.
- `kubectl backup restore --policy NAME --pvc NS/NAME --moment TIME --to-pvc NAME [--namespace NS]` makes a new PVC in the cluster's data pool with the contents at that moment. `--moment` takes the nearest earlier backup. Without it, the latest backup is used.
- For a VM, `--vm NS/NAME` restores all disks of the VM at one moment, to new PVCs with a suffix. The command prints the VM's disk names. It does not create the VM.

A restore copies the backup (after a rewind if the moment is not the latest) into a new volume with a Job, as restore from a snapshot does today. The new PVC stays Pending until the copy has finished.

### Keep time and clean-up

- Undo files and hard-link folders older than `keepFor` are removed on each cycle.
- If the snapshot pool is more than 85% full, the policy stops new backups and reports the error. It does not delete backups early. (A full pool is an alert; losing backups to make room would hide the problem.)
- `kubectl backup delete --policy NAME --pvc NS/NAME` removes the backups of a volume, after you confirm. It is never run by the controller.

### Status and metrics

New metrics: backups made, bytes written, last backup age per policy, and oldest backup age per policy. Alert rules go into `MONITORING.md`: no backup within twice the schedule, and the pool above 85%.

### What this step does not do

- No COS export, no encryption, no locks. That is step 9.
- No restore into another cluster. That is step 9.
- No console screens. They come after the command works.
- No restore of a whole VM definition. You restore disks and attach them to a VM.

### Changes to code and docs

- New: `BackupPolicy` CRD and webhook, a controller that owns `ReplicationPolicy` objects, the hard-link backup Job, the `kubectl backup` plugin (like `kubectl failover`), the 85% check.
- `api/v1alpha1`: run `make generate`. Update `CRD-SPEC.md`.
- Docs: `FEATURES.md`, `BACKUP-RECOVERY.md`, `MONITORING.md`, `HELM-VALUES.md` (RBAC), `CHANGELOG.md`.

## More than one share in the snapshot pool (issue #71)

A snapshot pool may have several shares. Each volume is backed up to one share, chosen when the volume is first backed up and kept for good, because its backups and its change lists are on that share.

- **Where a new volume goes:** the ready share below 85% that holds the fewest volumes (counted over all backup policies on the pool). Ties go to the first share in the pool's list. If no share qualifies, the volume waits and its error says so.
- **Where it is written down:** `shareID` in the volume's entry in the policy status. A restore and `kubectl backup delete` read it. An entry without one was made when the pool had one share; it belongs to the first share.
- **How it is copied:** one replication policy for each source pool and share. The SubVolumes carry a `storage.ibmcloud.io/backup-share` label that the replication policy selects on. The policy for the first share keeps its old name, so an upgrade keeps the change lists of running VM copies.
- **The 85% rule:** read for each share. Only the copies to a full share are paused. New volumes avoid it.
- **Not done:** backups are never moved between shares, and a share that holds backups must not be removed from the pool.

## Tests

- Unit tests with fakes: the owned policy is made from the backup policy and follows its changes; the webhook refuses overlap and a pool with claims; hard-link folders and their clean-up; the 85% rule; `restore` picks the nearest earlier moment.
- Release test stage 18: back up a running VM with two disks and a pod volume. Make a second backup. Restore the VM disks at the first moment and the pod volume at the second. The restored VM boots, every logged file matches its checksum, and the restored pod volume matches its checksums. Delete the data share's volume and restore it from the backup.

## Cost and risk

- **Space.** The snapshot pool needs about one copy of the selected data, plus the changes in `keepFor`. A VM that rewrites half its disk each day for 30 days needs up to 15 extra disks of undo files. The first policy shows the numbers in its status so you can size the pool.
- **Extra VPC shares.** The snapshot pool is normal VPC file shares, paid for as normal shares.
- **Same cluster.** These backups are lost if the VPC itself is lost. Step 9 (COS) and replication cover that.
- **Replication and backup of one VM share the VM.** Copies take turns, so each waits for the other. A VM under both policies is copied about twice per cycle.
- **File copies are not application-consistent.** Only VM disks get the freeze. Use hooks for databases.

## Decisions I made for you

- Backups keep the latest copy whole and older moments as undo files, the same as replication. This replaces the "chain of change files" in the main spec.
- A `BackupPolicy` is a thin layer: it makes and owns a `ReplicationPolicy` to the snapshot pool.
- Default keep time is 7 days. Default speed limit is 20 MB/s.
- A VM is backed up whole if any of its disks matches the selector.
- One volume belongs to one backup policy.
- Non-VM volumes use `rsync --link-dest` folders and are reported as file copies.
- A snapshot pool above 85% stops new backups. It never deletes backups to make room.
- Deleting a policy keeps the backups. Removing them is a separate, confirmed command.
- Restore is a command first (`kubectl backup`). The console comes later.
- Restore makes new PVCs. It never writes over an existing volume.
- A snapshot pool can have more than one share. A volume stays on the share it was first backed up to, so adding a share never moves backups (issue #71).
- A new volume goes to the ready share with room that holds the fewest volumes. If none has room, it waits.
- The 85% limit is for each share, not for the pool.
