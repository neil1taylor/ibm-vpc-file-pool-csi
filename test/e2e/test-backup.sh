#!/usr/bin/env bash
# End-to-end test: back up a running VM and a pod volume to a snapshot pool,
# then restore them.
#
# Makes two pools (data and snapshot), a CentOS VM with a root disk and a data
# disk that write a file every 0.1 s, a pod volume with files, and a
# BackupPolicy that selects them by label. When there are two backups, it
# checks:
#   - the policy lists all three volumes, the VM disks as FileSystemConsistent
#     and the pod volume as FileCopy
#   - the VM disks restored at the first backup moment boot with no XFS errors,
#     and every file in the writer's log is on the data disk with its checksum
#   - the pod volume restored at the first moment holds the first content, and
#     restored at the latest it holds the changed content
#   - the pod volume can be restored after the claim is deleted
#
# With COS settings (see lib/backup-cos.sh) the policy also exports to a COS
# bucket (Compliance lock, 25 hours), and after the local checks it checks the
# exports, runs `kubectl backup cos-list`, and restores the three volumes from
# COS into a second namespace: the VM boots with matching checksums, and the pod
# volume holds the first and the changed content. With COS_DR_KUBECONFIG it
# repeats the restore in the other cluster of the lab pair. Objects locked by
# Compliance are left in the bucket, under a prefix that is new for each run.
#
# Usage: KUBECONFIG=... SSH_KEY=~/.ssh/key BACKUP_BIN=bin/kubectl-backup test/e2e/test-backup.sh [--keep]
# SSH_KEY is a private key whose .pub file is put in the VM for virtctl ssh.
# BACKUP_BIN is the kubectl-backup plugin (make build-backup).
set -euo pipefail

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-bk}
NS=${NS:-$PREFIX-test}
SRC=$PREFIX-src
SNAP=$PREFIX-snap
POLICY=$PREFIX-policy
SSH_KEY=${SSH_KEY:?set SSH_KEY to a private key file}
BACKUP_BIN=${BACKUP_BIN:-kubectl-backup}
IMAGE_URL=${IMAGE_URL:-https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2}
SCHEDULE=${SCHEDULE:-5m}
BACKUP_TIMEOUT=${BACKUP_TIMEOUT:-5400}
# A small pod volume is copied in the first few cycles; a longer wait means the copy fails.
FIRST_FILES_TIMEOUT=${FIRST_FILES_TIMEOUT:-1200}

HERE_E2E="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/backup-cos.sh
source "$HERE_E2E/lib/backup-cos.sh"

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
# Show why a backup did not happen, before cleanup deletes the evidence.
diagnose() {
    oc get backuppolicy "$POLICY" -o json 2>/dev/null | jq -c '.status // {} | {volumeCount, lastError, volumes: [.volumes[]? | {pvc: .pvcName, last: .lastBackupTime, points: .restorePoints, err: .lastError}]}' || true
    oc get replicationpolicy -o json 2>/dev/null | jq -c --arg p "backup-$POLICY" '.items[] | select(.metadata.name | startswith($p)) | {name: .metadata.name, phase: .status.phase, failures: .status.consecutiveFailures, err: .status.lastError, volumes: [.status.subVolumeStatuses[]? | {sv: .subVolumeName, err: .lastError}]}' || true
    # The controller deletes a failed copy Job at once, so its pods are usually gone; the errors above and the log below are the record.
    oc get jobs,pods -n kube-system -l storage.ibmcloud.io/repl-temp=true -o wide 2>/dev/null || true
    cos_diagnose
    oc logs -n kube-system deploy/ibm-vpc-file-pool-csi-controller -c csi-controller --since=2h 2>/dev/null | grep -i "$POLICY" | tail -20 || true
}
fail() { log "FAIL: $*"; diagnose; exit 1; }

wait_until() { # wait_until <seconds> <description> <command...>
    local t=$1 what=$2; shift 2
    local end=$((SECONDS + t))
    until "$@"; do
        [ $SECONDS -ge $end ] && fail "timed out after ${t}s waiting for $what"
        sleep 10
    done
}

