# Spec: One unattended test run after each release update

Date: 2026-10-19. Status: draft for review.

## Goal

After you update the driver on a cluster, one command checks every feature, through the API and the web console, and measures storage speed. Nobody needs to watch it. At the end it gives one pass or fail report, and it leaves nothing behind on the cluster or in the VPC.

"Done" means:
- One command runs every stage below against `rovslab-prod-eude`.
- A full run finishes unattended, and you can read the result on one screen.
- A stage that fails does not stop the other stages, and the run still cleans up everything.
- Speed results are saved, so each run is compared with the last one.

## What exists today

| Part | What it does | State |
|---|---|---|
| `test/e2e/test-vm-clone.sh` | Pool, image import, clone, VM boot | Passed on rovslab, 19 Oct |
| `test/e2e/*_test.go` | Basic pool, cross-zone pool, replication objects only | Not run on rovslab |
| `console-plugin/integration-tests/` | 24 browser tests for the console plugin | Last changed February; not run on ROVS |
| `/bench` skill | fio in a pod | Instructions for Claude, not a script |

No end-to-end test exists for: volume expansion, auto-expand, snapshots and restore, group snapshots, clones of plain volumes, share draining, webhooks, metrics, golden images, real replication, VM live migration, the PVC migration tool, or controller and node restarts.

Found while writing this spec: group snapshots do not work on ROVS. The driver's snapshot sidecar runs without group snapshots, and it only knows the upstream API. rovslab has OpenShift's own copy, `groupsnapshot.storage.openshift.io/v1beta1`, and only because its feature set is not the default. The fix is tracked in GitHub issue #14. Until then, the group snapshot stage reports SKIP with this reason.

Testing between regions is a separate script, written later. This run tests replication within one cluster only.

## Design

### How it runs

- **Where:** on the build server `pool-csi-build`, not on your laptop. You start it with one command from the laptop. It keeps running after you close the laptop.
- **Entry point:** `make test-release CLUSTER=rovslab-prod-eude`. A Bash script, `test/release/run.sh`, runs the stages in order. Each stage is one file in `test/release/stages/`. Bash and `oc` match the existing VM clone test, and the build server already has them.
- **Access:** at the start, the laptop copies a kubeconfig and a short-lived IAM token to the build server. The run deletes both when it ends. No credential is stored in the repo or kept on the server.
- **Safety:**
  - The run refuses to start unless the cluster name matches `CLUSTER`.
  - Everything it makes is in namespaces and pools named `rt-<run id>-...`.
  - It never touches any other object. The cluster has other lab work on it.
- **Each stage** has a set-up step, checks, and its own clean-up, all with time limits. It ends as PASS, FAIL or SKIP, and a SKIP always gives its reason. A stage that times out counts as FAIL, and the run moves on.
- **Clean-up** always runs, also after a failure or an interrupted run. It then checks that no `rt-` objects, VPC shares or VPC mount targets are left. Any leftover counts as a FAIL.

### Stages

Each line says what the stage proves, and how it checks the result itself, not the status the driver reports.

1. **Pre-flight.** The driver runs the expected version (from the start-up log line). All pods are ready, and the CRDs and webhooks answer. No `rt-` leftovers exist from an earlier run.
2. **Pool.**
   - A pool reaches `Ready`.
   - Its share uses security group access mode, and its mount target is in the pool's zone (read from the VPC API).
   - Its StorageClass exists.
   - A pool with a security group that does not exist shows `Degraded` and creates no share.
3. **Volumes.**
   - Two pods on different workers write and read the same file.
   - Owner and permissions match the pool settings.
   - Deleting the PVC removes its SubVolume and its directory.
4. **Growth.**
   - A PVC is resized, and the pod sees the new size.
   - A small pool with auto-expand adds a second share when it fills.
5. **Snapshots.**
   - A VolumeSnapshot is restored to a new PVC. Its files match the source checksums.
   - A snapshot of a running VM that writes nonstop is restored, and a VM boots from it. Added by `2026-10-20-vm-safe-replication.md`.
6. **Group snapshots.** A group snapshot of two PVCs is restored. Both match. If the cluster has no group snapshot API, the stage is SKIP.
7. **Clones.**
   - A clone of a PVC matches its source.
   - A pod that uses the clone cannot start until the copy is done.
8. **VMs.**
   - The existing VM clone test runs: a CentOS VM boots, and its guest agent connects.
   - The VM then live-migrates to another worker and keeps running.
9. **Golden images.** A pool with golden images turned on produces a bootable golden image volume.
10. **Replication.** Replication runs between two pools on the same cluster, in direct NFS mode and in receiver mode. The copies at the destination match the source checksums. Pausing a policy stops new syncs.
    The VM checks below come from `2026-10-20-vm-safe-replication.md`. Each one is added when its part of that spec ships.
    - A running VM is replicated while it writes nonstop to two disks. A VM boots from the copy, and its guest agent connects. The kernel log shows no file system errors.
    - In the copy, every file the VM logged before the freeze matches its checksum, and both disks show the same moment.
    - The next cycle sends only changed blocks: far fewer bytes than the disk size.
    - The source VM live-migrates. The next cycle is a compare pass, sends far fewer bytes than the disk size, and the copy still boots.
    - The receiver is stopped in the middle of applying a copy. After it restarts, the copy boots and shows either the old or the new moment, never a mix.
    - A stopped VM is copied with rsync, and it is still stopped afterwards.
    - Until the new copy method ships, a running VM volume is skipped with an error, and no rsync copy is made.
    - The guest pause and the VM slowdown during a copy are saved to `test/release/history.csv`. More than 20% worse than the last run is a warning.
