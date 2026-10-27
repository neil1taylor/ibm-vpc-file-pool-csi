# Spec: a COS export reads a disk that does not change while it is read

Date: 2026-10-27. Status: draft for review. It fixes issue #310. It builds on `2026-10-21-backup-cos-export.md` (the export) and `2026-10-21-backup-to-snapshot-pool.md` (the local backup).

## Goal

A restore from COS gives the disk exactly as it was at the moment the export says. For a VM with two disks, both disks come back from the same moment.

## What goes wrong today

The export Job reads the live backup folder of a volume, then tags the result with the moment the volume had when the Job started. A local backup can be applied to the same folder while the Job reads it. The export then holds a mix of two moments under one tag. The undo files (the moments folder) are read after the disk, so the disk and its undo files can disagree.

Evidence is in the issue: in release test run 1027-1558 the root disk export took 15 minutes. The next local backup of that disk landed in the middle. The restored VM had a data disk with 556 of the 729 files that the root disk had. The check failed. The earlier full run passed only because its exports ended before the next backup.

Any export that is slower than the backup schedule can be torn. Nobody sees it until a restore.

## The change

While the export of a volume runs, the local backup of **that volume** waits. When the export ends, the backup runs at its next turn.

1. When the controller starts an export Job for a volume, it marks the volume as held. When the Job ends (done, failed, or removed), it removes the mark.
2. The replication controller skips a held volume in its cycle, as it already skips a volume in use by a running VM. The skip is not an error. The status says "held: export to COS running".
3. Other volumes on the same share are not held. Only the volume being exported waits.
4. A held volume that waits longer than its schedule is counted by the existing stale-backup metric. Nothing new to read: the age of the last restore point grows, and the alert for an old backup already covers it.
5. If the controller restarts, the marks are rebuilt from the export Jobs that exist. A mark with no running Job is removed.
6. A hold never lasts longer than a limit (default 6 hours). When the limit passes, the hold is removed, the export Job is stopped, and the export is marked failed with a clear message. Then it is tried again by the existing retry rule. This stops a stuck Job from stopping backups for ever.

## What it costs

- Restore points of a volume get older by the length of its export. A 2 GB disk on the lab took 15 minutes. A large disk over a slow link takes longer.
- The export spec has no sentence that says exports never hold back backups, but it implies that the two run side by side. It is changed to say that one volume's backup waits while its export runs.
- The VM is not affected. The export still reads the snapshot pool, never the running VM.

## Why not the other two ways

- **Freeze a copy first.** Backups carry on, but it needs free space for one extra copy of the disk on the snapshot pool and one more read of the disk. Space on a full share is already the first thing that breaks in the lab. Not chosen. It stays open if the hold proves too slow for large disks.
- **Check afterwards and repeat.** Nothing else changes, but on a busy schedule an export may never finish. Not chosen.

## Tests

Red first, in this order:

1. A held volume is skipped by the replication cycle, and the other volumes of its share are copied.
2. The mark is set when an export Job starts and removed when the Job ends, fails, or is gone.
3. After a controller restart, the mark matches the Jobs that exist.
4. A hold past the limit is removed, the Job is stopped, and the volume shows an export error.
5. The status shows the hold as a message, not as an error, and a held volume does not fail the policy.
6. The metric for the age of the last backup keeps growing during a hold.

Then the lab proof: release test stage 18, full length, with the other cluster restore (`COS_DR_CLUSTER`). Both disks must come back from the same moment. A second run with an export made slower than the schedule on purpose (a short schedule) must also pass.

## Documents to update

`docs/specs/2026-10-21-backup-cos-export.md` (add the hold), `docs/BACKUP-RECOVERY.md`, `docs/CRD-SPEC.md` (if the status gains a field), `CHANGELOG.md`.

## Decisions I made for you

- Wait, not freeze and not repeat. It needs no extra space and it always gives a clean export.
- Only the volume being exported waits, not the whole share.
- The wait is not an error. A held backup is normal, and the old-backup alert is what tells you if it lasts too long.
- The hold has a 6 hour limit, so a stuck export cannot stop backups for ever.
- Restore points get older during an export. I accept this, because a restore point that is wrong is worse than one that is old.
- The export spec is changed to describe the hold. It made no promise against it, but the issue said it did; I checked and it did not.
- No new setting for people. The 6 hour limit is fixed in the first build.
