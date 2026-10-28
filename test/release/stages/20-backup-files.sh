#!/usr/bin/env bash
# 20-backup-files — Browse and restore single files from a backup
# (docs/specs/2026-10-22-backup-file-restore.md; the spec calls this stage 19,
# but 19 is the instant snapshots stage).
# Part 1, the browse helper: on a real share, filled by three users, the helper
# (root, no capabilities, read-only mount) reads another user's files with mode
# 0600 and 0700, refuses a link, cannot write, and tars everything.
# test/e2e/test-backup-files.sh does the work and logs a "PASS:" line for each
# outcome, or one "FAIL:" line and exit 1. This stage runs it and checks the log.
# Part 2, browse sessions: test/e2e/test-backup-browse-session.sh backs up a pod
# volume twice, opens BackupBrowse sessions by hand (as the gateway will), and
# checks TLS with the pinned certificate, the token, the latest copy and the
# first moment, the locked-down helper pod, the NetworkPolicy, and that closing
# or deleting a session leaves nothing behind.
# Part 3, the gateway: the same script goes on with real users. A namespace admin
# lists and downloads through `kubectl backup ls/get/browse` with no setup; a user
# with only the view role, an admin of another namespace and a user whose binding
# was removed get "forbidden"; the audit Events name the user and the path and
# hold no token or content. Needs the kubectl-backup binary (RT_BACKUP_BIN, built
# into test/release/bin by remote.sh).
# Part 4, restore of files: test/e2e/test-backup-file-restore.sh makes
# BackupFileRestore objects by hand (as the gateway will) on a volume with files
# of three owners, a 0600 file and a 0700 folder owned by other users, a link, a
# name with a newline and a 40 MiB file with a checksum. It restores into a new
# claim (sized by the Job that measures), at the first moment, and into an
# existing claim of a pool with no configured owner, and checks owners, modes,
# links and the checksum; refusals (a claim with disk.img, no room, a missing
# path, a path with ".."); and that nothing is left behind.
# Part 5, restore through the gateway: the same script goes on with real users.
# A namespace admin runs `kubectl backup restore-files` into a new claim and a
# new folder of an existing claim with no setup; a user with only the view role,
# an admin of another namespace, and a user whose binding was removed are
# forbidden; a RoleBinding to backup-file-reader lets an admin of another
# namespace restore into a claim of their own; a restore is shown only to the
# user who asked; the audit Event names the user and the paths and holds no token.

# Part 6, backups in COS: test/e2e/test-backup-files-cos.sh backs up a pod volume
# with the policy's export block on, waits until the moments are in the bucket, and
# reads them with `kubectl backup ls/get/restore-files --from-cos` as real users:
# the latest copy and the first moment, a link as a link, a file of another owner,
# the checksum, a restore into a new claim, at the first moment and into a new
# folder of an existing claim. The helper pod has no service account token, runs as
# an ordinary user and mounts no share; the COS and Key Protect Secrets are not in
# the browse namespace; another pod cannot reach it. The wrong people are forbidden,
# naming a bucket directly needs the right to read its Secrets, a wrong root key, a
# missing Secret and a wrong COS key are said in words, no key reaches a log, an
# Event or a status, and nothing is left behind. It needs the COS configuration of
# stage 18 (see test/e2e/lib/backup-cos.sh) and is "not checked" without it. It
# cannot test a moment that the local keep time has removed: that takes too long.

# Part 7, VM disks: test/e2e/test-backup-files-vm.sh makes three small VM disks
# (XFS on LVM, ext4, NTFS) with libguestfs in the guest image, backs them up
# twice, and reads them with `kubectl backup ls/get/browse/restore-files --fs` as
# real users, without starting the VM: the latest copy and the first moment, a
# checksum, a restore with the file system name kept, the hold on the disk while a
# session is open, the locked-down helper pod, the wrong people, and that nothing
# is left. It needs the guest image (RT_GUEST_IMAGE, with backupBrowse.guest on)
# and is "not checked" without it. The undo-file path of a running VM is covered by
# unit tests and by the backups of stage 18, not by this script.