cleanup() {
    if $KEEP; then log "keeping test resources (--keep)"; return; fi
    log "cleaning up"
    oc get backuprestore -o json | jq -r --arg ns "$NS" '.items[] | select(.spec.targetNamespace == $ns) | .metadata.name' |
        while read -r name; do oc delete backuprestore "$name" --wait=false || true; done
    oc delete backuppolicy "$POLICY" --ignore-not-found --wait=false || true
    cos_cleanup
    oc delete ns "$NS" --ignore-not-found --wait=true --timeout=15m || true
    oc delete filesharepool "$SRC" "$SNAP" --ignore-not-found --wait=false || true
}
trap cleanup EXIT
cos_init

zone() {
    oc get nodes -o jsonpath='{range .items[*]}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' |
        sort | uniq -c | sort -rn | awk 'NR==1{print $2}'
}

make_pool() {
    oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $1
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

log "making pools $SRC and $SNAP"
make_pool "$SRC"
make_pool "$SNAP"
both_ready() { pool_ready "$SRC" && pool_ready "$SNAP"; }
wait_until 1200 "pools Ready" both_ready

oc create ns "$NS" --dry-run=client -o yaml | oc apply -f -
oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null

log "making the source disks and the pod volume"
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: root, labels: {backup: "yes"}}
spec:
  storageClassName: $SRC
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 20Gi}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data, labels: {backup: "yes"}}
spec:
  storageClassName: $SRC
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 10Gi}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: files, labels: {backup: "yes"}}
spec:
  storageClassName: $SRC
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 1Gi}}
---
apiVersion: batch/v1
kind: Job
metadata: {name: convert}
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      securityContext: {runAsUser: 0}
      containers:
      - name: c
        image: quay.io/centos/centos:stream9
        command: [/bin/bash, -ec]
        args:
        - |
          dnf -y -q install qemu-img
          curl -sSfL -o /tmp/i.qcow2 "$IMAGE_URL"
          qemu-img convert -f qcow2 -O raw /tmp/i.qcow2 /disk/disk.img
          qemu-img resize -f raw /disk/disk.img 20G
          chown 107:107 /disk/disk.img
        resources: {limits: {ephemeral-storage: 4Gi, memory: 2Gi}, requests: {memory: 1Gi}}
        volumeMounts: [{name: d, mountPath: /disk}]
      volumes: [{name: d, persistentVolumeClaim: {claimName: root}}]
YAML
oc wait -n "$NS" job/convert --for=condition=complete --timeout=20m

PUBKEY=$(cat "$SSH_KEY.pub")
vm_yaml() { # vm_yaml <name> <root claim> <data claim> <with cloud-init: yes|no>
    cat <<YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: $1, namespace: $NS}
spec:
  runStrategy: Always
  template:
    spec:
      evictionStrategy: LiveMigrate
      domain:
        cpu: {cores: 2}
        memory: {guest: 2Gi}
        devices:
          disks:
          - {name: rootdisk, disk: {bus: virtio}}
          - {name: datadisk, disk: {bus: virtio}}
$( [ "$4" = yes ] && echo "          - {name: cloudinit, disk: {bus: virtio}}" )
          interfaces: [{name: default, masquerade: {}}]
      networks: [{name: default, pod: {}}]
      volumes:
      - {name: rootdisk, persistentVolumeClaim: {claimName: $2}}
      - {name: datadisk, persistentVolumeClaim: {claimName: $3}}
YAML
    [ "$4" = yes ] || return 0
    cat <<YAML
      - name: cloudinit
        cloudInitNoCloud:
          userData: |
            #cloud-config
            user: cloud-user
            ssh_authorized_keys: ["$PUBKEY"]
            write_files:
            - path: /usr/local/bin/writer
              permissions: "0755"
              content: |
                #!/bin/bash
                # Writes /data/f<n> and then logs "<n> <sha256> <time>" on the root disk.
                n=\$(wc -l < /var/log/writer.log 2>/dev/null || echo 0)
                while :; do
                  n=\$((n+1))
                  head -c 65536 /dev/urandom > /data/f\$n
                  sync /data/f\$n
                  echo "\$n \$(sha256sum /data/f\$n | cut -d' ' -f1) \$(date +%s.%N)" >> /var/log/writer.log
                  sync /var/log/writer.log
                  sleep 0.1
                done
            - path: /etc/systemd/system/writer.service
              content: |
                [Unit]
                After=data.mount
                Requires=data.mount
                [Service]
                ExecStart=/usr/local/bin/writer
                [Install]
                WantedBy=multi-user.target
            - path: /etc/systemd/system/data.mount
              content: |
                [Mount]
                What=/dev/vdb
                Where=/data
                Type=xfs
                [Install]
                WantedBy=multi-user.target
            runcmd:
              - mkfs.xfs -f /dev/vdb
              - mkdir -p /data
              - systemctl daemon-reload
              - systemctl enable --now data.mount
              - chcon -t mnt_t /data
              - systemctl enable --now writer.service qemu-guest-agent
