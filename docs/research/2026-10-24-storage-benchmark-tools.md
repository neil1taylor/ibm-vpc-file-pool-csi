# Storage and VM benchmark tools compared with our performance test

Date: 2026-10-24

Question answered: how does our planned test (many everyday VMs on one NFS share, find the "knee", recommend 80 percent of it) compare with mature storage and VM benchmarks, and what should we change?

## How to read this file

- Every claim has a source URL beside it. All sources are the tool's own documentation, source code, specification, or the vendor's own pages. I read them directly unless the text says otherwise.
- "Not confirmed" means I could not find a primary source. I do not state it as fact. All such items are also listed in the last section.
- Terms are defined once, where first used.
- Our design is in `docs/specs/2026-10-24-performance-test-30-vms.md` and `test/perf/` on the branch worktree. At the time of reading, `run-tier.sh` only implements the steady-state ramp. It has no boot storm, patch window, migration, snapshot, or backup step yet, and no Windows (diskspd) profile. The comments below on those parts are about the spec.

## 1. Summary table

| Tool | What it is for | How it defines load | How it finds capacity or pass/fail | Source |
|---|---|---|---|---|
| fio | General I/O generator (Linux and others) | Job file: block size, read/write mix, queue depth, optional rate limit (`rate_iops`) | None built in. `latency_target` can search for the highest load under a latency limit. `steadystate` can stop a job when a metric is stable | https://github.com/axboe/fio/blob/master/HOWTO.rst |
| diskspd | Microsoft I/O generator for Windows | Command line: block size, write percent, threads, outstanding I/O, throttle `-g` (IOPS form `-g<n>i` since 2.1) | None built in. Has `-W` warm-up and `-C` cool-down | https://github.com/microsoft/diskspd/wiki/Command-line-and-parameters and https://github.com/microsoft/diskspd/blob/master/README.md |
| HCIBench (VMware) | Wrapper that deploys many VMs and runs Vdbench or fio in each, for hyper-converged storage | Number of VMs, disks per VM, threads per disk, or a parameter file. "Easy Run" picks the counts itself | Easy Run profiles try to hold p95 latency under 1, 3 or 5 ms for small random I/O. Otherwise the user compares runs | https://github.com/vmware-labs/hci-benchmark-appliance/blob/main/HCIBench_User_Guide.pdf |
| VMmark 4 (VMware) | Cluster benchmark. A "tile" is one fixed group of application VMs | Add tiles to add load. Infrastructure operations (vMotion, clone and deploy) run alongside | Score is peak load with quality of service met. Exact thresholds: not confirmed | https://blogs.vmware.com/cloud-foundation/2024/05/09/introducing-vmmark-4/ |
| SPECvirt Datacenter 2021 | Datacenter VM benchmark, tile based | Add tiles. Phased run of 90, 30 and 60 minutes | Peak is where another tile fails quality of service in any tile, or does not raise the score. Per-workload p90 and p95 response limits | https://www.spec.org/virt_datacenter2021/docs/designoverview.html and https://www.spec.org/virt_datacenter2021/docs/runrules.html |
| SPC-1 | Block storage benchmark for business applications | Requested I/O rate, split in fixed streams. One "stimulus scaling unit" is a user group that makes up to 500 IOPS | Valid only if average response time is at most 30 ms in every phase. Needs an 8 hour sustain test and a repeat test | https://storageperformance.org/sites/default/files/files/specifications/SPC1_v391_final.pdf |
| SPECstorage Solution 2020 | File-server (NFS and SMB) benchmark | A "business metric" per workload (builds, video streams, and others). LOAD, INCR_LOAD, NUM_RUNS give a ramp | Curve of business metric against response time. Needs at least 10 load points in one run. Latency pass rule: not confirmed | https://www.spec.org/storage2020/docs/usersguide.pdf and https://www.spec.org/storage2020/docs/runrules.pdf |
| SNIA SSS PTS 2.0.2 | Test method for one solid state drive | Prescribed workloads in "rounds" | Steady state: in a 5 round window, range under 20 percent of the mean and slope under 10 percent | https://www.snia.org/sites/default/files/2025-02/SNIA-SSS-PTS-2.0.2.pdf |
| KubeVirt perf and scale tests | Control plane tests (VM creation latency, API load) | Create 100 minimal VMIs with 100 ms between each | P50 and P95 creation-to-running latency. No disk I/O | https://github.com/kubevirt/kubevirt/blob/main/docs/perf-scale-benchmarks.md |
| kube-burner | Tool to create and measure Kubernetes objects at scale, including VMs | Job files that create objects | Measurements include `vmiLatency` and `pvcLatency`. Red Hat's VM-and-storage example boots VMs only and runs no I/O inside them | https://kube-burner.github.io/kube-burner/latest/measurements/ and https://developers.redhat.com/articles/2024/09/04/use-kube-burner-measure-red-hat-openshift-vm-and-storage-deployment-scale |
| IBM File Storage for VPC (dp2) | The system under test | IOPS limit set per share, by share size | IBM publishes limits, not latency expectations | https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-profiles |

### Facts behind the table

