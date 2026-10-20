#!/usr/bin/env bash
# 14-migration — kubectl-migrate copies a claim from IBM's own VPC File class
# into a pool. The new claim is bound on the pool and has the same files; the
# source claim is not changed or deleted.

RT_STAGE_TIMEOUT=1800

POOL=$(rt_name migrate)
NS=$(rt_name migrate)
SOURCE_SC="${RT_BASELINE_SC:-ibmc-vpc-file-3000-iops}"
MIGRATE="${RT_MIGRATE_BIN:-$(dirname "$RT_LIB")/bin/kubectl-migrate}"

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
  shareSizeGB: 300
  iops: 3000
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
YAML
}

source_claim() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: source
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $SOURCE_SC
  resources:
    requests:
      storage: 200Gi
YAML
}

bound() { [ "$(oc get pvc -n "$NS" "$1" -o jsonpath='{.status.phase}')" = "Bound" ]; }

class_of() { oc get pvc -n "$NS" "$1" -o jsonpath='{.spec.storageClassName}'; }

files_match() {
    oc exec -n "$NS" "$1" -- sh -c 'cd /data && sha256sum -c original.sha256'
}

run() {
    [ -x "$MIGRATE" ] || skip "no kubectl-migrate binary at $MIGRATE"
    oc get sc "$SOURCE_SC" >/dev/null 2>&1 || skip "no StorageClass $SOURCE_SC to migrate from"
    local node
    node=$(worker_nodes | sed -n 1p)

    create_pool
    check "pool reaches Ready" wait_until 600 10 pool_is_ready "$POOL"
    rt_namespace "$NS"

    source_claim
    check "source claim on $SOURCE_SC is bound" wait_until 600 5 bound source
    pod_on_node "$NS" writer "$node" source \
        "head -c 52428800 /dev/urandom > /data/payload && mkdir -p /data/dir && head -c 1048576 /dev/urandom > /data/dir/small && cd /data && sha256sum payload dir/small > original.sha256 && touch /data/written && sleep infinity"
    check "writer runs on $node" wait_until 600 5 pod_is_running "$NS" writer
    wait_until 120 5 oc exec -n "$NS" writer -- test -f /data/written
    oc delete pod -n "$NS" writer --wait=true --timeout=120s >/dev/null

    check "kubectl-migrate execute succeeds" \
        "$MIGRATE" execute --namespace "$NS" --pvc source --target-pool "$POOL" --target-storage-class "$POOL"
    check "the new claim source-pooled is bound" wait_until 300 5 bound source-pooled
    check "the new claim uses the pool's class" \
        test "$(class_of source-pooled)" = "$POOL"
    check "the source claim is still bound on $SOURCE_SC" \
        test "$(class_of source)" = "$SOURCE_SC"

    pod_on_node "$NS" reader "$node" source-pooled "sleep infinity"
    check "a pod on the new claim runs" wait_until 600 5 pod_is_running "$NS" reader
    check "the new claim has the source's files" files_match reader
}

cleanup() {
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=600s
    oc delete filesharepool "$POOL" --ignore-not-found --wait=true --timeout=600s
}
