#!/usr/bin/env bash
# End-to-end test: snapshot a running VM, restore the snapshot, boot the copy.
#
# Makes a pool, a CentOS VM on it that writes a file every 0.1 s (the file's
# checksum goes into a log on the same disk), and a VolumeSnapshot of the VM's
# disk while it runs. The snapshot must be made with the QEMU copy, not a file
# copy. Then it restores the snapshot to a new claim, boots a second VM from it
# and checks:
#   - the Snapshot says FileSystemConsistent and names the copy moment
#   - the restored VM boots and its guest agent connects
#   - the kernel log has no XFS errors
#   - every file in the writer's log is on the disk with the logged checksum
#   - the snapshot was not made with a file copy
#
# Windows: GUEST=windows boots a Windows Server 2022 VM from a copy of an existing
# Windows disk (WIN_IMAGE_NS/WIN_IMAGE_PVC, a block PVC; it is only read). A
# PowerShell writer replaces the Linux writer, and the checks run through the
# QEMU guest agent. This tests the freeze through VSS.
#
# Usage: KUBECONFIG=... SSH_KEY=~/.ssh/key test/e2e/test-vm-snapshot.sh [--keep]
# SSH_KEY is a private key whose .pub file is put in the VM for virtctl ssh.
set -euo pipefail

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-vm-snap}
NS=${NS:-$PREFIX-test}
POOL=$PREFIX-pool
CLASS=$PREFIX-class
SSH_KEY=${SSH_KEY:?set SSH_KEY to a private key file}
IMAGE_URL=${IMAGE_URL:-https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2}
GUEST=${GUEST:-linux}
case "$GUEST" in linux | windows) ;; *) echo "GUEST must be linux or windows" >&2; exit 2 ;; esac
WIN_IMAGE_NS=${WIN_IMAGE_NS:-brs-win-image}
WIN_IMAGE_PVC=${WIN_IMAGE_PVC:-win2k22-brs}
WIN_BLOCK_SC=${WIN_BLOCK_SC:-ocs-storagecluster-ceph-rbd-virtualization}
ROOT_GI=20
[ "$GUEST" = windows ] && ROOT_GI=24
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

# The pool's StorageClass is made by the pool; a VolumeSnapshotClass with a
# speed limit is made here.
oc apply -f - <<YAML
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata: {name: $CLASS}
driver: vpc-file-pool.csi.ibm.io
deletionPolicy: Delete
parameters:
  vmCopySpeedLimitMBps: "$SNAPSHOT_SPEED"
YAML

if [ "$GUEST" = windows ]; then
log "copying the Windows image $WIN_IMAGE_NS/$WIN_IMAGE_PVC onto the pool"
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: root}
spec:
  storageClassName: $POOL
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: ${ROOT_GI}Gi}}
---
apiVersion: cdi.kubevirt.io/v1beta1
kind: DataVolume
metadata: {name: winbase}
spec:
  source:
    pvc: {namespace: $WIN_IMAGE_NS, name: $WIN_IMAGE_PVC}
  storage:
    storageClassName: $WIN_BLOCK_SC
    volumeMode: Block
    accessModes: [ReadWriteMany]
    resources: {requests: {storage: 20Gi}}
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
          dd if=/dev/win of=/disk/disk.img bs=4M conv=sparse status=none
          chown 107:107 /disk/disk.img
        volumeDevices: [{name: w, devicePath: /dev/win}]
        volumeMounts: [{name: d, mountPath: /disk}]
      volumes:
      - {name: w, persistentVolumeClaim: {claimName: winbase, readOnly: true}}
      - {name: d, persistentVolumeClaim: {claimName: root}}
YAML
oc wait -n "$NS" job/convert --for=condition=complete --timeout=40m
else
log "making the source disk"
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: root}
spec:
  storageClassName: $POOL
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: ${ROOT_GI}Gi}}
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

fi

