#!/usr/bin/env bash
# test-gateway.sh — The chart sets up the backup gateway and its rights (issue #92).
#
# The gateway is the one door to browse sessions. It checks every caller with a
# token review and a SubjectAccessReview on the rights-only name "backupfiles".
# The chart ships two ClusterRoles for people (backup-file-reader aggregates into
# "admin"), a gateway ServiceAccount with only the rights it needs (Secrets only
# by a Role in its own namespace), a Service, a passthrough Route on OpenShift,
# and a ConfigMap with the gateway address that every signed-in user may read.
#
# Usage: bash test/chart/test-gateway.sh
# Requires: helm.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
check() { if grep -qF -- "$3" "$2"; then pass "$1"; else fail "$1 (missing: $3)"; fi; }
absent() { if grep -qF -- "$3" "$2"; then fail "$1 (found: $3)"; else pass "$1"; fi; }

PLAIN="$(mktemp)"; ON="$(mktemp)"; OFF="$(mktemp)"; OS="$(mktemp)"; OWN="$(mktemp)"; NOBROWSE="$(mktemp)"
trap 'find "$PLAIN" "$ON" "$OFF" "$OS" "$OWN" "$NOBROWSE" -delete' EXIT
helm template test "$CHART" --namespace kube-system > "$PLAIN"
helm template test "$CHART" --namespace kube-system --api-versions route.openshift.io/v1 > "$ON"
helm template test "$CHART" --namespace kube-system --set backupGateway.enabled=false > "$OFF"
helm template test "$CHART" --namespace kube-system --api-versions route.openshift.io/v1 \
    --set backupGateway.route.host=backups.example.com > "$OS"
helm template test "$CHART" --namespace kube-system --api-versions route.openshift.io/v1 --set backupGateway.tls.secretName=my-cert \
    --set backupGateway.url=https://gw.example.com > "$OWN"
helm template test "$CHART" --namespace kube-system --set backupBrowse.enabled=false > "$NOBROWSE"

check "the gateway runs in backup-gateway mode" "$ON" '- --mode=backup-gateway'
check "the gateway is served over TLS from files" "$ON" '--gateway-tls-cert-file=/etc/gateway-tls/tls.crt'
check "the gateway has its own component label" "$ON" 'app.kubernetes.io/component: backup-gateway'
check "the readiness probe uses HTTPS" "$ON" 'scheme: HTTPS'
check "the gateway container drops every capability" "$ON" 'allowPrivilegeEscalation: false'
check "OpenShift makes the serving certificate" "$ON" 'service.beta.openshift.io/serving-cert-secret-name: test-ibm-vpc-file-pool-csi-gateway-tls'
check "the gateway may review tokens" "$ON" 'tokenreviews'
check "the gateway may review access" "$ON" 'subjectaccessreviews'
check "the gateway may read policies" "$ON" 'resources: ["backuppolicies"]'
check "the gateway may make sessions" "$ON" 'resources: ["backupbrowses"]'
check "the gateway reads session Secrets by a Role" "$ON" 'resources: ["secrets"]'
check "the gateway is told the browse namespace" "$ON" '--gateway-browse-namespace=ibm-vpc-file-pool-csi-browse'
if awk -v RS='\n---\n' '/\nkind: Role\n/ && /name: test-ibm-vpc-file-pool-csi-gateway\n/ && /secrets/ && /namespace: kube-system\n/' "$ON" | grep -q .; then
    fail "a gateway Role in kube-system grants Secrets"
else
    pass "no gateway Role in kube-system grants Secrets"
fi
if awk -v RS='\n---\n' '/\nkind: Role\n/ && /secrets/ && /namespace: ibm-vpc-file-pool-csi-browse\n/ && /-gateway-secrets\n/' "$ON" | grep -q .; then
    pass "the gateway reads Secrets in the browse namespace only"
else
    fail "the gateway has no Secrets Role in the browse namespace"
fi
check "the gateway writes audit Events" "$ON" 'resources: ["events"]'
check "backup-file-reader exists" "$ON" 'name: backup-file-reader'
check "backup-file-restorer exists" "$ON" 'name: backup-file-restorer'
check "the rights-only name" "$ON" 'resources: ["backupfiles"]'
check "the reader aggregates into admin" "$ON" 'rbac.authorization.k8s.io/aggregate-to-admin: "true"'
check "the address ConfigMap" "$ON" 'name: backup-gateway-info'
check "everyone signed in may read only that ConfigMap" "$ON" 'resourceNames: ["backup-gateway-info"]'
check "system:authenticated is bound" "$ON" 'name: system:authenticated'
absent "no Route without the OpenShift API" "$PLAIN" 'kind: Route'
absent "no OpenShift annotations without the OpenShift API" "$PLAIN" 'service.beta.openshift.io'
check "a passthrough Route on OpenShift" "$OS" 'termination: passthrough'
check "the Route has the host" "$OS" 'host: backups.example.com'
check "the address ConfigMap has the Route's address" "$OS" 'url: "https://backups.example.com"'
check "the CA bundle is injected" "$OS" 'service.beta.openshift.io/inject-cabundle: "true"'
check "an own certificate Secret is used" "$OWN" 'secretName: my-cert'
absent "no serving-cert annotation with an own certificate" "$OWN" 'service.beta.openshift.io/serving-cert-secret-name'
check "the address can be set" "$OWN" 'url: "https://gw.example.com"'
absent "off: no gateway Deployment" "$OFF" 'mode=backup-gateway'
absent "off: no backup-file-reader" "$OFF" 'backup-file-reader'
absent "no browse, no gateway" "$NOBROWSE" 'mode=backup-gateway'

# The gateway's cluster-wide role must not read or write Secrets.
if awk -v RS='\n---\n' '/\nkind: ClusterRole\n/ && /name: test-ibm-vpc-file-pool-csi-gateway\n/ && /secrets/' "$ON" | grep -q .; then
    fail "the cluster-wide gateway role must not touch Secrets"
else
    pass "the cluster-wide gateway role does not touch Secrets"
fi

# The non-Helm manifests have the same two roles.
check "config/rbac: backup-file-reader" "$ROOT/config/rbac/backup-file-roles.yaml" 'name: backup-file-reader'
check "config/rbac: aggregation into admin" "$ROOT/config/rbac/backup-file-roles.yaml" 'rbac.authorization.k8s.io/aggregate-to-admin: "true"'

exit "$FAILED"
