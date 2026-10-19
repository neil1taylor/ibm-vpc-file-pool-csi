#!/usr/bin/env bash
# 02-pool — A pool gets a usable security group share in its zone; a pool
# whose share cannot be made says why and creates nothing.
#
# Leaves the pool $(rt_name basic) for later stages; the final sweep deletes it.

RT_STAGE_TIMEOUT=900

BASIC=$(rt_name basic)
BADSG=$(rt_name badsg)

create_pool() {
    local name="$1" extra="$2"
    oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $name
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  zone: $(detect_zone)
  profile: dp2
  shareSizeGB: 10
  maxShares: 2
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
$extra
YAML
}

share_ready() {
    [ "$(pool_field "$BASIC" '.status.shares[0].state')" = "stable" ] &&
        [ -n "$(pool_field "$BASIC" '.status.shares[0].mountTargetIP')" ] &&
        [ -n "$(pool_field "$BASIC" '.status.shares[0].exportPath')" ]
}

storage_class_made() {
    [ "$(oc get sc "$BASIC" -o jsonpath='{.provisioner}/{.parameters.pool}')" = "$RT_PROVISIONER/$BASIC" ]
}

share_uses_security_groups() {
    local id; id=$(pool_field "$BASIC" '.status.shares[0].shareID')
    [ "$(vpc_get "/shares/$id" | jq -r .access_control_mode)" = "security_group" ]
}

mount_target_in_pool_zone() {
    local id mt vni subnet zone
    id=$(pool_field "$BASIC" '.status.shares[0].shareID')
    mt=$(pool_field "$BASIC" '.status.shares[0].mountTargetID')
    vni=$(vpc_get "/shares/$id/mount_targets/$mt" | jq -r .virtual_network_interface.id)
    subnet=$(vpc_get "/virtual_network_interfaces/$vni" | jq -r .subnet.id)
    zone=$(vpc_get "/subnets/$subnet" | jq -r .zone.name)
    echo "mount target zone: $zone, pool zone: $(detect_zone)"
    [ "$zone" = "$(detect_zone)" ]
}

bad_pool_reports_cause() {
    oc get filesharepool "$BADSG" -o json |
        jq -e '.status.conditions[]? | select(.type == "SharesReady" and .status == "False" and (.message | test("security group|SecurityGroup|security_group"; "i")))' >/dev/null
}

bad_pool_has_no_share() {
    [ -z "$(pool_field "$BADSG" '.status.shares[*].shareID')" ] && [ "$(pool_field "$BADSG" .status.phase)" != "Ready" ]
}

no_vpc_share_for_bad_pool() { ! vpc_share_names | grep -q "^$BADSG-"; }

run() {
    create_pool "$BASIC" ""
    check "pool reaches Ready" wait_until 600 10 pool_is_ready "$BASIC"
    check "its share is stable with a mount target address and export path" share_ready
    check "its StorageClass is made for the driver" storage_class_made
    if vpc_available; then
        check "the VPC share uses security group access mode" share_uses_security_groups
        check "the mount target is in the pool's zone" mount_target_in_pool_zone
    else
        not_checked "share access mode and mount target zone" "no VPC access"
    fi

    create_pool "$BADSG" '  securityGroupIDs: ["r010-00000000-0000-0000-0000-000000000000"]'
    check "a pool with a missing security group says why" wait_until 300 10 bad_pool_reports_cause
    check "that pool has no share and is not Ready" bad_pool_has_no_share
    if vpc_available; then
        check "no VPC share was made for that pool" no_vpc_share_for_bad_pool
    fi
}

cleanup() {
    oc delete filesharepool "$BADSG" --ignore-not-found --wait=true --timeout=300s
}