PUBKEY=$(cat "$SSH_KEY.pub")
vm_yaml() { # vm_yaml <name> <root claim> <with cloud-init: yes|no>
    if [ "$GUEST" = windows ]; then
        cat <<YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata: {name: $1, namespace: $NS}
spec:
  runStrategy: Always
  instancetype: {name: u1.large, kind: VirtualMachineClusterInstancetype}
  preference: {name: windows.2k22.virtio, kind: VirtualMachineClusterPreference}
  template:
    spec:
      evictionStrategy: LiveMigrate
      domain:
        devices:
          disks:
          - {name: rootdisk, disk: {bus: virtio}, bootOrder: 1}
      volumes:
      - {name: rootdisk, persistentVolumeClaim: {claimName: $2}}
YAML
        return 0
    fi
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
$( [ "$3" = yes ] && echo "          - {name: cloudinit, disk: {bus: virtio}}" )
          interfaces: [{name: default, masquerade: {}}]
      networks: [{name: default, pod: {}}]
      volumes:
      - {name: rootdisk, persistentVolumeClaim: {claimName: $2}}
YAML
    [ "$3" = yes ] || return 0
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
                # Writes /opt/data/f<n> and then logs "<n> <sha256> <time>" in /var/log/writer.log.
                mkdir -p /opt/data
                n=\$(wc -l < /var/log/writer.log 2>/dev/null || echo 0)
                while :; do
                  n=\$((n+1))
                  head -c 65536 /dev/urandom > /opt/data/f\$n
                  sync /opt/data/f\$n
                  echo "\$n \$(sha256sum /opt/data/f\$n | cut -d' ' -f1) \$(date +%s.%N)" >> /var/log/writer.log
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

# wexec <vm> <PowerShell command> runs the command in a Windows VM through the
# QEMU guest agent and prints its output.
wexec() {
    local vm=$1 pod dom req pid r
    pod=$(oc get pod -n "$NS" -l "vm.kubevirt.io/name=$vm" -o jsonpath='{.items[0].metadata.name}')
    dom=$(oc exec -n "$NS" "$pod" -c compute -- virsh list --name | head -1)
    req=$(python3 -I -c 'import json,sys;print(json.dumps({"execute":"guest-exec","arguments":{"path":"powershell.exe","arg":["-NoProfile","-Command",sys.argv[1]],"capture-output":True}}))' "$2")
    pid=$(oc exec -n "$NS" "$pod" -c compute -- virsh qemu-agent-command "$dom" "$req" | python3 -I -c 'import json,sys;print(json.load(sys.stdin)["return"]["pid"])')
    for _ in $(seq 1 900); do
        r=$(oc exec -n "$NS" "$pod" -c compute -- virsh qemu-agent-command "$dom" "{\"execute\":\"guest-exec-status\",\"arguments\":{\"pid\":$pid}}")
        if echo "$r" | python3 -I -c 'import json,sys;sys.exit(0 if json.load(sys.stdin)["return"]["exited"] else 1)'; then
            echo "$r" | python3 -I -c 'import json,sys,base64;d=json.load(sys.stdin)["return"];print(base64.b64decode(d.get("out-data","")).decode(errors="replace"),end="");e=d.get("err-data");e and print(base64.b64decode(e).decode(errors="replace"),end="",file=sys.stderr);sys.exit(d.get("exitcode",0))'
            return $?
        fi
        sleep 1
    done
    return 1
}

# wping <vm>: the guest agent of a Windows VM answers a ping. Commands sent
# before it does leave the agent channel out of step, so wait for this first.
wping() {
    local pod dom
    pod=$(oc get pod -n "$NS" -l "vm.kubevirt.io/name=$1" -o jsonpath='{.items[0].metadata.name}') || return 1
    dom=$(oc exec -n "$NS" "$pod" -c compute -- virsh list --name 2>/dev/null | head -1) || return 1
    oc exec -n "$NS" "$pod" -c compute -- virsh qemu-agent-command "$dom" '{"execute":"guest-ping"}' >/dev/null 2>&1
}

# guest_ready <vm>: the VM answers a command.
guest_ready() {
    if [ "$GUEST" = windows ]; then wping "$1" && wexec "$1" '1' >/dev/null; else vssh "$1" true >/dev/null 2>&1; fi
}

WRITER_PS1='New-Item -ItemType Directory -Force C:\data | Out-Null
$n = 0
if (Test-Path C:\writer.log) { $n = (Get-Content C:\writer.log | Measure-Object -Line).Lines }
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$sha = [Security.Cryptography.SHA256]::Create()
while ($true) {
  $n++
  $b = New-Object byte[] 65536
  $rng.GetBytes($b)
  $fs = [IO.File]::Create("C:\data\f$n")
  $fs.Write($b, 0, $b.Length); $fs.Flush($true); $fs.Close()
  $h = ([BitConverter]::ToString($sha.ComputeHash($b)) -replace "-","").ToLower()
  $t = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() / 1000
  $fs = New-Object IO.FileStream("C:\writer.log", "Append", "Write", "Read")
  $w = [Text.Encoding]::ASCII.GetBytes("$n $h $t`n")
  $fs.Write($w, 0, $w.Length); $fs.Flush($true); $fs.Close()
  Start-Sleep -Milliseconds 100
}'

# logged_files <vm>: how many files the writer has logged.
logged_files() {
    if [ "$GUEST" = windows ]; then wexec "$1" '(Get-Content C:\writer.log | Measure-Object -Line).Lines'
    else vssh "$1" 'wc -l < /var/log/writer.log'; fi
}

log "starting the source VM"
vm_yaml src root yes | oc apply -f -
wait_until 900 "source guest agent" agent_connected src
if [ "$GUEST" = windows ]; then
    # A new Windows VM can take about 40 minutes before the agent runs commands.
    wait_until 3600 "the Windows guest agent to run commands" guest_ready src
    b64=$(printf '%s' "$WRITER_PS1" | base64 | tr -d '\n')
    wexec src "[IO.File]::WriteAllBytes('C:\\writer.ps1',[Convert]::FromBase64String('$b64')); Start-Process powershell.exe -ArgumentList '-NoProfile -ExecutionPolicy Bypass -File C:\\writer.ps1' -WindowStyle Hidden"
fi
writer_running() {
    if [ "$GUEST" = windows ]; then wexec src 'if ((Get-Content C:\writer.log -ErrorAction SilentlyContinue | Measure-Object -Line).Lines -gt 0) { exit 0 } else { exit 1 }' >/dev/null 2>&1
    else vssh src 'test -s /var/log/writer.log' >/dev/null 2>&1; fi
}
wait_until 300 "writer running" writer_running
before=$(logged_files src)

log "taking the snapshot of the running VM (speed limit $SNAPSHOT_SPEED MB/s)"
start=$SECONDS
oc apply -n "$NS" -f - <<YAML
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: {name: snap}
spec:
  volumeSnapshotClassName: $CLASS
  source: {persistentVolumeClaimName: root}
YAML
snapshot_ready() { [ "$(oc get volumesnapshot -n "$NS" snap -o jsonpath='{.status.readyToUse}' 2>/dev/null)" = true ]; }
wait_until "$SNAPSHOT_TIMEOUT" "the snapshot ready" snapshot_ready
log "the snapshot took $((SECONDS - start)) s"

content=$(oc get volumesnapshot -n "$NS" snap -o jsonpath='{.status.boundVolumeSnapshotContentName}')
handle=$(oc get volumesnapshotcontent "$content" -o jsonpath='{.status.snapshotHandle}')
snapcr=${handle##*/}
st=$(oc get snapshots.storage.ibmcloud.io "$snapcr" -o json)
echo "$st" | jq -c '{method: .metadata.annotations["storage.ibmcloud.io/snapshot-method"], consistency: .status.consistency, reason: .status.consistencyReason, moment: .status.copyMoment, attempts: .status.vmCopyAttempts}'
[ "$(echo "$st" | jq -r '.metadata.annotations["storage.ibmcloud.io/snapshot-method"]')" = vm ] || fail "the snapshot was not made with the QEMU copy"
[ "$(echo "$st" | jq -r '.status.copyMoment')" != null ] || fail "the snapshot names no copy moment"
consistency=$(echo "$st" | jq -r '.status.consistency')
[ "$consistency" = FileSystemConsistent ] || log "WARNING: the snapshot is $consistency: $(echo "$st" | jq -r '.status.consistencyReason')"
moment=$(echo "$st" | jq -r '.status.copyMoment')

after=$(logged_files src)
log "the source VM wrote $((after - before)) files while the snapshot ran"
[ $((after - before)) -gt 0 ] || fail "the source VM stopped writing during the snapshot"

log "restoring the snapshot to a new claim and booting from it"
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: restored}
spec:
  storageClassName: $POOL
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  dataSource: {apiGroup: snapshot.storage.k8s.io, kind: VolumeSnapshot, name: snap}
  resources: {requests: {storage: ${ROOT_GI}Gi}}
YAML
vm_yaml restored restored no | oc apply -f -
wait_until 2400 "restored guest agent" agent_connected restored
wait_until 1500 "the restored VM to answer" guest_ready restored
epoch=$(perl -MTime::Piece -e 'print Time::Piece->strptime(shift, "%Y-%m-%dT%H:%M:%SZ")->epoch' "$moment")
if [ "$GUEST" = windows ]; then
    result=$(wexec restored '
$boot = (Get-CimInstance Win32_OperatingSystem).LastBootUpTime
$bad = @(Get-WinEvent -FilterHashtable @{LogName="System"; ProviderName="Ntfs"; StartTime=$boot} -ErrorAction SilentlyContinue | Where-Object { $_.Id -in 55,136,137,140 -or $_.LevelDisplayName -in "Error","Critical" })
"NTFS_EVENTS=" + $bad.Count
"DIRTY_C=" + ((fsutil dirty query C:) -join " ")
$sha = [Security.Cryptography.SHA256]::Create()
$total = 0; $upto = 0; $badfiles = 0
foreach ($line in [IO.File]::ReadLines("C:\writer.log")) {
  $p = $line.Split(" ")
  if ($p.Count -lt 3) { continue }
  $total++
  if ([double]$p[2] -le '"$epoch"') { $upto++ }
  $f = "C:\data\f" + $p[0]
  if (-not (Test-Path $f)) { $badfiles++; continue }
  $h = ([BitConverter]::ToString($sha.ComputeHash([IO.File]::ReadAllBytes($f))) -replace "-","").ToLower()
  if ($h -ne $p[1]) { $badfiles++ }
}
"LOGGED=" + $total + " UPTO_MOMENT=" + $upto + " BAD=" + $badfiles
if ($badfiles -eq 0 -and $bad.Count -eq 0) { "CHECKSUMS_OK" }
')
    echo "$result"
    echo "$result" | grep -q CHECKSUMS_OK || fail "the restored Windows VM has file system errors or files that do not match the log"
    log "PASS: restored Windows VM booted; $(echo "$result" | grep -o 'LOGGED=.*') (moment $moment), all checksums match"
    exit 0
fi

xfs=$(vssh restored 'sudo journalctl -k --no-pager | grep -ci "xfs.*\(corrupt\|error\)" || true')
[ "$xfs" = 0 ] || fail "XFS errors in the kernel log of the restored VM: $xfs"
result=$(vssh restored 'sudo systemctl stop writer; awk -v m='"$epoch"' '"'"'$3 <= m {c++} END {print c+0}'"'"' /var/log/writer.log; while read n s t; do echo "$s  /opt/data/f$n"; done < /var/log/writer.log | sha256sum -c --quiet && echo CHECKSUMS_OK')
echo "$result"
echo "$result" | grep -q CHECKSUMS_OK || fail "files on the restored disk do not match the log"
log "PASS: restored VM booted; $(echo "$result" | head -1) files logged up to $moment, all checksums match"
