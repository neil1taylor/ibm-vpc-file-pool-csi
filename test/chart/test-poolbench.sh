#!/usr/bin/env bash
# test-poolbench.sh — Render the Helm chart and check the PoolBench service:
# off by default; when on, a hardened Deployment, its own service account and
# rights, the rights it binds in each run's namespace, the collector Service and
# the results volume.
#
# Usage: bash test/chart/test-poolbench.sh
# Requires: helm, yq.

set -euo pipefail

CHART="$(cd "$(dirname "$0")/../.." && pwd)/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
check() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi; }

render() {
    helm template test "$CHART" --namespace kube-system "$@"
}
# doc <rendered> <kind> <name> <yq path> prints one value of one object.
doc() {
    yq -N eval "select(.kind == \"$2\" and .metadata.name == \"$3\") | $4" - <<<"$1"
}

if ! command -v yq >/dev/null; then echo "yq is required"; exit 2; fi

OFF=$(render)
if grep -q "ibm-vpc-file-pool-csi-poolbench" <<<"$OFF"; then fail "off by default: nothing of PoolBench is rendered"; else pass "off by default: nothing of PoolBench is rendered"; fi

OUT=$(render --set poolBench.enabled=true)
D=test-ibm-vpc-file-pool-csi-poolbench
APPVER=$(yq eval '.appVersion' "$CHART/Chart.yaml")

check "one replica" "$(doc "$OUT" Deployment "$D" '.spec.replicas')" "1"
check "the results volume is never mounted by two pods" "$(doc "$OUT" Deployment "$D" '.spec.strategy.type')" "Recreate"
check "the driver image" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.containers[0].image')" "de.icr.io/ibm-vpc-file-pool-csi/driver:v${APPVER}"
check "its own service account" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.serviceAccountName')" "$D"
ARGS=$(doc "$OUT" Deployment "$D" '.spec.template.spec.containers[0].args[]')
for want in "--mode=poolbench" \
    "--poolbench-collector-url=http://$D.kube-system.svc.cluster.local:8080" \
    "--poolbench-run-role=$D-run" \
    "--poolbench-service-account=$D" \
    "--node-metrics-service=test-ibm-vpc-file-pool-csi-node-metrics" \
    "--poolbench-results-dir=/results"; do
    if grep -qxF -- "$want" <<<"$ARGS"; then pass "arg $want"; else fail "arg $want missing"; fi
done
check "POD_NAMESPACE is set" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.containers[0].env[] | select(.name == "POD_NAMESPACE") | .valueFrom.fieldRef.fieldPath')" "metadata.namespace"
SC='.spec.template.spec.containers[0].securityContext'
check "no privilege escalation" "$(doc "$OUT" Deployment "$D" "$SC.allowPrivilegeEscalation")" "false"
check "runs as non-root" "$(doc "$OUT" Deployment "$D" "$SC.runAsNonRoot")" "true"
check "drops ALL capabilities" "$(doc "$OUT" Deployment "$D" "$SC.capabilities.drop[0]")" "ALL"
check "read-only root file system" "$(doc "$OUT" Deployment "$D" "$SC.readOnlyRootFilesystem")" "true"
check "seccomp RuntimeDefault" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.securityContext.seccompProfile.type')" "RuntimeDefault"
check "the service may write the results volume" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.securityContext.fsGroup')" "65532"
check "results volume mounted" "$(doc "$OUT" Deployment "$D" '.spec.template.spec.containers[0].volumeMounts[] | select(.name == "results") | .mountPath')" "/results"
check "results claim" "$(doc "$OUT" PersistentVolumeClaim "$D-results" '.spec.resources.requests.storage')" "5Gi"
check "collector Service port" "$(doc "$OUT" Service "$D" '.spec.ports[] | select(.name == "collector") | .port')" "8080"

# Cluster-wide rights: no VMs, no secrets, no pod exec outside the run namespaces.
CR=$(doc "$OUT" ClusterRole "$D" '.rules')
if grep -q "pods/exec" <<<"$CR"; then fail "no pod exec cluster-wide"; else pass "no pod exec cluster-wide"; fi
if grep -q "virtualmachines" <<<"$CR"; then fail "no VM rights cluster-wide"; else pass "no VM rights cluster-wide"; fi
if yq -N eval '.[] | select(.resources[] == "secrets")' - <<<"$CR" | grep -q .; then fail "no secret rights cluster-wide"; else pass "no secret rights cluster-wide"; fi
# The backup event makes and deletes one backup policy, which is cluster-wide; the admission policy limits it.
check "it may make and delete backup policies" \
    "$(doc "$OUT" ClusterRole "$D" '.rules[] | select(.resources[] == "backuppolicies") | .verbs | contains(["get", "create", "delete"])')" "true"
