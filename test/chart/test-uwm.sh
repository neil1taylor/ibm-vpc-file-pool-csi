#!/usr/bin/env bash
# test-uwm.sh — Controller metrics are secured, and on OpenShift the alerts and scraping live
# in a monitoring namespace that user workload monitoring reads.
#
# Platform monitoring reads only namespaces labelled openshift.io/cluster-monitoring=true, and
# Red Hat says not to label other namespaces or to put user ServiceMonitors in kube-*. So the
# chart makes its own namespace for the ServiceMonitors, the PrometheusRule and selector-less
# Services (the controller keeps their Endpoints). It never touches cluster-monitoring-config.
#
# Usage: bash test/chart/test-uwm.sh
# Requires: helm.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CHART="$ROOT/charts/ibm-vpc-file-pool-csi"
SM="monitoring.coreos.com/v1/ServiceMonitor"
PR="monitoring.coreos.com/v1/PrometheusRule"
CP="console.openshift.io/v1/ConsolePlugin"
MON="ibm-vpc-file-pool-csi-monitoring"
FN="test-ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
render() { helm template test "$CHART" --namespace kube-system "$@"; }
check() { if [[ "$2" =~ ^(yes)+$ ]]; then pass "$1"; else fail "$1"; fi; }
has() { grep -qF -- "$1" <<<"$2" && echo yes || echo no; }
hasnt() { grep -qF -- "$1" <<<"$2" && echo no || echo yes; }
# doc KIND NAME-PART: the rendered documents of a kind whose text contains NAME-PART.
doc() { awk -v k="kind: $1" -v n="$2" 'BEGIN{RS="---\n"} index($0,k) && index($0,n) {print}'; }

OS=(--api-versions "$SM" --api-versions "$PR" --api-versions "$CP" --set webhook.certProvider=openshift)

# ---- Controller metrics are secured (in-namespace path, no OpenShift console API) ----
OUT="$(render --api-versions "$SM" --set webhook.certProvider=openshift)"
CSM="$(awk 'BEGIN{RS="---\n"} /kind: ServiceMonitor/ && /component: controller/ {print}' <<<"$OUT")"
check "controller ServiceMonitor uses https" "$(has 'scheme: https' "$CSM")"
check "controller ServiceMonitor sends the bearer token" "$(has 'bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token' "$CSM")"
check "controller ServiceMonitor checks the Service name" "$(has "serverName: $FN-metrics.kube-system.svc" "$CSM")"
check "controller ServiceMonitor never skips certificate checks" "$(hasnt 'insecureSkipVerify' "$CSM")"
check "openshift: controller ServiceMonitor trusts the service CA" "$(has 'caFile: /etc/prometheus/configmaps/serving-certs-ca-bundle/service-ca.crt' "$CSM")"
check "openshift: metrics Service asks the service CA for a certificate" "$(has "service.beta.openshift.io/serving-cert-secret-name: $FN-metrics-certs" "$OUT")"
DEP="$(render | awk 'BEGIN{RS="---\n"} /kind: Deployment/ && /component: controller/ {print}' | head -400)"
check "controller gets --metrics-cert-dir" "$(has '--metrics-cert-dir=/etc/metrics-certs' "$DEP")"
check "controller mounts the metrics certificate secret" "$(has "secretName: $FN-metrics-certs" "$DEP")"
ROLE="$(render | awk 'BEGIN{RS="---\n"} /kind: ClusterRole\n/ && /name: test-ibm-vpc-file-pool-csi-controller\n/ {print}')"
check "controller role may create tokenreviews" "$(has 'tokenreviews' "$ROLE")"
check "controller role may create subjectaccessreviews" "$(has 'subjectaccessreviews' "$ROLE")"

OUT="$(render --api-versions "$SM")"
CSM="$(awk 'BEGIN{RS="---\n"} /kind: ServiceMonitor/ && /component: controller/ {print}' <<<"$OUT")"
check "cert-manager: controller ServiceMonitor trusts ca.crt of its secret" "$(has "name: $FN-metrics-certs" "$CSM")"
CERT="$(awk 'BEGIN{RS="---\n"} /kind: Certificate/ && /-metrics-certs/ && !/node-metrics/ {print}' <<<"$OUT")"
check "cert-manager: Certificate for the controller metrics Service" "$(has "$FN-metrics.kube-system.svc" "$CERT")"
check "cert-manager: no OpenShift annotation on the metrics Service" "$(hasnt 'serving-cert-secret-name: '$FN-metrics-certs "$OUT")"

