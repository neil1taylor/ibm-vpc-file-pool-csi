#!/usr/bin/env bash
# test-copy-speed.sh — The controller can read the node agents' share counters, so the copy of a
# running VM's disks follows the load on the share (docs/specs/2026-10-26-adaptive-copy-speed.md).
#
# The controller needs the node metrics Service name, the CA of the node agents' certificate, the
# right to list the Service's endpoints in its own namespace, and the right to read /metrics.
#
# Usage: bash test/chart/test-copy-speed.sh
# Requires: helm, yq.

set -euo pipefail

CHART="$(cd "$(dirname "$0")/../.." && pwd)/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
check() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi; }

render() { helm template test "$CHART" --namespace kube-system "$@"; }
doc() { yq -N eval "select(.kind == \"$2\" and .metadata.name == \"$3\") | $4" - <<<"$1"; }

if ! command -v yq >/dev/null; then echo "yq is required"; exit 2; fi

C=test-ibm-vpc-file-pool-csi-controller
ARGS='.spec.template.spec.containers[] | select(.name == "csi-controller") | .args[]'

OS=$(render --set webhook.certProvider=openshift)
A=$(doc "$OS" Deployment "$C" "$ARGS")
if grep -qxF -- "--node-metrics-service=test-ibm-vpc-file-pool-csi-node-metrics" <<<"$A"; then pass "openshift: the controller reads the node metrics Service"; else fail "openshift: no --node-metrics-service"; fi
if grep -qxF -- "--node-metrics-ca=/etc/node-metrics-ca/service-ca.crt" <<<"$A"; then pass "openshift: it trusts the service CA"; else fail "openshift: no --node-metrics-ca"; fi
check "openshift: the service CA comes from a ConfigMap the cluster fills" \
    "$(doc "$OS" ConfigMap test-ibm-vpc-file-pool-csi-node-ca '.metadata.annotations."service.beta.openshift.io/inject-cabundle"')" "true"
check "openshift: the ConfigMap is mounted into the controller" \
    "$(doc "$OS" Deployment "$C" '.spec.template.spec.volumes[] | select(.name == "node-ca") | .configMap.name')" "test-ibm-vpc-file-pool-csi-node-ca"
check "the mount is read-only" \
    "$(doc "$OS" Deployment "$C" '.spec.template.spec.containers[] | select(.name == "csi-controller") | .volumeMounts[] | select(.name == "node-ca") | .readOnly')" "true"

CM=$(render --set webhook.certProvider=cert-manager)
if grep -qxF -- "--node-metrics-ca=/etc/node-metrics-ca/ca.crt" <<<"$(doc "$CM" Deployment "$C" "$ARGS")"; then pass "cert-manager: it trusts the node agents' ca.crt"; else fail "cert-manager: wrong --node-metrics-ca"; fi
check "cert-manager: the CA comes from the node metrics certificate secret" \
    "$(doc "$CM" Deployment "$C" '.spec.template.spec.volumes[] | select(.name == "node-ca") | .secret.secretName')" "test-ibm-vpc-file-pool-csi-node-metrics-certs"

OFF=$(render --set node.metricsPort=0)
if grep -q -- "--node-metrics-service" <<<"$(doc "$OFF" Deployment "$C" "$ARGS")"; then fail "port 0: the controller must not read the node agents"; else pass "port 0: the controller does not read the node agents"; fi

# Rights: list the endpoints in its own namespace only, and read /metrics.
check "it may list endpoint slices in its own namespace" \
    "$(doc "$OS" Role "$C-node-agents" '.rules[] | select(.resources[] == "endpointslices") | .verbs[0]')" "list"
check "that role is in the driver namespace" "$(doc "$OS" Role "$C-node-agents" '.metadata.namespace')" "kube-system"
check "and is bound to the controller" "$(doc "$OS" RoleBinding "$C-node-agents" '.subjects[0].name')" "$C"
check "it may read /metrics (the node agents check the token)" \
    "$(doc "$OS" ClusterRole "$C" '.rules[] | select(.nonResourceURLs[] == "/metrics") | .verbs[0]')" "get"
if doc "$OS" ClusterRole "$C" '.rules[].resources[]' | grep -qx endpointslices; then fail "no cluster-wide endpoint slice rights"; else pass "no cluster-wide endpoint slice rights"; fi

exit $FAILED
