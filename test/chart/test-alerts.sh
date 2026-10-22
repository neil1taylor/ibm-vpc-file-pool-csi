#!/usr/bin/env bash
# test-alerts.sh — Render the chart's alert rules and run the promtool unit tests on them.
#
# Usage: bash test/chart/test-alerts.sh
# Requires: helm, promtool (set PROMTOOL=/path if it is not on the PATH).

set -euo pipefail

DIR="$(cd "$(dirname "$0")" && pwd)"
CHART="$DIR/../../charts/ibm-vpc-file-pool-csi"
PROMTOOL="${PROMTOOL:-promtool}"
PR_API="monitoring.coreos.com/v1/PrometheusRule"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

render() {
    helm template test "$CHART" --namespace kube-system "$@"
}

# No rule on a cluster without the Prometheus Operator API: the chart must still install.
if render | grep -qF "kind: PrometheusRule"; then
    fail "default: PrometheusRule rendered on a cluster without the API"
else
    pass "default: no PrometheusRule without the API"
fi

if render --api-versions "$PR_API" --set metrics.alerts.enabled=false | grep -qF "kind: PrometheusRule"; then
    fail "disabled: PrometheusRule rendered"
else
    pass "disabled: no PrometheusRule"
fi

# Keep the "groups:" part of the rule and dedent it, so promtool can read it.
render --api-versions "$PR_API" \
    | awk '/^kind: PrometheusRule/{f=1} f&&/^---/{exit} f' \
    | awk '/^  groups:/{g=1; print "groups:"; next} g{sub(/^  /,""); print}' > "$WORK/rendered-rules.yml"
cp "$DIR/alerts_test.yml" "$WORK/alerts_test.yml"

if "$PROMTOOL" check rules "$WORK/rendered-rules.yml" >"$WORK/check.log" 2>&1; then
    pass "promtool accepts the rules"
else
    cat "$WORK/check.log"; fail "promtool rejects the rules"
fi

if (cd "$WORK" && "$PROMTOOL" test rules alerts_test.yml) >"$WORK/test.log" 2>&1; then
    pass "promtool rule tests"
else
    cat "$WORK/test.log"; fail "promtool rule tests"
fi

exit "$FAILED"
