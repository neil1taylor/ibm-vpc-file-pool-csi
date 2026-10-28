#!/usr/bin/env bash
# prepare-windows-image.sh — Makes the Windows image the performance test copies
# for its Windows VMs (docs/specs/2026-10-24-performance-test-30-vms.md), once per
# cluster. It starts a VM on an installed Windows Server 2022 disk and, through the
# QEMU guest agent:
#   1. installs fio for Windows (the load tool, the same as on the Linux VMs) from its
#      official release, checked against its checksum, and sets the clock to UTC;
#   2. activates the evaluation licence online, or, without internet, resets its
#      grace period (slmgr /rearm), and prints the licence state;
# then stops and removes the VM, keeps the disk, and points the DataSource the test
# uses at it. A second boot checks the licence. The VM needs internet for fio.
#
# Usage (KUBECONFIG points at the cluster, normally rovslab-dr-eude):
#   bash test/perf/prepare-windows-image.sh [--pvc win2k22-perf] [--datasource win2k22-perf]
#
# The disk (a PVC in openshift-virtualization-os-images) must hold an installed
# Windows Server 2022 with the QEMU guest agent, and must not be in use.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=guest.sh
source "$HERE/guest.sh"
NS=openshift-virtualization-os-images
PVC=win2k22-perf
DATASOURCE=win2k22-perf
PREFERENCE="${PERF_WINDOWS_PREFERENCE:-windows.2k22.virtio}"
FIO_MSI_URL=https://github.com/axboe/fio/releases/download/fio-3.43/fio-3.43-x64.msi
FIO_MSI_SHA256=8299E6E6FBC024D3B74AA1CE62FB94EF6960B45629FBFA678B720834B6EB34AA
VM=perf-win-prep

while [ $# -gt 0 ]; do
    case "$1" in
        --pvc) PVC="$2"; shift 2 ;;
        --datasource) DATASOURCE="$2"; shift 2 ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done

log() { echo "[$(date -u +%H:%M:%S)] $*"; }

vm_yaml() {
    cat <<YAML
apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: $VM
spec:
  runStrategy: Always
  preference:
    kind: VirtualMachineClusterPreference
    name: $PREFERENCE
  template:
    spec:
      domain:
        cpu:
          sockets: 2
        memory:
          guest: 4Gi
        devices:
          disks:
            - name: rootdisk
              bootOrder: 1
              disk:
                bus: virtio
      volumes:
        - name: rootdisk
          persistentVolumeClaim:
            claimName: $PVC
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


start_vm() {
    vm_yaml | oc apply -n "$NS" -f - >/dev/null || return 1
    wait_until 1200 "the guest agent of $VM" guest_agent_connected "$NS" "$VM" || return 1
    wait_until 600 "the guest agent of $VM to run commands" guest_answers
}
# guest_answers — the agent runs a command and returns its output. Early in a boot
# it says it is connected but can still restart and lose a command.
guest_answers() { [ "$(guest_ps "$NS" "$VM" 'Write-Output ok' 2>/dev/null)" = ok ]; }

stop_vm() {
    oc delete vm "$VM" -n "$NS" --wait=true --timeout=10m >/dev/null
    wait_until 600 "$VM to stop" vmi_gone
}
vmi_gone() { ! oc get vmi "$VM" -n "$NS" >/dev/null 2>&1; }

oc get pvc "$PVC" -n "$NS" >/dev/null || { log "FAIL: no PVC $PVC in $NS"; exit 1; }

log "starting $VM on $PVC"
start_vm || exit 1

log "installing fio and setting the clock to UTC"
guest_ps "$NS" "$VM" "
\$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force C:\\perf | Out-Null
# An earlier version of the test started the load with a start-up task. A load must never start twice.
Unregister-ScheduledTask -TaskName perf-load -Confirm:\$false -ErrorAction SilentlyContinue
Remove-Item C:\\perf\\* -Force -ErrorAction SilentlyContinue
# Windows reads the VM's clock as local time, but KubeVirt keeps it in UTC.
tzutil /s UTC
if (-not (Test-Path 'C:\\Program Files\\fio\\fio.exe')) {
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    Invoke-WebRequest -UseBasicParsing -Uri '$FIO_MSI_URL' -OutFile C:\\perf\\fio.msi
    if ((Get-FileHash C:\\perf\\fio.msi -Algorithm SHA256).Hash -ne '$FIO_MSI_SHA256') { throw 'the fio installer does not match its checksum' }
    \$p = Start-Process msiexec.exe -ArgumentList '/i C:\\perf\\fio.msi /qn /norestart' -Wait -PassThru
    if (\$p.ExitCode -ne 0) { throw \"msiexec failed with exit code \$(\$p.ExitCode)\" }
    Remove-Item C:\\perf\\fio.msi
}
& 'C:\\Program Files\\fio\\fio.exe' --version
" || { log "FAIL: cannot install fio"; exit 1; }

log "licence: online activation, else a reset of the grace period"
guest_ps "$NS" "$VM" "
\$out = cscript //nologo C:\\Windows\\System32\\slmgr.vbs /ato | Out-String
\$out
# slmgr exits 0 even when activation fails, so its text is checked.
if (\$out -notmatch 'successfully') { 'online activation failed; resetting the grace period'; cscript //nologo C:\\Windows\\System32\\slmgr.vbs /rearm }
" || log "WARNING: the licence step reported an error (see above)"

log "restarting to check the licence"
stop_vm || exit 1
start_vm || exit 1
guest_ps "$NS" "$VM" "cscript //nologo C:\\Windows\\System32\\slmgr.vbs /dli; tzutil /g; & 'C:\\Program Files\\fio\\fio.exe' --version"
stop_vm || exit 1

oc apply -n "$NS" -f - >/dev/null <<YAML || { log "FAIL: cannot make the DataSource"; exit 1; }
apiVersion: cdi.kubevirt.io/v1beta1
kind: DataSource
metadata:
  name: $DATASOURCE
spec:
  source:
    pvc:
      name: $PVC
      namespace: $NS
YAML
log "done: DataSource $DATASOURCE in $NS points at $PVC"
