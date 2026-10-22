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

# manual: the CA bundle comes from a chart value and is set on every webhook.
out=$(render --set webhook.certProvider=manual --set webhook.caBundle=Q0EtQlVORExF)
count=$(grep -c "caBundle: Q0EtQlVORExF" <<<"$out" || true)
if [[ "$count" == 8 ]]; then pass "manual: all 8 webhooks get the CA bundle"; else fail "manual: CA bundle on $count of 8 webhooks"; fi
expect_absent "manual: no cert-manager resources" "$out" "cert-manager.io"

# manual without a CA bundle cannot work, so the render fails.
if render --set webhook.certProvider=manual >/dev/null 2>&1; then
    fail "manual without webhook.caBundle should fail to render"
else
    pass "manual without webhook.caBundle fails to render"
fi

# The controller's webhook server port follows the chart value.
out=$(render --set webhook.port=9555)
expect_contains "controller is told the webhook port" "$out" '"--webhook-port=9555"'

# webhook.enabled=false: no webhook objects, and the controller registers none.
out=$(render --set webhook.enabled=false)
expect_absent "disabled: no ValidatingWebhookConfiguration" "$out" "kind: ValidatingWebhookConfiguration"
expect_contains "disabled: controller is told not to serve webhooks" "$out" '"--enable-webhooks=false"'
expect_absent "disabled: no webhook cert volume" "$out" "webhook-certs"

exit "$FAILED"
