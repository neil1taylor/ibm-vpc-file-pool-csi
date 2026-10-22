# Spec for #46 item 9: backups in the console

Date: 2026-10-22. Status: draft for review.

## Goal

An operator can do everything the backup tools and resources can do from the console, with no YAML and no `kubectl`: see what is backed up and how fresh it is, make and change a backup policy, restore a volume or a VM disk to a new PVC at a chosen moment, remove the backups of one volume, set up the COS export as far as a browser can, and list and restore from COS. "Done" means each row of the table below has a console route, and a release-test browser stage proves restore and delete.

Out of scope here, and kept as a clear seam: **browsing a backup and restoring single files** is in `docs/specs/2026-10-22-backup-file-restore.md` (issue #92). This spec only reserves a "Browse files" action on each backup moment and each volume row, which that spec fills in. Until it is built the action is not shown.

## What exists today

The console has no backup page. The driver has these (all in `cmd/backup` and the resources `BackupPolicy` and `BackupRestore`):

| Task | Today | Console route in this spec |
|---|---|---|
| See backups: volumes, newest and oldest moment, restore points, bytes, consistency, errors | `kubectl backup list`, `BackupPolicy.status.volumes` | Backups page, policy details page |
| See that new backups stopped because the snapshot pool is over 85% | `status.phase=Error` and `message` | Banner on both pages, with link to the pool |
| Make, change, suspend, delete a policy | YAML | Create/edit form, Suspend/Resume, Delete |
| Restore to a new PVC, newest or at a moment | `kubectl backup restore`, `BackupRestore` | Restore page |
| Remove all backups of one volume | `kubectl backup delete` (measure Job, typed confirmation, delete Job) | Delete backups action |
| Export to COS: set up bucket, keys, lock | `kubectl backup cos-setup` (needs the person's IBM Cloud login) | Guide only; see below |
| List volumes and moments in COS | `kubectl backup cos-list` (read-only Job) | COS tab |
| Restore from COS, even in another cluster | `restore --from-cos` | Restore page, source "COS" |
| Export status per volume (`exportedTime`, `exportedBytes`, `exportError`) | `status.volumes` | Same table |
| Backup metrics and alerts | Prometheus | Panels on Monitoring; alerts already show on the dashboard's Firing Alerts card |

## Design

### Pages (under Pools, one new tab "Backups")

1. **Backups list.** One row per BackupPolicy: snapshot pool, schedule, keep time, phase, number of volumes, stalest volume age, export on or off. A phase of `Error` shows the message in plain words. Help buttons on the status columns like the other lists (`column-help.ts`).
2. **Policy details.** Overview card (the spec, in plain words), a volumes table (claim, VM, last backup, oldest backup, restore points, size, consistency, error, export state; a `retained` volume is marked "no longer backed up, kept until <date>"), and per-row actions: **Restore**, **Delete backups**, and the reserved **Browse files** (hidden until #92 is built). Crash-consistent copies carry a warning label, as on the replication page. Card help like `card-help.ts`.
3. **Create and edit form.** Snapshot pool (only pools of the snapshot kind), schedule, keep time, label selector and namespaces (with a live count of the PVCs that match, so a wrong selector shows at once), VM copy speed limit (same explanation as the replication form), suspend. The form explains the 85% stop. The console shows the admission webhook's message instead of copying its rules.
4. **Restore page.** Source: this cluster (policy and claim, or a volume that is gone) or COS. Moment: "latest" or a picker listing the real moments (from the status for the snapshot pool, from the COS list Job for COS). Target namespace and name (must not exist; checked live), storage class (default: the class of the backed-up claim; for COS also the size, which COS does not keep). A confirm dialog says: a new PVC is made, nothing is overwritten, the copy takes time. Progress reuses the snapshot-restore banner: `BackupRestore.status.phase`, the restored moment, and the message on failure.
5. **Delete backups.** Same steps as the CLI, as a dialog: the console starts the measure Job, shows how much would go ("12 GiB, 9 moments"), and asks the operator to type the claim name before it starts the delete Job. It refuses while the policy still backs the volume up (the label is still there) or a restore of that volume is running, with the CLI's wording. It is the only destructive action.
6. **COS tab.** Shows the policy's export settings and the per-volume export state. **List COS contents** runs the read-only list Job and shows volumes and moments. COS credentials are never shown.

### Where the work runs

The measure, delete and list steps are Jobs. The console creates the same Jobs the CLI creates, with the user's own rights, and reads the result from the Pod's termination message (the pattern of the receiver check). To keep the Go and TypeScript Job specs in step, the Go builders write a golden JSON file and a console test builds the Job from the same input and compares it (the pattern of the migration Jobs). Restore needs no Job from the console: it creates one `BackupRestore` object, as the CLI does.

### COS setup: what a browser can and cannot do

`cos-setup` creates the bucket, the keys, the Key Protect wrap and the CBR rule with the person's own IBM Cloud login. The console has no IBM Cloud login and must not hold an IBM Cloud API key. So the console **does not run cos-setup**. It shows a "Set up the export" guide: the exact command with the person's names filled in, what each flag means, and the `spec.export` block the command prints, with a text box to paste it. The console then does what it can: creates the two Secrets from pasted manifests (the pattern of the receiver setup, never reading a Secret back), writes `spec.export` into the policy, and shows whether the export works (from `exportedTime` and `exportError`). This is the one part of the goal a browser cannot reach, and it is stated, not hidden. Closing it would need a driver-side setup service that takes a short-lived IAM token from the person. That is a security design of its own and is not assumed here.

### Access

Each action asks the cluster, by access review, whether the person may do it (create `BackupRestore`, edit `BackupPolicy`, create Jobs and Secrets in the driver namespace). If not, the action is disabled, the missing right is named, and the YAML for an administrator is shown, as in the receiver setup.

### The seam to the file-restore spec

- Each volume row and each moment gets a **Browse files** action, shown only when the file-restore feature is present (the console reads a flag from the place it reads other driver settings).
- The Restore page takes a `?moment=` query value and the Delete dialog takes `?claim=`, so #92 can link to them.
- This spec does not say how files are listed, previewed or restored. If #92 needs a field on `BackupRestore` (a list of paths), it is added there, and the Restore page shows it.

## Plan (about 7 small PRs, in this order; each has jest tests, help text, `FEATURES.md` and `CHANGELOG.md` lines)

1. Types, models, and the read-only Backups list and policy details (no driver change).
2. Restore page and progress banner (snapshot pool source).
3. Create, edit, suspend and delete policy.
4. Go: the Job builders for measure, delete and COS list write golden files; TypeScript builders compare to them.
5. Delete backups dialog.
6. COS tab: list Job, restore from COS, export state.
7. Export setup guide with Secret creation and the `spec.export` write.

Then a release-test browser stage for restore and delete (it needs a cluster and a browser).

Docs touched: `FEATURES.md`, `CHANGELOG.md`, `BACKUP-RECOVERY.md` (the console route next to each `kubectl` step), `MONITORING.md` if panels are added.

## Cost and risk

- Pages 1 to 4 are low risk: they read the resources or create one `BackupRestore`, which never overwrites a volume.
- Delete is the one dangerous action. The typed confirmation, the measure step that shows the size first, the refusals above, and the golden-file tests limit the risk.
- The Job specs are copied into TypeScript, so a drift test is required.
- The effort is larger than the earlier console items: about 7 PRs, mostly forms and tables.
- COS setup stays partly on the CLI. That is the one place the console cannot match the CLI, and it is a platform limit.

## Decisions I made for you

- The goal is full parity with the CLI and the resources, so the plan is 7 PRs, not a read-only page.
- Restore and policy changes are done directly by the console; it creates the same objects the CLI creates.
- Delete backups needs the measure step and a typed claim name, like the CLI.
- The console does not run `cos-setup`, because a browser has no IBM Cloud login and must not hold an API key. It guides, and does the Secret and spec parts.
- Browsing and single-file restore are left to the #92 spec; this one keeps only a hidden "Browse files" action and two link parameters.
- The Job specs are tied to Go by golden files, as for migration.
- Backups is a new tab under Pools, not a new top-level page.