YAML
}

agent_connected() {
    [ "$(oc get vmi -n "$NS" "$1" -o jsonpath='{.status.conditions[?(@.type=="AgentConnected")].status}' 2>/dev/null)" = "True" ]
}

vssh() { # vssh <vmi> <command>
    virtctl ssh -n "$NS" "cloud-user@vmi/$1" -i "$SSH_KEY" \
        -t "-o StrictHostKeyChecking=no" -t "-o UserKnownHostsFile=/dev/null" \
        -t "-o LogLevel=ERROR" -t "-o IdentitiesOnly=yes" -c "$2"
}

# files_pod <name> <claim> <script>: a pod that mounts the claim at /data and runs the script.
files_pod() {
    oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: Pod
metadata: {name: $1}
spec:
  securityContext: {runAsUser: 107, runAsGroup: 107}
  restartPolicy: Never
  containers:
  - name: c
    image: quay.io/centos/centos:stream9
    command: [/bin/bash, -ec, '$3']
    volumeMounts: [{name: v, mountPath: /data}]
  volumes: [{name: v, persistentVolumeClaim: {claimName: $2}}]
YAML
}
pod_running() { [ "$(oc get pod -n "$NS" "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ]; }
pod_succeeded() { [ "$(oc get pod -n "$NS" "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = Succeeded ]; }

log "starting the VM and writing the pod volume"
vm_yaml src root data yes | oc apply -f -
wait_until 900 "source guest agent" agent_connected src
writer_running() { vssh src 'test -s /var/log/writer.log' >/dev/null 2>&1; }
wait_until 300 "writer running" writer_running
files_pod files-writer files 'mkdir -p /data/a && for i in 1 2 3 4 5; do head -c 100000 /dev/urandom > /data/a/$i; done && (cd /data && find . -type f ! -name "manifest-*" | sort | xargs sha256sum > manifest-A.sha256) && touch /data/written && sleep infinity'
wait_until 300 "the writer pod running" pod_running files-writer
wait_until 300 "the pod volume written" oc exec -n "$NS" files-writer -- test -f /data/written

log "making the backup policy $POLICY (schedule $SCHEDULE)"
if $COS_ON; then
    cos_make_secrets "$DRIVER_NS" "$PREFIX" || fail "cannot make the COS Secrets in $DRIVER_NS"
fi
oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: BackupPolicy
metadata: {name: $POLICY}
spec:
  snapshotPool: $SNAP
  schedule: $SCHEDULE
  keepFor: 48h
  vmCopySpeedLimitMBps: 50
  namespaces: [$NS]
  selector: {matchLabels: {backup: "yes"}}
$(if $COS_ON; then cos_policy_export_yaml; fi)
YAML

# vol_field <claim> <jq expression on the volume entry>
vol_field() {
    oc get backuppolicy "$POLICY" -o json | jq -r --arg c "$1" ".status.volumes[]? | select(.pvcName == \$c) | $2"
}
files_backed_up() { [ "$(vol_field files '.lastBackupTime // empty')" != "" ]; }
log "waiting for the first backup of the pod volume"
wait_until "$FIRST_FILES_TIMEOUT" "the first backup of the pod volume" files_backed_up

# Change the pod volume after its first backup. The second backup holds this.
oc exec -n "$NS" files-writer -- bash -ec 'head -c 100000 /dev/urandom > /data/a/1; head -c 50000 /dev/urandom > /data/b; cd /data && find . -type f ! -name "manifest-*" | sort | xargs sha256sum > manifest-B.sha256'
log "changed the pod volume; waiting for two moments of every volume"
two_moments() {
    for c in root data files; do
        [ "$(vol_field $c '.restorePoints // 0')" -ge 1 ] || return 1
    done
}
# restorePoints counts the moments before the newest, for VM disks and pod volumes alike.
wait_until "$BACKUP_TIMEOUT" "a second backup of every volume" two_moments

