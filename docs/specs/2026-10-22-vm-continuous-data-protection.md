# Spec: continuous data protection for VMs (RPO of seconds)

Date: 2026-10-22. Status: draft for review. Nothing in this spec is built yet.

## Goal

Tier-1 VMs (databases, ERP) lose at most a few seconds of writes when the source site is lost. The DR site can start the VM as it was at any chosen second in a retention window. This is what Zerto and Veeam CDP offer.

"Done" means a lab test passes on two clusters in different VPCs:

- A write-heavy VM runs. We kill its source node in the middle of a write burst.
- The VM starts at the DR site at the newest safe second. At most 5 seconds of acknowledged writes are missing.
- We pick an earlier second from the last 4 hours. The VM starts as it was at that second. Checksums of files written before that second match. Nothing written after it is present.
- A VM with several disks shows the same second on every disk.
- The VM's disk writes are not slowed by more than the limit in "Effect on the VM".

Two parts of the goal need care, and this spec says so plainly:

- **RPO of seconds through a live migration or a node crash.** One of the two ways to tap the writes (the QEMU tap) cannot do this. It loses the journal for the time it takes to resync. The other way (the file-system tap) can. See "Which write tap".
- **One exact second across several VMs.** No design can promise it, because the VMs write on different nodes. We offer coordinated bookmarks instead. See "Several VMs".

## Words used here

- **RPO**: how much recent data you lose. "RPO 5 seconds" means the last 5 seconds of writes may be missing.
- **Journal**: an ordered record of every write to a disk. With it you can rebuild the disk as it was at any second.
- **Write tap**: the part that sees each write of a running VM disk and passes a copy on.
- **Relay**: a small service on the source node. It receives the copies from the tap, keeps them in order, and sends them to the DR site.
- **Bookmark**: a marked point in the journal where the guest file systems were frozen. A VM started from a bookmark needs no file-system check.
- **Batch**: all writes of one short window (1 second by default), joined into one unit. Each batch end is a point you can recover to.
- **Undo journal**: at the DR site, the old contents of every block that a batch changed. It is the finer form of the undo files that exist today.
- **Crash consistent**: the disk looks as after a power cut. A database repairs itself from that state.

## What exists today

All facts in this section come from the code and the docs in this repository.

