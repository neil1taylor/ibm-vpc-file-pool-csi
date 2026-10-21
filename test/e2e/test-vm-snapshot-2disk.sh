#!/usr/bin/env bash
# End-to-end test: a KubeVirt VirtualMachineSnapshot of a running VM with two
# disks on the pool, restored to a new VM. Issue #14.
#
# KubeVirt makes one VolumeSnapshot per disk. It never calls the group snapshot
# API. So the question is whether the two disk copies fit each other.
#
# The VM runs a writer. For each number n, the writer writes the file f<n> on the
# second disk (/mnt/data), waits for it to reach the disk, and only then logs
# "<n> <sha256>" in /var/log/writer.log on the first disk. If a copy of the first
# disk is later than the copy of the second disk, the log names a file that the
# second disk does not have. The restored VM must show:
#   - both VolumeSnapshots were made with the QEMU copy
#   - every file named in the log is on the second disk with the logged checksum
#   - the kernel log has no XFS errors
# The test prints the gap between the two copy moments.
#
# Usage: KUBECONFIG=... SSH_KEY=~/.ssh/key test/e2e/test-vm-snapshot-2disk.sh [--keep]
# SSH_KEY is a private key whose .pub file is put in the VM for virtctl ssh.
set -euo pipefail

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-vm2disk}
NS=${NS:-$PREFIX-test}
POOL=$PREFIX-pool
CLASS=$PREFIX-class
SSH_KEY=${SSH_KEY:?set SSH_KEY to a private key file}
IMAGE_URL=${IMAGE_URL:-https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2}
SNAPSHOT_TIMEOUT=${SNAPSHOT_TIMEOUT:-3600}
# Speed limit of the snapshot copy in MB/s; the default 20 would take about 20 minutes.
SNAPSHOT_SPEED=${SNAPSHOT_SPEED:-100}

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
    oc delete ns "$NS" --ignore-not-found --wait=true --timeout=15m || true
    oc delete volumesnapshotclass "$CLASS" --ignore-not-found
    oc delete storageclass "$POOL" --ignore-not-found
    oc delete filesharepool "$POOL" --ignore-not-found --wait=false
}
trap cleanup EXIT

zone() {
    oc get nodes -o jsonpath='{range .items[*]}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' |
        sort | uniq -c | sort -rn | awk 'NR==1{print $2}'
}

log "making pool $POOL"
oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $POOL
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
pool_ready() { [ "$(oc get filesharepool "$POOL" -o jsonpath='{.status.phase}' 2>/dev/null)" = Ready ]; }
wait_until 1200 "the pool Ready" pool_ready

oc create ns "$NS" --dry-run=client -o yaml | oc apply -f -
oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null

# KubeVirt picks the VolumeSnapshotClass of the driver. It must be the default one.
oc apply -f - <<YAML
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: $CLASS
  annotations: {snapshot.storage.kubernetes.io/is-default-class: "true"}
driver: vpc-file-pool.csi.ibm.io
deletionPolicy: Delete
parameters:
  vmCopySpeedLimitMBps: "$SNAPSHOT_SPEED"
YAML

log "making the two disks"
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: root}
spec:
  storageClassName: $POOL
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 20Gi}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data}
spec:
  storageClassName: $POOL
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 6Gi}}
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
          qemu-img convert -f qcow2 -O raw /tmp/i.qcow2 /root-disk/disk.img
          qemu-img resize -f raw /root-disk/disk.img 20G
          chown 107:107 /root-disk/disk.img
          truncate -s 5G /data-disk/disk.img
          chown 107:107 /data-disk/disk.img
        resources: {limits: {ephemeral-storage: 4Gi, memory: 2Gi}, requests: {memory: 1Gi}}
        volumeMounts:
        - {name: r, mountPath: /root-disk}
        - {name: d, mountPath: /data-disk}
      volumes:
      - {name: r, persistentVolumeClaim: {claimName: root}}
      - {name: d, persistentVolumeClaim: {claimName: data}}
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
            disk_setup:
              /dev/vdb: {table_type: gpt, layout: false, overwrite: false}
            fs_setup:
            - {label: data, filesystem: xfs, device: /dev/vdb}
            mounts:
            - [LABEL=data, /mnt/data, xfs, "defaults,nofail"]
            write_files:
            - path: /usr/local/bin/writer
              permissions: "0755"
              content: |
                #!/bin/bash
                # Writes /mnt/data/f<n> (disk 2), then logs "<n> <sha256> <time>" in
                # /var/log/writer.log (disk 1). The log is always behind the data.
                until mountpoint -q /mnt/data; do sleep 1; done
                n=\$(wc -l < /var/log/writer.log 2>/dev/null || echo 0)
                while :; do
                  n=\$((n+1))
                  head -c 65536 /dev/urandom > /mnt/data/f\$n
                  sync /mnt/data/f\$n
                  echo "\$n \$(sha256sum /mnt/data/f\$n | cut -d' ' -f1) \$(date +%s.%N)" >> /var/log/writer.log
                  sync /var/log/writer.log
                  sleep 0.1
                done
            - path: /etc/systemd/system/writer.service
              content: |
                [Service]
                ExecStart=/usr/local/bin/writer
                [Install]
                WantedBy=multi-user.target
            runcmd:
              - systemctl daemon-reload
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
wait_until 600 "writer running" writer_running
before=$(vssh src 'wc -l < /var/log/writer.log' | tr -dc '0-9')

