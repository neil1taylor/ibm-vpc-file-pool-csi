#!/usr/bin/env bash
# lib.sh — Helpers for release test stages. Sourced by run.sh and by each stage.
#
# A stage file defines run() and cleanup(). Inside run(), record each result
# with `check`, or end the stage with `skip`. Set RT_STAGE_TIMEOUT (seconds)
# at the top of the stage file to change its time limit.

RT_LABEL="release-test-run"

rt_log() { echo "[$(date -u +%H:%M:%S)] ${RT_STAGE:-run}: $*"; }

# rt_name SUFFIX — name for an object this run owns, e.g. rt-1019-2215-basic.
rt_name() { echo "rt-${RT_ID}-$1"; }

_record() { printf '%s\t%s\t%s\n' "$1" "$RT_STAGE" "$2" >>"$RT_OUT/results.tsv"; }

# check DESC CMD... — run CMD; record PASS or FAIL. Never stops the stage.
check() {
    local desc="$1"; shift
    if "$@" >>"$RT_OUT/$RT_STAGE.log" 2>&1; then
        _record PASS "$desc"
        rt_log "PASS $desc"
    else
        _record FAIL "$desc"
        rt_log "FAIL $desc"
    fi
    return 0
}

# skip REASON — record why the stage cannot run, and end it.
skip() {
    _record SKIP "$1"
    rt_log "SKIP $1"
    exit 0
}

# not_checked DESC REASON — a check that cannot run here. Reported, but it
# does not change the stage result.
not_checked() {
    _record NOTE "not checked: $1 ($2)"
    rt_log "NOT CHECKED $1 ($2)"
}

# wait_until TIMEOUT INTERVAL CMD... — retry CMD until it succeeds or TIMEOUT seconds pass.
wait_until() {
    local timeout="$1" interval="$2"; shift 2
    local end=$(( $(date +%s) + timeout ))
    until "$@"; do
        [ "$(date +%s)" -ge "$end" ] && return 1
        sleep "$interval"
    done
}

# rt_labels — the label every object of this run carries, for the final sweep.
rt_labels() { echo "${RT_LABEL}=${RT_ID}"; }

# --- Cluster helpers ---------------------------------------------------------

RT_DRIVER_NS="${RT_DRIVER_NS:-kube-system}"
RT_DRIVER_LABELS="app.kubernetes.io/name=ibm-vpc-file-pool-csi"
RT_PROVISIONER="vpc-file-pool.csi.ibm.io"
RT_IMAGE="${RT_IMAGE:-quay.io/centos/centos:stream9}"

# rt_namespace NAME — create a namespace that the final sweep deletes.
rt_namespace() {
    oc create namespace "$1" >/dev/null && oc label namespace "$1" "$(rt_labels)" >/dev/null
}

# pool_field POOL JSONPATH — print one field of a FileSharePool.
pool_field() { oc get filesharepool "$1" -o jsonpath="{$2}" 2>/dev/null; }

pool_is_ready() { [ "$(pool_field "$1" .status.phase)" = "Ready" ]; }

# worker_nodes — schedulable worker node names, one per line.
worker_nodes() {
    oc get nodes -l node-role.kubernetes.io/worker -o json |
        jq -r '.items[] | select(.spec.unschedulable != true) | .metadata.name'
}

# node_agent_pod NODE — the driver's node agent pod on NODE.
node_agent_pod() {
    oc get pods -n "$RT_DRIVER_NS" -l "$RT_DRIVER_LABELS,app.kubernetes.io/component=node" \
        --field-selector "spec.nodeName=$1" -o jsonpath='{.items[0].metadata.name}'
}

# share_mount_on_node NODE EXPORT — the node's mount point of the share with EXPORT,
# as seen by the node agent. Empty if the share is not mounted there.
share_mount_on_node() {
    oc exec -n "$RT_DRIVER_NS" "$(node_agent_pod "$1")" -c csi-node -- \
        awk -v e=":$2" 'index($1, e) && substr($1, length($1) - length(e) + 1) == e {print $2; exit}' /proc/mounts
}

# path_on_share POOL NODE PATH — is PATH (a volume or snapshot folder) on the
# pool's first share? Checked through the node agent's mount of the share on
# NODE, so a pod on NODE must be using the share. Exit 0 = present,
# 1 = absent, 2 = the share is not mounted on NODE.
path_on_share() {
    local export_path mnt
    export_path=$(pool_field "$1" '.status.shares[0].exportPath')
    mnt=$(share_mount_on_node "$2" "$export_path")
    [ -n "$mnt" ] || { echo "share $export_path is not mounted on $2"; return 2; }
    oc exec -n "$RT_DRIVER_NS" "$(node_agent_pod "$2")" -c csi-node -- test -d "$mnt$3"
}

path_gone_from_share() {
    path_on_share "$@"
    [ $? -eq 1 ]
}

# pod_on_node NS NAME NODE PVC COMMAND — run a pod with PVC at /data, pinned to NODE.
pod_on_node() {
    oc apply -n "$1" -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: $2
spec:
  nodeName: $3
  restartPolicy: Never
  containers:
    - name: main
      image: $RT_IMAGE
      command: ["/bin/sh", "-c", "$5"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $4
YAML
}

pod_is_running() { [ "$(oc get pod -n "$1" "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; }

# shellcheck source=../e2e/lib/zone.sh
source "$(dirname "${BASH_SOURCE[0]}")/../e2e/lib/zone.sh"
# shellcheck source=vpc.sh
source "$(dirname "${BASH_SOURCE[0]}")/vpc.sh"
