#!/usr/bin/env bash
# End-to-end test: instant snapshots of a running VM's disks (issue #14,
# docs/specs/2026-10-21-instant-vm-snapshots.md). Needs the KubeVirt Sidecar
# feature gate on (hack/sidecar-gate.sh).
#
# The VM has two disks and the writer of test-vm-snapshot-2disk.sh: it writes
# f<n> on the data disk, then logs "<n> <sha256>" on the root disk. The test
# logs one "PASS: ..." line per outcome, or one "FAIL: ..." line and exit 1:
#   - a KubeVirt VirtualMachineSnapshot makes instant, FileSystemConsistent
#     disk snapshots, and the VM restored from it has every logged file with
#     its checksum on the data disk and no XFS errors ("restored VM")
#   - a snapshot of a running VM's blank 100 GB disk is ready in under 10 s
#     ("100 GB disk")
#   - after three snapshots, a live migration right after the last one and a
#     restart, the VM opens the newest file of the chain and has every marker
#     file ("migration and restart")
#   - deleting the middle, the oldest and the newest snapshot merges the chain
#     back to disk.img, and after a restart every marker file is there
#     ("deleting snapshots")
#   - a snapshot and its delete on a stopped VM run as Jobs, and the VM starts
#     on the right file each time ("stopped VM")
# Not covered here (see the spec): the console's Snapshots tab, Windows,
# removing the hook, and the copy fallback with the gate off.
#
# Usage: KUBECONFIG=... SSH_KEY=~/.ssh/key test/e2e/test-vm-instant-snapshot.sh [--keep]
# SSH_KEY is a private key whose .pub file is put in the VM for virtctl ssh.
set -euo pipefail

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-vminstant}
NS=${NS:-$PREFIX-test}
POOL=$PREFIX-pool
CLASS=$PREFIX-class
SSH_KEY=${SSH_KEY:?set SSH_KEY to a private key file}
IMAGE_URL=${IMAGE_URL:-https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2}
# The longest a snapshot or delete may take when a Job does it (stopped VM).
JOB_TIMEOUT=${JOB_TIMEOUT:-900}

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
fail() { log "FAIL: $*"; exit 1; }
pass() { log "PASS: $*"; }

wait_until() { # wait_until <seconds> <description> <command...>
    local t=$1 what=$2; shift 2
    local end=$((SECONDS + t))
    until "$@"; do
        [ $SECONDS -ge $end ] && fail "timed out after ${t}s waiting for $what"
        sleep 5
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

gates=$(oc get kubevirt -A -o jsonpath='{.items[0].spec.configuration.developerConfiguration.featureGates}')
echo "$gates" | grep -q '"Sidecar"' || fail "the KubeVirt Sidecar feature gate is off; turn it on with hack/sidecar-gate.sh"

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
  shareSizeGB: 300
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
YAML

log "making the disks: root, data, and a blank 100 GB disk"
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
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: big}
spec:
  storageClassName: $POOL
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 100Gi}}
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
          truncate -s 99G /big-disk/disk.img
          chown 107:107 /big-disk/disk.img
        resources: {limits: {ephemeral-storage: 4Gi, memory: 2Gi}, requests: {memory: 1Gi}}
        volumeMounts:
        - {name: r, mountPath: /root-disk}
        - {name: d, mountPath: /data-disk}
        - {name: b, mountPath: /big-disk}
      volumes:
      - {name: r, persistentVolumeClaim: {claimName: root}}
      - {name: d, persistentVolumeClaim: {claimName: data}}
      - {name: b, persistentVolumeClaim: {claimName: big}}
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
            - [LABEL=data, /mnt/data, xfs, "defaults,nofail,context=system_u:object_r:mnt_t:s0"]
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

# --- helpers -----------------------------------------------------------------

