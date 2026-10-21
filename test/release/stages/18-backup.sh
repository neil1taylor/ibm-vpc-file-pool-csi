#!/usr/bin/env bash
# 18-backup — A BackupPolicy backs up a running VM (two disks) and a pod volume
# to a separate snapshot pool. The policy lists all three volumes, the VM disks
# as FileSystemConsistent and the pod volume as FileCopy. Restoring the VM
# disks at the first moment gives a VM that boots with no XFS errors and every
# logged file present with its checksum. The pod volume restored at the first
# moment holds the first content, at the latest moment the changed content, and
# it can be restored after the claim is deleted.
# test/e2e/test-backup.sh does the work and logs a "PASS:" line for each of
# those outcomes, or one "FAIL:" line and exit 1. This stage runs it and checks
# the log for each outcome. The restores use the kubectl-backup plugin, which
# remote.sh builds into test/release/bin.

# The e2e script waits for two backups of every volume (up to 90 minutes each
# wait) after a CentOS image copy and a VM boot. Measured on rovslab it takes
# 60 to 90 minutes, so the stage gets 100.
RT_STAGE_TIMEOUT=6000

HERE_STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$HERE_STAGE/../../e2e/test-backup.sh"
BACKUP_BIN="${RT_BACKUP_BIN:-$(dirname "$RT_LIB")/bin/kubectl-backup}"
PREFIX=$(rt_name backup)
# cleanup runs in its own shell, so the key lives at a path both can compute.
KEY_DIR="${TMPDIR:-/tmp}/$PREFIX-key"
E2E_LOG="$RT_OUT/backup-e2e.log"

run() {
    local tool rc fail_line
    for tool in oc virtctl jq ssh-keygen; do
        command -v "$tool" >/dev/null || skip "needs $tool"
    done
    oc get crd virtualmachines.kubevirt.io >/dev/null 2>&1 || skip "KubeVirt is not installed"
    oc get crd backuppolicies.storage.ibmcloud.io >/dev/null 2>&1 || skip "the BackupPolicy CRD is not installed"
    [ -x "$BACKUP_BIN" ] || skip "no kubectl-backup binary at $BACKUP_BIN"
    [ -f "$E2E" ] || skip "no end-to-end script at $E2E"

    mkdir -p "$KEY_DIR" && chmod 700 "$KEY_DIR"
    ssh-keygen -q -t ed25519 -N '' -f "$KEY_DIR/key" || skip "cannot make an ssh key"
    # The namespace carries the run's label, so the final sweep finds it.
    rt_namespace "$PREFIX-test" || skip "cannot make the namespace $PREFIX-test"

    rt_log "running $E2E (60 to 90 minutes)"
    env SSH_KEY="$KEY_DIR/key" BACKUP_BIN="$BACKUP_BIN" PREFIX="$PREFIX" bash "$E2E" >"$E2E_LOG" 2>&1
    rc=$?
    cat "$E2E_LOG"

    if fail_line=$(first_fail_line "$E2E_LOG"); then
        check "the backup script reported: $fail_line" false
    elif [ "$rc" -ne 0 ]; then
        check "the backup script exited with $rc and no FAIL line (see backup-e2e.log)" false
    else
        check "the backup script ended without a FAIL line" true
    fi

    check "the policy backs up three volumes: two VM disks FileSystemConsistent, the pod volume FileCopy, two moments each" \
        log_has_pass "$E2E_LOG" "three volumes backed up"
    check "the VM restored at the first moment boots with no XFS errors and every logged file has its checksum" \
        log_has_pass "$E2E_LOG" "the VM restored at"
    check "the pod volume restored at the first moment holds the first content" \
        log_has_pass "$E2E_LOG" "holds the first content"
    check "the pod volume restored at the latest moment holds the changed content" \
        log_has_pass "$E2E_LOG" "the latest restore holds the changed content"
    check "a pod volume whose claim was deleted can be restored from its backup" \
        log_has_pass "$E2E_LOG" "a volume that was deleted can be restored"
}

cleanup() {
    oc get backuprestore -o json 2>/dev/null |
        jq -r --arg ns "$PREFIX-test" '.items[] | select(.spec.targetNamespace == $ns) | .metadata.name' |
        while read -r name; do oc delete backuprestore "$name" --ignore-not-found --wait=false; done
    oc delete backuppolicy "$PREFIX-policy" --ignore-not-found --wait=false
    oc delete namespace "$PREFIX-test" --ignore-not-found --wait=true --timeout=900s
    oc delete filesharepool "$PREFIX-src" "$PREFIX-snap" --ignore-not-found --wait=false
    rm -rf "$KEY_DIR"
}
