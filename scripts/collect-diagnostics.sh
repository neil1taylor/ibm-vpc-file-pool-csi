#!/usr/bin/env bash
# collect-diagnostics.sh — Collect what support needs about the driver into one text file.
#
# Usage: bash scripts/collect-diagnostics.sh [output-dir]
# Environment: NS (driver namespace, default kube-system), KUBECTL (default kubectl).
#
# It reads the driver's custom resources, pods, logs and events. It never reads a
# Secret, and it removes values that look like keys, tokens or passwords. Read the
# file before you send it.

set -uo pipefail

NS="${NS:-kube-system}"
KUBECTL="${KUBECTL:-kubectl}"
OUTDIR="${1:-.}"
FILE="$OUTDIR/csi-diagnostics-$(date +%Y%m%d-%H%M%S).txt"
SEL="app.kubernetes.io/name=ibm-vpc-file-pool-csi"
API="storage.ibmcloud.io"

# Remove the value of anything that looks like a key, token or password.
redact() {
    sed -E 's/((api[_-]?key|token|password|passwd|secret|authorization|bearer)[A-Za-z0-9_"-]*[[:space:]]*[:=][[:space:]]*)[^[:space:],]+/\1[redacted]/Ig'
}

section() { printf '\n=== %s ===\n' "$1"; }

# run DESCRIPTION COMMAND... — one section; a failed command says so and the run goes on.
run() {
    section "$1"
    shift
    "$@" 2>&1 || echo "(command failed: $*)"
}

collect() {
    section "Collection time"
    date -u

    section "Driver version"
    "$KUBECTL" get deployment -n "$NS" ibm-vpc-file-pool-csi-controller \
        -o jsonpath='{.spec.template.spec.containers[0].image}' 2>&1 || echo "(command failed)"
    echo

    run "Cluster version" "$KUBECTL" version
    run "Nodes" "$KUBECTL" get nodes -o wide
    run "CSI driver registration" "$KUBECTL" get csidriver vpc-file-pool.csi.ibm.io -o yaml

    run "Pools" "$KUBECTL" get "filesharepools.$API" -o yaml
    run "SubVolumes" "$KUBECTL" get "subvolumes.$API" -o wide
    run "Snapshots" "$KUBECTL" get "snapshots.$API" -o yaml
    run "Backup policies" "$KUBECTL" get "backuppolicies.$API" -o yaml
    run "Backup restores" "$KUBECTL" get "backuprestores.$API" -o yaml
    run "Performance test runs" "$KUBECTL" get "poolbenches.$API" -o yaml
    run "Replication policies" "$KUBECTL" get "replicationpolicies.$API" -o yaml

    section "Pending PVCs"
    "$KUBECTL" get pvc --all-namespaces 2>&1 | grep -E "NAMESPACE|Pending" || echo "(none)"

    run "Controller pods" "$KUBECTL" get pods -n "$NS" -l "$SEL,app.kubernetes.io/component=controller" -o wide
    run "Node agent pods" "$KUBECTL" get pods -n "$NS" -l "$SEL,app.kubernetes.io/component=node" -o wide
    run "Leader lease" "$KUBECTL" get lease -n "$NS" ibm-vpc-file-pool-csi-leader -o yaml

    run "Controller logs (last 500 lines)" "$KUBECTL" logs -n "$NS" \
        -l "$SEL,app.kubernetes.io/component=controller" -c csi-controller --tail=500

    section "Node agent logs (last 200 lines per pod)"
    for pod in $("$KUBECTL" get pods -n "$NS" -l "$SEL,app.kubernetes.io/component=node" \
        -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
        echo "--- Pod: $pod ---"
        "$KUBECTL" logs -n "$NS" "$pod" -c csi-node --tail=200 2>&1 || echo "(command failed)"
    done

    section "Events (newest 100)"
    "$KUBECTL" get events --all-namespaces --sort-by='.lastTimestamp' 2>&1 | tail -100
}

mkdir -p "$OUTDIR"
collect 2>&1 | redact > "$FILE"
echo "Diagnostics saved to $FILE"
echo "No Secret is read and key-like values are removed. Read the file before you send it."