# chain <claim>: the disk chain of a claim's volume, newest first, or "disk.img".
chain() {
    local pv c
    pv=$(oc get pvc -n "$NS" "$1" -o jsonpath='{.spec.volumeName}')
    c=$(oc get subvolume "$pv" -o json | jq -r '(.status.diskChain // []) | join(" ")')
    echo "${c:-disk.img}"
}
top_of() { chain "$1" | cut -d' ' -f1; }
# words <text>: how many words. (wc pads its count with spaces on macOS.)
words() { set -f; set -- $1; set +f; echo $#; }

# open_file <vm> <volume>: the file QEMU has open for the volume, by name.
open_file() {
    local pod
    pod=$(oc get pod -n "$NS" -l "kubevirt.io=virt-launcher,vm.kubevirt.io/name=$1" \
        --field-selector=status.phase=Running -o jsonpath='{.items[0].metadata.name}')
    oc exec -n "$NS" "$pod" -c compute -- virsh domblklist "${NS}_$1" |
        awk -v v="/vmi-disks/$2/" 'index($2, v) {n=split($2, p, "/"); print p[n]}'
}

# expect_open <vm> <volume> <claim>: QEMU has the chain's top file open.
expect_open() {
    local want got
    want=$(top_of "$3"); got=$(open_file "$1" "$2")
    [ "$got" = "$want" ] || fail "$1 has $got open for $2, but the chain of $3 is: $(chain "$3")"
}

# snapshot_status <volumesnapshot>: "<method> <consistency>" of the driver's Snapshot.
snapshot_status() {
    local content handle
    content=$(oc get volumesnapshot -n "$NS" "$1" -o jsonpath='{.status.boundVolumeSnapshotContentName}')
    handle=$(oc get volumesnapshotcontent "$content" -o jsonpath='{.status.snapshotHandle}')
    oc get snapshots.storage.ibmcloud.io "${handle##*/}" -o json |
        jq -r '"\(.metadata.annotations["storage.ibmcloud.io/snapshot-method"]) \(.status.consistency)"'
}

snap_ready() { [ "$(oc get volumesnapshot -n "$NS" "$1" -o jsonpath='{.status.readyToUse}' 2>/dev/null)" = true ]; }

# take <name> <claim>: make a VolumeSnapshot of the claim.
take() {
    oc apply -n "$NS" -f - <<YAML >/dev/null
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: $1}
spec:
  volumeSnapshotClassName: $CLASS
  source: {persistentVolumeClaimName: $2}
YAML
}

# drop <name> <timeout>: delete a VolumeSnapshot and wait until it is gone.
drop() {
    oc delete volumesnapshot -n "$NS" "$1" --wait=false >/dev/null
    oc wait -n "$NS" "volumesnapshot/$1" --for=delete --timeout="${2}s" >/dev/null 2>&1 ||
        fail "the delete of snapshot $1 did not finish in $2 s; chain of root: $(chain root)"
}

marker() { vssh src "echo $1 | sudo tee /var/tmp/$1 >/dev/null && sync" >/dev/null; }
markers_present() { # markers_present <names...>
    local m out
    out=$(vssh src "for m in $*; do test -f /var/tmp/\$m && echo ok-\$m; done")
    for m in "$@"; do echo "$out" | grep -q "ok-$m" || fail "marker $m is missing; chain of root: $(chain root)"; done
}

