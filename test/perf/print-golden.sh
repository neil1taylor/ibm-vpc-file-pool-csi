#!/usr/bin/env bash
# print-golden.sh — Writes what the Bash tooling makes for the default load and for a
# fixed set of VM results, so the Go tests of the PoolBench service can check that the
# service makes the same load and reads results the same way (pkg/poolbench/testdata).
# test-perf.sh fails when the files there are not what this script prints.
#
# Usage: bash test/perf/print-golden.sh profile|step

set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=profile.sh
source "$HERE/profile.sh"
# shellcheck source=collect.sh
source "$HERE/collect.sh"
TESTDATA="$HERE/../../pkg/poolbench/testdata"

# The default load of run-tier.sh: 30 IOPS steady, 300 IOPS bursts of 45 s every 600 s, a 3G area.
profile() {
    echo "== steady jobs"; perf_fio_job_list steady 30
    echo "== burst jobs"; perf_fio_job_list burst 300
    echo "== linux common"; perf_fio_common /dev/vdb 3G libaio
    echo "== linux fill"; perf_prefill_command /dev/vdb 3G
    echo "== windows fill"; perf_windows_fill_args 3G
    echo "== windows steady"; perf_windows_fio_args steady 30 555 3G
    echo "== windows burst"; perf_windows_fio_args burst 300 45 3G
    echo "== ibm ops per io"; perf_ibm_ops_per_io
}

# One step over the VM logs in testdata/step, measured from 1700000000 to 1700002000:
# the step row of collect.sh (slow at 10, 20 and 30 ms, burst slow, stalls, short, all
# in percent of VMs but stalls) and the guest IOPS.
step() {
    local dir="$TESTDATA/step" row
    export PERF_TARGET_IOPS=30 PERF_STEADY_SECS=555 PERF_BURST_SECS=45
    row=$(perf_step_row 4 "$dir" 1700000000 1700002000 "" 0)
    IFS=, read -r _ s10 s20 s30 sb stalls _ _ short <<<"$row"
    echo "slow10=$s10 slow20=$s20 slow30=$s30 burstslow=$sb stalls=$stalls short=$short guestiops=$(perf_guest_iops "$dir" 1700000000 1700002000)"
}

case "${1:-}" in
    profile) profile ;;
    step) step ;;
    *) echo "usage: $0 profile|step" >&2; exit 2 ;;
esac
