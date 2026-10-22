#!/usr/bin/env bash
# End-to-end test of browse sessions (issue #92, phase 1 part 2).
#
# Backs up a pod volume to a snapshot pool twice (the content changes between
# the two), then makes BackupBrowse objects by hand, as the gateway will, and
# checks: the helper starts and answers over TLS with the session token and the
# pinned certificate; the latest session reads the changed content and a session
# at the first moment reads the first content; the helper pod has no privileges,
# mounts read-only, and is closed to other pods by the NetworkPolicy; closing a
# session, and deleting one, leaves no Job, pod, claim, volume or Secret.
#
# Usage: KUBECONFIG=... test/e2e/test-backup-browse-session.sh [--keep]
set -euo pipefail

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-bs}
NS=${NS:-$PREFIX-test}
SRC=$PREFIX-src
SNAP=$PREFIX-snap
POLICY=$PREFIX-policy
DRIVER_NS=${DRIVER_NS:-kube-system}
WORK=$(mktemp -d)
PF_PIDS=()

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
pass() { log "PASS: $*"; }
fail() { log "FAIL: $*"; oc get backupbrowse -o wide 2>&1 | tail -5 || true; exit 1; }

cleanup() {
    for p in "${PF_PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null || true; done
    find "$WORK" -delete 2>/dev/null || true
    if $KEEP; then log "keeping test resources (--keep)"; return; fi
    log "cleaning up"
    oc delete backupbrowse -l "$PREFIX-test=yes" --ignore-not-found --wait=false || true
    oc delete backuppolicy "$POLICY" --ignore-not-found --wait=false || true
    oc delete ns "$NS" --ignore-not-found --wait=true --timeout=15m || true
    oc delete filesharepool "$SRC" "$SNAP" --ignore-not-found --wait=false || true
    oc wait --for=delete "filesharepool/$SRC" "filesharepool/$SNAP" --timeout=900s || log "the pools are not gone yet"
}
trap cleanup EXIT

wait_until() { # wait_until <seconds> <description> <command...>
    local t=$1 what=$2; shift 2
    local end=$((SECONDS + t))
    until "$@"; do
        [ $SECONDS -ge $end ] && fail "timed out after ${t}s waiting for $what"
        sleep 5
    done
}

zone() {
    oc get nodes -o jsonpath='{range .items[*]}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' |
        sort | uniq -c | sort -rn | awk 'NR==1{print $2}'
}
make_pool() {
    oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata: {name: $1}
spec:
  zone: $(zone)
  profile: dp2
  shareSizeGB: 100
  iops: 3000
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
YAML
}
pool_ready() { [ "$(oc get filesharepool "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = Ready ]; }
both_ready() { pool_ready "$SRC" && pool_ready "$SNAP"; }

log "making pools $SRC and $SNAP"
make_pool "$SRC"; make_pool "$SNAP"
wait_until 1200 "pools Ready" both_ready

oc create ns "$NS"
oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data, labels: {backup: "yes"}}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $SRC
  resources: {requests: {storage: 2Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: writer}
spec:
  terminationGracePeriodSeconds: 1
  containers:
  - name: w
    image: quay.io/centos/centos:stream9
    command: ["sleep","infinity"]
    volumeMounts: [{name: d, mountPath: /data}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: data}}]
YAML
oc wait pod/writer -n "$NS" --for=condition=Ready --timeout=600s >/dev/null
oc exec -n "$NS" writer -- sh -c 'mkdir -p /data/docs && echo first > /data/v.txt && echo one > /data/docs/a.txt'

oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: BackupPolicy
metadata: {name: $POLICY}
spec:
  snapshotPool: $SNAP
  schedule: 1m
  keepFor: 1h
  namespaces: [$NS]
  selector: {matchLabels: {backup: "yes"}}
YAML
vol() { oc get backuppolicy "$POLICY" -o json | jq -r ".status.volumes[]? | select(.pvcName == \"data\") | $1"; }
backed_up() { [ -n "$(vol '.lastBackupTime // empty')" ]; }
wait_until 1200 "the first backup" backed_up
FIRST=$(vol '.lastBackupTime')
log "first backup at $FIRST"
oc exec -n "$NS" writer -- sh -c 'echo second > /data/v.txt && echo two > /data/docs/b.txt'
two_moments() { [ "$(vol '.restorePoints // 0')" -ge 1 ] && [ "$(vol '.lastBackupTime')" != "$FIRST" ]; }
wait_until 1500 "a second backup" two_moments
pass "two backup moments exist (first $FIRST)"