fio:
- `rate_iops` caps IOPS. With a block size range, "the smallest block size is used as the metric" (HOWTO, `rate_iops`).
- `rate_process` default is `linear`: "fixed delays between I/Os that gets adjusted based on I/O completion rates". `poisson` models a random request flow (HOWTO, `rate_process`).
- `ramp_time` runs the workload "before logging any performance numbers" (HOWTO, `ramp_time`).
- `steadystate` plus `steadystate_duration` use a rolling window and stop the job when a criterion (iops, bw, lat, and their slopes) falls inside a limit. Default duration 0 means off (HOWTO, steadystate options).
- `clat_percentiles` reports completion latency percentiles. `lat_percentiles` reports total latency (submission plus completion). `percentile_list` sets the percentiles (HOWTO, "Measurements and reporting").
- `direct=1` is non-buffered I/O, usually `O_DIRECT` (HOWTO, `direct`).
- `randrepeat` seeds the random generators "in a predictable way so the pattern is repeatable across runs". Default: true (HOWTO, `randrepeat`). `randseed` sets the seed.
- `write_hist_log` and `log_hist_msec` write latency histograms over time (HOWTO, `write_hist_log`).
- `slat`, `clat`, `lat`: `lat` is "the time from when fio created the I/O unit to completion" (https://fio.readthedocs.io/en/latest/fio_doc.html, section "Interpreting the output").

diskspd:
- `-W` warm-up (before measurement), `-C` cool-down, `-d` duration, `-L` latency statistics, `-D` IOPS statistics interval (text output gives standard deviation, XML gives the series), `-Sh` is the same as `-Suw` (unbuffered and write-through), `-w` write percent, `-g<n>i` throttle in IOPS (wiki; README for version 2.1).
- The README says the latency histogram "now reports to 9-nines" in text and XML output (version 2.0.17a note).

HCIBench:
- Easy Run and the three latency profiles, and the general-purpose workloads "4K, 70% Read, 100% Random" and "8K, 50% Read" (guide, "Easy Run" section).
- Start points: for best IOPS, 4 VMs per ESXi host, 8 data disks per VM, 4 threads per disk. For best latency, 2 VMs per host, 4 disks, 1 thread (guide, "Best Practice" in the test configuration section).
- "We strongly recommend selecting to Prepare Virtual Disk Before Testing to allow blocks being allocated to avoid first write penalty as well as the read served by memory only" (guide, test configuration best practice).
- "Clear Read/Write Cache Before Each Testing" is only for vSAN OSA, a two-tier design (guide).
- A "Testing Duration" field overrides the elapsed time in the parameter files (guide).
- Mixed Workload Mode (version 3.0 and later) runs groups of VMs with different parameter files at the same time (guide).
- Disk initialisation options ZERO, RANDOM, NONE: https://blogs.vmware.com/cloud-foundation/?p=24181

SPECvirt Datacenter 2021:
- Scaling is "an increasing number of tiles. Peak performance is the point at which the addition of another tile (or fraction of a tile) either fails the Quality of Service (QoS) criteria in any tile or fails to improve the overall metric" (design overview, 2.2.2).
- Measurement interval is three phases: initial load 90 minutes, host provisioning 30 minutes, final load 60 minutes (design overview, 2.5).
- Mail server: 90 percent of response times at most 150 ms and 95 percent at most 170 ms. Web server 190 and 210 ms. Collaboration server 385 and 425 ms. This applies "for every tile" (run rules, 4.4.1.1). These are application transaction times, not disk times.
- HammerDB needs at least 200 new-order transactions a second in every tile (run rules, 4.4.1.2).
- The older SPECvirt_sc2010 used a tile of six VMs, one of them idle "to represent VMs in a typical user environment which are not running at full capacity" (https://research.spec.org/icpe_proceedings/2012/p251.pdf).

SPC-1 (v3.9.1):
- The spec uses "stimulus scaling unit" (SSU), not "BSU". One SSU is "a community of users who collectively generate up to 500 I/O's per second" (clause 1.2.2). "BSU" was the older name. I did not check older versions.
- Average response time is computed per test phase "and cannot exceed 30 milliseconds. If ... exceeds that 30-millisecond constraint for any TEST PHASE, the MEASUREMENT is invalid" (6.3.2.4).
- Phases (Table 6-1): sustain at 100 percent load, transition at least 3 minutes, measurement at least 8 hours. Then a response time ramp down through 100, 95, 90, 80, 50 and 10 percent and back up, each with at least 3 minutes transition and 10 minutes measurement. Then repeatability phases at 10 and 100 percent.
- Sustain throughput must be within 5 percent of the reported result (6.3.3.2). Repeat throughput must be above the reported result minus 5 percent (6.3.8.5). Repeat average response time at 10 percent load must be within 5 percent or 1 ms of the first (6.3.8.6).
- Repeat phases use "a statistically distinct sequence of I/O REQUESTS to assure that prior cache contents do not affect the results" (6.3.8.7).
- "The HOST SYSTEM(s), shall not cache or buffer any data associated with implementing the ASUs" (4.2.1.2).
- Steady state is "throughput and response time are stable and sustainable". A gradual constant drift or a sudden permanent change fails it (5.1.1).
- Response time is "COMPLETION TIME minus START TIME" (glossary).

SPECstorage Solution 2020:
- Workloads: SWBUILD, VDA, EDA_BLENDED, AI_IMAGE, GENOMICS (user guide, parameter BENCHMARK). There is no VDI workload in this version. (The older SPECsfs2014 had one; I read that only in a secondary news article, so it is not confirmed.)
- Load ramp: set LOAD, INCR_LOAD and NUM_RUNS (user guide, section 3). Auto mode `-a` "finds maximum passing load value" (user guide, command line list). The pass rule behind "passing": not confirmed.
- Run rules 5.3: at least 10 load points in one run, spaced evenly within 30 percent of nominal. "Any invalid data points will invalidate the entire run." Run rules 5.4: overall response time is the area under the curve divided by peak throughput.
- Run rules 5.2: cloud storage components "should be (re-)initialized to the fullest extent possible and documented".
- User guide: "Ensure that your test network is idle ... Results obtained on production networks may not be reproducible."
- The guide's sample output shows "Test run time = 300 seconds, Warmup = 300 seconds" and a latency band histogram. I did not confirm these are defaults.

SNIA PTS 2.0.2 (section 2.1.24): steady state needs both (a) range of the tracked value within the measurement window at most 20 percent of its average, and (b) slope of the best linear fit changes the value by at most 10 percent of the average. The measurement window is 5 rounds (2.1.13). Pre-conditioning is writing data to bring the device to steady state, either with an unrelated workload (workload independent) or the test workload itself (workload dependent) (2.1.18). The spec is for single SSDs. I use it only for the generic ideas.

KubeVirt and Red Hat:
- KubeVirt's benchmark doc "doesn't mention storage or disk I/O": it covers creation latency (P50, P95), CPU and memory of KubeVirt components, and API call counts.
- kube-burner lists `vmiLatency`, `pvcLatency`, `dataVolumeLatency`, `volumeSnapshotLatency` among its measurements.
- Red Hat's 2024 article uses kube-burner to boot 10 namespaces of 50 VMs that clone a boot disk, and reports `vmiLatency` percentiles. It says the workload "only boots the VMs and waits for them to reach Running" and treats I/O inside the VMs as a possible future step.
- Red Hat has a blog post and reference architecture about OpenShift Virtualization performance and boot storms. The search snippets mention them, but the page itself returned HTTP 404 when I fetched it, so I treat it as not confirmed.

IBM File Storage for VPC (https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-profiles):
- The dp2 table of share size against IOPS range matches `pkg/ibmcloud/share_profiles.go` as quoted in our spec.
- "IOPS values are based on a 16 KB block size for all profiles, with a 50-50 random workload. Each 16 KB of data that is read or written counts as one read/write operation. A single write of less than 16 KB counts as a single write operation."
- dp2 maximum bandwidth is IOPS times 256 KB, capped at 8192 Mbps.
- "For the 96,000 IOPS to be realized, a single file share must be accessed by multiple virtual server instances. A single file share that is accessed by one client is limited to 48,000 IOPS."
- "A single session can achieve up to 64 KB block size transfer. To use the max allowable bandwidth, you need multiple concurrent sessions to the share."
- Mounting needs NFS 4.1 or higher, and `sec=sys` (https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-mount-RHEL).
- Encryption in transit "can decrease performance" and workloads that bypass VSI caching "might have a substantial performance impact" (https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-planning).
- I found no IBM page that states latency expectations or a benchmark method for File Storage for VPC.

## 2. Gap analysis

Each item gives: what mature tools do, what we do, whether it matters for our goal, and the change. Our goal is a sizing table of how many everyday VMs one NFS share carries before users notice.

### Gap 1. Warm-up and steady-state detection

- Mature tools: SPC-1 forces a transition of at least 3 minutes before each 10 minute measurement and an 8 hour sustain (Table 6-1). fio has `ramp_time` and `steadystate`. diskspd has `-W`. SNIA defines steady state with a 20 percent range and 10 percent slope test. HCIBench tells you to write the disks first (see gap 3).
- We do: a 5 minute settle before each 45 minute window (`run-tier.sh`, `SETTLE_MIN`). There is no check that the window was stable. The settle is a fixed time, not a test. Each fio run is a new process with no `ramp_time`, so the first seconds of every phase include start-up.
- Matters: yes, moderately. New VMs added at each step boot, write to their disks, and may still be copying clone data. A drifting step gives a wrong p95.
- Change (small): after collection, test each step for stability using the SNIA idea. Split the window into 5 minute bins, take the share-level IOPS and average latency per bin, and mark the step "not stable" if the range exceeds 20 percent of the mean or the slope exceeds 10 percent. Report unstable steps and do not use them for the knee unless repeated. Add `--ramp_time=30` to each fio call so the first seconds are not counted (small; note the HOWTO says ramp time adds to runtime).

### Gap 2. Run length and number of repeats

- Mature tools: SPC-1 runs each point 10 minutes but demands an 8 hour sustain and two repeats (6.3.3, 6.3.8). SPECvirt Datacenter measures for 3 hours in three phases. SPECstorage needs at least 10 load points in one run and voids the run if one is invalid. Repeat tolerance in SPC-1 is 5 percent.
- We do: 45 minutes a step, one pass per tier. The spec repeats "the first full run ... once at its knee step".
- Matters: yes. Cloud storage speed varies by hour (our own spec says so). One sample per step cannot show whether a difference is real. Our steps are also coarse: 15, 30, 45, 60, 75, 90. A knee at 45 gives a recommendation of 36, rounded down to 30. That is a large rounding loss.
- Change (medium): (a) after the first knee, add one step halfway between the last good step and the knee (for example 37 or 38) so the answer is not a 15 VM guess. (b) Repeat the knee step and the step below it, at least once more, on a different hour. Accept the knee only if both runs agree. Use an SPC-1-like band (for example p95 within 10 percent or 2 ms). (c) At the end of the ramp, run the 15 VM step again and compare with the first 15 VM step. This is the SPC-1 repeatability idea (6.3.8.6) and it detects backend drift during the day.

### Gap 3. Working set versus cache (is 2 GB per VM enough?)

- Mature tools: SPC-1 forbids host caching of the test data (4.2.1.2) and uses a different request sequence on repeat phases (6.3.8.7). HCIBench says to write every disk first so reads are not "served by memory only", and offers cache clearing on vSAN OSA. SPECstorage says to re-initialise cloud components "to the fullest extent possible". I did not find a primary statement of a required ratio of working set to cache size in any of them. HCIBench's working-set setting: not confirmed.
- We do (`profile.sh`): `--direct=1` and `--size=2G` on the load disk, with `--ioengine=libaio`. No pre-write of the area. No `randrepeat=0`, no `randseed`.
- What this means:
  1. `direct=1` removes the guest page cache (HOWTO, `direct`). It does not remove the node's NFS client cache or any cache on the file server. Whether QEMU opens the disk file with direct I/O in our VMs: not confirmed. The VM definition has to be checked.
  2. fio's `randrepeat` default is true: the random pattern is "repeatable across runs". Each steady phase and burst phase starts a new fio process with the same options, so each process reads and writes the same sequence of offsets again, every 10 minutes. After the first period those blocks can be in a cache. At 30 IOPS average and about 34 KB average I/O size, one steady phase touches well under the 2 GB area, so every repeat revisits mostly the same early blocks. The result would be too fast, and it would be too fast more as time goes on. This is a real defect in the current `profile.sh`, not a theory, though I did not run it. I did not verify the size of the effect.
  3. Never-written regions: if the disk file is sparse, reads of unwritten blocks may be answered without reading media. How the IBM backend treats this: not confirmed. HCIBench's advice to write first covers it.
  4. Aggregate size is not the issue (90 VMs times 2 GB is 180 GB). The per-VM random area and the repeated pattern are.
- Matters: yes, strongly. It can make the share look better than it is, and the sizing table would then recommend too many VMs.
- Change:
  - (small) Add `--randrepeat=0` or a per-phase `--randseed` to every fio call. Add the same on diskspd (`-z` with a changing seed).
  - (small) Write the area once before measuring (a sequential fill with large blocks), as HCIBench advises. Measure the fill time in the smoke run, because IBM counts operations in 16 KB units.
  - (small) Cross-check cache effects in the smoke run: compare the sum of guest read IOPS with the share's NFS read operations from the node agent counters that the driver already exports. If the guests report far more reads than the share saw, a cache is absorbing them.
  - (medium) Use a larger area per VM than 2 GB (for example most of the load disk) and check by a short A/B run (2 GB against the larger area) that read latency does not change. If it changes, the larger one is the right default.
  - (small) Check the VM definition for the disk cache mode and write it in the results.

### Gap 4. Latency as a percentile or an average

- Mature tools: SPC-1 validity uses average response time (at most 30 ms) and also publishes frequency distributions and response time graphs (10.9.x). SPECvirt Datacenter uses p90 and p95 limits per workload in every tile. HCIBench Easy Run targets p95. SPECstorage reports average operation latency with latency bands in its sample output. Which of these is "right" is a choice, but percentiles are the common choice for user-visible behaviour.
- We do: p95 of read latency per VM, from fio JSON completion latency, taken from steady phases only. Per VM we use the highest p95 across the phases in the window (`perf_vm_stats`). Burst phases are excluded from the p95 and only count in the stall test. Write latency is parsed but not used in the knee.
- Matters: yes, in three ways.
  1. Taking the highest p95 across phases is a maximum of a percentile. It is sensitive to one bad phase and to the number of phases. It is not a standard statistic.
  2. Bursts are what users notice (boot, patching, scans), and the knee ignores their latency.
  3. Percentiles from separate phases cannot be combined by averaging them. Pooling needs histograms.
- Change:
  - (small) Record per-VM p95 for the steady phase and, separately, p95 and p99 for the burst phase. Apply a burst rule (for example burst p95 under a looser limit such as 50 ms) as a second condition, set in the spec.
  - (small) Use write p95 too, since 30 percent of the I/O is writes.
  - (medium) Capture a latency histogram per VM per step (`write_hist_log` with `log_hist_msec`, HOWTO) so the step p95 and p99 are computed by pooling, not by taking a maximum or an average of percentiles. For diskspd, use the XML output, which includes the histogram (README).
  - (small) Also report the share-level average NFS latency, as SPC-1 and SPECstorage do, next to the p95.

### Gap 5. Coordinated omission

Definition: coordinated omission is when a load generator slows its own requests when the system is slow, so the slow period produces fewer samples and the measured latency looks better than users would see.

- What fio's documentation says:
  - The `io_submit_mode=offload` entry says offloading "avoids skewed latency reporting if I/O gets backed up on the device side (the coordinated omission problem)", and that it "cannot reliably be used with async IO engines" (HOWTO).
  - `rate_process=linear` uses "fixed delays between I/Os that gets adjusted based on I/O completion rates" (HOWTO). That wording suggests the rate adapts to completions, but I could not confirm from the documentation what happens to the intended start time of a late I/O.
  - `lat` is measured "from when fio created the I/O unit to completion", and `clat` from submission to completion (fio_doc, "Interpreting the output"). Our parser uses `clat_ns`, which excludes any wait before submission.
- Answer to your question: the fio documentation does not say that, with `rate_iops`, latency is measured from the intended start time. It names coordinated omission as a known problem and offers `offload` mode, which it says not to use with async engines. We use `libaio`, which is an async engine. So the claim "fio with `rate_iops` measures from the intended start" is **not confirmed**, and the safe assumption is that it does not.
- diskspd: the README says only that version 2.2 "improved" latency measurement. Coordinated omission is not mentioned. Not confirmed.
- Matters: yes, for the stall and tail figures. In a near-stall, a single fio process with queue depth 4 can stop issuing, and the time it waited is not in `clat`. The p95 then looks fine while the user waits.
- Change:
  - (small) Record the achieved IOPS against the target for every phase. A shortfall (for example below 90 percent of the target) is the sign that the generator was held back. Treat a step with a shortfall as failed or unreliable. SPC-1 also requires the measured load to match the intended load within a tolerance (5.2.9).
  - (small) Also keep `lat` (total latency) by adding `--lat_percentiles=1`, and use it instead of `clat`.
  - (medium) Test the generator itself once, outside the cluster: run the same fio command against a local device that is made slow on purpose and see whether the reported p95 and achieved IOPS reveal the slowdown. This answers the "not confirmed" above for the versions we use.
  - (medium) Consider `--rate_process=poisson` for arrivals that look like real users. This does not fix omission. It only changes the spacing.

### Gap 6. How the knee or pass line is chosen

- Mature tools:
  - SPC-1: average response time at most 30 ms in every phase, and the reported figure must also hold in a sustain and in repeats (6.3.2.4, 6.3.3, 6.3.8).
  - SPECvirt Datacenter: per-workload p90 and p95 limits, and every tile must pass (run rules 4.4.1.1). One failing tile ends the run. Peak is also where the overall score stops rising.
  - HCIBench Easy Run: p95 targets of 1, 3 and 5 ms for small random I/O on flash storage.
  - VMmark: quality of service exists, thresholds not confirmed. A VMware forum post puts the VMmark 2 era DVD Store limit at 500 ms. That is a forum post, not a primary source.
  - IBM: no published latency expectation (not confirmed).
- We do: a step is a knee if more than 25 percent of VMs have p95 read latency over 20 ms for the step, or any stall, or a boot storm over twice the first, or the share at its limit.
- Assessment:
  - 20 ms p95 for disk reads is in the same range as SPC-1's 30 ms average ceiling, and stricter, since a p95 is higher than the average. It is a reasonable first value. I found no primary source that says 20 ms is the point where everyday users notice. The number stays a decision.
  - "More than a quarter of VMs" is looser than SPECvirt Datacenter's "every tile". It is also easy to change. It should be a stated parameter.
  - "Any stall" over 5 s across up to 90 VMs and 45 minutes is a very sensitive rule. One event (a live migration, a snapshot, a backup) will trigger it. Our spec plans to run those operations during the 30 VM step. Left as written, they decide the knee.
  - The boot storm rule compares the storm at N VMs with the storm at 15 VMs. A storm of 30 VMs is naturally longer than one of 15, so "twice as long" can fire from VM count alone. In `report.sh` the baseline is taken from the first row, but the spec runs the first storm at 30 VMs, so the baseline would be empty and the rule would not fire. The runner does not run a boot storm yet.
  - "Share at its limit" uses NFS operations a second compared with the configured IOPS (`run-tier.sh`, 95 percent). IBM counts one operation per 16 KB. A 64 KB I/O counts as four. With 4 KB to 64 KB blocks, the share can reach its IBM limit when the NFS operation count looks lower. Whether the NFS client's operation count tracks IBM's 16 KB unit: not confirmed.
- Change:
  - (small) Keep 20 ms, but compute the knee at 10, 20 and 30 ms and publish all three. The data already exists, so this is cheap, and it shows how sensitive the result is.
  - (small) Make the 25 percent and 20 ms values report parameters, and show also the result for "any VM over limit" (the SPECvirt style).
  - (medium) Split each step into a baseline window and an event window. Apply the knee rule only to the baseline window. Report the migration, snapshot and backup as separate deltas against the baseline. VMmark treats infrastructure operations as a separate load added to the application tiles (VMmark 4 blog), though how it scores them is not confirmed.
  - (small) Compare boot storms per VM (seconds per VM, or time to last VM against count), and run the baseline storm at the first step. Add a test to `test-perf.sh` for an empty baseline.
  - (medium) Express the share load in IBM's unit: for each fio I/O, count ceil(size / 16 KiB). Compare that with the share limit. This needs the block size mix from the fio output or a fixed known distribution.

### Gap 7. Synchronised or random bursts

- Mature tools: VMmark and SPECvirt use tiles with each workload on a predefined stress pattern. SPECvirt Datacenter says tiles "may be started in rapid succession or in delayed sequences" with delay factors (design overview, 2.5). SPC-1 spreads its load over streams at fixed intensities. I found no primary text about synchronised bursts as a design choice.
- We do: random start offsets per VM, set from the boot id (`profile.sh`), plus a separate synchronised "patch window" and boot storm in the spec.
- Matters: it is a good approach. Real peaks line up (patch, backup, boot). A random mix shows the everyday case and a synchronised one shows the worst case.
- Change (small): record the offsets (or seed them from a run seed) so a repeat run can use the same pattern or a changed one on purpose. Report the result of the synchronised phase beside the random phase, and do not mix them into one knee figure. The spec already separates them; keep it so.

### Gap 8. Does low average with bursts match published VM profiles?

- Mature tools: HCIBench's "general purpose" Easy Run workload is 4K, 70 percent read, 100 percent random, at the maximum rate the system gives (guide), and OLTP is 8K, 50 percent read. IBM rates dp2 at 16 KB blocks, 50/50 random. The SPECvirt tile includes an idle VM to represent VMs not at full load (2012 paper). SPECstorage 2020 has no VDI workload. I did not find a primary published profile for everyday desktop or server VMs in IOPS terms. The View Planner (VMware VDI benchmark) search did not return its guide, so VDI profile: not confirmed.
- We do: 20 to 50 IOPS average, 300 IOPS bursts of 30 to 60 s every 10 to 15 minutes, 70/30, 4K to 64K block mix. The spec states these are defaults taken from the issue and not measured from a real fleet.
- Matters: yes, because the answer scales almost directly with the assumed load. The result is only true for this profile. The spec says so.
- Change:
  - (small) State plainly in the sizing table that the numbers are for this stated profile, with the source "owner's defaults, not measured".
  - (medium) Measure real averages and bursts from a real cluster if one is available (guest disk counters over a week) and replace the defaults. Not a benchmark change, but the biggest effect on the answer.
  - (small) Add a sensitivity run: the same ramp at half and at double the average rate for one tier. This shows how the knee moves with load, as SPC-1 shows it with its ramp.
  - Note for balance: HCIBench-style closed-loop "maximum rate" runs would answer a different question (the peak the share gives), which our spec does not need. The rate-limited approach is the right one for "how many VMs".

### Gap 9. Per-VM and aggregate reporting

- Mature tools: SPC-1 and SPECstorage report aggregates. SPECvirt and VMmark report per tile and require every tile to pass. HCIBench reports aggregates across all VMs and keeps the per-VM files (guide).
- We do: both: per-VM p95 (share of VMs slow) and share-level NFS IOPS and average latency from the node agents.
- Matters: this is a strength. One missing piece is a reconciliation: the sum of guest IOPS should match the share's NFS operations after allowing for block splitting.
- Change (small): write both numbers in `steps.csv` and flag a step where they differ by more than a set margin. This also feeds gap 3.

### Gap 10. Variance and confidence

- Mature tools: SPC-1 states a 5 percent repeatability band. SPECstorage run rules mention a margin of error of 1 percent (peak business metric) and 5 percent (response time) for later general-availability results and say the margins "may be larger for black-box cloud publications" (run rules, item 5). SNIA uses the 20 and 10 percent steady state tests. None of the sources I read gives confidence intervals.
- We do: no variance reporting. One run per step, one repeat at the knee.
- Matters: yes. A sizing table without a spread invites false precision.
- Change (medium): for each step record the spread across VMs (median, p90 and maximum of the per-VM p95), and for the knee step and the one below it, the result from at least two runs. Publish the range (low and high) in the table beside the recommended count. Agree a repeat band up front (SPC-1 uses 5 percent; a cloud share will need wider, so choose 10 percent and write it down).

### Gap 11. Noisy neighbours on shared cloud storage

- Mature tools: SPECstorage says an idle network and idle solution are needed, and says results on shared production networks "may not be reproducible". Its run rules allow larger margins for cloud and require documenting how cloud parts were re-initialised. SPC-1 requires the sponsor to do nothing between phases and not restart the system (6.3.2.6, 6.3.2.7). None offers a method for measuring a shared backend you do not control.
- We do: the lab cluster is idle, which is good for the compute side. The IBM storage backend is shared and not controllable. The spec says to repeat the knee step.
- Matters: yes. A noisy period during one step could move the knee by a whole step.
- Change:
  - (medium) Add a fixed probe: a small fixed-rate fio job on a separate small share (or a fixed VM on the same share, low rate) that runs for the whole test, with the same settings at every step. If the probe's p95 changes by more than the repeat band without a change in our load, the step is tagged "backend noise" and repeated. This is my design, not a method from a source.
  - (small) Record the time of day, the zone, and the share's IOPS setting for each step. Start all three tiers at similar times of day.
  - (small) The 15 VM repeat at the end (gap 2) is the cheap version of the same check.

### Gap 12. NFS client behaviour and concurrency

- IBM's page says a single session reaches only a 64 KB transfer size and that full bandwidth needs "multiple concurrent sessions to the share". It also says one client is limited to 48,000 IOPS on a 96,000 IOPS share. I found no IBM advice on mount options for performance beyond `sec=sys`, NFS 4.1 and `_netdev`, and no advice on `nconnect` (not confirmed).
- We do: the driver mounts each share once per node, so all VMs on a node share one NFS client mount. Three workers carry the whole load. The high tier is 20,000 IOPS, below the 48,000 single-client limit, but each node is one client.
- Matters: the node mount may be a limit before the share is. If one node's single session saturates, the knee is a driver or client limit, not the share. This is useful to know but must not be mixed up with the share limit.
- Change (small): add per-node NFS counters to the report (the node agents already export per-share counters). Check whether load is spread evenly across the three nodes. Note in the results which limit was reached. Record the mount options in the results.

### Gap 13. VM-level scale tooling

- KubeVirt's own test and the Red Hat kube-burner example measure creation latency and the control plane, not guest disk I/O. The Red Hat article calls guest I/O a possible future step. Our test fills that gap, but we also should use their measurements for the boot side.
- Change (small): for boot storm timing, use the same measure as kube-burner's `vmiLatency` (VM creation to ready, phase by phase), so numbers can be compared with Red Hat's published method. The spec already measures "time for each VM to boot and report its guest agent". Keep it and record percentiles (P50, P95, P99), as KubeVirt and kube-burner do.

## 3. Where we are at least as good

- **Rate-limited, per-VM load.** Most storage benchmarks push for maximum rate. HCIBench's Easy Run does this. Ours holds each VM at a fixed low average, which answers "how many VMs" directly. SPC-1 and the SPEC tile benchmarks also use fixed intensity, so this matches the mature method.
- **Real VMs in the real stack.** HCIBench runs on VMs on a hypervisor, and so do we, through the CSI driver, NFS and KubeVirt. The KubeVirt and Red Hat tools I found boot VMs but run no guest I/O (Red Hat article).
- **Mixed guest operating systems.** HCIBench Easy Run and SPC-1 use one kind of generator. Our plan mixes Linux (fio) and Windows (diskspd) and keeps the same share. Windows is not built yet.
- **Boot storm, patch window, and live operations together with steady load.** VMmark 4 includes infrastructure operations; SPC-1 and SPECstorage do not have a counterpart. The KubeVirt tools do not test them against disk load.
- **Per-VM pass counting.** A share-wide average can hide a few suffering VMs. Counting VMs over the limit is closer to what users feel than SPC-1's average.
- **Both guest and share measurements.** We have the node agent counters, a second independent view that most guest-only tools (HCIBench's fio runs) lack.
- **Stall rule.** A 5 s stall rule catches freezes that p95 hides. SPC-1 and SPECvirt have no stall rule in the clauses I read.
- **Cleanup and cost control.** The planned sweep and VPC check match the care SPECstorage asks for in re-initialising the system.

## 4. Where we are weaker

- **Cache and repeated patterns.** The fio default `randrepeat=true` repeats the same offsets every phase (gap 3). No data pre-write. Cache mode of the VM disk unchecked. This is the most important weakness because it makes results optimistic.
- **Latency measured without correction for waiting.** We use completion latency with a rate-limited async generator and no shortfall check (gap 5).
- **Stability and repeats.** No steady state test (gap 1). One pass per step, with a repeat at the knee only (gap 2). SPC-1 requires 8 hour sustain and repeats. SPECvirt measures 3 hours. Our run is 8 hours for the whole ramp, with less than 1 hour at the answer.
- **Rule design.** The knee rule mixes planned events with steady load (gap 6). The boot storm baseline is inconsistent between the spec and `report.sh`. The share-limit test uses a different unit from IBM's (gap 6).
- **Burst latency is not judged.** Only steady phases set the p95.
- **No spread or confidence reported** (gap 10), and no noisy neighbour control (gap 11).
- **Step size is coarse** near the answer (gap 2).
- **Profile not measured from a real fleet** (gap 8). This is the largest uncertainty in the final number, and no benchmark design removes it.
- **Windows side not built.** diskspd needs `-g<n>i` (version 2.1 or later), `-W`, a changing seed, and unbuffered, write-through I/O (`-Sh`). Whether diskspd's default is buffered: not confirmed from what I read.

## 5. Recommended changes, in order of value

Size: small is under half a day, medium is one to two days, large is more than two days. The sizes are my estimates.

1. **Stop the repeated random pattern** (add `--randrepeat=0` or a new `--randseed` per fio call; the same for diskspd). Small. Gap 3.
2. **Write the test area once before measuring, and check cache effects** (fill step, compare guest reads with share NFS reads in the smoke run, record the VM disk cache mode). Small. Gap 3.
3. **Record achieved against target IOPS per phase and use `lat` (total latency) not only `clat`.** Treat a shortfall as an unreliable step. Small. Gap 5.
4. **Count share load in IBM's 16 KB units** and use that for the "at limit" test. Small to medium. Gap 6.
5. **Separate the baseline window from the event window** (migration, snapshot, backup) and apply the knee rule only to the baseline. Medium. Gap 6.
6. **Add burst-phase and write latency to the rules, and pool latency histograms instead of taking a maximum of p95 values.** Medium. Gap 4.
7. **Repeat runs and a repeat band.** Run the knee step and the one below at least twice, end the ramp with a repeat of the 15 VM step, publish a range. Medium. Gaps 2, 10, 11.
8. **Add an intermediate step after the first knee** (bisect) so rounding down to 80 percent does not cost a whole step. Small. Gap 2.
9. **Add a stability check (SNIA-style 20 percent range and 10 percent slope) per step, and `ramp_time` in each fio call.** Small. Gap 1.
10. **Publish the knee at 10, 20 and 30 ms, and make the 25 percent and 20 ms values parameters.** Small. Gap 6.
11. **Fix the boot storm baseline** (storm at the first step, compare per VM) and add a unit test for an empty baseline. Small. Gap 6.
12. **Add a fixed-rate probe against backend noise.** Medium. Gap 11.
13. **Test the generator's latency reporting on a deliberately slow local device**, to settle the coordinated omission question for our fio and diskspd versions. Medium. Gap 5.
14. **Add per-node counters and a guest-versus-share reconciliation to `steps.csv`.** Small. Gaps 9, 12.
15. **Sensitivity run at half and double the average rate on one tier.** Medium (lab time rather than build time). Gap 8.
16. **Replace the default profile with a measured one** when real fleet data exists. Large, and outside the test code. Gap 8.

## 6. Not confirmed

- VMmark: the exact quality-of-service thresholds, scoring formula, and run lengths of VMmark 3 and 4. The VMmark user guide link I found was a Broadcom community page, not the PDF. VMmark 4 blog: https://blogs.vmware.com/cloud-foundation/2024/05/09/introducing-vmmark-4/ does not describe scoring or thresholds.
- VMmark default run length (3 hours, turbo 30 minutes) and the 500 ms DVD Store limit: seen only in VMware community forum posts, which are not primary documentation.
- HCIBench: warm-up inside the workload, recommended test durations and repeat counts, working-set percentage, how latency percentiles are reported (only the Easy Run p95 targets and "latency details" appear in the guide). Recommended durations (900 to 1800 s trial, 6 hours formal) appeared only in a StorageReview article.
- SPECstorage Solution 2020: the exact pass or fail rule for latency or for requested against achieved load ("passing" in auto mode), default warm-up and run times, and whether a VDI workload ever existed in 2020 (the SPECsfs2014 VDI workload is mentioned only by a news article).
- SPC-1: I read v3.9.1 only. The term "BSU" does not appear in it. The SPC-1 ratio of data set size to cache: not confirmed.
- fio: whether `rate_iops` with `libaio` measures latency from the intended start time. The documentation does not say this, and I did not test it.
- diskspd: latency percentile list in the output, whether the default is buffered or unbuffered, and any coordinated omission handling.
- IBM File Storage for VPC: published latency expectations, a recommended benchmark method, NFS client mount tuning (`nconnect`, `rsize`, `wsize`) beyond `sec=sys`, NFS 4.1 and `_netdev`, and how the NFS operation count maps to IBM's 16 KB unit.
- KubeVirt and OpenShift Virtualization: an official guide for many-VM storage tests. KubeVirt's own tests and Red Hat's kube-burner article cover creation latency only. The Red Hat blog and reference architecture on performance, scale and boot storms could not be fetched (HTTP 404), so I did not use them.
- Published VDI or "everyday VM" IOPS profiles from a primary source (View Planner, SPECsfs2014 VDI, or similar).
- Whether the VMs' disk files are opened with direct I/O by QEMU in our setup, and what the effect of `randrepeat` is on our measurements. These need a check in the smoke run.
- Whether the shared IBM backend uses a server-side cache that a 2 GB area would stay inside.

## 7. Sources read

- fio HOWTO: https://github.com/axboe/fio/blob/master/HOWTO.rst (raw text read) and https://fio.readthedocs.io/en/latest/fio_doc.html
- diskspd: https://github.com/microsoft/diskspd/wiki/Command-line-and-parameters, https://github.com/microsoft/diskspd/blob/master/README.md
- HCIBench user guide (49 pages, read as text): https://github.com/vmware-labs/hci-benchmark-appliance/blob/main/HCIBench_User_Guide.pdf; blog: https://blogs.vmware.com/cloud-foundation/?p=24181
- VMmark 4: https://blogs.vmware.com/cloud-foundation/2024/05/09/introducing-vmmark-4/
- SPECvirt Datacenter 2021: https://www.spec.org/virt_datacenter2021/docs/designoverview.html, https://www.spec.org/virt_datacenter2021/docs/runrules.html
- SPECvirt_sc2010 paper: https://research.spec.org/icpe_proceedings/2012/p251.pdf
- SPC-1 v3.9.1: https://storageperformance.org/sites/default/files/files/specifications/SPC1_v391_final.pdf
- SPECstorage Solution 2020: https://www.spec.org/storage2020/docs/usersguide.pdf, https://www.spec.org/storage2020/docs/runrules.pdf
- SNIA SSS PTS 2.0.2: https://www.snia.org/sites/default/files/2025-02/SNIA-SSS-PTS-2.0.2.pdf
- KubeVirt: https://github.com/kubevirt/kubevirt/blob/main/docs/perf-scale-benchmarks.md
- kube-burner: https://kube-burner.github.io/kube-burner/latest/measurements/
- Red Hat Developer article: https://developers.redhat.com/articles/2024/09/04/use-kube-burner-measure-red-hat-openshift-vm-and-storage-deployment-scale
- IBM Cloud: https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-profiles, https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-planning, https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-mount-RHEL
