#!/usr/bin/env bash
# 06-vm-snapshots — A snapshot of a running VM that writes nonstop is made with
# a QEMU copy (not a file copy), says FileSystemConsistent, and names its
# moment. A claim restored from it boots a VM with no file system errors, and
# every file the writer logged is on the disk with the logged checksum.
# test/e2e/test-vm-snapshot.sh does the work and checks all of that.

RT_STAGE_TIMEOUT=5400

HERE_STAGE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
E2E="$HERE_STAGE/../../e2e/test-vm-snapshot.sh"
PREFIX=$(rt_name vmsnap)

run() {
    local tool key
    for tool in virtctl perl jq ssh-keygen; do
        command -v "$tool" >/dev/null || skip "needs $tool"
    done
    key="$RT_OUT/vm-snapshot-key"
    [ -f "$key" ] || ssh-keygen -q -t ed25519 -N '' -f "$key"
    check "a snapshot of a running VM is consistent, restores, and the restored VM boots with all checksums matching" \
        env SSH_KEY="$key" PREFIX="$PREFIX" bash "$E2E"
}

cleanup() {
    oc delete namespace "$PREFIX-test" --ignore-not-found --wait=true --timeout=300s
    oc delete volumesnapshotclass "$PREFIX-class" --ignore-not-found
    oc delete storageclass "$PREFIX-pool" --ignore-not-found
    oc delete filesharepool "$PREFIX-pool" --ignore-not-found --wait=false
}
