#!/usr/bin/env bash
# 21-failover — Failover of chosen VMs from a replicated copy, through the
# Failover resource and the console's Recovery page (issues #44 step 4 and #46).
#
# Two clusters. The run cluster (oc) is the SOURCE: it holds two running VMs
# (vm-a and vm-b, each with a root and a data disk) and a plain volume, on one
# pool, replicated by one ReplicationPolicy through a receiver to the DESTINATION
# (DR) cluster. RT_DR_KUBECONFIG is the admin kubeconfig of the DR cluster
# (remote.sh sets it when you give RT_DR_CLUSTER). With one cluster the stage
# skips: a failed-over SubVolume has the name of the source one, and the two
# would collide.
#
# Checks, in this order (the webhook names the first thing wrong, so the order
# matters):
#   1. A Plan for all VMs lists both VMs and the plain volume.
#   2. A Plan with vms [vm-a] lists vm-a and its two disks only.
#   3. An Execute for vm-b is refused: only a Plan for [vm-a] exists. Nothing is made.
#   4. A Plan for vm-a with rewindTo an older moment shows that moment and two disks to rewind.
#   5. The Execute for vm-a is Done. It makes only vm-a's PVCs, PVs and SubVolumes; only its
#      two disks are rewound on the share.
#   6. vm-a boots on the DR cluster from the rewound disks: no XFS errors, every logged file
#      has its checksum, and no file is newer than the chosen moment.
#   7. A second Execute for vm-a is refused as already failed over.
#   8. A Plan with vms [vm-b] lists vm-b only.
#   9. vm-b is failed over in the console's Recovery page (a Playwright test in a local
#      console with a token for failovers, see console-plugin/integration-tests/tests/recovery.spec.ts):
#      the VM table, the detail view, the typed VM name, and the progress to Done. Without
#      podman (or the console plugin) the Execute is made by the API instead.
#  10. vm-b boots on the DR cluster at its newest moment, with the same file checks.
#
# What the stage needs:
#   - RT_DR_KUBECONFIG: admin kubeconfig of a second cluster with this driver (v0.15 or newer:
#     the Failover resource, its controller and webhook) and KubeVirt. Both clusters have
#     internet access (the CentOS image is downloaded on the source cluster). The DR
#     cluster needs a route domain for the receiver.
#   - oc, virtctl, jq, ssh-keygen, openssl on the machine that runs the stage.
#   - Browser part: podman, and the console plugin Deployment on the DR cluster.
#   - Time: about 3 hours. Two pools (5 to 15 minutes each), the image copy, a first and a
#     second VM copy of two VMs (the schedule is 10 minutes), a rewind of two disks and
#     the VM boots.
#   - Space: a 100 GB pool on the source, a 200 GB pool on DR; the stage makes both.
# The stage makes everything with names that start with rt-<run id> and deletes only those.
# It never touches other namespaces on either cluster (for example ovl-test).
#
# The receiver's data claim is an ordinary claim of the DR pool, as in the tutorials: a CSI
# volume of the driver. The failover controller reads the share address from its volume
# attributes, so an Execute (and the console) needs no spec.drShareIP or spec.drExportPath.
# The policy uses destinationBasePath "/" because the console's Recovery page does not set
# spec.basePath; the copy is then at the root of the claim, at <claim>/pvcs.

RT_STAGE_TIMEOUT=14400

PREFIX=$(rt_name fo)
NS=$PREFIX
SRC_POOL=$PREFIX-src
DST_POOL=$PREFIX-dst
POLICY=$PREFIX-policy
RECV=$PREFIX-recv
RECV_LABEL="pool-csi-test=$RECV"
VM_A="vm-a"
VM_B="vm-b"
PLAIN=plain
SCHEDULE="${RT_FAILOVER_SCHEDULE:-10m}"
COPY_TIMEOUT="${RT_FAILOVER_COPY_TIMEOUT:-5400}"
IMAGE_URL="${RT_FAILOVER_IMAGE_URL:-https://cloud.centos.org/centos/9-stream/x86_64/images/CentOS-Stream-GenericCloud-9-latest.x86_64.qcow2}"
ROOT_GI=12
DATA_GI=3
# cleanup runs in its own shell, so the key lives at a path both can compute.
KEY_DIR="${TMPDIR:-/tmp}/$PREFIX-key"
DR_KUBECONFIG="${RT_DR_KUBECONFIG:-}"

# The browser part.
PLAYWRIGHT_VERSION="${RT_PLAYWRIGHT_VERSION:-1.50.1}"
PLAYWRIGHT_IMAGE="${RT_PLAYWRIGHT_IMAGE:-mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-jammy}"
CONSOLE_IMAGE="${RT_CONSOLE_IMAGE:-quay.io/openshift/origin-console:4.21}"
PLUGIN_NAME=ibm-vpc-file-pool-csi
PLUGIN_PORT=9301
CONSOLE_PORT=9300
TEST_DIR="$(cd "$(dirname "$RT_LIB")/../../console-plugin/integration-tests" 2>/dev/null && pwd)"
SHOTS="$RT_OUT/failover-screenshots"
SA=failover-console
TOKEN_FILE="${TMPDIR:-/tmp}/rt-$RT_ID-failover-token"
PLUGIN_CONTAINER="rt-$RT_ID-fo-plugin"
CONSOLE_CONTAINER="rt-$RT_ID-fo-console"

# --- Helpers -----------------------------------------------------------------

# dr COMMAND... — run COMMAND (oc, virtctl, or a function of this file) against the DR cluster.
dr() { KUBECONFIG="$DR_KUBECONFIG" "$@"; }

# stop MESSAGE — a step that every later step needs has failed. Record why and end the stage.
stop() { check "$1" false; exit 0; }

# fo_name SUFFIX — name of a Failover object (cluster scoped). Kept short: its Job is failover-<name>.
fo_name() { rt_name "fo$1"; }

fo_field() { dr oc get failover "$1" -o jsonpath="{$2}" 2>/dev/null; }
fo_ended() { case "$(fo_field "$1" .status.phase)" in Planned | Done | Failed) return 0 ;; *) return 1 ;; esac; }