check "it may not change a backup policy" \
    "$(doc "$OUT" ClusterRole "$D" '.rules[] | select(.resources[] == "backuppolicies") | .verbs | contains(["update"])')" "false"
check "it may bind only the run role" "$(doc "$OUT" ClusterRole "$D" '.rules[] | select(.verbs[] == "bind") | .resourceNames[0]')" "$D-run"
check "the cluster role is bound to it" "$(doc "$OUT" ClusterRoleBinding "$D" '.subjects[0].name')" "$D"

# The run role: what it needs inside a run's namespace.
RR=$(doc "$OUT" ClusterRole "$D-run" '.rules[].resources[]')
for want in pods/exec virtualmachines virtualmachineinstances virtualmachineinstancemigrations virtualmachinesnapshots secrets persistentvolumeclaims datavolumes; do
    if grep -qxF -- "$want" <<<"$RR"; then pass "run role has $want"; else fail "run role lacks $want"; fi
done
# The boot storm restarts VMs by deleting their instance; the migration event makes migrations.
check "the run role may delete VM instances" \
    "$(doc "$OUT" ClusterRole "$D-run" '.rules[] | select(.resources[] == "virtualmachineinstances") | .verbs | contains(["delete"])')" "true"
# The backup event labels the disks it backs up.
if doc "$OUT" ClusterRole "$D-run" '.rules[] | select(.resources[] == "persistentvolumeclaims") | .verbs[]' | grep -qx patch; then
    pass "the run role may label disks"; else fail "the run role may not label disks"; fi
# Exec into a VM pod is allowed only for pods that run under the KubeVirt SCC.
check "the run role may use only the KubeVirt SCC" \
    "$(doc "$OUT" ClusterRole "$D-run" '.rules[] | select(.resources[] == "securitycontextconstraints") | .resourceNames[0] + ":" + .verbs[0]')" "kubevirt-controller:use"
if doc "$OUT" ClusterRoleBinding "$D-run" '.metadata.name' | grep -q .; then fail "the run role is never bound cluster-wide"; else pass "the run role is never bound cluster-wide"; fi

# The metrics of the service are scraped, so the console can draw the live graphs.
OS=$(render --set poolBench.enabled=true --set webhook.certProvider=openshift --api-versions monitoring.coreos.com/v1/ServiceMonitor --set metrics.monitoring.mode=disabled)
check "openshift: the service CA makes the metrics certificate" \
    "$(doc "$OS" Service "$D" '.metadata.annotations."service.beta.openshift.io/serving-cert-secret-name"')" "$D-certs"
check "the metrics certificate is mounted" \
    "$(doc "$OS" Deployment "$D" '.spec.template.spec.volumes[] | select(.name == "metrics-certs") | .secret.secretName')" "$D-certs"
check "the metrics endpoint uses it" \
    "$(doc "$OS" Deployment "$D" '.spec.template.spec.containers[0].args[] | select(test("^--poolbench-metrics-cert-dir"))')" \
    "--poolbench-metrics-cert-dir=/etc/poolbench/metrics-certs"
check "a ServiceMonitor scrapes the service" \
    "$(doc "$OS" ServiceMonitor "$D" '.spec.selector.matchLabels."app.kubernetes.io/component"')" "poolbench"
check "it checks the service's certificate name" \
    "$(doc "$OS" ServiceMonitor "$D" '.spec.endpoints[0].tlsConfig.serverName')" "$D.kube-system.svc"
CM=$(render --set poolBench.enabled=true --set webhook.certProvider=cert-manager)
check "cert-manager: a Certificate for the service" "$(doc "$CM" Certificate "$D" '.spec.secretName')" "$D-certs"
UWM=$(render --set poolBench.enabled=true --set webhook.certProvider=openshift --set metrics.monitoring.mode=enabled \
    --api-versions monitoring.coreos.com/v1/ServiceMonitor)
check "user workload monitoring: a Service for the service in the monitoring namespace" \
    "$(yq -N eval "select(.kind == \"Service\" and .metadata.name == \"$D\" and .metadata.namespace != \"kube-system\") | .spec.ports[0].port" - <<<"$UWM")" "8443"
check "user workload monitoring: a ServiceMonitor for it" \
    "$(yq -N eval "select(.kind == \"ServiceMonitor\" and .metadata.name == \"$D\") | .spec.endpoints[0].tlsConfig.serverName" - <<<"$UWM")" "$D.kube-system.svc"
