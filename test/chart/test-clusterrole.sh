#!/usr/bin/env bash
# test-clusterrole.sh — The controller role lets the controller own objects.
#
# OpenShift refuses an object whose owner reference sets blockOwnerDeletion
# unless the creator may update the owner's "finalizers". The backup controller
# makes ReplicationPolicies owned by a BackupPolicy, so the role must allow
# "backuppolicies/finalizers". The lab run on rovslab found this.
#
# Before a Job changes the files of a VM disk, the controller lists the
# VirtualMachineInstances, so a VM that starts is seen. That needs "list".
#
# The volume resizer sidecar watches pods, so it can see a volume that a pod
# still uses. Without "watch" on pods it logs "Failed to watch *v1.Pod" for
# ever, and on some clusters it restarts without end.
#
# Usage: bash test/chart/test-clusterrole.sh
# Requires: helm.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

OWNERS=(backuppolicies)

for owner in "${OWNERS[@]}"; do
    if helm template test "$CHART" --namespace kube-system | grep -qF "\"$owner/finalizers\""; then
        pass "chart: the controller may update $owner/finalizers"
    else
        fail "chart: the controller role lacks $owner/finalizers"
    fi
    if grep -qF "\"$owner/finalizers\"" "$ROOT/config/rbac/clusterrole.yaml"; then
        pass "config/rbac: the controller may update $owner/finalizers"
    else
        fail "config/rbac: the controller role lacks $owner/finalizers"
    fi
done

# vmi_verbs FILE prints the verbs of the rule for virtualmachineinstances.
vmi_verbs() { grep -A1 -F 'resources: ["virtualmachineinstances"]' "$1" | grep 'verbs:' || true; }

RENDERED="$(mktemp)"
trap 'rm -f "$RENDERED"' EXIT
helm template test "$CHART" --namespace kube-system > "$RENDERED"
for file in "$RENDERED" "$ROOT/config/rbac/clusterrole.yaml"; do
    name="chart"
    [ "$file" = "$RENDERED" ] || name="config/rbac"
    if vmi_verbs "$file" | grep -qF '"list"'; then
        pass "$name: the controller may list VirtualMachineInstances"
    else
        fail "$name: the controller role cannot list VirtualMachineInstances"
    fi
done

# pod_verbs FILE prints the verbs of the rule for pods.
pod_verbs() { grep -A1 -F 'resources: ["pods"]' "$1" | grep 'verbs:' || true; }

for file in "$RENDERED" "$ROOT/config/rbac/clusterrole.yaml"; do
    name="chart"
    [ "$file" = "$RENDERED" ] || name="config/rbac"
    if pod_verbs "$file" | grep -qF '"watch"'; then
        pass "$name: the controller may watch pods (the volume resizer needs it)"
    else
        fail "$name: the controller role cannot watch pods, so the volume resizer fails"
    fi
done

exit "$FAILED"
