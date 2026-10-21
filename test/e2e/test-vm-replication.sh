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
#   - the next scheduled copy changes far less than the disk size, and both
#     disks then have a restore point at the destination
#
# Usage: KUBECONFIG=... SSH_KEY=~/.ssh/key [MODE=receiver] test/e2e/test-vm-replication.sh [--keep]
# MODE=direct (default) writes the copy over NFS. MODE=receiver starts a replication
# receiver in kube-system (own TLS certificate, token and data volume on the
# destination pool) and sends the copy to it over HTTPS.
# SSH_KEY is a private key whose .pub file is put in the VM for virtctl ssh.
#
# Windows: GUEST=windows boots a Windows Server 2022 VM from a copy of an existing
# Windows disk (WIN_IMAGE_NS/WIN_IMAGE_PVC, a block PVC; it is only read). A
# PowerShell writer on C: and D: replaces the Linux writer, and the checks run
# through the QEMU guest agent. This tests the freeze through VSS.
#
# Two clusters: set DR_KUBECONFIG (needs MODE=receiver). KUBECONFIG is then the
# source cluster. The destination pool, the receiver, the far-site disks and
# the copy VMs all live on the DR cluster. The receiver is reached through an
# OpenShift Route (TLS passthrough), as in docs/CROSS-REGION-DR.md.
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
COPY_TIMEOUT=${COPY_TIMEOUT:-5400}
SCHEDULE=${SCHEDULE:-15m}
MODE=${MODE:-direct}
case "$MODE" in direct | receiver) ;; *) echo "MODE must be direct or receiver" >&2; exit 2 ;; esac
RECV=$PREFIX-recv
GUEST=${GUEST:-linux}
case "$GUEST" in linux | windows) ;; *) echo "GUEST must be linux or windows" >&2; exit 2 ;; esac
WIN_IMAGE_NS=${WIN_IMAGE_NS:-brs-win-image}
WIN_IMAGE_PVC=${WIN_IMAGE_PVC:-win2k22-brs}
WIN_BLOCK_SC=${WIN_BLOCK_SC:-ocs-storagecluster-ceph-rbd-virtualization}
ROOT_GI=20
[ "$GUEST" = windows ] && ROOT_GI=24
DATA_GI=10
DR_KUBECONFIG=${DR_KUBECONFIG:-}
SRC_KUBECONFIG=${KUBECONFIG:-$HOME/.kube/config}
if [ -n "$DR_KUBECONFIG" ] && [ "$MODE" != receiver ]; then echo "DR_KUBECONFIG needs MODE=receiver" >&2; exit 2; fi

# onfar <command...> runs the command against the destination cluster. With one
# cluster it runs the command unchanged. It also works for the functions below.
onfar() {
    if [ -n "$DR_KUBECONFIG" ]; then KUBECONFIG=$DR_KUBECONFIG "$@"; else "$@"; fi
}

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

RECV_LABEL=pool-csi-test=$RECV