vmi_ready() { [ "$(oc get vmi -n "$NS" "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && agent_connected "$1"; }
guest_ready() { vssh "$1" true >/dev/null 2>&1; }
restart_vm() { # restart_vm <vm>
    local uid
    uid=$(oc get vmi -n "$NS" "$1" -o jsonpath='{.metadata.uid}')
    virtctl restart "$1" -n "$NS" >/dev/null
    new_vmi() { local u; u=$(oc get vmi -n "$NS" "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null); [ -n "$u" ] && [ "$u" != "$uid" ]; }
    wait_until 600 "a new VMI of $1" new_vmi "$1"
    wait_until 900 "$1 running" vmi_ready "$1"
    wait_until 600 "$1 answering" guest_ready "$1"
}
vmi_gone() { ! oc get vmi -n "$NS" "$1" >/dev/null 2>&1; }

# --- the VMs ------------------------------------------------------------------

log "starting the source VM"
vm_yaml src root data yes | oc apply -f -
wait_until 900 "source guest agent" agent_connected src
writer_running() { vssh src 'test -s /var/log/writer.log' >/dev/null 2>&1; }
wait_until 600 "writer running" writer_running

# A VM with only the blank 100 GB disk. It finds nothing to boot, but QEMU
# runs, so the disk can have an instant snapshot.
oc apply -f - <<YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: blank, namespace: $NS}
spec:
  runStrategy: Always
  template:
    spec:
      domain:
        memory: {guest: 512Mi}
        devices:
          disks: [{name: bigdisk, disk: {bus: virtio}}]
      volumes: [{name: bigdisk, persistentVolumeClaim: {claimName: big}}]
YAML
blank_running() { [ "$(oc get vmi -n "$NS" blank -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ]; }
wait_until 900 "the blank VM running" blank_running

# --- 1. VirtualMachineSnapshot and restore ------------------------------------

log "taking a VirtualMachineSnapshot of the running VM"
before=$(vssh src 'wc -l < /var/log/writer.log' | tr -dc '0-9')
start=$SECONDS
oc apply -n "$NS" -f - <<YAML
apiVersion: snapshot.kubevirt.io/v1beta1
kind: VirtualMachineSnapshot
metadata: {name: vmsnap}
spec:
  failureDeadline: 1h
  source: {apiGroup: kubevirt.io, kind: VirtualMachine, name: src}
YAML
snapshot_done() {
    local p
    p=$(oc get vmsnapshot -n "$NS" vmsnap -o jsonpath='{.status.phase}' 2>/dev/null)
    [ "$p" = Failed ] && fail "the VirtualMachineSnapshot failed: $(oc get vmsnapshot -n "$NS" vmsnap -o jsonpath='{.status.conditions}')"
    [ "$p" = Succeeded ]
}
wait_until 600 "the VirtualMachineSnapshot done" snapshot_done
vsc=$(oc get vmsnapshot -n "$NS" vmsnap -o jsonpath='{.status.virtualMachineSnapshotContentName}')
disk_snapshots=$(oc get vmsnapshotcontent -n "$NS" "$vsc" -o json | jq -r '.spec.volumeBackups[].volumeSnapshotName')
all_ready() { local vs; for vs in $disk_snapshots; do snap_ready "$vs" || return 1; done; }
wait_until 120 "the disk snapshots ready" all_ready
took=$((SECONDS - start))
for vs in $disk_snapshots; do
    st=$(snapshot_status "$vs")
    log "$vs: $st"
    [ "$st" = "instant FileSystemConsistent" ] || fail "$vs is \"$st\", want an instant, FileSystemConsistent snapshot"
done
[ "$(words "$disk_snapshots")" = 2 ] || fail "expected 2 disk snapshots, found: $disk_snapshots"
log "the VM snapshot was ready $took s after the request"
expect_open src rootdisk root
expect_open src datadisk data
after=$(vssh src 'wc -l < /var/log/writer.log' | tr -dc '0-9')
[ $((after - before)) -gt 0 ] || fail "the source VM stopped writing during the snapshot"

log "restoring the snapshot to a new VM and booting it"
oc apply -n "$NS" -f - <<YAML
apiVersion: snapshot.kubevirt.io/v1beta1
kind: VirtualMachineRestore
metadata: {name: vmrestore}
spec:
  target: {apiGroup: kubevirt.io, kind: VirtualMachine, name: copy}
  virtualMachineSnapshotName: vmsnap
  patches:
  - '{"op":"remove","path":"/spec/template/spec/domain/devices/interfaces/0/macAddress"}'
YAML
restore_done() { [ "$(oc get vmrestore -n "$NS" vmrestore -o jsonpath='{.status.complete}' 2>/dev/null)" = true ]; }
wait_until 2400 "the restore complete" restore_done
virtctl start copy -n "$NS" >/dev/null
wait_until 2400 "restored guest agent" agent_connected copy
wait_until 1500 "the restored VM to answer" guest_ready copy
xfs=$(vssh copy 'sudo journalctl -k --no-pager | grep -ci "xfs.*\(corrupt\|error\)" || true')
[ "$xfs" = 0 ] || fail "XFS errors in the kernel log of the restored VM: $xfs"
result=$(vssh copy 'sudo systemctl stop writer; total=$(wc -l < /var/log/writer.log); missing=0; wrong=0
while read n s t; do
  if [ ! -f /mnt/data/f$n ]; then missing=$((missing+1)); elif [ "$(sha256sum /mnt/data/f$n | cut -d" " -f1)" != "$s" ]; then wrong=$((wrong+1)); fi
done < /var/log/writer.log
echo "LOGGED=$total MISSING=$missing WRONG=$wrong"')
echo "$result" | grep -q 'MISSING=0 WRONG=0' || fail "the restored disks do not fit each other: $result"
pass "restored VM booted from an instant VM snapshot taken in $took s; $result"

log "deleting the restored VM and the VM snapshot"
oc delete vm -n "$NS" copy --wait=true --timeout=600s >/dev/null
oc delete vmrestore -n "$NS" vmrestore --ignore-not-found >/dev/null
oc delete vmsnapshot -n "$NS" vmsnap --wait=true --timeout=600s >/dev/null
plain() { [ "$(chain root)" = disk.img ] && [ "$(chain data)" = disk.img ]; }
wait_until 900 "both disks merged back to disk.img" plain
expect_open src rootdisk root
expect_open src datadisk data

# --- 2. 100 GB disk -------------------------------------------------------------

log "taking a snapshot of the running VM's blank 100 GB disk"
start=$(perl -MTime::HiRes=time -e 'printf "%.1f", time')
take bigsnap big
until snap_ready bigsnap; do
    [ "$(perl -MTime::HiRes=time -e "printf '%d', time - $start")" -lt 60 ] || fail "the snapshot of the 100 GB disk was not ready after 60 s"
    sleep 0.5
done
took=$(perl -MTime::HiRes=time -e "printf '%.1f', time - $start")
st=$(snapshot_status bigsnap)
[ "${st%% *}" = instant ] || fail "the snapshot of the 100 GB disk is \"$st\", want instant"
perl -e "exit($took < 10 ? 0 : 1)" || fail "the snapshot of the 100 GB disk took $took s; it must take under 10 s"
drop bigsnap 600
pass "100 GB disk: instant snapshot ready in $took s ($st)"

# --- 3. chain, live migration and restart ---------------------------------------

log "three snapshots of the root disk, with a marker file before each"
for i in 1 2 3; do
    marker "m$i"
    take "s$i" root
    wait_until 60 "snapshot s$i ready" snap_ready "s$i"
    st=$(snapshot_status "s$i")
    [ "${st%% *}" = instant ] || fail "s$i is \"$st\", want instant"
done
[ "$(words "$(chain root)")" = 4 ] || fail "the root chain is \"$(chain root)\", want 3 snapshot files and disk.img"
expect_open src rootdisk root

log "live migration right after the last snapshot"
virtctl migrate src -n "$NS" >/dev/null
migrated() {
    local p
    p=$(oc get vmim -n "$NS" -o json | jq -r '[.items[] | select(.spec.vmiName == "src")] | sort_by(.metadata.creationTimestamp) | last | .status.phase')
    [ "$p" = Failed ] && fail "the live migration failed"
    [ "$p" = Succeeded ]
}
wait_until 900 "the live migration" migrated
expect_open src rootdisk root
marker m4
restart_vm src
expect_open src rootdisk root
expect_open src datadisk data
markers_present m1 m2 m3 m4
pass "migration and restart: the VM opens $(top_of root) and has every marker"

# --- 4. deleting snapshots --------------------------------------------------------

for s in s2 s1 s3; do
    log "deleting snapshot $s"
    drop "$s" 600
    expect_open src rootdisk root
done
[ "$(chain root)" = disk.img ] || fail "the root chain is \"$(chain root)\" after all deletes, want disk.img"
marker m5
restart_vm src
expect_open src rootdisk root
markers_present m1 m2 m3 m4 m5
pass "deleting snapshots: middle, oldest and newest merged; the VM runs on disk.img with every marker"

# --- 5. stopped VM ----------------------------------------------------------------

log "a snapshot and a delete while the VM is stopped"
virtctl stop src -n "$NS" >/dev/null
wait_until 600 "the VM stopped" vmi_gone src
take s4 root
wait_until "$JOB_TIMEOUT" "snapshot s4 of the stopped VM ready" snap_ready s4
[ "$(words "$(chain root)")" = 2 ] || fail "the root chain is \"$(chain root)\", want 1 snapshot file and disk.img"
virtctl start src -n "$NS" >/dev/null
wait_until 900 "src running" vmi_ready src
wait_until 600 "src answering" guest_ready src
expect_open src rootdisk root
marker m6
virtctl stop src -n "$NS" >/dev/null
wait_until 600 "the VM stopped" vmi_gone src
drop s4 "$JOB_TIMEOUT"
[ "$(chain root)" = disk.img ] || fail "the root chain is \"$(chain root)\" after the delete, want disk.img"
virtctl start src -n "$NS" >/dev/null
wait_until 900 "src running" vmi_ready src
wait_until 600 "src answering" guest_ready src
expect_open src rootdisk root
markers_present m1 m2 m3 m4 m5 m6
pass "stopped VM: the snapshot and the delete ran as Jobs, and the VM started on the right file each time"
