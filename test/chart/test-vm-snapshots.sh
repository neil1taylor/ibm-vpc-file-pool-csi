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

if render --set vmSnapshots.method=fast >/dev/null 2>&1; then
    fail "an unknown method must fail to render"
else
    pass "an unknown method fails to render"
fi

exit "$FAILED"