cleanup() {
    if $KEEP; then log "keeping test resources (--keep)"; return; fi
    log "cleaning up"
    oc delete replicationpolicy "$POLICY" --ignore-not-found --wait=false
    oc delete ns "$NS" --ignore-not-found --wait=true --timeout=10m || true
    onfar oc delete ns "$NS" --ignore-not-found --wait=true --timeout=10m || true
    onfar oc delete pv "$PREFIX-copy-root" "$PREFIX-copy-data" "$PREFIX-rewind-root" "$PREFIX-rewind-data" --ignore-not-found
    if [ "$MODE" = receiver ]; then
        onfar oc delete deploy,svc,route,secret,pvc -n kube-system -l "$RECV_LABEL" --ignore-not-found --wait=true --timeout=5m || true
        if [ -n "$DR_KUBECONFIG" ]; then
            oc delete secret -n kube-system -l "$RECV_LABEL" --ignore-not-found || true
        fi
    fi
    oc delete filesharepool "$SRC" --ignore-not-found --wait=false
    onfar oc delete filesharepool "$DST" --ignore-not-found --wait=false
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

pool_field() { oc get filesharepool "$1" -o jsonpath="{$2}" 2>/dev/null; }
pool_ready() { [ "$(pool_field "$1" .status.phase)" = "Ready" ]; }

log "making pools $SRC and $DST"
make_pool "$SRC"
onfar make_pool "$DST"
pools_ready() { pool_ready "$SRC" && onfar pool_ready "$DST"; }
wait_until 1200 "pools Ready" pools_ready

oc create ns "$NS" --dry-run=client -o yaml | oc apply -f -
oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null
onfar oc create ns "$NS" --dry-run=client -o yaml | onfar oc apply -f -

if [ -n "${RESUME:-}" ]; then
    log "RESUME: reusing the disks and namespace of an earlier --keep run"
elif [ "$GUEST" = windows ]; then
log "copying the Windows image $WIN_IMAGE_NS/$WIN_IMAGE_PVC onto the source pool"
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: root}
spec:
  storageClassName: $SRC
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: ${ROOT_GI}Gi}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data}
spec:
  storageClassName: $SRC
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: ${DATA_GI}Gi}}
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
          truncate -s 8G /data/disk.img
          chown 107:107 /data/disk.img
        volumeDevices: [{name: w, devicePath: /dev/win}]
        volumeMounts: [{name: d, mountPath: /disk}, {name: e, mountPath: /data}]
      volumes:
      - {name: w, persistentVolumeClaim: {claimName: winbase, readOnly: true}}
      - {name: d, persistentVolumeClaim: {claimName: root}}
      - {name: e, persistentVolumeClaim: {claimName: data}}
