#!/usr/bin/env bash
# test-hardening.sh — Render the Helm chart and check the pod hardening:
# the default image tag, the controller's non-root security context, the
# console plugin's context, and that the node agent still runs as root.
#
# Usage: bash test/chart/test-hardening.sh
# Requires: helm, yq.

set -euo pipefail

CHART="$(cd "$(dirname "$0")/../.." && pwd)/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

render() {
    helm template test "$CHART" --namespace kube-system \
        --api-versions console.openshift.io/v1/ConsolePlugin \
        --set replicationReceiver.enabled=true \
        --set replicationReceiver.pvcName=data \
        --set replicationReceiver.authSecretName=token "$@"
}

# field <rendered> <component label> <yq path under the workload> prints one value.
field() {
    yq -N eval "select(.kind == \"Deployment\" or .kind == \"DaemonSet\") | select(.spec.template.metadata.labels.\"app.kubernetes.io/component\" == \"$2\") | $3" - <<<"$1"
}

check() { # check <name> <got> <want>
    if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi
}

if ! command -v yq >/dev/null; then echo "yq is required"; exit 2; fi

APPVER=$(yq eval '.appVersion' "$CHART/Chart.yaml")
OUT=$(render)

check "default tag is v<appVersion>, not latest (controller)" \
    "$(field "$OUT" controller '.spec.template.spec.containers[] | select(.name == "csi-controller") | .image')" \
    "de.icr.io/ibm-vpc-file-pool-csi/driver:v${APPVER}"
check "default tag is v<appVersion> (node)" \
    "$(field "$OUT" node '.spec.template.spec.containers[] | select(.name == "csi-node") | .image')" \
    "de.icr.io/ibm-vpc-file-pool-csi/driver:v${APPVER}"
check "default tag is v<appVersion> (console plugin)" \
    "$(field "$OUT" console-plugin '.spec.template.spec.containers[0].image')" \
    "de.icr.io/ibm-vpc-file-pool-csi/console-plugin:v${APPVER}"
if grep -q ':latest' <<<"$OUT"; then fail "rendered chart still contains :latest"; else pass "no :latest in the rendered chart"; fi
check "image.tag overrides the default" \
    "$(field "$(render --set image.tag=v9.9.9)" controller '.spec.template.spec.containers[] | select(.name == "csi-controller") | .image')" \
    "de.icr.io/ibm-vpc-file-pool-csi/driver:v9.9.9"
check "--driver-image uses the same tag" \
    "$(field "$OUT" controller '.spec.template.spec.containers[] | select(.name == "csi-controller") | .args[] | select(test("^--driver-image"))')" \
    "--driver-image=de.icr.io/ibm-vpc-file-pool-csi/driver:v${APPVER}"

CTRL='.spec.template.spec.containers[] | select(.name == "csi-controller") | .securityContext'
check "controller: no privilege escalation" "$(field "$OUT" controller "$CTRL.allowPrivilegeEscalation")" "false"
check "controller: runAsNonRoot" "$(field "$OUT" controller "$CTRL.runAsNonRoot")" "true"
check "controller: drops ALL capabilities" "$(field "$OUT" controller "$CTRL.capabilities.drop[0]")" "ALL"
check "controller: seccomp RuntimeDefault" "$(field "$OUT" controller '.spec.template.spec.securityContext.seccompProfile.type')" "RuntimeDefault"
check "controller: fsGroup set off OpenShift (webhook key is mode 0400)" \
    "$(field "$OUT" controller '.spec.template.spec.securityContext.fsGroup')" "65532"
check "controller: fsGroup set on OpenShift in kube-system (no SCC assigns one there)" \
    "$(field "$(render --api-versions security.openshift.io/v1)" controller '.spec.template.spec.securityContext.fsGroup')" "65532"
check "controller: no fixed fsGroup on OpenShift in an ordinary namespace" \
    "$(field "$(render --namespace csi-test --api-versions security.openshift.io/v1)" controller '.spec.template.spec.securityContext.fsGroup')" "null"
SECRET='.spec.template.spec.containers[] | select(.name == "storage-secret-sidecar") | .securityContext.runAsUser'
for sc in csi-provisioner csi-resizer csi-snapshotter liveness-probe; do
    check "sidecar $sc: runs as the controller user so it can open the driver socket" \
        "$(field "$OUT" controller ".spec.template.spec.containers[] | select(.name == \"$sc\") | .securityContext.runAsUser")" "65532"
done
check "secret sidecar: runs as the controller user so the controller can open its socket (kube-system)" \
    "$(field "$(render --api-versions security.openshift.io/v1)" controller "$SECRET")" "65532"
check "secret sidecar: runs as the controller user off OpenShift" "$(field "$OUT" controller "$SECRET")" "65532"
check "secret sidecar: user left to OpenShift in an ordinary namespace" \
    "$(field "$(render --namespace csi-test --api-versions security.openshift.io/v1)" controller "$SECRET")" "null"
check "controller: security context can be overridden" \
    "$(field "$(render --set controller.securityContext.runAsNonRoot=false)" controller "$CTRL.runAsNonRoot")" "false"
for sc in csi-provisioner csi-resizer csi-snapshotter liveness-probe storage-secret-sidecar; do
    check "controller sidecar $sc: no privilege escalation, drops ALL" \
        "$(field "$OUT" controller ".spec.template.spec.containers[] | select(.name == \"$sc\") | .securityContext | (.allowPrivilegeEscalation, .capabilities.drop[0])" | tr '\n' ' ')" \
        "false ALL "
done

NODE='.spec.template.spec.containers[] | select(.name == "csi-node") | .securityContext'
check "node: still privileged" "$(field "$OUT" node "$NODE.privileged")" "true"
check "node: explicitly root, because the image now defaults to non-root" "$(field "$OUT" node "$NODE.runAsUser")" "0"

CP='.spec.template.spec.containers[0].securityContext'
check "console plugin: no privilege escalation" "$(field "$OUT" console-plugin "$CP.allowPrivilegeEscalation")" "false"
check "console plugin: runAsNonRoot" "$(field "$OUT" console-plugin "$CP.runAsNonRoot")" "true"
check "console plugin: drops ALL" "$(field "$OUT" console-plugin "$CP.capabilities.drop[0]")" "ALL"

RC='.spec.template.spec.containers[0].securityContext'
check "receiver: no privilege escalation" "$(field "$OUT" replication-receiver "$RC.allowPrivilegeEscalation")" "false"
check "receiver: drops ALL" "$(field "$OUT" replication-receiver "$RC.capabilities.drop[0]")" "ALL"
check "receiver: still runs as 107" "$(field "$OUT" replication-receiver '.spec.template.spec.securityContext.runAsUser')" "107"

if grep -q '^USER 65532' "$CHART/../../Dockerfile"; then pass "Dockerfile sets a numeric non-root USER"; else fail "Dockerfile has no 'USER 65532'"; fi

exit $FAILED
