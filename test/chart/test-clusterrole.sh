#!/usr/bin/env bash
# test-clusterrole.sh — The controller role lets the controller own objects.
#
# OpenShift refuses an object whose owner reference sets blockOwnerDeletion
# unless the creator may update the owner's "finalizers". The backup controller
# makes ReplicationPolicies owned by a BackupPolicy, so the role must allow
# "backuppolicies/finalizers". The lab run on rovslab found this.
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

exit "$FAILED"
