#!/usr/bin/env bash
# run-tier.sh — One tier of the performance test: one pool with one share at a
# given IOPS, a growing number of VMs (Linux and Windows) each running a
# general-purpose load, and a report. See docs/specs/2026-10-24-performance-test-30-vms.md.
#
# Usage (KUBECONFIG points at the cluster, normally rovslab-dr-eude):
#   bash test/perf/run-tier.sh --iops 3000 [--share-gb 3900] [--steps 15,30,45]
#        [--step-minutes 45] [--settle-minutes 15] [--avg 30] [--burst 300]
#        [--burst-secs 45] [--period 600] [--windows-every 3] [--out DIR] [--keep]
#   bash test/perf/run-tier.sh --iops 3000 --print-vm 7     print the YAML of VM 7 and stop
#   bash test/perf/run-tier.sh --iops 3000 --print-user-data print the cloud-init file of the Linux VMs and stop
#   bash test/perf/run-tier.sh --iops 3000 --print-windows-config print the settings and fio arguments of the Windows VMs and stop
#   bash test/perf/run-tier.sh --iops 3000 --print-windows-start  print the PowerShell that starts a Windows VM's load and stop
#
# Small smoke run: --iops 3000 --share-gb 200 --steps 5 --step-minutes 10 --settle-minutes 2 --period 120 --burst-secs 20
# One Windows VM only: the same with --steps 1 --windows-every 1
#
# Every Nth VM (--windows-every, 3 by default, so a third) is Windows: it starts
# from the prepared image (prepare-windows-image.sh), which has fio for Windows, and
# runs the same fio jobs as a Linux VM from a config disk the run makes (ConfigMap
# perf-windows). The run starts each Windows VM's load through the guest agent.
# 0 makes every VM Linux.
#
# Everything the run makes carries the label perf-test-run=<ID>. At the end, or
# when the script stops, it deletes the VMs, the namespace and the pool, unless
# --keep is given.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# The Windows load scripts live next to the PoolBench service, which puts the same files on its VMs.
WINDOWS_DIR="$HERE/../../pkg/poolbench/guest/windows"
# shellcheck source=profile.sh
source "$HERE/profile.sh"
# shellcheck source=report.sh
source "$HERE/report.sh"
# shellcheck source=collect.sh
source "$HERE/collect.sh"

IOPS=""
SHARE_GB=3900
STEPS="15,30,45"
STEP_MIN=45
SETTLE_MIN=15
AVG=30
BURST=300
BURST_SECS=45
PERIOD=600
OUT=""
KEEP=false
PRINT_VM=""
PRINT_USER_DATA=false
PRINT_WINDOWS_CONFIG=false
PRINT_WINDOWS_START=false
WINDOWS_EVERY=3

CATALOG_IMAGE="${PERF_CATALOG_IMAGE:-centos-stream9}"
CATALOG_PREFERENCE="${PERF_CATALOG_PREFERENCE:-centos.stream9}"
INSTANCETYPE="${PERF_INSTANCETYPE:-u1.small}"
WINDOWS_IMAGE="${PERF_WINDOWS_IMAGE:-win2k22-perf}"
WINDOWS_PREFERENCE="${PERF_WINDOWS_PREFERENCE:-windows.2k22.virtio}"
IMAGES_NS=openshift-virtualization-os-images
# The copy of a catalog disk must be at least as large as the catalog disk (30 GiB).
ROOT_GB=30
# The load disk holds the test area (AREA), which is written once before the measuring starts.
AREA=3G
DATA_GB=4
LABEL=perf-test-run

while [ $# -gt 0 ]; do
    case "$1" in
        --iops) IOPS="$2"; shift 2 ;;
        --share-gb) SHARE_GB="$2"; shift 2 ;;
        --steps) STEPS="$2"; shift 2 ;;
        --step-minutes) STEP_MIN="$2"; shift 2 ;;
        --settle-minutes) SETTLE_MIN="$2"; shift 2 ;;
        --avg) AVG="$2"; shift 2 ;;
        --burst) BURST="$2"; shift 2 ;;
        --burst-secs) BURST_SECS="$2"; shift 2 ;;
        --period) PERIOD="$2"; shift 2 ;;
        --out) OUT="$2"; shift 2 ;;
        --keep) KEEP=true; shift ;;
        --print-vm) PRINT_VM="$2"; shift 2 ;;
        --print-user-data) PRINT_USER_DATA=true; shift ;;
        --print-windows-config) PRINT_WINDOWS_CONFIG=true; shift ;;
        --print-windows-start) PRINT_WINDOWS_START=true; shift ;;
        --windows-every) WINDOWS_EVERY="$2"; shift 2 ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done