status=$(oc get backuppolicy "$POLICY" -o json)
echo "$status" | jq -c '.status.volumes[] | {pvc: .pvcName, vm: .vmName, last: .lastBackupTime, oldest: .oldestBackupTime, points: .restorePoints, consistency: .consistency, err: .lastError}'
[ "$(echo "$status" | jq '.status.volumeCount')" = 3 ] || fail "the policy backs up $(echo "$status" | jq '.status.volumeCount') volumes, want 3"
[ "$(vol_field root .consistency)" = FileSystemConsistent ] || fail "the root disk backup is not FileSystemConsistent"
[ "$(vol_field data .consistency)" = FileSystemConsistent ] || fail "the data disk backup is not FileSystemConsistent"
[ "$(vol_field files .consistency)" = FileCopy ] || fail "the pod volume backup is not FileCopy"
log "PASS: three volumes backed up, two moments each"
# The volume names are how COS keeps backups. Save them before the claim "files" is deleted.
export COS_PV_ROOT COS_PV_DATA COS_PV_FILES
COS_PV_ROOT=$(oc get pvc -n "$NS" root -o jsonpath='{.spec.volumeName}')
COS_PV_DATA=$(oc get pvc -n "$NS" data -o jsonpath='{.spec.volumeName}')
COS_PV_FILES=$(oc get pvc -n "$NS" files -o jsonpath='{.spec.volumeName}')

first_vm=$(vol_field root .oldestBackupTime)
first_files=$(vol_field files .oldestBackupTime)
log "oldest moments: VM disks $first_vm, pod volume $first_files"

restore() { # restore <pvc> <to> [moment]
    local moment=()
    [ -z "${3:-}" ] || moment=(--moment "$3")
    "$BACKUP_BIN" restore --policy "$POLICY" --pvc "$NS/$1" --to "$NS/$2" ${moment[@]+"${moment[@]}"} --wait --timeout 1h
}

log "restoring the VM disks at $first_vm"
restore root root-r1 "$first_vm"
restore data data-r1 "$first_vm"
vm_yaml restored root-r1 data-r1 no | oc apply -f -
wait_until 900 "restored guest agent" agent_connected restored
ssh_ok() { vssh restored true >/dev/null 2>&1; }
wait_until 300 "ssh on the restored VM" ssh_ok
xfs=$(vssh restored 'sudo journalctl -k --no-pager | grep -ci "xfs.*\(corrupt\|error\)" || true')
[ "$xfs" = 0 ] || fail "XFS errors in the kernel log of the restored VM: $xfs"
result=$(vssh restored 'sudo systemctl stop writer; wc -l < /var/log/writer.log; while read n s t; do echo "$s  /data/f$n"; done < /var/log/writer.log | sha256sum -c --quiet && echo CHECKSUMS_OK')
echo "$result"
echo "$result" | grep -q CHECKSUMS_OK || fail "files on the restored data disk do not match the log on the restored root disk"
log "PASS: the VM restored at $first_vm booted; $(echo "$result" | head -1) files logged, all checksums match"

# check_files <pod> <claim> <manifest> <must be absent file or empty>: restore check on the pod volume.
check_files() {
    files_pod "$1" "$2" "cd /data && sha256sum -c $3 && $4"
    wait_until 600 "pod $1 to end" pod_succeeded "$1" || true
    oc logs -n "$NS" "$1" || true
    pod_succeeded "$1" || fail "the restored pod volume $2 does not match $3"
}

log "restoring the pod volume at $first_files (the first content)"
restore files files-r1 "$first_files"
check_files check-r1 files-r1 manifest-A.sha256 'test ! -e /data/b'
log "PASS: the pod volume restored at $first_files holds the first content"

log "restoring the pod volume at the latest moment (the changed content)"
restore files files-r2
check_files check-r2 files-r2 manifest-B.sha256 'test -e /data/b'
log "PASS: the latest restore holds the changed content"

log "deleting the pod volume and restoring it from the backup"
oc delete pod -n "$NS" files-writer check-r1 check-r2 --ignore-not-found --wait=true >/dev/null
oc delete pvc -n "$NS" files --wait=true --timeout=10m
restore files files-gone
check_files check-gone files-gone manifest-B.sha256 'test -e /data/b'
log "PASS: a volume that was deleted can be restored from its backup"

if $COS_ON; then
    cos_phase
fi
