#!/usr/bin/env bash
# End-to-end test of browsing and restoring files from a backup in COS
# (issue #92, phase 4). Part 6 of release test stage 20.
#
# Needs the same COS configuration as the COS part of stage 18 (see
# test/e2e/lib/backup-cos.sh): a bucket made by `kubectl backup cos-setup`, the
# HMAC key, the Key Protect API key and the wrapped backup key. Without COS_BUCKET
# it prints "NOT CHECKED" and ends well. Secret values go from file to file to
# Secret and are never in a variable, on a command line or in the log.
#
# What it does: backs up a pod volume (files of three owners, a link, a name with
# a newline, a 0600 file, a checksummed file) to a snapshot pool twice, with the
# policy's export block on, and waits until both moments are in the bucket. Then:
# - through the gateway, as real users, `kubectl backup ls/get/restore-files
#   --from-cos` read the bucket of the policy's export block: the latest copy and
#   the first moment, a link as a link, a file of another owner, the checksum;
#   restore into a new claim, at the first moment, and into a new folder of an
#   existing claim;
# - the helper pod that reads COS has no service account token, runs as an
#   ordinary user, mounts no share, and the COS and Key Protect Secrets are not in
#   the browse namespace; another pod cannot reach it;
# - a user with only "view", an admin of another namespace and a user whose
#   binding was removed are forbidden; naming a bucket directly is forbidden to a
#   namespace admin and works for someone who may read the Secrets;
# - a wrong root key, a missing Secret and a wrong COS key are said in words;
# - no key reaches a log, an Event or a status; closing or deleting a session
#   leaves no pod, Job or Secret in either namespace.
#
# It cannot test a moment that the local keep time has removed (that takes longer
# than the stage); the gateway's choice of COS for such a moment is covered by unit
# tests, and --from-cos asks for COS by hand.
#
# Usage: KUBECONFIG=... [BACKUP_BIN=bin/kubectl-backup] COS_BUCKET=... test/e2e/test-backup-files-cos.sh [--keep]
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/backup-cos.sh
source "$HERE/lib/backup-cos.sh"

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-bc}
NS=${NS:-$PREFIX-test}
OTHER=$NS-other
SRC=$PREFIX-src
SNAP=$PREFIX-snap
PLAIN=$PREFIX-plain
POLICY=$PREFIX-policy
DRIVER_NS=${DRIVER_NS:-kube-system}
BROWSE_NS=${BROWSE_NS:-ibm-vpc-file-pool-csi-browse}
IMG=${IMG:-quay.io/centos/centos:stream9}
WORK=$(mktemp -d)
chmod 700 "$WORK"

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
pass() { log "PASS: $*"; }
fail() { log "FAIL: $*" >&2; oc get backupbrowse,backupfilerestore -o wide 2>&1 | tail -10 >&2 || true; exit 1; }

cos_load_settings "$(cos_config_dir)/cos-stage.env"
if ! cos_wanted; then
    log "NOT CHECKED: no COS bucket is configured (COS_BUCKET), so browsing a backup in COS was not tried"
    log "all checks passed"
    exit 0
fi
if missing=$(cos_missing_config); then :; else
    echo "$missing"
    fail "COS is wanted (COS_BUCKET is set) but its configuration is incomplete; nothing was started"
fi
COS_PREFIX=$(cos_prefix "${COS_PREFIX_BASE:-stage20}" "$PREFIX" "$(date -u +%Y%m%d%H%M%S)") || fail "cannot make a COS prefix"
CRED_SECRET=$PREFIX-cos-credentials
KEY_SECRET=$PREFIX-cos-backup-key
KP_SECRET=$PREFIX-kp-apikey
BAD_SECRET=$PREFIX-cos-bad
log "COS prefix: $COS_PREFIX (bucket $COS_BUCKET, keep $COS_KEEP_FOR, Compliance lock)"
cos_locked_note "$COS_PREFIX"

BACKUP_BIN=${BACKUP_BIN:-kubectl-backup}
command -v "$BACKUP_BIN" >/dev/null 2>&1 || [ -x "$BACKUP_BIN" ] || fail "no kubectl-backup binary (BACKUP_BIN)"

