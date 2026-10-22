#!/usr/bin/env bash
# test-servicemonitor.sh — Render the Helm chart and check when the ServiceMonitor is created.
#
# Usage: bash test/chart/test-servicemonitor.sh
# Requires: helm.

set -euo pipefail

CHART="$(cd "$(dirname "$0")/../.." && pwd)/charts/ibm-vpc-file-pool-csi"
SM_API="monitoring.coreos.com/v1/ServiceMonitor"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

render() {
    helm template test "$CHART" --namespace kube-system "$@"
}

has_sm() { grep -qF "kind: ServiceMonitor" <<<"$1"; }

# By default the chart creates a ServiceMonitor when the cluster has the
# Prometheus Operator API, so the console plugin's charts get data.
if has_sm "$(render --api-versions "$SM_API")"; then
    pass "default: ServiceMonitor rendered when the API exists"
else
    fail "default: ServiceMonitor missing although the API exists"
fi

# Without the API the chart must still install, so no ServiceMonitor.
if has_sm "$(render)"; then
    fail "default: ServiceMonitor rendered on a cluster without the API"
else
    pass "default: no ServiceMonitor without the API"
fi

# The user can still turn it off.
if has_sm "$(render --api-versions "$SM_API" --set metrics.serviceMonitor.enabled=false)"; then
    fail "disabled: ServiceMonitor rendered"
else
    pass "disabled: no ServiceMonitor"
fi

exit "$FAILED"