11. **Draining.** A share marked for draining gets no new volumes.
12. **Restarts.**
    - The controller pod is deleted. A new one takes over, and every pool and volume is unchanged.
    - A node agent pod is deleted. A pod that is writing to a volume keeps writing.
13. **Webhooks.** Bad objects are refused, each with the expected message. Examples: a bad permissions value, a change to an immutable field, and limits out of range.
14. **Migration tool.** `kubectl-migrate` copies a PVC from IBM's own VPC File class into a pool. The data matches.
15. **Metrics.**
    - Prometheus has the driver metrics, with non-zero values after the earlier stages.
    - Each pool reconciles about once a minute. Today's loop bug made it about 30 times a minute.
16. **Console.**
    - The browser tests run with a token, so no IBMid sign-in is needed.
    - A screenshot of each plugin tab is saved with the report.
17. **Speed.** fio runs these tests, 60 seconds each:
    - sequential read and write with 1 MB blocks
    - random read and write with 4 KB blocks
    - a 70/30 random mix

    They run in three places:
    - a pod on a pool volume
    - a pod on a PVC from IBM's own class `ibmc-vpc-file-3000-iops`, as a baseline
    - inside a VM disk on pool storage

    The pool's shares get 3,000 IOPS, the same as the baseline. Results go to `test/release/history.csv`. A result more than 20% worse than the last run is a warning, not a FAIL, because cloud storage speed varies.
18. **Backup.** From `2026-10-20-vm-safe-replication.md`, added when backup ships.
    - A backup policy takes three snapshots of a running VM and of a normal PVC, into a separate snapshot pool.
    - The oldest and the newest snapshot are restored. The VM boots from each, and the PVC files match their checksums.
    - With a keep count of 2, the third snapshot merges the oldest one away. The remaining snapshots still restore and boot.
    - The newest snapshot is exported to COS and restored from COS to a new PVC. The VM boots from it.
    - A known text string written in the VM cannot be found in any object in the bucket, which shows that the data is encrypted.
    - The run uses its own `rt-<run id>/` prefix in one test bucket, and deletes that prefix at the end.
19. **Clean-up and report.**

### The report

- **One screen:** the version tested, each stage's result and time taken, the speed table compared with the last run, and anything left behind.
- **Saved files:** a report folder on the build server with the logs, screenshots and fio output. It is copied back to the laptop.
- **Exit code:** the run exits non-zero if any stage fails.

### How it is built

Stage by stage, smallest first. Each stage is proven on rovslab before the next one is written. These are real cluster tests, so TDD here means:
1. Define the check.
2. See it fail where a feature is broken, or prove that it can fail.
3. Then see it pass.

The bugs that the stages find are fixed in the same PRs, the same way as before.

## Cost

These figures are estimates from today's runs, not measurements of the full run.

- **Run time:** about 2 to 3 hours. The VM, golden image and speed stages take the longest.
- **VPC shares:**
  - about 8 small shares (100 GB or less) for up to 3 hours
  - one 2 TB, 3,000 IOPS share for the speed stage
  - one 500 GB, 10,000 IOPS share for the VM stage
  - one baseline PVC from IBM's class

  All are deleted at the end.
- **Build server:** running for the length of the run.
- **Work:** about 5 PRs:
  - the runner, with stages 1 to 3
  - data stages 4 to 7
  - VM and golden image stages 8 and 9
  - replication, draining, restarts and webhooks, stages 10 to 13
  - the migration tool, metrics, console and speed stages, 14 to 17

## Decisions I made for you

- The run uses the cluster's own VMs for "VSI" tests. A VPC VSI outside the cluster cannot use this driver.
- Replication is tested between two pools on rovslab. Testing between regions is a second script (your decision, 19 Oct).
- Speed tests use 3,000 IOPS shares and compare with IBM's own 3,000 IOPS class. That is the most useful comparison for the money.
- A slower speed result is a warning, not a failure.
- The run executes on the build server, never on the laptop.
- Credentials are passed in at the start and deleted at the end.
- A stage that fails does not stop the run, and clean-up always runs.
- Anything left behind after the run counts as a failure.
- The run is started by hand after an update. It is not run on a schedule or from CI, because each run costs money.
- It is written in Bash with `oc`, the same as the existing VM clone test.
- The VM replication and backup checks test Linux VMs only. Windows freeze is tested once, in the lab test of the VM replication spec, because the run has no Windows image.
- Object lock is not tested in the run, because locked objects cannot be cleaned up afterwards.