- Replication of a running VM is a QEMU copy of all disks at one moment, on a schedule (5 minutes is the practical floor). The controller sends QMP commands (QEMU's control commands) through `virsh` in the VM pod (`pkg/vmcopy`). Red Hat does not support this. The owner accepted that on 20 Oct.
- Between copies QEMU keeps a change list (a dirty bitmap) in memory. The next copy sends only the changed blocks. A live migration or a restart erases the list (lab, 20 Oct). The next copy is then a full compare pass.
- The copy goes to the destination through the receiver: an HTTPS service with a bearer token (`pkg/replication/receiver*.go`). The call model is one session per copy: upload a staging file, compare, commit.
- At the destination, `ApplyCopy` first writes the old contents of every block it will change into an undo file, then writes the new blocks. The disk always holds the newest copy. Undo files give restore points for 24 hours by default (`restorePointKeepHours`). `Rewind` builds an older disk by copying the newest one and applying undo files backward. `kubectl failover plan|execute --rewind-to` uses it. A disk that a VM has run on cannot be rewound.
- Unfinished applies are rolled back at failover (`Recover`).
- A copy cannot run during a live migration. The controller cancels the copy when it sees the move. Long copies slow the VM: the lab saw 10 to 20 times slower writes when the copy shared the VM's file share.
- Instant snapshots (`snap-*.qcow2` overlay files) need the KubeVirt `Sidecar` gate, which Red Hat does not support. The hook is a sidecar container in the VM pod.
- Issue #89 and PR #105 gave the receiver its own ServiceAccount with no role, no API token, no capabilities, a token check in constant time, and a rule that it writes only into `pvc-<uuid>` folders. TLS on by default was left for later.
- IBM file share replication runs at most every 15 minutes (the FAQ says hourly; verify). It does not meet this goal. `docs/specs/2026-10-22-zone-loss.md` (PR #199) says the same.
- `CROSS-REGION-DR.md` rejects file-level streaming (inotify) because NFS gives no write events. That reasoning does not apply here. We tap writes inside QEMU or in a mount that we control, not on the share.

## Key questions this design answers

| Question | Answer |
|---|---|
| Can QEMU send every write of a running disk somewhere else? | Yes. `blockdev-mirror` copies a disk's writes to a target over NBD, in `background` or `write-blocking` mode. Docs: "blockdev-mirror: Start mirroring a block device's writes to a new destination." `write-blocking`: "when data is written to the source, write it (synchronously) to the target as well." `background`: "copy data in background only." A running job can switch from `background` to `write-blocking`, but not back (QEMU commit "mirror: implement mirror_change method", 2023). |
| Does a mirror give us a journal? | No. A mirror makes the target a copy of the disk. It sends no log of writes, no order and no time. The journal must come from our own server at the other end of the connection. |
| Is `background` mode enough? | No. It copies blocks later, from a list of changed blocks. It gives no order across disks and no history. Only `write-blocking` gives a usable order. |
| Can today's restore points grow into a journal? | Partly. The model stays: the disk holds the newest data, and undo records go back in time. The format and the calls are new. Today one undo file covers one copy. A journal needs thousands of small records an hour, and a way to stop in the middle of a file. |
| How do all disks of a VM stay at the same second? | All disks' writes must pass one sequencer on the source node, with the mirror in `write-blocking` mode. Separate mirror jobs have no shared order. The relay (below) is that sequencer. |
| Does it work for plain volumes, not only VM disks? | Not in this design. The QEMU tap sees only VM disks. A file-system tap could also see plain volumes, but a folder of files also needs create, rename and delete in the journal. That is a much larger job, and it is out of scope. |
| Does Red Hat support it? | Not the QEMU tap: the driver works with QEMU directly, as today's VM copies do. The sidecar tap would also need the `Sidecar` gate. The file-system tap changes nothing in KubeVirt or QEMU, but we must ask Red Hat whether a CSI driver that serves files through a user-space mount is supported (to verify). |
| What if the link to DR is slow? | With a relay on the same node, `write-blocking` waits for the relay, not for the DR site. The link then decides how far the DR site lags, not how fast the VM writes. |
| What happens on a live migration? | Probably the mirror stops and a resync is needed (not proven). The QEMU docs say lists of changed blocks can move with a VM, but our lab lost them, so this is probably off under libvirt. Phase 0 tests it. |
| Can IBM share replication do this instead? | No. It copies every 15 minutes or more (IBM docs). |

## Research: QEMU and KubeVirt facts

Sources are the official docs. Items marked "verify in the lab" are not proven for our VM pods (libvirt 10.10, QEMU 9.1 on OpenShift 4.21).

1. **Mirror jobs.** `blockdev-mirror` mirrors a node of the block graph to a target that we create with `blockdev-add`. Sync modes: `full`, `top`, `none` (only new writes), `bitmap`. After the job is `ready`, "the source and destination are synchronized". Cancelling it then leaves a point-in-time copy. Completing it switches the guest to the target. Source: [Live Block Device Operations](https://qemu-project.gitlab.io/qemu/interop/live-block-operations.html), [QMP reference](https://qemu-project.gitlab.io/qemu/interop/qemu-qmp-ref.html).
2. **Copy mode.** `copy-mode` is `background` (default) or `write-blocking` (since QEMU 3.0). `query-block-jobs` shows `actively-synced`. Source: QMP reference. The docs do not say what happens to the guest when the target stalls. Verify in the lab.
3. **NBD as target.** QEMU's NBD client connects out. The VM pod forwards incoming connections to the guest (lab, 20 Oct), but outgoing connections work. The `nbd` block driver has reconnect options (`reconnect-delay`). Verify the exact option names for QEMU 9.1.
4. **Dirty bitmaps.** In-memory bitmaps track writes. Only qcow2 files can store a bitmap on disk. Our disks are raw, so bitmaps are in memory only. The docs say bitmaps can migrate with a live migration. Our lab lost them. Verify why. Source: [Dirty Bitmaps and Incremental Backup](https://qemu-project.gitlab.io/qemu/interop/bitmaps.html).
5. **Backup and fleecing.** `blockdev-backup` copies a point in time. The `copy-before-write` filter copies the old contents of a cluster to a target before the guest overwrites it. It has `target`, `bitmap`, `on-cbw-error`, `cbw-timeout` and `min-cluster-size`. `discard-source` exists since QEMU 9.1. This is built for a point in time, not for a stream of every write. Source: QMP reference.
6. **qemu-storage-daemon.** A long-running QEMU process without a VM. It builds block nodes, exports them (NBD, vhost-user-blk, FUSE, VDUSE), runs block jobs, and is controlled by QMP. NBD servers can use TLS through `--object tls-creds-*`. Exports are read-only unless `writable=on`. Source: [qemu-storage-daemon](https://qemu-project.gitlab.io/qemu/tools/qemu-storage-daemon.html). It has no write journal of its own.
7. **KubeVirt and OpenShift Virtualization backup.** Changed block tracking arrives in KubeVirt v1.8. Veeam says it is a Tech Preview in OpenShift 4.22 and that Kasten reads QEMU's dirty bitmap. It is a periodic backup API (push a full or changed-block copy), not a write stream. It cannot give an RPO of seconds. The gate name and the API shape are not confirmed. Sources: [Veeam blog](https://www.veeam.com/blog/openshift-virtualization-backup-kubevirt-cbt.html), [Red Hat 4.22 release notes](https://docs.redhat.com/en/documentation/openshift_container_platform/4.22/html/virtualization/release-notes) (I could not read the Technology Preview section; verify).
8. **Hook sidecar.** Our hook can change the VM's domain XML before start, as the instant snapshot hook does. It needs the `Sidecar` gate.

## Options compared

Each option is tested against the goal. "Goal" means: RPO of seconds in steady state, any second in the window, one second on all disks of a VM.

| # | Option | Steady-state RPO of seconds | Any second | Same second on all disks | Survives live migration with no RPO loss | Survives node crash with no divergence | Effect on VM | Meets the goal? |
|---|---|---|---|---|---|---|---|---|
| 1 | `blockdev-mirror` straight to an NBD server at the DR site | In `background` mode, no bound. In `write-blocking` mode, yes, but every write waits for the WAN round trip (10 to 30 ms between regions) | Only if the DR server journals | No (one job per disk, no shared order) | No | No | Severe in `write-blocking` | **No.** Fails on write speed or on order |
| 2 | `blockdev-mirror` in `write-blocking` mode to a **node-local relay**, which journals and ships to DR (the **QEMU tap**) | Yes | Yes (1 s batches) | Yes (one relay orders all disks) | **No.** The mirror stops. A resync of unknown size follows | Not unless the relay keeps a durable spool | Small (the relay answers from memory) | **In steady state, yes. Not through a migration or a crash.** |
| 3 | `blockdev-backup` or `copy-before-write` with fleecing, run every few seconds | No. A cycle needs a guest freeze (0.1 to 0.2 s) and the lab saw 10 to 20 times slower writes during a copy. A 10 second cycle is not workable | Only at cycle points | Yes (one transaction) | No | Yes | Severe | **No.** Keep it as the fallback when the journal breaks |
| 4 | **Write-splitting layer in the hook sidecar:** `qemu-storage-daemon` exports the disk to the VM over NBD (loopback) and tees writes to a journal | Yes | Yes | Yes | Yes | Yes (with a durable spool) | All reads and writes pass one more hop. Breaks the instant snapshot and QEMU copy code, because the file leaves the VM's own block graph | **Yes**, at the highest cost, and it needs the `Sidecar` gate |
| 5 | **File-system tap** (new): the CSI node plugin serves the VM disk file through a user-space mount that tees each write to the relay (the **file-system tap**) | Yes | Yes | Yes (one daemon per node orders all disks) | Yes | Yes (with a durable spool) | Every read and write passes the user-space daemon. Highest risk to I/O speed | **Yes**, if its speed is acceptable |
| 6 | KubeVirt incremental backup (changed block tracking) | No (periodic) | No | Yes | Yes (supported API) | Yes | Moderate | **No.** A good later replacement for the periodic copy |
| 7 | Synchronous mirror to DR, `write-blocking` straight to a receiver within the same region (round trip 2 ms or less) | RPO 0 | Yes if journaled | Needs the relay | No | Yes | Adds one round trip to every write | **Yes, for zone loss only.** Not for cross-region |
| 8 | IBM share replication | No (15 minutes or more) | No | No | n/a | n/a | None | **No.** |
| 9 | In-guest or application replication (database log shipping, for example) | Yes for that application | Per application | Per application | Yes | Yes | Per application | Outside the driver. Fine to use next to it |

## The design

### In one paragraph

A **relay** on each source node receives a copy of every write to the disks of protected VMs. It numbers the writes in the order they arrive, groups them into 1 second batches, compresses them and sends them over TLS to the **receiver** at the DR site. The receiver stores each batch durably, then applies it to the replica disk, after saving the old block contents in an **undo journal**. The replica disk always holds the newest data. So a failover to the newest second is as fast as today. A failover to an earlier second applies undo records backward. Bookmarks, made with a guest freeze every 5 minutes, mark points where the file systems are consistent.

### Which write tap

The tap is the part that sees each write. The relay, the transport, the DR journal, the failover and the console are the same for every tap. We build them once. The tap is replaceable, as the copy piece was for replication.

**Tap 1: QEMU mirror into the relay (build first).**
- The controller sends QMP commands to the VM's QEMU, as `pkg/vmcopy` does today. It adds an NBD target that points at the node's relay, and starts `blockdev-mirror` with `sync=none` and `copy-mode=write-blocking`. Every guest write then goes to the disk file and to the relay. The guest write completes when both finish.
- Order is correct: the guest issues a dependent write only after the first write completes. In `write-blocking` mode that means the relay already holds the first write. So any prefix of the relay's order is a state the disks could really have had. This is why one relay gives the same second on all disks.
- The relay is a separate DaemonSet, not part of the CSI node plugin. A driver upgrade then does not drop the connection. The NBD client uses a reconnect delay, so a relay restart of a few seconds only pauses writes for that long (verify the option).
- It needs no `Sidecar` gate. It needs QMP, which the driver already uses and which Red Hat does not support.
- The limit: when a live migration or a crash ends the QEMU process, the job ends. The new QEMU has no job and no change list. See "Live migration and restart".

**Tap 2: file-system tap (build if phase 0 says Tap 1's gap is not acceptable).**
- For a protected VM, the node plugin serves the disk folder through a FUSE mount (a file system that runs as a normal program). The program passes each read and write to the NFS share. It tees each write to the relay before it answers.
- QEMU, libvirt and KubeVirt see an ordinary file. Nothing in the VM changes. Live migration and restart work, because no QEMU job is involved. Instant snapshot overlays and QEMU copies still work.
- The relay on each node journals the writes of all disks on that node, in order. After a live migration, the new node's relay continues the same journal (see "Live migration and restart").
- The risk is speed and failure. Every I/O of a tier-1 VM passes through our program. If the program dies, the mount returns errors and the VM sees disk errors. Phase 0 measures the speed. Red Hat's view on support is still to verify.

**Tap 3: sidecar with `qemu-storage-daemon` (option 4).** We do not build it. It meets the goal like Tap 2, but it needs the `Sidecar` gate, it moves the disk out of the VM's own block graph, and so it breaks instant snapshots and the QEMU copies. Tap 2 has none of those costs.

**Recommendation.** The design that meets the whole goal is the journal with Tap 2. Tap 1 meets the goal in steady state only. We build the shared parts first with Tap 1, because it needs no new data-path program and it gives a working RPO of seconds early. Phase 0 and phase 2 results decide whether Tap 2 is also built. Tap 1 alone is labelled as not meeting the goal for migration and crash.

### Source side

**Relay.** One pod per node (DaemonSet, own ServiceAccount, no Kubernetes role). For each protected VM it holds a **stream**.
- It accepts writes from the tap. For each write it records: stream number, sequence number, disk, offset, length, bytes, and the node clock time.
- It adds an empty heartbeat batch every second, so an idle VM still shows a fresh time.
- Every second it closes a **batch**. Overlapping writes inside one batch collapse into the last one. Batch ends are the recovery points. The batch length is a policy setting (`batchInterval`, 1 s; 100 ms to 10 s allowed).
- It compresses each batch (zstd) and sends it. It sends over one TLS connection per stream.
- It keeps a batch until the receiver says it is durable at DR.
- Trim and write-zeroes commands are journaled as zero ranges, not as data.

**What is in the write path.** For Tap 1 the guest waits for the relay to accept the write into memory. It does not wait for the network to DR. Expected cost: a loopback round trip, well under 1 ms. We measure it in phase 0. The relay adds no I/O to the VM's file share, which is the big difference from the periodic copies. Those read the disk from the same share and slowed the VM 10 to 20 times in the lab.

**Durability levels** (policy setting `spool.durability`):

| Level | Where an unsent batch lives | A relay or node crash loses | Cost |
|---|---|---|---|
| `memory` (default) | Relay memory | Up to the unsent tail: the batch window plus what the link has not carried | None |
| `node-disk` | Node-local disk, written before the guest write completes | Nothing, if only the relay restarts. The tail if the node is lost | A local write per batch |
| `shared` | A small spool file on the pool share | Nothing, even if the node is lost. The tail is read by another node's relay and sent | One more NFS write per batch. Doubles the write latency on that share |

The default is `memory`. A site loss loses at most the unsent tail either way, because the DR site holds only what has arrived. The levels matter for a relay restart, a node crash and divergence (below).

### Transport

- One TLS 1.3 connection per stream, relay to receiver, with certificates on both sides (see Security).
- Frames: open stream (policy, volume names, epoch), batch (sequence range, time, compressed records, checksum), bookmark, heartbeat, seal.
- The receiver answers each batch with the highest sequence it holds durably. The relay frees buffers up to there.
- Sizing the link: see "Bandwidth".
- The relay sends batches in order, with a window of several unacknowledged batches. One slow batch does not stop the stream.

### DR side

The DR cluster runs the receiver, extended with a journal service. For each disk:

1. **Receive.** A batch is written to a staging journal on the DR share and flushed. The receiver then tells the relay "durable up to sequence N". **This is the moment that counts for the RPO.**
2. **Apply.** An apply worker reads the staging batches in order. For each batch it reads the old contents of the blocks, appends them to the **undo journal** and flushes it, and writes the new blocks to `disk.img`. It then records "applied up to sequence N". The staging batch can be deleted after that. If the receiver stops in the middle, recovery knows the batch from the staging journal and the undo journal. It can finish the batch or roll it back.
3. **Result.** `disk.img` holds the newest second. The undo journal holds how to go back.

**Undo journal format.** Append-only segments, one per minute. Each record: sequence, batch time, disk, offset, old data. A batch is complete only when its end marker is written. A rewind to second T applies records backward from the newest batch down to the first batch whose time is after T. It stops at a batch end, never inside a batch.

**Why not keep only after-images and apply them late (the way Zerto describes its journal).** Then the replica disk is hours behind. A failover to the newest second must replay hours of writes first. A failover to the newest second is the common case in a disaster, so we keep the disk current and pay for it with extra reads and writes at the DR site (below).

**DR cost per incoming byte.** One read of the old block, one write to the undo journal, one write to the disk. That is about 3 operations per changed block. DR shares need room for this. See "Bandwidth and size".

### Retention in two tiers

- **Seconds tier** (`window`, default 4 hours): every batch end is a recovery point. The undo journal keeps this.
- **Minutes tier** (existing `restorePointKeepHours`, default 24 hours): after the seconds window, a background task merges the undo journal into one undo file per 5 minutes, using the same format as today. The restore points of the existing model take over.
- Beyond 24 hours: backups (existing). A BackupPolicy keeps working as before.

The merge keeps the oldest block contents of each 5 minute span. A rewind to a second older than `window` lands on the nearest 5 minute point.

### Checkpoints with guest freeze (bookmarks)

- Every `bookmarkInterval` (default 5 minutes) the controller freezes the guest through the guest agent, as it does for copies today. The relay writes a bookmark into every disk's stream between the freeze and the thaw. The guest is paused about 0.15 s.
- A VM without a guest agent gets a crash-consistent bookmark and the status says why, as today.
- A bookmark is a recovery point at which file systems are consistent. A second between bookmarks is crash consistent. A database recovers from a crash-consistent state. A bookmark is better for applications that do not.
- Lifecycle hooks (existing) run before the freeze, so an application can flush.
- Bookmark IDs are random and unique. The same ID goes into every disk and, for a group, into every VM (below).

### Several disks of one VM

One relay orders the writes of all disks on the node. Every batch end is the same second on all disks. Failover picks one second and applies it to every disk of the VM. The existing planner already refuses to mix moments across disks of a VM.

### Several VMs (protection groups)

- **What we can promise.** A bookmark ID shared by all VMs in a group. A coordinated freeze of the group, one VM after another, in under about 1 second in total. A recovery to that bookmark gives a consistent state per VM, and an application-consistent state across the group for applications that tolerate that.
- **What we cannot promise.** One exact crash-consistent second across VMs on different nodes. The VMs write on different machines. No single writer sees their order. We can cut by clock time, and the node clocks differ by some milliseconds (chrony). A message from VM A to VM B that arrives faster than the skew can end up on the wrong side of the cut. The status shows the measured skew.
- Zerto's protection groups are the same idea. A group is optional. By default each VM is on its own.

### Network loss

The relay never lets the network slow the VM, unless the policy says so.

1. **Buffer.** Unsent batches stay in memory, up to `spool.memoryMiB` per VM (default 512), then spill to a node-local spool file up to `spool.diskGiB` per VM (default 10).
2. **Alert.** `CDPLagging` fires when the RPO passes `rpoWarning` (default 15 s). `CDPJournalBroken` fires at the next step.
3. **Overflow.** When both buffers are full, policy `onOverflow` decides:
   - `degrade` (default): the relay stops journaling for this VM and tells the controller. The VM keeps full speed. The controller falls back to the existing periodic copy (5 minutes), which still has its change list. When the link returns, the copy catches up and the journal restarts from a fresh baseline. The recovery range has a hole for the broken time. The status and the console show it.
   - `block`: the relay slows the guest's writes until there is room. The RPO stays bounded. The VM slows or stops during a long outage. For customers who prefer a slow VM to a hole.
4. **Catch-up after a hole.** The journal does not know which blocks changed in the hole. The periodic copy's change list does (Tap 1: QEMU's list, kept always on as the safety net). The catch-up sends only those blocks. The journal is valid again from the end of the catch-up (see "Starting a journal").

### Starting a journal

At the start, and after every hole, the DR disk must match the source disk at some moment, and the journal must have no gap after it.

1. Start the change list in the same QEMU step as today's copy (it already does `block-dirty-bitmap-add` in a transaction).
2. Send the copy as today, as the baseline. Or send a catch-up copy of only the changed blocks.
3. Start the mirror (`sync=none`, `write-blocking`) after the baseline starts. Writes between the baseline moment and the mirror start are in the change list. A catch-up copy sends them.
4. Replaying the journal on top of a baseline and a catch-up gives a correct disk from the end of the catch-up onward. Blocks that were written in both windows end up right because the journal replays in order.

So the recovery range of a new journal starts at the end of the catch-up, not at the baseline moment. Before that, the periodic restore points apply.

Bench for this step: write a pattern into a VM during a start, then compare checksums of the DR disk. This is a phase 1 test.

### Live migration and restart

**Tap 1 (QEMU mirror).** The job ends with the QEMU process. What the design does:

- A planned migration: the controller sees the request (it already does, for copies). It seals the stream: the relay flushes all batches and the receiver confirms. It stops the mirror. The migration runs. On the new node, a new mirror starts.
- The writes between stopping the mirror and starting the new one are not in the journal. The change list is gone (lab). So the replica could differ from the source on blocks written in that window. The window is the length of the migration, 10 seconds to a few minutes.
- The fix is a **compare pass** of the whole disk. Both sides compute a checksum for each 1 MiB chunk. Only differing chunks cross the link. This exists today. Its time is the disk size divided by the slower read speed. A 500 GB disk at 150 MB/s takes about 55 minutes. During it, the journal runs on top, and recovery to a second before the migration works. **Recovery to a second after the migration is safe only after the compare pass ends.** Until then, a site loss gives a recovery point at the seal, not at the newest second. That is an RPO of the compare pass time. **This is the part of the goal Tap 1 does not meet.**
- Sizes: a VM with a 100 GB disk, about 11 minutes; 500 GB, about 55 minutes; 2 TB, about 3.7 hours. These are estimates; phase 0 measures them.
- Cheaper ways to narrow the gap, all tested in phase 0:
  - Turn on QEMU's `dirty-bitmaps` migration capability, if libvirt or KubeVirt lets us. Then the change list survives and no compare pass is needed.
  - Check whether QEMU allows a migration while a mirror job runs. If it does, the mirror can run until the guest pauses, which makes the window as small as the switch-over.
  - Check whether the destination QEMU accepts `blockdev-mirror` before the guest resumes. KubeVirt resumes it at once, so probably not.
- A restart: if the guest shuts down cleanly, the relay seals after the last write and the disk has not changed since. The next start checks the disk's size and change time against the seal record and starts a new mirror with no compare pass. A VM that crashed (not clean) needs the compare pass, because writes the relay never received (or lost) may be on the source disk.

**Tap 2 (file-system tap).** There is no job to lose.
- On a migration the source node's relay and the destination node's relay both hold the stream. The receiver allows one writer at a time. The source relay seals when the guest pauses. The destination relay opens the next **epoch** only after the seal arrives, or after a fence time (the controller marks the old pod gone). Writes in the new epoch sort after the old epoch.
- A restart is the same. A crash restart: the relay keeps a persistent record of dirty regions (a bitmap on the pool share). After a crash, only those regions are compared, not the whole disk. If `spool.durability` is `shared`, even the unsent tail survives.
- So the goal holds through migration and crash.

### Failover to any point in time

Reuse `kubectl failover`. Add:

- `kubectl failover points --vm <name>` lists the recovery range, the bookmarks and the holes.
- `kubectl failover plan|execute --at <time>` picks the newest batch end at or before the time, on all disks of the VM. `--bookmark <id>` picks a bookmark. `--rewind-to` keeps working for the minutes tier.
- **Newest second:** drain the staging journal (seconds), run `Recover`, create the PV and PVC as today. The disk is current. No copy.
- **Earlier second:** `Rewind` copies the newest disk to a new folder, then applies undo records backward to the chosen batch. This copies the whole disk, as a rewind does today. It is slow for big disks (a 30 GB disk with 2 GB used took 165 s in the lab; a 500 GB disk with 50 GB used takes minutes). A faster rewind that does not copy (an overlay file, with the existing hook) is part 2; see "Phases".
- The latest disk stays unchanged, so the admin can try a second time at a different second.
- The newest second is chosen by the receiver: the highest batch end that all disks of the VM have. The plan shows the RPO in seconds, as `plan` does now.
- Failover to a second that lies in a hole (a gap after a broken journal) is refused with the nearest valid seconds on each side.

### Failback (reverse CDP)

- After a failover the VM runs at the DR site. The old source disk may hold writes the DR site never received (the unsent tail). It cannot be used as is.
- Failback is a new policy in the other direction. The first step is a compare pass of the old source disk against the running DR disk, with the same checksum method. Differing chunks flow back. Then a journal starts in the reverse direction (Tap 1 or Tap 2 at the DR site).
- A planned failback is clean: stop the VM at the DR site, wait for the seal (zero tail), start it at the old source. No data is lost. Then the policy turns around again.
- Today `CROSS-REGION-DR.md` describes failback as a reverse replication policy. This is the same, with a journal on top.

### Effect on the VM

These are targets for the phase 0 tests, not measured results.

- Tap 1 write latency added: 0.5 ms or less at the 99th percentile, with a loopback relay.
- Tap 1 throughput: 95% or more of the unprotected disk on a write-heavy fio run. The relay adds no I/O to the file share.
- Tap 2 write latency added: 1 ms or less. Tap 2 read throughput: 90% or more. If Tap 2 misses these, we do not build it.
- Guest pause per bookmark: 0.2 s or less (lab: 0.1 to 0.2 s).
- A relay restart pauses writes for seconds, bounded by the reconnect delay. It must not cause I/O errors in the guest.
- `write-blocking` mode in QEMU has one more risk: while the target is slow, guest writes wait. The relay answers from memory so the wait is short. If the relay is stalled, the guest stalls. The relay needs a watchdog: after a timeout it drops the stream (overflow `degrade`) so the guest never hangs for long. The timeout is a policy setting (default 2 s).

Phase 0 measures each of these for both taps.

### Bandwidth and size

Bandwidth is the write rate of the VM after batch coalescing and compression. It is **not** the changed-block rate that periodic copies send. A database that rewrites the same blocks again and again sends more under CDP than under 5 minute copies. The table uses assumed factors (coalescing and compression together: 2 to 1). Replace them with the phase 0 measurements.

| VM write rate | After 2 to 1 | Link needed (bits) | Journal for 4 hours | DR share I/O (3 operations per block) |
|---|---|---|---|---|
| 5 MB/s | 2.5 MB/s | 20 Mbit/s | 36 GB | 15 MB/s |
| 20 MB/s | 10 MB/s | 80 Mbit/s | 144 GB | 60 MB/s |
| 100 MB/s | 50 MB/s | 400 Mbit/s | 720 GB | 300 MB/s |
| 200 MB/s (the rate in the 20 Oct lab VM) | 100 MB/s | 800 Mbit/s | 1.4 TB | 600 MB/s |

Rules:
- The link must carry the **peak** write rate for longer than the buffer can hide. Buffer time = buffer size divided by (peak minus link). A 512 MiB buffer hides a 100 MB/s surplus for about 5 seconds.
- Size the DR share for 3 times the incoming rate, plus the 4 hour journal, plus the minutes tier, plus the disk itself.
- Compression is cheap on the relay (zstd level 1). It uses CPU on the source node: about one core for 200 MB/s (to verify).
- The cross-region link rate on IBM Cloud (Transit Gateway or VPN) is to verify in phase 0. The lab uses two different VPCs; VPN throughput can be far lower than a Transit Gateway.
- A VM that writes more than the link can carry for hours cannot have an RPO of seconds, whatever the design. The console shows this before the policy is saved (a warning when the sustained write rate is above 70% of the measured link).

### Security

- **TLS and authentication.** The current receiver uses a bearer token, and TLS on by default was left for later in #89. CDP carries every write of the most sensitive VMs, so TLS 1.3 with client certificates is **required** for a journal stream. The receiver refuses a journal stream without it. The certificates come from cert-manager, one per source cluster, with a short life.
- **Stream scope.** Each stream is bound to one policy and one list of `pvc-<uuid>` volumes, fixed when the stream opens. The receiver refuses a write to any other volume. It checks that sequence numbers only increase and that the epoch is the current one. A replayed or forged batch is refused.
- **The receiver's rights.** Unchanged in kind: own ServiceAccount, no role, no API token, no capabilities, writes only inside `pvc-<uuid>` folders (PR #105). The journal service adds write access to a `journal/` area inside those folders. Limits: batch size, stream count, segment size, and total journal size per volume. A full journal area stops the stream (degrade) instead of filling the share.
- **Tap 1 connection on the node.** QEMU in the VM pod connects to the relay over the pod network to the node. Another pod on the node could connect and send fake writes. The export name is a random secret for each stream, shared only through QMP. The relay accepts one connection per stream and checks the source address. A NetworkPolicy limits who can reach the relay's port. The OVN user-defined network (UDN) may block a VM pod from reaching the node. Verify in the lab.
- **Relay rights.** It runs privileged only if Tap 2 needs it. The relay for Tap 1 needs no extra rights. It sees the plain data of every protected VM on its node. A node compromise already allows this, because the node mounts the shares.
- **Data at rest.** The DR share is encrypted by IBM as every share is. The journal holds plain writes, so anyone who can read the DR share can read them. The spool on the node disk (`node-disk`) is also plain; it uses an encrypted node disk or an `emptyDir` on the node's encrypted volume (to verify on ROKS).
- **Controller.** It keeps its right to run commands in VM pods (the `kubevirt-controller` SCC), as for copies today. No new right.
- **Red Hat support.** QMP into KubeVirt VMs is unsupported (accepted on 20 Oct). A journal that stops the VM from live-migrating or hangs it on a relay stall is a new kind of risk. The owner should treat CDP VMs as a separate support class.

### How it fits with what exists

- **Periodic replication** keeps running as a **safety net** while the journal is healthy, but at a longer interval (default 60 minutes, off if you choose). It supplies the change list for catch-ups and a checksum scrub of the DR disk. If the journal breaks, the periodic copy takes over at its own interval (5 minutes if you set it).
- **Backups** (`BackupPolicy`) are unchanged. They read from the local share, not from the DR site. A CDP VM can be in a backup policy.
- **Instant snapshots** work with Tap 1 (QEMU does the snapshot; the mirror follows the top file; verify that a snapshot during a mirror job is allowed). With Tap 2 they work, and the journal records writes to the overlay files. Replicas then hold the overlay files too. First release: a CDP VM must have no instant snapshots (the webhook refuses the policy, or flatten first). This is simpler. Phase 4 lifts it.
- **Restore points.** The minutes tier is the existing format. Rewind and failover code is reused. The journal adds a finer layer on top.
- **Zone loss** (PR #199): Level 2 copies a pool to a partner pool in another zone at 5 to 10 minutes. CDP gives the same-cluster option an RPO of seconds when the receiver and relay run in the partner zone. Option 7 (a synchronous mirror within the region, 2 ms or less) can give RPO 0 there. That is an extension, not part of this build. Regional `rfs` shares give RPO 0 without any of this, where IBM offers them.

### Console and CLI

- **Policy.** A new `cdp` block in `ReplicationPolicy`: `enabled`, `batchInterval`, `window`, `bookmarkInterval`, `onOverflow`, `rpoWarning`, `spool.{memoryMiB,diskGiB,durability}`, `tap` (`qemu` or `filesystem`). The webhook checks it: needs the receiver mode, TLS, a guest-agent warning, no instant snapshots (first release).
- **Console.** The VM page shows the live RPO, the state (`Seeding`, `Healthy`, `Lagging`, `Broken`, `Resyncing`), the lag in seconds and bytes, the buffer fill, and the recovery range as a bar. The bar shows bookmarks and holes. A "Recover to" picker takes a time or a bookmark and shows the plan before it runs. At the DR site, the same picker starts the failover.
- **CLI.** `kubectl failover points`, `plan --at`, `execute --at`, `--bookmark` (above). `kubectl failover status --vm` shows the live numbers.

### Metrics and alerts

New metrics (names follow the existing `pool_csi_` style; they go into `MONITORING.md` when built):

- `pool_csi_cdp_rpo_seconds{vm}`: age of the oldest write not yet durable at DR. It is 0 when nothing is pending. Heartbeats prove it.
- `pool_csi_cdp_state{vm,state}`.
- `pool_csi_cdp_buffer_bytes{vm,level}` and `pool_csi_cdp_sent_bytes_total{vm}`.
- `pool_csi_cdp_write_ack_seconds{vm}`: histogram of the relay's answer time. This is the cost to the guest.
- `pool_csi_cdp_apply_lag_seconds{vm}` at DR.
- `pool_csi_cdp_range_seconds{vm}`: length of the recovery range.
- `pool_csi_cdp_resync_progress{vm}`, `pool_csi_cdp_clock_skew_seconds{node}`.

Alerts: `CDPLagging` (RPO above `rpoWarning` for 1 minute), `CDPRpoBreach` (above the policy target for 2 minutes), `CDPJournalBroken`, `CDPResyncLong` (resync over 1 hour), `CDPApplyBehind`, `CDPJournalSpaceLow`.

## Cost, effort and risk

**Effort.** In pull requests, the same unit as the earlier specs. About 25 PRs for the journal with Tap 1, and about 10 more for Tap 2. Plus lab time on two clusters. This is a larger project than the whole of VM replication (about 5 PRs). It is the biggest feature in the driver.

**What it costs to run.**
- DR share space: the disk, the journal window, and the minutes tier. See the bandwidth table.
- DR share speed: 3 operations per changed block.
- Link: the VM's write rate after compression, for as long as the VM runs. Cross-region data transfer is billed by IBM (verify the price).
- A relay pod on each node, with memory for buffers (512 MiB per protected VM by default).
- Tap 2 adds a user-space program on the path of every I/O.

**Risks, most serious first.**
1. **Tap 1 loses the journal on a live migration or a crash.** Sized above. Phase 0 may shrink it. Tap 2 removes it.
2. **A stalled relay stalls the VM** (`write-blocking`). The watchdog and the degrade mode limit it. The test "kill the relay under load" must show no guest I/O error.
3. **Tap 2 is in the I/O path.** A bug or a crash in it hurts the VM. It needs the strongest testing and a soak run. It may fail the speed targets.
4. **Unsupported by Red Hat.** An OpenShift Virtualization update can change the VM pod and break QMP, or change how mirror jobs and migrations interact. Every release test run gets a CDP stage.
5. **Hot-block workloads send far more than periodic copies do.** The console warns about it.
6. **DR apply lag.** If the apply worker cannot keep up, failover to the newest second takes longer. The metric and alert show it.
7. **Clock skew** limits cross-VM cuts and the meaning of "second".
8. **Complex recovery code.** Undo journal recovery after a crash at any point must be correct. Property tests and crash tests cover it (see Testing).

## Testing

Tests ship in the same PR as the code, as in the earlier specs.

**Unit (no cluster).**
- Batch coalescing: later writes win, trims and zero writes are right, order across disks is kept.
- Wire frames: reject bad sizes, bad checksums, a sequence that goes back, a wrong epoch, a volume that is not in the stream.
- Undo journal: apply then rewind to every batch end gives the exact earlier disk (property test with random writes). A crash injected at every step of apply (stage, undo append, disk write, marker) recovers to a valid batch end.
- Retention merge: a rewind before and after the merge gives the same bytes.
- Hole handling: failover to a second inside a hole is refused.
- Failover planner: the newest common batch end across disks.
- Overflow: `degrade` and `block` behave as set.

**Lab (two clusters).** Source `rovs-aug-2026-v2`, DR `rovslab-dr-eude`. They are in different VPCs. The link is what the lab has (VPN or Transit Gateway). Verify the link speed first.
1. **Steady state.** A CentOS VM with two disks runs a writer that logs a counter with a checksum to each disk every 10 ms. Journal on. Check the live RPO stays under 5 s for 1 hour.
2. **Kill the source mid-write.** Power off the source worker (or delete the VM pod and cut the network) during a write burst. Fail over at the newest second. Check: the VM boots, file systems mount, and the counters on both disks end within 5 s of the kill and **match each other** (the same second).
3. **Chosen second.** Fail over to a second T from 30 minutes ago. Check: every record written before T is present and its checksum matches. No record written after T is present. Run for five different T, including one between two bookmarks and one exactly on a bookmark.
4. **Window ends.** Fail over to a second older than `window`: land on the nearest 5 minute point. Check the plan says so.
5. **Link loss.** Cut the link for 30 s, then 5 minutes, then 30 minutes. Check the alerts, the buffers, `degrade` falling back to the periodic copy, the catch-up, and a new journal. Check the VM's fio throughput is unchanged during the outage in `degrade` mode.
6. **Live migration.** Migrate the VM during the writer. Tap 1: measure the gap, the compare pass time, and the recovery point during it. Tap 2: check the journal continues with no hole.
7. **Restart and crash.** Clean restart, guest crash, node crash. Check divergence handling.
8. **Relay restart under load.** No guest I/O error.
9. **Failback.** Reverse resync and a planned failback with zero loss.
10. **Windows.** A Windows Server VM with VSS bookmarks.
11. **Performance.** fio random and sequential, read and write, 4 KiB and 1 MiB, with and without CDP, for each tap. Report latency percentiles and throughput. Check the targets in "Effect on the VM".
12. **Release test run.** Add a CDP stage with a short version of tests 1, 2, 3 and 6.

## Phases

**Phase 0: lab spikes (about 2 weeks; no product code).** Each answers a question that can change the design.
1. Mirror throughput: `blockdev-mirror` with `sync=none` and `copy-mode=write-blocking` to an NBD server on the same node (a small test server). Does it work with `sync=none`? Does the job report `actively-synced`?
2. VM write latency and fio throughput with no mirror, `background`, and `write-blocking`. Latency added per write.
3. Link loss: break the NBD connection. What does the job do? What does the guest see? Which `on-target-error` and reconnect options give no guest error?
4. Live migration with a running mirror job: refused or allowed? What does the destination QEMU have? Is `dirty-bitmaps` migration available through libvirt and KubeVirt?
5. Node-local relay reachability from a VM pod, including a pod with a primary user-defined network and a NetworkPolicy.
6. Snapshots during a mirror job (instant snapshot, blockcommit).
7. Link speed between the two lab VPCs, and the compare pass speed on a 100 GB disk.
8. FUSE tap: a pass-through FUSE mount over the NFS share, as the disk of a VM. Speed with fio. Does QEMU's `O_DIRECT` work? What happens on a daemon kill? Does live migration work? Ask Red Hat about support.
9. Clock skew between nodes of one cluster, and between the two lab clusters.
10. zstd cost and ratio on real write traffic (a database benchmark such as pgbench or HammerDB).

Decision at the end of phase 0: build Tap 1 first (default), or go straight to Tap 2 if Tap 1's migration gap cannot be narrowed and Tap 2's speed passes.

**Phase 1: one disk, steady state, Tap 1 (about 8 PRs).** Relay, wire format, receiver journal service, staging, apply with undo journal, crash recovery, starting a journal, failover to the newest second, metrics. Lab tests 1 and 2.

**Phase 2: any second, several disks, bookmarks (about 6 PRs).** Batches across disks, freeze bookmarks, `failover points|plan --at|--bookmark`, rewind from the undo journal, retention tiers. Lab tests 3 and 4.

**Phase 3: failures (about 5 PRs).** Memory and disk buffers, `degrade` and `block`, catch-up, migration and restart handling for Tap 1 (seal, compare pass), alerts. Lab tests 5, 6, 7, 8.

**Phase 4: operations (about 5 PRs).** Policy webhook, console page and picker, protection groups, failback, docs, release test stage. Lab tests 9, 10, 12.

**Phase 5: Tap 2 (about 10 PRs, only if chosen).** The FUSE tap, relay changes (epochs, dirty-region record), `shared` spool, soak tests. Lab tests 6, 7 and 11 again.

**Phase 6 (optional): synchronous mode within a region.** Same-region RPO 0 for the zone-loss design.

## Open items to verify in the lab

- `sync=none` with `write-blocking` in QEMU 9.1, and the QMP options for NBD reconnect.
- Migration with a running mirror job; `dirty-bitmaps` migration under libvirt.
- Reachability of the node relay from VM pods (UDN, NetworkPolicy).
- Red Hat support position for a CSI driver that serves files through a user-space mount.
- OpenShift Virtualization 4.22 Technology Preview list (changed block tracking) and the gate name.
- Whether IBM file share replication is 15 minutes or hourly (the pages disagree).
- Cross-region link throughput and data transfer cost.
- Node disk encryption on ROKS for the node-local spool.

## Decisions I made for you

- The goal is a journal of every write, shipped within seconds, with recovery to any second in a window. Periodic copies cannot do it.
- The default window of one-second recovery points is 4 hours. After that, 5 minute points to 24 hours. You can change both per policy.
- Recovery points are the ends of 1 second batches. A batch is the smallest step. This keeps bandwidth lower. The batch length is a setting.
- The DR disk always holds the newest second. A failover to the newest second is fast. We pay with 3 operations at DR for each changed block.
- A relay on each source node orders the writes. It is its own DaemonSet so a driver upgrade does not cut streams.
- The QEMU mirror in `write-blocking` mode into the relay is the first tap. The guest never waits for the WAN.
- The first tap does not keep the journal through a live migration or a node crash. It needs a resync of up to hours for big disks. I say so rather than hide it.
- The design that meets the whole goal uses the file-system tap. I build it only if phase 0 says the first tap's gap is not acceptable and the file-system tap is fast enough.
- I do not build the sidecar storage daemon option. It needs the unsupported `Sidecar` gate and breaks instant snapshots and QEMU copies.
- The mirror straight to the DR site is rejected. It either lags without limit or adds the WAN round trip to every write.
- Short copies every few seconds are rejected. They freeze the guest and slow the VM too much. Periodic copies stay as the fallback.
- KubeVirt's own incremental backup is not used. It is a periodic backup, not a write stream. It may replace the periodic copy later.
- When the link is lost and the buffers are full, the VM keeps full speed and the journal pauses (`degrade`). The periodic copy takes over. A `block` setting exists for the opposite choice.
- Bookmarks come from a guest freeze every 5 minutes. Any second between is crash consistent. Databases recover from that.
- Several VMs share only bookmark IDs, not an exact second. No design can order writes across nodes.
- Journal streams need TLS 1.3 with client certificates. The old bearer token alone is not enough for this.
- The receiver keeps its small rights from PR #105. The journal adds write access to one area in the volumes it already serves.
- Periodic replication keeps running at 60 minutes as a safety net and for catch-ups.
- First release: a CDP VM may not have instant snapshots. This keeps the replica format simple.
- CDP covers VM disks only. Plain volumes stay with periodic replication.
- Rewind to an earlier second copies the disk first, as a rewind does today. A faster rewind is later work.
- CDP VMs are a separate support class. Red Hat does not support the QMP method, and the journal adds a risk to live migration.
- Same-region RPO 0 (a synchronous mode) is an extension in phase 6, not part of this build.
- The size is about 25 PRs for the first tap and 10 more for the second. Phase 0 comes first and may change it.
