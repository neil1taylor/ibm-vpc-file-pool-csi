# Spec: the console shows the driver's real VM snapshot mode

Date: 2026-10-24. Status: draft for review. It builds on `docs/VM-INSTANT-SNAPSHOTS.md` (the "Choosing a setting" section) and `2026-10-21-instant-vm-snapshots.md`.

## Goal

A person who opens the console on a cluster can see, without reading Helm values, how the driver takes VM disk snapshots there: **instant** or **copy**. The console must never say "instant" while the driver takes copies.

## What exists today (from the code)

- The driver has one setting, `vmSnapshots.method` (flag `--vm-snapshot-method`, in `cmd/main.go`): `auto` or `copy`. With `copy`, every VM disk snapshot is a copy, whatever the cluster allows.
- Instant snapshots need two things at once: `method=auto` and the KubeVirt `Sidecar` feature gate turned on.
- The console box "Instant VM snapshots: on / off / unknown" (`console-plugin/src/components/common/InstantSnapshotStatus.tsx`, used on the Snapshots page and the Pools page) reads **only the gate**, through `instantSnapshotState()` in `console-plugin/src/utils/instant-snapshots.ts`. It never sees `method`.

## The gap

If the gate is on and `method=copy`, the box says "on" but snapshots are copies. A wrong "on" is worse than no box, because a person may rely on instant snapshots that are not happening. The gate can be turned on by anyone who can edit the cluster's `HyperConverged` object, so the two values can differ.

## Design that meets the goal

The console needs one more fact, `method`, and it already has the other one, the gate. The effective mode is a function of both:

| `method` | Gate | Mode shown |
|---|---|---|
| `copy` | on or off | **Copy** |
| `auto` | on | **Instant** |
| `auto` | off | **Copy** |
| any | cannot be read | **Unknown** |
| not published (older chart) | on or off | **Unknown**, with the words "the driver does not report its setting" |

**How the console learns `method`.** The Helm chart renders a small ConfigMap, `ibm-vpc-file-pool-csi-settings`, in the release namespace. It holds only non-secret settings the console wants to show. It starts with one key, `vmSnapshotMethod`. It is configuration written by Helm, not driver state, so the rule "all state in CRDs" is not touched.

Ordinary console users cannot read ConfigMaps in `kube-system`. So the chart also renders a Role that allows `get` on that one ConfigMap by name, and a RoleBinding for all authenticated users. Nothing else in the namespace becomes readable.

**What the box says.**
- Instant: "VM snapshots: instant. Each snapshot starts a new small file. The KubeVirt Sidecar feature gate is on."
- Copy because the gate is off (`method=auto`): "VM snapshots: copy. The driver would take instant snapshots, but the KubeVirt Sidecar feature gate is off."
- Copy because of the setting (`method=copy`): "VM snapshots: copy. The driver is set to copy. Each snapshot copies the disk: slower, and it uses more storage. Nothing else on the cluster is changed."
- Unknown: as today, with the reason.

The list of VMs that have instant snapshots stays, shown only in Instant mode.

## Tests

1. A chart test (`test/chart/`, run by `make test-scripts`): `helm template` renders the ConfigMap with the value of `vmSnapshots.method`, and a Role limited to that ConfigMap name. Written first and seen failing.
2. A console unit test: the mode function returns the table above for every row, including "not published".
3. A component test: the box text matches each mode.
4. By hand on a cluster: `copy` with the gate on shows Copy (the case that is wrong today).

## Build plan

One pull request, two commits: the chart (ConfigMap, Role, RoleBinding, test, `docs/HELM-VALUES.md` note), then the console (mode function, box text, tests). Update `docs/VM-INSTANT-SNAPSHOTS.md` and the CHANGELOG.

## Does the design meet the goal?

Yes. The box shows copy or instant, and it cannot say "instant" unless both conditions hold, because it combines both facts. It does not depend on monitoring being installed.

## Cost, effort and risk

- Effort: about a day, including the by-hand check.
- Risk: low. The only cluster change is one read-only ConfigMap and a Role that reads that one name. A cluster on an older chart shows "Unknown", never a wrong answer.
- Not covered: a cluster where someone edits the driver's flag by hand, not through Helm. The ConfigMap would then be wrong. Helm is the supported way, so this spec accepts that.

## Decisions I made for you

- The box shows the driver's effective mode (copy or instant), not the raw gate.
- The driver's setting reaches the console as a Helm-rendered ConfigMap, not a new CRD and not a metric. A metric needs monitoring installed, which is optional.
- All authenticated users may read that one ConfigMap and nothing else in the namespace.
- An older chart with no ConfigMap shows "Unknown" with a reason. It never guesses.
- The list of VMs with instant snapshots appears only in Instant mode.
- One pull request, chart first, console second.
