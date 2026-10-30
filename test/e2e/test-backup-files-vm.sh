#!/usr/bin/env bash
# End-to-end test of browsing and restoring files inside a VM disk backup
# (issue #92, phase 5). Part 7 of release test stage 20.
#
# It needs the guest image (Dockerfile.browse-guest, installed with
# backupBrowse.guest.enabled) and says "NOT CHECKED" without it (GUEST_IMAGE is the
# image to build the test disks with, and the chart value must be on). It has not
# been run when it was written: the guest image could not be built where it was
# written. Expect to fix details of the libguestfs commands on the first run.
#
# What it does: makes three small VM disks as raw files on three claims, with no
# VM running (the VM object is stopped, so its disks are backed up as plain file
# copies, which is the path that needs no running QEMU):
#   disk1  GPT, an LVM volume group, an XFS logical volume   (guestfish)
#   disk2  one ext4 file system                              (virt-make-fs)
#   disk3  one NTFS file system, MBR partition               (virt-make-fs)
# Each holds hello.txt ("first"), a folder, a name with a newline and a 4 MiB file
# with a checksum. A policy backs them up; the content of hello.txt on disk1 is
# changed ("second") and the policy backs up again, so there are two moments.
# Then, as real users through the gateway:
# - `kubectl backup ls` with no --fs lists the file systems of each disk; --fs
#   reads inside one; the latest copy says "second" and the first moment "first",
#   for XFS on LVM, ext4 and NTFS; the checksum matches;
# - `restore-files --fs` restores a file and a folder into a new claim, and the file
#   system name is kept at the top of the restored tree;
# - while a session is open the SubVolume of the disk is held, and the hold ends
#   when the session is closed;
# - a user with only "view" is forbidden; the helper pod is locked down (no
#   service account token, no capability but none added, read-only mount of the
#   backup, no host path) and may ask for the KVM device only if the chart says so;
# - nothing is left: no helper pod, BackupBrowse or restore Job.
#
# Not covered here (covered by unit tests, or needs more): the undo-file path of a
# running VM (stage 18 backs those up; a run of this stage with RT_VM_POLICY set
# to such a policy and RT_VM_CLAIM could check it), BitLocker and LUKS (only the
# listing code is tested, with text), and the speed of large disks.
#
# Usage: KUBECONFIG=... GUEST_IMAGE=... [BACKUP_BIN=bin/kubectl-backup] test/e2e/test-backup-files-vm.sh [--keep]
set -euo pipefail

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-bv}
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

if [ -z "${GUEST_IMAGE:-}" ]; then
    log "NOT CHECKED: no guest image (GUEST_IMAGE), so browsing inside a VM disk was not tried"
    log "all checks passed"
    exit 0
fi
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
    # The gateway makes restores without the test's label.
    while read -r name; do
        [ -n "$name" ] && oc delete backupfilerestore "$name" --ignore-not-found --wait=false
    done < <(oc get backupfilerestore -o json | jq -r --arg p "$POLICY" '.items[] | select(.spec.policy == $p) | .metadata.name') || true
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

log "making pools $SRC (owner 107, as VM pools have) and $SNAP"
make_pool "$SRC" '  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"'
make_pool "$SNAP" ''
wait_until 1500 "pools Ready" all_ready

