# Spec: snapshots of a whole pool (share-level snapshots)

Date: 2026-10-22. Status: draft for review. It builds on `2026-10-21-backup-to-snapshot-pool.md`, `2026-10-21-backup-cos-export.md`, `2026-10-22-backup-file-restore.md` and `2026-10-21-instant-vm-snapshots.md`. Read the backup spec first. Facts about the driver come from the code on `main` on 22 Oct 2026. Facts about IBM Cloud come from the IBM docs and carry a link. Where the docs are unclear, this spec says "to verify in the lab" and puts the check in phase 0.

## Goal

The owner's question was: is there a use case for a snapshot of the pool or share?

Yes. Today the driver uses no IBM share snapshot. It keeps snapshots per volume, as file copies or QEMU copies (see `docs/CONTEXT.md`). An earlier decision kept it that way because a share holds many tenants' volumes, and a share snapshot cannot give back one volume without touching the others (`docs/CONSISTENCY-MODEL.md`). That reason is still true for a restore **in place**. It does not stop us using share snapshots in other ways.

The goal of this spec: **an administrator can protect and recover a whole pool quickly, in minutes whatever its size. The administrator and the tenants can also recover single volumes or VM disks from the same pool-level moment, without touching other tenants.**

Two real uses:
1. **Fast recovery of a whole pool** after ransomware, a bad mass change, or a script that deleted many folders. One share snapshot covers every volume on the share, and it is ready in seconds.
2. **A steady, same-moment source** for backups and replication. They read a frozen view of the share, not live files that change while they copy.

## What IBM Cloud gives us (facts, with sources)

All links are on cloud.ibm.com. "Gen 1" means the `dp2` profile that this driver uses.

| Fact | Source |
|---|---|
| A snapshot is taken with the VPC API (`POST /shares/{id}/snapshots`), the CLI, the console or Terraform. It is stable "within seconds". Only one snapshot a minute. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-create |
| The first snapshot copies all the share's contents. Later snapshots hold only the changes. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-about |
| Up to 750 snapshots per `dp2` share. Together they cannot pass 100 TB per share. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-about and https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-planning |
| **Shares in access mode `vpc` cannot have snapshots. Shares in access mode `security_group` can.** The driver makes only `security_group` shares (`pkg/ibmcloud/vpc_client.go`: `AccessControlMode = "security_group"`). | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-about (Limitations) |
| Snapshots live in the same zone and on the same share. They cannot be copied to another zone or region. **When the share is deleted, its snapshots are deleted.** | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-about |
| You cannot take a snapshot of a share in a degraded state, or of a replica share. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-about |
| A replica share (zonal replication) receives the source's snapshots at the next sync. Snapshots on a replica cannot be deleted by hand. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-create |
| **Restore is "make a new share from the snapshot". There is no restore in place.** The new share has the same profile, zone and encryption. It can be bigger, and you can choose its IOPS. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-restore |
| The new share starts with pointers to the old data. It is in state `pending` while IBM copies the data. **You can mount it and read and write at once while it is pending.** In `pending` you cannot make a replica or a snapshot of it. You cannot delete the source snapshot until the copy ends. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-restore |
| Single files: the share shows a hidden read-only folder `/.snapshot` at the top of the mount. It has one subfolder for each snapshot, named by the snapshot's fingerprint. You can read it with normal commands. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-restore ("Restoring files from a snapshot") |
| Cost: you pay for the storage in GB-hours. A snapshot kept for part of a month is charged for those hours. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-about |
| Schedules: IBM "Backup for VPC" runs a plan on shares that carry a matching user tag. Up to 10 policies per region, 4 plans per policy. Retention by days (1 to 1000) or by count. It needs a service-to-service authorization for file storage. | https://cloud.ibm.com/docs/vpc?topic=vpc-backup-service-about |
| The size of snapshots is reported within 15 minutes. Activity Tracker records create, delete and change of snapshots. | https://cloud.ibm.com/docs/vpc?topic=vpc-fs-snapshots-about |
| IBM says a backup or snapshot restore is a manual step that takes time and is not a disaster-recovery product. | https://cloud.ibm.com/docs/vpc?topic=vpc-backup-service-about |
| The Go SDK in `go.mod` (`vpc-go-sdk` v0.80.0) has `CreateShareSnapshot`, `GetShareSnapshot`, `ListShareSnapshots`, `UpdateShareSnapshot`, `DeleteShareSnapshot` and a pager. A share is made from a snapshot with the `source_snapshot` field of the share prototype. The driver's own client has none of these yet. | `vpcv1/vpc_v1.go` in the module cache; `pkg/ibmcloud/client.go` |

