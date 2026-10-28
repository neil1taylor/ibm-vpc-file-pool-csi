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

# A pool takes 5 to 15 minutes to become ready; the rest is under 5 minutes.
RT_STAGE_TIMEOUT=7200

HERE_STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$HERE_STAGE/../../e2e/test-backup-files.sh"
PREFIX=$(rt_name files)
E2E_LOG="$RT_OUT/backup-files-e2e.log"
E2E_SESSION="$HERE_STAGE/../../e2e/test-backup-browse-session.sh"
SESSION_PREFIX=$(rt_name bsess)
SESSION_LOG="$RT_OUT/backup-browse-session-e2e.log"
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
}