open() { # open <name> [moment]
    local moment=""
    [ -n "${2:-}" ] && moment=", moment: $2"
    oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: BackupBrowse
metadata: {name: $1, labels: {$PREFIX-test: "yes"}}
spec: {policy: $POLICY, pvcNamespace: $NS, pvcName: data, requestedBy: lab-test$moment}
YAML
}
phase() { oc get backupbrowse "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
is_ready() { [ "$(phase "$1")" = Ready ]; }

open "$PREFIX-latest"
open "$PREFIX-first" "$FIRST"
wait_until 300 "the latest session" is_ready "$PREFIX-latest"
wait_until 300 "the first-moment session" is_ready "$PREFIX-first"
pass "both sessions are Ready"

uid_of() { oc get backupbrowse "$1" -o jsonpath='{.metadata.uid}'; }
prepare() { # prepare <session> <port>: fetch the token and the certificate, forward the port
    local uid sec pod
    uid=$(uid_of "$1"); sec="browse-$uid"
    oc get secret "$sec" -n "$DRIVER_NS" -o jsonpath='{.data.token}' | base64 -d > "$WORK/$1.token"
    oc get secret "$sec" -n "$DRIVER_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$WORK/$1.crt"
    pod=$(oc get pod -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$uid" -o jsonpath='{.items[0].metadata.name}')
    oc port-forward -n "$DRIVER_NS" "pod/$pod" "$2:8443" >/dev/null 2>&1 &
    PF_PIDS+=($!)
    sleep 3
}
# talk <session> <port> <endpoint> <json>: over TLS with the pinned certificate and the token.
talk() {
    curl -s --cacert "$WORK/$1.crt" --resolve "browse-session:$2:127.0.0.1" -H "Authorization: Bearer $(cat "$WORK/$1.token")" \
        -X POST "https://browse-session:$2$3" -d "$4"
}
b64() { printf '%s' "$1" | base64; }
prepare "$PREFIX-latest" 18443
prepare "$PREFIX-first" 18444
read_file() { # read_file <session> <port> <name...>
    local s=$1 p=$2; shift 2
    talk "$s" "$p" /v1/file "{\"path\":[$(for n in "$@"; do printf '"%s",' "$(b64 "$n")"; done | sed 's/,$//')],\"length\":-1}"
}
[ "$(read_file "$PREFIX-latest" 18443 v.txt)" = second ] && pass "the latest session reads the changed file" || fail "latest v.txt is not 'second'"
[ "$(read_file "$PREFIX-first" 18444 v.txt)" = first ] && pass "the first-moment session reads the first content" || fail "first-moment v.txt is not 'first'"
[ "$(read_file "$PREFIX-first" 18444 docs b.txt | jq -r .code 2>/dev/null)" = not_found ] && pass "a file made after the moment is not in it" || fail "b.txt is in the first moment"
[ "$(read_file "$PREFIX-latest" 18443 docs b.txt)" = two ] && pass "the latest session reads a new file" || fail "latest docs/b.txt missing"
talk "$PREFIX-latest" 18443 /v1/info '{}' | jq -e '.latest == true' >/dev/null && pass "info says latest" || fail "info of the latest session"
talk "$PREFIX-first" 18444 /v1/info '{}' | jq -e '.latest == false and (.moment | length) > 0' >/dev/null && pass "info names the older moment" || fail "info of the first-moment session"
curl -s --cacert "$WORK/$PREFIX-latest.crt" --resolve "browse-session:18443:127.0.0.1" -H "Authorization: Bearer wrong-wrong-wrong-wrong-wrong-12345" \
    -X POST https://browse-session:18443/v1/list -d '{"path":[]}' | jq -e '.code == "unauthorized"' >/dev/null && pass "a wrong token is refused" || fail "a wrong token was accepted"
if curl -s --cacert "$WORK/$PREFIX-first.crt" --resolve "browse-session:18443:127.0.0.1" -X POST https://browse-session:18443/v1/list -d '{}' >/dev/null 2>&1; then
    fail "a different session certificate was accepted"
else
    pass "the certificate of another session is not trusted"
fi

UID1=$(uid_of "$PREFIX-latest")
POD=$(oc get pod -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$UID1" -o json)
jq -e '.items[0].spec | .automountServiceAccountToken == false and (.hostNetwork // false) == false and ([.volumes[] | select(.hostPath)] | length) == 0' <<<"$POD" >/dev/null \
    && pass "the helper pod has no service account token and no host access" || fail "the helper pod is not locked down"
jq -e '.items[0].spec.containers[0].securityContext | .allowPrivilegeEscalation == false and .readOnlyRootFilesystem == true and .capabilities.drop == ["ALL"]' <<<"$POD" >/dev/null \
    && pass "the helper container drops every capability and has a read-only root file system" || fail "container security context"
jq -e '[.items[0].spec.containers[0].volumeMounts[] | select(.mountPath == "/latest" or .mountPath == "/moments") | .readOnly] | all' <<<"$POD" >/dev/null \
    && pass "the backup folders are mounted read-only" || fail "a backup folder is writable"
oc get secret "browse-$UID1" -n "$DRIVER_NS" -o json | jq -e --arg u "$UID1" '.metadata.ownerReferences[0].uid == $u' >/dev/null \
    && pass "the session Secret is owned by the session" || fail "the Secret has no owner"

POD_IP=$(oc get backupbrowse "$PREFIX-latest" -o jsonpath='{.status.podIP}')
if oc run probe -n "$NS" --rm -i --restart=Never --image=quay.io/centos/centos:stream9 --command -- \
    sh -c "curl -sk -m 8 -o /dev/null https://$POD_IP:8443/healthz" >/dev/null 2>&1; then
    fail "another pod reached the helper"
else
    pass "another pod cannot reach the helper (NetworkPolicy)"
fi

gone() { # gone <uid>: no pod, Job, claim, volume or Secret of the session remains
    [ -z "$(oc get pod,job,pvc,secret -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$1" -o name 2>/dev/null)" ] &&
        [ -z "$(oc get pv -l "storage.ibmcloud.io/browse=$1" -o name 2>/dev/null)" ]
}
oc patch backupbrowse "$PREFIX-latest" --type merge -p '{"spec":{"close":true}}' >/dev/null
closed() { [ "$(phase "$1")" = Closed ]; }
wait_until 180 "the session to close" closed "$PREFIX-latest"
[ "$(oc get backupbrowse "$PREFIX-latest" -o jsonpath='{.status.endReason}')" = closed ] && pass "a closed session says why" || fail "end reason"
wait_until 180 "the closed session's objects to go" gone "$UID1"
pass "closing a session removes its pod, Job, claim, volume and Secret"

UID2=$(uid_of "$PREFIX-first")
oc delete backupbrowse "$PREFIX-first" --wait=true >/dev/null
wait_until 180 "the deleted session's objects to go" gone "$UID2"
pass "deleting a session removes its pod, Job, claim, volume and Secret"
log "all checks passed"