cleanup() {
    find "$WORK" -delete 2>/dev/null || true
    if $KEEP; then log "keeping test resources (--keep)"; return; fi
    log "cleaning up"
    while read -r name; do
        [ -n "$name" ] && oc delete backupbrowse "$name" --ignore-not-found --wait=false
    done < <(oc get backupbrowse -o json | jq -r --arg p "$POLICY" '.items[] | select(.spec.policy == $p) | .metadata.name') || true
    oc delete backupfilerestore -l "$PREFIX-test=yes" --ignore-not-found --wait=false || true
    oc delete backuppolicy "$POLICY" --ignore-not-found --wait=false || true
    oc delete rolebinding "$PREFIX-cosadmin" -n "$DRIVER_NS" --ignore-not-found || true
    oc delete ns "$NS" "$OTHER" --ignore-not-found --wait=true --timeout=15m || true
    oc delete filesharepool "$SRC" "$SNAP" "$PLAIN" --ignore-not-found --wait=false || true
    oc delete secret "$BAD_SECRET" -n "$DRIVER_NS" --ignore-not-found || true
    cos_delete_secrets "$DRIVER_NS" "$PREFIX"
    oc wait --for=delete "filesharepool/$SRC" "filesharepool/$SNAP" "filesharepool/$PLAIN" --timeout=900s || log "the pools are not gone yet"
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
make_pool() { # make_pool <name> <owner lines>
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
$2
YAML
}
pool_ready() { [ "$(oc get filesharepool "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = Ready ]; }
all_ready() { pool_ready "$SRC" && pool_ready "$SNAP" && pool_ready "$PLAIN"; }

cos_make_secrets "$DRIVER_NS" "$PREFIX" || fail "cannot make the COS Secrets"
log "making pools $SRC (owner 107), $SNAP and $PLAIN (no configured owner)"
make_pool "$SRC" '  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"'
make_pool "$SNAP" ''
make_pool "$PLAIN" ''
wait_until 1500 "pools Ready" all_ready

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
    image: $IMG
    command: ["sleep","infinity"]
    volumeMounts: [{name: d, mountPath: /data}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: data}}]
