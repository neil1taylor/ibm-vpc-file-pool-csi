#!/usr/bin/env bash
# 01-preflight — The driver is the expected version and healthy, and no
# earlier run left anything behind.

RT_STAGE_TIMEOUT=300

controller_ready() {
    [ "$(oc get deploy -n "$RT_DRIVER_NS" ibm-vpc-file-pool-csi-controller -o jsonpath='{.status.readyReplicas}')" -ge 1 ]
}

node_agents_ready() {
    local want have
    want=$(oc get ds -n "$RT_DRIVER_NS" ibm-vpc-file-pool-csi-node -o jsonpath='{.status.desiredNumberScheduled}')
    have=$(oc get ds -n "$RT_DRIVER_NS" ibm-vpc-file-pool-csi-node -o jsonpath='{.status.numberReady}')
    [ -n "$want" ] && [ "$want" -gt 0 ] && [ "$want" = "$have" ]
}

running_version() {
    oc logs -n "$RT_DRIVER_NS" deploy/ibm-vpc-file-pool-csi-controller -c csi-controller 2>/dev/null |
        grep -m1 'Starting IBM VPC File Pool CSI Driver' | sed -n 's/.*version="\([^"]*\)".*/\1/p'
}

version_matches() {
    local v; v=$(running_version)
    echo "running version: $v"
    [ -n "$v" ] && [ "$v" != "dev" ] && { [ -z "$RT_EXPECTED_VERSION" ] || [ "$v" = "$RT_EXPECTED_VERSION" ]; }
}

crds_installed() {
    local crd
    for crd in filesharepools subvolumes snapshots volumegroupsnapshots replicationpolicies; do
        oc get crd "$crd.storage.ibmcloud.io" >/dev/null || return 1
    done
}

# The webhook must refuse a pool with a bad permissions value.
webhook_refuses_bad_pool() {
    local out
    out=$(oc create --dry-run=server -f - 2>&1 <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $(rt_name webhook-probe)
spec:
  zone: $(detect_zone)
  profile: dp2
  shareSizeGB: 10
  maxShares: 1
  initialShares: 1
  defaultPermissions: "999"
YAML
)
    echo "$out"
    echo "$out" | grep -q "defaultPermissions must be an octal mode"
}

no_earlier_leftovers() {
    local left
    left=$(oc get namespaces,filesharepools -l "$RT_LABEL" -o name 2>/dev/null)
    echo "$left"
    [ -z "$left" ]
}

# to_mib QUANTITY — a Kubernetes memory quantity (8Gi, 600M, ...) in MiB.
to_mib() {
    awk -v q="$1" 'BEGIN {
        n = q + 0; u = substr(q, length(n "") + 1)
        f["Ki"] = 1/1024; f["Mi"] = 1; f["Gi"] = 1024; f["Ti"] = 1048576
        f["K"] = 1000/1048576; f["M"] = 1e6/1048576; f["G"] = 1e9/1048576; f[""] = 1/1048576
        printf "%d", n * f[u]
    }'
}

# CDI's copy pods must be allowed enough memory to write onto NFS. The
# controller keeps the limit at the Helm value cdi.copyPodMemoryLimit.
cdi_copy_limit_ok() {
    local want="${RT_CDI_COPY_LIMIT:-8Gi}" got
    got=$(oc get cdi -o jsonpath='{.items[0].spec.config.podResourceRequirements.limits.memory}')
    echo "CDI copy pod memory limit: ${got:-not set (600M)}, want at least $want"
    [ -n "$got" ] && [ "$(to_mib "$got")" -ge "$(to_mib "$want")" ]
}

run() {
    check "controller is ready" controller_ready
    check "every node agent is ready" node_agents_ready
    check "driver reports the expected version" version_matches
    check "all 5 CRDs are installed" crds_installed
    check "webhook refuses a pool with bad permissions" webhook_refuses_bad_pool
    check "no objects left from an earlier run" no_earlier_leftovers
    if oc get crd cdis.cdi.kubevirt.io >/dev/null 2>&1; then
        check "CDI copy pods may use enough memory for NFS" cdi_copy_limit_ok
    fi
    if vpc_available; then
        check "VPC API can be read" vpc_get /shares?limit=1
    else
        not_checked "VPC API can be read" "no VPC access from this machine"
    fi
}

cleanup() { :; }