YAML
oc wait -n "$NS" job/convert --for=condition=complete --timeout=40m
else
log "making the source disks"
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: root}
spec:
  storageClassName: $SRC
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: ${ROOT_GI}Gi}}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data}
spec:
  storageClassName: $SRC
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: ${DATA_GI}Gi}}
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
vm_yaml() { # vm_yaml <name> <root claim> <data claim> <with cloud-init: yes|no>
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
          - {name: datadisk, disk: {bus: virtio}}
      volumes:
      - {name: rootdisk, persistentVolumeClaim: {claimName: $2}}
      - {name: datadisk, persistentVolumeClaim: {claimName: $3}}
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

WRITER_PS1='$n = 0
if (Test-Path C:\writer.log) { $n = (Get-Content C:\writer.log | Measure-Object -Line).Lines }
$rng = [Security.Cryptography.RandomNumberGenerator]::Create()
$sha = [Security.Cryptography.SHA256]::Create()
while ($true) {
  $n++
  $b = New-Object byte[] 65536
  $rng.GetBytes($b)
  $fs = [IO.File]::Create("D:\f$n")
  $fs.Write($b, 0, $b.Length); $fs.Flush($true); $fs.Close()
  $h = ([BitConverter]::ToString($sha.ComputeHash($b)) -replace "-","").ToLower()
  $t = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() / 1000
  $fs = New-Object IO.FileStream("C:\writer.log", "Append", "Write", "Read")
  $w = [Text.Encoding]::ASCII.GetBytes("$n $h $t`n")
  $fs.Write($w, 0, $w.Length); $fs.Flush($true); $fs.Close()
  Start-Sleep -Milliseconds 100
}'

log "starting the source VM"
vm_yaml src root data yes | oc apply -f -
wait_until 900 "source guest agent" agent_connected src
if [ "$GUEST" = windows ]; then
    # A new Windows VM can take about 40 minutes before the agent runs commands.
    wait_until 3600 "the Windows guest agent to run commands" guest_ready src
    b64=$(printf '%s' "$WRITER_PS1" | base64 | tr -d '\n')
    wexec src "[IO.File]::WriteAllBytes('C:\\writer.ps1',[Convert]::FromBase64String('$b64')); Set-Disk -Number 1 -IsOffline \$false; Set-Disk -Number 1 -IsReadOnly \$false; Initialize-Disk -Number 1 -PartitionStyle GPT; New-Partition -DiskNumber 1 -DriveLetter D -UseMaximumSize | Format-Volume -FileSystem NTFS -NewFileSystemLabel data -Confirm:\$false | Out-Null; Start-Process powershell.exe -ArgumentList '-NoProfile -ExecutionPolicy Bypass -File C:\\writer.ps1' -WindowStyle Hidden"
fi
writer_running() {
    if [ "$GUEST" = windows ]; then wexec src 'if ((Get-Content C:\writer.log -ErrorAction SilentlyContinue | Measure-Object -Line).Lines -gt 0) { exit 0 } else { exit 1 }' >/dev/null 2>&1
    else vssh src 'test -s /var/log/writer.log' >/dev/null 2>&1; fi
}
wait_until 300 "writer running" writer_running

server=$(onfar pool_field "$DST" '.status.shares[0].mountTargetIP')
export_path=$(onfar pool_field "$DST" '.status.shares[0].exportPath')

# start_receiver makes a receiver in kube-system. Its data volume is a claim on
# the destination pool, so the far-site disks can be mounted by NFS afterwards.
start_receiver() {
    local image dir crt san token
    image=$(oc get deploy -n kube-system -l app.kubernetes.io/component=controller \
        -o jsonpath='{.items[0].spec.template.spec.containers[?(@.name=="csi-controller")].image}')
    dir=$(mktemp -d)
    san="DNS:$RECV.kube-system.svc,DNS:$RECV.kube-system.svc.cluster.local"
    [ -z "$DR_KUBECONFIG" ] || san="$san,DNS:$RECV_HOST"
    openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=$RECV" \
        -addext "subjectAltName=$san" \
        -keyout "$dir/tls.key" -out "$dir/tls.crt" 2>/dev/null
    crt=$dir/tls.crt
    token=$(openssl rand -hex 24)
    oc create secret generic "$RECV-tls" -n kube-system --from-file=tls.crt="$crt" --from-file=tls.key="$dir/tls.key"
    oc create secret generic "$RECV-ca" -n kube-system --from-file=ca.crt="$crt"
    oc create secret generic "$RECV-token" -n kube-system --from-literal=token="$token"
    oc label secret -n kube-system "$RECV-tls" "$RECV-ca" "$RECV-token" "$RECV_LABEL"
    if [ -n "$DR_KUBECONFIG" ]; then
        # The source cluster needs the same CA and token to call the receiver.
        KUBECONFIG=$SRC_KUBECONFIG oc create secret generic "$RECV-ca" -n kube-system --from-file=ca.crt="$crt"
        KUBECONFIG=$SRC_KUBECONFIG oc create secret generic "$RECV-token" -n kube-system --from-literal=token="$token"
        KUBECONFIG=$SRC_KUBECONFIG oc label secret -n kube-system "$RECV-ca" "$RECV-token" "$RECV_LABEL"
    fi
    oc apply -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $RECV-data, namespace: kube-system, labels: {pool-csi-test: $RECV}}
spec:
  storageClassName: $DST
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 60Gi}}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: $RECV, namespace: kube-system, labels: {pool-csi-test: $RECV}}
spec:
  replicas: 1
  selector: {matchLabels: {pool-csi-test-app: $RECV}}
  template:
    metadata: {labels: {pool-csi-test-app: $RECV}}
    spec:
      securityContext: {runAsUser: 107, runAsGroup: 107}
      imagePullSecrets: [{name: all-icr-io}]
      containers:
      - name: receiver
        image: $image
        args:
        - --mode=receiver
        - --receiver-base-path=/data
        - --receiver-auth-token-file=/etc/replication/token
        - --receiver-listen-addr=:8443
        - --receiver-tls-cert-file=/certs/tls.crt
        - --receiver-tls-key-file=/certs/tls.key
        ports: [{name: https, containerPort: 8443}]
        readinessProbe: {httpGet: {path: /api/v1/health, port: https, scheme: HTTPS}, periodSeconds: 5}
        volumeMounts:
        - {name: data, mountPath: /data}
        - {name: token, mountPath: /etc/replication, readOnly: true}
        - {name: certs, mountPath: /certs, readOnly: true}
      volumes:
      - {name: data, persistentVolumeClaim: {claimName: $RECV-data}}
      - {name: token, secret: {secretName: $RECV-token}}
      - {name: certs, secret: {secretName: $RECV-tls}}