oc create ns "$NS"
oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null
# The guest image is in the private registry: copy the driver's pull secret here.
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
# A stopped VM that names the three claims makes the policy see them as VM disks.
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
BIG_SHA=""
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
    # Stop at once when the Job fails, not only at the time limit.
    local end=$((SECONDS + 1800)) ok="" failed=""
    while [ $SECONDS -lt $end ]; do
        ok=$(oc get "job/$1" -n "$NS" -o jsonpath='{.status.succeeded}' 2>/dev/null)
        failed=$(oc get "job/$1" -n "$NS" -o jsonpath='{.status.failed}' 2>/dev/null)
        [ -n "$ok" ] || [ -n "$failed" ] && break
        sleep 10
    done
    if [ -z "$ok" ]; then
        oc logs "job/$1" -n "$NS" --tail=40 >&2 || true
        oc get events -n "$NS" --field-selector involvedObject.kind=Pod --sort-by=.lastTimestamp 2>/dev/null | tail -5 >&2 || true
        fail "the disk Job $1 failed"
    fi
}
run_disk_job make-disks '
mkdir -p /tmp/t/docs
printf first > /tmp/t/hello.txt
printf one > /tmp/t/docs/a.txt
head -c 4194304 /dev/urandom > /tmp/t/big.bin
printf x > "/tmp/t/line
break"
sha256sum /tmp/t/big.bin | cut -d" " -f1 > /d1/big.sha
# disk1: GPT, a partition that is an LVM physical volume, an XFS logical volume.
guestfish <<EOF
disk-create /d1/disk.img raw 1G
add-drive /d1/disk.img format:raw
run
part-init /dev/sda gpt
part-add /dev/sda p 2048 -2048
pvcreate /dev/sda1
vgcreate vg0 /dev/sda1
lvcreate root vg0 600
mkfs xfs /dev/vg0/root
mount /dev/vg0/root /
copy-in /tmp/t/hello.txt /
copy-in /tmp/t/docs /
copy-in /tmp/t/big.bin /
EOF
# disk2: one ext4 file system; disk3: an NTFS file system in an MBR partition.
virt-make-fs --format=raw --type=ext4 /tmp/t /d2/disk.img
virt-make-fs --format=raw --type=ntfs --partition=mbr /tmp/t /d3/disk.img
# NFS maps root to nobody, so the chown to QEMU (107) may be refused; the files stay readable.
chmod 666 /d1/disk.img /d2/disk.img /d3/disk.img
chown 107:107 /d1/disk.img /d2/disk.img /d3/disk.img 2>/dev/null || true
'
BIG_SHA=$(oc exec -n "$NS" "$(oc get pod -n "$NS" -l job-name=make-disks -o name | head -1)" -- cat /d1/big.sha 2>/dev/null || true)
[ ${#BIG_SHA} -eq 64 ] || BIG_SHA=$(oc logs -n "$NS" job/make-disks | grep -Eo '^[0-9a-f]{64}$' | head -1 || true)

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
vol() { oc get backuppolicy "$POLICY" -o json | jq -r ".status.volumes[]? | select(.pvcName == \"$1\") | $2"; }
backed_up() { [ -n "$(vol disk1 '.lastBackupTime // empty')" ] && [ -n "$(vol disk2 '.lastBackupTime // empty')" ] && [ -n "$(vol disk3 '.lastBackupTime // empty')" ]; }
wait_until 1800 "the first backup of the three disks" backed_up
[ -n "$(vol disk1 '.vmName // empty')" ] && pass "the policy sees the three claims as disks of a VM" || fail "the volumes have no vmName"
FIRST=$(vol disk1 '.lastBackupTime')

# Change hello.txt on disk1 (the disk file changes, so the next copy has something to take).
run_disk_job change-disk '
guestfish --format=raw -a /d1/disk.img <<EOF
run
mount /dev/vg0/root /
write /hello.txt second
EOF
'
two_moments() { [ "$(vol disk1 '.restorePoints // 0')" -ge 1 ] && [ "$(vol disk1 '.lastBackupTime')" != "$FIRST" ]; }
wait_until 1800 "a second backup of disk1" two_moments
pass "two moments of disk1 are in the snapshot pool (first $FIRST)"


# ---- users --------------------------------------------------------------------------------------
GW_DEPLOY=$(oc get deploy -n "$DRIVER_NS" -l app.kubernetes.io/component=backup-gateway -o name 2>/dev/null | head -1)
if [ -z "$GW_DEPLOY" ]; then
    log "NOT CHECKED: the backup gateway is not installed (backupGateway.enabled)"
    log "all checks passed"
    exit 0
fi
oc rollout status "$GW_DEPLOY" -n "$DRIVER_NS" --timeout=300s >/dev/null || fail "the gateway is not ready"
for sa in alice viewer; do oc create sa "$sa" -n "$NS" >/dev/null; done
oc create rolebinding alice-admin -n "$NS" --clusterrole=admin --serviceaccount="$NS:alice" >/dev/null
oc create rolebinding viewer-view -n "$NS" --clusterrole=view --serviceaccount="$NS:viewer" >/dev/null
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
GW_ROUTE=$(oc get route -n "$DRIVER_NS" -l app.kubernetes.io/component=backup-gateway -o jsonpath='{.items[0].spec.host}' 2>/dev/null || true)
[ -n "$GW_ROUTE" ] || fail "the gateway has no Route"
GW_CA=()
[ -n "${GATEWAY_CA_FILE:-}" ] && GW_CA=(--gateway-ca-file "$GATEWAY_CA_FILE")
bk() { # bk <kubeconfig> <command> <args...>
    local kc=$1; shift
    "$BACKUP_BIN" "$1" --kubeconfig "$kc" --gateway "https://$GW_ROUTE" "${GW_CA[@]}" "${@:2}"
}
T=(--timeout 25m)
D1=(--policy "$POLICY" --pvc "$NS/disk1")
D2=(--policy "$POLICY" --pvc "$NS/disk2")
D3=(--policy "$POLICY" --pvc "$NS/disk3")
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

# ---- 1. the file systems of each disk, and files in them --------------------------------------
# Each disk has its own session, and a namespace may have 2 open (backupBrowse.maxSessionsPerNamespace):
# the sessions of one disk are closed before the next disk is opened.
OUT=$(bk "$ALICE" ls "${D1[@]}" "${T[@]}")
grep -q "vg0-root/" <<<"$OUT" && pass "ls lists the file systems of an LVM disk (the logical volume, not the physical volume)" || fail "disk1 file systems: $OUT"
! grep -q "sda1/" <<<"$OUT" || fail "the LVM physical volume is listed as a file system: $OUT"
close_all
OUT=$(bk "$ALICE" ls "${D2[@]}" "${T[@]}")
grep -q "/" <<<"$OUT" && pass "ls lists the file system of the ext4 disk" || fail "disk2 file systems: $OUT"
close_all
OUT=$(bk "$ALICE" ls "${D3[@]}" "${T[@]}")
grep -q "sda1/" <<<"$OUT" && pass "ls lists the NTFS file system of the third disk" || fail "disk3 file systems: $OUT"
close_all

mkdir "$WORK/got" "$WORK/got1"
bk "$ALICE" get "${D1[@]}" "${T[@]}" --fs vg0-root /hello.txt --out "$WORK/got" >/dev/null
[ "$(cat "$WORK/got/hello.txt")" = second ] && pass "XFS on LVM: the latest copy reads 'second'" || fail "hello.txt latest: $(cat "$WORK/got/hello.txt")"
bk "$ALICE" get "${D1[@]}" "${T[@]}" --moment "$FIRST" --fs vg0-root /hello.txt --out "$WORK/got1" >/dev/null
[ "$(cat "$WORK/got1/hello.txt")" = first ] && pass "XFS on LVM: the first moment reads 'first', without starting the VM" || fail "hello.txt first: $(cat "$WORK/got1/hello.txt")"
bk "$ALICE" get "${D1[@]}" "${T[@]}" --fs vg0-root /big.bin --out "$WORK/got" >/dev/null
if [ -n "$BIG_SHA" ]; then
    [ "$(sha256sum "$WORK/got/big.bin" | awk '{print $1}')" = "$BIG_SHA" ] && pass "a 4 MiB file from the XFS volume has the same checksum" || fail "big.bin checksum"
fi
close_all
FS2=$(bk "$ALICE" ls "${D2[@]}" "${T[@]}" | awk '{for (i = 1; i <= NF; i++) if ($i ~ /\/$/) { sub(/\/$/, "", $i); print $i; exit }}')
[ -n "$FS2" ] || fail "no file system name in the listing of the ext4 disk"
bk "$ALICE" get "${D2[@]}" "${T[@]}" --fs "$FS2" /hello.txt --out - | grep -q first \
    && pass "ext4: a file is read" || fail "ext4 hello.txt"
close_all
bk "$ALICE" get "${D3[@]}" "${T[@]}" --fs sda1 /hello.txt --out - | grep -q first \
    && pass "NTFS: a file is read (through virt-cat)" || fail "ntfs hello.txt"
close_all
# The ext4 disk holds the name with a newline (disk1 was made without it).
OUT=$(bk "$ALICE" ls "${D2[@]}" "${T[@]}" --fs "$FS2" /)
grep -qF 'line\x0abreak' <<<"$OUT" && pass "a name with a newline is listed escaped" || fail "newline name: $OUT"
grep -q "docs/" <<<"$OUT" && grep -q "hello.txt" <<<"$OUT" && pass "a folder is listed with its files and subfolders" || fail "listing of /: $OUT"
close_all

# ---- 2. the hold ------------------------------------------------------------------------------
SESSION=$(bk "$ALICE" browse "${D1[@]}" "${T[@]}" | awk '{print $2}')
[ -n "$SESSION" ] || fail "browse printed no session"
SVNAME=$(vol disk1 '.subVolumeName')
held() { oc get subvolume "$SVNAME" -o json | jq -e '.metadata.annotations["storage.ibmcloud.io/browse-hold"] // "" | length > 0' >/dev/null; }
held && pass "while a session is open, the disk is held (its next backup copy waits)" || fail "no browse-hold on the SubVolume"
bk "$ALICE" browse --close "$SESSION" >/dev/null
not_held() { ! held; }
wait_until 600 "the hold to end" not_held
pass "closing the session ends the hold"

# ---- 3. restore --------------------------------------------------------------------------------
bk "$ALICE" restore-files "${D1[@]}" --fs vg0-root --path /hello.txt --path /docs --to "$NS/$PREFIX-new" --wait --timeout 60m >/dev/null \
    || fail "restore-files from inside a VM disk failed"
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
OUT=$(inspect "$PREFIX-new" 'echo "hello=$(cat vg0-root/hello.txt)"; echo "a=$(cat vg0-root/docs/a.txt)"')
grep -q "^hello=second$" <<<"$OUT" && grep -q "^a=one$" <<<"$OUT" \
    && pass "a restore from inside a VM disk keeps the file system name at the top of the new claim" || fail "restored content: $OUT"
oc get backupfilerestore -o json | jq -e --arg p "$POLICY" --arg n "$PREFIX-new" '.items[] | select(.spec.policy == $p and .spec.targetName == $n) | .spec.guest == true' >/dev/null \
    && pass "the restore is a guest restore" || fail "the restore has no spec.guest"
not_held_after_restore() { ! held; }
wait_until 600 "the hold of the restore to end" not_held_after_restore
pass "the hold ends with the restore"

# ---- 4. the helper pod -------------------------------------------------------------------------
SESSION=$(bk "$ALICE" browse "${D1[@]}" "${T[@]}" | awk '{print $2}')
BUID=$(oc get backupbrowse "$SESSION" -o jsonpath='{.metadata.uid}')
POD=$(oc get pod -n "$BROWSE_NS" -l "storage.ibmcloud.io/browse=$BUID" -o json)
jq -e '.items[0].spec | .automountServiceAccountToken == false and (.hostNetwork // false) == false and ([.volumes[] | select(.hostPath)] | length) == 0' <<<"$POD" >/dev/null \
    && pass "the guest helper has no service account token and no host path" || fail "guest helper pod: $(jq -c '.items[0].spec' <<<"$POD")"
jq -e '.items[0].spec.containers[0].securityContext | .allowPrivilegeEscalation == false and .readOnlyRootFilesystem == true and .capabilities.drop == ["ALL"] and ((.capabilities.add // []) | length) == 0 and ((.privileged // false) == false)' <<<"$POD" >/dev/null \
    && pass "the guest helper has no capability, no privilege and a read-only root file system" || fail "guest helper security context"
jq -e '[.items[0].spec.containers[0].volumeMounts[] | select(.mountPath == "/latest") | .readOnly] | all' <<<"$POD" >/dev/null \
    && pass "the backup is mounted read-only" || fail "the backup mount is writable"
bk "$ALICE" browse --close "$SESSION" >/dev/null

# ---- 5. the wrong people ------------------------------------------------------------------------
if out=$(bk "$VIEWER" ls "${D1[@]}" 2>&1); then fail "a user with only the view role listed a VM disk: $out"; else grep -qi forbidden <<<"$out" && pass "a user with only the view role: forbidden" || fail "view-only answer: $out"; fi

# ---- 6. nothing is left -------------------------------------------------------------------------
close_all
gone() {
    [ -z "$(oc get pod,job -n "$BROWSE_NS" -o name 2>/dev/null | grep -E 'browse-' || true)" ]
}
wait_until 600 "the helper pods to go" gone
pass "no helper pod or Job of a session is left"
[ -z "$(oc get pod,job -n "$DRIVER_NS" -o name 2>/dev/null | grep -E 'filerestore' || true)" ] \
    && pass "no Job or pod of a restore is left" || fail "a restore Job or pod is left"
log "all checks passed"
