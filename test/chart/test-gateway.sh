#!/usr/bin/env bash
# test-gateway.sh — The chart sets up the backup gateway and its rights (issue #92).
#
# The gateway is the one door to browse sessions. It checks every caller with a
# token review and a SubjectAccessReview on the rights-only name "backupfiles".
# The chart ships two ClusterRoles for people (backup-file-reader aggregates into
# "admin"), a gateway ServiceAccount with only the rights it needs (Secrets only
# by a Role in its own namespace), a Service, a reencrypt Route on OpenShift,
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
check "the gateway may ask for file restores" "$ON" 'resources: ["backupfilerestores"]'
check "one user may have 10 open restores" "$ON" '--gateway-max-open-restores=10'
check "the gateway may read claims, to take the UID of a target" "$ON" 'resources: ["persistentvolumeclaims"]'
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
if awk -v RS='\n---\n' '/\nkind: Role\n/ && /name: test-ibm-vpc-file-pool-csi-gateway\n/ && /events/ && /namespace: default\n/' "$ON" | grep -q . \
    && awk -v RS='\n---\n' '/\nkind: RoleBinding\n/ && /name: test-ibm-vpc-file-pool-csi-gateway\n/ && /namespace: default\n/' "$ON" | grep -q .; then
    pass "the audit Events are allowed in default, the only namespace for Events of cluster-scoped objects"
else
    fail "the gateway may not write Events in default"
fi
check "backup-file-reader exists" "$ON" 'name: backup-file-reader'
check "backup-file-restorer exists" "$ON" 'name: backup-file-restorer'
check "the rights-only name" "$ON" 'resources: ["backupfiles"]'
check "the reader aggregates into admin" "$ON" 'rbac.authorization.k8s.io/aggregate-to-admin: "true"'
check "the address ConfigMap" "$ON" 'name: backup-gateway-info'
check "everyone signed in may read only that ConfigMap" "$ON" 'resourceNames: ["backup-gateway-info"]'
check "system:authenticated is bound" "$ON" 'name: system:authenticated'
absent "no Route without the OpenShift API" "$PLAIN" 'kind: Route'
absent "no OpenShift annotations without the OpenShift API" "$PLAIN" 'service.beta.openshift.io'
check "a reencrypt Route on OpenShift" "$OS" 'termination: reencrypt'
absent "the Route is not passthrough (the service certificate is for the service name, not the Route host)" "$OS" 'termination: passthrough'
check "the Route has the host" "$OS" 'host: backups.example.com'
check "the address ConfigMap has the Route's address" "$OS" 'url: "https://backups.example.com"'
absent "no CA bundle is injected (clients use the router certificate)" "$OS" 'inject-cabundle'
check "an own certificate Secret is used" "$OWN" 'secretName: my-cert'
absent "no serving-cert annotation with an own certificate" "$OWN" 'service.beta.openshift.io/serving-cert-secret-name'
check "the address can be set" "$OWN" 'url: "https://gw.example.com"'
# Offline there is no ingress domain to look up, so the address stays empty.
check "no address without a cluster to ask" "$ON" 'url: ""'
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

# The console reaches the gateway through the plugin proxy, with the person's own token.
if command -v yq >/dev/null; then
    CPAPI="--api-versions console.openshift.io/v1/ConsolePlugin"
    proxy() { yq -N eval "select(.kind == \"ConsolePlugin\") | .spec.proxy[] | select(.alias == \"$2\") | $3" - <<<"$1"; }
    both=$(helm template test "$CHART" --namespace kube-system $CPAPI --set poolBench.enabled=true)
    only_gw=$(helm template test "$CHART" --namespace kube-system $CPAPI)
    only_pb=$(helm template test "$CHART" --namespace kube-system $CPAPI --set poolBench.enabled=true --set backupGateway.enabled=false)
    nogw_browse=$(helm template test "$CHART" --namespace kube-system $CPAPI --set backupBrowse.enabled=false)
    none=$(helm template test "$CHART" --namespace kube-system $CPAPI --set backupGateway.enabled=false)
    eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1 (got: $2, want: $3)"; fi; }
    eq "the proxy names the gateway Service" "$(proxy "$only_gw" backup-gateway .endpoint.service.name)" "test-ibm-vpc-file-pool-csi-gateway"
    eq "the proxy uses the gateway port" "$(proxy "$only_gw" backup-gateway .endpoint.service.port)" "8443"
    eq "the proxy passes the person's own token" "$(proxy "$only_gw" backup-gateway .authorization)" "UserToken"
    eq "the proxy is in the release namespace" "$(proxy "$only_gw" backup-gateway .endpoint.service.namespace)" "kube-system"
    eq "with PoolBench on both proxies are there (poolbench)" "$(proxy "$both" poolbench .endpoint.service.port)" "8444"
    eq "with PoolBench on both proxies are there (gateway)" "$(proxy "$both" backup-gateway .endpoint.service.port)" "8443"
    eq "PoolBench alone: no gateway proxy" "$(proxy "$only_pb" backup-gateway .authorization)" ""
    eq "PoolBench alone: its proxy stays" "$(proxy "$only_pb" poolbench .authorization)" "UserToken"
    eq "gateway off: no proxy key at all" "$(yq -N eval 'select(.kind == "ConsolePlugin") | .spec.proxy' - <<<"$none")" "null"
    eq "no browse namespace, no gateway proxy" "$(proxy "$nogw_browse" backup-gateway .authorization)" ""
    eq "the console proxy and the gateway Service agree on the name" \
        "$(yq -N eval 'select(.kind == "Service" and .metadata.name == "test-ibm-vpc-file-pool-csi-gateway") | .metadata.name' - <<<"$only_gw")" \
        "$(proxy "$only_gw" backup-gateway .endpoint.service.name)"
else
    fail "yq is required for the plugin proxy checks"
fi

# The console reads from the settings ConfigMap whether the file browser exists.
check "settings: the file browser is on" "$PLAIN" 'fileBrowse: "true"'
absent "settings: off with the gateway" "$OFF" 'fileBrowse: "true"'
check "settings: false with the gateway off" "$OFF" 'fileBrowse: "false"'
check "settings: false with browse off" "$NOBROWSE" 'fileBrowse: "false"'

exit "$FAILED"
