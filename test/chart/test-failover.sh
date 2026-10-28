#!/usr/bin/env bash
# test-failover.sh — The Failover resource needs a controller rule, an admin
# role that nobody holds, a validating webhook, and the default claim flag.
#
# Usage: bash test/chart/test-failover.sh
# Requires: helm.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

RENDERED="$(mktemp)"
trap 'rm -f "$RENDERED"' EXIT
helm template test "$CHART" --namespace kube-system --set replicationReceiver.pvcName=recv-data > "$RENDERED"

has() { grep -qF -- "$2" "$1"; }

for file in "$RENDERED" "$ROOT/config/rbac/clusterrole.yaml"; do
    name="chart"
    [[ "$file" == "$RENDERED" ]] || name="config/rbac"
    if has "$file" 'resources: ["failovers", "failovers/status"]'; then
        pass "$name: the controller may manage failovers and their status"
    else
        fail "$name: the controller role lacks failovers"
    fi
    if has "$file" 'failover-admin'; then
        pass "$name: the failover-admin role exists"
    else
        fail "$name: no failover-admin role"
    fi
    # A binding to the admin role would hand out failover to someone by default.
    if grep -A8 'kind: ClusterRoleBinding' "$file" | grep -qF 'failover-admin'; then
        fail "$name: failover-admin is bound to someone"
    else
        pass "$name: failover-admin is bound to nobody"
    fi
done

if has "$RENDERED" 'path: /validate-storage-ibmcloud-io-v1alpha1-failover'; then
    pass "chart: the Failover webhook is registered"
else
    fail "chart: no Failover webhook"
fi
if has "$RENDERED" 'resources: ["failovers"]'; then
    pass "chart: the webhook covers failovers"
else
    fail "chart: the webhook does not cover failovers"
fi
if has "$RENDERED" '"--failover-default-source=recv-data"'; then
    pass "chart: the controller defaults to the receiver claim"
else
    fail "chart: the controller is not told the default claim"
fi

out="$(helm template test "$CHART" --namespace kube-system)"
if grep -qF -- '--failover-default-source' <<<"$out"; then
    fail "chart: no default claim set, but the flag is passed"
else
    pass "chart: without replicationReceiver.pvcName the flag is left out"
fi

exit "$FAILED"
