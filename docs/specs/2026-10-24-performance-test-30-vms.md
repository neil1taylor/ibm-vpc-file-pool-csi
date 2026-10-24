# Spec: how many general-purpose VMs one share carries

Date: 2026-10-24. Status: draft for review. It answers issue #118 and gives the numbers behind the share alerts of issue #117 (closed).

## Goal

An administrator can look up, for each share speed (IOPS tier), **how many everyday VMs one share carries before users notice**, and what the share's IOPS limit and delay look like at that point. The answer is a sizing table in `docs/CAPACITY-PLANNING.md` and `docs/PERFORMANCE-TUNING.md`, with the measurements behind it.

"Everyday VMs" means a general-purpose mix of Linux and Windows VMs, not databases: low average load, short bursts at boot, patching and backup scans.

## What exists today (from the repo and the lab clusters)

- **The `/bench` skill** runs `fio` sequential and random tests on one plain pool. It does not use VMs.
- **Release test stage 17** runs five 60-second `fio` tests in three places: a pod on a pool volume, a pod on IBM's own 3000-IOPS class, and inside one VM on a pool disk. It writes `speed.csv`, keeps a history, and warns (does not fail) when a result is 20% worse than the last run.
- **Share speed rules.** IBM's `dp2` table is kept in `pkg/ibmcloud/share_profiles.go`, and the pool admission check refuses a bad size and IOPS pair (issue #37):

  | Share size (GB) | IOPS allowed |
  |---|---|
  | 10 to 39 | 100 to 1,000 |
  | 40 to 79 | 100 to 2,000 |
  | 80 to 99 | 100 to 4,000 |
  | 100 to 499 | 100 to 6,000 |
  | 500 to 999 | 100 to 10,000 |
  | 1,000 to 1,999 | 100 to 20,000 |
  | 2,000 to 3,999 | 200 to 40,000 |
  | 4,000 to 7,999 | 300 to 40,000 |
  | 8,000 to 15,999 | 500 to 64,000 |
  | 16,000 to 32,000 | 2,000 to 96,000 |

- **Measurements the driver already makes.** The node agents read the NFS client counters of each share and publish `vpc_file_pool_share_nfs_operations_total` and `vpc_file_pool_share_nfs_execute_seconds_total` (by pool, share and operation). Two alerts use them: `VPCFilePoolShareNFSLatencyHigh` (more than 0.1 s per request on average for 10 minutes, judged only above 1 request a second) and `VPCFilePoolShareIOPSHigh` (off by default). Allocation time has its own histogram.
- **Room in the lab.** `rovslab-dr-eude` has 3 workers of about 96 vCPU and 389 GB each and no pools in use. `rovslab-prod-eude` has 3 workers of about 64 vCPU and 512 GB each, and holds other tests.
- **A Windows Server 2022 test VM** was already run on the lab (instant snapshot tests), so a Windows image exists. How it is licensed is not recorded here.

## The gap

No test runs many VMs at once, and no test uses a Windows VM under load. So the repo cannot say how many VMs fit on one share. The guides give an IOPS model and advice by volume count and size, but no measured number of VMs per share.

## Design that meets the goal

### What is measured

For each IOPS tier, one pool with one share carries a growing number of VMs. For each step the test records:

- **Per VM, from inside the guest:** IOPS, throughput, and p50, p95 and p99 latency, from `fio` on Linux and `diskspd` on Windows.
- **Per share, from the node agents:** NFS operations a second and average latency a request, all nodes together, with the share's IOPS limit beside them.
- **Stalls:** a stall is any I/O that takes more than 5 seconds, or a guest log line that reports a blocked disk.
- **Driver:** time to allocate each volume, time for each VM to boot and report its guest agent, time to take a snapshot, time for a backup.

### The load profile (one per VM)

- Average 20 to 50 IOPS, set with a rate limit so it does not depend on the VM's speed.
- Bursts of about 300 IOPS for 30 to 60 seconds every 10 to 15 minutes, which stand for boot, patching and backup scans.
- 70% reads, 30% writes, block sizes mixed from 4 KB to 64 KB.
- Each VM gets a different start time, so bursts do not line up except in the patch window.

### The mix of VMs

Two thirds Linux (CentOS Stream 9, 1 vCPU, 2 GB) and one third Windows Server 2022 (2 vCPU, 4 GB), at every step. So 30 VMs is 20 Linux and 10 Windows. All disks are on the one share under test.

### The tiers

