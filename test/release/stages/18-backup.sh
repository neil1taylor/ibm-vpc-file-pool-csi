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
#
# Second part, the COS export (docs/specs/2026-10-21-backup-cos-export.md, part 6).
# When COS settings are present (test/e2e/lib/backup-cos.sh lists them: an
# existing COS bucket, an HMAC key file, a Key Protect API key file and root key,
# and the wrapped backup key manifest, read from ~/.config/pool-csi), the same
# script also exports the policy to COS (Compliance lock, 25 hours, a prefix new
# for each run), checks exportedTime and exportedBytes, runs `kubectl backup
# cos-list` (one export of each volume is enough: an export holds the earlier moments as undo files), and restores the three volumes from COS into a second namespace: the
# VM boots with matching checksums and the pod volume holds the first and the
# changed content. COS_DR_KUBECONFIG repeats the restore in the other cluster of
# the lab pair. Without settings these checks are reported as not checked.
# Objects locked by Compliance cannot be deleted: they stay in the bucket.

# The e2e script waits for two backups of every volume (up to 90 minutes each
# wait) after a CentOS image copy and a VM boot. Measured on rovslab it takes
# 60 to 90 minutes, so the stage gets 100.
# With the COS part it adds the export waits (up to 30 minutes each), one restore of three
# volumes and a VM boot here, and the same in the other cluster: 180 minutes.
RT_STAGE_TIMEOUT=10800

HERE_STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$HERE_STAGE/../../e2e/test-backup.sh"
# shellcheck source=../../e2e/lib/backup-cos.sh
source "$HERE_STAGE/../../e2e/lib/backup-cos.sh"
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

    cos_load_settings "$(cos_config_dir)/cos-stage.env"
    if cos_wanted && ! cos_missing_config >"$RT_OUT/backup-cos-missing.txt"; then
        check "the COS settings are complete (see backup-cos-missing.txt)" false
        cat "$RT_OUT/backup-cos-missing.txt"
        return 0
    fi

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

    if cos_wanted; then
        check "COS export: all three volumes have exportedTime and exportedBytes above 0" \
            log_has_pass "$E2E_LOG" "COS export: all three volumes"
        check "COS export: kubectl backup cos-list shows an export of each volume (an export holds the earlier moments as undo files)" \
            log_has_pass "$E2E_LOG" "COS export: kubectl backup cos-list"
        cos_restore_checks "local cluster"
        if [ -n "${COS_DR_KUBECONFIG:-}" ]; then
            cos_restore_checks "other cluster"
        else
            not_checked "restore from COS in the other cluster" "COS_DR_KUBECONFIG is not set"
        fi
        log_note_locked
    else
        not_checked "export to COS and restore from COS" "no COS settings (COS_BUCKET); see test/e2e/lib/backup-cos.sh"
    fi
}

# cos_restore_checks LABEL — the three outcomes of a restore from COS.
cos_restore_checks() {
    check "COS restore, $1: the VM restored from COS boots with no XFS errors and every logged file has its checksum" \
        log_has_pass "$E2E_LOG" "COS restore, $1: the VM restored at"
    check "COS restore, $1: the pod volume restored at the first moment holds the first content" \
        log_has_pass "$E2E_LOG" "COS restore, $1: the pod volume restored at"
    check "COS restore, $1: the pod volume restored at the latest moment holds the changed content" \
        log_has_pass "$E2E_LOG" "COS restore, $1: the latest restore"
}

# log_note_locked — say, in the stage report, that the locked objects stay.
log_note_locked() {
    local prefix
    prefix=$(grep -m1 -o 'COS prefix: [^ ]*' "$E2E_LOG" | cut -d' ' -f3)
    [ -z "$prefix" ] || _record NOTE "objects under s3://$COS_BUCKET/$prefix/ are locked (Compliance, 25h) and were left in the bucket"
}

cleanup() {
    oc get backuprestore -o json 2>/dev/null |
        jq -r --arg ns "$PREFIX-test" '.items[] | select(.spec.targetNamespace == $ns) | .metadata.name' |
        while read -r name; do oc delete backuprestore "$name" --ignore-not-found --wait=false; done
    oc delete backuppolicy "$PREFIX-policy" --ignore-not-found --wait=false
    oc delete namespace "$PREFIX-test" --ignore-not-found --wait=true --timeout=900s
    oc delete filesharepool "$PREFIX-src" "$PREFIX-snap" --ignore-not-found --wait=false
    rm -rf "$KEY_DIR"
    # The e2e script cleans up after itself unless it was killed by the time limit.
    # The COS objects stay in the bucket: a Compliance lock cannot be removed.
    COS_NS="$PREFIX-test-cos" COSR_POOL="$PREFIX-cosr" DRIVER_NS="$RT_DRIVER_NS"
    cos_load_settings "$(cos_config_dir)/cos-stage.env"
    cos_cleanup_cluster
    if [ -n "${COS_DR_KUBECONFIG:-}" ] && [ -r "$COS_DR_KUBECONFIG" ]; then
        COS_HOME_KC_SET=${KUBECONFIG+x} COS_HOME_KC=${KUBECONFIG:-}
        cos_with_kubeconfig "$COS_DR_KUBECONFIG" cos_cleanup_cluster || true
    fi
}