# A pool takes 5 to 15 minutes to become ready; the rest is under 5 minutes.
# The COS part adds up to an hour (the export of two moments, then the restores).
RT_STAGE_TIMEOUT=14400

HERE_STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$HERE_STAGE/../../e2e/test-backup-files.sh"
PREFIX=$(rt_name files)
E2E_LOG="$RT_OUT/backup-files-e2e.log"
E2E_SESSION="$HERE_STAGE/../../e2e/test-backup-browse-session.sh"
SESSION_PREFIX=$(rt_name bsess)
SESSION_LOG="$RT_OUT/backup-browse-session-e2e.log"
E2E_RESTORE="$HERE_STAGE/../../e2e/test-backup-file-restore.sh"
RESTORE_PREFIX=$(rt_name frest)
RESTORE_LOG="$RT_OUT/backup-file-restore-e2e.log"
E2E_COS="$HERE_STAGE/../../e2e/test-backup-files-cos.sh"
COSF_PREFIX=$(rt_name fcos)
COSF_LOG="$RT_OUT/backup-files-cos-e2e.log"
E2E_VM="$HERE_STAGE/../../e2e/test-backup-files-vm.sh"
VMF_PREFIX=$(rt_name fvm)
VMF_LOG="$RT_OUT/backup-files-vm-e2e.log"
BACKUP_BIN="${RT_BACKUP_BIN:-$(dirname "$RT_LIB")/bin/kubectl-backup}"

