# Spec: Run on ROVS, move to security group access, fix what the test found

Date: 2026-10-19. Status: draft for review.

## Goal

The driver creates pools, serves volumes and boots a cloned VM on a ROVS cluster (`rovslab-prod-eude`), and on new ROKS clusters. No pool on the old access mode loses data when IBM ends support for it on 6 May 2027. Every problem found on the way is fixed in this repo.

"Done" means the VM clone test passes on `rovslab-prod-eude`, the console plugin shows real pools there, CI is green on `main`, and the docs describe what the driver does now.

## What we found

Measured on `rovslab-prod-eude` (ROVS 4.21.31, 3 bare metal workers in `eu-de-3`) on 19 October 2026, unless a line says otherwise.

| # | Problem | Effect |
|---|---------|--------|
| 1 | The driver creates file shares in **VPC access mode**. IBM has deprecated that mode (End of Support 6 May 2027). The API now refuses it: a share with a VPC-mode mount target fails with HTTP 501 `shares_not_implemented`. A share without a mount target is created in security group mode, and a VPC-mode mount target cannot then be added (HTTP 400). Seen in zones `eu-de-1` and `eu-de-3`. | **The driver cannot create usable storage on any new cluster.** Not ROVS-specific. |
| 2 | The driver reads a security-group mount target's export path as `/`. The real mount path has a share-specific export, for example `10.250.0.18:/83ad9ea6_1be6_...`. | Even with #1 fixed, every NFS mount would fail. |
| 3 | The driver uses the first subnet in `vpc_subnet_ids`, whatever zone the pool is in. | On a multi-zone cluster, a pool's mount target can land in the wrong zone. |
| 4 | A pool reported phase `Ready`, "2 shares ready", while neither share had a mount target. | The pool looks healthy but cannot serve one volume. |
| 5 | When no share was usable, volume requests failed with "no available capacity", and the pool grew to its share limit. | A 500 GB, 10,000 IOPS share was created that could never be used, at a real cost. |
| 6 | The second share was created by a code path that passes no VPC, subnet or security group details. | Shares created by auto-expansion never get a mount target. |
| 7 | Existing pools on VPC-mode shares have no migration path. None were found in this account (eu-de, all resource groups); other accounts are not known. | Data loss risk after 6 May 2027. |
| 8 | CI has failed on `main` since February: 34 `errcheck` lint errors (mostly in `pkg/replication/`), and the integration tests do not compile (a test fake no longer satisfies the Kubernetes client interface). All unit tests pass. | Real failures cannot be seen among the old ones. |
| 9 | The E2E VM clone test defaults to zone `eu-de-1`. All rovslab workers are in `eu-de-3`. | The test fails on any cluster outside `eu-de-1` unless you know the flag. |
| 10 | The container image logs `version="dev"`. The Dockerfile build does not stamp the version. | You cannot tell which build is running. |
| 11 | Every CSI health probe is logged at the default log level: about 3 lines per second. | Real events are hard to find in the controller log. |
| 12 | `webhook.certProvider=manual` never sets the CA bundle on the webhook config, so every API call to the webhook fails TLS. With `webhook.enabled=false`, the controller still starts the webhook server, which needs certificates. *Found by reading the code; not yet run.* | Both documented alternatives to cert-manager are likely broken. |
| 13 | The `/deploy` skill assumes Docker and does not set the pull secret or the certificate option. | Following it on this Mac and on ROVS fails. |
| 14 | `VPC-NETWORKING.md`, the tutorials and the E2E script describe only ROKS and VPC access mode. | The docs describe a setup that no longer works. |
| 15 | The OpenShift console plugin (`console-plugin/`) has never been built, deployed or tested on ROVS. It is off by default, and its image is not in this account's registry. | Unknown whether pool management in the web console works. |
| 16 | Clones of small volumes were meant to copy at once, inside the create call. That path is switched off in code, because the controller cannot reach NFS. Every clone now runs as a background Job. The unused path and its size setting are still in the code, and `VOLUME-CLONING.md` and `CLAUDE.md` still describe it. | The docs promise a fast path that does not exist, and the dead code misleads anyone who reads it. |

Already fixed and merged in PR #3: webhook certificates through the OpenShift service CA, the trusted profile and pull secret install steps, and x86 image builds.

## Design

### Security group access mode (fixes 1, 2, 3, 6)

Every new share is created in security group mode, with one mount target. The mount target gets a virtual network interface (VNI), which is a network port with its own IP address, in a worker subnet.

- **Subnet:** the cluster's worker subnet in the pool's zone. The driver reads all IDs in `vpc_subnet_ids`, asks the VPC API for each subnet's zone, and picks the one that matches `spec.zone`. If no subnet matches, the pool reports an error and creates nothing.
- **Security group:** the cluster's own group, `kube-<cluster_id>`, with the cluster ID read from the `cluster-info` configmap. IBM's file driver uses this group on rovslab. The group already allows all traffic between its members, so the workers can reach NFS port 2049 with no rule change. A new optional pool field, `securityGroupIDs`, overrides it.
- **Mount path:** read the server and export path from the mount target's `mount_path` in every mode. Never assume `/`.
- **One code path:** initial provisioning, auto-expansion and mount-target recovery all build the request in one place. Then a share can never be created without its mount target details.
- **Other zones:** pods in other zones mount the same mount target across zones. IBM documents cross-zone mounting for security group mode. The VM clone test on a multi-zone cluster proves it; until then, the docs say that it is untested.

