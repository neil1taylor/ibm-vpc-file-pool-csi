# Spec: the VM disk copy speed adapts to the load on the share

Date: 2026-10-26. Status: draft for review. It builds on `2026-10-21-backup-to-snapshot-pool.md` (the backup copy) and `2026-10-25-poolbench.md` (the backup event that measured it).

## Goal

A backup of a running VM is as fast as the share allows, and the VM is never noticeably slower because of it. A person does not have to guess a speed number.

Today the copy runs at a fixed limit, 20 MB/s for each disk (`vmCopySpeedLimitMBps`). The PoolBench backup event on the DR lab copied one 30 GB VM in 1,650 s at that limit. The limit is a guess. Too low, and a backup of a large VM takes hours and the restore point is old. Too high, and the copy reads the VM's own share hard enough to slow the VM.

## What exists today (from the code and the lab)

- The copy is a QEMU `blockdev-backup` job for each disk. It starts with a `speed` in bytes per second, set once in `pkg/vmcopy/vmcopy.go` (`Start`). The same copier makes the VM snapshots by copy (`pkg/pool/snapshot_vm.go`) and the backups (`pkg/pool/replication_vmcopy.go`).
- QEMU can change the speed of a job that is running (`block-job-set-speed`). The driver does not use it yet. **Lab check 1** proves it works on these VMs before anything else is built.
- The replication controller wakes every 30 s and asks QEMU about each running copy (`Progress`). That is the natural moment to change the speed.
- Each node agent already counts the reads and writes of each share and the seconds they took (the metrics `vpc_file_pool_share_nfs_operations_total` and `vpc_file_pool_share_nfs_execute_seconds_total`). PoolBench reads them to find the average latency of a share (`pkg/poolbench/shares.go`).
- The VM's disk lives on the source share, and the copy reads from it. So the latency of the source share is the signal that says "the VM is being slowed".

## What you see

**On the policy.** Three settings in `BackupPolicy.spec`:

- `vmCopySpeedLimitMBps` stays. With adaptive speed on, it is the **highest** speed for each disk. Default `0` = no highest limit (today the default is 20). With adaptive speed off, it is the fixed speed, as before.
- `vmCopyAdaptive` (new): `true` or `false`. Default `true`.
- `vmCopyMaxLatencyMs` (new): the average share latency above which the copy slows down. Default `15`. The webhook refuses a value above 20, the PoolBench limit for a slow VM.

**While a copy runs.** The speed starts low (5 MB/s for each disk), goes up while the source share is quiet, and goes down at once when its latency passes the limit. The status of the volume shows the speed now (`copySpeedMBps`) and the highest speed reached.

**In PoolBench.** The backup event result says how fast the copy went, the average and the highest, and what the VMs' latency did during the event compared with before it. A run can then answer "did the backup slow the VMs, and by how much".

## How it works

1. The copy starts at the start speed (5 MB/s for each disk).
2. Every 30 s, for each running copy, the controller reads the average latency of the source share over the last interval (the change in `execute_seconds_total` divided by the change in `operations_total`, from every node agent, as PoolBench does).
3. If the latency is under the limit, the speed goes up by 5 MB/s for each disk, up to the highest speed. If it is over the limit, the speed is halved, down to the start speed. A network uses the same rule to share a link: grow slowly, shrink fast.
4. The new speed is sent to QEMU with `block-job-set-speed`.
5. If the node agents cannot be read, or the share had no operations in the interval, the speed does not change. A missing reading never speeds the copy up.
6. Two copies that read the same source share count as one load. The controller raises the speed of one copy at a time, so two copies do not both grow together and overshoot.

**Why share latency and not the VM's own latency.** The driver cannot see inside the VM. The share latency is what every VM on that share feels, and it is already measured.

**Why a slow start and a fast brake.** A backup that is a little too slow costs minutes. A VM that is too slow costs the people who use it. The rule errs on the side of the VM.

## Checks before building (the lab)

1. `block-job-set-speed` on a running copy changes its speed, on a Linux and a Windows VM, and the copy still ends with a valid restore point.
2. The average latency of a share goes up when a copy runs at high speed, so that the signal is real. Measure with PoolBench: a backup event at 20, 50 and 0 (no limit) MB/s, with the VM latency before and during.
3. Choose the start speed, the step and the default latency limit from check 2, not from this spec.

## What is not in this step

- The speed of VM snapshots made by copy. They use the same copier, so they can get the same behavior later, but their deadline (`vmSnapshotDeadline`) is worked out from the speed limit and needs its own change.
- Reading latency from inside the VM.
- The export to COS. It reads the backup share, not the VM, and has its own rate.

## Decisions I made for you

- Adaptive speed is on by default, because a number nobody can choose well should not be a setting people must set.
- The existing speed limit becomes the highest speed, and a policy that sets one keeps it. A policy that sets nothing no longer has the 20 MB/s limit.
- The signal is the average latency of the source share, the same one PoolBench uses.
- The rule is: up 5 MB/s each 30 s while quiet, halve when over the limit. The numbers are first guesses; lab check 2 replaces them.
- A missing reading never raises the speed.
- Snapshots by copy keep the fixed limit in this step.
- The PoolBench backup event reports the speed and the effect on VM latency, so the result of this work is measured, not claimed.