run() {
    local tool rc fail_line
    for tool in oc jq curl tar base64; do
        command -v "$tool" >/dev/null || skip "needs $tool"
    done
    [ -f "$E2E" ] || skip "no end-to-end script at $E2E"

    rt_log "running $E2E"
    env PREFIX="$PREFIX" BROWSE_IMAGE="${RT_BROWSE_IMAGE:-}" bash "$E2E" >"$E2E_LOG" 2>&1
    rc=$?
    cat "$E2E_LOG"

    if fail_line=$(first_fail_line "$E2E_LOG"); then
        check "the browse helper script reported: $fail_line" false
    elif [ "$rc" -ne 0 ]; then
        check "the browse helper script exited with $rc and no FAIL line (see backup-files-e2e.log)" false
    else
        check "the browse helper script ended without a FAIL line" true
    fi

    check "the helper reads another user's file with mode 0600" log_has_pass "$E2E_LOG" "another user's file with mode 0600"
    check "the helper reads a file in another user's folder with mode 0700" log_has_pass "$E2E_LOG" "mode 0700"
    check "a path through a link is refused" log_has_pass "$E2E_LOG" "a path through a link to /etc is refused"
    check "a tar of the volume holds every entry" log_has_pass "$E2E_LOG" "a tar of the volume holds all"
    check "the helper cannot write to the backup and has no capabilities" log_has_pass "$E2E_LOG" "has no capabilities"

    [ -f "$E2E_SESSION" ] || skip "no end-to-end script at $E2E_SESSION"
    rt_log "running $E2E_SESSION"
    env PREFIX="$SESSION_PREFIX" BACKUP_BIN="$BACKUP_BIN" bash "$E2E_SESSION" >"$SESSION_LOG" 2>&1
    rc=$?
    cat "$SESSION_LOG"
    if fail_line=$(first_fail_line "$SESSION_LOG"); then
        check "the browse session script reported: $fail_line" false
    elif [ "$rc" -ne 0 ]; then
        check "the browse session script exited with $rc and no FAIL line (see backup-browse-session-e2e.log)" false
    else
        check "the browse session script ended without a FAIL line" true
    fi
    check "a session starts and answers over TLS" log_has_pass "$SESSION_LOG" "both sessions are Ready"
    check "the latest session reads the changed file" log_has_pass "$SESSION_LOG" "reads the changed file"
    check "a session at the first moment reads the first content" log_has_pass "$SESSION_LOG" "reads the first content"
    check "a wrong token and another session's certificate are refused" log_has_pass "$SESSION_LOG" "certificate of another session"
    check "the helper pod is locked down and mounts the backup read-only" log_has_pass "$SESSION_LOG" "read-only"
    check "another pod cannot reach the helper" log_has_pass "$SESSION_LOG" "NetworkPolicy"
    check "closing a session leaves nothing behind" log_has_pass "$SESSION_LOG" "closing a session removes"
    check "deleting a session leaves nothing behind" log_has_pass "$SESSION_LOG" "deleting a session removes"
    check "no session object is in the driver namespace" log_has_pass "$SESSION_LOG" "no session object is in the driver namespace"
    if grep -qF "NOT CHECKED: the backup gateway is not installed" "$SESSION_LOG"; then
        not_checked "browsing through the gateway with real users" "the backup gateway is not installed"
    else
        check "a namespace admin lists a backup through kubectl backup with no setup" log_has_pass "$SESSION_LOG" "a namespace admin lists the backup"
        check "kubectl backup get downloads a file, at the latest copy and at a moment" log_has_pass "$SESSION_LOG" "downloads the older content"
        check "a user with only the view role is forbidden" log_has_pass "$SESSION_LOG" "a user with only the view role: forbidden"
        check "an admin of another namespace is forbidden" log_has_pass "$SESSION_LOG" "an admin of another namespace: forbidden"
        check "a removed binding stops access at once" log_has_pass "$SESSION_LOG" "a user whose binding was removed: forbidden"
        check "a call with no token is refused" log_has_pass "$SESSION_LOG" "a call with no token is refused"
        check "the audit Events name the user and the path and hold no secrets" log_has_pass "$SESSION_LOG" "hold no token and no content"
    fi

    [ -f "$E2E_RESTORE" ] || skip "no end-to-end script at $E2E_RESTORE"
    rt_log "running $E2E_RESTORE"
    env PREFIX="$RESTORE_PREFIX" bash "$E2E_RESTORE" >"$RESTORE_LOG" 2>&1
    rc=$?
    cat "$RESTORE_LOG"
    if fail_line=$(first_fail_line "$RESTORE_LOG"); then
        check "the file restore script reported: $fail_line" false
    elif [ "$rc" -ne 0 ]; then
        check "the file restore script exited with $rc and no FAIL line (see backup-file-restore-e2e.log)" false
    else
        check "the file restore script ended without a FAIL line" true
    fi
    check "a restore into a new claim ends Done" log_has_pass "$RESTORE_LOG" "a restore into a new claim ends Done"
    check "the new claim is sized by the Job that measures" log_has_pass "$RESTORE_LOG" "sized the claim to 1 GiB"
    check "the new claim holds the latest content" log_has_pass "$RESTORE_LOG" "the new claim holds the latest content"
    check "the 40 MiB file has the same checksum" log_has_pass "$RESTORE_LOG" "same checksum"
    check "files with mode 0600 get the owner of the pool and keep their mode" log_has_pass "$RESTORE_LOG" "files with mode 0600 keep their mode"
    check "a folder with mode 0700 is restored whole" log_has_pass "$RESTORE_LOG" "a folder with mode 0700 is restored whole, with the pool"
    check "a name with a newline is restored" log_has_pass "$RESTORE_LOG" "a name with a newline is restored"
    check "a link is restored as a link and not followed" log_has_pass "$RESTORE_LOG" "restored as a link, not followed"
    check "only the chosen paths are restored" log_has_pass "$RESTORE_LOG" "only the chosen paths"
    check "a restore at the first moment brings back the first content" log_has_pass "$RESTORE_LOG" "brings back the first content"
    check "a new folder in an existing claim, with the backup's owners kept" log_has_pass "$RESTORE_LOG" "keep the backup's owners"
    check "a folder with mode 0700 owned by another user is restored whole in an existing claim" log_has_pass "$RESTORE_LOG" "owned by another user is restored whole"
    check "the claim's own file is untouched" log_has_pass "$RESTORE_LOG" "the claim's own file is untouched"
    check "a second restore takes the next free folder name" log_has_pass "$RESTORE_LOG" "takes the next free name"
    check "no staging folder is left" log_has_pass "$RESTORE_LOG" "no staging folder is left in the claim"
    check "a claim that holds disk.img is refused" log_has_pass "$RESTORE_LOG" "holds disk.img is refused"
    check "files that do not fit are refused before they are copied" log_has_pass "$RESTORE_LOG" "do not fit in the claim are refused"
    check "a path that is not in the backup fails the restore" log_has_pass "$RESTORE_LOG" "not in the backup fails"
    check "the webhook refuses a path with .." log_has_pass "$RESTORE_LOG" "the webhook refuses a path with .."
    check "no Job or share claim of a restore is left" log_has_pass "$RESTORE_LOG" "no Job or share claim of a restore is left"
    check "deleting a restore removes its list of paths" log_has_pass "$RESTORE_LOG" "deleting a restore removes its list of paths"
    check "the Events name no path" log_has_pass "$RESTORE_LOG" "the Events name no path"
    if grep -qF "NOT CHECKED: the backup gateway is not installed" "$RESTORE_LOG"; then
        not_checked "restoring files through the gateway with real users" "the backup gateway is not installed"
    else
        check "a namespace admin restores into a new claim through kubectl backup with no setup" log_has_pass "$RESTORE_LOG" "restores into a new claim with kubectl backup restore-files and no setup"
        check "the files restored by a user are in the new claim" log_has_pass "$RESTORE_LOG" "restored by a user are in the new claim"
        check "a user restores into a new folder of an existing claim" log_has_pass "$RESTORE_LOG" "a user restores into a new folder of an existing claim"
        check "a user with only the view role is forbidden" log_has_pass "$RESTORE_LOG" "a user with only the view role: forbidden"
        check "an admin of another namespace is forbidden" log_has_pass "$RESTORE_LOG" "an admin of another namespace: forbidden"
        check "nobody restores into a namespace where they cannot make claims" log_has_pass "$RESTORE_LOG" "cannot make claims: forbidden"
        check "a refused request makes no restore and no claim" log_has_pass "$RESTORE_LOG" "a refused request made no claim"
        check "backup-file-reader lets an admin of another namespace restore into their own claim" log_has_pass "$RESTORE_LOG" "backup-file-reader in the source namespace"
        check "a restore is shown only to the user who asked" log_has_pass "$RESTORE_LOG" "sees no such restore"
        check "a removed binding stops access at once" log_has_pass "$RESTORE_LOG" "a user whose binding was removed: forbidden"
        check "the audit Events name the user and the paths" log_has_pass "$RESTORE_LOG" "the audit Events name the user and the paths"
        check "the audit Events hold no token" log_has_pass "$RESTORE_LOG" "the audit Events hold no token"
    fi

    [ -f "$E2E_COS" ] || skip "no end-to-end script at $E2E_COS"
    rt_log "running $E2E_COS"
    env PREFIX="$COSF_PREFIX" BACKUP_BIN="$BACKUP_BIN" bash "$E2E_COS" >"$COSF_LOG" 2>&1
    rc=$?
    cat "$COSF_LOG"
    if fail_line=$(first_fail_line "$COSF_LOG"); then
        check "the COS browse script reported: $fail_line" false
    elif [ "$rc" -ne 0 ]; then
        check "the COS browse script exited with $rc and no FAIL line (see backup-files-cos-e2e.log)" false
    else
        check "the COS browse script ended without a FAIL line" true
    fi
    if grep -qF "NOT CHECKED: no COS bucket is configured" "$COSF_LOG"; then
        not_checked "browsing and restoring files from a backup in COS" "no COS bucket is configured (COS_BUCKET)"
    elif grep -qF "NOT CHECKED: the backup gateway is not installed" "$COSF_LOG"; then
        not_checked "browsing and restoring files from a backup in COS with real users" "the backup gateway is not installed"
    else
        check "a namespace admin lists a backup in COS with no setup" log_has_pass "$COSF_LOG" "lists the backup in COS with no setup"
        check "the latest copy is read from COS" log_has_pass "$COSF_LOG" "the latest copy is read from COS"
        check "the first moment is read from COS" log_has_pass "$COSF_LOG" "the first moment is read from COS"
        check "a file made after the first moment is not in it" log_has_pass "$COSF_LOG" "a file made after the first moment is not in it"
        check "a file from COS has the same checksum" log_has_pass "$COSF_LOG" "a file from COS has the same checksum"
        check "a link is listed as a link and is not followed" log_has_pass "$COSF_LOG" "a path through a link is refused"
        check "a name with a newline is listed escaped" log_has_pass "$COSF_LOG" "a name with a newline is listed escaped"
        check "a file with mode 0600 of another owner is read" log_has_pass "$COSF_LOG" "a file with mode 0600 of another owner is read"
        check "the moments in COS can be listed" log_has_pass "$COSF_LOG" "the moments in COS can be listed"
        check "the session reads the policy's bucket and names none" log_has_pass "$COSF_LOG" "names none"
        check "the COS helper has no service account token and mounts no share" log_has_pass "$COSF_LOG" "no service account token, no host path and mounts no share"
        check "the COS helper runs as an ordinary user with no capability" log_has_pass "$COSF_LOG" "an ordinary user with every capability dropped"
        check "the keys are mounted read-only" log_has_pass "$COSF_LOG" "the keys are mounted read-only"
        check "the browse namespace never holds the COS or Key Protect keys" log_has_pass "$COSF_LOG" "session Secrets only, never the COS or Key Protect keys"
        check "another pod cannot reach the COS helper" log_has_pass "$COSF_LOG" "another pod cannot reach the COS helper"
        check "a restore from COS reads COS" log_has_pass "$COSF_LOG" "the restore read COS"
        check "a restore from COS into a new claim holds the latest content" log_has_pass "$COSF_LOG" "into a new claim holds the latest content"
        check "the restored file has the same checksum" log_has_pass "$COSF_LOG" "the restored file has the same checksum"
        check "restored files get the pool's owner and keep their mode" log_has_pass "$COSF_LOG" "keep their mode"
        check "a newline name and a link are restored" log_has_pass "$COSF_LOG" "a name with a newline is restored, and a link is restored as a link"
        check "a restore from COS at the first moment brings back the first content" log_has_pass "$COSF_LOG" "brings back the first content"
        check "a restore from COS into an existing claim writes a new folder" log_has_pass "$COSF_LOG" "into an existing claim writes a new folder"
        check "a user with only the view role is forbidden" log_has_pass "$COSF_LOG" "a user with only the view role: forbidden"
        check "an admin of another namespace is forbidden" log_has_pass "$COSF_LOG" "an admin of another namespace: forbidden"
        check "a removed binding stops access at once" log_has_pass "$COSF_LOG" "a user whose binding was removed: forbidden"
        check "naming a bucket needs the right to read its Secrets" log_has_pass "$COSF_LOG" "forbidden to a namespace admin who cannot read its Secrets"
        check "someone who may read the Secrets reads a bucket named directly" log_has_pass "$COSF_LOG" "reads a bucket named directly"
        check "a file from a bucket named directly has the same checksum" log_has_pass "$COSF_LOG" "a file from a bucket named directly has the same checksum"
        check "a bucket named directly serves the first moment" log_has_pass "$COSF_LOG" "a bucket named directly serves the first moment"
        check "files are restored from a bucket named directly" log_has_pass "$COSF_LOG" "restored from a bucket named directly are in the new claim"
        check "a wrong Key Protect root key is said in words" log_has_pass "$COSF_LOG" "a wrong Key Protect root key:"
        check "a Secret that does not exist is said in words" log_has_pass "$COSF_LOG" "a Secret that does not exist:"
        check "a wrong COS key is said in words" log_has_pass "$COSF_LOG" "a wrong COS key:"
        check "a prefix with no repository fails in words" log_has_pass "$COSF_LOG" "a prefix with no repository:"
        check "a volume that was never backed up is said in words" log_has_pass "$COSF_LOG" "a volume that was never backed up:"
        check "no key is in a log, an Event or a status" log_has_pass "$COSF_LOG" "no COS key, API key or wrapped key is in"
        check "the audit Events say the backup was read from COS" log_has_pass "$COSF_LOG" "say the backup was read from COS"
        check "a COS session has its Secret in both namespaces while it is open" log_has_pass "$COSF_LOG" "has its Secret in both namespaces"
        check "closing a COS session leaves nothing behind" log_has_pass "$COSF_LOG" "closing a COS session removes"
        check "deleting a COS session leaves nothing behind" log_has_pass "$COSF_LOG" "deleting a COS session removes"
        check "no pod, Job or Secret of any session is left in either namespace" log_has_pass "$COSF_LOG" "no pod, Job or Secret of any session of this test is left"
        check "no Job or pod of a restore or a session is left" log_has_pass "$COSF_LOG" "no Job or pod of a restore or a session is left"
    fi

    [ -f "$E2E_VM" ] || skip "no end-to-end script at $E2E_VM"
    rt_log "running $E2E_VM"
    env PREFIX="$VMF_PREFIX" BACKUP_BIN="$BACKUP_BIN" GUEST_IMAGE="${RT_GUEST_IMAGE:-}" bash "$E2E_VM" >"$VMF_LOG" 2>&1
    rc=$?
    cat "$VMF_LOG"
    if fail_line=$(first_fail_line "$VMF_LOG"); then
        check "the VM disk browse script reported: $fail_line" false
    elif [ "$rc" -ne 0 ]; then
        check "the VM disk browse script exited with $rc and no FAIL line (see backup-files-vm-e2e.log)" false
    else
        check "the VM disk browse script ended without a FAIL line" true
    fi
    if grep -qF "NOT CHECKED: no guest image" "$VMF_LOG"; then
        not_checked "browsing and restoring files inside a VM disk" "no guest image (RT_GUEST_IMAGE)"
    elif grep -qF "NOT CHECKED: the backup gateway is not installed" "$VMF_LOG"; then
        not_checked "browsing and restoring files inside a VM disk with real users" "the backup gateway is not installed"
    else
        check "the policy sees the claims as disks of a VM" log_has_pass "$VMF_LOG" "disks of a VM"
        check "ls lists the logical volume of an LVM disk, not its physical volume" log_has_pass "$VMF_LOG" "file systems of an LVM disk"
        check "ls lists the file system of the ext4 disk" log_has_pass "$VMF_LOG" "file system of the ext4 disk"
        check "ls lists the NTFS file system" log_has_pass "$VMF_LOG" "NTFS file system"
        check "XFS on LVM: the latest copy reads the changed content" log_has_pass "$VMF_LOG" "latest copy reads 'second'"
        check "XFS on LVM: the first moment reads the first content without starting the VM" log_has_pass "$VMF_LOG" "first moment reads 'first'"
        check "a file from the XFS volume has the same checksum" log_has_pass "$VMF_LOG" "same checksum"
        check "ext4: a file is read" log_has_pass "$VMF_LOG" "ext4: a file is read"
        check "NTFS: a file is read" log_has_pass "$VMF_LOG" "NTFS: a file is read"
        check "a name with a newline is listed escaped" log_has_pass "$VMF_LOG" "newline is listed escaped"
        check "an open session holds the disk" log_has_pass "$VMF_LOG" "the disk is held"
        check "closing the session ends the hold" log_has_pass "$VMF_LOG" "closing the session ends the hold"
        check "a restore keeps the file system name at the top" log_has_pass "$VMF_LOG" "file system name at the top"
        check "the restore is a guest restore" log_has_pass "$VMF_LOG" "the restore is a guest restore"
        check "the hold ends with the restore" log_has_pass "$VMF_LOG" "the hold ends with the restore"
        check "the guest helper has no token and no host path" log_has_pass "$VMF_LOG" "no service account token and no host path"
        check "the guest helper has no capability and no privilege" log_has_pass "$VMF_LOG" "no capability, no privilege"
        check "the backup is mounted read-only" log_has_pass "$VMF_LOG" "mounted read-only"
        check "a user with only the view role is forbidden" log_has_pass "$VMF_LOG" "only the view role: forbidden"
        check "no helper pod or Job of a session is left" log_has_pass "$VMF_LOG" "no helper pod or Job of a session is left"
        check "no Job or pod of a restore is left" log_has_pass "$VMF_LOG" "no Job or pod of a restore is left"
    fi
}

