#!/usr/bin/env bash
# 12-restarts — The driver survives its own restarts.
#   - The controller pod is deleted. A new one takes over, every pool and
#     volume is unchanged, and a new volume can still be made.
#   - A node agent pod is deleted. A pod that is writing to a volume keeps
#     writing, and no write is lost.
#
# This stage restarts the driver on the cluster. Run it only when nobody else
# is using the driver there.

RT_STAGE_TIMEOUT=1500

POOL=$(rt_name restart)
NS=$(rt_name restart)
WRITER=writer
BEFORE_STATE=""

create_pool() {
    oc apply -f - >/dev/null <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $POOL
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  zone: $(detect_zone)
  profile: dp2
  shareSizeGB: 10
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
YAML
}

claim() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $POOL
  resources:
    requests:
      storage: 1Gi
YAML
}

bound() { [ "$(oc get pvc -n "$NS" "$1" -o jsonpath='{.status.phase}')" = "Bound" ]; }

# state — the pool and its volumes, as they must be after a restart.
state() {
    oc get filesharepool "$POOL" -o json |
        jq -S '{phase: .status.phase, allocatedGB: .status.totalAllocatedGB,
                shares: ([.status.shares[]? | {shareID, state, mountTargetIP}] | sort_by(.shareID))}'
    oc get subvolumes.storage.ibmcloud.io -o json |
        jq -S --arg ns "$NS" '[.items[] | select(.spec.pvcNamespace == $ns) | {name: .metadata.name, share: .spec.shareID}] | sort_by(.name)'
}

state_unchanged() { [ "$(state)" = "$BEFORE_STATE" ]; }

controller_pods() { oc get pods -n "$RT_DRIVER_NS" -l "$RT_DRIVER_LABELS,app.kubernetes.io/component=controller" -o name | sort; }

# new_controller_ready OLD_PODS — no old controller pod is left, and every controller pod is Ready.
new_controller_ready() {
    local old="$1" now
    now=$(controller_pods)
    [ -n "$now" ] || return 1
    [ -z "$(comm -12 <(echo "$old") <(echo "$now"))" ] || return 1
    oc get pods -n "$RT_DRIVER_NS" -l "$RT_DRIVER_LABELS,app.kubernetes.io/component=controller" -o json |
        jq -e '[.items[] | .status.phase == "Running" and (.status.containerStatuses | all(.ready))] | all' >/dev/null
}

# new_node_agent_ready NODE OLD_POD — the node agent on NODE is a new pod, and Ready.
new_node_agent_ready() {
    local pod
    pod=$(node_agent_pod "$1") || return 1
    { [ -n "$pod" ] && [ "$pod" != "$2" ]; } || return 1
    oc get pod -n "$RT_DRIVER_NS" "$pod" -o json |
        jq -e '.status.phase == "Running" and (.status.containerStatuses | all(.ready))' >/dev/null
}

lines_written() { oc exec -n "$NS" "$WRITER" -- sh -c 'wc -l < /data/log' | tr -d ' '; }

# writes_continue — the writer's log grows in 30 seconds.
writes_continue() {
    local before after
    before=$(lines_written)
    sleep 30
    after=$(lines_written)
    echo "lines written: $before, then $after"
    [ "$after" -gt "$before" ]
}

# no_write_lost — the writer numbers its lines, so a gap in the numbers is a lost write.
no_write_lost() {
    local bad
    bad=$(oc exec -n "$NS" "$WRITER" -- cat /data/log | awk 'NR > 1 && $1 != prev + 1 {bad++} {prev = $1} END {print bad + 0}')
    echo "gaps in the numbers: $bad"
    [ "$bad" -eq 0 ]
}

run() {
    rt_namespace "$NS"
    create_pool
    check "the pool is Ready" wait_until 900 10 pool_is_ready "$POOL"
    pool_is_ready "$POOL" || return 0

    claim data
    wait_until 300 5 bound data
    local node
    node=$(worker_nodes | head -1)
    pod_on_node "$NS" "$WRITER" "$node" data 'i=0; while true; do echo $i >> /data/log; i=$((i+1)); sleep 1; done'
    check "the writer pod runs" wait_until 300 5 pod_is_running "$NS" "$WRITER"
    pod_is_running "$NS" "$WRITER" || return 0
    check "the writer is writing" writes_continue

    # --- The controller restarts ---
    local old_controllers
    BEFORE_STATE=$(state)
    old_controllers=$(controller_pods)
    oc delete pod -n "$RT_DRIVER_NS" -l "$RT_DRIVER_LABELS,app.kubernetes.io/component=controller" --wait=true >/dev/null
    check "a new controller pod takes over" wait_until 600 5 new_controller_ready "$old_controllers"
    check "the pool and its volumes are unchanged" wait_until 120 5 state_unchanged
    claim after-controller
    check "a new volume is made after the controller restarted" wait_until 300 5 bound after-controller

    # --- The node agent restarts ---
    local old_agent
    old_agent=$(node_agent_pod "$node")
    oc delete pod -n "$RT_DRIVER_NS" "$old_agent" --wait=true >/dev/null
    check "a new node agent pod starts on that node" wait_until 600 5 new_node_agent_ready "$node" "$old_agent"
    check "the writer keeps writing after the node agent restarted" writes_continue
    check "no write was lost across both restarts" no_write_lost
}

cleanup() {
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=600s
    oc delete filesharepool "$POOL" --ignore-not-found --wait=true --timeout=600s
}
