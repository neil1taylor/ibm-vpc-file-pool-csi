#!/usr/bin/env bash
# test-browse.sh — The chart sets up browse sessions (issue #92).
#
# The controller gets the browse image and the limits, may manage BackupBrowse
# objects, and may make and delete the session Secrets, but only in its own
# namespace (a Role, not the ClusterRole). A NetworkPolicy lets only the backup
# gateway reach the helper pods, on the helper's port. With backupBrowse.enabled
# false none of this is rendered.
#
# Usage: bash test/chart/test-browse.sh
# Requires: helm.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
check() { # check DESC FILE PATTERN
    if grep -qF -- "$3" "$2"; then pass "$1"; else fail "$1 (missing: $3)"; fi
}
absent() { # absent DESC FILE PATTERN
    if grep -qF -- "$3" "$2"; then fail "$1 (found: $3)"; else pass "$1"; fi
}

ON="$(mktemp)"
OFF="$(mktemp)"
CUSTOM="$(mktemp)"
trap 'rm -f "$ON" "$OFF" "$CUSTOM"' EXIT
helm template test "$CHART" --namespace kube-system --set image.tag=v1.2.3 > "$ON"
helm template test "$CHART" --namespace kube-system --set backupBrowse.enabled=false > "$OFF"
helm template test "$CHART" --namespace kube-system --set image.tag=v1.2.3 \
    --set backupBrowse.image.repository=example.com/b --set backupBrowse.image.tag=t9 \
    --set backupBrowse.idle=5m --set backupBrowse.maxLife=1h \
    --set backupBrowse.maxSessions=7 --set backupBrowse.maxSessionsPerNamespace=3 > "$CUSTOM"

check "the controller gets the browse image, from the driver's tag" "$ON" '--browse-image=de.icr.io/ibm-vpc-file-pool-csi/browse:v1.2.3'
check "default idle time" "$ON" '--browse-idle=15m'
check "default hard limit" "$ON" '--browse-max-life=2h'
check "default open sessions" "$ON" '--browse-max-sessions=5'
check "default open sessions for each namespace" "$ON" '--browse-max-sessions-per-namespace=2'
check "the image can be changed" "$CUSTOM" '--browse-image=example.com/b:t9'
check "the limits can be changed (idle)" "$CUSTOM" '--browse-idle=5m'
check "the limits can be changed (hard)" "$CUSTOM" '--browse-max-life=1h'
check "the limits can be changed (sessions)" "$CUSTOM" '--browse-max-sessions=7'
check "the limits can be changed (per namespace)" "$CUSTOM" '--browse-max-sessions-per-namespace=3'
check "the controller manages BackupBrowse objects" "$ON" '"backupbrowses", "backupbrowses/status"'
check "a Role gives the controller the session Secrets in its namespace" "$ON" 'kind: Role'
check "the Role may create Secrets" "$ON" 'verbs: ["get", "list", "watch", "create", "delete"]'
check "a NetworkPolicy guards the helper pods" "$ON" 'kind: NetworkPolicy'
check "the policy selects helper pods" "$ON" 'storage.ibmcloud.io/browse'
check "the policy lets the gateway in" "$ON" 'app.kubernetes.io/component: backup-gateway'
check "the policy names the helper port" "$ON" 'port: 8443'
absent "off: no browse image" "$OFF" '--browse-image'
absent "off: no NetworkPolicy" "$OFF" 'kind: NetworkPolicy'
absent "off: no Role" "$OFF" 'kind: Role'

# The non-Helm role has the same rule.
check "config/rbac: the controller manages BackupBrowse objects" "$ROOT/config/rbac/clusterrole.yaml" '"backupbrowses"'

# The Role must not be a ClusterRole rule: Secrets stay out of the cluster-wide verbs.
if awk '/kind: ClusterRole$/{c=1} /kind: Role$/{c=0} c && /secrets/ && /delete/' "$ON" | grep -q .; then
    fail "the cluster-wide role must not delete Secrets"
else
    pass "the cluster-wide role does not delete Secrets"
fi

exit "$FAILED"
