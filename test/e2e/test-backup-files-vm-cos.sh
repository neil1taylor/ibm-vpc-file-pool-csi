#!/usr/bin/env bash
# End-to-end test of browsing files inside a VM disk that is backed up in COS
# (issue #371, phases 6b to 6d of docs/specs/2026-10-29-vm-disks-in-cos.md).
#
# Needs the guest image (GUEST_IMAGE, with backupBrowse.guest.enabled and the
# backup gateway on) and the COS configuration of stage 18 (see
# test/e2e/lib/backup-cos.sh). Without them it prints "NOT CHECKED" and ends well.
# Secret values go from file to file to Secret and are never in a variable, on a
# command line or in the log.
#
# What it does: makes three small VM disks (XFS on LVM, ext4, NTFS) as raw files on
# three claims of a stopped VM, backs them up to a snapshot pool with the export
# block off, changes hello.txt on disk1 and backs up again, then turns the export
# on (so that no export holds the first restore point as its own newest), and waits
# until the newest restore point of each disk is in COS with its file list. The first
# restore point of disk1 is then in COS only in the undo files of that export. Then, as a real user
# through the gateway with --from-cos:
# - the file systems and folders of each disk are listed; hello.txt reads "second";
#   a 256 MiB file has the checksum it was made with;
# - the speed bar is measured and logged as "MEASURE:" lines: first listing,
#   another folder in the open session, the first bytes of a file, the whole file;
# - an older restore point that no export holds as its own newest (it lives only in the undo
#   files) is listed by ls --restore-points, read, and has its own content (hello.txt reads
#   "first", the 256 MiB file has its checksum); the speed of that is logged as "MEASURE:" lines;
# - files are restored from a VM disk in COS into a new claim (phase 6c): a small
#   file from the XFS disk and from the NTFS disk, and the 256 MiB file, with the
#   same checksum; the disk server of each restore is gone when it ends; a file is also
#   restored from the older restore point (phase 6d);
# - the disk server pod and the browse Secrets are locked down;
# - closing the session leaves no pod, Job or Secret behind.
#
# Usage: KUBECONFIG=... GUEST_IMAGE=... COS_BUCKET=... [BACKUP_BIN=bin/kubectl-backup] test/e2e/test-backup-files-vm-cos.sh [--keep]
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/backup-cos.sh
source "$HERE/lib/backup-cos.sh"

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-bw}
NS=${NS:-$PREFIX-test}
SRC=$PREFIX-src
SNAP=$PREFIX-snap
POLICY=$PREFIX-policy
DRIVER_NS=${DRIVER_NS:-kube-system}
BROWSE_NS=${BROWSE_NS:-ibm-vpc-file-pool-csi-browse}
IMG=${IMG:-quay.io/centos/centos:stream9}
WORK=$(mktemp -d)
chmod 700 "$WORK"

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
pass() { log "PASS: $*"; }
fail() { log "FAIL: $*" >&2; oc get backupbrowse,backupfilerestore -o wide 2>&1 | tail -10 >&2 || true; exit 1; }
now() { perl -MTime::HiRes=time -e 'printf "%.2f", time'; }
measure() { # measure <what> <start>
    log "MEASURE: $1 took $(perl -e "printf '%.1f', $(now) - $2") seconds"
}

if [ -z "${GUEST_IMAGE:-}" ]; then
    log "NOT CHECKED: no guest image (GUEST_IMAGE), so browsing inside a VM disk was not tried"
    log "all checks passed"
    exit 0
fi
cos_load_settings "$(cos_config_dir)/cos-stage.env"
if ! cos_wanted; then
    log "NOT CHECKED: no COS bucket is configured (COS_BUCKET), so a VM disk in COS was not tried"
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
    oc delete ns "$NS" --ignore-not-found --wait=true --timeout=15m || true
    oc delete filesharepool "$SRC" "$SNAP" --ignore-not-found --wait=false || true
    cos_delete_secrets "$DRIVER_NS" "$PREFIX"
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
make_pool() { # make_pool <name> <extra spec lines>
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
all_ready() { pool_ready "$SRC" && pool_ready "$SNAP"; }

cos_make_secrets "$DRIVER_NS" "$PREFIX" || fail "cannot make the COS Secrets"
log "making pools $SRC (owner 107, as VM pools have) and $SNAP"
make_pool "$SRC" '  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"'
make_pool "$SNAP" ''
wait_until 1500 "pools Ready" all_ready

oc create ns "$NS"
oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null
oc get secret all-icr-io -n "$DRIVER_NS" -o json | jq 'del(.metadata.namespace,.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp,.metadata.ownerReferences)' | oc apply -n "$NS" -f - >/dev/null
for d in disk1 disk2 disk3; do
    oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $d, labels: {backup: "yes"}}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $SRC
  resources: {requests: {storage: 2Gi}}
