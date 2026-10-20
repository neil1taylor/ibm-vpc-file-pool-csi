# Spec: deletes, snapshots and restores act on the share data

Date: 2026-10-20. Status: in progress (fix for a data-safety bug found by the release test run).

## Problem

The controller has no NFS access. Its pool manager was built with no file operations, so four
operations skip their data step and still report success:

| Operation | What happens today | Consequence |
|---|---|---|
| Delete a volume | The volume's folder stays on the share | Pool shows space as free, but the share is still full of old data |
| Create a snapshot | No copy is made; the snapshot reports ready | A snapshot that holds nothing |
| Restore a snapshot | No copy is made; the volume is empty | A restore that "succeeds" with no data |
| Delete a snapshot | Nothing is removed | (no data to remove today) |

Even with file access, the old code could not work: it joined paths to one local folder and had no
idea which share a path belongs to.

## Design

All four data steps run as short Kubernetes Jobs, the same way volume clones already copy data.

- A **share Job** mounts one share root (or two, for a restore onto a different share) through a
  temporary PersistentVolume and claim, and runs one shell step: remove a folder, or copy a folder.
- Share Jobs run in the **driver's own namespace** (from the pod's namespace, default `kube-system`),
  as root. Measured on rovslab on 2026-10-20: root in such a pod can read, copy (owners kept) and
  delete another user's private files on a VPC file share (`sec=sys`).
- Each Job is named from the operation and the volume or snapshot name, so a retry finds the same
  Job. On success the Job, claim and volume are removed. On failure they are removed and the
  operation reports an error; the next retry starts a new Job.
- A failed copy removes its partial folder before it ends.
- A delete first stops a copy that is still writing into the same folder (an unfinished restore or
  snapshot), and answers "in progress" until that copy has stopped.
- Every path is checked against the volume or snapshot path rules before it goes into a step.

### Per operation

- **Delete a volume:** the first call starts the remove Job and answers "in progress". The CSI
  provisioner retries. When the Job has succeeded, the volume record is deleted and the space is
  given back to the pool. The space comes back only after the data is gone.
- **Create a snapshot:** the snapshot record is written first as not ready, and the copy Job starts.
  The CSI snapshotter calls again until the snapshot is ready. When the Job has succeeded, the record
  is marked ready. If the Job fails, the record is deleted and its space given back, so the next call
  starts again. A short API error changes nothing.
- **Restore a snapshot:** the new volume's record is written first, with phase `Creating`, so the
  chosen share keeps its reservation across retries. The copy Job copies the snapshot folder into the
  new volume folder, then sets owner and permissions. Until it succeeds, the call answers
  "in progress" and the claim stays Pending. No pod can mount a half-restored volume.
- **Delete a snapshot:** like deleting a volume.

"In progress" is gRPC `Aborted`. The CSI sidecars treat it as "not final" and retry. If a claim is
deleted while its restore runs, the provisioner finishes the create and then deletes the volume.

## Proof

- Unit tests for the Job runner and for each manager operation (Job not started, running, succeeded,
  failed).
- Release test stage 3 (delete removes the folder) passes on rovslab.
- New release test stage 5: write data, snapshot, restore to a new claim, compare checksums, delete
  the snapshot and check its folder is gone.

## Not in this change

- Volume clones still run their Job in the claim's namespace. That needs the `anyuid` permission in
  every user namespace. Moving clones to share Jobs is the next change.
- Group snapshots still do not work on ROVS (issue #14). This change only stops the group code from
  reporting a group as complete before its copies finish: the group stays `InProgress`, and later
  calls check the copies again.

## Decisions I made for you

- Data steps run as Jobs, not inside the controller. Giving the controller NFS mounts would make it a
  privileged pod and would block CSI calls for minutes on large copies.
- Jobs run in the driver's namespace, not the user's, so no user namespace needs extra permissions.
- A deleted volume's space is freed only after its data is removed, not before.
- A restore keeps the claim Pending until the copy is done, instead of binding at once and blocking the
  mount. This needs no new fields and no change to the node agent.
- A failed Job is removed and retried on the next call, without a retry limit. The CSI sidecars already
  slow their retries down over time.
- The old in-process copy and remove code paths for these four operations are removed, with their
  unit tests. They never ran in a real controller. New tests cover the Job-based behaviour.
- The Job uses the same image as clone Jobs (`quay.io/centos/centos:stream9`).
