#!/usr/bin/env bash
# End-to-end test: replicate a running VM with two disks, then boot a copy.
#
# Makes two pools (source and destination) on the installed driver, a CentOS
# VM on the source pool that writes files nonstop, and a direct NFS
# ReplicationPolicy to the destination pool. When both disks have a good copy,
# it boots a second VM from the destination disks and checks:
#   - the copy VM boots and its guest agent connects
#   - the kernel log has no XFS errors
#   - every file in the writer's log (root disk) is on the data disk with the
#     logged checksum, so both disks show the same moment
#   - the policy status names the copy moment and its consistency
#
# Usage: KUBECONFIG=... SSH_KEY=~/.ssh/key test/e2e/test-vm-replication.sh [--keep]
# SSH_KEY is a private key whose .pub file is put in the VM for virtctl ssh.
set -euo pipefail

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-vm-step3c}
NS=${NS:-$PREFIX-test}
SRC=$PREFIX-src
DST=$PREFIX-dst
POLICY=$PREFIX-policy
SSH_KEY=${SSH_KEY:?set SSH_KEY to a private key file}
IMAGE_URL=${IMAGE_URL:-https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2}
COPY_TIMEOUT=${COPY_TIMEOUT:-3600}

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
fail() { log "FAIL: $*"; exit 1; }

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
    oc delete replicationpolicy "$POLICY" --ignore-not-found --wait=false
    oc delete ns "$NS" --ignore-not-found --wait=true --timeout=10m || true
    oc delete pv "$PREFIX-copy-root" "$PREFIX-copy-data" --ignore-not-found
    oc delete filesharepool "$SRC" "$DST" --ignore-not-found --wait=false
}
trap cleanup EXIT

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
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
YAML
}

pool_field() { oc get filesharepool "$1" -o jsonpath="{$2}" 2>/dev/null; }
pool_ready() { [ "$(pool_field "$1" .status.phase)" = "Ready" ]; }

log "making pools $SRC and $DST"
make_pool "$SRC"
make_pool "$DST"
pools_ready() { pool_ready "$SRC" && pool_ready "$DST"; }
wait_until 1200 "pools Ready" pools_ready

oc create ns "$NS" --dry-run=client -o yaml | oc apply -f -
oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null

log "making the source disks"
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: root}
spec:
  storageClassName: $SRC
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 20Gi}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data}
spec:
  storageClassName: $SRC
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 10Gi}}
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

log "starting the source VM"
vm_yaml src root data yes | oc apply -f -
wait_until 900 "source guest agent" agent_connected src
writer_running() { vssh src 'test -s /var/log/writer.log' >/dev/null 2>&1; }
wait_until 300 "writer running" writer_running

server=$(pool_field "$DST" '.status.shares[0].mountTargetIP')
export_path=$(pool_field "$DST" '.status.shares[0].exportPath')
log "making policy $POLICY to $server:$export_path"
oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: ReplicationPolicy
metadata: {name: $POLICY}
spec:
  sourcePoolName: $SRC
  destinationNFSServer: "$server"
  destinationExportPath: "$export_path"
  destinationBasePath: /repl
  schedule: 1h
  maxRetries: 3
  maxParallelSyncs: 2
YAML

copies_done() {
    local st
    st=$(oc get replicationpolicy "$POLICY" -o json)
    echo "$st" | jq -c '[.status.subVolumeStatuses[]? | {sv: .subVolumeName[0:12], phase: .vmCopy.phase, moment: .copyMoment, err: .lastError}]'
    [ "$(echo "$st" | jq '[.status.subVolumeStatuses[]? | select(.copyMoment != null and .vmCopy == null)] | length')" = 2 ]
}
log "waiting for both disks to have a good copy"
start=$SECONDS
wait_until "$COPY_TIMEOUT" "both disks copied" copies_done
log "copy took $((SECONDS - start))s"

status=$(oc get replicationpolicy "$POLICY" -o json)
moments=$(echo "$status" | jq -r '[.status.subVolumeStatuses[].copyMoment] | unique | length')
[ "$moments" = 1 ] || fail "the two disks show different moments"
moment=$(echo "$status" | jq -r '.status.subVolumeStatuses[0].copyMoment')
consistency=$(echo "$status" | jq -r '.status.subVolumeStatuses[0].consistency')
log "copy moment $moment, $consistency"
[ "$consistency" = FileSystemConsistent ] || log "WARNING: copy is $consistency: $(echo "$status" | jq -r '.status.subVolumeStatuses[0].consistencyReason')"

log "making far-site volumes"
for d in root data; do
    pv=$(oc get pvc -n "$NS" "$d" -o jsonpath='{.spec.volumeName}')
    sub=$(oc get subvolume "$pv" -o jsonpath='{.spec.subPath}')
    oc apply -f - <<YAML
apiVersion: v1
kind: PersistentVolume
metadata: {name: $PREFIX-copy-$d}
spec:
  capacity: {storage: $( [ $d = root ] && echo 20Gi || echo 10Gi )}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  mountOptions: [nfsvers=4.1, sec=sys, soft, timeo=600, retrans=3]
  nfs: {server: "$server", path: "${export_path%/}/repl$sub"}
  claimRef: {namespace: $NS, name: copy-$d}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: copy-$d, namespace: $NS}
spec:
  storageClassName: ""
  volumeName: $PREFIX-copy-$d
  accessModes: [ReadWriteMany]
  resources: {requests: {storage: $( [ $d = root ] && echo 20Gi || echo 10Gi )}}
YAML
done

log "booting the copy"
vm_yaml copy copy-root copy-data no | oc apply -f -
wait_until 900 "copy guest agent" agent_connected copy

log "checking the copy"
xfs=$(vssh copy 'sudo journalctl -k --no-pager | grep -ci "xfs.*\(corrupt\|error\)" || true')
[ "$xfs" = 0 ] || fail "XFS errors in the copy's kernel log: $xfs"
result=$(vssh copy 'sudo systemctl stop writer; awk -v m="$(date -d '"$moment"' +%s)" '"'"'$3 <= m {c++} END {print c+0}'"'"' /var/log/writer.log; while read n s t; do echo "$s  /data/f$n"; done < /var/log/writer.log | sha256sum -c --quiet && echo CHECKSUMS_OK')
echo "$result"
echo "$result" | grep -q CHECKSUMS_OK || fail "files on the data disk do not match the log on the root disk"
log "PASS: the copy booted; $(echo "$result" | head -1) files logged up to the copy moment, all checksums match"