cleanup() {
    oc delete pod "$PREFIX-browse" -n "$RT_DRIVER_NS" --ignore-not-found --wait=true
    oc delete pvc "$PREFIX-share" -n "$RT_DRIVER_NS" --ignore-not-found
    oc delete pv "$PREFIX-share" --ignore-not-found
    oc delete secret "$PREFIX-token" -n "$RT_DRIVER_NS" --ignore-not-found
    oc delete namespace "$PREFIX-test" --ignore-not-found --wait=true --timeout=900s
    oc delete filesharepool "$PREFIX-pool" --ignore-not-found --wait=false
    oc delete backupbrowse -l "$SESSION_PREFIX-test=yes" --ignore-not-found --wait=false
    oc delete backuppolicy "$SESSION_PREFIX-policy" --ignore-not-found --wait=false
    oc delete namespace "$SESSION_PREFIX-test" "$SESSION_PREFIX-test-other" --ignore-not-found --wait=true --timeout=900s
    oc delete filesharepool "$SESSION_PREFIX-src" "$SESSION_PREFIX-snap" --ignore-not-found --wait=false
    oc delete backupfilerestore -l "$RESTORE_PREFIX-test=yes" --ignore-not-found --wait=false
    oc delete backuppolicy "$RESTORE_PREFIX-policy" --ignore-not-found --wait=false
    oc delete namespace "$RESTORE_PREFIX-test" --ignore-not-found --wait=true --timeout=900s
    oc delete filesharepool "$RESTORE_PREFIX-src" "$RESTORE_PREFIX-snap" "$RESTORE_PREFIX-plain" --ignore-not-found --wait=false
    oc delete backuppolicy "$COSF_PREFIX-policy" --ignore-not-found --wait=false
    oc delete namespace "$COSF_PREFIX-test" "$COSF_PREFIX-test-other" --ignore-not-found --wait=true --timeout=900s
    oc delete filesharepool "$COSF_PREFIX-src" "$COSF_PREFIX-snap" "$COSF_PREFIX-plain" --ignore-not-found --wait=false
    oc delete backuppolicy "$VMF_PREFIX-policy" --ignore-not-found --wait=false
    oc delete namespace "$VMF_PREFIX-test" --ignore-not-found --wait=true --timeout=900s
    oc delete filesharepool "$VMF_PREFIX-src" "$VMF_PREFIX-snap" --ignore-not-found --wait=false
    oc delete rolebinding "$COSF_PREFIX-cosadmin" -n "$RT_DRIVER_NS" --ignore-not-found
    oc delete secret "$COSF_PREFIX-cos-credentials" "$COSF_PREFIX-cos-backup-key" "$COSF_PREFIX-kp-apikey" "$COSF_PREFIX-cos-bad" -n "$RT_DRIVER_NS" --ignore-not-found
}
