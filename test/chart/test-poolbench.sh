#!/usr/bin/env bash
# test-poolbench.sh — Render the Helm chart and check the PoolBench service:
# off by default; when on, a hardened Deployment, its own service account and
# rights, the rights it binds in each run's namespace, the collector Service and
# the results volume.
#
# Usage: bash test/chart/test-poolbench.sh
# Requires: helm, yq.

set -euo pipefail

CHART="$(cd "$(dirname "$0")/../.." && pwd)/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
check() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi; }

render() {
    helm template test "$CHART" --namespace kube-system "$@"
}
# doc <rendered> <kind> <name> <yq path> prints one value of one object.
doc() {
    yq -N eval "select(.kind == \"$2\" and .metadata.name == \"$3\") | $4" - <<<"$1"
}

if ! command -v yq >/dev/null; then echo "yq is required"; exit 2; fi

OFF=$(render)
if grep -q "ibm-vpc-file-pool-csi-poolbench" <<<"$OFF"; then fail "off by default: nothing of PoolBench is rendered"; else pass "off by default: nothing of PoolBench is rendered"; fi

OUT=$(render --set poolBench.enabled=true)
D=test-ibm-vpc-file-pool-csi-poolbench
APPVER=$(yq eval '.appVersion' "$CHART/Chart.yaml")

check "one replica" "$(doc "$OUT" Deployment "$D" '.spec.replicas')" "1"
check "the results volume is never mounted by two pods" "$(doc "$OUT" Deployment "$D" '.spec.strategy.type')" "Recreate"
check "the driver image" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.containers[0].image')" "de.icr.io/ibm-vpc-file-pool-csi/driver:v${APPVER}"
check "its own service account" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.serviceAccountName')" "$D"
ARGS=$(doc "$OUT" Deployment "$D" '.spec.template.spec.containers[0].args[]')
for want in "--mode=poolbench" \
    "--poolbench-collector-url=http://$D.kube-system.svc:8080" \
    "--poolbench-run-role=$D-run" \
    "--poolbench-service-account=$D" \
    "--node-metrics-service=test-ibm-vpc-file-pool-csi-node-metrics" \
    "--poolbench-results-dir=/results"; do
    if grep -qxF -- "$want" <<<"$ARGS"; then pass "arg $want"; else fail "arg $want missing"; fi
done
check "POD_NAMESPACE is set" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.containers[0].env[] | select(.name == "POD_NAMESPACE") | .valueFrom.fieldRef.fieldPath')" "metadata.namespace"
SC='.spec.template.spec.containers[0].securityContext'
check "no privilege escalation" "$(doc "$OUT" Deployment "$D" "$SC.allowPrivilegeEscalation")" "false"
check "runs as non-root" "$(doc "$OUT" Deployment "$D" "$SC.runAsNonRoot")" "true"
check "drops ALL capabilities" "$(doc "$OUT" Deployment "$D" "$SC.capabilities.drop[0]")" "ALL"
check "read-only root file system" "$(doc "$OUT" Deployment "$D" "$SC.readOnlyRootFilesystem")" "true"
check "seccomp RuntimeDefault" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.securityContext.seccompProfile.type')" "RuntimeDefault"
check "results volume mounted" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.containers[0].volumeMounts[] | select(.name == "results") | .mountPath')" "/results"
check "results claim" "$(doc "$OUT" PersistentVolumeClaim "$D-results" '.spec.resources.requests.storage')" "5Gi"
check "collector Service port" "$(doc "$OUT" Service "$D" '.spec.ports[] | select(.name == "collector") | .port')" "8080"

# Cluster-wide rights: no VMs, no secrets, no pod exec outside the run namespaces.
CR=$(doc "$OUT" ClusterRole "$D" '.rules')
if grep -q "pods/exec" <<<"$CR"; then fail "no pod exec cluster-wide"; else pass "no pod exec cluster-wide"; fi
if grep -q "virtualmachines" <<<"$CR"; then fail "no VM rights cluster-wide"; else pass "no VM rights cluster-wide"; fi
if yq -N eval '.[] | select(.resources[] == "secrets")' - <<<"$CR" | grep -q .; then fail "no secret rights cluster-wide"; else pass "no secret rights cluster-wide"; fi
check "it may bind only the run role" "$(doc "$OUT" ClusterRole "$D" '.rules[] | select(.verbs[] == "bind") | .resourceNames[0]')" "$D-run"
check "the cluster role is bound to it" "$(doc "$OUT" ClusterRoleBinding "$D" '.subjects[0].name')" "$D"

# The run role: what it needs inside a run's namespace.
RR=$(doc "$OUT" ClusterRole "$D-run" '.rules[].resources[]')
for want in pods/exec virtualmachines virtualmachineinstances secrets persistentvolumeclaims datavolumes; do
    if grep -qxF -- "$want" <<<"$RR"; then pass "run role has $want"; else fail "run role lacks $want"; fi
done
if doc "$OUT" ClusterRoleBinding "$D-run" '.metadata.name' | grep -q .; then fail "the run role is never bound cluster-wide"; else pass "the run role is never bound cluster-wide"; fi

exit "$FAILED"
