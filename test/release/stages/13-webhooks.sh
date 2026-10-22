#!/usr/bin/env bash
# 13-webhooks — Bad objects are refused, each with a message that names the
# problem. A good object of each kind is accepted, so each refusal is caused
# by the bad field. Every request is a server-side dry run: nothing is made.

RT_STAGE_TIMEOUT=600

BASIC=$(rt_name basic)
ZONE=""

pool_yaml() {
    cat <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $(rt_name webhook)
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  zone: $ZONE
  profile: dp2
  shareSizeGB: 10
  maxShares: 2
  initialShares: 1
  expandThresholdPercent: 80
  defaultPermissions: "0755"
$1
YAML
}

subvolume_yaml() {
    cat <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: SubVolume
metadata:
  name: pvc-00000000-0000-0000-0000-000000000000
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  poolName: $BASIC
  shareID: r000-00000000-0000-0000-0000-000000000000
  shareMountTargetIP: 10.0.0.1
  pvName: pvc-00000000-0000-0000-0000-000000000000
  pvcName: webhook-test
  pvcNamespace: default
  requestedGB: 1
  subPath: ${1:-/pvcs/pvc-00000000-0000-0000-0000-000000000000}
YAML
}

replication_yaml() {
    cat <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: ReplicationPolicy
metadata:
  name: $(rt_name webhook)
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  sourcePoolName: $BASIC
  destinationBasePath: /pvcs
  schedule: ${2:-15m}
  maxRetries: 3
$1
YAML
}

# accepted YAML — the API server and the webhook accept YAML (dry run).
accepted() { echo "$1" | oc create --dry-run=server -f - ; }

# refused MESSAGE YAML — the request is refused, and the error has MESSAGE.
refused() {
    local out
    if out=$(echo "$2" | oc create --dry-run=server -f - 2>&1); then
        echo "accepted, but should be refused: $out"
        return 1
    fi
    echo "$out"
    echo "$out" | grep -qiF -- "$1"
}

# zone_change_refused — changing an existing pool's zone is refused.
zone_change_refused() {
    local out
    if out=$(oc patch filesharepool "$BASIC" --dry-run=server --type=merge -p '{"spec":{"zone":"eu-de-9"}}' 2>&1); then
        echo "accepted, but should be refused: $out"
        return 1
    fi
    echo "$out"
    echo "$out" | grep -qF "spec.zone is immutable"
}

run() {
    ZONE=$(detect_zone)

    check "a good pool is accepted" accepted "$(pool_yaml '')"
    check "a pool with permissions 0999 is refused with the reason" \
        refused "spec.defaultPermissions must be an octal mode" "$(pool_yaml '  defaultPermissions: "0999"')"
    check "a pool with more initial shares than its limit is refused with the reason" \
        refused "must not exceed spec.maxShares" "$(pool_yaml '  initialShares: 3')"
    check "a pool with an expand threshold of 40% is refused with the range" \
        refused "50" "$(pool_yaml '  expandThresholdPercent: 40')"
    check "a pool with 101 shares is refused with the range" \
        refused "100" "$(pool_yaml '  maxShares: 101')"
    if pool_is_ready "$BASIC"; then
        check "changing a pool's zone is refused" zone_change_refused
    else
        not_checked "changing a pool's zone is refused" "needs the Ready pool from stage 02"
    fi

    check "a good SubVolume is accepted" accepted "$(subvolume_yaml)"
    check "a SubVolume whose path leaves the volume folder is refused" \
        refused "subPath" "$(subvolume_yaml '/pvcs/../etc')"

    check "a good replication policy is accepted" accepted "$(replication_yaml '  destinationNFSServer: 10.0.0.1')"
    check "a replication policy to a plain HTTP receiver is refused" \
        refused "must use HTTPS" "$(replication_yaml '  destinationEndpoint: http://receiver.example.com
  destinationAuthSecretRef: receiver-token')"
    check "a replication policy with a blocked rsync option is refused" \
        refused "disallowed flag" "$(replication_yaml '  destinationNFSServer: 10.0.0.1
  rsyncOptions: ["--rsh=ssh"]')"
    check "a replication policy with shell characters in an rsync option is refused" \
        refused "no spaces, quotes or shell characters" "$(replication_yaml '  destinationNFSServer: 10.0.0.1
  rsyncOptions: ["--rsh=sh -c id"]')"
    check "a replication policy with a bad schedule is refused" \
        refused "spec.schedule is not a valid duration" "$(replication_yaml '  destinationNFSServer: 10.0.0.1' often)"
}

cleanup() { :; }
