#!/usr/bin/env bash
# test-vm-snapshots.sh — Render the Helm chart and check the wiring of instant
# VM snapshots: the controller flags, the VM hook webhook and its RBAC.
#
# Usage: bash test/chart/test-vm-snapshots.sh
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

# Default: instant snapshots where the cluster allows them.
out=$(render)
expect_contains "default: controller runs with method auto" "$out" '"--vm-snapshot-method=auto"'
expect_absent "default: no launcher image flag (found from virt-operator)" "$out" '--virt-launcher-image'
expect_contains "default: VM hook webhook" "$out" "kind: MutatingWebhookConfiguration"
expect_contains "default: webhook path" "$out" "path: /mutate-kubevirt-io-v1-virtualmachineinstance"
hook=$(awk '/kind: MutatingWebhookConfiguration/,/^---/' <<<"$out")
expect_contains "the hook webhook never blocks VM starts when it is down" "$hook" "failurePolicy: Ignore"
expect_contains "the hook webhook has no side effects" "$hook" "sideEffects: None"
expect_contains "the hook webhook only sees VMI creation" "$hook" 'resources: ["virtualmachineinstances"]'
expect_contains "RBAC: patch VMIs" "$out" 'resources: ["virtualmachineinstances"]'
expect_contains "RBAC: the hook ConfigMap may be updated" "$out" 'resourceNames: ["ibm-vpc-file-pool-vm-hook"]'
expect_contains "RBAC: read virt-operator for its launcher image" "$out" 'resourceNames: ["virt-operator"]'

# A set launcher image is passed on.
out=$(render --set vmSnapshots.launcherImage=registry.example/virt-launcher:1)
expect_contains "launcher image flag" "$out" '"--virt-launcher-image=registry.example/virt-launcher:1"'

# copy: no instant snapshots and no VM hook webhook.
out=$(render --set vmSnapshots.method=copy)
expect_contains "copy: controller runs with method copy" "$out" '"--vm-snapshot-method=copy"'
expect_absent "copy: no VM hook webhook" "$out" "kind: MutatingWebhookConfiguration"

# Webhooks off: no VM hook webhook either.
out=$(render --set webhook.enabled=false)
expect_absent "webhooks off: no VM hook webhook" "$out" "kind: MutatingWebhookConfiguration"

# The Sidecar gate Job: off by default.
out=$(render)
expect_absent "default: no Sidecar gate Job" "$out" '"--mode=enable-sidecar-gate"'
expect_absent "default: no HyperConverged update right for a gate Job" "$out" "sidecar-gate"

# On: a hook Job after install and upgrade turns the gate on; nothing turns it off.
out=$(render --set vmSnapshots.enableSidecarGate=true)
gate=$(awk '/^---/{if (doc ~ /kind: Job/ && doc ~ /enable-sidecar-gate/) print doc; doc=""; next} {doc = doc $0 "\n"} END{if (doc ~ /kind: Job/ && doc ~ /enable-sidecar-gate/) print doc}' <<<"$out")
expect_contains "gate on: Job runs the enable-sidecar-gate mode" "$gate" '"--mode=enable-sidecar-gate"'
expect_contains "gate on: Job runs after install and upgrade" "$gate" '"helm.sh/hook": post-install,post-upgrade'
expect_absent "gate on: the Job never runs on delete" "$gate" "pre-delete"
expect_contains "gate on: Job uses its own service account" "$gate" "serviceAccountName: test-ibm-vpc-file-pool-csi-sidecar-gate"
role=$(awk '/^---/{if (doc ~ /kind: ClusterRole\n/ && doc ~ /sidecar-gate/) print doc; doc=""; next} {doc = doc $0 "\n"} END{if (doc ~ /kind: ClusterRole\n/ && doc ~ /sidecar-gate/) print doc}' <<<"$out")
expect_contains "gate on: RBAC on HyperConverged only" "$role" 'resources: ["hyperconvergeds"]'
expect_contains "gate on: RBAC get, list and update only" "$role" 'verbs: ["get", "list", "update"]'
notes=$(helm install test "$CHART" --namespace kube-system --dry-run=client --set vmSnapshots.enableSidecarGate=true 2>&1 | sed -n "/^NOTES:/,\$p")
expect_contains "gate on: install notes say the gate is on" "$notes" "turns on the KubeVirt Sidecar feature gate"
expect_contains "gate on: install notes say Red Hat does not support it" "$notes" "Red Hat does not support"
notes=$(helm install test "$CHART" --namespace kube-system --dry-run=client 2>&1 | sed -n "/^NOTES:/,\$p")
expect_absent "default: install notes do not mention the gate" "$notes" "Sidecar feature gate"

if render --set vmSnapshots.method=copy --set vmSnapshots.enableSidecarGate=true >/dev/null 2>&1; then
    fail "the gate with method copy must fail to render (it would do nothing)"
else
    pass "the gate with method copy fails to render"
fi

if render --set vmSnapshots.method=fast >/dev/null 2>&1; then
    fail "an unknown method must fail to render"
else
    pass "an unknown method fails to render"
fi

exit "$FAILED"