[ -n "$IOPS" ] || { echo "--iops is required" >&2; exit 2; }

ID="${PERF_ID:-$(date -u +%m%d-%H%M)}"
NAME="perf-$ID"
NS="$NAME"
POOL="$NAME"
OUT="${OUT:-$HERE/out/$ID}"

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
vm_name() { printf 'perf-%03d' "$1"; }
# is_windows N — VM N is one of the Windows VMs.
is_windows() { [ "$WINDOWS_EVERY" -gt 0 ] && [ $(($1 % WINDOWS_EVERY)) -eq 0 ]; }
# windows_mix — which VMs are Windows, in words.
windows_mix() {
    case "$WINDOWS_EVERY" in
        0) echo "all VMs Linux" ;;
        1) echo "all VMs Windows" ;;
        *) echo "1 VM in $WINDOWS_EVERY Windows" ;;
    esac
}

# user_data — the cloud-init file every VM gets: it writes the load script,
# installs fio, says PERF-READY on the serial console, and starts the load. It is
# kept in a Secret because KubeVirt limits inline cloud-init to 2 KiB.
user_data() {
    printf '#cloud-config\nwrite_files:\n  - path: /root/load.sh\n    permissions: "0755"\n    content: |\n'
    perf_linux_load_script "$AVG" "$BURST" "$BURST_SECS" "$PERIOD" /dev/vdb "$AREA" | sed 's/^/      /'
    printf 'runcmd:\n  - |\n    set -e\n    dnf install -y -q fio\n    %s > /dev/null\n    echo PERF-READY > /dev/ttyS0\n  - "nohup sh /root/load.sh > /dev/null 2>&1 &"\n' "$(perf_prefill_command /dev/vdb "$AREA")"
}

# windows_settings — the settings file of the Windows load (load.ps1).
windows_settings() {
    printf 'period=%s\nserial=perfload\n' "$PERIOD"
}

# windows_config DIR — the files of the Windows VMs' config disk: the settings, the fio
# arguments of the fill and of the two phases, and the load scripts.
windows_config() {
    local dir="$1"
    windows_settings >"$dir/settings.txt"
    perf_windows_fill_args "$AREA" >"$dir/fill.args"
    perf_windows_fio_args steady "$AVG" $((PERIOD - BURST_SECS)) "$AREA" >"$dir/steady.args"
    perf_windows_fio_args burst "$BURST" "$BURST_SECS" "$AREA" >"$dir/burst.args"
    cp "$WINDOWS_DIR/load.ps1" "$WINDOWS_DIR/perf.ps1" "$dir/"
}

# windows_start_script — the PowerShell the run sends through the guest agent to start a
# Windows VM's load. It does nothing if the load runs, and fails until the config disk is
# there. (A start-up task in Windows would have been simpler, but it stayed "Queued" and never ran.)
windows_start_script() {
    cat <<'PS'
if (Get-CimInstance Win32_Process -Filter "Name = 'powershell.exe'" | Where-Object CommandLine -like '*load.ps1*') { 'running'; exit 0 }
$volume = Get-Volume | Where-Object FileSystemLabel -eq 'PERFCFG' | Select-Object -First 1
if (-not ($volume -and $volume.DriveLetter)) { 'no config disk yet'; exit 1 }
Start-Process powershell.exe -WindowStyle Hidden -ArgumentList "-NoProfile -ExecutionPolicy Bypass -File $($volume.DriveLetter):\load.ps1"
'started'
PS
}

# vm_yaml N — VM N: Linux or Windows.
vm_yaml() {
    if is_windows "$1"; then windows_vm_yaml "$1"; else linux_vm_yaml "$1"; fi
}

# data_pvc_yaml VM — the load disk of a VM, on the pool.
data_pvc_yaml() {
    cat <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $1-data
  labels:
    $LABEL: "$ID"
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $POOL
  resources:
    requests:
      storage: ${DATA_GB}Gi
YAML
}

# root_dv_yaml VM DATASOURCE — the root disk of a VM, copied from an image onto the pool.
root_dv_yaml() {
    cat <<YAML
    - metadata:
        name: $1-root
        labels:
          $LABEL: "$ID"
      spec:
        sourceRef:
          kind: DataSource
          name: $2
          namespace: $IMAGES_NS
        storage:
          storageClassName: $POOL
          resources:
            requests:
              storage: ${ROOT_GB}Gi
YAML
}

