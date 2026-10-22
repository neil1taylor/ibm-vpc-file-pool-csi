# Spec: VMs use the standard catalog; the driver keeps CDI's copy limit at 8 GB

Date: 2026-10-20. Status: approved in chat: "hide the converter/golden images just in case we need it in the future. have an installation task that checks/updates the cdi to 2GB and we use the cdi from now on", then "yes, build it with 8GB" after the 25 GB test below.

## Problem

To make a VM from the standard OpenShift catalog with its boot disk on a pool, CDI copies the
catalog image onto the pool. CDI's copy pod may use at most 600 MB of memory. When it writes onto
NFS, the file cache counts against that limit and the pod is killed.

What was measured on rovslab, 20 October:

| Test | Result |
|---|---|
| CDI copy of RHEL 9 from Ceph onto a pool, 600 MB limit | Killed for lack of memory, 3 of 3 (also onto IBM VPC File) |
| The same copy, 2 GB limit | Done in 77 s, peak 1.95 GB |
| VM from the standard RHEL 9 catalog image onto a pool, 2 GB limit | Guest agent after 103 s |
| VM from a RHEL 9 master copy already on the pool (driver copy) | Guest agent after 154 s |
| CDI copy of 25 GB of random data from Ceph onto a pool, 2 GB limit | Killed 6 times, restarts from zero each time, never finishes |
| The same copy, 8 GB limit | Done in 134 s, checksum matches; memory sat at the limit |
| Driver copy of 25 GB on the pool (512 MB limit) | Done in 160 s, checksum matches |

Research: Red Hat documents `HyperConverged.spec.resourceRequirements.storageWorkloads` as the
way to change CDI's pod limits, and its example uses 2Gi. CDI has a known problem with file cache
under the 600 MB limit; its fix covers only image conversion, not plain disk copies. IBM's ROVS
docs say the virtualization add-on is pre-configured and cannot be modified, and do not mention
this setting.

## Design

### The driver keeps CDI's copy limit at 8 GB

- When OpenShift Virtualization is installed, the controller (leader only) reads the
  `HyperConverged` object at start-up and every 5 minutes.
- If CDI's copy pod memory limit is missing or below the target, it raises it:
  - If `storageWorkloads` is not set, it sets CDI's own defaults with the memory limit raised:
    limits CPU 750m and memory 8Gi, requests CPU 100m and memory 60M.
  - If `storageWorkloads` is set, it changes only `limits.memory`.
  - It never lowers a limit and never changes any other field.
- Each time it raises the limit, it logs a warning and adds 1 to the metric
  `vpc_file_pool_cdi_copy_limit_raised_total`. A raise after the first one means something, such as
  IBM's add-on, put the old value back.
- Helm value `cdi.copyPodMemoryLimit` (default `8Gi`) sets the target. An empty value turns the
  check off. Controller flag: `--cdi-copy-pod-memory-limit`.
- The controller ClusterRole gets `get`, `list` and `patch` on `hco.kubevirt.io` `hyperconvergeds`.

### Golden images are hidden

- The golden image code stays, so it can be used again. It is off unless a pool turns it on, as
  today.
- The console's pool form no longer offers golden images, and its "VM storage" preset no longer
  turns them on. The pool details page still shows golden image status if a pool has it.
- The docs mark golden images as hidden and kept for possible later use. They point to the
  standard catalog instead.
- The old converter is already deleted. It can be restored from git history (before PR #27).

### Release test

- Stage 01 checks that CDI's copy pod memory limit is at least 8Gi.
- Stage 08 makes its VMs from the standard catalog (CentOS Stream 9) with the boot disk on the
  pool. It still checks boot, guest agent, and live migration without a restart. It also copies
  25 GB of random data from Ceph onto the pool with CDI and checks the checksum, so every run
  re-tests the limit. The golden image checks are removed from the run; their unit tests stay.

## Decisions I made for you

- The driver does the check and the raise, not a one-time install step, so a reset by IBM's add-on
  is undone within 5 minutes and shows up as a warning and a metric.
- 8 GB is the target. 2 GB (Red Hat's example) worked for a RHEL image but failed for 25 GB of
  data; 8 GB worked for 25 GB. CDI's memory sat at the limit, so a much larger disk is not proven.
  The workers have about 500 GB of memory each.
- The driver only raises the limit. If someone sets a higher one, it is left alone.
- The Helm value can turn the check off, for clusters where changing `HyperConverged` is not
  allowed.
- Pool StorageProfiles keep `cloneStrategy: csi-clone`, so copies from one pool disk to another
  are made by the driver.
- The release test no longer tests golden images, because they are hidden.
