# draw.io diagrams for the docs

Status: agreed 2026-10-24. Build in progress.

## Goal

Every Markdown doc that explains a structure or a flow has draw.io diagrams next to its text. The diagrams are simple and professional. They carry labels only; the explanation stays in the text. The style and file rules are in [docs/images/README.md](../images/README.md).

## Done means

1. Each doc in scope was read, and each section that explains a structure, a flow, a sequence of steps across parts, or a set of states has a diagram, unless the text is already short and clear without one.
2. Each text drawing made of characters (boxes of `┌─┐`, arrows of `──>`) is replaced by a draw.io diagram that shows the same thing. The text drawing is removed.
3. Every diagram follows docs/images/README.md, was looked at as a PNG, and has alt text.
4. A list at the end of this spec names each doc in scope and its diagrams, or "no diagram" with a one-line reason.

## Scope

In scope: every `.md` file in `docs/` and the repository root, except those listed below.

Out of scope:
- `CHANGELOG.md`: a list of changes.
- `docs/specs/`: build plans. They can get diagrams later.
- `docs/images/README.md`: the style guide itself.

## How the work is split

Two batches, each on its own branch and pull request:
- **Batch 1, design docs:** ARCHITECTURE, CONSISTENCY-MODEL, VOLUME-GROUP-SNAPSHOTS, VOLUME-CLONING, CROSS-REGION-DR, VPC-NETWORKING, VM-DISK-FORMATS, VM-INSTANT-SNAPSHOTS, CSI-INTERFACE, CRD-SPEC, IBM-VPC-API, GOLDEN-IMAGE-WORKFLOW, OVERVIEW, README, FEATURES, KNOWN-LIMITATIONS, SECURITY, CONTEXT, CAPACITY-PLANNING, PERFORMANCE-TUNING, TESTING, CODING-GUIDELINES, SKILL, CONTRIBUTING.
- **Batch 2, operations and tutorials:** TUTORIAL and TUTORIAL-01 to TUTORIAL-14, INSTALL, UPGRADE-GUIDE, HELM-VALUES, API-KEY-SETUP, MONITORING, DAY2-OPERATIONS, BACKUP-RECOVERY, TROUBLESHOOTING, USER-GUIDE, DEPLOYMENT-NOTES, SUPPORT.

A tutorial links to the design doc's diagram when it shows the same thing, instead of a copy.

## Decisions I made for you

- Diagrams are `.drawio.svg` files: one file that GitHub shows as a picture and draw.io opens for editing. No separate `.drawio` and `.png` pair that can drift apart.
- All diagrams live in `docs/images/`, named after their doc.
- Text drawings of boxes and arrows are replaced, not kept beside the new diagram. Folder trees and file lists stay as text, because they are easier to read and copy as text.
- The diagrams show what the doc says. Where a doc looks wrong against the code, the diagram follows the doc, and the difference is reported, not fixed in this work.
- The colours are IBM's grey and blue, with green for live and red only for failures.
- The interactive pages in `docs/diagrams/` stay. They teach by clicking; these diagrams sit beside the text.
- Specs are left out of this pass.

## Docs and their diagrams

Filled in as each batch merges.

### Batch 2, operations and tutorials

- TUTORIAL.md: `tutorial-reading-order`
- TUTORIAL-01-POOL-CREATION.md: `tutorial-01-storageclass-chain`, `tutorial-01-vm-disks`
- TUTORIAL-02-SNAPSHOTS-AND-CLONES.md: `tutorial-02-snapshot-restore-clone`
- TUTORIAL-03-GROUP-SNAPSHOTS-AND-HOOKS.md: `tutorial-03-sync-hooks`
- TUTORIAL-04-POOL-CONFIGURATION.md: `tutorial-04-share-draining`
- TUTORIAL-05-GOLDEN-IMAGES.md: `tutorial-05-golden-image-syncer`
- TUTORIAL-06-REPLICATION-AND-FAILOVER.md: `tutorial-06-direct-nfs`, `tutorial-06-driver-to-driver`
- TUTORIAL-07-MONITORING.md: no diagram: commands and queries; the scrape path is drawn in `monitoring-user-workload` (MONITORING.md)
- TUTORIAL-08-MIGRATION-AND-CONSOLE.md: no diagram: command steps and console screens
- TUTORIAL-09-COS-BACKUP-BUCKET.md: `tutorial-09-cos-bucket`
- TUTORIAL-10-BACKUP-AND-RESTORE.md: `tutorial-10-backup-restore`
- TUTORIAL-11-INSTANT-VM-SNAPSHOTS.md: `tutorial-11-snapshot-chain`
- TUTORIAL-12-REPLICATING-RUNNING-VMS.md: `tutorial-12-vm-copy`
- TUTORIAL-13-FAILOVER-OF-RUNNING-VMS.md: `tutorial-13-failover`
- TUTORIAL-14-CONSOLE-RESTORE.md: `tutorial-14-console-restore`
- INSTALL.md: `install-steps`
- UPGRADE-GUIDE.md: `upgrade-guide-steps`
- HELM-VALUES.md: no diagram: a reference table of values
- API-KEY-SETUP.md: `api-key-setup-managed-credentials`
- MONITORING.md: `monitoring-user-workload`
- DAY2-OPERATIONS.md: `day2-operations-expansion`
- BACKUP-RECOVERY.md: `backup-recovery-snapshot-pool`, `backup-recovery-cos-export`
- TROUBLESHOOTING.md: no diagram: a list of symptoms with commands and fixes
- USER-GUIDE.md: `user-guide-pooling`
- DEPLOYMENT-NOTES.md: no diagram: short status notes
- SUPPORT.md: no diagram: short list of where to get help