# linux_vm_yaml N — a Linux VM and its two disks. The root disk is copied from the catalog
# image and the second disk (/dev/vdb) is the one the load runs on. Both are on the pool.
linux_vm_yaml() {
    local vm
    vm=$(vm_name "$1")
    data_pvc_yaml "$vm"
    cat <<YAML
---
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: $vm
  labels:
    $LABEL: "$ID"
spec:
  runStrategy: Always
  instancetype:
    kind: VirtualMachineClusterInstancetype
    name: $INSTANCETYPE
  preference:
    kind: VirtualMachineClusterPreference
    name: $CATALOG_PREFERENCE
  dataVolumeTemplates:
$(root_dv_yaml "$vm" "$CATALOG_IMAGE")
  template:
    metadata:
      labels:
        $LABEL: "$ID"
    spec:
      domain:
        devices:
          logSerialConsole: true
      volumes:
        - name: rootdisk
          dataVolume:
            name: $vm-root
        - name: loaddisk
          persistentVolumeClaim:
            claimName: $vm-data
        - name: cloudinit
          cloudInitNoCloud:
            secretRef:
              name: perf-userdata
YAML
}

# windows_vm_yaml N — a Windows VM: its root disk copied from the prepared image, a
# load disk the load script finds by its serial number, and the run's config disk as a CD.
windows_vm_yaml() {
    local vm
    vm=$(vm_name "$1")
    data_pvc_yaml "$vm"
    cat <<YAML
---
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: $vm
  labels:
    $LABEL: "$ID"
spec:
  runStrategy: Always
  preference:
    kind: VirtualMachineClusterPreference
    name: $WINDOWS_PREFERENCE
  dataVolumeTemplates:
$(root_dv_yaml "$vm" "$WINDOWS_IMAGE")
  template:
    metadata:
      labels:
        $LABEL: "$ID"
    spec:
      domain:
        cpu:
          sockets: 2
        memory:
          guest: 4Gi
        devices:
          logSerialConsole: true
          disks:
            - name: rootdisk
              bootOrder: 1
              disk:
                bus: virtio
            - name: loaddisk
              serial: perfload
              disk:
                bus: virtio
            - name: perfcfg
              cdrom:
                bus: sata
      volumes:
        - name: rootdisk
          dataVolume:
            name: $vm-root
        - name: loaddisk
          persistentVolumeClaim:
            claimName: $vm-data
        - name: perfcfg
          configMap:
            name: perf-windows
            volumeLabel: PERFCFG
YAML
}

if [ -n "$PRINT_VM" ]; then
    vm_yaml "$PRINT_VM"
    exit 0
fi
if $PRINT_USER_DATA; then
    user_data
    exit 0
fi
if $PRINT_WINDOWS_CONFIG; then
    windows_settings
    perf_windows_fill_args "$AREA"
    perf_windows_fio_args steady "$AVG" $((PERIOD - BURST_SECS)) "$AREA"
    perf_windows_fio_args burst "$BURST" "$BURST_SECS" "$AREA"
    exit 0
fi
if $PRINT_WINDOWS_START; then
    windows_start_script
    exit 0
fi

# shellcheck source=../e2e/lib/zone.sh
source "$HERE/../e2e/lib/zone.sh"
# shellcheck source=guest.sh
source "$HERE/guest.sh"

create_pool() {
    oc apply -f - >/dev/null <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $POOL
  labels:
    $LABEL: "$ID"
spec:
  zone: $(detect_zone)
  profile: dp2
  shareSizeGB: $SHARE_GB
  iops: $IOPS
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
YAML
}

wait_until() { # wait_until SECONDS DESCRIPTION COMMAND...
    local limit="$1" what="$2" end
    shift 2
    end=$((SECONDS + limit))
    until "$@"; do
        [ "$SECONDS" -lt "$end" ] || { log "FAIL: timed out after ${limit}s waiting for $what"; return 1; }
        sleep 10
    done
}

pool_ready() { [ "$(oc get filesharepool "$POOL" -o jsonpath='{.status.phase}' 2>/dev/null)" = Ready ]; }

# vm_console VM — the serial console log of one VM, from its launcher pod.
vm_console() {
    local pod
    pod=$(oc get pods -n "$NS" -l "vm.kubevirt.io/name=$1" -o name 2>/dev/null | head -1)
    [ -n "$pod" ] || return 1
    oc logs -n "$NS" "$pod" -c guest-console-log 2>/dev/null
}

# start_windows_loads N — starts the load of each of the first N VMs that is Windows, has
# not said PERF-READY yet, and has a guest agent. Starting twice does nothing.
start_windows_loads() {
    local i vm
    for ((i = 1; i <= $1; i++)); do
        is_windows "$i" || continue
        vm=$(vm_name "$i")
        vm_console "$vm" | perf_said_ready && continue
        guest_agent_connected "$NS" "$vm" || continue
        guest_ps "$NS" "$vm" "$(windows_start_script)" >/dev/null 2>&1 || true
    done
}

