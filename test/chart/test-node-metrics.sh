#!/usr/bin/env bash
# test-node-metrics.sh — The node agent serves per-share metrics and Prometheus scrapes them.
#
# The node agent reads used bytes and NFS statistics of the shares mounted on its node.
# It needs a port, a PodMonitor (only when the cluster has the Prometheus Operator API),
# and the right to read FileSharePools to name the shares.
#
# Usage: bash test/chart/test-node-metrics.sh
# Requires: helm.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

render() { helm template test "$CHART" --namespace kube-system "$@"; }

# node_doc prints the DaemonSet of the node agent.
node_doc() { awk 'BEGIN{RS="---\n"} /kind: DaemonSet/ && /component: node/ {print}'; }

check() { # check NAME CONDITION-RESULT
    if [ "$2" = "yes" ]; then pass "$1"; else fail "$1"; fi
}
has() { grep -qF -- "$1" <<<"$2" && echo yes || echo no; }

DS="$(render | node_doc)"
check "default: node agent gets --node-metrics-addr=:9810" "$(has '--node-metrics-addr=:9810' "$DS")"
check "default: node agent gets --share-stats-interval=30s" "$(has '--share-stats-interval=30s' "$DS")"
check "default: node agent declares the metrics container port" "$(has 'containerPort: 9810' "$DS")"

DS="$(render --set node.metricsPort=9911 | node_doc)"
check "port value: --node-metrics-addr follows node.metricsPort" "$(has '--node-metrics-addr=:9911' "$DS")"

DS="$(render --set node.metricsPort=0 | node_doc)"
check "port 0: the metrics flag is empty, which turns the endpoint off" "$(has '"--node-metrics-addr="' "$DS")"
check "port 0: no container port" "$([ "$(has 'containerPort' "$DS")" = no ] && echo yes || echo no)"

# ServiceMonitor for the node agents: only with the API, and not when turned off.
SM_API="monitoring.coreos.com/v1/ServiceMonitor"
node_sm() { awk 'BEGIN{RS="---\n"} /kind: ServiceMonitor/ && /-node\n/ {print}'; }
check "default: node ServiceMonitor rendered when the API exists" "$(has 'component: node' "$(render --api-versions "$SM_API" | node_sm)")"
check "default: no node ServiceMonitor without the API" "$([ -z "$(render | node_sm)" ] && echo yes || echo no)"
check "serviceMonitor disabled: no node ServiceMonitor" "$([ -z "$(render --api-versions "$SM_API" --set metrics.serviceMonitor.enabled=false | node_sm)" ] && echo yes || echo no)"
check "port 0: no node ServiceMonitor" "$([ -z "$(render --api-versions "$SM_API" --set node.metricsPort=0 | node_sm)" ] && echo yes || echo no)"

SM="$(render --api-versions "$SM_API" | node_sm)"
check "ServiceMonitor: scrapes the metrics port" "$(has 'port: metrics' "$SM")"
check "ServiceMonitor: uses https" "$(has 'scheme: https' "$SM")"
check "ServiceMonitor: sends the bearer token" "$(has 'bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token' "$SM")"
check "ServiceMonitor: uses the ServiceMonitor interval" "$(has 'interval: 30s' "$SM")"

SVC="$(render | awk 'BEGIN{RS="---\n"} /kind: Service\n/ && /-node-metrics\n/ {print}')"
check "node metrics Service: headless" "$(has 'clusterIP: None' "$SVC")"
check "node metrics Service: selects the node pods" "$(has 'component: node' "$SVC")"
check "node metrics Service: port 9810" "$(has 'port: 9810' "$SVC")"
check "port 0: no node metrics Service" "$([ -z "$(render --set node.metricsPort=0 | awk 'BEGIN{RS="---\n"} /kind: Service\n/ && /-node-metrics\n/ {print}')" ] && echo yes || echo no)"

# The scraper sends its own token, so it must check who answers: the certificate has to be
# one it can verify, for every certificate provider. Never skip the check.
SVCNAME="test-ibm-vpc-file-pool-csi-node-metrics"
SECRET="test-ibm-vpc-file-pool-csi-node-metrics-certs"
for provider in cert-manager openshift manual; do
    OUT="$(render --api-versions "$SM_API" --set webhook.certProvider=$provider --set webhook.caBundle=Q0E=)"
    SM="$(node_sm <<<"$OUT")"
    DSP="$(node_doc <<<"$OUT")"
    check "$provider: ServiceMonitor does not skip certificate checks" "$([ "$(has 'insecureSkipVerify' "$SM")" = no ] && echo yes || echo no)"
    check "$provider: ServiceMonitor checks the Service name" "$(has "serverName: $SVCNAME.kube-system.svc" "$SM")"
    check "$provider: node agent reads its certificate from the secret" "$(has "secretName: $SECRET" "$DSP")"
    check "$provider: node agent gets --node-metrics-cert-dir" "$(has '--node-metrics-cert-dir=/etc/node-metrics-certs' "$DSP")"
