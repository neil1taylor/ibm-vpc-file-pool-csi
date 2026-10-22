#!/usr/bin/env bash
# 20-backup-files — Browse and restore single files from a backup
# (docs/specs/2026-10-22-backup-file-restore.md; the spec calls this stage 19,
# but 19 is the instant snapshots stage).
# Part 1, the browse helper: on a real share, filled by three users, the helper
# (root, no capabilities, read-only mount) reads another user's files with mode
# 0600 and 0700, refuses a link, cannot write, and tars everything.
# test/e2e/test-backup-files.sh does the work and logs a "PASS:" line for each
# outcome, or one "FAIL:" line and exit 1. This stage runs it and checks the log.

# A pool takes 5 to 15 minutes to become ready; the rest is under 5 minutes.
RT_STAGE_TIMEOUT=2700

HERE_STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$HERE_STAGE/../../e2e/test-backup-files.sh"
PREFIX=$(rt_name files)
E2E_LOG="$RT_OUT/backup-files-e2e.log"

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
}

cleanup() {
    oc delete pod "$PREFIX-browse" -n "$RT_DRIVER_NS" --ignore-not-found --wait=true
    oc delete pvc "$PREFIX-share" -n "$RT_DRIVER_NS" --ignore-not-found
    oc delete pv "$PREFIX-share" --ignore-not-found
    oc delete secret "$PREFIX-token" -n "$RT_DRIVER_NS" --ignore-not-found
    oc delete namespace "$PREFIX-test" --ignore-not-found --wait=true --timeout=900s
    oc delete filesharepool "$PREFIX-pool" --ignore-not-found --wait=false
}