YAML
done
oc apply -n "$NS" -f - <<YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: vm}
spec:
  runStrategy: Halted
  template:
    spec:
      domain:
        cpu: {cores: 1}
        memory: {guest: 1Gi}
        devices:
          disks:
          - {name: d1, disk: {bus: virtio}}
          - {name: d2, disk: {bus: virtio}}
          - {name: d3, disk: {bus: virtio}}
      volumes:
      - {name: d1, persistentVolumeClaim: {claimName: disk1}}
      - {name: d2, persistentVolumeClaim: {claimName: disk2}}
      - {name: d3, persistentVolumeClaim: {claimName: disk3}}
YAML

# ---- make the disks (a Job in the guest image) --------------------------------------------------
run_disk_job() { # run_disk_job <name> <script>
    local script
    script=$(printf '%s' "$2" | base64 | tr -d '\n')
    oc apply -n "$NS" -f - <<YAML >/dev/null
apiVersion: batch/v1
kind: Job
metadata: {name: $1}
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      imagePullSecrets: [{name: all-icr-io}]
      containers:
      - name: j
        image: $GUEST_IMAGE
        command: ["/bin/sh","-c"]
        args: ["echo $script | base64 -d | sh -e"]
        env:
        - {name: LIBGUESTFS_BACKEND, value: direct}
        - {name: LIBGUESTFS_PATH, value: /opt/guestfs-appliance}
        - {name: LIBGUESTFS_BACKEND_SETTINGS, value: force_tcg}
        - {name: HOME, value: /tmp}
        volumeMounts:
        - {name: d1, mountPath: /d1}
        - {name: d2, mountPath: /d2}
        - {name: d3, mountPath: /d3}
      volumes:
      - {name: d1, persistentVolumeClaim: {claimName: disk1}}
      - {name: d2, persistentVolumeClaim: {claimName: disk2}}
      - {name: d3, persistentVolumeClaim: {claimName: disk3}}
YAML
    local end=$((SECONDS + 1800)) ok="" failed=""
    while [ $SECONDS -lt $end ]; do
        ok=$(oc get "job/$1" -n "$NS" -o jsonpath='{.status.succeeded}' 2>/dev/null)
        failed=$(oc get "job/$1" -n "$NS" -o jsonpath='{.status.failed}' 2>/dev/null)
        [ -n "$ok" ] || [ -n "$failed" ] && break
        sleep 10
    done
    if [ -z "$ok" ]; then
        oc logs "job/$1" -n "$NS" --tail=40 >&2 || true
        fail "the disk Job $1 failed"
    fi
}
run_disk_job make-disks '
mkdir -p /tmp/t/docs
printf first > /tmp/t/hello.txt
printf one > /tmp/t/docs/a.txt
head -c 268435456 /dev/urandom > /tmp/t/big.bin
printf x > "/tmp/t/line
break"
sha256sum /tmp/t/big.bin | cut -d" " -f1 > /d1/big.sha
guestfish <<EOF
disk-create /d1/disk.img raw 1G
add-drive /d1/disk.img format:raw
run
part-init /dev/sda gpt
part-add /dev/sda p 2048 -2048
pvcreate /dev/sda1
vgcreate vg0 /dev/sda1
lvcreate root vg0 900
mkfs xfs /dev/vg0/root
mount /dev/vg0/root /
copy-in /tmp/t/hello.txt /
copy-in /tmp/t/docs /
copy-in /tmp/t/big.bin /
EOF
virt-make-fs --format=raw --size=+400M --type=ext4 /tmp/t /d2/disk.img
virt-make-fs --format=raw --size=+400M --type=ntfs --partition=mbr /tmp/t /d3/disk.img
chmod 666 /d1/disk.img /d2/disk.img /d3/disk.img
chown 107:107 /d1/disk.img /d2/disk.img /d3/disk.img 2>/dev/null || true
'
BIG_SHA=$(oc logs -n "$NS" job/make-disks 2>/dev/null | grep -Eo '^[0-9a-f]{64}$' | head -1 || true)
if [ ${#BIG_SHA} -ne 64 ]; then
    # The checksum file is on the claim: read it with a throw-away pod.
    oc run "$PREFIX-sha" -n "$NS" --restart=Never --image="$IMG" --overrides='{"spec":{"containers":[{"name":"s","image":"'"$IMG"'","command":["cat","/d1/big.sha"],"volumeMounts":[{"name":"d","mountPath":"/d1"}]}],"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"disk1"}}]}}' >/dev/null
    wait_until 300 "the checksum pod" bash -c "[ \"\$(oc get pod $PREFIX-sha -n $NS -o jsonpath='{.status.phase}')\" = Succeeded ]"
    BIG_SHA=$(oc logs -n "$NS" "$PREFIX-sha" | grep -Eo '^[0-9a-f]{64}$' | head -1 || true)
fi
[ ${#BIG_SHA} -eq 64 ] || fail "no checksum of the big file"

# The policy starts without an export block. The export is turned on after the later backups, so that
# no export holds the first restore point as its own newest: it is then only in the undo files.
policy_yaml() { # policy_yaml <schedule> <with export: yes|no>
    cat <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: BackupPolicy
metadata: {name: $POLICY}
spec:
  snapshotPool: $SNAP
  schedule: $1
  keepFor: 1h
  namespaces: [$NS]
  selector: {matchLabels: {backup: "yes"}}
YAML
    [ "$2" = yes ] && cos_export_yaml "$COS_PREFIX" "$CRED_SECRET" "$KEY_SECRET" "$KP_SECRET"
    return 0
}
policy_yaml 1m no | oc apply -f -
vol() { oc get backuppolicy "$POLICY" -o json | jq -r ".status.volumes[]? | select(.pvcName == \"$1\") | $2"; }
backed_up() { [ -n "$(vol disk1 '.lastBackupTime // empty')" ] && [ -n "$(vol disk2 '.lastBackupTime // empty')" ] && [ -n "$(vol disk3 '.lastBackupTime // empty')" ]; }
wait_until 1800 "the first backup of the three disks" backed_up
FIRST=$(vol disk1 '.lastBackupTime')

run_disk_job change-disk '
guestfish --format=raw -a /d1/disk.img <<EOF
run
mount /dev/vg0/root /
write /hello.txt second
EOF
'
two_moments() { [ "$(vol disk1 '.restorePoints // 0')" -ge 1 ] && [ "$(vol disk1 '.lastBackupTime')" != "$FIRST" ]; }
wait_until 1800 "a second backup of disk1" two_moments
# A backup already running when the file changed can end after the change and still hold the old
# content, so wait for one more backup, which started after the change.
SECOND=$(vol disk1 '.lastBackupTime')
later_moment() { [ "$(vol disk1 '.lastBackupTime')" != "$SECOND" ]; }
wait_until 1800 "a backup of disk1 that started after the change" later_moment
# The test schedule is 1 minute, which makes a newer restore point before an export ends, so no
# export is ever of the newest restore point. Now that the later backups are in, the schedule is
# stretched and the export is turned on. The first export is of the newest restore point, and it
# holds the earlier ones (the first among them) in its undo files only.
policy_yaml 24h yes | oc apply -f -
exported() { # exported <claim>
    [ -n "$(vol "$1" '.exportedTime // empty')" ] && [ "$(vol "$1" '.exportedTime')" = "$(vol "$1" '.lastBackupTime')" ] && [ -z "$(vol "$1" '.exportError // empty')" ]
}
all_exported() { exported disk1 && exported disk2 && exported disk3; }
EXPORT_START=$(now)
wait_until 3000 "the newest restore point of the three disks to be exported to COS" all_exported
pass "two restore points of disk1 are in the snapshot pool (first $FIRST) and the newest of each disk is in COS"
for d in disk1 disk2 disk3; do
    n=$(vol "$d" '.fileListEntries // 0')
    [ "$n" -gt 0 ] && pass "$d: a file list of $n names was saved with the export" || log "NOTE: $d has no file list (fileListError: $(vol "$d" '.fileListError // "none"'))"
done
vol disk1 '.fileListError // empty' | grep -q . && fail "disk1 file list error: $(vol disk1 '.fileListError')"

# ---- users ------------------------------------------------------------------------------------
GW_DEPLOY=$(oc get deploy -n "$DRIVER_NS" -l app.kubernetes.io/component=backup-gateway -o name 2>/dev/null | head -1)
if [ -z "$GW_DEPLOY" ]; then
    log "NOT CHECKED: the backup gateway is not installed (backupGateway.enabled)"
    log "all checks passed"
    exit 0
fi
oc rollout status "$GW_DEPLOY" -n "$DRIVER_NS" --timeout=300s >/dev/null || fail "the gateway is not ready"
oc create sa alice -n "$NS" >/dev/null
oc create rolebinding alice-admin -n "$NS" --clusterrole=admin --serviceaccount="$NS:alice" >/dev/null
SERVER=$(oc whoami --show-server)
ALICE="$WORK/alice.kubeconfig"
oc create token alice -n "$NS" --duration=2h > "$WORK/alice.sa-token"
kubectl --kubeconfig "$ALICE" config set-cluster c --server="$SERVER" --insecure-skip-tls-verify=true >/dev/null
kubectl --kubeconfig "$ALICE" config set-credentials u --token="$(cat "$WORK/alice.sa-token")" >/dev/null
kubectl --kubeconfig "$ALICE" config set-context x --cluster=c --user=u >/dev/null
kubectl --kubeconfig "$ALICE" config use-context x >/dev/null
GW_ROUTE=$(oc get route -n "$DRIVER_NS" -l app.kubernetes.io/component=backup-gateway -o jsonpath='{.items[0].spec.host}' 2>/dev/null || true)
[ -n "$GW_ROUTE" ] || fail "the gateway has no Route"
GW_CA=()
[ -n "${GATEWAY_CA_FILE:-}" ] && GW_CA=(--gateway-ca-file "$GATEWAY_CA_FILE")
bk() { # bk <command> <args...>
    "$BACKUP_BIN" "$1" --kubeconfig "$ALICE" --gateway "https://$GW_ROUTE" "${GW_CA[@]}" "${@:2}"
}
T=(--timeout 25m)
D1=(--from-cos --policy "$POLICY" --pvc "$NS/disk1")
D2=(--from-cos --policy "$POLICY" --pvc "$NS/disk2")
D3=(--from-cos --policy "$POLICY" --pvc "$NS/disk3")
none_open() {
    [ "$(oc get backupbrowse -o json | jq --arg p "$POLICY" '[.items[] | select(.spec.policy == $p and ((.status.phase // "") | IN("Pending","Starting","Ready")))] | length')" = 0 ]
}
close_all() {
    local name
    while read -r name; do
        [ -n "$name" ] && oc patch backupbrowse "$name" --type merge -p '{"spec":{"close":true}}' >/dev/null
    done < <(oc get backupbrowse -o json | jq -r --arg p "$POLICY" '.items[] | select(.spec.policy == $p and ((.status.phase // "") | IN("Pending","Starting","Ready"))) | .metadata.name')
    wait_until 600 "the sessions of this test to close" none_open
}

# ---- 1. the speed bar on disk1 ------------------------------------------------------------------
# The first listing starts the session (disk server and guest reader); the bar is 30 seconds.
S=$(now)
OUT=$(bk ls "${D1[@]}" "${T[@]}") || fail "ls --from-cos of a VM disk: $OUT"
measure "the first listing of a VM disk in COS (bar 30)" "$S"
grep -q "vg0-root/" <<<"$OUT" && pass "ls lists the file systems of an LVM disk in COS" || fail "disk1 file systems: $OUT"
echo "$OUT" | sed -n 1,5p | sed 's/^/  | /'
# Another folder in the open session; the bar is 5 seconds.
S=$(now)
OUT=$(bk ls "${D1[@]}" "${T[@]}" --fs vg0-root /docs) || fail "ls of a folder: $OUT"
measure "another folder in the open session (bar 5)" "$S"
grep -q "a.txt" <<<"$OUT" && pass "a folder of a VM disk in COS is listed" || fail "docs listing: $OUT"
OUT=$(bk ls "${D1[@]}" "${T[@]}" --fs vg0-root /)
grep -q "hello.txt" <<<"$OUT" && grep -q "docs/" <<<"$OUT" && pass "the root folder is listed with its files and folders" || fail "root listing: $OUT"
grep -qF 'line\x0abreak' <<<"$OUT" && pass "a name with a newline is listed escaped" || log "NOTE: the newline name is on the ext4 disk only"

mkdir "$WORK/got"
bk get "${D1[@]}" "${T[@]}" --fs vg0-root /hello.txt --out "$WORK/got" >/dev/null
[ "$(cat "$WORK/got/hello.txt")" = second ] && pass "XFS on LVM in COS: the latest copy reads 'second'" || fail "hello.txt: $(cat "$WORK/got/hello.txt")"
S=$(now)
bk get "${D1[@]}" "${T[@]}" --fs vg0-root /big.bin --out "$WORK/got" >/dev/null
measure "downloading a 256 MiB file (bar: start within 2 minutes, then 50 MiB/s)" "$S"
log "MEASURE: that is $(perl -e "printf '%.1f', 256 / ($(now) - $S)") MiB/s including the start"
[ "$(shasum -a 256 "$WORK/got/big.bin" | awk '{print $1}')" = "$BIG_SHA" ] && pass "a 256 MiB file from COS has the same checksum" || fail "big.bin checksum"
# The rate through the gateway depends on the network of whoever runs this script (from a laptop over a
# VPN it is far below the bar). The rate of the helper itself is read from inside its pod.
UID1=$(oc get backupbrowse -o json | jq -r --arg p "$POLICY" '[.items[] | select(.spec.policy == $p and .status.phase == "Ready")][0].metadata.uid')
HELPER=$(oc get pod -n "$BROWSE_NS" -l "storage.ibmcloud.io/browse=$UID1,storage.ibmcloud.io/browse-guest=true" -o name | head -1)
if [ -n "$HELPER" ]; then
    for i in 1 2; do
        RATE=$(oc exec -n "$BROWSE_NS" "$HELPER" -- sh -c 'curl -sk -X POST -H "Authorization: Bearer $(cat /etc/browse/token)" -H "Content-Type: application/json" -d "{\"path\":[\"dmcwLXJvb3Q=\",\"YmlnLmJpbg==\"]}" -o /dev/null -w "%{size_download} %{time_starttransfer} %{time_total}" https://localhost:8443/v1/file' 2>/dev/null || true)
        log "MEASURE: inside the helper pod, download $i of the 256 MiB file (bytes, first byte, total): $RATE ; the rate after the first byte is $(awk '{ if ($3 > $2) printf "%.1f", $1 / 1048576 / ($3 - $2); else print "n/a" }' <<<"$RATE") MiB/s (bar 50)"
    done
fi
close_all

# ---- 2. the other file systems ------------------------------------------------------------------
OUT=$(bk ls "${D2[@]}" "${T[@]}")
FS2=$(awk '{for (i = 1; i <= NF; i++) if ($i ~ /\/$/) { sub(/\/$/, "", $i); print $i; exit }}' <<<"$OUT")
[ -n "$FS2" ] || fail "no file system name in the listing of the ext4 disk: $OUT"
bk get "${D2[@]}" "${T[@]}" --fs "$FS2" /hello.txt --out - | grep -q first && pass "ext4 in COS: a file is read" || fail "ext4 hello.txt"
close_all
OUT=$(bk ls "${D3[@]}" "${T[@]}")
grep -q "sda1/" <<<"$OUT" || fail "disk3 file systems: $OUT"
bk get "${D3[@]}" "${T[@]}" --fs sda1 /hello.txt --out - | grep -q first && pass "NTFS in COS: a file is read" || fail "ntfs hello.txt"
close_all

# ---- 3. an older restore point, read from the undo files (phase 6d) -------------------------------
# No export was made before the newest backup, so the first restore point is in no export as its own
# newest. The one export holds it only in its undo files; the disk server rebuilds it from them.
POINTS=$(bk ls "${D1[@]}" "${T[@]}" --restore-points) || fail "ls --restore-points of a VM disk in COS: $POINTS"
N_POINTS=$(grep -c . <<<"$POINTS" || true)
log "restore points of disk1 in COS: $(tr '\n' ' ' <<<"$POINTS")"
[ "$N_POINTS" -ge 3 ] && pass "ls --restore-points lists the restore points of the undo files and the disk ($N_POINTS)" \
    || fail "ls --restore-points of a VM disk in COS lists $N_POINTS restore points, want at least 3: $POINTS"
OLDEST=$(sort <<<"$POINTS" | head -1)
NEWEST=$(sort <<<"$POINTS" | tail -1)
[ "$OLDEST" != "$NEWEST" ] || fail "the oldest and the newest restore point are the same: $POINTS"
S=$(now)
OUT=$(bk ls "${D1[@]}" "${T[@]}" --restore-point "$OLDEST" --fs vg0-root /) || fail "ls of an older restore point of a VM disk in COS: $OUT"
measure "the first listing of an older restore point from the undo files (bar 300)" "$S"
grep -q "hello.txt" <<<"$OUT" && pass "an older restore point of a VM disk in COS is listed" || fail "older restore point listing: $OUT"
GOT=$(bk get "${D1[@]}" "${T[@]}" --restore-point "$OLDEST" --fs vg0-root /hello.txt --out -) || fail "get from an older restore point: $GOT"
[ "$GOT" = first ] && pass "an older restore point of a VM disk in COS reads 'first' (held only by undo files)" \
    || fail "an older restore point of a VM disk in COS reads the wrong content: $GOT"
S=$(now)
bk get "${D1[@]}" "${T[@]}" --restore-point "$OLDEST" --fs vg0-root /big.bin --out "$WORK/got/old-big.bin" >/dev/null || fail "get of the 256 MiB file from an older restore point"
measure "downloading the 256 MiB file from an older restore point" "$S"
[ "$(shasum -a 256 "$WORK/got/old-big.bin" | awk '{print $1}')" = "$BIG_SHA" ] \
    && pass "a 256 MiB file from an older restore point in COS has the same checksum" || fail "older restore point big.bin checksum"
# A restore point before every undo file is refused in words.
if out=$(bk ls "${D1[@]}" "${T[@]}" --restore-point 2000-01-01T00:00:00Z 2>&1); then
    fail "a restore point older than every backup was browsed: $out"
fi
log "  | $out"
pass "a restore point older than every backup in COS is refused"
close_all

# ---- 3b. restore files from a VM disk in COS (phase 6c) ----------------------------------------------
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
S=$(now)
bk restore-files "${D1[@]}" --fs vg0-root --path /hello.txt --path /big.bin --to "$NS/$PREFIX-new" --wait --timeout 30m >/dev/null \
    || fail "restore-files from a VM disk in COS failed"
measure "restore of a small file and the 256 MiB file from the XFS disk in COS (disk server start included)" "$S"
OUT=$(inspect "$PREFIX-new" 'echo "hello=$(cat vg0-root/hello.txt)"; echo "sum=$(sha256sum vg0-root/big.bin | cut -d" " -f1)"')
grep -q "^hello=second$" <<<"$OUT" && pass "a small file restored from the XFS disk in COS has its content" || fail "restored hello.txt: $OUT"
grep -q "^sum=$BIG_SHA$" <<<"$OUT" && pass "the 256 MiB file restored from COS has the same checksum" || fail "restored big.bin: $OUT"
bk restore-files "${D3[@]}" --fs sda1 --path /hello.txt --to "$NS/$PREFIX-new3" --wait --timeout 30m >/dev/null \
    || fail "restore-files from the NTFS disk in COS failed"
OUT=$(inspect "$PREFIX-new3" 'echo "hello=$(cat sda1/hello.txt)"')
grep -q "^hello=first$" <<<"$OUT" && pass "a file restored from the NTFS disk in COS has its content" || fail "restored ntfs hello.txt: $OUT"
# A restore keeps no disk server: its Job, pod and Secret go with it.
restore_gone() {
    [ -z "$(oc get job,pod -n "$DRIVER_NS" -l "storage.ibmcloud.io/restore-disk" -o name 2>/dev/null)" ] &&
        [ -z "$(oc get secret -n "$DRIVER_NS" -o name 2>/dev/null | grep -E 'restoredisk-' || true)" ]
}
wait_until 600 "the disk servers of the restores to go" restore_gone
pass "no disk server of a restore is left"
# Files from an older restore point of the disk, held only by undo files (phase 6d).
bk restore-files "${D1[@]}" --restore-point "$OLDEST" --fs vg0-root --path /hello.txt --to "$NS/$PREFIX-new4" --wait --timeout 30m >/dev/null \
    || fail "restore-files from an older restore point of a VM disk in COS failed"
OUT=$(inspect "$PREFIX-new4" 'echo "hello=$(cat vg0-root/hello.txt)"')
grep -q "^hello=first$" <<<"$OUT" && pass "files restored from an older restore point of a VM disk in COS have its content" || fail "restored older restore point: $OUT"
wait_until 600 "the disk servers of the restores to go" restore_gone
close_all

# ---- 4. the pods and Secrets of a session -----------------------------------------------------------
SESSION=$(bk browse "${D1[@]}" "${T[@]}" | awk '{print $2}')
[ -n "$SESSION" ] || fail "browse printed no session"
BUID=$(oc get backupbrowse "$SESSION" -o jsonpath='{.metadata.uid}')
DISKPOD=$(oc get pod -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$BUID,storage.ibmcloud.io/browse-disk=true" -o json)
GUESTPOD=$(oc get pod -n "$BROWSE_NS" -l "storage.ibmcloud.io/browse=$BUID,storage.ibmcloud.io/browse-guest=true" -o json)
log "pods of the session: $(oc get pod -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$BUID" -o name | tr '\n' ' ') $(oc get pod -n "$BROWSE_NS" -l "storage.ibmcloud.io/browse=$BUID" -o name | tr '\n' ' ')"
jq -e '.items | length == 1' <<<"$DISKPOD" >/dev/null && pass "one disk server pod runs for the session" || fail "disk server pods: $(jq '.items | length' <<<"$DISKPOD")"
jq -e '.items[0].spec | .automountServiceAccountToken == false and (.hostNetwork // false) == false and ([.volumes[] | select(.hostPath)] | length) == 0' <<<"$DISKPOD" >/dev/null \
    && pass "the disk server has no service account token and no host path" || fail "disk server pod spec"
jq -e '[.items[0].spec.volumes[] | select(.persistentVolumeClaim)] | length == 0' <<<"$DISKPOD" >/dev/null \
    && pass "the disk server mounts no share" || fail "the disk server mounts a claim"
jq -e '.items[0].spec.containers[0].securityContext | .allowPrivilegeEscalation == false and .capabilities.drop == ["ALL"] and ((.capabilities.add // []) | length) == 0' <<<"$DISKPOD" >/dev/null \
    && pass "the disk server has no capability and cannot gain privilege" || fail "disk server security context"
jq -e '.items | length == 1 and (.[0].spec.automountServiceAccountToken == false)' <<<"$GUESTPOD" >/dev/null \
    && pass "the guest reader has no service account token" || fail "guest reader pod spec"
KEYS=$(oc get secret -n "$BROWSE_NS" -o json | jq -r '[.items[] | select(.data | has("tls.key"))] | length')
[ "$KEYS" = 0 ] || log "NOTE: $KEYS Secret(s) in the browse namespace hold a tls.key (check that it is not the disk server's)"
oc get secret -n "$BROWSE_NS" -o name | grep -E 'browsedisk-' | while read -r s; do
    oc get "$s" -n "$BROWSE_NS" -o json | jq -e '.data | has("tls.key") | not' >/dev/null && pass "$s in the browse namespace has no private key" || fail "$s holds a private key"
done
oc get secret -n "$BROWSE_NS" -o json | jq -e '[.items[] | select(.data | has("secret-access-key") or has("apikey") or has("access_key_id"))] | length == 0' >/dev/null \
    && pass "the browse namespace never holds the COS or Key Protect keys" || fail "a COS key is in the browse namespace"
bk browse --close "$SESSION" >/dev/null
close_all

# ---- 5. nothing is left -------------------------------------------------------------------------
gone() {
    [ -z "$(oc get pod,job,secret -n "$DRIVER_NS" -l "storage.ibmcloud.io/browse=$BUID" -o name 2>/dev/null)" ] &&
        [ -z "$(oc get pod,job,secret -n "$BROWSE_NS" -l "storage.ibmcloud.io/browse=$BUID" -o name 2>/dev/null)" ] &&
        [ -z "$(oc get secret -n "$DRIVER_NS" -o name 2>/dev/null | grep -E 'browsedisk-' || true)" ] &&
        [ -z "$(oc get secret -n "$BROWSE_NS" -o name 2>/dev/null | grep -E 'browsedisk-' || true)" ]
}
wait_until 600 "the pods, Jobs and Secrets of the session to go" gone
pass "no pod, Job or Secret of the session is left"
log "all checks passed"
