#!/usr/bin/env bash
# 11-draining — A share on the pool's drain list gets no new volumes, and the
# pool reports how many volumes are left on it. Taking the share off the list
# puts it back in use.

RT_STAGE_TIMEOUT=1500

POOL=$(rt_name drain)
NS=$(rt_name drain)
CLAIMS=4

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
  maxShares: 2
  initialShares: 2
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

two_shares_ready() {
    [ "$(oc get filesharepool "$POOL" -o json | jq '[.status.shares[]? | select(.state == "stable" and .mountTargetIP != "")] | length')" -eq 2 ]
}

share_state() { oc get filesharepool "$POOL" -o json | jq -r --arg id "$1" '.status.shares[] | select(.shareID == $id) | .state'; }
set_drain_list() { oc patch filesharepool "$POOL" --type=merge -p "{\"spec\":{\"drainShares\":$1}}" >/dev/null; }

share_is() { [ "$(share_state "$1")" = "$2" ]; }

bound() { [ "$(oc get pvc -n "$NS" "$1" -o jsonpath='{.status.phase}')" = "Bound" ]; }
share_of_claim() {
    oc get subvolumes.storage.ibmcloud.io "$(oc get pvc -n "$NS" "$1" -o jsonpath='{.spec.volumeName}')" -o jsonpath='{.spec.shareID}'
}

no_new_claim_on() {
    local drained="$1" i on
    for i in $(seq 1 "$CLAIMS"); do
        on=$(share_of_claim "new-$i")
        echo "new-$i is on $on"
        [ "$on" != "$drained" ] || return 1
    done
}

drain_status_counts() {
    local want="$2" got
    got=$(oc get filesharepool "$POOL" -o json | jq -r --arg id "$1" '.status.drainStatus[]? | select(.shareID == $id) | .remainingSubVolumes')
    echo "volumes left on the drained share: ${got:-none}, want $want"
    [ "$got" = "$want" ]
}

run() {
    rt_namespace "$NS"
    create_pool
    check "the pool has two usable shares" wait_until 900 10 two_shares_ready
    two_shares_ready || return 0

    # One volume on each share, so the drained share holds one.
    claim before-1
    claim before-2
    wait_until 300 5 bound before-1
    wait_until 300 5 bound before-2
    local drained
    drained=$(share_of_claim before-1)

    set_drain_list "[\"$drained\"]"
    check "the share on the drain list is marked draining" wait_until 180 5 share_is "$drained" draining
    check "the pool counts the volumes left on it" wait_until 180 5 drain_status_counts "$drained" \
        "$(oc get subvolumes.storage.ibmcloud.io -o json | jq --arg id "$drained" '[.items[] | select(.spec.shareID == $id)] | length')"

    local i
    for i in $(seq 1 "$CLAIMS"); do claim "new-$i"; done
    for i in $(seq 1 "$CLAIMS"); do wait_until 300 5 bound "new-$i"; done
    check "no new volume goes on the draining share" no_new_claim_on "$drained"

    set_drain_list "[]"
    check "taken off the drain list, the share is back in use" wait_until 300 10 share_is "$drained" stable
    claim after-1
    claim after-2
    wait_until 300 5 bound after-1
    wait_until 300 5 bound after-2
    check "a new volume goes on that share again" \
        sh -c "[ \"\$(oc get subvolumes.storage.ibmcloud.io -o json | jq --arg id $drained --arg ns $NS '[.items[] | select(.spec.shareID == \$id and .spec.pvcNamespace == \$ns and (.spec.pvcName | startswith(\"after-\")))] | length')\" -ge 1 ]"
}

cleanup() {
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=600s
    oc delete filesharepool "$POOL" --ignore-not-found --wait=true --timeout=600s
}