YAML
oc wait pod/writer -n "$NS" --for=condition=Ready --timeout=600s >/dev/null
oc exec -n "$NS" writer -- sh -c '
set -e
cd /data
mkdir -p docs big
echo first > v.txt
echo one > docs/a.txt
head -c 8388608 /dev/urandom > big/blob
mkdir -p u1000/private0700
echo secret > u1000/secret0600; chmod 600 u1000/secret0600
echo inside > u1000/private0700/inner0600; chmod 600 u1000/private0700/inner0600; chmod 700 u1000/private0700
ln -sfn /etc u1000/to-etc
nl=$(printf "\n"); printf x > "u1000/line${nl}break"
chown -R 1000:1000 u1000
'
BLOB_SHA=$(oc exec -n "$NS" writer -- sha256sum /data/big/blob | awk '{print $1}')
[ ${#BLOB_SHA} -eq 64 ] || fail "no checksum of the big file"

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
$(cos_export_yaml "$COS_PREFIX" "$CRED_SECRET" "$KEY_SECRET" "$KP_SECRET")
YAML
vol() { oc get backuppolicy "$POLICY" -o json | jq -r ".status.volumes[]? | select(.pvcName == \"data\") | $1"; }
backed_up() { [ -n "$(vol '.lastBackupTime // empty')" ]; }
wait_until 1500 "the first backup" backed_up
FIRST=$(vol '.lastBackupTime')
oc exec -n "$NS" writer -- sh -c 'echo second > /data/v.txt && echo two > /data/docs/b.txt'
two_moments() { [ "$(vol '.restorePoints // 0')" -ge 1 ] && [ "$(vol '.lastBackupTime')" != "$FIRST" ]; }
wait_until 1500 "a second backup" two_moments
exported() { [ -n "$(vol '.exportedTime // empty')" ] && [ "$(vol '.exportedTime')" = "$(vol '.lastBackupTime')" ] && [ -z "$(vol '.exportError // empty')" ]; }
wait_until 2400 "the newest moment to be exported to COS" exported
VOLNAME=$(vol '.subPath' | sed 's#.*/##')
pass "two moments are in the snapshot pool and the newest is in COS (first $FIRST, volume $VOLNAME)"

# ---- users ------------------------------------------------------------------------------------
GW_DEPLOY=$(oc get deploy -n "$DRIVER_NS" -l app.kubernetes.io/component=backup-gateway -o name 2>/dev/null | head -1)
if [ -z "$GW_DEPLOY" ]; then
    log "NOT CHECKED: the backup gateway is not installed (backupGateway.enabled)"
    log "all checks passed"
    exit 0
fi
oc rollout status "$GW_DEPLOY" -n "$DRIVER_NS" --timeout=300s >/dev/null || fail "the gateway is not ready"

oc create ns "$OTHER" >/dev/null
for sa in alice viewer cosadmin; do oc create sa "$sa" -n "$NS" >/dev/null; done
oc create sa mallory -n "$OTHER" >/dev/null
oc create rolebinding alice-admin -n "$NS" --clusterrole=admin --serviceaccount="$NS:alice" >/dev/null
oc create rolebinding viewer-view -n "$NS" --clusterrole=view --serviceaccount="$NS:viewer" >/dev/null
oc create rolebinding mallory-admin -n "$OTHER" --clusterrole=admin --serviceaccount="$OTHER:mallory" >/dev/null
# cosadmin may read the Secrets in the driver namespace, which is what naming a bucket needs.
oc create rolebinding "$PREFIX-cosadmin" -n "$DRIVER_NS" --clusterrole=admin --serviceaccount="$NS:cosadmin" >/dev/null
oc create rolebinding cosadmin-admin -n "$NS" --clusterrole=admin --serviceaccount="$NS:cosadmin" >/dev/null

SERVER=$(oc whoami --show-server)
user_kubeconfig() { # user_kubeconfig <name> <namespace>
    local kc="$WORK/$1.kubeconfig"
    oc create token "$1" -n "$2" --duration=2h > "$WORK/$1.sa-token"
    kubectl --kubeconfig "$kc" config set-cluster c --server="$SERVER" --insecure-skip-tls-verify=true >/dev/null
    kubectl --kubeconfig "$kc" config set-credentials u --token="$(cat "$WORK/$1.sa-token")" >/dev/null
    kubectl --kubeconfig "$kc" config set-context x --cluster=c --user=u >/dev/null
    kubectl --kubeconfig "$kc" config use-context x >/dev/null
    echo "$kc"
}
ALICE=$(user_kubeconfig alice "$NS"); VIEWER=$(user_kubeconfig viewer "$NS")
MALLORY=$(user_kubeconfig mallory "$OTHER"); COSADMIN=$(user_kubeconfig cosadmin "$NS")

GW_ROUTE=$(oc get route -n "$DRIVER_NS" -l app.kubernetes.io/component=backup-gateway -o jsonpath='{.items[0].spec.host}' 2>/dev/null || true)
[ -n "$GW_ROUTE" ] || fail "the gateway has no Route"
GW_CA=()
[ -n "${GATEWAY_CA_FILE:-}" ] && GW_CA=(--gateway-ca-file "$GATEWAY_CA_FILE")
bk() { # bk <kubeconfig> <command> <args...>
    local kc=$1; shift
    "$BACKUP_BIN" "$1" --kubeconfig "$kc" --gateway "https://$GW_ROUTE" "${GW_CA[@]}" "${@:2}"
}
FROM_POLICY=(--from-cos --policy "$POLICY" --pvc "$NS/data")
gone() { # gone <uid>: no pod, Job or Secret of the session in either namespace
    [ -z "$(oc get pod,job,secret -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$1" -o name 2>/dev/null)" ] &&
        [ -z "$(oc get pod,job,secret -n "$BROWSE_NS" -l "storage.ibmcloud.io/browse=$1" -o name 2>/dev/null)" ]
}
# Sessions on a bucket named directly all count against the namespace "-", which allows 2 open sessions
# (backupBrowse.maxSessionsPerNamespace), and the cluster allows 5. So every section closes its sessions.
none_open() {
    [ "$(oc get backupbrowse -o json | jq --arg p "$POLICY" '[.items[] | select(.spec.policy == $p and ((.status.phase // "") | IN("Pending","Starting","Ready")))] | length')" = 0 ]
}
close_all() {
    local name
    while read -r name; do
        [ -n "$name" ] && oc patch backupbrowse "$name" --type merge -p '{"spec":{"close":true}}' >/dev/null
    done < <(oc get backupbrowse -o json | jq -r --arg p "$POLICY" '.items[] | select(.spec.policy == $p and ((.status.phase // "") | IN("Pending","Starting","Ready"))) | .metadata.name')
    wait_until 300 "the sessions of this test to close" none_open
}
wait_until 300 "the gateway to answer for COS" bk "$ALICE" ls "${FROM_POLICY[@]}" --timeout 5m /

# ---- 1. read the bucket of the policy's export block --------------------------------------------
OUT=$(bk "$ALICE" ls "${FROM_POLICY[@]}" /)
grep -q "v.txt" <<<"$OUT" && grep -q "docs/" <<<"$OUT" && grep -q "u1000/" <<<"$OUT" \
    && pass "a namespace admin lists the backup in COS with no setup" || fail "ls --from-cos: $OUT"
mkdir "$WORK/got"
bk "$ALICE" get "${FROM_POLICY[@]}" /v.txt --out "$WORK/got" >/dev/null
[ "$(cat "$WORK/got/v.txt")" = second ] && pass "the latest copy is read from COS" || fail "v.txt from COS is not 'second'"
mkdir "$WORK/got1"
bk "$ALICE" get "${FROM_POLICY[@]}" --moment "$FIRST" /v.txt --out "$WORK/got1" >/dev/null
[ "$(cat "$WORK/got1/v.txt")" = first ] && pass "the first moment is read from COS" || fail "v.txt at the first moment from COS is not 'first'"
if bk "$ALICE" get "${FROM_POLICY[@]}" --moment "$FIRST" /docs/b.txt --out "$WORK/got1" >/dev/null 2>&1; then
    fail "a file made after the first moment is in it"
else
    pass "a file made after the first moment is not in it"
fi
bk "$ALICE" get "${FROM_POLICY[@]}" /big/blob --out "$WORK/got" >/dev/null
[ "$(sha256sum "$WORK/got/blob" | awk '{print $1}')" = "$BLOB_SHA" ] && pass "a file from COS has the same checksum" || fail "blob checksum differs"
OUT=$(bk "$ALICE" ls "${FROM_POLICY[@]}" /u1000)
grep -q "to-etc -> /etc" <<<"$OUT" && pass "a link is listed as a link" || fail "link listing: $OUT"
grep -qF 'line\x0abreak' <<<"$OUT" && pass "a name with a newline is listed escaped" || fail "newline name: $OUT"
if out=$(bk "$ALICE" get "${FROM_POLICY[@]}" /u1000/to-etc/passwd --out "$WORK/got" 2>&1); then
    fail "a path through a link to /etc was followed: $out"
else
    pass "a path through a link is refused"
fi
bk "$ALICE" get "${FROM_POLICY[@]}" /u1000/secret0600 --out - | grep -q secret \
    && pass "a file with mode 0600 of another owner is read" || fail "u1000/secret0600"
bk "$ALICE" ls "${FROM_POLICY[@]}" --moments | grep -q "^20" && pass "the moments in COS can be listed" || fail "no moments listed from COS"

# ---- 2. the helper pod ------------------------------------------------------------------------
BB=$(oc get backupbrowse -o json | jq -r '[.items[] | select(.spec.source == "cos")][0].metadata.name')
[ -n "$BB" ] && [ "$BB" != null ] || fail "no COS session was made"
BUID=$(oc get backupbrowse "$BB" -o jsonpath='{.metadata.uid}')
oc get backupbrowse "$BB" -o json | jq -e '.spec.source == "cos" and (.spec.fromCOS // null) == null' >/dev/null \
    && pass "the session reads the policy's bucket and names none" || fail "session spec"
POD=$(oc get pod -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$BUID" -o json)
jq -e '.items[0].spec | .automountServiceAccountToken == false and (.hostNetwork // false) == false and ([.volumes[] | select(.hostPath or .persistentVolumeClaim)] | length) == 0' <<<"$POD" >/dev/null \
    && pass "the COS helper has no service account token, no host path and mounts no share" || fail "the COS helper pod is not locked down"
jq -e '.items[0].spec | (.securityContext.runAsUser // 0) != 0 and .containers[0].securityContext.readOnlyRootFilesystem == true and .containers[0].securityContext.allowPrivilegeEscalation == false and .containers[0].securityContext.capabilities.drop == ["ALL"]' <<<"$POD" >/dev/null \
    && pass "the COS helper runs as an ordinary user with every capability dropped" || fail "COS helper security context"
jq -e '[.items[0].spec.containers[0].volumeMounts[] | select(.mountPath | startswith("/var/run/pool-csi")) | .readOnly] | all' <<<"$POD" >/dev/null \
    && pass "the keys are mounted read-only" || fail "a key mount is writable"
if oc get secret -n "$BROWSE_NS" -o name | grep -qv '^secret/browse-'; then
    fail "the browse namespace holds a Secret that is not a session Secret: $(oc get secret -n "$BROWSE_NS" -o name | tr '\n' ' ')"
else
    pass "the browse namespace holds session Secrets only, never the COS or Key Protect keys"
fi
oc get secret "browse-$BUID" -n "$DRIVER_NS" -o json | jq -e --arg u "$BUID" '.metadata.ownerReferences[0].uid == $u' >/dev/null \
    && pass "the helper's copy of the session Secret is owned by the session" || fail "the copy has no owner"
oc logs -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$BUID" --tail=-1 >"$WORK/helper-$BUID.log" 2>&1 || true
POD_IP=$(oc get backupbrowse "$BB" -o jsonpath='{.status.podIP}')
if oc run probe -n "$NS" --rm -i --restart=Never --image="$IMG" --command -- \
    sh -c "curl -sk -m 8 -o /dev/null https://$POD_IP:8443/healthz" >/dev/null 2>&1; then
    fail "another pod reached the COS helper"
else
    pass "another pod cannot reach the COS helper (NetworkPolicy)"
fi

# ---- 3. restore files from COS ----------------------------------------------------------------
frphase() { oc get backupfilerestore "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
inspect() { # inspect <claim> <script>
    local pod="$PREFIX-insp-$RANDOM$RANDOM" script
    script=$(printf '%s' "cd /claim && { $2 ; }" | base64 | tr -d '\n')
    oc apply -n "$NS" -f - <<YAML >/dev/null
apiVersion: v1
kind: Pod
metadata: {name: $pod}
spec:
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: i
    image: $IMG
    command: ["sh","-c"]
    args: ["echo $script | base64 -d | sh"]
    volumeMounts: [{name: c, mountPath: /claim}]
  volumes: [{name: c, persistentVolumeClaim: {claimName: $1}}]
YAML
    local end=$((SECONDS + 600))
    until [ "$(oc get pod "$pod" -n "$NS" -o jsonpath='{.status.phase}')" = Succeeded ]; do
        [ "$(oc get pod "$pod" -n "$NS" -o jsonpath='{.status.phase}')" = Failed ] && { oc logs "$pod" -n "$NS" >&2 || true; fail "inspection of $1 failed"; }
        [ $SECONDS -ge $end ] && fail "inspection of $1 did not finish"
        sleep 3
    done
    oc logs "$pod" -n "$NS"
    oc delete pod "$pod" -n "$NS" --wait=false >/dev/null
}

bk "$ALICE" restore-files "${FROM_POLICY[@]}" --to "$NS/$PREFIX-new" --path /docs --path /v.txt --path /big --path /u1000 --wait --timeout 40m >/dev/null \
    || fail "restore-files --from-cos into a new claim failed"
NEWFR=$(oc get backupfilerestore -o json | jq -r --arg n "$PREFIX-new" '.items[] | select(.spec.targetName == $n) | .metadata.name')
[ "$(oc get backupfilerestore "$NEWFR" -o jsonpath='{.spec.source}')" = cos ] && pass "the restore read COS" || fail "the restore did not read COS"
OUT=$(inspect "$PREFIX-new" 'nl=$(printf "\n")
echo "docs=$(cat docs/a.txt) v=$(cat v.txt)"
echo "sha=$(sha256sum big/blob | cut -d" " -f1)"
echo "secret=$(stat -c "%u:%g %a" u1000/secret0600)"
echo "private=$(stat -c "%u:%g %a" u1000/private0700) inner=$(stat -c "%u:%g %a" u1000/private0700/inner0600)"
echo "linebreak=$([ -f "u1000/line${nl}break" ] && echo yes || echo no)"
echo "link=$([ -L u1000/to-etc ] && readlink u1000/to-etc || echo notalink)"')
echo "$OUT"
grep -q "^docs=one v=second$" <<<"$OUT" && pass "a restore from COS into a new claim holds the latest content" || fail "content: $OUT"
grep -q "^sha=$BLOB_SHA$" <<<"$OUT" && pass "the restored file has the same checksum" || fail "checksum: $OUT"
grep -q "^secret=107:107 600$" <<<"$OUT" && grep -q "^private=107:107 700 inner=107:107 600$" <<<"$OUT" \
    && pass "files with mode 0600 and a folder with mode 0700 get the pool's owner and keep their mode" || fail "owners: $OUT"
grep -q "^linebreak=yes$" <<<"$OUT" && grep -q "^link=/etc$" <<<"$OUT" \
    && pass "a name with a newline is restored, and a link is restored as a link" || fail "names and links: $OUT"

bk "$ALICE" restore-files "${FROM_POLICY[@]}" --moment "$FIRST" --to "$NS/$PREFIX-first" --path /v.txt --path /docs --size-gb 3 --wait --timeout 40m >/dev/null \
    || fail "restore-files --from-cos at the first moment failed"
OUT=$(inspect "$PREFIX-first" 'echo "v=$(cat v.txt)"; echo "b=$([ -e docs/b.txt ] && echo yes || echo no)"')
grep -q "^v=first$" <<<"$OUT" && grep -q "^b=no$" <<<"$OUT" && pass "a restore from COS at the first moment brings back the first content" || fail "first moment: $OUT"

oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: work}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $PLAIN
  resources: {requests: {storage: 2Gi}}
YAML
OUT=$(inspect work 'echo mine > mine.txt; echo ok')
grep -q "^ok$" <<<"$OUT" || fail "could not write into the existing claim"
bk "$ALICE" restore-files "${FROM_POLICY[@]}" --existing --to "$NS/work" --path /u1000 --path /docs --wait --timeout 40m >/dev/null \
    || fail "restore-files --from-cos into an existing claim failed"
FOLDER=$(oc get backupfilerestore -o json | jq -r '.items[] | select(.spec.targetName == "work") | .status.folder')
OUT=$(inspect work "echo \"mine=\$(cat mine.txt)\"; cd '$FOLDER'; echo \"s=\$(stat -c '%u:%g %a' u1000/secret0600)\"; echo \"link=\$(readlink u1000/to-etc)\"")
grep -q "^mine=mine$" <<<"$OUT" && grep -q "^s=1000:1000 600$" <<<"$OUT" && grep -q "^link=/etc$" <<<"$OUT" \
    && pass "a restore from COS into an existing claim writes a new folder, keeps the backup's owners and leaves the claim's file" || fail "existing claim: $OUT"

close_all

# ---- 4. who may read -----------------------------------------------------------------------------
expect_forbidden() { # expect_forbidden <description> <kubeconfig>
    local out
    if out=$(bk "$2" ls "${FROM_POLICY[@]}" / 2>&1); then fail "$1: the call worked: $out"; fi
    grep -qi "may not read" <<<"$out" && pass "$1: forbidden" || fail "$1: unexpected error: $out"
}
expect_forbidden "a user with only the view role" "$VIEWER"
expect_forbidden "an admin of another namespace" "$MALLORY"
oc delete rolebinding alice-admin -n "$NS" >/dev/null
expect_forbidden "a user whose binding was removed" "$ALICE"

# ---- 5. naming a bucket directly --------------------------------------------------------------------
# The prefix is the one set in the policy's export block; the policy's own area is below it.
BUCKET_FLAGS=(--from-cos --policy "$POLICY" --bucket "$COS_BUCKET" --endpoint "$COS_ENDPOINT" --prefix "$COS_PREFIX" --volume "$VOLNAME"
    --credentials-secret "$CRED_SECRET" --backup-key-secret "$KEY_SECRET" --kp-api-key-secret "$KP_SECRET"
    --kp-instance "$KP_INSTANCE_ID" --kp-region "$KP_REGION" --kp-root-key "$KP_ROOT_KEY_ID")
oc create rolebinding alice-admin-again -n "$NS" --clusterrole=admin --serviceaccount="$NS:alice" >/dev/null
if out=$(bk "$ALICE" ls "${BUCKET_FLAGS[@]}" / 2>&1); then
    fail "a namespace admin named a bucket directly: $out"
fi
grep -q "Secrets" <<<"$out" && pass "naming a bucket directly is forbidden to a namespace admin who cannot read its Secrets" || fail "alice naming a bucket: $out"
OUT=$(bk "$COSADMIN" ls "${BUCKET_FLAGS[@]}" /) || fail "cosadmin could not name the bucket: $OUT"
grep -q "v.txt" <<<"$OUT" && pass "someone who may read the Secrets reads a bucket named directly, with no policy lookup" || fail "named bucket ls: $OUT"
mkdir "$WORK/named"
bk "$COSADMIN" get "${BUCKET_FLAGS[@]}" /big/blob --out "$WORK/named" >/dev/null
[ "$(sha256sum "$WORK/named/blob" | awk '{print $1}')" = "$BLOB_SHA" ] && pass "a file from a bucket named directly has the same checksum" || fail "named bucket checksum"
bk "$COSADMIN" get "${BUCKET_FLAGS[@]}" --moment "$FIRST" /v.txt --out - | grep -q first && pass "a bucket named directly serves the first moment" || fail "named bucket first moment"
bk "$COSADMIN" restore-files "${BUCKET_FLAGS[@]}" --to "$NS/$PREFIX-named" --storage-class "$SRC" --path /docs --wait --timeout 40m >/dev/null \
    || fail "restore-files from a bucket named directly failed"
OUT=$(inspect "$PREFIX-named" 'echo "docs=$(cat docs/a.txt)"')
grep -q "^docs=one$" <<<"$OUT" && pass "files restored from a bucket named directly are in the new claim" || fail "named restore: $OUT"

close_all

# ---- 6. failures are said in words ------------------------------------------------------------------
expect_words() { # expect_words <description> <pattern> <args...>
    local what=$1 pat=$2 out; shift 2
    if out=$(bk "$COSADMIN" ls "$@" --timeout 6m / 2>&1); then fail "$what: the call worked: $out"; fi
    grep -qi -- "$pat" <<<"$out" && pass "$what: $(grep -oi -- "$pat" <<<"$out" | head -1)" || fail "$what: the message was: $out"
    if grep -qE "AKIA|panic|goroutine|\.go:[0-9]+" <<<"$out"; then fail "$what: the message holds an internal detail: $out"; fi
}
expect_words "a wrong Key Protect root key" "backup key could not be unwrapped" "${BUCKET_FLAGS[@]}" --kp-root-key 00000000-0000-0000-0000-000000000000
expect_words "a Secret that does not exist" "does not exist in namespace" "${BUCKET_FLAGS[@]}" --credentials-secret "$PREFIX-no-such-secret"
oc create secret generic "$BAD_SECRET" -n "$DRIVER_NS" --from-literal=access_key_id=notakey --from-literal=secret_access_key=notasecret >/dev/null
expect_words "a wrong COS key" "COS refused the key" "${BUCKET_FLAGS[@]}" --credentials-secret "$BAD_SECRET"
expect_words "a prefix with no repository" "helper pod failed" "${BUCKET_FLAGS[@]}" --prefix "$COS_PREFIX-nothing-here"
expect_words "a volume that was never backed up" "no backup of volume" "${BUCKET_FLAGS[@]}" --volume pvc-00000000-0000-0000-0000-000000000000

# ---- 7. no key leaks ------------------------------------------------------------------------------------
K=$(cos_private_dir)
jq -j '.credentials.cos_hmac_keys.access_key_id // empty' "$(cos_key_file)" >"$K/p1"
jq -j '.credentials.cos_hmac_keys.secret_access_key // empty' "$(cos_key_file)" >"$K/p2"
jq -j '.apikey // empty' "$(cos_kp_file)" >"$K/p3"
oc get secret "$KEY_SECRET" -n "$DRIVER_NS" -o jsonpath='{.data.wrappedKey}' | base64 -d >"$K/p4"
{
    oc logs -n "$DRIVER_NS" deploy/ibm-vpc-file-pool-csi-controller -c csi-controller --since=6h 2>&1 || true
    oc logs -n "$DRIVER_NS" "$GW_DEPLOY" --since=6h 2>&1 || true
    for p in $(oc get pod -n "$DRIVER_NS" -l storage.ibmcloud.io/browse -o name 2>/dev/null); do oc logs -n "$DRIVER_NS" "$p" 2>&1 || true; done
    cat "$WORK"/helper-*.log 2>/dev/null || true
    oc get events -A -o json 2>&1 || true
    oc get backupbrowse,backupfilerestore -o json 2>&1 || true
} >"$K/all"
for f in p1 p2 p3 p4; do
    [ -s "$K/$f" ] || fail "a key file for the leak check is empty ($f)"
    if grep -qF -f "$K/$f" "$K/all"; then cos_remove_dir "$K"; fail "a key is in a log, an Event or a status (pattern $f)"; fi
done
cos_remove_dir "$K"
pass "no COS key, API key or wrapped key is in the controller, gateway or helper logs, the Events or the statuses"
EV=$(oc get events -n "$DRIVER_NS" -o json | jq -r '.items[] | select(.reason == "SessionStarted" or .reason == "FileRestoreRequested") | .message')
grep -q "system:serviceaccount:$NS:alice" <<<"$EV" && grep -q "from COS" <<<"$EV" && pass "the audit Events name the user and say the backup was read from COS" || fail "audit events: $EV"

# ---- 8. nothing is left ------------------------------------------------------------------------------------
close_all
session_of() { # session_of <flags...>: open a session by hand and print its name
    bk "$COSADMIN" browse "$@" | awk '{print $2}'
}
S1=$(session_of "${BUCKET_FLAGS[@]}")
[ -n "$S1" ] || fail "browse printed no session"
UID1=$(oc get backupbrowse "$S1" -o jsonpath='{.metadata.uid}')
[ -n "$(oc get secret "browse-$UID1" -n "$DRIVER_NS" -o name 2>/dev/null)" ] && [ -n "$(oc get secret "browse-$UID1" -n "$BROWSE_NS" -o name 2>/dev/null)" ] \
    && pass "a COS session has its Secret in both namespaces while it is open" || fail "the session Secrets: driver=$(oc get secret -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$UID1" -o name) browse=$(oc get secret -n "$BROWSE_NS" -l "storage.ibmcloud.io/browse=$UID1" -o name)"
bk "$COSADMIN" browse --close "$S1" >/dev/null
closed() { [ "$(oc get backupbrowse "$S1" -o jsonpath='{.status.phase}')" = Closed ]; }
wait_until 240 "the COS session to close" closed
wait_until 240 "the closed COS session's objects to go" gone "$UID1"
pass "closing a COS session removes its pod, Job and both copies of its Secret"
S2=$(session_of "${BUCKET_FLAGS[@]}" --moment "$FIRST")
[ -n "$S2" ] || fail "browse printed no second session"
UID2=$(oc get backupbrowse "$S2" -o jsonpath='{.metadata.uid}')
oc delete backupbrowse "$S2" --wait=true >/dev/null
wait_until 240 "the objects of the deleted session to go" gone "$UID2"
pass "deleting a COS session removes its pod, Job and both copies of its Secret"
close_all
# Every session of this test names the test's policy; none is left behind.
for uid in $(oc get backupbrowse -o json | jq -r --arg p "$POLICY" '.items[] | select(.spec.policy == $p) | .metadata.uid'); do
    wait_until 240 "the objects of an ended session to go" gone "$uid"
done
pass "no pod, Job or Secret of any session of this test is left in either namespace"
[ -z "$(oc get pod,job -n "$DRIVER_NS" -o name 2>/dev/null | grep -E 'filerestore|browse' || true)" ] \
    && pass "no Job or pod of a restore or a session is left" || fail "a Job or pod is left: $(oc get pod,job -n "$DRIVER_NS" -o name | grep -E 'filerestore|browse')"
log "all checks passed"