# make_failover NAME ACTION [SPEC LINE]... — create a Failover that reads the receiver's claim.
# Each SPEC LINE is a line of the spec, such as 'vms: ["ns/vm"]'. Prints the API server's answer.
make_failover() {
    local name=$1 action=$2 line
    shift 2
    {
        cat <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: Failover
metadata:
  name: $name
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  action: $action
  source: $RECV-data
YAML
        for line in "$@"; do echo "  $line"; done
    } | dr oc create -f - 2>&1
}

# finish_failover NAME PHASE SECONDS — wait for NAME to end and say whether it ended in PHASE.
# Prints the driver's message when it did not.
finish_failover() {
    local name=$1 want=$2 secs=$3 got
    wait_until "$secs" 10 fo_ended "$name" || { echo "Failover $name did not end within ${secs}s (phase: $(fo_field "$name" .status.phase))"; return 1; }
    got=$(fo_field "$name" .status.phase)
    [ "$got" = "$want" ] && return 0
    echo "Failover $name ended in phase $got, expected $want. Message: $(fo_field "$name" .status.message)"
    return 1
}

# plan_ok NAME [SPEC LINE]... — make a Plan and wait until it is Planned. A Plan that fails because
# a copy is still being applied is made again by the caller (wait_until).
plan_ok() {
    local name=$1
    make_failover "$@" || return 1
    finish_failover "$name" Planned 900
}

plan_retry_n=0
# plan_until_ok BASE [SPEC LINE]... — plan again (new name each try) until it works; the name that worked is in PLAN_NAME.
PLAN_NAME=""
plan_until_ok() {
    local base=$1
    shift
    plan_attempt() {
        plan_retry_n=$((plan_retry_n + 1))
        PLAN_NAME="$base-$plan_retry_n"
        plan_ok "$PLAN_NAME" Plan "$@"
    }
    wait_until 2400 30 plan_attempt "$@"
}

# plan_without_copies NAME — a Plan on a base path with no copies ends Failed, and the message says where it looked.
plan_without_copies() {
    local name=$1 msg
    make_failover "$name" Plan 'basePath: /nothing-here' || return 1
    finish_failover "$name" Failed 600 >/dev/null || { echo "the Plan did not end Failed (phase: $(fo_field "$name" .status.phase))"; return 1; }
    msg=$(fo_field "$name" .status.message)
    echo "message: $msg"
    echo "$msg" | grep -qF 'destinationBasePath' && echo "$msg" | grep -qF '/nothing-here/pvcs'
}

# refused TEXT NAME ACTION [SPEC LINE]... — the API server refuses to create the Failover, with TEXT in the answer.
refused() {
    local text=$1 out
    shift
    if out=$(make_failover "$@"); then
        echo "NOT refused: the Failover was created. Answer: $out"
        dr oc delete failover "$1" --ignore-not-found >/dev/null 2>&1
        return 1
    fi
    echo "answer: $out"
    echo "$out" | grep -qF -- "$text" || { echo "refused, but the answer does not contain: $text"; return 1; }
}

# --- Cluster objects ---------------------------------------------------------

