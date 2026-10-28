#!/usr/bin/env bash
# End-to-end test of file restore (issue #92, phase 2).
#
# Backs up a pod volume to a snapshot pool twice (the content changes between
# the two). The volume holds files of three owners, a file with mode 0600 and a
# folder with mode 0700 owned by other users, a link to /etc, a name with a
# newline, and a 40 MiB file with a recorded checksum. Then it makes
# BackupFileRestore objects by hand, as the gateway will, and checks:
#
# - into a new claim of a pool that configures an owner (107): every file has
#   that owner and its own mode, the folder with mode 0700 is whole, the link is
#   a link, the checksum of the big file is the same, the claim is sized by the
#   Job that measures (1 GiB), and pods can use the claim afterwards;
# - at the first moment, the first content comes back;
# - into an existing claim of a pool that configures no owner: a new folder
#   holds the tree with the backup's owners (1000, 2000) and modes, the claim's
#   own file is untouched, a second restore gets the next free name, and no
#   staging folder is left;
# - a claim that holds disk.img, a claim with too little room, a path that is
#   not in the backup, and a path with ".." are refused, and leave nothing;
# - after the restores no Job, share volume, claim or ConfigMap of a restore
#   remains, the Events hold counts and no path, and the backup is read-only to
#   the Job.
#
# When the backup gateway is installed it goes on with the real user path (part 2
# of the restore): people with their own tokens run `kubectl backup restore-files`.
# A namespace admin restores into a new claim and into a new folder of an existing
# claim with no setup; a user with only "view", and an admin of another namespace,
# are forbidden; after a RoleBinding to backup-file-reader an admin of another
# namespace restores into a claim of their own namespace, and cannot once the
# binding is removed; nobody restores into a namespace where they cannot make
# claims; a restore is shown only to the user who asked; the audit Event names the
# user and the paths and holds no token.
#
# Usage: KUBECONFIG=... [BACKUP_BIN=bin/kubectl-backup] test/e2e/test-backup-file-restore.sh [--keep]
set -euo pipefail

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-fr}
NS=${NS:-$PREFIX-test}
SRC=$PREFIX-src
SNAP=$PREFIX-snap
PLAIN=$PREFIX-plain
POLICY=$PREFIX-policy
DRIVER_NS=${DRIVER_NS:-kube-system}
IMG=${IMG:-quay.io/centos/centos:stream9}
WORK=$(mktemp -d)

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
pass() { log "PASS: $*"; }
fail() { log "FAIL: $*" >&2; oc get backupfilerestore -o wide 2>&1 | tail -10 >&2 || true; exit 1; }