Three runs, one for each share speed. Each uses a fresh pool with one share of 3,900 GB, made and deleted for the run. Only the IOPS changes, so the three results compare like with like. IBM allows 200 to 40,000 IOPS at that size (the 2,000 to 3,999 GB row). The disks of 90 VMs need about 3.2 TB: a copy of a catalog disk must be 30 GiB, so each Linux VM needs 32 GiB with its load disk and each Windows VM about 42 GiB. (The first draft gave each tier its own smaller share. That was wrong: 90 VMs do not fit on 200 GB, or on 3,000 GB.)

| Tier | IOPS set |
|---|---|
| Low | 3,000 |
| Middle | 10,000 |
| High | 20,000 |

### The steps in one run

1. Start 15 VMs. Wait until all report their guest agent. Run the profile for 45 minutes.
2. Add VMs up to 30. Run 45 minutes. At this step, also run:
   - **Boot storm:** stop all 30 VMs and start them together. Record how long until the last one is ready.
   - **Patch window:** all 30 burst at 300 IOPS together for 10 minutes.
   - **Mixed work:** while the load runs, live-migrate one VM, take one snapshot, and run one backup.
3. Add VMs up to 45, 60, 75, 90, with 45 minutes at each step. Stop at the **knee** or at 90 VMs, whichever comes first.
4. At the last step reached, repeat the boot storm.
5. Delete every VM, the pool and the share. Check in the VPC that the share is gone.

### The knee

The knee is the first step where **any one** of these is true for 5 minutes in a row:

- p95 read latency above 20 ms on more than a quarter of the VMs,
- any stall,
- the boot storm takes more than twice as long as the same storm at 15 VMs,
- the share runs at its IOPS limit and requests queue.

The recommended count in the sizing table is 80% of the knee, rounded down to a whole step.

### Where it runs

On `rovslab-dr-eude`. It is idle, has the room, and a mistake there does not touch the other tests on `rovslab-prod-eude`. The release test's name sweep (everything labelled with the run ID) cleans up at the end, the same way as the release run.

### What gets built

A folder `test/perf/` with: a script that runs one tier; the Linux and Windows VM definitions with the load profile in them (cloud-init for Linux, the prepared image for Windows); a collector that gathers the guest results and the node agent metrics; and a report step that writes the table and a CSV. It reuses the release test's library (labels, waits, sweep) and the `fio` code of stage 17. The test is started the same way, from the build server, so the laptop can sleep.

### The output

- `docs/CAPACITY-PLANNING.md` and `docs/PERFORMANCE-TUNING.md`: a table of VMs per share for each tier, the knee, the recommended count, and the p95 and p99 latency and the share IOPS used at the recommended count.
- The raw results of each run: a CSV and a short summary kept in `docs/perf/`.
- A note on whether the 0.1 s threshold of `VPCFilePoolShareNFSLatencyHigh` fires before the knee, with the numbers. Any change to the alert is a separate pull request.

## Tests of the test

- Unit tests (Bash, like the other `test/` scripts) for the report step with made-up results: the knee rule, the 80% rule, and the table.
- A smoke run first: 5 VMs on the low tier for 10 minutes, to prove the tools, the collection and the cleanup before any long run.
- After every run, a check that nothing labelled with the run ID is left in the cluster or the VPC.

## Build plan: phases, one PR each

1. **Tooling and smoke run.** The VM definitions, the load profile, the collector, the report step and its tests; the 5-VM smoke run.
2. **First full run:** the middle tier. Review the knee rule against what the data shows. Adjust the rule in the spec if it is wrong.
3. **The low and high tiers.**
4. **Documentation:** the sizing table in the two guides, the raw results, the alert note.

## Does the design meet the goal?

Yes. It finds the knee for each tier by growing the count until users would notice, up to 90 VMs, and it includes the Windows VMs, the boot storm, the patch window, and a migration, a snapshot and a backup under load. Three limits are stated, not hidden:

- At the assumed profile, the high tier may not reach a knee by 90 VMs (90 VMs at 50 IOPS average is 4,500 IOPS; their bursts together are 27,000). If so, the table says "more than 90 at this tier", not a made-up number.
- The result applies to this profile. A heavier VM mix (databases) needs its own run.
- Cloud storage speed varies from hour to hour. The first full run is repeated once at its knee step, and both results go in the table.

## Cost, effort and risk

- **Effort:** about 3 to 5 days to build the tooling and the smoke run. Each full run is about 8 hours of lab time, so the three tiers are about a day of running, which can go overnight.
- **Cost:** three shares of 3,900 GB for a few hours each, deleted after. The VMs use the lab cluster's spare room.
- **Risk:**
  - The Windows image: if it cannot be used for a test like this (licence or no `diskspd`), the Windows third of the mix is replaced and the table says so. Today no Windows catalog image is ready on `rovslab-dr-eude`, so the Windows part needs an image brought in first. Phase 1 builds the Linux half; the Windows half waits for a decision on the image.
  - A leftover VM or share after a failed run costs money until it is deleted. The sweep and the final VPC check guard against it.
  - Results from a shared cloud backend are noisy. The repeat run and the written knee rule limit this.