make_pool() { # make_pool NAME SIZE_GB  (on the cluster set by dr, or the run cluster)
    oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $1
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  zone: $(detect_zone)
  profile: dp2
  shareSizeGB: $2
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

both_pools_ready() { pool_is_ready "$SRC_POOL" && dr pool_is_ready "$DST_POOL"; }

make_namespaces() {
    oc create namespace "$NS" >/dev/null && oc label namespace "$NS" "$(rt_labels)" >/dev/null || return 1
    oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null || return 1
    dr oc create namespace "$NS" >/dev/null && dr oc label namespace "$NS" "$(rt_labels)" >/dev/null
}

# source_claims — the claims of the two VMs and the plain volume, on the source pool.
source_claims() {
    local name size
    for spec in a-root:$ROOT_GI b-root:$ROOT_GI a-data:$DATA_GI b-data:$DATA_GI $PLAIN:1; do
        name=${spec%:*}
        size=${spec#*:}
        oc apply -n "$NS" -f - >/dev/null <<YAML || return 1
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $name
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  storageClassName: $SRC_POOL
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources:
    requests:
      storage: ${size}Gi
YAML
    done
}

# convert_job — one download of the CentOS image, converted to a raw disk for each VM.
convert_job() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: convert
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      securityContext:
        runAsUser: 0
      containers:
        - name: c
          image: quay.io/centos/centos:stream9
          command: [/bin/bash, -ec]
          args:
            - |
              dnf -y -q install qemu-img
              curl -sSfL -o /tmp/i.qcow2 "$IMAGE_URL"
              for d in a b; do
                qemu-img convert -f qcow2 -O raw /tmp/i.qcow2 /disk-\$d/disk.img
                qemu-img resize -f raw /disk-\$d/disk.img 11G
                chown 107:107 /disk-\$d/disk.img
              done
          resources:
            limits: {ephemeral-storage: 4Gi, memory: 2Gi}
            requests: {memory: 1Gi}
          volumeMounts:
            - {name: a, mountPath: /disk-a}
            - {name: b, mountPath: /disk-b}
      volumes:
        - {name: a, persistentVolumeClaim: {claimName: a-root}}
        - {name: b, persistentVolumeClaim: {claimName: b-root}}
YAML
}
convert_done() { [ "$(oc get job -n "$NS" convert -o jsonpath='{.status.succeeded}' 2>/dev/null)" = 1 ]; }

# vm_yaml NAME ROOT_CLAIM DATA_CLAIM CLOUDINIT(yes|no) — a CentOS VM that writes a file twice a second to its
# data disk and logs "<n> <sha256> <time>" on its root disk. The copy of the disks carries the writer,
# so a failed-over VM starts it again at boot and needs no cloud-init.
vm_yaml() {
    local pubkey
    pubkey=$(cat "$KEY_DIR/key.pub")
    cat <<YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: $1
  namespace: $NS
  labels:
    $RT_LABEL: "$RT_ID"
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
$( [ "$4" = yes ] && echo "            - {name: cloudinit, disk: {bus: virtio}}" )
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
              ssh_authorized_keys: ["$pubkey"]
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
                      sleep 0.5
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

agent_connected() { # agent_connected VM  (on the cluster set by dr, or the run cluster)
    [ "$(oc get vmi -n "$NS" "$1" -o jsonpath='{.status.conditions[?(@.type=="AgentConnected")].status}' 2>/dev/null)" = "True" ]
}

vssh() { # vssh VM COMMAND  (on the cluster set by dr, or the run cluster)
    virtctl ssh -n "$NS" "cloud-user@vmi/$1" -i "$KEY_DIR/key" \
        -t "-o StrictHostKeyChecking=no" -t "-o UserKnownHostsFile=/dev/null" \
        -t "-o LogLevel=ERROR" -t "-o IdentitiesOnly=yes" -c "$2"
}
guest_answers() { vssh "$1" true >/dev/null 2>&1; }
writer_logged() { vssh "$1" 'test -s /var/log/writer.log' >/dev/null 2>&1; }

# guest_check VM MOMENT — on the DR cluster: the VM that started from failed-over disks shows no XFS
# errors, every file of the writer's log is on the data disk with its checksum, files of the moment are
# present, and no file was written between MOMENT and the time the VM booted. The writer restarts at
# boot and writes again, so only the time between the moment and the boot says which disk the VM used.
guest_check() {
    local vm=$1 at=$2 script out
    script=$(cat <<'GUEST'
systemctl stop writer
m=$(date -d "$AT" +%s)
boot=$(date -d "$(uptime -s)" +%s)
echo "XFS=$(journalctl -k --no-pager | grep -ci 'xfs.*\(corrupt\|error\)' || true)"
echo "UPTO=$(awk -v m="$m" '$3 <= m + 10 {c++} END {print c+0}' /var/log/writer.log)"
echo "GAP=$(awk -v m="$m" -v b="$boot" '$3 > m + 10 && $3 < b {c++} END {print c+0}' /var/log/writer.log)"
echo "LAST_BEFORE_BOOT=$(awk -v b="$boot" '$3 < b {t=$3} END {print t+0}' /var/log/writer.log) MOMENT=$m BOOT=$boot"
while read -r n s t; do echo "$s  /data/f$n"; done < /var/log/writer.log | sha256sum -c --quiet && echo CHECKSUMS_OK
GUEST
)
    out=$(dr vssh "$vm" "echo $(printf 'AT=%q\n%s\n' "$at" "$script" | base64 | tr -d '\n') | base64 -d | sudo bash")
    echo "$out"
    echo "$out" | grep -qx 'XFS=0' || { echo "$vm: XFS errors in the kernel log"; return 1; }
    echo "$out" | grep -q '^UPTO=[1-9]' || { echo "$vm: the log has no file up to the moment $at"; return 1; }
    echo "$out" | grep -qx 'GAP=0' || { echo "$vm: files were written after the moment $at and before the boot: the VM did not start from that moment"; return 1; }
    echo "$out" | grep -qx 'CHECKSUMS_OK' || { echo "$vm: a logged file is missing from the data disk or its checksum differs"; return 1; }
}

# --- The receiver on the DR cluster -------------------------------------------

start_receiver() {
    local image dir token
    image=$(dr oc get deploy -n "$RT_DRIVER_NS" -l "app.kubernetes.io/component=controller" \
        -o jsonpath='{.items[0].spec.template.spec.containers[?(@.name=="csi-controller")].image}')
    dir=$(mktemp -d)
    RECV_HOST=$RECV-$RT_DRIVER_NS.$(dr oc get ingresses.config cluster -o jsonpath='{.spec.domain}')
    openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=$RECV" \
        -addext "subjectAltName=DNS:$RECV.$RT_DRIVER_NS.svc,DNS:$RECV_HOST" \
        -keyout "$dir/tls.key" -out "$dir/tls.crt" 2>/dev/null || return 1
    token=$(openssl rand -hex 24)
    dr oc create secret generic "$RECV-tls" -n "$RT_DRIVER_NS" --from-file=tls.crt="$dir/tls.crt" --from-file=tls.key="$dir/tls.key" || return 1
    dr oc create secret generic "$RECV-ca" -n "$RT_DRIVER_NS" --from-file=ca.crt="$dir/tls.crt" || return 1
    dr oc create secret generic "$RECV-token" -n "$RT_DRIVER_NS" --from-literal=token="$token" || return 1
    dr oc label secret -n "$RT_DRIVER_NS" "$RECV-tls" "$RECV-ca" "$RECV-token" "$RECV_LABEL" || return 1
    # The source cluster needs the same CA and token to call the receiver.
    oc create secret generic "$RECV-ca" -n "$RT_DRIVER_NS" --from-file=ca.crt="$dir/tls.crt" || return 1
    oc create secret generic "$RECV-token" -n "$RT_DRIVER_NS" --from-literal=token="$token" || return 1
    oc label secret -n "$RT_DRIVER_NS" "$RECV-ca" "$RECV-token" "$RECV_LABEL" || return 1
    rm -rf "$dir"

    # The data claim is a claim of the DR pool; the receiver writes into its folder.
    dr oc apply -f - >/dev/null <<YAML || return 1
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $RECV-data, namespace: $RT_DRIVER_NS, labels: {pool-csi-test: $RECV}}
spec:
  storageClassName: $DST_POOL
  accessModes: [ReadWriteMany]
  volumeMode: Filesystem
  resources: {requests: {storage: 80Gi}}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $RECV
  namespace: $RT_DRIVER_NS
  labels:
    pool-csi-test: $RECV
    app.kubernetes.io/component: replication-receiver
spec:
  replicas: 1
  selector: {matchLabels: {pool-csi-test-app: $RECV}}
  template:
    metadata:
      labels:
        pool-csi-test-app: $RECV
        app.kubernetes.io/component: replication-receiver
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
metadata: {name: $RECV, namespace: $RT_DRIVER_NS, labels: {pool-csi-test: $RECV}}
spec:
  selector: {pool-csi-test-app: $RECV}
  ports: [{name: https, port: 8443, targetPort: https}]
---
apiVersion: route.openshift.io/v1
kind: Route
metadata: {name: $RECV, namespace: $RT_DRIVER_NS, labels: {pool-csi-test: $RECV}}
spec:
  host: $RECV_HOST
  to: {kind: Service, name: $RECV}
  port: {targetPort: https}
  tls: {termination: passthrough}
YAML
    wait_until 600 5 dr data_is_bound || return 1
    wait_until 300 10 dr oc rollout status "deploy/$RECV" -n "$RT_DRIVER_NS" --timeout=10s
}
data_is_bound() { [ "$(oc get pvc -n "$RT_DRIVER_NS" "$RECV-data" -o jsonpath='{.status.phase}')" = Bound ]; }

claims_bound() { [ "$(oc get pvc -n "$NS" -o json | jq '[.items[] | select(.status.phase == "Bound")] | length')" = 5 ]; }
both_agents_connected() { agent_connected "$VM_A" && agent_connected "$VM_B"; }
both_writers_logging() { guest_answers "$VM_A" && guest_answers "$VM_B" && writer_logged "$VM_A" && writer_logged "$VM_B"; }
plain_writer_done() { [ "$(oc get pod -n "$NS" plain-writer -o jsonpath='{.status.phase}' 2>/dev/null)" = Succeeded ]; }

# epoch_of TIME — seconds since 1970 of an RFC 3339 time (UTC, as the Failover status writes it).
epoch_of() { jq -rn --arg t "$1" '$t | fromdateiso8601'; }

# --- Replication --------------------------------------------------------------

policy_json() { oc get replicationpolicy "$POLICY" -o json; }

# every_volume_copied — all five volumes have a finished copy and no VM copy is running.
every_volume_copied() {
    local st
    st=$(policy_json)
    echo "$st" | jq -c '[.status.subVolumeStatuses[]? | {sv: .subVolumeName[0:12], phase: .vmCopy.phase, moment: .copyMoment, synced: .lastSyncTime, err: .lastError}]'
    # A VM disk records the moment of its copy; a plain volume records only when its copy finished.
    [ "$(echo "$st" | jq '[.status.subVolumeStatuses[]? | select(.vmCopy == null and (.copyMoment != null or (.lastSyncTime != null and (.lastError // "") == "")))] | length')" = 5 ]
}

# vm_disks_copied_again — each of the four VM disks has a newer copy moment than the one in FIRST_MOMENTS.
FIRST_MOMENTS=""
VM_DISK_SVS=""
vm_disks_copied_again() {
    policy_json | jq -e --argjson first "$FIRST_MOMENTS" --argjson disks "$VM_DISK_SVS" '
        [.status.subVolumeStatuses[]? | select(.vmCopy == null and (.subVolumeName as $s | $disks | index($s)) != null)
            | select(.copyMoment != $first[.subVolumeName])] | length == 4' >/dev/null
}
no_copy_running() { [ "$(policy_json | jq '[.status.subVolumeStatuses[]? | select(.vmCopy != null)] | length')" = 0 ]; }

# --- Facts from a Plan -------------------------------------------------------

# plan_vm_names NAME — the VMs of a Plan, sorted, one line.
plan_vm_names() { dr oc get failover "$1" -o json | jq -r '[.status.vms[]?.name] | sort | join(",")'; }
plan_pvc_names() { dr oc get failover "$1" -o json | jq -r '[.status.volumes[]?.pvcName] | sort | join(",")'; }
plan_svs_of() { # plan_svs_of PLAN VM — volume names of one VM's disks, sorted, one line
    dr oc get failover "$1" -o json | jq -r --arg vm "$2" '[.status.vms[]? | select(.name == $vm) | .disks[].subVolume] | sort | join(",")'
}
plan_moment_of() { dr oc get failover "$1" -o json | jq -r --arg vm "$2" '.status.vms[]? | select(.name == $vm) | .moment'; }
plan_moment_n() { dr oc get failover "$1" -o json | jq -r --arg vm "$2" --argjson n "$3" '.status.vms[]? | select(.name == $vm) | .moments[$n] // empty'; }
plan_rewinds_of() { dr oc get failover "$1" -o json | jq -r --arg vm "$2" '[.status.vms[]? | select(.name == $vm) | .disks[].rewind] | map(tostring) | join(",")'; }

expect_eq() { # expect_eq WHAT WANT GOT
    echo "$1: want '$2', got '$3'"
    [ "$2" = "$3" ]
}

# --- What exists on the DR cluster -------------------------------------------

dr_pvcs_in_ns() { dr oc get pvc -n "$NS" -o json | jq -r '[.items[].metadata.name] | sort | join(",")'; }
dr_pvs_of_ns() { dr oc get pv -o json | jq -r --arg ns "$NS" '[.items[] | select(.spec.claimRef.namespace == $ns) | .metadata.name] | sort | join(",")'; }
dr_failover_svs_of_ns() {
    dr oc get subvolumes -l storage.ibmcloud.io/failover-source -o json |
        jq -r --arg ns "$NS" '[.items[] | select(.spec.pvcNamespace == $ns) | .metadata.name] | sort | join(",")'
}
dr_pvcs_bound() { [ "$(dr oc get pvc -n "$NS" -o json | jq '[.items[] | select(.status.phase != "Bound")] | length')" = 0 ]; }
# rewound_folders — the rewound disk folders on the receiver's share, sorted, one line.
rewound_folders() {
    dr oc exec -n "$RT_DRIVER_NS" "deploy/$RECV" -- sh -c 'ls -1 /data/pvcs 2>/dev/null | grep "\.rewind-" | LC_ALL=C sort | paste -sd, -'
}
sv_of_claim() { oc get pvc -n "$NS" "$1" -o jsonpath='{.spec.volumeName}'; }

# --- The browser part --------------------------------------------------------

console_is_up() { curl -fsS "http://localhost:$CONSOLE_PORT/health" >/dev/null 2>&1; }

# failover_admin_role — the chart's cluster role for failovers (its name ends in -failover-admin).
failover_admin_role() { dr oc get clusterrole -o name | sed 's|^clusterrole.rbac.authorization.k8s.io/||' | grep -- '-failover-admin$' | head -1; }

# make_failover_token ROLE — a ServiceAccount on the DR cluster that reads like cluster-reader, reads
# the driver's resources, and has the failover-admin role. It has no other write right. Prints a 2-hour token.
make_failover_token() {
    local role=$1
    dr oc create serviceaccount "$SA" -n "$NS" >/dev/null || return 1
    dr oc apply -f - >/dev/null <<YAML || return 1
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: $NS-read
  labels:
    $RT_LABEL: "$RT_ID"
rules:
  - apiGroups: ["storage.ibmcloud.io"]
    resources: ["*"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS-reader
  labels:
    $RT_LABEL: "$RT_ID"
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-reader}
subjects: [{kind: ServiceAccount, name: $SA, namespace: $NS}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS-read
  labels:
    $RT_LABEL: "$RT_ID"
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: $NS-read}
subjects: [{kind: ServiceAccount, name: $SA, namespace: $NS}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS-failover
  labels:
    $RT_LABEL: "$RT_ID"
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: $role}
subjects: [{kind: ServiceAccount, name: $SA, namespace: $NS}]
YAML
    dr oc create token "$SA" -n "$NS" --duration=4h
}
sa_can() { dr oc auth can-i "$@" --as="system:serviceaccount:$NS:$SA" >/dev/null 2>&1; }
token_is_limited() {
    sa_can create failovers.storage.ibmcloud.io &&
        ! sa_can create persistentvolumeclaims -n "$NS" &&
        ! sa_can create pods -n default &&
        ! sa_can delete filesharepools.storage.ibmcloud.io
}

# browser_part — Fail over vm-b in the Recovery page. Prints why it could not run, and returns 2 if it did not
# run (the caller then uses the API); returns 1 if it ran and a check failed; 0 if it ran and passed.
browser_part() {
    local plugin_image role api work ca_args=() counts passed failed
    command -v podman >/dev/null || { echo "podman is not installed"; return 2; }
    [ -d "$TEST_DIR" ] || { echo "the console plugin tests are not in this copy of the scripts"; return 2; }
    plugin_image=$(dr oc get deploy -n "$RT_DRIVER_NS" -l "app.kubernetes.io/component=console-plugin" -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null)
    [ -n "$plugin_image" ] || { echo "the console plugin is not installed on the DR cluster"; return 2; }
    role=$(failover_admin_role)
    [ -n "$role" ] || { echo "the DR cluster has no cluster role that ends in -failover-admin (is the chart older than the Failover resource?)"; return 2; }

    (umask 077; make_failover_token "$role" >"$TOKEN_FILE")
    check "the console token can make failovers and nothing else" token_is_limited

    api=$(dr oc whoami --show-server)
    work=$(mktemp -d)
    cp -R "$TEST_DIR" "$work/integration-tests"
    mkdir -p "$SHOTS" "$RT_OUT/failover-test-results"
    dr oc config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$work/ca.crt" 2>/dev/null
    if [ -s "$work/ca.crt" ]; then
        ca_args=(-v "$work/ca.crt:/ca.crt:ro,Z" -e BRIDGE_K8S_MODE_OFF_CLUSTER_CA_FILE=/ca.crt)
    fi
    cat >"$work/nginx.conf" <<NGINX
pid /tmp/nginx.pid;
events { worker_connections 64; }
http {
    include /etc/nginx/mime.types;
    server {
        listen $PLUGIN_PORT;
        root /opt/app-root/src;
        location / { try_files \$uri \$uri/ =404; }
    }
}
NGINX
    # The console points at the DR cluster and signs in nobody; every call uses the token. The plugin image is
    # the one the DR cluster runs. The token goes in a file, not on the command line.
    podman run -d --rm --network host --name "$PLUGIN_CONTAINER" \
        -v "$work/nginx.conf:/etc/nginx/nginx.conf:ro,Z" "$plugin_image" >/dev/null
    (umask 077; printf 'BRIDGE_K8S_AUTH_BEARER_TOKEN=%s\n' "$(cat "$TOKEN_FILE")" >"$work/console.env")
    podman run -d --rm --network host --name "$CONSOLE_CONTAINER" \
        -e BRIDGE_LISTEN="http://127.0.0.1:$CONSOLE_PORT" \
        -e BRIDGE_USER_AUTH=disabled \
        -e BRIDGE_K8S_MODE=off-cluster \
        -e BRIDGE_K8S_MODE_OFF_CLUSTER_ENDPOINT="$api" \
        "${ca_args[@]}" \
        -e BRIDGE_K8S_AUTH=bearer-token \
        --env-file "$work/console.env" \
        -e BRIDGE_PLUGINS="$PLUGIN_NAME=http://127.0.0.1:$PLUGIN_PORT" \
        "$CONSOLE_IMAGE" >/dev/null
    check "the local console for the DR cluster starts" wait_until 120 3 console_is_up

    podman run --rm --network host \
        -v "$work:/work:Z" -v "$SHOTS:/screenshots:Z" -v "$RT_OUT/failover-test-results:/work/test-results:Z" \
        -e CONSOLE_URL="http://localhost:$CONSOLE_PORT" -e CONSOLE_AUTH=none \
        -e SCREENSHOT_DIR=/screenshots -e PLAYWRIGHT_JSON_OUTPUT_NAME="/work/result-recovery.json" \
        -e RECOVERY_VM="$NS/$VM_B" -e RECOVERY_OTHER_VM="$NS/$VM_A" -e RECOVERY_SOURCE="$RECV-data" \
        -e RECOVERY_POOL="$DST_POOL" -e RECOVERY_PVCS="$NS/b-root,$NS/b-data" \
        -w /work "$PLAYWRIGHT_IMAGE" \
        sh -c "npm init -y >/dev/null && npm install --no-save --silent @playwright/test@$PLAYWRIGHT_VERSION && npx playwright test --config integration-tests/playwright.config.ts --reporter=json integration-tests/tests/recovery.spec.ts" \
        >"$RT_OUT/failover-playwright.log" 2>&1 || true
    podman logs "$CONSOLE_CONTAINER" >"$RT_OUT/failover-console-bridge.log" 2>&1 || true

    check "Playwright wrote a result file for the Recovery page" test -s "$work/result-recovery.json"
    if [ -s "$work/result-recovery.json" ]; then
        counts=$(jq -r '"\(.stats.expected) \(.stats.unexpected + .stats.flaky)"' "$work/result-recovery.json")
        passed=${counts% *}
        failed=${counts#* }
        echo "recovery browser tests: $passed passed, $failed failed (details: failover-playwright.log, failover-test-results)"
        check "no Recovery page browser test failed ($passed passed, $failed failed)" test "$failed" -eq 0
        check "the Recovery page browser tests ran (4 expected, $passed passed)" test "$passed" -ge 4
        [ "$failed" -eq 0 ] || { rm -rf "$work"; return 1; }
    else
        rm -rf "$work"
        return 1
    fi
    rm -rf "$work"
}

# console_executes_for_b — the Executes made by the console for vm-b: how many, and are they all Done.
console_executes_for_b() {
    dr oc get failover -o json | jq --arg vm "$NS/$VM_B" '[.items[] | select(.spec.action == "Execute" and .spec.vms == [$vm])]'
}

# --- The stage ---------------------------------------------------------------

run() {
    local tool a_svs plan_all plan_a plan_b m_old m_new out rewound_a rewind_epoch

    for tool in oc virtctl jq ssh-keygen openssl base64; do
        command -v "$tool" >/dev/null || skip "needs $tool"
    done
    [ -n "$DR_KUBECONFIG" ] || skip "RT_DR_KUBECONFIG is not set: this stage needs the DR cluster's admin kubeconfig (remote.sh: RT_DR_CLUSTER=NAME)"
    [ -r "$DR_KUBECONFIG" ] || skip "RT_DR_KUBECONFIG is not a readable file: $DR_KUBECONFIG"
    [ "$(oc whoami --show-server)" != "$(dr oc whoami --show-server)" ] ||
        skip "RT_DR_KUBECONFIG points at the same cluster as the run: a failed-over SubVolume would have the name of the source one"
    oc get crd virtualmachines.kubevirt.io >/dev/null 2>&1 || skip "KubeVirt is not installed on the run cluster"
    dr oc get crd virtualmachines.kubevirt.io >/dev/null 2>&1 || skip "KubeVirt is not installed on the DR cluster"
    dr oc get crd failovers.storage.ibmcloud.io >/dev/null 2>&1 || skip "the Failover resource is not installed on the DR cluster (driver older than v0.15?)"
    mkdir -p "$KEY_DIR" && chmod 700 "$KEY_DIR"
    ssh-keygen -q -t ed25519 -N '' -f "$KEY_DIR/key" || skip "cannot make an ssh key"

    # --- Setup -------------------------------------------------------------
    make_pool "$SRC_POOL" 100 >/dev/null
    dr make_pool "$DST_POOL" 200 >/dev/null
    check "both pools reach Ready (source $SRC_POOL, DR $DST_POOL)" wait_until 1800 15 both_pools_ready
    both_pools_ready || stop "a pool did not become Ready: source '$(pool_field "$SRC_POOL" .status.phase)', DR '$(dr pool_field "$DST_POOL" .status.phase)'"
    check "the namespace $NS exists on both clusters" make_namespaces

    check "the receiver runs on the DR cluster, with a claim of the DR pool as its data" start_receiver
    wait_until 60 5 dr oc get deploy -n "$RT_DRIVER_NS" "$RECV" >/dev/null || stop "the receiver was not made"

    source_claims || stop "the claims on the source pool were not made"
    check "the five claims are Bound on the source pool" wait_until 600 5 claims_bound
    convert_job
    check "the CentOS image is converted to the disks of both VMs" wait_until 1800 15 convert_done
    convert_done || stop "the image conversion Job did not finish: $(oc logs -n "$NS" job/convert --tail=3 2>&1 | tr '\n' ' ')"

    vm_yaml "$VM_A" a-root a-data yes | oc apply -f - >/dev/null
    vm_yaml "$VM_B" b-root b-data yes | oc apply -f - >/dev/null
    check "both source VMs report a connected guest agent" wait_until 1200 10 both_agents_connected
    check "both source VMs answer on ssh and their writers are logging" wait_until 600 10 both_writers_logging
    both_writers_logging || stop "a source VM does not run or its writer logs nothing"

    # The plain volume: a file and its checksum, written by a pod that then ends. It is not a VM disk.
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: plain-writer
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  restartPolicy: Never
  containers:
    - name: main
      image: $RT_IMAGE
      command: ["/bin/sh", "-c", "head -c 4194304 /dev/urandom > /data/payload && cd /data && sha256sum payload > payload.sha256"]
      volumeMounts:
        - {name: data, mountPath: /data}
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: $PLAIN}
YAML
    check "the plain volume gets its file" wait_until 300 5 plain_writer_done
    oc delete pod -n "$NS" plain-writer --ignore-not-found --wait=true >/dev/null

    oc apply -f - >/dev/null <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: ReplicationPolicy
metadata:
  name: $POLICY
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  sourcePoolName: $SRC_POOL
  destinationEndpoint: https://$RECV_HOST
  destinationAuthSecretRef: $RECV-token
  destinationCACertSecretRef: $RECV-ca
  destinationBasePath: /
  schedule: $SCHEDULE
  maxRetries: 3
  maxParallelSyncs: 3
YAML
    check "every volume has a first copy (up to $((COPY_TIMEOUT / 60)) minutes)" wait_until "$COPY_TIMEOUT" 20 every_volume_copied
    every_volume_copied >/dev/null || stop "the first copy did not finish: $(policy_json | jq -c '[.status.subVolumeStatuses[]? | {sv: .subVolumeName[0:12], err: .lastError}]')"
    FIRST_MOMENTS=$(policy_json | jq -c '[.status.subVolumeStatuses[] | {key: .subVolumeName, value: .copyMoment}] | from_entries')
    VM_DISK_SVS=$(printf '%s\n' "$(sv_of_claim a-root)" "$(sv_of_claim a-data)" "$(sv_of_claim b-root)" "$(sv_of_claim b-data)" | jq -Rsc 'split("\n") | map(select(. != ""))')
    check "the receiver holds the copy at the root of its claim (the failover Job reads <claim>/pvcs)" \
        dr oc exec -n "$RT_DRIVER_NS" "deploy/$RECV" -- test -d /data/pvcs

    check "all four VM disks are copied a second time, so each keeps a restore point (schedule $SCHEDULE)" wait_until "$COPY_TIMEOUT" 20 vm_disks_copied_again
    # Pause at once, so the copy does not change under the Plans. A copy that has started finishes.
    oc annotate replicationpolicy "$POLICY" storage.ibmcloud.io/paused=true --overwrite >/dev/null
    check "no copy is running after the pause" wait_until 1800 15 no_copy_running
    no_copy_running || stop "a VM copy is still running 30 minutes after the pause"

    # A Plan on a folder with no copies fails and says so; it is not an empty "Planned".
    check "a Plan with basePath /nothing-here fails and names destinationBasePath" plan_without_copies "$(fo_name nb)"

    a_svs=$(printf '%s\n' "$(sv_of_claim a-root)" "$(sv_of_claim a-data)" | LC_ALL=C sort | paste -sd, -)

    # --- 1. Plan for all VMs -------------------------------------------------
    plan_until_ok "$(fo_name p)" || stop "a Plan for all VMs does not finish: $(fo_field "$PLAN_NAME" .status.message)"
    plan_all=$PLAN_NAME
    check "a Plan for all VMs lists $NS/$VM_A and $NS/$VM_B" expect_eq "VMs in the Plan" "$NS/$VM_A,$NS/$VM_B" "$(plan_vm_names "$plan_all")"
    check "a Plan for all VMs lists the plain volume besides the four VM disks" \
        expect_eq "claims in the Plan" "a-data,a-root,b-data,b-root,$PLAIN" "$(plan_pvc_names "$plan_all")"
    m_new=$(plan_moment_of "$plan_all" "$NS/$VM_A")
    m_old=$(plan_moment_n "$plan_all" "$NS/$VM_A" 1)
    echo "vm-a: newest moment $m_new, the one before it $m_old"
    [ -n "$m_old" ] || stop "$NS/$VM_A has only one moment to choose from, so there is no older moment to rewind to (moments: $(dr oc get failover "$plan_all" -o jsonpath='{.status.vms[0].moments}'))"

    # --- 2. Plan for vm-a only -----------------------------------------------
    plan_until_ok "$(fo_name pa)" "vms: [\"$NS/$VM_A\"]" || stop "a Plan for $VM_A does not finish: $(fo_field "$PLAN_NAME" .status.message)"
    plan_a=$PLAN_NAME
    check "a Plan with vms [$VM_A] lists $VM_A only" expect_eq "VMs in the Plan" "$NS/$VM_A" "$(plan_vm_names "$plan_a")"
    check "a Plan with vms [$VM_A] lists its two disks and no other volume (no vm-b disk, no plain volume)" \
        expect_eq "claims in the Plan" "a-data,a-root" "$(plan_pvc_names "$plan_a")"

    # --- 3. An Execute with a different list is refused ----------------------
    check "an Execute for $VM_B is refused, because the Plan was for $VM_A (the answer names spec.vms)" \
        refused "spec.vms" "$(fo_name xb)" Execute "vms: [\"$NS/$VM_B\"]" "drPool: $DST_POOL"
    check "the refused Execute made nothing: no claim, volume or SubVolume on the DR cluster" \
        expect_eq "claims, volumes and SubVolumes of $NS on DR" ",," "$(dr_pvcs_in_ns),$(dr_pvs_of_ns),$(dr_failover_svs_of_ns)"

    # --- 4. Plan with rewindTo -----------------------------------------------
    plan_until_ok "$(fo_name pr)" "vms: [\"$NS/$VM_A\"]" "rewindTo: \"$m_old\"" || stop "a Plan for $VM_A with rewindTo $m_old does not finish: $(fo_field "$PLAN_NAME" .status.message)"
    plan_a=$PLAN_NAME
    rewind_epoch=$(epoch_of "$m_old")
    check "a Plan with rewindTo shows the older moment for $VM_A" \
        expect_eq "moment of $VM_A (seconds since 1970)" "$rewind_epoch" "$(epoch_of "$(plan_moment_of "$plan_a" "$NS/$VM_A")")"
    check "a Plan with rewindTo says both disks of $VM_A are rewound" expect_eq "rewind flags" "true,true" "$(plan_rewinds_of "$plan_a" "$NS/$VM_A")"

    # --- 5. Execute for vm-a ---------------------------------------------------
    out=$(make_failover "$(fo_name xa)" Execute "vms: [\"$NS/$VM_A\"]" "rewindTo: \"$m_old\"" "drPool: $DST_POOL") || stop "the Execute for $VM_A was refused: $out"
    check "the Execute for $VM_A is Done (a rewind copies two disks: up to 30 minutes)" finish_failover "$(fo_name xa)" Done 3600
    check "the Execute made the claims of $VM_A only" expect_eq "claims of $NS on DR" "a-data,a-root" "$(dr_pvcs_in_ns)"
    check "the Execute made the volumes of $VM_A only (dr-<volume>)" \
        expect_eq "volumes claimed in $NS" "$(echo "$a_svs" | tr ',' '\n' | sed 's/^/dr-/' | paste -sd, -)" "$(dr_pvs_of_ns)"
    check "the Execute made the SubVolumes of $VM_A only" expect_eq "failover SubVolumes of $NS" "$a_svs" "$(dr_failover_svs_of_ns)"
    check "the Execute's result lists the two claims it made" \
        expect_eq "createdPVCs" "$NS/a-data,$NS/a-root" "$(dr oc get failover "$(fo_name xa)" -o json | jq -r '[.status.createdPVCs[]? | .namespace + "/" + .name] | sort | join(",")')"
    check "the claims of $VM_A become Bound" wait_until 300 5 dr dr_pvcs_bound
    rewound_a=$(echo "$a_svs" | tr ',' '\n' | sed "s/\$/.rewind-$rewind_epoch/" | paste -sd, -)
    check "only the two disks of $VM_A were rewound on the share" expect_eq "rewound folders" "$rewound_a" "$(rewound_folders)"

    # --- 6. vm-a boots from the rewound disks --------------------------------
    vm_yaml "$VM_A" a-root a-data no | dr oc apply -f - >/dev/null
    check "$VM_A runs on the DR cluster and its guest agent connects" wait_until 1200 10 dr agent_connected "$VM_A"
    check "$VM_A answers on ssh" wait_until 600 10 dr guest_answers "$VM_A"
    check "$VM_A on the DR cluster started from the moment $m_old: no XFS errors, all logged files with their checksums, no file after the moment" \
        guest_check "$VM_A" "$m_old"

    # --- 7. A second Execute for vm-a is refused -----------------------------
    plan_until_ok "$(fo_name pa2)" "vms: [\"$NS/$VM_A\"]" || stop "a second Plan for $VM_A does not finish: $(fo_field "$PLAN_NAME" .status.message)"
    check "a second Execute for $VM_A is refused as already failed over" \
        refused "failed over on this cluster already" "$(fo_name xa2)" Execute "vms: [\"$NS/$VM_A\"]" "drPool: $DST_POOL"

    # --- 8. Plan for vm-b ------------------------------------------------------
    plan_until_ok "$(fo_name pb)" "vms: [\"$NS/$VM_B\"]" || stop "a Plan for $VM_B does not finish: $(fo_field "$PLAN_NAME" .status.message)"
    plan_b=$PLAN_NAME
    check "a Plan with vms [$VM_B] lists $VM_B only, with its two disks" \
        expect_eq "VMs and claims in the Plan" "$NS/$VM_B b-data,b-root" "$(plan_vm_names "$plan_b") $(plan_pvc_names "$plan_b")"
    m_new=$(plan_moment_of "$plan_b" "$NS/$VM_B")

    # --- 9. vm-b fails over in the console (or by the API) --------------------------
    browser_part
    case $? in
        0) check "the console made exactly one Execute for $VM_B (the cancelled dialog made none), and it is Done" \
               expect_eq "Executes for $NS/$VM_B made by the console, and how many are Done" "1 1" \
               "$(console_executes_for_b | jq -r '"\(length) \([.[] | select(.status.phase == "Done")] | length)"')" ;;
        2) echo "The browser part did not run, so $VM_B is failed over by the API."
           not_checked "the Recovery page in a browser" "see the lines above for what is missing"
           out=$(make_failover "$(fo_name xb)" Execute "vms: [\"$NS/$VM_B\"]" "drPool: $DST_POOL") || stop "the Execute for $VM_B was refused: $out"
           check "the Execute for $VM_B is Done" finish_failover "$(fo_name xb)" Done 1800 ;;
        *) echo "The Recovery page test failed. $VM_B is not failed over by the API: a second Execute would be refused and hide the failure." ;;
    esac
    check "the claims of $VM_B exist on the DR cluster next to those of $VM_A, and nothing else" \
        expect_eq "claims of $NS on DR" "a-data,a-root,b-data,b-root" "$(dr_pvcs_in_ns)"
    check "no disk of $VM_B was rewound (it is used at its newest moment)" expect_eq "rewound folders" "$rewound_a" "$(rewound_folders)"

    # --- 10. vm-b boots at its newest moment -----------------------------------
    check "the claims of $VM_B become Bound" wait_until 300 5 dr dr_pvcs_bound
    vm_yaml "$VM_B" b-root b-data no | dr oc apply -f - >/dev/null
    check "$VM_B runs on the DR cluster and its guest agent connects" wait_until 1200 10 dr agent_connected "$VM_B"
    check "$VM_B answers on ssh" wait_until 600 10 dr guest_answers "$VM_B"
    check "$VM_B on the DR cluster started from its newest moment $m_new: no XFS errors, all logged files with their checksums, no file after the moment" \
        guest_check "$VM_B" "$m_new"
}