cleanup() {
    find "$WORK" -delete 2>/dev/null || true
    if $KEEP; then log "keeping test resources (--keep)"; return; fi
    log "cleaning up"
    oc delete backupfilerestore -l "$PREFIX-test=yes" --ignore-not-found --wait=false || true
    oc delete backuppolicy "$POLICY" --ignore-not-found --wait=false || true
    oc delete ns "$NS" "$NS-other" --ignore-not-found --wait=true --timeout=15m || true
    oc delete filesharepool "$SRC" "$SNAP" "$PLAIN" --ignore-not-found --wait=false || true
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

# The tree. Owners are set with chown, as root, on the share (sec=sys).
oc exec -n "$NS" writer -- sh -c '
set -e
cd /data
mkdir -p docs big
echo first > v.txt
echo one > docs/a.txt
head -c 41943040 /dev/urandom > big/blob
for u in 1000 2000; do
  mkdir -p u$u/private0700
  echo secret > u$u/secret0600; chmod 600 u$u/secret0600
  echo public > u$u/public0644; chmod 644 u$u/public0644
  echo inside > u$u/private0700/inner0600; chmod 600 u$u/private0700/inner0600
  chmod 700 u$u/private0700
  ln -sfn /etc u$u/to-etc
  nl=$(printf "\n"); printf x > "u$u/line${nl}break"
  chown -R $u:$u u$u
done
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
YAML
vol() { oc get backuppolicy "$POLICY" -o json | jq -r ".status.volumes[]? | select(.pvcName == \"data\") | $1"; }
backed_up() { [ -n "$(vol '.lastBackupTime // empty')" ]; }
wait_until 1500 "the first backup" backed_up
FIRST=$(vol '.lastBackupTime')
log "first backup at $FIRST"
oc exec -n "$NS" writer -- sh -c 'echo second > /data/v.txt && echo two > /data/docs/b.txt'
two_moments() { [ "$(vol '.restorePoints // 0')" -ge 1 ] && [ "$(vol '.lastBackupTime')" != "$FIRST" ]; }
wait_until 1500 "a second backup" two_moments
pass "two backup moments exist (first $FIRST)"

# ---- helpers ------------------------------------------------------------------------------------
frphase() { oc get backupfilerestore "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
frmsg() { oc get backupfilerestore "$1" -o jsonpath='{.status.message}' 2>/dev/null; }
ended() { case "$(frphase "$1")" in Done|Failed) return 0 ;; esac; return 1; }
wait_ended() { # wait_ended <name> <seconds>
    wait_until "$2" "restore $1 to end" ended "$1"
}
restore() { # restore <name> <target claim> <existing true|false> <moment or ""> <paths yaml lines> [sizeGB]
    local moment=""
    [ -n "$4" ] && moment="  moment: \"$4\""
    local size=""
    [ -n "${6:-}" ] && size="  sizeGB: $6"
    oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: BackupFileRestore
metadata: {name: $1, labels: {$PREFIX-test: "yes"}}
spec:
  policy: $POLICY
  sourceNamespace: $NS
  sourceName: data
${moment}
  targetNamespace: $NS
  targetName: $2
  existing: $3
${size}
  requestedBy: lab-test
  paths:
$5
YAML
}

inspect() { # inspect <claim> <script>: run a script as root in a pod that mounts the claim; print its output
    local pod="$PREFIX-insp-$RANDOM$RANDOM" script
    # The script travels as base64, so no quote in it can break the manifest.
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

# ---- 1. a new claim, latest copy ----------------------------------------------------------------
NEW_PATHS="    - /docs
    - /v.txt
    - /big
    - /u1000
    - /u2000/secret0600
    - /u2000/private0700
    - '/u2000/line\\x0abreak'
    - /u2000/to-etc"
restore "$PREFIX-new" "$PREFIX-new" false "" "$NEW_PATHS"
wait_ended "$PREFIX-new" 1800
[ "$(frphase "$PREFIX-new")" = Done ] || fail "restore into a new claim: $(frphase "$PREFIX-new") $(frmsg "$PREFIX-new")"
pass "a restore into a new claim ends Done ($(frmsg "$PREFIX-new"))"
[ "$(oc get backupfilerestore "$PREFIX-new" -o jsonpath='{.status.sizeGB}')" = 1 ] && pass "the Job that measures sized the claim to 1 GiB (about 42 MiB of files)" || fail "the new claim was not sized to 1 GiB"
[ "$(oc get pvc "$PREFIX-new" -n "$NS" -o jsonpath='{.spec.resources.requests.storage}')" = 1Gi ] || fail "the claim is not 1Gi"
oc get pvc "$PREFIX-new" -n "$NS" -o json | jq -e --arg n "$PREFIX-new" '.metadata.annotations["storage.ibmcloud.io/backup-file-restore"] == $n' >/dev/null || fail "the new claim has no restore annotation"

OUT=$(inspect "$PREFIX-new" 'nl=$(printf "\n")
echo "docs=$(cat docs/a.txt) v=$(cat v.txt)"
echo "sha=$(sha256sum big/blob | cut -d" " -f1)"
echo "secret=$(stat -c "%u:%g %a" u1000/secret0600) $(stat -c "%u:%g %a" u2000/secret0600)"
echo "private=$(stat -c "%u:%g %a" u2000/private0700) inner=$(stat -c "%u:%g %a" u2000/private0700/inner0600)"
echo "linebreak=$([ -f "u2000/line${nl}break" ] && echo yes || echo no)"
echo "link=$([ -L u2000/to-etc ] && readlink u2000/to-etc || echo notalink) $([ -L u1000/to-etc ] && echo ok || echo notalink)"
echo "only=$(ls | tr "\n" " ")"
echo "u2000=$(ls u2000 | wc -l)"')
echo "$OUT"
grep -q "^docs=one v=second$" <<<"$OUT" && pass "the new claim holds the latest content" || fail "content: $OUT"
grep -q "^sha=$BLOB_SHA$" <<<"$OUT" && pass "the 40 MiB file has the same checksum" || fail "checksum: $OUT"
grep -q "^secret=107:107 600 107:107 600$" <<<"$OUT" && pass "files with mode 0600 keep their mode and get the owner of the pool (107)" || fail "0600 files: $OUT"
grep -q "^private=107:107 700 inner=107:107 600$" <<<"$OUT" && pass "a folder with mode 0700 is restored whole, with the pool's owner" || fail "0700 folder: $OUT"
grep -q "^linebreak=yes$" <<<"$OUT" && pass "a name with a newline is restored" || fail "newline name: $OUT"
grep -q "^link=/etc ok$" <<<"$OUT" && pass "a link to /etc is restored as a link, not followed" || fail "link: $OUT"
grep -q "^only=big docs u1000 u2000 v.txt $" <<<"$OUT" && pass "only the chosen paths and the folders above them are in the claim" || fail "listing: $OUT"
grep -q "^u2000=4$" <<<"$OUT" && pass "the chosen files of u2000 are there and no others (4 entries)" || fail "u2000 count: $OUT"
[ "$(oc get subvolume -l storage.ibmcloud.io/restore-pending=true -o name | wc -l | tr -d ' ')" = 0 ] && pass "no claim is held back after the restores" || fail "a claim is still held back"

# ---- 2. a new claim at the first moment ---------------------------------------------------------
restore "$PREFIX-first" "$PREFIX-first" false "$FIRST" '    - /v.txt
    - /docs' 3
wait_ended "$PREFIX-first" 1800
[ "$(frphase "$PREFIX-first")" = Done ] || fail "restore at the first moment: $(frmsg "$PREFIX-first")"
[ "$(oc get pvc "$PREFIX-first" -n "$NS" -o jsonpath='{.spec.resources.requests.storage}')" = 3Gi ] && pass "sizeGB sets the size of a new claim" || fail "claim size is not 3Gi"
OUT=$(inspect "$PREFIX-first" 'echo "v=$(cat v.txt)"; echo "b=$([ -e docs/b.txt ] && echo yes || echo no)"')
grep -q "^v=first$" <<<"$OUT" && grep -q "^b=no$" <<<"$OUT" && pass "a restore at the first moment brings back the first content, without files made later" || fail "first moment: $OUT"

# ---- 3. an existing claim of a pool with no configured owner -------------------------------------
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
EX_PATHS="    - /u1000
    - /u2000
    - /docs"
restore "$PREFIX-ex1" work true "" "$EX_PATHS"
wait_ended "$PREFIX-ex1" 900
[ "$(frphase "$PREFIX-ex1")" = Done ] || fail "restore into an existing claim: $(frmsg "$PREFIX-ex1")"
FOLDER=$(oc get backupfilerestore "$PREFIX-ex1" -o jsonpath='{.status.folder}')
case "$FOLDER" in restored-"$POLICY"-*) pass "an existing claim gets the new folder $FOLDER" ;; *) fail "folder name: $FOLDER" ;; esac
OUT=$(inspect work "echo \"mine=\$(cat mine.txt)\"
cd '$FOLDER'
echo \"s1000=\$(stat -c '%u:%g %a' u1000/secret0600)\"
echo \"s2000=\$(stat -c '%u:%g %a' u2000/secret0600)\"
echo \"p2000=\$(stat -c '%u:%g %a' u2000/private0700) \$(stat -c '%u:%g %a' u2000/private0700/inner0600)\"
echo \"link=\$(readlink u1000/to-etc)\"
echo \"top=\$(ls -A .. | tr '\\n' ' ')\"")
echo "$OUT"
grep -q "^mine=mine$" <<<"$OUT" && pass "the claim's own file is untouched" || fail "mine.txt: $OUT"
grep -q "^s1000=1000:1000 600$" <<<"$OUT" && grep -q "^s2000=2000:2000 600$" <<<"$OUT" && pass "with no configured owner the files keep the backup's owners (1000, 2000) and mode 0600" || fail "owners: $OUT"
grep -q "^p2000=2000:2000 700 2000:2000 600$" <<<"$OUT" && pass "a folder with mode 0700 owned by another user is restored whole" || fail "0700: $OUT"
grep -q "^link=/etc$" <<<"$OUT" && pass "the link is a link" || fail "link: $OUT"
if grep -q "restoring-" <<<"$(grep '^top=' <<<"$OUT")"; then fail "a staging folder is left: $OUT"; else pass "no staging folder is left in the claim"; fi

restore "$PREFIX-ex2" work true "" "$EX_PATHS"
wait_ended "$PREFIX-ex2" 900
[ "$(frphase "$PREFIX-ex2")" = Done ] || fail "second restore: $(frmsg "$PREFIX-ex2")"
FOLDER2=$(oc get backupfilerestore "$PREFIX-ex2" -o jsonpath='{.status.folder}')
[ "$FOLDER2" = "$FOLDER-2" ] && pass "a second restore takes the next free name ($FOLDER2) and never replaces the first" || fail "second folder: $FOLDER2"

# ---- 4. refusals --------------------------------------------------------------------------------
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: vmdisk}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $PLAIN
  resources: {requests: {storage: 2Gi}}
