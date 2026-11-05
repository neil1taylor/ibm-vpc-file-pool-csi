# Spec: Use "restore point" instead of "moment"

Date: 2026-11-05. Status: draft for review. Issue: #412.

## Goal

One term, **restore point**, for the point in time that a copy, snapshot or backup shows. People who use other backup products already know it. "Moment" is gone from everything a person reads or types.

"Done" means:
- `docs/CONTEXT.md` defines **restore point** once. The entry "Moment" is removed.
- The words "moment" and "moments" do not appear in user-visible text: docs, console, `kubectl backup`, `kubectl failover` and `kubectl browse` output, events, error messages, webhook messages.
- Every command-line flag and API field that has "moment" in its name also has a restore-point name. The old name still works for one release and prints a deprecation note.
- Backups, copies and pool folders made before the change are still read and still restore.
- The release test passes.

## What changes, and what does not

| Area | Change |
|---|---|
| Words in docs, tutorials, specs, glossary | Replaced. |
| Console text, labels, buttons | Replaced. |
| Command-line output and column headings (`MOMENT` becomes `RESTORE POINT`) | Replaced. |
| Events, errors, webhook messages | Replaced. |
| Command-line flags (`--moment`, `--moments`, `--list-moments`, `--restore-moment`) | New names `--restore-point`, `--restore-points`, `--list-restore-points`, `--restore-to-restore-point`. Old names still work and warn. |
| API fields users write (`momentKeepHours`, `moment` on BackupRestore, BackupFileRestore, BackupBrowse) | New fields added. Old ones still accepted. |
| API status fields (`copyMoment`, `restorePointMoments`, `commonMoments`, `moment`, `moments`, `momentsTruncated`, `restoredMoment`) | New fields written. Old ones are written too for one release, so scripts that read them keep working. |
| Names stored in data (kopia tag `moment`, folder `.moments`, file `.repl-moment`, JSON key `moment` in saved file lists) | **Unchanged.** Renaming them would hide existing backups. |
| Names between pods (`X-Disk-Moment`, `Repl-Disk-Moment`, browse path `/v1/moments`) | **Unchanged.** Pods of different versions talk to each other during an upgrade. |
| Go identifiers inside the code | Renamed only where the file is already changed for another reason. Not a goal. |

A comment next to each unchanged stored or wire name says why it keeps "moment".

## New names

| Old | New |
|---|---|
| `spec.momentKeepHours` | `spec.volumeRestorePointKeepHours` |
| `spec.moment` (BackupRestore, BackupFileRestore, BackupBrowse) | `spec.restorePoint` |
| `status.copyMoment` | `status.copyRestorePoint` |
| `status.restorePointMoments` | `status.restorePoints` |
| `status.commonMoments` | `status.commonRestorePoints` |
| `status.moment` | `status.restorePoint` |
| `status.moments`, `status.momentsTruncated` | `status.restorePoints`, `status.restorePointsTruncated` |
| `status.restoredMoment` | `status.restoredRestorePoint` |

## How the old names keep working

- **Spec fields:** if only the old field is set, the controller reads it. If both are set and differ, the webhook rejects the object with a message naming both. The webhook adds a warning (shown by `kubectl apply`) when the old field is used.
- **Status fields:** the controller writes both for one release. The old ones are marked deprecated in `docs/CRD-SPEC.md`.
- **Flags:** an old flag is a hidden alias. Using it prints one line on stderr: `--moment is deprecated; use --restore-point`.
- **Removal:** the old names go in the release after next, and the release notes say so.

## Order of work (one change at a time, each with its test first)

1. Glossary: merge the two entries.
2. API: new fields, copy logic, webhook warnings, `make generate`, `docs/CRD-SPEC.md`.
3. Command line: new flags, aliases, headings, messages.
4. Console: text, URL parameter (accept both), request body (send the new field; the browse path stays).
5. Docs, tutorials, specs.
6. A test that fails if "moment" appears in user-visible text (an allow-list holds the stored and wire names).
7. DR lab proof: restore an old backup made before the change; fail over with a rewind.
8. Release test on prod, then a release.

Tutorials 12, 13 and 17 and the fixes for #410 and #411 wait for step 5, so they are written once.

## Decisions I made for you

- The term is **restore point**, as you chose.
- Old names keep working for one release, then go. Scripts that read the old status fields get a warning period.
- Stored data keeps its old names. Otherwise old backups would stop showing up.
- Pod-to-pod names keep their old names, so an upgrade in progress does not break.
- The keep time for plain volumes is `volumeRestorePointKeepHours`, because `restorePointKeepHours` already means the keep time for VM disks.
- Go internals are not renamed on their own. Only user-visible words matter.
- A new test blocks the old word from coming back in user-visible text.
- The tutorials for #410 and #411 are run after the rename, not before.