# --- Cleanup -----------------------------------------------------------------
# Deletes only what this run made (names that start with rt-<id>, and Failovers
# that read this run's receiver claim). The order matters: the pool goes last,
# because the failed-over SubVolumes belong to it. Waits are short: the whole
# cleanup has 10 minutes, and the final sweep deletes namespaces and pools that
# carry the run label on the run cluster.

cleanup() {
    local pv sv f
    podman stop -t 5 "$CONSOLE_CONTAINER" "$PLUGIN_CONTAINER" >/dev/null 2>&1 || true
    rm -f "$TOKEN_FILE"
    rm -rf "$KEY_DIR"

    # Source cluster: stop the copy first.
    oc delete replicationpolicy "$POLICY" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    oc delete namespace "$NS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    oc delete secret -n "$RT_DRIVER_NS" -l "$RECV_LABEL" --ignore-not-found >/dev/null 2>&1 || true
    oc delete secret -n "$RT_DRIVER_NS" "$RECV-ca" "$RECV-token" --ignore-not-found >/dev/null 2>&1 || true

    [ -r "$DR_KUBECONFIG" ] || return 0
    # DR cluster: the Failovers that read this receiver (including the console's, which carry no run label),
    # and the Jobs a timed-out one left behind.
    for f in $(dr oc get failover -o json 2>/dev/null | jq -r --arg c "$RECV-data" '.items[] | select((.status.source // .spec.source) == $c) | .metadata.name'); do
        dr oc delete failover "$f" --ignore-not-found --wait=false >/dev/null 2>&1 || true
        dr oc delete job -n "$RT_DRIVER_NS" "failover-$f" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done
    dr oc delete failover -l "$RT_LABEL=$RT_ID" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    dr oc delete clusterrolebinding,clusterrole -l "$RT_LABEL=$RT_ID" --ignore-not-found --wait=false >/dev/null 2>&1 || true

    # The namespace (the VMs and the failed-over claims), then the volumes claimed in it, then their SubVolumes.
    dr oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=240s >/dev/null 2>&1 || true
    for pv in $(dr oc get pv -o json 2>/dev/null | jq -r --arg ns "$NS" '.items[] | select(.spec.claimRef.namespace == $ns) | .metadata.name'); do
        dr oc delete pv "$pv" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done
    for sv in $(dr oc get subvolumes -l storage.ibmcloud.io/failover-source -o json 2>/dev/null | jq -r --arg ns "$NS" '.items[] | select(.spec.pvcNamespace == $ns) | .metadata.name'); do
        dr oc delete subvolume "$sv" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done
    # The receiver and its data claim.
    dr oc delete deploy,svc,route,secret,pvc -n "$RT_DRIVER_NS" -l "$RECV_LABEL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    dr oc delete pv -l "$RECV_LABEL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    dr oc delete filesharepool "$DST_POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    oc delete filesharepool "$SRC_POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