YAML
inspect vmdisk 'head -c 1000 /dev/zero > disk.img; echo ok' >/dev/null
restore "$PREFIX-vm" vmdisk true "" '    - /docs'
wait_ended "$PREFIX-vm" 600
[ "$(frphase "$PREFIX-vm")" = Failed ] && frmsg "$PREFIX-vm" | grep -q "VM disk" && pass "a claim that holds disk.img is refused" || fail "VM disk claim: $(frphase "$PREFIX-vm") $(frmsg "$PREFIX-vm")"
OUT=$(inspect vmdisk 'ls -A | tr "\n" " "')
[ "$OUT" = "disk.img " ] && pass "nothing was written into the claim that holds disk.img" || fail "vmdisk content: $OUT"

oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: tiny}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $PLAIN
  resources: {requests: {storage: 1Gi}}
YAML
inspect tiny 'dd if=/dev/zero of=fill bs=1M count=1000 2>/dev/null; echo ok' >/dev/null
restore "$PREFIX-full" tiny true "" '    - /big'
wait_ended "$PREFIX-full" 600
[ "$(frphase "$PREFIX-full")" = Failed ] && frmsg "$PREFIX-full" | grep -q "not enough space" && pass "files that do not fit in the claim are refused before they are copied" || fail "no room: $(frphase "$PREFIX-full") $(frmsg "$PREFIX-full")"
OUT=$(inspect tiny 'ls -A | tr "\n" " "')
[ "$OUT" = "fill " ] && pass "a refused restore leaves nothing in the claim" || fail "tiny content: $OUT"