### To verify in the lab (phase 0)

The docs are silent or unclear on these. Each one decides part of the design below.

1. **When is the moment captured?** At the API call, or when the snapshot becomes `stable`? The API has a `captured_at` field. If the moment is the call time, snapshots of all shares can be called in parallel and fit within a second or two.
2. **Does the one-a-minute limit apply per share or per account?** The design assumes per share.
3. **What does a snapshot of a share with open NFS writers contain?** Expected: crash consistent (like a power loss). Prove it with a running VM.
4. **Is `/.snapshot` visible on our mounts?** Our mount is `sec=sys`, NFS version 4.x, through the share's mount target in `security_group` mode, and the node mounts the share root. Check that `ls /mnt/share/.snapshot/` works, that a read of a file in it is correct, and that it works from a Job pod and from a node mount.
5. **How long does a new share from a snapshot take** to be mountable, for 1 TB and 10 TB shares with a lot of data? And how fast is I/O during `pending`? IBM does not give figures. This is the number that decides "minutes, any size".
6. **How long does `pending` last**, and do we know when it ends? (Share state `stable`.) This sets how long a restored share is pinned (see "Limits").
7. **Does a snapshot change the IOPS or latency of the share** while it is taken, while it holds a lot of changes, and while a restore from it is pending?
8. **Does a snapshot use the share's free space** (the driver's capacity numbers) or is it billed and counted apart? Check `df` before and after. The price per GB-hour for `dp2` snapshots is not in the pages above; read it from the catalog (`ibmcloud-global-catalog-pricing`) before we promise a cost.
9. **What IAM role does the driver's API key need** to create, list, delete snapshots and to make a share from a snapshot? Check the key from `API-KEY-SETUP.md`.
10. **Two pages disagree** on what happens to a share backup when its share is deleted. One says the backups are deleted. Another says backups "remain available". Check with the share's own snapshots too. The design assumes the safe reading: **they are deleted with the share.**
11. **Does a share made from a snapshot keep our folder layout and file owners?** Expected yes. Check that a KubeVirt disk file boots from it, and that the mount target we create (in `security_group` mode) works on it with `sec=sys`.
12. **Does the VPC replication of a share carry its snapshots to a replica in another zone?** (This is how a pool snapshot could survive the loss of a zone. The driver does not use VPC replication today. This check only sizes an option.)

## What exists today (from the code)

- A pool is one or more `dp2` shares in one zone. A volume is a folder `/pvcs/pvc-<uuid>` on one share. The `SubVolume` records the share (`shareID`, `shareMountTargetIP`, `shareExportPath`), the folder and the claim.
- A PV carries the share's address in its `volumeAttributes` (`server`, `share`, `subDir`). **A PV's attributes cannot be changed after it is made.** The node plugin reads them in `NodeStageVolume`. It already reads the `SubVolume` for the clone gate.
- A pool cannot adopt a share that already exists. Shares are made by the pool manager only.
- The controller has no NFS access. Everything that touches files runs as a Job that mounts the share (`pkg/pool/share_job*.go`).
- Freezing a running VM's file systems through the guest agent already exists (`pkg/vmsnap`, `pkg/vmcopy`). The guest thaws by itself after 30 seconds at most.
- A backup of a plain volume is a hard-link folder made by rsync from the live folder. A backup of a running VM is a QEMU copy that sends only changed blocks.
- `BackupRestore` and the browse and file-restore design already copy a backup into a new claim, with the access rules by the namespace of the claim.
- Metrics, alert rules, a console plugin and a release-test harness exist.

## Design that meets the goal

