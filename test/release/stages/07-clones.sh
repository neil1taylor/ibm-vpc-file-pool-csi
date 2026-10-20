#!/usr/bin/env bash
# 07-clones — A clone of a claim has the source's files, in a namespace with no
# extra permissions; a pod using the clone starts only after the copy; the
# copy leaves nothing in the app's namespace; deleting the clone removes its
# folder from the share.

RT_STAGE_TIMEOUT=1200

BASIC=$(rt_name basic)
NS=$(rt_name clone)

claim() {
    local source=""
    [ -z "${2:-}" ] || source="
  dataSource:
    kind: PersistentVolumeClaim
    name: $2"
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
      storage: 1Gi$source
YAML
}

bound() { [ "$(oc get pvc -n "$NS" "$1" -o jsonpath='{.status.phase}')" = "Bound" ]; }
pv_of() { oc get pvc -n "$NS" "$1" -o jsonpath='{.spec.volumeName}'; }
clone_status() { oc get subvolumes.storage.ibmcloud.io "$1" -o jsonpath='{.status.cloneStatus}'; }
gone() { ! oc get "$@" >/dev/null 2>&1; }

files_match_source() {
    oc exec -n "$NS" "$1" -- sh -c 'cd /data && sha256sum -c original.sha256'
}

owner_and_mode() {
    local got; got=$(oc exec -n "$NS" "$1" -- stat -c '%u %g %a' /data)
    echo "owner and mode: $got"
    [ "$got" = "107 107 777" ]
}

copy_complete_when_pod_runs() {
    local status; status=$(clone_status "$1")
    echo "clone status when the pod runs: $status"
    [ "$status" = "Complete" ]
}

nothing_left_in_app_namespace() {
    local jobs claims
    jobs=$(oc get jobs -n "$NS" -o name | wc -l)
    claims=$(oc get pvc -n "$NS" -o name | grep -cv -e '/source$' -e '/copy$')
    echo "Jobs: $jobs, other claims: $claims"
    [ "$jobs" -eq 0 ] && [ "$claims" -eq 0 ]
}

run() {
    pool_is_ready "$BASIC" || skip "needs the Ready pool from stage 02"
    local nodes a b pv sub
    nodes=$(worker_nodes)
    a=$(echo "$nodes" | sed -n 1p)
    b=$(echo "$nodes" | sed -n 2p)
    [ -n "$b" ] || b="$a"
    rt_namespace "$NS"

    claim source
    check "source claim is bound" wait_until 300 5 bound source
    pod_on_node "$NS" writer "$a" source \
        "head -c 52428800 /dev/urandom > /data/payload && mkdir -p /data/dir && head -c 1048576 /dev/urandom > /data/dir/small && cd /data && sha256sum payload dir/small > original.sha256 && touch /data/written && sleep infinity"
    check "writer runs on $a" wait_until 300 5 pod_is_running "$NS" writer
    wait_until 120 5 oc exec -n "$NS" writer -- test -f /data/written

    claim copy source
    check "clone claim is bound" wait_until 300 5 bound copy
    pv=$(pv_of copy)
    pod_on_node "$NS" reader "$b" copy "sleep infinity"
    check "reader of the clone runs on $b" wait_until 600 5 pod_is_running "$NS" reader
    check "the copy had finished before the pod could use the clone" copy_complete_when_pod_runs "$pv"
    check "the clone has the source's files" files_match_source reader
    check "the clone has the pool's owner 107:107 and mode 777" owner_and_mode reader
    check "the copy left no Jobs or claims in the app's namespace" nothing_left_in_app_namespace

    sub=$(oc get subvolumes.storage.ibmcloud.io "$pv" -o jsonpath='{.spec.subPath}')
    oc delete pod -n "$NS" reader --wait=true --timeout=120s >/dev/null
    oc delete pvc -n "$NS" copy --wait=true --timeout=300s >/dev/null
    check "the clone's SubVolume is deleted" wait_until 300 5 gone subvolumes.storage.ibmcloud.io "$pv"
    check "the clone's folder is removed from the share" \
        wait_until 300 10 path_gone_from_share "$BASIC" "$a" "$sub"
}

cleanup() {
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=300s
}