restore "$PREFIX-gone" work true "" '    - /no/such/file'
wait_ended "$PREFIX-gone" 600
[ "$(frphase "$PREFIX-gone")" = Failed ] && pass "a path that is not in the backup fails the restore" || fail "missing path: $(frphase "$PREFIX-gone")"
OUT=$(inspect work 'ls -A | tr "\n" " "')
grep -q "restoring-" <<<"$OUT" && fail "a failed restore left a staging folder: $OUT" || pass "a failed restore leaves no staging folder"

if oc apply -f - >/dev/null 2>"$WORK/webhook.err" <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: BackupFileRestore
metadata: {name: $PREFIX-dots, labels: {$PREFIX-test: "yes"}}
spec: {policy: $POLICY, sourceNamespace: $NS, sourceName: data, targetNamespace: $NS, targetName: $PREFIX-dots, paths: ["/docs/../../etc"]}
YAML
then
    fail "a path with .. was accepted"
else
    grep -q "paths\[0\]" "$WORK/webhook.err" && pass "the webhook refuses a path with .." || fail "webhook message: $(cat "$WORK/webhook.err")"
fi

# ---- 5. users, through the gateway ------------------------------------------------------------
GW_DEPLOY=$(oc get deploy -n "$DRIVER_NS" -l app.kubernetes.io/component=backup-gateway -o name 2>/dev/null | head -1)
if [ -z "$GW_DEPLOY" ]; then
    log "NOT CHECKED: the backup gateway is not installed (backupGateway.enabled)"