Four parts: a **pool snapshot** (one moment for all shares), **pool snapshot policies** (schedule and keep time), **volume recovery** from a pool snapshot (no effect on others), and **pool recovery** (new shares, then a controlled switch).

### 1. Taking a pool snapshot

A new cluster resource, `PoolSnapshot`. It names a pool, and its status lists one VPC share snapshot per share of the pool. The controller makes it, on a schedule or on request.

Steps, in order:

1. **Write the manifest.** A short Job on each share writes `/.pool-manifest.json` at the share root: the list of volumes on that share with their claim (namespace and name), size, folder, owner and mode, VM chain files, and clone state, and the pool's name. The snapshot then carries it. This is what lets us recover a volume whose `SubVolume` is gone, and tell the administrator what a restore will lose. (The `PoolSnapshot` status holds only the counts and a hash, so it stays small with thousands of volumes.)
2. **Freeze (optional, on by default for VMs).** For each running VM that has a disk on the pool, ask the guest agent to freeze, as the VM snapshot code does now. The freeze runs for one short window. The window is bounded: the controller thaws everything when all share snapshots have been **called**, or after a time limit (default 20 seconds, never above the guest's own 30), whichever comes first. The status records, for each VM, `FileSystemConsistent` or `CrashConsistent` and why. A VM with no guest agent is taken as crash consistent. A plain volume is a file copy, as today.
3. **Call the snapshots in parallel**, one per share, tagged `pool-csi:<pool>:<snapshot name>`. The calls go through the driver's client with its rate limit. With 10 shares this needs about two seconds.
4. **Thaw.**
5. **Wait for each snapshot to be `stable`.** Record its fingerprint and its `captured_at` time.

**Same moment across shares.** One share snapshot is one moment for its share. Several shares are called within a second or two of each other, not at the same instant. IBM gives no way to snapshot several shares as one unit. The effect:
- A volume is entirely on one share. For every volume, and every VM disk, the snapshot is one moment. This is the case that matters.
- A VM with disks on two shares could see its disks about one or two seconds apart. With the freeze on, the guest wrote nothing between the two calls, so the disks agree. Without a freeze, the guest sees a crash between its disks, which a file system or database normally repairs.
- The status records the spread (latest `captured_at` minus earliest) and the list of VMs that have disks on more than one share. A spread above a limit (default 10 seconds) makes the snapshot `Degraded`, and the policy takes another.
- Phase 0 check 1 may show that the spread is zero. Then this section simplifies.

A VM with a chain of instant snapshots is safe: the chain files are plain files on the share, so the snapshot has them all.

**Failures.** If one share cannot be snapshotted (degraded, rate limit, API error), the controller deletes the snapshots it made for this pool snapshot and retries, up to a limit. It never leaves a half pool snapshot marked ready. The pool snapshot states are `Pending`, `Ready`, `Degraded` (taken, but spread too large or a freeze failed) and `Failed`. Only `Ready` is offered for a pool recovery. `Degraded` is still usable for single volume recovery.

**Share state.** A share that is itself `pending` after a restore cannot be snapshotted (see "Limits"). The policy skips it and raises an alert.

### 2. Schedules and keep time

A new cluster resource, `PoolSnapshotPolicy`: `pool`, `schedule` (an interval such as `1h`, minimum 15 minutes), `keepFor` (default `3d`) and `keepLast` (default 6, the minimum number to keep whatever their age), `freeze` (`VMs` by default, or `None`), and `suspend`.

**The driver runs the schedule. We do not use IBM Backup for VPC for the pool snapshots.** Reasons, in plain words:
- IBM's plans run per share and know nothing of the others. They cannot give one moment across a pool, cannot freeze guests, and cannot write the manifest.
- They need a service-to-service authorization and an account-level policy (10 per region). That is more setup, and a failure there is silent.
- The driver still marks its snapshots with a user tag, so an administrator sees them in the IBM console.

Removal of old snapshots: the controller deletes the share snapshots of a pool snapshot that is older than `keepFor`, keeping at least `keepLast`. It never deletes a pool snapshot that a restore uses. The limits that bound the plan: 750 snapshots per share and 100 TB of snapshot data per share, and one snapshot a minute. At an hourly schedule with 3 days kept, a share holds about 72. The webhook refuses a policy whose `keepFor / schedule` is above 500 (the margin leaves room for manual snapshots).

**Space and cost.** A snapshot holds only the changes since the one before it. A pool where VMs rewrite a lot (disk images change all the time) grows faster than a pool of documents. The first policy shows the snapshot size in its status (read from the VPC, which reports it within 15 minutes) so the administrator sees the growth. The price is per GB-hour, to verify (check 8).

### 3. Recovering one volume or VM disk from a pool snapshot

This is the case that matters most, and it needs **no new share and no wait**.

The pool snapshot shows each old moment of the share as a read-only folder: `/.snapshot/<fingerprint>/`. A volume's folder at that moment is `/.snapshot/<fingerprint>/pvcs/pvc-<uuid>`. The existing restore Job already copies a folder into a new claim. It gets one new source: this path. So:

- `kubectl backup list --pool-snapshots` shows the moments of a claim. `kubectl backup restore --from-pool-snapshot <name> --pvc NS/NAME --to-pvc NEW` makes a **new** PVC with the contents at that moment. It never writes over an existing volume. The new PVC is held back from pods until the copy ends, like a restore today.
- For a VM, `--vm NS/NAME` restores all its disks at the one moment. It prints the disk names; the user attaches them to a VM. (Same as for backups.)
- The manifest tells us the claim's original size, owner and mode, and the chain files, so a **deleted** volume can be restored too. The new volume's `SubVolume` gets the chain list from the manifest.
- The copy time grows with the size of the volume. A 500 GB disk takes minutes, not seconds. Phase 0 measures it. A later phase may offer an instant variant (below).
- **Other tenants are not touched.** The Job reads one folder, read-only, and writes only into the new volume. The tenants' live folders are never written.

**Files in the browse feature.** The browse and file-restore design (`2026-10-22-backup-file-restore.md`) works on a path. A pool snapshot is one more source for it: the same helper pod, with the read-only mount of one `/.snapshot/<fingerprint>/pvcs/<volume>` folder. File-level restore from a pool snapshot then comes at no new design cost. It is a later phase.

**Instant variant (later, optional).** Make a new share from the snapshot, and point a PV at the volume's folder on it. A VM boots at once while IBM copies the data. This is VMware's "instant recovery". It adds a tenant volume on a recovery share, and later needs a move into the live pool. It is not needed to meet the goal, so it comes last.

### 4. Recovering the whole pool

IBM can only restore into **new shares**. So a pool recovery has two stages. The first is safe and changes nothing. The second is the switch, and the administrator approves it.

A new cluster resource, `PoolRestore`: `pool`, `poolSnapshot`, and `mode`.

**Stage 1: side by side (changes nothing).**
- For each share, make a new share from its snapshot: same profile, zone and encryption, the same size, the same IOPS, a mount target in the same subnet and security group as the original (the driver's own function makes it). The shares are made in parallel, so the time is that of one share (30 to 90 seconds for the API, plus the pointer setup; phase 0 check 5 gives the real figure). The size of the data does not matter, because the new share starts with pointers.
- The status shows, for each share, the new share, its state, and whether IBM's copy has ended.
- The manifests are read (by a Job on each new share) and compared with the live `SubVolume` list. The status reports three lists: **volumes in both** (will be back as they were), **volumes created after the snapshot** (their data is not in the snapshot; they will be lost by a switch), and **folders in the snapshot with no volume now** (deleted since; they can be restored one by one).
- At this point the administrator can already recover single volumes from the new shares if they want, or stop and delete them.

**Stage 2: the switch (the administrator approves it).** A switch is a planned outage of the pool. It needs the administrator to say `approve: true` on the `PoolRestore`, after reading the three lists.
1. **Quiesce.** The pool refuses new volumes (retriable error, as when a pool is expanding). The controller lists the pods that use claims of this pool. It does **not** stop other people's workloads. The administrator stops them, or sets `force: true`, which accepts that those pods lose their mount. The switch waits until none remain (or `force`).
2. **Switch each share.** The pool's share list and every matching `SubVolume` get the address of the new share (`shareID`, mount target, export path). The old shares leave the pool and become `retired`. They are **not deleted**. Their snapshots stay too (the pool snapshot stays pinned, see "Limits").
3. **Mark the lost volumes.** A `SubVolume` for a volume created after the snapshot has no folder on the new share. It is marked `LostByRestore`. The node plugin refuses to mount it (the same way the clone gate does, with a clear message). It must never give a pod a silent empty folder. The administrator decides: delete it, or restore it from a backup.
4. **Node plugin reads the address from the `SubVolume`.** This is a change to `NodeStageVolume`: the PV's own attributes are old after a switch and cannot be edited, so the node plugin uses the `SubVolume`'s current share address when it differs. Without this change a switch would need every PV to be deleted and made again.
5. **Unquiesce.** Pods can start. Their claims mount the new shares.
6. **Old shares.** Kept for `retireFor` (default 7 days), then listed for deletion. Deletion is a separate confirmed command, never automatic. This is the rollback: before then, a second `PoolRestore` can switch back. (The old shares still hold the data from after the snapshot, including what the ransomware did. The administrator may need them for a forensic look.)

**Other modes.**
- `mode: Copy`: leave the live pool as it is, and copy chosen volumes out of the new shares. This is the same as stage 1 plus single volume recovery. It does not need an outage, but it takes time in proportion to the data. It does **not** meet "minutes, any size" for a whole pool.
- There is no restore in place. IBM does not offer it.

**The honest gap: the first hours after a switch.** While IBM copies the data into a restored share, that share is `pending`. We cannot snapshot it or replicate it, and the snapshot we restored from cannot be deleted. So **for a period after a pool recovery there is no pool snapshot of the new shares.** The length depends on the data and is unknown until check 6. During it, the driver's backups to the snapshot pool continue, and the administrator is told in the pool status and by an alert. If the lab shows this is many hours for a large pool, it is the main weakness of this design, and the answer is to keep the backups running and to not restore a pool to a share that is ransomware-affected twice in a row.

## Tenancy and access (RBAC)

- **Pool snapshots are an administrator thing.** `PoolSnapshot`, `PoolSnapshotPolicy` and `PoolRestore` are cluster resources. Only cluster administrators (the chart's admin ClusterRole) can see or change them. Tenants never see the pool, the shares, or other tenants' volumes.
- **A tenant can recover their own volumes** with the same rule as backups: the right is by the **namespace of the claim** that owned the data. The gateway from the file-restore spec (or the plain `BackupRestore` path) checks it as the user. The rights-only name is `backupfiles`, as there. A new pair of verbs is not needed: reading a pool snapshot's copy of a volume of namespace A needs `get backupfiles` in A, and making the new PVC needs the usual rights in the target namespace. A person without the right gets "forbidden", and cannot tell whether the volume exists.
- **A deleted namespace.** The manifest records the namespace of each volume. If the namespace is gone, only an administrator can restore it.
- **A pool snapshot never gives a tenant a view of the share.** The restore Job mounts one volume's folder only, read-only, and the tenant sees the result only as a new PVC. The Job runs in the driver namespace with the same safety rules as the other jobs: paths checked against the folder pattern, no links followed, nothing written outside the new volume.
- **Ransomware thinking.** An attacker with the cluster admin role, or with the VPC API key, can delete a share and its snapshots with it. Pool snapshots are not a lock. For that, use the COS export with a Compliance lock (step 9). Say so in the docs.

## How it works with backups and replication

- **Backup from a share snapshot (the steady source).** A backup of a plain volume reads from `/.snapshot/<fingerprint>/pvcs/<volume>` of the newest `Ready` pool snapshot, instead of the live folder. A read-only, frozen source gives a true same-moment copy of every plain volume of the pool, which the live rsync cannot. The backup's `consistency` for these volumes changes from `FileCopy` to `CrashConsistent` (the state of the share at one moment). The backup policy gets a `source` field: `Live` (today, the default) or `PoolSnapshot`. It needs a `PoolSnapshotPolicy` on the pool, and the backup waits for a snapshot that is newer than its last run, or asks for one.
- **VM disks keep the QEMU path.** A running VM's backup is a QEMU copy that sends only changed blocks, and it is file-system consistent with the freeze. Reading a whole disk file from a snapshot every time would read all its blocks, because a file snapshot has no change list. So VM disks do **not** move to the snapshot source. A stopped VM's disk can use it.
- **Replication.** A replication of a plain volume (rsync) can read from the snapshot too (`source: PoolSnapshot` on the policy), for the same reason. VM replication does not change.
- **A pool snapshot is not a backup.** It is on the same share and zone. It dies with the share and the zone. The documents will say this in the first paragraph, and the console will show "same share" on it.
- **Snapshot of the backup pool.** A `PoolSnapshotPolicy` can name a snapshot pool too. That protects backups from a mistaken delete of folders (not from a deleted share).
- **Zone loss.** IBM copies snapshots to a zonal replica share. If phase 0 check 12 passes, a later phase may offer an option to make a replica of each share. This is a separate feature (cost: a second set of shares) and is not in the build plan.

## Limits and rules

- 750 snapshots per share, 100 TB per share, one a minute. The policy checks these and refuses a plan that cannot fit.
- A share in `pending` after a restore cannot be snapshotted, and the snapshot it came from cannot be deleted until the copy ends. The controller pins that pool snapshot and does not remove it.
- The first snapshot of a share copies all its contents. Phase 0 checks how long the first one takes and what it does to the IOPS of a busy share.
- A pool snapshot of a pool that is expanding or draining includes only the shares that exist and are `stable` at that moment. The status lists them. A share added later is not in it. A restore of that pool snapshot does not touch the later share.
- A share that is `failed` or `degraded` cannot be snapshotted. The pool snapshot becomes `Failed` and the policy retries.
- All share snapshots of one pool snapshot have the same keep time.
- A pool snapshot is useless once a share is deleted (its snapshots go with it). The controller refuses to remove a share from the pool or delete it while a `Ready` pool snapshot depends on it, unless the administrator uses `force`.
- No tiers special case: all shares in all tiers of a pool are in the pool snapshot.

## Monitoring and alerts

New metrics (listed in `MONITORING.md` when they are built):
- `pool_snapshot_total` (by pool and result), `pool_snapshot_duration_seconds`, `pool_snapshot_spread_seconds`
- `pool_snapshot_age_seconds` (newest `Ready` for each pool), `pool_snapshot_count` (by pool)
- `pool_snapshot_share_bytes` (snapshot size, by pool and share)
- `pool_snapshot_freeze_failures_total`
- `pool_restore_phase` (by pool), `pool_restore_share_pending` (shares still copying)

New alert rules, in the chart with rule tests:
- no `Ready` pool snapshot within twice the schedule
- a pool snapshot `Degraded` or `Failed` three times in a row
- a share with more than 600 snapshots (limit 750)
- snapshot data above 50 percent of the share size
- a restored share `pending` for more than 24 hours
- a pool with a policy but no snapshot after a restore (the gap above)

## Console and command line

- Command line: `kubectl pool-snapshots` already exists for VM flatten. Add `kubectl pool-snapshots take|list|delete`, and `kubectl pool-restore plan|start|approve|status|retire`. `kubectl backup restore --from-pool-snapshot` is for volumes (tenant-safe, goes through the access checks).
- Console (administrator): a Pool snapshots page for each pool: the list with state, time, spread and size; "Take now"; the policy form; a Restore wizard that shows the three lists and the quiesce list before the approve button. A tenant sees their volume's restore points, under the existing volume page, once the file-restore console work lands.
- Every dangerous step (approve a switch, `force`, delete retired shares) asks the user to type the pool name.

## Tests

- **Unit (fakes).** The fake VPC client gets share snapshots and share-from-snapshot. Tests: all-or-nothing creation (one share fails, the others are deleted); the spread rule; the freeze window ends at the time limit; the manifest has every volume; retention keeps `keepLast` and never removes a pinned snapshot; the policy limits; the webhook refusals; the three lists of a `PoolRestore`; a switch updates every `SubVolume` and marks the lost ones; the node plugin uses the `SubVolume` address and refuses `LostByRestore`; `from-pool-snapshot` path checks (no `..`, no links); access checks per namespace.
- **Chart tests.** Roles, the new CRDs, the alert rules (with `promtool` rule tests), the new values.
- **Release test stage on the lab (rovslab).** Make a pool of two shares with a plain volume and a two-disk VM (one disk on each share if possible). Write known files with checksums. Take a pool snapshot with the freeze. Then: (1) delete a volume, restore it from the snapshot, check the checksums; (2) restore the VM's disks at the one moment and boot the VM; (3) run `PoolRestore` stage 1 and check the three lists; (4) switch (with the VM stopped) and check that the pods mount the new shares, a volume created after the snapshot is refused with a clear message, and the old shares are `retired`; (5) switch back. Clean up all shares at the end.
- **Failure test.** Make the snapshot of the second share fail. The first share's snapshot must be deleted and the pool snapshot must be `Failed`.

## Build plan: phases, one PR each

| Phase | What | Meets |
|---|---|---|
| 0 | Lab checks 1 to 12 of the IBM facts, with the numbers written into this spec. No product code. | Decides the rest |
| 1 | VPC client: create, get, list, delete share snapshot and share-from-snapshot, in the real and fake clients. IAM check. | Foundation |
| 2 | `PoolSnapshot`, the manifest Job, parallel snapshots, all-or-nothing, the freeze window, metrics. On request only. | Use 1 (protect) |
| 3 | `PoolSnapshotPolicy`: schedule, keep time, limits, webhook, alerts, chart. | Use 1 (protect) |
| 4 | Volume and VM disk recovery from a pool snapshot: the `/.snapshot` source for the restore Job, `kubectl backup restore --from-pool-snapshot`, access checks. | Single-volume goal |
| 5 | `PoolRestore` stage 1 (side by side) and the three lists. | Whole-pool goal (half) |
| 6 | `PoolRestore` stage 2: quiesce, switch, `LostByRestore`, node plugin reads the `SubVolume`, retired shares, rollback. | Whole-pool goal |
| 7 | Backup and replication read from the snapshot (`source: PoolSnapshot`) for plain volumes. | Use 2 |
| 8 | Console pages and the release test stage. (Release-test pieces for earlier phases are added in those phases.) | Usability |
| 9 (optional) | File-level browse from a pool snapshot; instant recovery volume. | Extra |

Size, honestly: phases 1 to 4 are about the size of the backup step 8 (new CRDs, a controller, a Job, a command). Phase 6 is the largest and riskiest: it changes how the node plugin finds a volume and moves every `SubVolume` of a pool. Total: about 8 PRs, 2 to 3 of them large.

## Does the design meet the goal?

| Part of the goal | Met? | Why |
|---|---|---|
| Protect a whole pool, any size, snapshot in seconds | Yes, if checks 1, 3 and 7 pass | One snapshot call per share, in parallel |
| Same moment for every volume | Yes for each volume (one share each). Within about 2 seconds for a VM with disks on two shares, closed by the freeze | IBM has no cross-share snapshot |
| Recover a whole pool in minutes, any size | **Yes for the new shares and the switch, if checks 5 and 6 pass.** | IBM makes the new share from pointers, usable while `pending` |
| Recover single volumes without affecting tenants | Yes, by copy. The time grows with the volume | The restore reads one read-only folder and writes a new PVC |
| Whole-pool recovery in place | **No** | IBM has no restore in place. We use new shares and a switch |
| Survive the loss of the share, zone or VPC | **No** | Snapshots live and die with the share. Use backups and COS for that |
| Protect against an attacker who has the cloud API key | **No** | They can delete the share and its snapshots. Use a COS lock |
| A pool snapshot of the new shares right after a restore | **No, for a time** | IBM forbids it while the share is `pending` |
| Zero outage for a whole-pool recovery | **No** | A switch needs the pods to stop. Mode `Copy` avoids the outage but is slow |

### Options considered

- **Recommended: the design above** (driver-run pool snapshots, `/.snapshot` single-volume recovery, side-by-side restore then a switch). **Meets the goal** except the rows marked No, which IBM's platform rules out.
- **Only IBM Backup for VPC plans on the shares, no driver code.** **Does not meet the goal.** No same moment across shares, no freeze, no manifest, no volume-level recovery, no switch. It is cheap and may do for an administrator who only wants a safety net. It can run next to the design above.
- **Only single-volume recovery (phase 4), no pool recovery.** **Does not meet the goal** (a whole pool would be recovered volume by volume, at the speed of the data). Useful first step; it is phases 1 to 4.
- **Restore a pool by copying all volumes out.** **Does not meet "minutes, any size".** It is mode `Copy`.

## Cost, effort and risk

- **IBM cost.** Snapshot storage in GB-hours (price to verify, check 8). A restored share costs as a normal share for as long as it exists, so a stage 1 that nobody switches or deletes is a bill. The controller reports restored shares older than 7 days. A switch leaves the old shares billed until deleted (default 7 days).
- **Effort.** About 8 PRs. The riskiest are phase 6 (the switch) and the first lab numbers (phase 0). If check 5 shows that a restored share is slow while `pending`, the "minutes" promise is weaker, and the spec must be changed before phase 5 starts.
- **Risk: the switch.** It changes where every volume of a pool lives. Mitigations: nothing is deleted, the old shares are retired and can be switched back, the lost volumes are blocked not emptied, and the administrator must approve after seeing three lists.
- **Risk: freeze.** A freeze of many VMs for 20 seconds can stall their I/O. It is off for `freeze: None`. The time limit is hard. A freeze failure makes a VM `CrashConsistent`, never blocks the snapshot.
- **Risk: false safety.** A pool snapshot is not a backup. The documents, the console and the alert text say so.
- **Risk: IBM facts wrong.** Twelve checks are listed in phase 0. Phases 2 and later do not start until the checks that they depend on are written into this spec.
- **Docs to update when built:** `FEATURES.md`, `BACKUP-RECOVERY.md`, `CRD-SPEC.md` (new CRDs), `MONITORING.md`, `HELM-VALUES.md`, `CONSISTENCY-MODEL.md` (the line that says "this driver does not use share snapshots"), `KNOWN-LIMITATIONS.md`, `API-KEY-SETUP.md` (new IAM rights), `CHANGELOG.md`, a new tutorial.

## Decisions I made for you

- Pool snapshots are an extra tool. They do not replace per-volume snapshots or backups.
- The driver runs the schedule. We do not use IBM Backup for VPC plans for pool snapshots.
- A pool snapshot is one VPC share snapshot per share, called in parallel. It is `Ready` only if all of them worked, else all are removed.
- Running VMs are frozen for a short, bounded window by default. A failed freeze gives a crash-consistent copy and never stops the snapshot.
- A manifest of the volumes is written onto each share before the snapshot, so a deleted volume can be recovered and a restore can say what it loses.
- A single volume or VM disk is recovered by copying from the read-only `/.snapshot` folder into a **new** PVC. It never writes over a live volume.
- A whole-pool recovery uses new shares from the snapshots. It has two stages: side by side first, then a switch that the administrator approves.
- The switch needs the pool's pods to be stopped. The driver does not stop other people's workloads by itself. `force` is the administrator's choice.
- Volumes created after the snapshot are blocked from mounting after a switch, not given an empty folder.
- Old shares are retired, kept 7 days, and deleted only by a confirmed command. That is the rollback.
- The node plugin will read a volume's share address from the `SubVolume`, because a PV's own address cannot be changed.
- Default schedule floor is 15 minutes. Default keep time is 3 days and at least 6 snapshots.
- Backup and replication of plain volumes can read from a pool snapshot (opt in). VM disks keep the QEMU path, because it sends only changed blocks.
- Tenants may recover their own volumes using the same namespace rule as backups. Pool snapshots, shares and restores of a whole pool are administrator only.
- Pool snapshots are not protection against loss of the share, the zone, or the VPC API key. The documents say so plainly.
- Phase 0 (lab checks) comes first, and later phases wait for the numbers they depend on.
