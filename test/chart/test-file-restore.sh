#!/usr/bin/env bash
# test-file-restore.sh — The chart sets up file restore (issue #92, phase 2).
#
# A BackupFileRestore copies chosen files out of a backup into a claim. The
# controller runs it, so its role must reach the resource and its status. A
# validating webhook checks every request. The size limit of one restore is a
# controller flag with a Helm value.
#
# Usage: bash test/chart/test-file-restore.sh
# Requires: helm.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
check() { if grep -qF -- "$3" "$2"; then pass "$1"; else fail "$1 (missing: $3)"; fi; }

RENDERED="$(mktemp)"; SET="$(mktemp)"
trap 'find "$RENDERED" "$SET" -delete' EXIT
helm template test "$CHART" --namespace kube-system > "$RENDERED"
helm template test "$CHART" --namespace kube-system --set backupBrowse.restoreMaxBytes=1073741824 > "$SET"

check "chart: the controller reaches file restores and their status" "$RENDERED" 'resources: ["backupfilerestores", "backupfilerestores/status"]'
check "config/rbac: the controller reaches file restores and their status" "$ROOT/config/rbac/clusterrole.yaml" 'resources: ["backupfilerestores", "backupfilerestores/status"]'
check "chart: a webhook checks file restores" "$RENDERED" 'name: validate-backupfilerestore.storage.ibmcloud.io'
check "chart: the webhook path" "$RENDERED" 'path: /validate-storage-ibmcloud-io-v1alpha1-backupfilerestore'
check "chart: the webhook sees the resource" "$RENDERED" 'resources: ["backupfilerestores"]'
check "chart: the size limit of one restore defaults to 200 GiB" "$RENDERED" '--file-restore-max-bytes=214748364800'
check "chart: the size limit can be set" "$SET" '--file-restore-max-bytes=1073741824'
check "the CRD is in config/crd" "$ROOT/config/crd/storage.ibmcloud.io_backupfilerestores.yaml" 'kind: BackupFileRestore'

# The controller must not be able to delete ConfigMaps: the list of paths of a
# restore is removed with the restore by its owner reference.
if awk -v RS='\n  - ' '/resources: \["configmaps"\]/ && /delete/' "$ROOT/config/rbac/clusterrole.yaml" | grep -q .; then
    fail "config/rbac: the controller may delete ConfigMaps"
else
    pass "config/rbac: the controller may not delete ConfigMaps"
fi

exit "$FAILED"