CTRL_ARGS=$(yq -N eval 'select(.kind == "Deployment" and .metadata.name == "test-ibm-vpc-file-pool-csi-controller") | .spec.template.spec.containers[] | select(.name == "csi-controller") | .args[]' - <<<"$UWM")
for want in "--monitoring-poolbench-service=$D" "--monitoring-poolbench-port=8443"; do
    if grep -qxF -- "$want" <<<"$CTRL_ARGS"; then pass "controller keeps its endpoints: $want"; else fail "controller arg $want missing"; fi
done
UWM_OFF=$(render --set webhook.certProvider=openshift --set metrics.monitoring.mode=enabled --api-versions monitoring.coreos.com/v1/ServiceMonitor)
if grep -q -- "--monitoring-poolbench" <<<"$UWM_OFF"; then fail "no PoolBench target while PoolBench is off"; else pass "no PoolBench target while PoolBench is off"; fi

# The console reads the raw results through its plugin proxy, with the person's own token.
CP=$(render --set poolBench.enabled=true --set webhook.certProvider=openshift --api-versions console.openshift.io/v1/ConsolePlugin)
check "the results port on the Service" "$(doc "$CP" Service "$D" '.spec.ports[] | select(.name == "results") | .port')" "8444"
check "the results port on the pod" \
    "$(doc "$CP" Deployment "$D" '.spec.template.spec.containers[0].ports[] | select(.name == "results") | .containerPort')" "8444"
check "the console proxies to it" \
    "$(yq -N eval 'select(.kind == "ConsolePlugin") | .spec.proxy[] | select(.alias == "poolbench") | .endpoint.service.port' - <<<"$CP")" "8444"
check "with the person's own token" \
    "$(yq -N eval 'select(.kind == "ConsolePlugin") | .spec.proxy[] | select(.alias == "poolbench") | .authorization' - <<<"$CP")" "UserToken"
CP_OFF=$(render --api-versions console.openshift.io/v1/ConsolePlugin)
if yq -N eval 'select(.kind == "ConsolePlugin") | .spec.proxy' - <<<"$CP_OFF" | grep -q poolbench; then
    fail "no proxy while PoolBench is off"; else pass "no proxy while PoolBench is off"; fi

# An admission policy holds the service to its own namespaces and pools, whatever its role allows.
VAP=$(render --set poolBench.enabled=true --api-versions admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy)
check "the policy applies to the service account only" \
    "$(doc "$VAP" ValidatingAdmissionPolicy "$D" '.spec.matchConditions[0].expression')" \
    "request.userInfo.username == 'system:serviceaccount:kube-system:$D'"
RULES=$(doc "$VAP" ValidatingAdmissionPolicy "$D" '.spec.matchConstraints.resourceRules[].resources[]')
for want in namespaces rolebindings filesharepools backuppolicies; do
    if grep -qxF "$want" <<<"$RULES"; then pass "the policy covers $want"; else fail "the policy does not cover $want"; fi
done
check "the policy binding denies" "$(doc "$VAP" ValidatingAdmissionPolicyBinding "$D" '.spec.validationActions[0]')" "Deny"
VX=$(doc "$VAP" ValidatingAdmissionPolicy "$D" '.spec.validations[].expression')
if grep -q "object.subjects\[0\].name == '$D'" <<<"$VX"; then pass "a binding may name only the service"; else fail "a binding may name others"; fi
if grep -q "namespaceObject.metadata.labels" <<<"$VX"; then pass "a binding needs a labelled namespace"; else fail "a binding trusts the name only"; fi
if grep -q "poolbench-" <<<"$VX"; then
    pass "only names that start with poolbench-"; else fail "the names are not limited"; fi

BK=$(doc "$VAP" ValidatingAdmissionPolicy "$D" '.spec.validations[] | select(.message | test("backup policy")) | .expression')
if grep -q "object.spec.namespaces.size() == 1" <<<"$BK" && grep -q "startsWith('poolbench-')" <<<"$BK"; then pass "a backup policy covers one PoolBench namespace"; else fail "a backup policy may cover other namespaces"; fi

if grep -q "!has(object.spec.export)" <<<"$BK"; then pass "a backup policy never exports to a bucket"; else fail "a backup policy may export to a bucket"; fi
if grep -q "object.spec.snapshotPool == object.spec.namespaces\[0\] + '-backups'" <<<"$BK"; then pass "a backup policy copies to the backup pool of its own run"; else fail "a backup policy may copy to another pool"; fi

DEL=$(doc "$VAP" ValidatingAdmissionPolicy "$D" '.spec.validations[] | select(.message | test("delete a namespace or pool")) | .expression')
if grep -q "oldObject.metadata.labels" <<<"$DEL"; then pass "a pool is deleted only if a run made it"; else fail "a pool can be deleted without the run label"; fi

exit "$FAILED"