log "taking a VirtualMachineSnapshot of the running VM"
start=$SECONDS
oc apply -n "$NS" -f - <<YAML
apiVersion: snapshot.kubevirt.io/v1beta1
kind: VirtualMachineSnapshot
metadata: {name: vmsnap}
spec:
  source: {apiGroup: kubevirt.io, kind: VirtualMachine, name: src}
YAML
snapshot_done() {
    local p
    p=$(oc get vmsnapshot -n "$NS" vmsnap -o jsonpath='{.status.phase}' 2>/dev/null)
    [ "$p" = Failed ] && fail "the VirtualMachineSnapshot failed: $(oc get vmsnapshot -n "$NS" vmsnap -o jsonpath='{.status.conditions}')"
    [ "$p" = Succeeded ]
}
wait_until "$SNAPSHOT_TIMEOUT" "the VirtualMachineSnapshot done" snapshot_done
log "the snapshot took $((SECONDS - start)) s"

# One Snapshot resource of the driver per disk: check the method and the moment.
moments=()
vsc=$(oc get vmsnapshot -n "$NS" vmsnap -o jsonpath='{.status.virtualMachineSnapshotContentName}')
for vs in $(oc get vmsnapshotcontent -n "$NS" "$vsc" -o json | jq -r '.spec.volumeBackups[].status.volumeSnapshotName'); do
    content=$(oc get volumesnapshot -n "$NS" "$vs" -o jsonpath='{.status.boundVolumeSnapshotContentName}')
    handle=$(oc get volumesnapshotcontent "$content" -o jsonpath='{.status.snapshotHandle}')
    st=$(oc get snapshots.storage.ibmcloud.io "${handle##*/}" -o json)
    echo "$vs: $(echo "$st" | jq -c '{method: .metadata.annotations["storage.ibmcloud.io/snapshot-method"], consistency: .status.consistency, moment: .status.copyMoment}')"
    [ "$(echo "$st" | jq -r '.metadata.annotations["storage.ibmcloud.io/snapshot-method"]')" = vm ] || fail "$vs was not made with the QEMU copy"
    moments+=("$(echo "$st" | jq -r '.status.copyMoment')")
done
[ "${#moments[@]}" = 2 ] || fail "expected 2 disk snapshots, found ${#moments[@]}"
to_epoch() { perl -MTime::Piece -e 'print Time::Piece->strptime(shift, "%Y-%m-%dT%H:%M:%SZ")->epoch' "$1"; }
gap=$(( $(to_epoch "${moments[1]}") - $(to_epoch "${moments[0]}") ))
log "the two copy moments are ${gap#-} s apart (${moments[0]} and ${moments[1]})"

after=$(vssh src 'wc -l < /var/log/writer.log' | tr -dc '0-9')
log "the source VM wrote $((after - before)) files while the snapshot ran"
[ $((after - before)) -gt 0 ] || fail "the source VM stopped writing during the snapshot"

log "restoring the snapshot to a new VM and booting it"
oc apply -n "$NS" -f - <<YAML
apiVersion: snapshot.kubevirt.io/v1beta1
kind: VirtualMachineRestore
metadata: {name: vmrestore}
spec:
  target: {apiGroup: kubevirt.io, kind: VirtualMachine, name: copy}
  virtualMachineSnapshotName: vmsnap
YAML
restore_done() { [ "$(oc get vmrestore -n "$NS" vmrestore -o jsonpath='{.status.complete}' 2>/dev/null)" = true ]; }
wait_until 2400 "the restore complete" restore_done
wait_until 2400 "restored guest agent" agent_connected copy
guest_ready() { vssh copy true >/dev/null 2>&1; }
wait_until 1500 "the restored VM to answer" guest_ready

xfs=$(vssh copy 'sudo journalctl -k --no-pager | grep -ci "xfs.*\(corrupt\|error\)" || true')
[ "$xfs" = 0 ] || fail "XFS errors in the kernel log of the restored VM: $xfs"

# The writer in the copy starts again when the copy boots; stop it, then check
# that every logged file is on disk 2 with the logged checksum.
result=$(vssh copy 'sudo systemctl stop writer; total=$(wc -l < /var/log/writer.log); missing=0; wrong=0
while read n s t; do
  if [ ! -f /mnt/data/f$n ]; then missing=$((missing+1)); elif [ "$(sha256sum /mnt/data/f$n | cut -d" " -f1)" != "$s" ]; then wrong=$((wrong+1)); fi
done < /var/log/writer.log
echo "LOGGED=$total MISSING=$missing WRONG=$wrong"')
echo "$result"
echo "$result" | grep -q 'MISSING=0 WRONG=0' ||
    fail "the two disks of the restored VM do not fit each other (copy moments ${gap#-} s apart): $result"
log "PASS: restored 2-disk VM booted; $result; copy moments ${gap#-} s apart"