else
    BACKUP_BIN=${BACKUP_BIN:-kubectl-backup}
    command -v "$BACKUP_BIN" >/dev/null 2>&1 || [ -x "$BACKUP_BIN" ] || fail "no kubectl-backup binary (BACKUP_BIN)"
    oc rollout status "$GW_DEPLOY" -n "$DRIVER_NS" --timeout=300s >/dev/null || fail "the gateway is not ready"

    OTHER=$NS-other
    oc create ns "$OTHER" >/dev/null
    for sa in alice viewer; do oc create sa "$sa" -n "$NS" >/dev/null; done
    oc create sa mallory -n "$OTHER" >/dev/null
    oc create rolebinding alice-admin -n "$NS" --clusterrole=admin --serviceaccount="$NS:alice" >/dev/null
    oc create rolebinding viewer-view -n "$NS" --clusterrole=view --serviceaccount="$NS:viewer" >/dev/null
    oc create rolebinding mallory-admin -n "$OTHER" --clusterrole=admin --serviceaccount="$OTHER:mallory" >/dev/null

    SERVER=$(oc whoami --show-server)
    user_kubeconfig() { # user_kubeconfig <name> <namespace>: a kubeconfig that holds only that user's token
        local kc="$WORK/$1.kubeconfig"
        oc create token "$1" -n "$2" --duration=2h > "$WORK/$1.sa-token"
        kubectl --kubeconfig "$kc" config set-cluster c --server="$SERVER" --insecure-skip-tls-verify=true >/dev/null
        kubectl --kubeconfig "$kc" config set-credentials u --token="$(cat "$WORK/$1.sa-token")" >/dev/null
        kubectl --kubeconfig "$kc" config set-context x --cluster=c --user=u >/dev/null
        kubectl --kubeconfig "$kc" config use-context x >/dev/null
        echo "$kc"
    }
    ALICE=$(user_kubeconfig alice "$NS"); VIEWER=$(user_kubeconfig viewer "$NS"); MALLORY=$(user_kubeconfig mallory "$OTHER")

    GW_ROUTE=$(oc get route -n "$DRIVER_NS" -l app.kubernetes.io/component=backup-gateway -o jsonpath='{.items[0].spec.host}' 2>/dev/null || true)
    [ -n "$GW_ROUTE" ] || fail "the gateway has no Route"
    GW_CA=()
    [ -n "${GATEWAY_CA_FILE:-}" ] && GW_CA=(--gateway-ca-file "$GATEWAY_CA_FILE")
    bk() { # bk <kubeconfig> <args...>
        local kc=$1; shift
        "$BACKUP_BIN" "$1" --kubeconfig "$kc" --gateway "https://$GW_ROUTE" "${GW_CA[@]}" "${@:2}"
    }
    SRC_ARGS=(--policy "$POLICY" --pvc "$NS/data")

    OUT=$(bk "$ALICE" restore-files "${SRC_ARGS[@]}" --path /docs --path /v.txt --to "$NS/gw-new" --wait --timeout 25m)
    echo "$OUT"
    grep -q "^Done\|Done" <<<"$OUT" && pass "a namespace admin restores into a new claim with kubectl backup restore-files and no setup" || fail "alice's restore: $OUT"
    OUT=$(inspect gw-new 'echo "v=$(cat v.txt) a=$(cat docs/a.txt)"')
    grep -q "^v=second a=one$" <<<"$OUT" && pass "the files restored by a user are in the new claim" || fail "gw-new content: $OUT"

    OUT=$(bk "$ALICE" restore-files "${SRC_ARGS[@]}" --path /docs --to "$NS/work" --existing --wait --timeout 25m)
    echo "$OUT"
    grep -q "restored-$POLICY-" <<<"$OUT" && pass "a user restores into a new folder of an existing claim" || fail "alice's existing restore: $OUT"

    expect_denied() { # expect_denied <description> <expected text> <kubeconfig> <restore-files args...>
        local what=$1 text=$2 kc=$3 out; shift 3
        if out=$(bk "$kc" restore-files "$@" 2>&1); then fail "$what: the call worked: $out"; fi
        grep -qi "$text" <<<"$out" && pass "$what: forbidden" || fail "$what: unexpected error: $out"
    }
    expect_denied "a user with only the view role" "may not read the backups" "$VIEWER" "${SRC_ARGS[@]}" --path /docs --to "$NS/denied-1"
    expect_denied "an admin of another namespace" "may not read the backups" "$MALLORY" "${SRC_ARGS[@]}" --path /docs --to "$OTHER/denied-2"
    expect_denied "restoring into a namespace where the user cannot make claims" "claims in that namespace" "$ALICE" "${SRC_ARGS[@]}" --path /docs --to "$OTHER/denied-3"
    [ -z "$(oc get backupfilerestore -o json | jq -r '.items[] | select(.spec.targetName | startswith("denied-")) | .metadata.name')" ] && pass "a refused request made no restore" || fail "a restore was made for a refused request"
    [ -z "$(oc get pvc -A -o name | grep denied- || true)" ] && pass "a refused request made no claim" || fail "a claim was made for a refused request"

    oc create rolebinding mallory-reader -n "$NS" --clusterrole=backup-file-reader --serviceaccount="$OTHER:mallory" >/dev/null
    OUT=$(bk "$MALLORY" restore-files "${SRC_ARGS[@]}" --path /v.txt --to "$OTHER/copy" --wait --timeout 25m)
    echo "$OUT"
    grep -q "Done" <<<"$OUT" && pass "with backup-file-reader in the source namespace an admin of another namespace restores into a claim of their own" || fail "mallory's restore: $OUT"

    ASKED=$(bk "$ALICE" restore-files "${SRC_ARGS[@]}" --path /docs --to "$NS/gw-status")
    RNAME=$(awk '/^Restore /{print $2; exit}' <<<"$ASKED")
    [ -n "$RNAME" ] || fail "no restore name in: $ASKED"
    bk "$ALICE" restore-files --status "$RNAME" >/dev/null && pass "the user who asked sees the restore" || fail "alice cannot see her restore"
    if bk "$MALLORY" restore-files --status "$RNAME" >/dev/null 2>&1; then fail "another user saw the restore"; else pass "another user, even one with the same rights, sees no such restore"; fi

    oc delete rolebinding mallory-reader -n "$NS" >/dev/null
    expect_denied "a user whose binding was removed" "may not read the backups" "$MALLORY" "${SRC_ARGS[@]}" --path /v.txt --to "$OTHER/denied-4"

    EV=$(oc get events -n "$DRIVER_NS" -o json | jq -r '.items[] | select(.reason == "FileRestoreRequested") | .message')
    grep -q "system:serviceaccount:$NS:alice" <<<"$EV" && grep -q "/docs" <<<"$EV" && pass "the audit Events name the user and the paths" || fail "audit events: $EV"
    if grep -qF "$(cat "$WORK/alice.sa-token")" <<<"$EV" || grep -qF "$(cat "$WORK/mallory.sa-token")" <<<"$EV"; then fail "an audit Event holds a token"; else pass "the audit Events hold no token"; fi