---
apiVersion: v1
kind: Service
metadata: {name: $RECV, namespace: kube-system, labels: {pool-csi-test: $RECV}}
spec:
  selector: {pool-csi-test-app: $RECV}
  ports: [{name: https, port: 8443, targetPort: https}]
---
apiVersion: route.openshift.io/v1
kind: Route
metadata: {name: $RECV, namespace: kube-system, labels: {pool-csi-test: $RECV}}
spec:
  host: $RECV_HOST
  to: {kind: Service, name: $RECV}
  port: {targetPort: https}
  tls: {termination: passthrough}
YAML
    recv_bound() { [ "$(oc get pvc -n kube-system "$RECV-data" -o jsonpath='{.status.phase}')" = Bound ]; }
    wait_until 600 "the receiver data volume" recv_bound
    wait_until 300 "the receiver" oc rollout status deploy/"$RECV" -n kube-system --timeout=10s
}

if [ "$MODE" = receiver ]; then
    log "starting the receiver $RECV"
    if [ -n "$DR_KUBECONFIG" ]; then
        RECV_HOST=$RECV-kube-system.$(onfar oc get ingresses.config cluster -o jsonpath='{.spec.domain}')
        RECV_URL=https://$RECV_HOST
    else
        RECV_HOST=$RECV.kube-system.svc
        RECV_URL=https://$RECV_HOST:8443
    fi
    onfar start_receiver
    recv_pv=$(onfar oc get pvc -n kube-system "$RECV-data" -o jsonpath='{.spec.volumeName}')
    recv_sub=$(onfar oc get subvolume "$recv_pv" -o jsonpath='{.spec.subPath}')
    far_prefix="${export_path%/}$recv_sub/repl"
    log "making policy $POLICY to $RECV_URL"
    oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: ReplicationPolicy
metadata: {name: $POLICY}
spec:
  sourcePoolName: $SRC
  destinationEndpoint: $RECV_URL
  destinationAuthSecretRef: $RECV-token
  destinationCACertSecretRef: $RECV-ca
  destinationBasePath: /repl
  schedule: $SCHEDULE
  maxRetries: 3
  maxParallelSyncs: 2
YAML
else
    far_prefix="${export_path%/}/repl"
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
  schedule: $SCHEDULE
  maxRetries: 3
  maxParallelSyncs: 2
YAML
fi

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

# The next scheduled copy holds only changed blocks, and the destination
# keeps the first copy as a restore point.
first=$(oc get replicationpolicy "$POLICY" -o jsonpath='{.status.subVolumeStatuses[0].copyMoment}')
second_done() {
    copies_done >/dev/null &&
        [ "$(oc get replicationpolicy "$POLICY" -o json | jq --arg f "$first" '[.status.subVolumeStatuses[] | select(.copyMoment != $f)] | length')" = 2 ]
}
log "waiting for the change copy (schedule $SCHEDULE)"
start=$SECONDS
wait_until "$COPY_TIMEOUT" "the change copy" second_done
log "change copy done $((SECONDS - start))s after the first"
status=$(oc get replicationpolicy "$POLICY" -o json)
echo "$status" | jq -c '[.status.subVolumeStatuses[] | {sv: .subVolumeName[0:12], bytes: .bytesSynced, restorePoints, oldest: .oldestRestorePoint}]'
root_pv=$(oc get pvc -n "$NS" root -o jsonpath='{.spec.volumeName}')
root_bytes=$(echo "$status" | jq --arg sv "$root_pv" '.status.subVolumeStatuses[] | select(.subVolumeName == $sv) | .bytesSynced // 0')
[ "$root_bytes" -lt $((2 << 30)) ] || fail "the change copy of the 20 GiB root disk changed $root_bytes bytes; expected far less"
[ "$(echo "$status" | jq '[.status.subVolumeStatuses[] | select(.restorePoints >= 1 and .oldestRestorePoint != null)] | length')" = 2 ] ||
    fail "expected a restore point on both disks"
log "change copy changed $root_bytes bytes on the root disk; both disks have a restore point"

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
    onfar oc apply -f - <<YAML
