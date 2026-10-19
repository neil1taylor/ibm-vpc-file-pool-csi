#!/usr/bin/env bash
# 03-volumes — Pods on two workers share one volume; the volume has the pool's
# owner and permissions; deleting the claim removes its record and directory.

RT_STAGE_TIMEOUT=900

BASIC=$(rt_name basic)
NS=$(rt_name vol)

claim() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $BASIC
  resources:
    requests:
      storage: 1Gi
YAML
}

bound() { [ "$(oc get pvc -n "$NS" "$1" -o jsonpath='{.status.phase}')" = "Bound" ]; }
pv_of() { oc get pvc -n "$NS" "$1" -o jsonpath='{.spec.volumeName}'; }

reader_sees_writer_data() {
    wait_until 120 5 oc exec -n "$NS" reader -- test -f /data/payload.sha256 &&
        oc exec -n "$NS" reader -- sh -c 'cd /data && sha256sum -c payload.sha256'
}

owner_and_mode() {
    local got; got=$(oc exec -n "$NS" writer -- stat -c '%u %g %a' /data)
    echo "owner and mode: $got"
    [ "$got" = "107 107 777" ]
}

subvolume_recorded() {
    [ "$(oc get subvolumes.storage.ibmcloud.io "$1" -o jsonpath='{.spec.poolName}')" = "$BASIC" ]
}

gone() { ! oc get "$@" >/dev/null 2>&1; }

# The anchor pod keeps the share mounted on its node, so the node agent can
# show whether the deleted volume's directory is still on the share.
dir_on_share() {
    local node="$1" sub="$2" export_path mnt
    export_path=$(pool_field "$BASIC" '.status.shares[0].exportPath')
    mnt=$(share_mount_on_node "$node" "$export_path")
    [ -n "$mnt" ] || { echo "share $export_path is not mounted on $node"; return 2; }
    oc exec -n "$RT_DRIVER_NS" "$(node_agent_pod "$node")" -c csi-node -- test -d "$mnt$sub"
}

dir_removed() {
    dir_on_share "$1" "$2"
    [ $? -eq 1 ]
}

run() {
    pool_is_ready "$BASIC" || skip "needs the Ready pool from stage 02"
    local nodes a b pv sub
    nodes=$(worker_nodes)
    a=$(echo "$nodes" | sed -n 1p)
    b=$(echo "$nodes" | sed -n 2p)
    [ -n "$b" ] || skip "needs two schedulable workers"
    rt_namespace "$NS"

    claim shared
    claim anchor
    check "claims are bound" wait_until 300 5 bound shared
    wait_until 300 5 bound anchor
    pod_on_node "$NS" anchor "$a" anchor "sleep infinity"
    pod_on_node "$NS" writer "$a" shared \
        "head -c 10485760 /dev/urandom > /data/payload && cd /data && sha256sum payload > payload.sha256 && sleep infinity"
    pod_on_node "$NS" reader "$b" shared "sleep infinity"
    check "writer runs on $a" wait_until 300 5 pod_is_running "$NS" writer
    check "reader runs on $b" wait_until 300 5 pod_is_running "$NS" reader
    wait_until 300 5 pod_is_running "$NS" anchor
    check "reader on another worker reads the writer's data intact" reader_sees_writer_data
    check "volume has the pool's owner 107:107 and mode 777" owner_and_mode

    pv=$(pv_of shared)
    sub=$(oc get subvolumes.storage.ibmcloud.io "$pv" -o jsonpath='{.spec.subPath}')
    check "a SubVolume records the volume in the pool" subvolume_recorded "$pv"
    check "the volume's directory is on the share" dir_on_share "$a" "$sub"

    oc delete pod -n "$NS" writer reader --wait=true --timeout=120s >/dev/null
    oc delete pvc -n "$NS" shared --wait=true --timeout=120s >/dev/null
    check "the PV is deleted" wait_until 180 5 gone pv "$pv"
    check "the SubVolume is deleted" wait_until 180 5 gone subvolumes.storage.ibmcloud.io "$pv"
    check "the volume's directory is removed from the share" wait_until 180 10 dir_removed "$a" "$sub"
}

cleanup() {
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=300s
}
