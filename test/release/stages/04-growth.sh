#!/usr/bin/env bash
# 04-growth — A resized claim gets its new size in the pool's accounts; a pool
# with auto-expand adds a usable second share when it passes its threshold,
# and new volumes go on it.

RT_STAGE_TIMEOUT=1500

GROW=$(rt_name grow)
NS=$(rt_name grow)

claim() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $GROW
  resources:
    requests:
      storage: $2
YAML
}

bound() { [ "$(oc get pvc -n "$NS" "$1" -o jsonpath='{.status.phase}')" = "Bound" ]; }
pv_of() { oc get pvc -n "$NS" "$1" -o jsonpath='{.spec.volumeName}'; }
claim_capacity() { [ "$(oc get pvc -n "$NS" "$1" -o jsonpath='{.status.capacity.storage}')" = "$2" ]; }
subvolume_gb() { [ "$(oc get subvolumes.storage.ibmcloud.io "$1" -o jsonpath='{.spec.requestedGB}')" = "$2" ]; }
pool_allocated() { [ "$(pool_field "$GROW" .status.totalAllocatedGB)" = "$1" ]; }
share_of() { oc get subvolumes.storage.ibmcloud.io "$1" -o jsonpath='{.spec.shareID}'; }

two_usable_shares() {
    [ "$(pool_field "$GROW" '.status.shares[1].state')" = "stable" ] &&
        [ -n "$(pool_field "$GROW" '.status.shares[1].mountTargetIP')" ] &&
        [ -n "$(pool_field "$GROW" '.status.shares[1].exportPath')" ]
}

no_more_than_two_shares() {
    local n; n=$(oc get filesharepool "$GROW" -o json | jq '.status.shares | length')
    echo "shares: $n"
    [ "$n" -le 2 ]
}

second_share_in_vpc() {
    local name; name=$(pool_field "$GROW" '.status.shares[1].shareName')
    vpc_share_names | grep -qx "$name"
}

run() {
    rt_namespace "$NS"
    oc apply -f - >/dev/null <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $GROW
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  zone: $(detect_zone)
  profile: dp2
  shareSizeGB: 10
  maxShares: 2
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 50
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
YAML
    check "pool with auto-expand reaches Ready" wait_until 600 10 pool_is_ready "$GROW"
    pool_is_ready "$GROW" || return 0

    # Resize: 3 GiB to 4 GiB.
    claim first 3Gi
    check "first claim is bound" wait_until 300 5 bound first
    local first_pv; first_pv=$(pv_of first)
    oc patch pvc -n "$NS" first --type=merge -p '{"spec":{"resources":{"requests":{"storage":"4Gi"}}}}' >/dev/null
    check "the resized claim reports 4Gi" wait_until 300 5 claim_capacity first 4Gi
    check "its SubVolume records 4 GB" subvolume_gb "$first_pv" 4
    check "the pool counts 4 GB allocated" wait_until 120 5 pool_allocated 4
    not_checked "a pod sees the new size" "an NFS folder shows the whole share's size, not the claim's"

    # Auto-expand: 4 + 3 = 7 of 10 GB is above the 50% threshold.
    claim second 3Gi
    check "second claim is bound" wait_until 300 5 bound second
    check "the pool adds a second usable share" wait_until 900 15 two_usable_shares
    check "the pool stops at its limit of two shares" no_more_than_two_shares
    if vpc_available; then
        check "the second share exists in the VPC" second_share_in_vpc
    else
        not_checked "the second share exists in the VPC" "no VPC access"
    fi

    claim third 2Gi
    check "third claim is bound" wait_until 300 5 bound third
    local new_share; new_share=$(pool_field "$GROW" '.status.shares[1].shareID')
    check "a new volume goes on the new share (spread)" \
        test "$(share_of "$(pv_of third)")" = "$new_share"
}

cleanup() {
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=300s
    oc delete filesharepool "$GROW" --ignore-not-found --wait=true --timeout=600s
}