## Phase 1 results (smoke runs on rovslab-dr-eude, 2026-10-24)

Five small Linux VMs on a 200 GB, 3,000 IOPS share, 10 minutes of measuring. It took four runs to get a good one. What each run found:

1. The VM definition used a cloud-init field name KubeVirt does not know (`secretRef` is right).
2. A copy of a catalog disk must be at least as large as the catalog disk, 30 GiB. This changed the share size in this spec (3,900 GB).
3. `fio` counted every I/O of a job with a block size range as the smallest block size, so the load came out about 8 times too light (35 IOPS for a target of 300). The load is now three jobs with fixed block sizes. After the fix the VMs hit their targets: about 28 steady and 298 burst IOPS against 30 and 300.
4. The node agents' `/metrics` answered HTTP 500 when a node held the same share more than once, so the share counters read zero. This was a driver bug, fixed in PR #246 and in the `main-55211050` build that DR runs.

The good run: all 5 VMs ready in about 4 minutes (including the disk fill), no stall, no slow VM, no VM short of its rate, and the cleanup left nothing.

**Open, to settle before the first full run.** The share counters showed 982 NFS operations a second at 532 ms average, while the VMs reported about 359 IOPS and a p95 of about 1.3 ms. The two sides do not agree. Possible causes: a host cache between the guest and the share (the disk cache mode), the root disks' own traffic on the same share, or write-behind queueing in the NFS client that the guests never wait for. Every run now records the first VM's disk definition (`vm1-disks.txt`) to answer the first. If a cache is the cause, the guest latency is flattered and the 20 ms rule must be applied to the share-side latency, or the cache must be bypassed.

**Windows VMs (built, not yet run on the cluster).** The image is Windows Server 2022 Datacenter Evaluation, copied from `rovslab-prod-eude` (`brs-win-image/win2k22-brs`) to `rovslab-dr-eude` (`openshift-virtualization-os-images/win2k22-perf`). `test/perf/prepare-windows-image.sh` prepares it once: it installs fio for Windows (3.43, from the fio project's release, checked against its checksum), sets the clock to UTC, and activates the evaluation licence (online it gives 180 days; without internet it resets the grace period). `run-tier.sh` makes every third VM Windows (`--windows-every`, 0 for none). Each Windows VM gets a config disk from a ConfigMap with the fio arguments of its fill and its two phases, built by the same code as the Linux jobs, and `windows/load.ps1`, which fills the test area once, says PERF-READY and prints the same result line as a Linux VM. The run starts each Windows VM's load through the guest agent. The Windows VMs share one computer name and machine identity (the image is not sysprepped); that has no effect on a disk test. The first run is one Windows VM alone: `--steps 1 --windows-every 1`.

Decisions made while building it:
- fio, not DiskSpd, on Windows. DiskSpd 2.2 and 2.3 both refused every job file on this Windows build ("failed to load profile, line 0, errorCode 80070057"), even one DiskSpd wrote itself, and its command line cannot give each block size its own rate. fio runs the exact Linux jobs, so the two kinds of VM compare like with like.
- The run starts the load through the guest agent, not a Windows start-up task. The task stayed "Queued" and never ran, even when started by hand.

**Not built yet:** repeat runs and a steady-state check, the baseline and event windows, the boot storm and the other planned events. They are items 5, 7, 9 and 12 of the research file `docs/research/2026-10-24-storage-benchmark-tools.md`.

## Decisions I made for you

- The real VM mix and peaks are not known, so I used the issue's defaults: 20 Linux and 10 Windows, 20 to 50 IOPS average, 300 IOPS bursts, 70/30 read/write. Change them here and the test follows.
- "Users notice" means p95 read latency above 20 ms, or any stall over 5 seconds, or a boot storm twice as slow as at 15 VMs.
- The recommended count is 80% of the knee.
- Three tiers: 3,000, 10,000 and 20,000 IOPS, all on a 3,900 GB share, so only the IOPS differs.
- The ramp goes 15, 30, 45, 60, 75, 90 and stops at the knee or 90.
- The test runs on `rovslab-dr-eude`, not on `rovslab-prod-eude`.
- The Linux VMs are small (1 vCPU, 2 GB) and the Windows VMs are 2 vCPU, 4 GB, so the disk is the thing being tested, not the CPU.
- The tool lives in `test/perf/` and results in `docs/perf/`.
- Alerts are not changed by this work; it only reports whether the 0.1 s threshold fits.