fi

# ---- 6. what is left behind --------------------------------------------------------------------
none_left() { [ -z "$(oc get job,pvc -n "$DRIVER_NS" -o name 2>/dev/null | grep filerestore || true)" ]; }
wait_until 180 "the Jobs and share claims of the restores to go" none_left
pass "no Job or share claim of a restore is left in the driver namespace"
[ -z "$(oc get pv -o name | grep filerestore || true)" ] && pass "no share volume of a restore is left" || fail "a share PV is left"
CM_UID=$(oc get backupfilerestore "$PREFIX-ex1" -o jsonpath='{.metadata.uid}')
oc delete backupfilerestore "$PREFIX-ex1" --wait=true >/dev/null
gone_cm() { [ -z "$(oc get configmap -n "$DRIVER_NS" -o json | jq -r --arg u "$CM_UID" '.items[] | select(.metadata.ownerReferences[]?.uid == $u) | .metadata.name')" ]; }
wait_until 120 "the list of paths to go with its restore" gone_cm
pass "deleting a restore removes its list of paths"

EV=$(oc get events -n "$DRIVER_NS" -o json | jq -r '.items[] | select(.reason == "FileRestoreDone" or .reason == "FileRestoreFailed") | .message')
grep -q "restored .* files" <<<"$EV" && pass "the controller writes an Event with the counts when a restore ends" || fail "no restore Event: $EV"
if grep -q "u1000\|u2000\|secret0600\|/docs" <<<"$EV"; then fail "an Event names a path: $EV"; else pass "the Events name no path"; fi
log "all checks passed"