apiVersion: v1
kind: PersistentVolume
metadata: {name: $PREFIX-copy-$d}
spec:
  capacity: {storage: $( [ $d = root ] && echo ${ROOT_GI}Gi || echo ${DATA_GI}Gi )}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  mountOptions: [nfsvers=4.1, sec=sys, soft, timeo=600, retrans=3]
  nfs: {server: "$server", path: "$far_prefix$sub"}
  claimRef: {namespace: $NS, name: copy-$d}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: copy-$d, namespace: $NS}
spec:
  storageClassName: ""
  volumeName: $PREFIX-copy-$d
  accessModes: [ReadWriteMany]
  resources: {requests: {storage: $( [ $d = root ] && echo ${ROOT_GI}Gi || echo ${DATA_GI}Gi )}}
YAML
done

# check_copy_windows <vm> <moment>: no NTFS errors since boot, and every file in
# the writer's log on C: is on D: with the logged checksum.
check_copy_windows() {
    local vm=$1 at=$2 epoch result
    epoch=$(perl -MTime::Piece -e 'print Time::Piece->strptime(shift, "%Y-%m-%dT%H:%M:%SZ")->epoch' "$at")
    result=$(onfar wexec "$vm" '
$boot = (Get-CimInstance Win32_OperatingSystem).LastBootUpTime
$bad = @(Get-WinEvent -FilterHashtable @{LogName="System"; ProviderName="Ntfs"; StartTime=$boot} -ErrorAction SilentlyContinue | Where-Object { $_.Id -in 55,136,137,140 -or $_.LevelDisplayName -in "Error","Critical" })
"NTFS_EVENTS=" + $bad.Count
$bad | ForEach-Object { "  " + $_.Id + " " + $_.Message.Split("`n")[0] }
"DIRTY_C=" + ((fsutil dirty query C:) -join " ")
"DIRTY_D=" + ((fsutil dirty query D:) -join " ")
$sha = [Security.Cryptography.SHA256]::Create()
$total = 0; $upto = 0; $badfiles = 0
foreach ($line in [IO.File]::ReadLines("C:\writer.log")) {
  $p = $line.Split(" ")
  if ($p.Count -lt 3) { continue }
  $total++
  if ([double]$p[2] -le '"$epoch"') { $upto++ }
  $f = "D:\f" + $p[0]
  if (-not (Test-Path $f)) { $badfiles++; continue }
  $h = ([BitConverter]::ToString($sha.ComputeHash([IO.File]::ReadAllBytes($f))) -replace "-","").ToLower()
  if ($h -ne $p[1]) { $badfiles++ }
}
"LOGGED=" + $total + " UPTO_MOMENT=" + $upto + " BAD=" + $badfiles
if ($badfiles -eq 0 -and $bad.Count -eq 0) { "CHECKSUMS_OK" }
')
    echo "$result"
    echo "$result" | grep -q CHECKSUMS_OK || fail "the Windows copy $vm has file system errors or files that do not match the log"
    log "PASS: $vm booted; $(echo "$result" | grep -o 'LOGGED=.*') (moment $at), all checksums match"
}

# check_copy <vmi> <moment>: the VM booted from far-site disks shows no file
# system errors, and every file in the writer's log on the root disk is on the
# data disk with the logged checksum.
check_copy() {
    local vmi=$1 at=$2 xfs result
    if [ "$GUEST" = windows ]; then
        check_copy_windows "$vmi" "$at"
        return
    fi
    xfs=$(onfar vssh "$vmi" 'sudo journalctl -k --no-pager | grep -ci "xfs.*\(corrupt\|error\)" || true')
    [ "$xfs" = 0 ] || fail "XFS errors in the kernel log of $vmi: $xfs"
    result=$(onfar vssh "$vmi" 'sudo systemctl stop writer; awk -v m="$(date -d '"$at"' +%s)" '"'"'$3 <= m {c++} END {print c+0}'"'"' /var/log/writer.log; while read n s t; do echo "$s  /data/f$n"; done < /var/log/writer.log | sha256sum -c --quiet && echo CHECKSUMS_OK')
    echo "$result"
    echo "$result" | grep -q CHECKSUMS_OK || fail "files on the data disk of $vmi do not match the log on the root disk"
    log "PASS: $vmi booted; $(echo "$result" | head -1) files logged up to $at, all checksums match"
}

# failover_check rewinds both disks to the first copy with the failover tool,
# run in the receiver pod where the share is mounted, and boots from them.
failover_check() {
    local bin epoch pod=deploy/$RECV d pv sub moments
    bin=$(mktemp)
    log "building kubectl-failover and putting it in the receiver pod"
    (cd "$(dirname "$0")/../.." && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$bin" ./cmd/failover/)
    onfar oc exec -i -n kube-system "$pod" -- sh -c 'cat > /tmp/kubectl-failover && chmod +x /tmp/kubectl-failover' < "$bin"

    log "failover: moments both disks can show"
    moments=$(onfar oc exec -n kube-system "$pod" -- /tmp/kubectl-failover plan --nfs-mount-path /data/repl --list-moments)
    echo "$moments"
    echo "$moments" | grep -q "$moment" || fail "the latest moment $moment is not listed"
    echo "$moments" | grep -q "$first" || fail "the first copy moment $first is not listed"

    log "failover: rewinding both disks to $first"
    onfar oc exec -n kube-system "$pod" -- /tmp/kubectl-failover execute --nfs-mount-path /data/repl --prepare-only --rewind-to "$first"
    epoch=$(perl -MTime::Piece -e 'print Time::Piece->strptime(shift, "%Y-%m-%dT%H:%M:%SZ")->epoch' "$first")
    for d in root data; do
        pv=$(oc get pvc -n "$NS" "$d" -o jsonpath='{.spec.volumeName}')
        sub=$(oc get subvolume "$pv" -o jsonpath='{.spec.subPath}')
        onfar oc apply -f - <<YAML
apiVersion: v1
kind: PersistentVolume
metadata: {name: $PREFIX-rewind-$d}
spec:
  capacity: {storage: $( [ $d = root ] && echo ${ROOT_GI}Gi || echo ${DATA_GI}Gi )}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  mountOptions: [nfsvers=4.1, sec=sys, soft, timeo=600, retrans=3]
  nfs: {server: "$server", path: "$far_prefix$sub.rewind-$epoch"}
  claimRef: {namespace: $NS, name: rewind-$d}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: rewind-$d, namespace: $NS}
spec:
  storageClassName: ""
  volumeName: $PREFIX-rewind-$d
  accessModes: [ReadWriteMany]
  resources: {requests: {storage: $( [ $d = root ] && echo ${ROOT_GI}Gi || echo ${DATA_GI}Gi )}}
YAML
    done
    log "booting the rewound copy"
    vm_yaml rewound rewind-root rewind-data no | onfar oc apply -f -
    wait_until 900 "rewound guest agent" onfar agent_connected rewound
    wait_until 300 "the rewound copy to answer" onfar guest_ready rewound
    check_copy rewound "$first"
}

# Rewind first. A VM that boots from the latest copy writes to it, and a disk
# that was written to cannot be rewound.
if [ "$MODE" = receiver ]; then
    failover_check
fi

log "booting the copy"
vm_yaml copy copy-root copy-data no | onfar oc apply -f -
wait_until 900 "copy guest agent" onfar agent_connected copy

log "waiting for ssh on the copy"
wait_until 300 "the copy to answer" onfar guest_ready copy

log "checking the copy"
check_copy copy "$moment"

# The copy VM wrote to the latest disk, so the failover tool must now refuse
# to rewind from it.
if [ "$MODE" = receiver ]; then
    refusal=$(onfar oc exec -n kube-system "deploy/$RECV" -- /tmp/kubectl-failover execute --nfs-mount-path /data/repl --prepare-only --rewind-to "$first" 2>&1 || true)
    echo "$refusal"
    echo "$refusal" | grep -q "has changed since its last copy" || fail "the failover tool rewound a disk that a VM had written to"
    log "PASS: the failover tool refuses to rewind a disk that a VM has written to"
fi