# ready N — starts the Windows loads, then checks every one of the first N VMs is ready.
ready() { start_windows_loads "$1"; all_ready "$1"; }

# copies_done — no DataVolume of the run is still copying.
copies_done() {
    [ -z "$(oc get dv -n "$NS" -o jsonpath='{range .items[?(@.status.phase!="Succeeded")]}{.metadata.name}{"\n"}{end}' 2>/dev/null)" ]
}

# all_ready N — every one of the first N VMs has said PERF-READY.
all_ready() {
    local i
    for ((i = 1; i <= $1; i++)); do
        vm_console "$(vm_name "$i")" | perf_said_ready || return 1
    done
}

# scrape_shares — the NFS counters of our pool, from every node agent.
scrape_shares() {
    local token pod pf
    token=$(oc create token prometheus-k8s -n openshift-monitoring --duration=3h 2>/dev/null) || return 1
    for pod in $(oc get pods -n kube-system -l app.kubernetes.io/component=node -o name 2>/dev/null); do
        oc port-forward -n kube-system "$pod" 19810:9810 >/dev/null 2>&1 &
        pf=$!
        sleep 2
        curl -sk -m 30 -H "Authorization: Bearer $token" https://localhost:19810/metrics | grep "^vpc_file_pool_share_nfs.*pool=\"$POOL\""
        kill "$pf" 2>/dev/null
        wait "$pf" 2>/dev/null
    done
}

cleanup() {
    $KEEP && { log "keeping everything (--keep): namespace and pool $NS"; return; }
    log "cleaning up"
    oc delete vm -n "$NS" --all --wait=false >/dev/null 2>&1
    oc delete ns "$NS" --ignore-not-found --wait=true --timeout=20m >/dev/null 2>&1
    oc delete filesharepool "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1
    if wait_until 1800 "pool $POOL to be gone" pool_gone; then log "cleanup done: pool and namespace are gone"; else log "WARNING: pool $POOL is still there: delete it by hand"; fi
}
pool_gone() { ! oc get filesharepool "$POOL" >/dev/null 2>&1; }

mkdir -p "$OUT"
trap cleanup EXIT
log "run $ID: tier $IOPS IOPS, share $SHARE_GB GB, steps $STEPS, $STEP_MIN min each, $(windows_mix); results in $OUT"

