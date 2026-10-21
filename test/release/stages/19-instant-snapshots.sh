#!/usr/bin/env bash
# 19-instant-snapshots — With the KubeVirt Sidecar gate on, a snapshot of a
# running VM's disk is instant: a new top file in the disk's chain, no copy.
# test/e2e/test-vm-instant-snapshot.sh does the work and logs a "PASS:" line
# for each outcome, or one "FAIL:" line and exit 1. This stage runs it and
# checks the log for each outcome. With the gate off, the stage skips and
# stage 06 tests the QEMU copy instead.

# A CentOS image copy, a VM boot, a restore of two disks, two restarts, a live
# migration and two stopped-VM Jobs: about 45 to 75 minutes.
RT_STAGE_TIMEOUT=5400

HERE_STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$HERE_STAGE/../../e2e/test-vm-instant-snapshot.sh"
PREFIX=$(rt_name instant)
# cleanup runs in its own shell, so the key lives at a path both can compute.
KEY_DIR="${TMPDIR:-/tmp}/$PREFIX-key"
E2E_LOG="$RT_OUT/instant-snapshot-e2e.log"

run() {
    local tool rc fail_line
    for tool in oc virtctl jq perl ssh-keygen; do
        command -v "$tool" >/dev/null || skip "needs $tool"
    done
    oc get crd virtualmachines.kubevirt.io >/dev/null 2>&1 || skip "KubeVirt is not installed"
    sidecar_gate_on || skip "the KubeVirt Sidecar gate is off, so VM disk snapshots are QEMU copies (stage 06)"
    [ -f "$E2E" ] || skip "no end-to-end script at $E2E"

    mkdir -p "$KEY_DIR" && chmod 700 "$KEY_DIR"
    ssh-keygen -q -t ed25519 -N '' -f "$KEY_DIR/key" || skip "cannot make an ssh key"
    rt_namespace "$PREFIX-test" || skip "cannot make the namespace $PREFIX-test"

    rt_log "running $E2E (45 to 75 minutes)"
    env SSH_KEY="$KEY_DIR/key" PREFIX="$PREFIX" bash "$E2E" >"$E2E_LOG" 2>&1
    rc=$?
    cat "$E2E_LOG"

    if fail_line=$(first_fail_line "$E2E_LOG"); then
        check "the instant snapshot script reported: $fail_line" false
    elif [ "$rc" -ne 0 ]; then
        check "the instant snapshot script exited with $rc and no FAIL line (see instant-snapshot-e2e.log)" false
    else
        check "the instant snapshot script ended without a FAIL line" true
    fi

    check "a KubeVirt VM snapshot is instant and consistent, and the VM restored from it has every logged file" \
        log_has_pass "$E2E_LOG" "restored VM booted from an instant VM snapshot"
    check "a snapshot of a running VM's 100 GB disk is ready in under 10 s" \
        log_has_pass "$E2E_LOG" "100 GB disk:"
    check "after three snapshots, a live migration right after the last one and a restart, the VM opens the newest file and keeps its data" \
        log_has_pass "$E2E_LOG" "migration and restart:"
    check "deleting the middle, oldest and newest snapshot merges the chain back, and the VM keeps its data" \
        log_has_pass "$E2E_LOG" "deleting snapshots:"
    check "a snapshot and a delete of a stopped VM's disk run as Jobs, and the VM starts on the right file" \
        log_has_pass "$E2E_LOG" "stopped VM:"
    not_checked "a snapshot from the console's Snapshots tab of a catalog VM is instant" "manual: needs the console"
    not_checked "a Windows VM's instant snapshot is consistent through VSS" "manual: needs the Windows image"
    not_checked "a VM with a chain does not start without the hook" "manual: needs a hand-edited VM"
    not_checked "with the gate off, snapshots are copies and kubectl pool-snapshots flatten works" "manual: needs the gate turned off"
}

cleanup() {
    oc delete namespace "$PREFIX-test" --ignore-not-found --wait=true --timeout=900s
    oc delete volumesnapshotclass "$PREFIX-class" --ignore-not-found
    oc delete storageclass "$PREFIX-pool" --ignore-not-found
    oc delete filesharepool "$PREFIX-pool" --ignore-not-found --wait=false
    rm -rf "$KEY_DIR"
}
