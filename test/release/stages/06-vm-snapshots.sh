#!/usr/bin/env bash
# 06-vm-snapshots — A snapshot of a running VM that writes nonstop is made with
# a QEMU copy (not a file copy), says FileSystemConsistent, and names its
# moment. A claim restored from it boots a VM with no file system errors, and
# every file the writer logged is on the disk with the logged checksum.
# test/e2e/test-vm-snapshot.sh does the work and checks all of that.
# A second check takes a KubeVirt VirtualMachineSnapshot of a running VM with two
# disks (issue #14) and tests that the two restored disks fit each other.
# With the KubeVirt Sidecar gate on, VM disk snapshots are instant instead, so
# this stage skips and stage 19 runs.

RT_STAGE_TIMEOUT=5400

HERE_STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$HERE_STAGE/../../e2e/test-vm-snapshot.sh"
E2E2="$HERE_STAGE/../../e2e/test-vm-snapshot-2disk.sh"
PREFIX=$(rt_name vmsnap)
PREFIX2=$(rt_name vm2disk)

run() {
    local tool key
    for tool in virtctl perl jq ssh-keygen; do
        command -v "$tool" >/dev/null || skip "needs $tool"
    done
    sidecar_gate_on && skip "the KubeVirt Sidecar gate is on, so VM disk snapshots are instant, not QEMU copies; stage 19 tests them"
    key="$RT_OUT/vm-snapshot-key"
    [ -f "$key" ] || ssh-keygen -q -t ed25519 -N '' -f "$key"
    check "a snapshot of a running VM is consistent, restores, and the restored VM boots with all checksums matching" \
        env SSH_KEY="$key" PREFIX="$PREFIX" bash "$E2E"
    check "a KubeVirt snapshot of a running VM with two disks restores to disks that fit each other" \
        env SSH_KEY="$key" PREFIX="$PREFIX2" bash "$E2E2"
}

cleanup() {
    oc delete namespace "$PREFIX-test" --ignore-not-found --wait=true --timeout=300s
    oc delete volumesnapshotclass "$PREFIX-class" --ignore-not-found
    oc delete storageclass "$PREFIX-pool" --ignore-not-found
    oc delete filesharepool "$PREFIX-pool" --ignore-not-found --wait=false
    oc delete namespace "$PREFIX2-test" --ignore-not-found --wait=true --timeout=300s
    oc delete volumesnapshotclass "$PREFIX2-class" --ignore-not-found
    oc delete storageclass "$PREFIX2-pool" --ignore-not-found
    oc delete filesharepool "$PREFIX2-pool" --ignore-not-found --wait=false
}