# ---- OpenShift: monitoring namespace for user workload monitoring ----
OUT="$(render "${OS[@]}")"
NSDOC="$(doc Namespace "name: $MON" <<<"$OUT")"
check "uwm: monitoring namespace is created" "$([ -n "$NSDOC" ] && echo yes || echo no)"
check "uwm: monitoring namespace has no cluster-monitoring label" "$(hasnt 'cluster-monitoring' "$NSDOC")"
check "uwm: never changes cluster-monitoring-config" "$(hasnt 'cluster-monitoring-config' "$OUT")"
check "uwm: two ServiceMonitors in the monitoring namespace" "$([ "$(awk -v m="namespace: $MON" 'BEGIN{RS="---\n"} /kind: ServiceMonitor/ && index($0,m) {c++} END{print c+0}' <<<"$OUT")" = 2 ] && echo yes || echo no)"
check "uwm: no ServiceMonitor left in kube-system" "$([ "$(awk 'BEGIN{RS="---\n"} /kind: ServiceMonitor/ && /namespace: kube-system/ {c++} END{print c+0}' <<<"$OUT")" = 0 ] && echo yes || echo no)"
check "uwm: PrometheusRule is in the monitoring namespace" "$(has "namespace: $MON" "$(awk 'BEGIN{RS="---\n"} /kind: PrometheusRule/ {print}' <<<"$OUT")")"
check "uwm: controller-down alert looks in the monitoring namespace" "$(has "namespace=\"$MON\"" "$(awk 'BEGIN{RS="---\n"} /kind: PrometheusRule/ {print}' <<<"$OUT")")"
MSM="$(awk -v m="namespace: $MON" 'BEGIN{RS="---\n"} /kind: ServiceMonitor/ && index($0,m) {print}' <<<"$OUT")"
check "uwm: ServiceMonitors use https" "$(has 'scheme: https' "$MSM")"
check "uwm: ServiceMonitors read the token from a Secret" "$(has "key: token" "$MSM")"
check "uwm: ServiceMonitors take the CA from a ConfigMap" "$(has 'configMap:' "$MSM")"
check "uwm: ServiceMonitors check the kube-system Service names" "$(has "serverName: $FN-node-metrics.kube-system.svc" "$MSM")$(has "serverName: $FN-metrics.kube-system.svc" "$MSM")"
check "uwm: ServiceMonitors do not read files (user workload monitoring refuses that)" "$(hasnt 'bearerTokenFile' "$MSM")$(hasnt 'caFile' "$MSM")"
check "uwm: ServiceMonitors never skip certificate checks" "$(hasnt 'insecureSkipVerify' "$MSM")"
SVCS="$(awk -v m="namespace: $MON" 'BEGIN{RS="---\n"} /kind: Service\n/ && index($0,m) {print}' <<<"$OUT")"
check "uwm: two Services in the monitoring namespace" "$([ "$(grep -c 'kind: Service$' <<<"$SVCS")" = 2 ] && echo yes || echo no)"
check "uwm: those Services have no selector (the controller keeps their Endpoints)" "$(hasnt 'selector:' "$SVCS")"
check "uwm: token Secret for the scraper" "$(has 'type: kubernetes.io/service-account-token' "$OUT")"
check "uwm: CA bundle ConfigMap is injected by the service CA" "$(has 'service.beta.openshift.io/inject-cabundle: "true"' "$(awk -v m="namespace: $MON" 'BEGIN{RS="---\n"} /kind: ConfigMap/ && index($0,m) {print}' <<<"$OUT")")"
BIND="$(awk 'BEGIN{RS="---\n"} /kind: ClusterRoleBinding/ && /metrics-reader/ {print}' <<<"$OUT")"
check "uwm: the scraper account (not prometheus-k8s) may get /metrics" "$(has "namespace: $MON" "$BIND")$(hasnt 'prometheus-k8s' "$BIND")"
check "uwm: controller may keep Endpoints in the monitoring namespace" "$(has 'endpoints' "$(awk -v m="namespace: $MON" 'BEGIN{RS="---\n"} /kind: Role\n/ && index($0,m) {print}' <<<"$OUT")")"
DEP="$(awk 'BEGIN{RS="---\n"} /kind: Deployment/ && /component: controller/ {print}' <<<"$OUT")"
check "uwm: controller gets --monitoring-namespace" "$(has "--monitoring-namespace=$MON" "$DEP")"
check "uwm: controller gets the node agent port" "$(has '--monitoring-node-port=9810' "$DEP")"

# OpenShift refuses Endpoints that point at pod IPs unless the writer may create endpoints/restricted.
check "uwm: controller may write Endpoints with pod addresses (endpoints/restricted)" "$(has 'endpoints/restricted' "$(awk -v m="namespace: $MON" 'BEGIN{RS="---\n"} /kind: Role\n/ && index($0,m) {print}' <<<"$OUT")")"

# Fallbacks keep the in-namespace path.
check "no console API (not OpenShift): ServiceMonitors stay in kube-system" "$(hasnt "$MON" "$(render --api-versions "$SM" --api-versions "$PR" --set webhook.certProvider=openshift)")"
check "cert-manager provider: ServiceMonitors stay in kube-system" "$(hasnt "$MON" "$(render --api-versions "$SM" --api-versions "$PR" --api-versions "$CP")")"
check "monitoring.mode=disabled: ServiceMonitors stay in kube-system" "$(hasnt "$MON" "$(render "${OS[@]}" --set metrics.monitoring.mode=disabled)")"
check "controller has no monitoring flag on the in-namespace path" "$(hasnt '--monitoring-namespace' "$(render --api-versions "$SM")")"
OUT2="$(render "${OS[@]}" --set metrics.monitoring.namespace=my-mon)"
check "the namespace name can be set" "$(has 'name: my-mon' "$OUT2")$(hasnt "$MON" "$OUT2")"
if render --api-versions "$SM" --api-versions "$PR" --set metrics.monitoring.mode=enabled >/dev/null 2>&1; then
    fail "mode=enabled with the cert-manager provider should fail (the CA must come from the service CA)"
else
    pass "mode=enabled with the cert-manager provider fails"
fi

exit "$FAILED"
