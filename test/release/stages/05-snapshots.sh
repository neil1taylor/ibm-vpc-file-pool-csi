#!/usr/bin/env bash
# 05-snapshots — A snapshot holds a copy of the volume's data at the time it
# was taken; a claim restored from it has the same files; deleting the
# snapshot removes its copy from the share.

RT_STAGE_TIMEOUT=1500

BASIC=$(rt_name basic)
NS=$(rt_name snap)
SNAPSHOT_CLASS="${RT_SNAPSHOT_CLASS:-ibm-vpc-file-pool}"

claim() {
    local source=""
    [ -z "${2:-}" ] || source="
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
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

take_snapshot() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: $1
spec:
  volumeSnapshotClassName: $SNAPSHOT_CLASS
  source:
    persistentVolumeClaimName: $2
YAML
}

bound() { [ "$(oc get pvc -n "$NS" "$1" -o jsonpath='{.status.phase}')" = "Bound" ]; }
snapshot_ready() { [ "$(oc get volumesnapshot -n "$NS" "$1" -o jsonpath='{.status.readyToUse}')" = "true" ]; }
gone() { ! oc get "$@" >/dev/null 2>&1; }

# snapshot_folder SNAPSHOT — the snapshot's folder on the share.
snapshot_folder() {
    local content handle
    content=$(oc get volumesnapshot -n "$NS" "$1" -o jsonpath='{.status.boundVolumeSnapshotContentName}')
    handle=$(oc get volumesnapshotcontent "$content" -o jsonpath='{.status.snapshotHandle}')
    oc get snapshots.storage.ibmcloud.io "${handle##*/}" -o jsonpath='{.spec.snapshotPath}'
}

files_match_original() {
    oc exec -n "$NS" "$1" -- sh -c 'cd /data && sha256sum -c original.sha256'
}

owner_and_mode() {
    local got; got=$(oc exec -n "$NS" "$1" -- stat -c '%u %g %a' /data)
    echo "owner and mode: $got"
    [ "$got" = "107 107 777" ]
}

run() {
    pool_is_ready "$BASIC" || skip "needs the Ready pool from stage 02"
    oc get volumesnapshotclass "$SNAPSHOT_CLASS" >/dev/null 2>&1 ||
        skip "no VolumeSnapshotClass $SNAPSHOT_CLASS"
    local nodes a b content folder
    nodes=$(worker_nodes)
    a=$(echo "$nodes" | sed -n 1p)
    b=$(echo "$nodes" | sed -n 2p)
    [ -n "$b" ] || b="$a"
    rt_namespace "$NS"

    claim source
    check "source claim is bound" wait_until 300 5 bound source
    pod_on_node "$NS" writer "$a" source \
        "head -c 20971520 /dev/urandom > /data/payload && mkdir -p /data/dir && head -c 1048576 /dev/urandom > /data/dir/small && cd /data && sha256sum payload dir/small > original.sha256 && touch /data/written && sleep infinity"
    check "writer runs on $a" wait_until 300 5 pod_is_running "$NS" writer
    wait_until 120 5 oc exec -n "$NS" writer -- test -f /data/written

    take_snapshot snap source
    check "the snapshot becomes ready to use" wait_until 600 10 snapshot_ready snap
    folder=$(snapshot_folder snap)
    echo "snapshot folder: $folder"
    check "the snapshot's folder is on the share" path_on_share "$BASIC" "$a" "$folder"

    # Change the source after the snapshot. The restore must have the old data.
    oc exec -n "$NS" writer -- sh -c 'head -c 1048576 /dev/urandom > /data/payload'
    check "the source has changed since the snapshot" \
        sh -c "! oc exec -n '$NS' writer -- sh -c 'cd /data && sha256sum -c original.sha256' >/dev/null 2>&1"

    claim restored snap
    check "restored claim is bound" wait_until 900 10 bound restored
    pod_on_node "$NS" reader "$b" restored "sleep infinity"
    check "reader of the restored claim runs on $b" wait_until 300 5 pod_is_running "$NS" reader
    check "the restored files match the data at snapshot time" files_match_original reader
    check "the restored volume has the pool's owner 107:107 and mode 777" owner_and_mode reader

    oc delete pod -n "$NS" reader --wait=true --timeout=120s >/dev/null
    oc delete pvc -n "$NS" restored --wait=true --timeout=300s >/dev/null
    content=$(oc get volumesnapshot -n "$NS" snap -o jsonpath='{.status.boundVolumeSnapshotContentName}')
    oc delete volumesnapshot -n "$NS" snap --wait=true --timeout=300s >/dev/null
    check "the snapshot content is deleted" wait_until 300 5 gone volumesnapshotcontent "$content"
    check "the snapshot's folder is removed from the share" \
        wait_until 300 10 path_gone_from_share "$BASIC" "$a" "$folder"
}

cleanup() {
    oc delete volumesnapshot -n "$NS" --all --ignore-not-found --wait=true --timeout=300s
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=300s
}
