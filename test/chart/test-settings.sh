#!/usr/bin/env bash
# test-settings.sh — Render the Helm chart and check the settings ConfigMap that
# the console reads to show the driver's real VM snapshot mode: the ConfigMap,
# and the Role that lets ordinary users read that one ConfigMap and nothing else.
#
# Usage: bash test/chart/test-settings.sh
# Requires: helm.

set -euo pipefail

CHART="$(cd "$(dirname "$0")/../.." && pwd)/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

render() {
    helm template test "$CHART" --namespace kube-system "$@"
}

expect_contains() {
    local desc="$1" out="$2" needle="$3"
    if grep -qF -- "$needle" <<<"$out"; then pass "$desc"; else fail "$desc (missing: $needle)"; fi
}

expect_absent() {
    local desc="$1" out="$2" needle="$3"
    if grep -qF -- "$needle" <<<"$out"; then fail "$desc (found: $needle)"; else pass "$desc"; fi
}

# The name is fixed, not built from the release name: the console looks for exactly this name.
out=$(render --show-only templates/settings.yaml 2>&1) || out=""
expect_contains "the ConfigMap has the fixed name" "$out" "name: ibm-vpc-file-pool-csi-settings"
expect_contains "default: vmSnapshotMethod is auto" "$out" 'vmSnapshotMethod: "auto"'
expect_contains "the Role allows get on that one ConfigMap by name" "$out" '- ibm-vpc-file-pool-csi-settings'
expect_contains "the Role allows only get" "$out" '- get'
expect_absent "the Role does not allow list" "$out" '- list'
expect_absent "the Role does not allow watch" "$out" '- watch'
expect_contains "all authenticated users are bound" "$out" 'name: system:authenticated'
expect_contains "the objects are in the release namespace" "$out" 'namespace: kube-system'

out=$(render --show-only templates/settings.yaml --set vmSnapshots.method=copy 2>&1) || out=""
expect_contains "method=copy is published" "$out" 'vmSnapshotMethod: "copy"'
expect_absent "method=copy does not publish auto" "$out" 'vmSnapshotMethod: "auto"'

# The setting holds no secret.
all=$(render)
cm=$(render --show-only templates/settings.yaml 2>/dev/null || true)
expect_absent "the ConfigMap holds no key or token" "$cm" 'apikey'
expect_absent "the ConfigMap holds no token" "$cm" 'token'
[ -n "$all" ] || fail "the chart renders"

exit "$FAILED"
