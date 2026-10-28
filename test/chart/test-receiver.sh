#!/usr/bin/env bash
# test-receiver.sh — Render the Helm chart and check the replication receiver's user.
#
# The receiver writes the far-site VM disks. KubeVirt runs QEMU as user 107 and
# the disk file has mode 0660, so the receiver must run as 107 or a failed-over
# VM cannot open its disk.
#
# Usage: bash test/chart/test-receiver.sh
# Requires: helm.

set -euo pipefail

CHART="$(cd "$(dirname "$0")/../.." && pwd)/charts/ibm-vpc-file-pool-csi"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

render() {
    helm template test "$CHART" --namespace kube-system \
        --set replicationReceiver.enabled=true \
        --set replicationReceiver.pvcName=data \
        --set replicationReceiver.authSecretName=token "$@"
}

# receiver_user prints "uid:gid" of the receiver pod, or "none".
receiver_user() {
    awk '/^kind: Deployment/{d=1} /^---/{d=0} d' <<<"$1" |
        awk '/app.kubernetes.io\/component: replication-receiver/{r=1} r && /runAsUser:/{u=$2} r && /runAsGroup:/{g=$2} END{if (u=="") print "none"; else print u ":" g}'
}

if [ "$(receiver_user "$(render)")" = "107:107" ]; then
    pass "default: the receiver runs as 107:107"
else
    fail "default: the receiver user is $(receiver_user "$(render)"), want 107:107"
fi

if [ "$(receiver_user "$(render --set replicationReceiver.runAsUser=1234 --set replicationReceiver.runAsGroup=4321)")" = "1234:4321" ]; then
    pass "override: the receiver user can be set"
else
    fail "override: the receiver user was not changed"
fi

if grep -q "kind: Deployment" <<<"$(helm template test "$CHART" --namespace kube-system)" &&
    [ "$(receiver_user "$(helm template test "$CHART" --namespace kube-system)")" = "none" ]; then
    pass "receiver disabled: no receiver pod"
else
    fail "receiver disabled: a receiver pod was rendered"
fi

# The receiver takes data from outside the cluster. It must not run with the
# controller's account, which reads Secrets and execs into pods everywhere
# (issue #89). It never calls the Kubernetes API, so it gets an account of its
# own and no API token.
receiver_pod() { awk '/^kind: Deployment/{d=1; buf=""} d{buf=buf $0 "\n"} /^---/{if (buf ~ /component: replication-receiver/) {printf "%s", buf; exit} d=0; buf=""} END{if (buf ~ /component: replication-receiver/) printf "%s", buf}' <<<"$1"; }

RENDERED="$(render)"
SA=$(receiver_pod "$RENDERED" | awk '/serviceAccountName:/{print $2; exit}')
if [ -n "$SA" ] && [[ "$SA" == *-receiver ]]; then
    pass "the receiver runs as its own account ($SA)"
else
    fail "the receiver runs as '$SA', want an account that ends in -receiver"
fi

if receiver_pod "$RENDERED" | grep -q "automountServiceAccountToken: false"; then
    pass "the receiver pod mounts no API token"
else
    fail "the receiver pod mounts an API token"
fi

if grep -q "^  name: .*-receiver$" <<<"$(awk '/^kind: ServiceAccount/{k=1} /^---/{k=0} k' <<<"$RENDERED")"; then
    pass "the receiver account is rendered"
else
    fail "no ServiceAccount for the receiver is rendered"
fi

if grep -q "name: .*-receiver" <<<"$(awk '/^kind: (ClusterRoleBinding|RoleBinding)/{k=1} /^---/{k=0} k' <<<"$RENDERED")"; then
    fail "the receiver account is bound to a role"
else
    pass "the receiver account has no role binding"
fi

if receiver_pod "$RENDERED" | grep -q "allowPrivilegeEscalation: false"; then
    pass "the receiver cannot gain more privilege"
else
    fail "the receiver may gain privilege (allowPrivilegeEscalation is not false)"
fi

exit "$FAILED"
