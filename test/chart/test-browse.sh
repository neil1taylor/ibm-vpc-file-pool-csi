#!/usr/bin/env bash
# test-browse.sh — The chart sets up browse sessions (issue #92).
#
# The controller gets the browse image and the limits, may manage BackupBrowse
# objects, and may make and delete the session Secrets, but only in its own
# namespace (a Role, not the ClusterRole). A NetworkPolicy lets only the backup
# gateway reach the helper pods, on the helper's port. With backupBrowse.enabled
# false none of this is rendered.
#
# The helper pods and the session Secrets live in their own namespace
# (ibm-vpc-file-pool-csi-browse), which holds nothing else, so the gateway's right
# to read Secrets there reaches nothing else. The namespace allows pods that run
# as root (the helper needs it to read backups made by other users) without any
# capability. On OpenShift one SCC allows exactly that for the namespace.
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
    --set backupBrowse.maxSessions=7 --set backupBrowse.maxSessionsPerNamespace=3 \
    --set backupBrowse.namespace=my-browse > "$CUSTOM"

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
check "the browse namespace is made" "$ON" 'kind: Namespace'
check "the browse namespace has its own name" "$ON" 'name: ibm-vpc-file-pool-csi-browse'
check "the browse namespace allows root without privilege (baseline)" "$ON" 'pod-security.kubernetes.io/enforce: baseline'
check "the controller is told the browse namespace" "$ON" '--browse-namespace=ibm-vpc-file-pool-csi-browse'
check "the namespace can be changed" "$CUSTOM" '--browse-namespace=my-browse'
check "a Role gives the controller the session Secrets in the browse namespace" "$ON" 'kind: Role'
check "the Role may create Secrets" "$ON" 'verbs: ["get", "list", "watch", "create", "delete"]'
check "a NetworkPolicy guards the helper pods" "$ON" 'kind: NetworkPolicy'
check "the policy selects helper pods" "$ON" 'storage.ibmcloud.io/browse'
check "the policy lets the gateway in" "$ON" 'app.kubernetes.io/component: backup-gateway'
check "the policy names the gateway's namespace" "$ON" 'kubernetes.io/metadata.name: kube-system'
check "the policy names the helper port" "$ON" 'port: 8443'
PULL="$(mktemp)"
helm template test "$CHART" --namespace kube-system --set 'imagePullSecrets[0].name=all-icr-io' > "$PULL"
check "the helper pulls with the chart's pull secrets" "$PULL" '--browse-image-pull-secrets=all-icr-io'
absent "no pull secrets, no flag" "$ON" '--browse-image-pull-secrets'
find "$PULL" -delete
absent "off: no browse image" "$OFF" '--browse-image'
absent "off: no NetworkPolicy" "$OFF" 'kind: NetworkPolicy'
absent "off: no browse Role" "$OFF" "name: test-ibm-vpc-file-pool-csi-browse"

# The non-Helm role has the same rule.
check "config/rbac: the controller manages BackupBrowse objects" "$ROOT/config/rbac/clusterrole.yaml" '"backupbrowses"'

# No Role in the release namespace may grant Secrets to the browse or gateway parts.
if awk -v RS='\n---\n' '/\nkind: Role\n/ && /namespace: kube-system\n/ && /-(browse|gateway)\n/ && /secrets/' "$ON" | grep -q .; then
    fail "a Role in kube-system grants Secrets to the browse or gateway parts"
else
    pass "no Role in the release namespace grants Secrets to the browse or gateway parts"
fi
# Every Role that grants Secrets for sessions is in the browse namespace.
if awk -v RS='\n---\n' '/\nkind: Role\n/ && /secrets/ && /-(browse|gateway)\n/ && !/namespace: ibm-vpc-file-pool-csi-browse\n/' "$ON" | grep -q .; then
    fail "a Secrets Role for sessions is outside the browse namespace"
else
    pass "every Secrets Role for sessions is in the browse namespace"
fi

# OpenShift: one SCC lets the helper run as root with no capabilities.
SCC="$(mktemp)"
helm template test "$CHART" --namespace kube-system --api-versions security.openshift.io/v1 > "$SCC"
check "openshift: an SCC for the helper" "$SCC" 'kind: SecurityContextConstraints'
check "openshift: it names the browse namespace's default account" "$SCC" 'system:serviceaccount:ibm-vpc-file-pool-csi-browse:default'
check "openshift: it allows root" "$SCC" 'type: RunAsAny'
check "openshift: it drops every capability" "$SCC" '- ALL'
check "openshift: it refuses privilege escalation" "$SCC" 'allowPrivilegeEscalation: false'
awk -v RS='\n---\n' '/kind: SecurityContextConstraints/' "$SCC" > "$SCC.only"
absent "openshift: it allows no host path" "$SCC.only" 'hostPath'
absent "no SCC without the OpenShift API" "$ON" 'SecurityContextConstraints'
find "$SCC" "$SCC.only" -delete

# The Role must not be a ClusterRole rule: Secrets stay out of the cluster-wide verbs.
if awk '/kind: ClusterRole$/{c=1} /kind: Role$/{c=0} c && /secrets/ && /delete/' "$ON" | grep -q .; then
    fail "the cluster-wide role must not delete Secrets"
else
    pass "the cluster-wide role does not delete Secrets"
fi

exit "$FAILED"