create_pool || { log "FAIL: cannot make the pool"; exit 1; }
wait_until 1800 "pool $POOL to be Ready" pool_ready || exit 1
oc create ns "$NS" >/dev/null && oc label ns "$NS" "$LABEL=$ID" >/dev/null
ud=$(mktemp)
user_data >"$ud"
oc create secret generic perf-userdata -n "$NS" --from-file=userdata="$ud" >/dev/null
find "$ud" -type f -delete
max_vms=${STEPS##*,}
windows_vms=0
[ "$WINDOWS_EVERY" -gt 0 ] && windows_vms=$((max_vms / WINDOWS_EVERY))
if [ "$windows_vms" -gt 0 ]; then
    mkdir -p "$OUT/windows-config"
    windows_config "$OUT/windows-config"
    oc create configmap perf-windows -n "$NS" --from-file="$OUT/windows-config" >/dev/null || { log "FAIL: cannot make the Windows config"; exit 1; }
fi

printf 'vms,slow10_pct,slow20_pct,slow30_pct,burst_slow_pct,stalls,boot_storm_secs,share_at_limit,short_pct\n' >"$OUT/steps.csv"
printf 'vms,share_nfs_iops,share_avg_latency_ms,guest_iops,ibm_ops_demand\n' >"$OUT/share.csv"

# What the guest-side readers need: the rate each VM aims at, and the length of its phases.
export PERF_TARGET_IOPS="$AVG"
export PERF_STEADY_SECS=$((PERIOD - BURST_SECS))
export PERF_BURST_SECS="$BURST_SECS"
ibm_factor=$(perf_ibm_ops_per_io)

prev=0
IFS=, read -ra STEP_LIST <<<"$STEPS"
for n in "${STEP_LIST[@]}"; do
    log "step $n VMs: creating $((n - prev)) more"
    for ((i = prev + 1; i <= n; i++)); do
        vm_yaml "$i" | oc apply -n "$NS" -f - >/dev/null || log "WARNING: could not make VM $i"
    done
    wait_until 3600 "$n VMs to say PERF-READY" ready "$n" || exit 1
    if [ "$prev" -eq 0 ]; then
        # How the disks are opened (the cache mode) decides whether a host cache flatters the guests' latency.
        # The research for this test could not confirm it, so every run records it for the first VM.
        pod=$(oc get pods -n "$NS" -l "vm.kubevirt.io/name=$(vm_name 1)" -o name 2>/dev/null | head -1)
        [ -z "$pod" ] || oc exec -n "$NS" "$pod" -c compute -- sh -c 'virsh dumpxml "$(virsh list --name | head -1)"' 2>/dev/null |
            grep -E '<driver |<source (file|dev)|<target dev' >"$OUT/vm1-disks.txt" || true
    fi
    # Copying the new VMs' disks loads the share for minutes after the VMs are ready.
    wait_until 3600 "the disk copies of step $n to end" copies_done || exit 1
    log "step $n: all ready; settling for $SETTLE_MIN min"
    sleep $((SETTLE_MIN * 60))

    t0=$(date +%s)
    m0=$(scrape_shares | perf_share_totals)
    log "step $n: measuring for $STEP_MIN min"
    sleep $((STEP_MIN * 60))
    t1=$(date +%s)
    m1=$(scrape_shares | perf_share_totals)
    sleep 90 # the last lines of the window reach the console logs

    mkdir -p "$OUT/step-$n"
    for ((i = 1; i <= n; i++)); do
        vm_console "$(vm_name "$i")" >"$OUT/step-$n/$(vm_name "$i").log" || : >"$OUT/step-$n/$(vm_name "$i").log"
    done

    read -r share_iops share_lat <<<"$(perf_share_rate "$m0" "$m1" $((t1 - t0)))"
    guest_iops=$(perf_guest_iops "$OUT/step-$n" "$t0" "$t1")
    # IBM counts one operation for every 16 KiB of an I/O, so the load on the share is
    # the guests' IOPS times the average number of 16 KiB units in an I/O of the mix.
    ibm_demand=$(awk -v g="$guest_iops" -v f="$ibm_factor" 'BEGIN { printf "%.0f", g * f }')
    at_limit=0
    awk -v r="$share_iops" -v d="$ibm_demand" -v l="$IOPS" 'BEGIN { exit !(r >= 0.95 * l || d >= 0.95 * l) }' && at_limit=1
    perf_step_row "$n" "$OUT/step-$n" "$t0" "$t1" "" "$at_limit" >>"$OUT/steps.csv"
    echo "$n,$share_iops,$share_lat,$guest_iops,$ibm_demand" >>"$OUT/share.csv"
    log "step $n: $(tail -1 "$OUT/steps.csv") ; share $share_iops IOPS, $share_lat ms"
    prev=$n
done

{
    echo "# Performance test, tier $IOPS IOPS (run $ID)"
    echo
    echo "Share: $SHARE_GB GB at $IOPS IOPS. Load per VM: $AVG IOPS average, bursts of $BURST IOPS for $BURST_SECS s every $PERIOD s, 70/30 read/write, blocks of $PERF_BLOCK_MIX (size:percent), a test area of $AREA written once before measuring, a different random pattern in every fio process. VMs: $(windows_mix). Windows VMs are Windows Server 2022 (2 vCPU, 4 GiB) running the same fio jobs with fio for Windows; Linux VMs are CentOS Stream 9."
    echo
    echo "| Tier | Knee at 20 ms (VMs) | Recommended (80% of the knee) | Knee at 10 ms | Knee at 30 ms |"
    echo "|---|---|---|---|---|"
    perf_table_row "$IOPS IOPS" "$OUT/steps.csv"
    echo
    unreliable=$(perf_unreliable "$OUT/steps.csv")
    if [ -n "$unreliable" ]; then
        echo "**Unreliable steps (VMs, some VMs could not keep their rate, so their latency looks better than users would see): $unreliable**"
        echo
    fi
    echo "## Steps"
    echo
    echo '```'
    cat "$OUT/steps.csv"
    echo '```'
    echo
    echo "## Share (from the node agents) and the guests"
    echo
    echo "share_nfs_iops is what the node agents counted. guest_iops is what the VMs said they did, added up. ibm_ops_demand is guest_iops times $ibm_factor, the average number of 16 KiB units IBM counts in one I/O of this mix, and is what is compared with the share's IOPS limit. If guest_iops is far above share_nfs_iops, a cache is answering some reads."
    echo
    echo '```'
    cat "$OUT/share.csv"
    echo '```'
} >"$OUT/report.md"
log "report: $OUT/report.md"
