#!/usr/bin/env bash
# guest.sh — Runs PowerShell in a Windows VM through the QEMU guest agent, from the
# VM's launcher pod. Sourced by run-tier.sh and prepare-windows-image.sh.

# guest_agent_connected NS VM — the guest agent of the VM is connected.
guest_agent_connected() {
    [ "$(oc get vmi "$2" -n "$1" -o jsonpath='{.status.conditions[?(@.type=="AgentConnected")].status}' 2>/dev/null)" = True ]
}

# guest_ps NS VM SCRIPT — runs a PowerShell script in the VM, prints what it printed,
# and returns its exit code. Gives up after 10 minutes.
guest_ps() {
    local ns="$1" vm="$2" pod dom enc req pid status lost=0
    pod=$(oc get pods -n "$ns" -l "vm.kubevirt.io/name=$vm" -o name 2>/dev/null | head -1)
    [ -n "$pod" ] || { echo "guest_ps: no launcher pod for $vm" >&2; return 1; }
    dom=$(oc exec -n "$ns" "$pod" -c compute -- virsh list --name | head -1)
    # shellcheck disable=SC2016 # a PowerShell variable, not a shell one
    enc=$(printf '$ProgressPreference = "SilentlyContinue"\n%s' "$3" | iconv -t UTF-16LE | base64 | tr -d '\n')
    req=$(jq -cn --arg e "$enc" '{execute: "guest-exec", arguments: {path: "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe", arg: ["-NoProfile", "-ExecutionPolicy", "Bypass", "-EncodedCommand", $e], "capture-output": true}}')
    pid=$(oc exec -n "$ns" "$pod" -c compute -- virsh qemu-agent-command "$dom" "$req" 2>/dev/null | jq -r .return.pid)
    if [ -z "$pid" ] || [ "$pid" = null ]; then
        echo "guest_ps: the guest agent of $vm did not start the command" >&2
        return 1
    fi
    for _ in $(seq 120); do
        # The agent forgets a command when it restarts, which it does early in a boot.
        if ! status=$(oc exec -n "$ns" "$pod" -c compute -- virsh qemu-agent-command "$dom" "{\"execute\":\"guest-exec-status\",\"arguments\":{\"pid\":$pid}}" 2>/dev/null); then
            lost=$((lost + 1))
            [ "$lost" -lt 3 ] || { echo "guest_ps: the guest agent of $vm lost the command" >&2; return 1; }
            sleep 5
            continue
        fi
        if [ "$(jq -r .return.exited <<<"$status")" = true ]; then
            jq -r '.return["out-data"] // empty' <<<"$status" | base64 -d | tr -d '\r'
            jq -r '.return["err-data"] // empty' <<<"$status" | base64 -d | tr -d '\r' >&2
            return "$(jq -r .return.exitcode <<<"$status")"
        fi
        sleep 5
    done
    echo "guest_ps: the command in $vm did not end in 10 minutes" >&2
    return 1
}
