#!/usr/bin/env bash
# test-webhook-certs.sh — Render the Helm chart and check webhook certificate wiring
# for each webhook.certProvider value.
#
# Usage: bash test/chart/test-webhook-certs.sh
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

# openshift: the OpenShift service CA issues the serving cert and injects the CA bundle.
out=$(render --set webhook.certProvider=openshift)
expect_contains "openshift: Service requests a serving cert" "$out" \
    "service.beta.openshift.io/serving-cert-secret-name: test-ibm-vpc-file-pool-csi-webhook-certs"
expect_contains "openshift: webhook config gets the CA bundle injected" "$out" \
    'service.beta.openshift.io/inject-cabundle: "true"'
expect_absent "openshift: no cert-manager resources" "$out" "cert-manager.io"

# cert-manager (default): unchanged behaviour.
out=$(render)
expect_contains "cert-manager: Certificate is rendered" "$out" "kind: Certificate"
expect_contains "cert-manager: CA injection annotation" "$out" "cert-manager.io/inject-ca-from: kube-system/test-ibm-vpc-file-pool-csi-webhook"
expect_absent "cert-manager: no OpenShift service CA annotations" "$out" "service.beta.openshift.io"

exit "$FAILED"