### Pool health (fixes 4, 5)

- A share counts as ready only when it is `stable` **and** has a mount target with an IP address.
- A pool is `Ready` only when at least one share is ready. Otherwise it is `Degraded`, with a condition that names the cause.
- Auto-expansion runs only when the ready shares are full. It never adds a share because the existing ones have no mount target.

### Migrating existing VPC-mode shares (fixes 7)

IBM's procedure needs every client to unmount the share first. A conversion therefore stops every workload on that share for a few minutes.

- The driver does **not** convert shares by itself.
- An administrator starts the conversion for one pool with an annotation. The driver converts one share at a time. It refuses a share while any pod still uses one of its volumes, and it says which pods.
- The conversion follows IBM's steps: delete the VPC-mode mount target, change the mode, create a security-group mount target, then update the share's volumes to the new address. Existing pods pick up the new address when they restart.
- From now until May 2027, a pool that still has VPC-mode shares shows a warning condition and a metric. Then an alert can fire.
- A runbook in `DAY2-OPERATIONS.md` gives the order of steps, how long each one takes, and how to check each result.

### Console plugin (fixes 15)

- Build and push the plugin image for x86, the same way as the driver image.
- Turn it on in the rovslab install (`consolePlugin.enabled=true`), with the same pull secret.
- Check each tab against real objects: the dashboard, pools, volumes, snapshots, group snapshots, replication policies and monitoring. Each tab must show what `oc get` shows, and the create and edit forms must make valid objects.
- Show the new pool states from this spec: `Degraded`, the cause of an unusable share, and the warning for VPC-mode shares that must be migrated.
- Make the plugin's own lint and unit tests run in CI, if they do not already.

### Housekeeping (fixes 8–14, 16)

- Fix all 34 lint errors properly. For example, return close errors where they matter, not `_ =` everywhere. Make the integration tests compile and pass.
- E2E script: read the zone from the worker nodes when `--zone` is not given.
- Dockerfile: stamp the version, as `make build` already does.
- Log CSI probes at a detailed level, `V(6)`, not at `V(4)`.
- Webhooks: `manual` mode takes the CA bundle from a chart value. With `webhook.enabled=false`, the controller registers no webhooks. Run both on rovslab, not only in tests.
- `/deploy` skill: use podman or Docker, set the pull secret, and use `webhook.certProvider=openshift` on OpenShift.
- Clones: delete the unused synchronous path and its size setting, and correct `VOLUME-CLONING.md` and `CLAUDE.md` to say that every clone runs as a Job.
- Docs: `VPC-NETWORKING.md` describes security group mode; ROKS-only wording becomes "ROKS or ROVS" where both are true; record what this spec changes in `CHANGELOG.md` and `FEATURES.md`.

## How we prove it

Every change starts with a failing test. Unit tests use the fake VPC client. Then, on `rovslab-prod-eude`:

1. Create a pool. Its share has a security-group mount target in `eu-de-3`, and the pool reaches `Ready`.
2. Run the VM clone test with the zone read from the nodes. A cloned CentOS Stream VM boots to a login prompt, checked on the VNC screen and not only on the serial console.
3. Create a pool whose share cannot get a mount target, for example with a security group ID that does not exist. The pool shows `Degraded` and does not grow.
4. Make a VPC-mode share by hand if the API still allows it for a short test. If not, cover the conversion with fakes only, and say so in the PR.
5. The console plugin loads in the rovslab web console. Each tab shows the same objects as `oc get`, checked by screenshot.
6. CI is green on the PR, with Lint, Test and Build all passing.

## Order of work

1. CI fixes (8), so that each later PR shows its own failures.
2. Security group mode and the mount path (1, 2, 3, 6), together with pool health (4, 5), as one PR. Then the VM clone test on rovslab.
3. Housekeeping (9–14, 16).
4. Console plugin (15), after step 2, so it can show the new pool states.
5. Migration (7).

Each step is a separate PR that is merged before the next one starts.

## Cost

- Each run of the VM clone test creates one 500 GB, 10,000 IOPS share for about 15 minutes. The test then deletes it.
- The work is about 5 PRs. Step 2 is the largest. The migration step depends on whether the API still allows VPC-mode test shares.

## Decisions I made for you

- New shares always use security group mode. There is no setting to choose VPC mode, because the API refuses it.
- The default security group is the cluster's own `kube-<cluster_id>` group, the same as IBM's file driver. You can give a different group per pool.
- One mount target per share, in the pool's zone. Other zones mount it across zones.
- A pool with no usable share is `Degraded`, not `Ready`, and it does not grow.
- Migration of old shares is started by a person, one pool at a time, and is never automatic. It stops the workloads on each share for a few minutes.
- I fix the CI failures first, so each later PR has a clean signal.
- The 34 lint errors get real fixes, not blanket "ignore the error" lines.
- The fixes found by reading code (problem 12) are tested on the cluster before I call them fixed.
- The console plugin is in scope. It is tested after the security group change, because it must show the new pool states.