done

OUT="$(render --api-versions "$SM_API" --set webhook.certProvider=openshift)"
check "openshift: Service asks the service CA for a certificate" "$(has "service.beta.openshift.io/serving-cert-secret-name: $SECRET" "$OUT")"
check "openshift: ServiceMonitor trusts the service CA bundle" "$(has 'caFile: /etc/prometheus/configmaps/serving-certs-ca-bundle/service-ca.crt' "$(node_sm <<<"$OUT")")"
check "openshift: no cert-manager Certificate for the node metrics" "$([ "$(has "$SVCNAME" "$(awk 'BEGIN{RS="---\n"} /kind: Certificate/ {print}' <<<"$OUT")")" = no ] && echo yes || echo no)"

OUT="$(render --api-versions "$SM_API")"
check "cert-manager: Certificate for the Service name" "$(has "$SVCNAME.kube-system.svc" "$(awk 'BEGIN{RS="---\n"} /kind: Certificate/ {print}' <<<"$OUT")")"
check "cert-manager: ServiceMonitor trusts ca.crt of the secret" "$(has "name: $SECRET" "$(node_sm <<<"$OUT")")"
check "cert-manager: no OpenShift annotation on the node metrics Service" "$([ "$(has 'serving-cert-secret-name: '$SECRET "$OUT")" = no ] && echo yes || echo no)"

OUT="$(render --api-versions "$SM_API" --set webhook.certProvider=manual --set webhook.caBundle=Q0E=)"
check "manual: no Certificate for the node metrics" "$([ "$(has "$SVCNAME" "$(awk 'BEGIN{RS="---\n"} /kind: Certificate/ {print}' <<<"$OUT")")" = no ] && echo yes || echo no)"
check "manual: ServiceMonitor trusts ca.crt of the secret" "$(has "name: $SECRET" "$(node_sm <<<"$OUT")")"

# Port 0: no certificate mount either.
check "port 0: no certificate volume" "$([ "$(has "$SECRET" "$(render --set node.metricsPort=0 | node_doc)")" = no ] && echo yes || echo no)"

# The node agent checks the caller's token and rights, so it may create TokenReviews and
# SubjectAccessReviews; the scraper's role may get /metrics.
NODEROLE="$(render | awk 'BEGIN{RS="---\n"} /kind: ClusterRole\n/ && /-node\n/ {print}')"
check "chart: node role may create tokenreviews" "$(has 'tokenreviews' "$NODEROLE")"
check "chart: node role may create subjectaccessreviews" "$(has 'subjectaccessreviews' "$NODEROLE")"
check "chart: a metrics-reader role may get /metrics" "$(has '/metrics' "$(render | awk 'BEGIN{RS="---\n"} /kind: ClusterRole\n/ && /metrics-reader/ {print}')")"
check "chart: the reader role is bound to the Prometheus service account" "$(has 'prometheus-k8s' "$(render | awk 'BEGIN{RS="---\n"} /kind: ClusterRoleBinding\n/ && /metrics-reader/ {print}')")"
check "chart: no reader binding when metrics.scraper.serviceAccount is empty" "$([ -z "$(render --set metrics.scraper.serviceAccount= | awk 'BEGIN{RS="---\n"} /kind: ClusterRoleBinding\n/ && /metrics-reader/ {print}')" ] && echo yes || echo no)"
check "config/rbac: node role may create tokenreviews" "$(has 'tokenreviews' "$(cat "$ROOT/config/rbac/clusterrole.yaml")")"

# RBAC: the node role may read FileSharePools (names the shares), in chart and plain manifests.
role_has_pools() { awk 'BEGIN{RS="---\n"} /kind: ClusterRole\n/ && /-node\n/ {print}' <<<"$1" | grep -A2 'filesharepools' | grep -qF '"list"' && echo yes || echo no; }
check "chart: node role may list filesharepools" "$(role_has_pools "$(render)")"
check "config/rbac: node role may list filesharepools" "$(role_has_pools "$(cat "$ROOT/config/rbac/clusterrole.yaml")")"

exit "$FAILED"
