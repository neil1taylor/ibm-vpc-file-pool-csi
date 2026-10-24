# draw.io diagrams for the docs

Status: agreed and built, 2026-10-24.

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

### Batch 1, design docs

- `docs/ARCHITECTURE.md`: `architecture-system-overview.drawio.svg`, `architecture-create-volume.drawio.svg`, `architecture-delete-volume.drawio.svg`, `architecture-reconcile.drawio.svg`, `architecture-create-snapshot.drawio.svg`, `architecture-clone-volume.drawio.svg`
- `docs/CONSISTENCY-MODEL.md`: `consistency-model-copy.drawio.svg`, `consistency-model-file-level.drawio.svg`, `consistency-model-cross-file.drawio.svg`, `consistency-model-crash.drawio.svg`, `consistency-model-quiesce.drawio.svg`, `consistency-model-vm-blocks.drawio.svg`, `consistency-model-vm-smear.drawio.svg`, `consistency-model-vm-rule.drawio.svg`, `consistency-model-building-blocks.drawio.svg`, `consistency-model-workload-safe.drawio.svg`
- `docs/VOLUME-GROUP-SNAPSHOTS.md`: `volume-group-snapshots-quiesce.drawio.svg`, `volume-group-snapshots-no-quiesce.drawio.svg`, `volume-group-snapshots-components.drawio.svg`, `volume-group-snapshots-create.drawio.svg`, `volume-group-snapshots-delete.drawio.svg`, `volume-group-snapshots-pre-hook.drawio.svg`, `volume-group-snapshots-post-hook.drawio.svg`, `volume-group-snapshots-failure-abort.drawio.svg`, `volume-group-snapshots-failure-continue.drawio.svg`
- `docs/VOLUME-CLONING.md`: `volume-cloning-same-share.drawio.svg`, `volume-cloning-cross-share.drawio.svg`, `volume-cloning-request.drawio.svg`, `volume-cloning-vm-rule.drawio.svg`, `volume-cloning-worker.drawio.svg`, `volume-cloning-node-gate.drawio.svg`, `architecture-clone-volume.drawio.svg`
- `docs/CROSS-REGION-DR.md`: `cross-region-dr-overview.drawio.svg`, `cross-region-dr-vm-blocks.drawio.svg`, `cross-region-dr-components.drawio.svg`, `cross-region-dr-data-flow.drawio.svg`, `cross-region-dr-failover.drawio.svg`, `cross-region-dr-native-snapshots.drawio.svg`
- `docs/VPC-NETWORKING.md`: `vpc-networking-topology.drawio.svg`, `vpc-networking-path.drawio.svg`, `vpc-networking-single-zone.drawio.svg`, `vpc-networking-cross-zone.drawio.svg`, `vpc-networking-pool-per-zone.drawio.svg`, `vpc-networking-encryption.drawio.svg`
- `docs/VM-DISK-FORMATS.md`: `vm-disk-formats-io-path.drawio.svg`, `vm-disk-formats-cdi-import.drawio.svg`
- `docs/VM-INSTANT-SNAPSHOTS.md`: `vm-instant-snapshots-chain.drawio.svg`, `vm-instant-snapshots-take.drawio.svg`, `vm-instant-snapshots-hook.drawio.svg`
- `docs/CSI-INTERFACE.md`: `csi-interface-node-mount.drawio.svg`
- `docs/CRD-SPEC.md`: `crd-spec-relationships.drawio.svg`
- `docs/IBM-VPC-API.md`: `ibm-vpc-api-authentication.drawio.svg`, `ibm-vpc-api-share-lifecycle.drawio.svg`
- `docs/GOLDEN-IMAGE-WORKFLOW.md`: `golden-image-workflow-syncer.drawio.svg`
- `docs/OVERVIEW.md`: `overview-controller-and-node-agent.drawio.svg`
- `README.md`: `readme-architecture.drawio.svg`
- `docs/FEATURES.md`: no diagram: a catalog of features and a comparison table; no structure or flow to draw
- `docs/KNOWN-LIMITATIONS.md`: no diagram: a list of limits and workarounds; no structure or flow to draw
- `docs/SECURITY.md`: no diagram: a policy and a hardening checklist; no structure or flow to draw
- `docs/CONTEXT.md`: `context-storage.drawio.svg`
- `docs/CAPACITY-PLANNING.md`: `capacity-planning-decisions.drawio.svg`
- `docs/PERFORMANCE-TUNING.md`: `performance-tuning-spread.drawio.svg`, `performance-tuning-binpack.drawio.svg`
- `docs/TESTING.md`: `testing-pyramid.drawio.svg`
- `docs/CODING-GUIDELINES.md`: no diagram: rules and conventions; its only drawing is a folder-role listing, kept as text
- `SKILL.md`: no diagram: build guidance; its only drawing is a folder tree, kept as text
- `CONTRIBUTING.md`: no diagram: contributor steps and rules; no structure or flow to draw

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

### Tutorials added later

- TUTORIAL-15-VM-FROM-THE-CATALOG.md: `tutorial-15-vm-from-catalog`
- TUTORIAL-16-CONSOLE-BACKUPS-CLONES-RECEIVER.md: `tutorial-16-backup-and-restore`, `tutorial-16-receiver-setup`
