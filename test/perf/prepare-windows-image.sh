#!/usr/bin/env bash
# prepare-windows-image.sh — Makes the Windows image the performance test copies
# for its Windows VMs (docs/specs/2026-10-24-performance-test-30-vms.md), once per
# cluster. It starts a VM on an installed Windows Server 2022 disk and, through the
# QEMU guest agent:
#   1. puts bootstrap.ps1 at C:\perf and registers it as a start-up task, which runs
#      the load script of a test VM's config disk (see windows/load.ps1);
#   2. activates the evaluation licence online, or, without internet, resets its
#      grace period (slmgr /rearm), and prints the licence state;
# then stops and removes the VM, keeps the disk, and points the DataSource the test
# uses at it. A second boot checks the licence after the reset.
#
# Usage (KUBECONFIG points at the cluster, normally rovslab-dr-eude):
#   bash test/perf/prepare-windows-image.sh [--pvc win2k22-perf] [--datasource win2k22-perf]
#
# The disk (a PVC in openshift-virtualization-os-images) must hold an installed
# Windows Server 2022 with the QEMU guest agent, and must not be in use.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
NS=openshift-virtualization-os-images
PVC=win2k22-perf
DATASOURCE=win2k22-perf
PREFERENCE="${PERF_WINDOWS_PREFERENCE:-windows.2k22.virtio}"
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

agent_connected() {
    [ "$(oc get vmi "$VM" -n "$NS" -o jsonpath='{.status.conditions[?(@.type=="AgentConnected")].status}' 2>/dev/null)" = True ]
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

# guest_ps SCRIPT — runs a PowerShell script in the VM through the guest agent and
# prints what it printed. Returns its exit code.
guest_ps() {
    local pod dom enc req pid status code
    pod=$(oc get pods -n "$NS" -l "vm.kubevirt.io/name=$VM" -o name | head -1)
    dom=$(oc exec -n "$NS" "$pod" -c compute -- virsh list --name | head -1)
    enc=$(printf '%s' "$1" | iconv -t UTF-16LE | base64 | tr -d '\n')
    req=$(jq -cn --arg e "$enc" '{execute: "guest-exec", arguments: {path: "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe", arg: ["-NoProfile", "-ExecutionPolicy", "Bypass", "-EncodedCommand", $e], "capture-output": true}}')
    pid=$(oc exec -n "$NS" "$pod" -c compute -- virsh qemu-agent-command "$dom" "$req" | jq -r .return.pid)
    [ -n "$pid" ] && [ "$pid" != null ] || { log "FAIL: the guest agent did not start the command"; return 1; }
    for _ in $(seq 120); do
        status=$(oc exec -n "$NS" "$pod" -c compute -- virsh qemu-agent-command "$dom" "{\"execute\":\"guest-exec-status\",\"arguments\":{\"pid\":$pid}}")
        if [ "$(jq -r .return.exited <<<"$status")" = true ]; then
            jq -r '.return["out-data"] // empty' <<<"$status" | base64 -d | tr -d '\r'
            jq -r '.return["err-data"] // empty' <<<"$status" | base64 -d | tr -d '\r' >&2
            code=$(jq -r .return.exitcode <<<"$status")
            return "$code"
        fi
        sleep 5
    done
    log "FAIL: the command in the guest did not end in 10 minutes"
    return 1
}

start_vm() {
    vm_yaml | oc apply -n "$NS" -f - >/dev/null || return 1
    wait_until 1200 "the guest agent of $VM" agent_connected
}

stop_vm() {
    oc delete vm "$VM" -n "$NS" --wait=true --timeout=10m >/dev/null
    wait_until 600 "$VM to stop" vmi_gone
}
vmi_gone() { ! oc get vmi "$VM" -n "$NS" >/dev/null 2>&1; }

oc get pvc "$PVC" -n "$NS" >/dev/null || { log "FAIL: no PVC $PVC in $NS"; exit 1; }

log "starting $VM on $PVC"
start_vm || exit 1

boot64=$(base64 <"$HERE/windows/bootstrap.ps1" | tr -d '\n')
log "installing the start-up task"
guest_ps "
\$ErrorActionPreference = 'Stop'
New-Item -ItemType Directory -Force C:\\perf | Out-Null
[IO.File]::WriteAllBytes('C:\\perf\\bootstrap.ps1', [Convert]::FromBase64String('$boot64'))
\$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument '-NoProfile -ExecutionPolicy Bypass -File C:\\perf\\bootstrap.ps1'
\$trigger = New-ScheduledTaskTrigger -AtStartup
\$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit 0 -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName perf-load -Action \$action -Trigger \$trigger -Settings \$settings -User SYSTEM -RunLevel Highest -Force | Out-Null
'installed'
" || { log "FAIL: cannot install the start-up task"; exit 1; }

log "licence: online activation, else a reset of the grace period"
guest_ps "
\$out = cscript //nologo C:\\Windows\\System32\\slmgr.vbs /ato | Out-String
\$out
# slmgr exits 0 even when activation fails, so its text is checked.
if (\$out -notmatch 'successfully') { 'online activation failed; resetting the grace period'; cscript //nologo C:\\Windows\\System32\\slmgr.vbs /rearm }
" || log "WARNING: the licence step reported an error (see above)"

log "restarting to check the licence"
stop_vm || exit 1
start_vm || exit 1
guest_ps "cscript //nologo C:\\Windows\\System32\\slmgr.vbs /dli; Get-ScheduledTask perf-load | Select-Object -ExpandProperty State"
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
