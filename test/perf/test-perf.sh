#!/usr/bin/env bash
# test-perf.sh — Tests for the pure parts of the performance test tooling: the
# load profile a VM runs, the reading of its console log, and the knee rule of
# the report. Needs no cluster.
#
# Usage: bash test/perf/test-perf.sh

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=profile.sh
source "$HERE/profile.sh"
# shellcheck source=report.sh
source "$HERE/report.sh"
# shellcheck source=collect.sh
source "$HERE/collect.sh"
FAILED=0
WORK="$(mktemp -d)"
trap 'find "$WORK" -type f -delete; find "$WORK" -depth -type d -empty -delete' EXIT

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
eq() { if [ "$2" = "$3" ]; then pass "$1"; else fail "$1: got '$2', want '$3'"; fi; }
has() { if grep -qF -- "$3" <<<"$2"; then pass "$1"; else fail "$1 (missing: $3)"; fi; }
lacks() { if grep -qF -- "$3" <<<"$2"; then fail "$1 (found: $3)"; else pass "$1"; fi; }

# --- the IOPS split: fio takes one rate for reads and one for writes ---
eq "30 IOPS at 70/30 is 21 reads and 9 writes" "$(perf_split_iops 30)" "21,9"
eq "300 IOPS at 70/30 is 210 reads and 90 writes" "$(perf_split_iops 300)" "210,90"
eq "a rate that does not divide rounds the reads down and gives the rest to writes" "$(perf_split_iops 25)" "17,8"

# --- the script a Linux VM runs ---
# fio treats rate_iops as a bandwidth cap when block sizes vary: it counts the
# smallest block size for every I/O. With 4k-64k blocks the real rate came out
# about 8 times too low (35 IOPS for a target of 300 in the first smoke run). So
# the load is three jobs, each with one fixed block size and its own rate.
s=$(perf_linux_load_script 30 300 45 600 /dev/vdb)
has "the steady phase has a 4k job" "$s" "--name=steady-4k"
has "the steady phase has a 16k job" "$s" "--name=steady-16k"
has "the steady phase has a 64k job" "$s" "--name=steady-64k"
has "the 4k job takes half the steady IOPS (15 total: 10 reads, 5 writes)" "$s" "--bs=4k --rate_iops=10,5"
has "the 16k job takes 30 percent (9 total: 6 reads, 3 writes)" "$s" "--bs=16k --rate_iops=6,3"
has "the 64k job takes 20 percent (6 total: 4 reads, 2 writes)" "$s" "--bs=64k --rate_iops=4,2"
has "the burst 4k job (150 total: 105 reads, 45 writes)" "$s" "--bs=4k --rate_iops=105,45"
has "the burst 64k job (60 total: 42 reads, 18 writes)" "$s" "--bs=64k --rate_iops=42,18"
lacks "no job uses a block size range, which would make the rate wrong" "$s" "--bsrange"
has "the jobs are reported as one result" "$s" "--group_reporting"
has "the steady phase lasts the period minus the burst" "$s" "--runtime=555"
has "the burst lasts the burst time" "$s" "--runtime=45"
has "reads are 70 percent" "$s" "--rwmixread=70"
has "each fio process uses a different random pattern" "$s" "--randrepeat=0"
has "total latency is recorded, not only completion latency" "$s" "--lat_percentiles=1"
has "it works on the second disk" "$s" "--filename=/dev/vdb"
has "it works on an area of 3G by default" "$s" "--size=3G"
has "results go to the serial console" "$s" "/dev/ttyS0"
has "each result line says its phase" "$s" "PERF "
has "each VM starts at its own offset" "$s" "OFFSET"
lacks "it never asks for a rate above the burst rate" "$s" "--rate_iops=300"
s2=$(perf_linux_load_script 30 300 45 600 /dev/vdb 1G)
has "the area size can be set" "$s2" "--size=1G"

# IBM counts one share operation for every 16 KiB of an I/O, so a 64k I/O counts as four.
eq "the block mix averages 1.6 IBM operations for each I/O" "$(perf_ibm_ops_per_io)" "1.6"
eq "a mix of only 4k blocks is one operation each" "$(PERF_BLOCK_MIX='4k:100' perf_ibm_ops_per_io)" "1.0"
eq "a 1M block is 64 operations" "$(PERF_BLOCK_MIX='1M:100' perf_ibm_ops_per_io)" "64.0"

# the disk is written once before any measuring, so reads are not answered from holes
fill=$(perf_prefill_command /dev/vdb 3G)
has "the fill writes the same area" "$fill" "--size=3G"
has "the fill is a sequential write" "$fill" "--rw=write"
has "the fill uses large blocks" "$fill" "--bs=1M"
has "the fill bypasses the guest cache" "$fill" "--direct=1"

# --- reading a VM's serial console log ---
# fio result as the VM prints it: one line of JSON. Times are in nanoseconds.
fio_json() { # read_iops write_iops read_p95_ns read_p99_ns read_max_ns write_p95_ns write_max_ns [key]
    local key="${8:-lat_ns}"
    printf '{"jobs":[{"read":{"iops":%s,"%s":{"max":%s,"percentile":{"95.000000":%s,"99.000000":%s}}},"write":{"iops":%s,"%s":{"max":%s,"percentile":{"95.000000":%s,"99.000000":%s}}}}]}' \
        "$1" "$key" "$5" "$3" "$4" "$2" "$key" "$7" "$6" "$7"
}

line=$(printf 'PERF 1700000100 steady %s' "$(fio_json 20 9 4000000 9000000 12000000 2000000 3000000)")
eq "a PERF line becomes one CSV row" "$(perf_parse_line "$line")" "1700000100,steady,20,9,4,2,9,12"
line=$(printf 'PERF 1700000100 steady %s' "$(fio_json 20 9 4000000 9000000 12000000 2000000 3000000 clat_ns)")
eq "a line with only completion latency still reads" "$(perf_parse_line "$line")" "1700000100,steady,20,9,4,2,9,12"
both='{"jobs":[{"read":{"iops":20,"lat_ns":{"max":9000000,"percentile":{"95.000000":7000000,"99.000000":8000000}},"clat_ns":{"max":1000000,"percentile":{"95.000000":1000000,"99.000000":1000000}}},"write":{"iops":9,"lat_ns":{"max":3000000,"percentile":{"95.000000":2000000,"99.000000":3000000}},"clat_ns":{"max":1000000,"percentile":{"95.000000":1000000,"99.000000":1000000}}}}]}'
eq "total latency wins over completion latency" "$(perf_parse_line "PERF 1700000100 steady $both")" "1700000100,steady,20,9,7,2,8,9"
eq "a line that is not a PERF line gives nothing" "$(perf_parse_line "[  OK  ] Started cloud-init")" ""
eq "a PERF line with broken JSON gives nothing" "$(perf_parse_line "PERF 1700000100 steady {oops")" ""

# perf_vm_stats prints: steady worst p95, burst worst p95, stalls, short phases
# (worst p95 is the larger of the read and write p95 of any one phase, in ms)
log="$WORK/vm-a.log"
{
    echo "[    5.1] boot noise"
    printf 'PERF 1700000100 steady %s\n' "$(fio_json 20 9 4000000 9000000 12000000 2000000 3000000)"
    printf 'PERF 1700000700 burst %s\n' "$(fio_json 210 90 50000000 90000000 120000000 40000000 60000000)"
    printf 'PERF 1700001300 steady %s\n' "$(fio_json 21 9 6000000 9000000 15000000 8000000 3000000)"
} >"$log"
eq "a VM's steady value is its highest p95, read or write, in the window" "$(PERF_TARGET_IOPS=30 perf_vm_stats "$log" 1700000000 1700002000)" "8 50 0 0"
eq "the burst phase has its own value and does not set the steady one" "$(PERF_TARGET_IOPS=30 perf_vm_stats "$log" 1700000600 1700000800)" "-1 50 0 0"
eq "a window with no line is no data" "$(PERF_TARGET_IOPS=30 perf_vm_stats "$log" 1700005000 1700006000)" "-1 -1 0 0"
eq "a steady phase well under the target rate is short" "$(PERF_TARGET_IOPS=100 perf_vm_stats "$log" 1700000000 1700002000)" "8 50 0 2"
eq "with no target set nothing is short" "$(PERF_TARGET_IOPS='' perf_vm_stats "$log" 1700000000 1700002000)" "8 50 0 0"
eq "a steady phase at 90 percent of the target is not short" "$(PERF_TARGET_IOPS=32 perf_vm_stats "$log" 1700000000 1700002000)" "8 50 0 0"

log2="$WORK/vm-b.log"
printf 'PERF 1700000100 steady %s\n' "$(fio_json 20 9 4000000 9000000 5001000000 2000000 3000000)" >"$log2"
eq "an I/O over 5 seconds counts as a stall" "$(PERF_TARGET_IOPS=30 perf_vm_stats "$log2" 1700000000 1700002000)" "4 -1 1 0"

# --- one step row from many logs ---
# columns: vms, slow at 10 ms, 20 ms, 30 ms (percent of VMs), burst slow (over 50 ms), stalls, boot storm secs, share at limit, short percent
dir="$WORK/logs"; mkdir -p "$dir"
for n in 1 2 3 4; do printf 'PERF 1700000100 steady %s\n' "$(fio_json 20 9 4000000 9000000 12000000 2000000 3000000)" >"$dir/vm-$n.log"; done
export PERF_TARGET_IOPS=30
eq "a step with no slow VM" "$(perf_step_row 4 "$dir" 1700000000 1700002000 120 0)" "4,0,0,0,0,0,120,0,0"
printf 'PERF 1700000100 steady %s\n' "$(fio_json 20 9 15000000 40000000 12000000 2000000 3000000)" >"$dir/vm-4.log"
eq "one VM of four over 10 ms is 25 percent at 10 ms only" "$(perf_step_row 4 "$dir" 1700000000 1700002000 120 0)" "4,25,0,0,0,0,120,0,0"
printf 'PERF 1700000100 steady %s\n' "$(fio_json 20 9 35000000 40000000 12000000 2000000 3000000)" >"$dir/vm-4.log"
eq "one VM of four over 30 ms is slow at every limit" "$(perf_step_row 4 "$dir" 1700000000 1700002000 120 0)" "4,25,25,25,0,0,120,0,0"
printf 'PERF 1700000100 steady %s\nPERF 1700000300 burst %s\n' "$(fio_json 20 9 4000000 9000000 12000000 2000000 3000000)" "$(fio_json 210 90 60000000 90000000 120000000 40000000 60000000)" >"$dir/vm-4.log"
eq "one VM with a burst over 50 ms is counted in the burst column only" "$(perf_step_row 4 "$dir" 1700000000 1700002000 120 0)" "4,0,0,0,25,0,120,0,0"
: >"$dir/vm-3.log"
eq "a VM with no data counts as slow at every limit" "$(perf_step_row 4 "$dir" 1700000000 1700002000 "" 1)" "4,25,25,25,25,0,,1,0"
printf 'PERF 1700000100 steady %s\n' "$(fio_json 3 1 4000000 9000000 12000000 2000000 3000000)" >"$dir/vm-2.log"
eq "a VM that ran far under its target is counted as short" "$(perf_step_row 4 "$dir" 1700000000 1700002000 120 0)" "4,25,25,25,25,0,120,0,25"
unset PERF_TARGET_IOPS

# --- the knee rule ---
# columns: vms, slow10, slow20, slow30 (percent), burst slow, stalls, boot storm seconds, share at limit, short percent
steps() { printf 'vms,slow10_pct,slow20_pct,slow30_pct,burst_slow_pct,stalls,boot_storm_secs,share_at_limit,short_pct\n%s\n' "$1" >"$WORK/steps.csv"; echo "$WORK/steps.csv"; }

f=$(steps $'15,0,0,0,0,0,,0,0\n30,10,10,5,0,0,,0,0\n45,40,30,10,0,0,,0,0\n60,70,60,30,0,0,,0,0')
eq "the knee at 20 ms is the first step where more than a quarter of VMs are slow" "$(perf_knee "$f")" "45"
eq "the knee at 10 ms comes earlier" "$(perf_knee "$f" 10)" "45"
eq "the knee at 30 ms comes later" "$(perf_knee "$f" 30)" "60"
eq "the recommended count is the largest step at or below 80 percent of the knee" "$(perf_recommended "$f")" "30"

f=$(steps $'15,0,0,0,0,0,,0,0\n30,0,0,0,0,1,,0,0\n45,0,0,0,0,0,,0,0')
eq "any stall is a knee" "$(perf_knee "$f")" "30"

f=$(steps $'15,0,0,0,30,0,,0,0\n30,0,0,0,0,0,,0,0')
eq "more than a quarter of VMs with a slow burst is a knee" "$(perf_knee "$f")" "15"

f=$(steps $'15,0,0,0,0,0,,0,0\n30,0,0,0,0,0,200,0,0\n45,0,0,0,0,0,300,0,0\n60,0,0,0,0,0,900,0,0')
eq "boot storms are compared per VM, from the first step that has one" "$(perf_knee "$f")" "60"
f=$(steps $'30,0,0,0,0,0,150,0,0\n45,0,0,0,0,0,225,0,0\n60,0,0,0,0,0,300,0,0')
eq "a longer storm for more VMs is not a knee while the time per VM holds" "$(perf_knee "$f")" "none"
f=$(steps $'30,0,0,0,0,0,150,0,0\n45,0,0,0,0,0,680,0,0')
eq "a storm that takes more than twice as long per VM is a knee" "$(perf_knee "$f")" "45"
f=$(steps $'30,0,0,0,0,0,150,0,0\n45,0,0,0,0,0,450,0,0')
eq "exactly twice the time per VM is not a knee" "$(perf_knee "$f")" "none"
f=$(steps $'15,0,0,0,0,0,,0,0\n30,0,0,0,0,0,,0,0')
eq "no boot storm anywhere gives no boot storm knee" "$(perf_knee "$f")" "none"

f=$(steps $'15,0,0,0,0,0,,0,0\n30,0,0,0,0,0,,0,0\n45,0,0,0,0,0,,1,0')
eq "a share at its IOPS limit is a knee" "$(perf_knee "$f")" "45"

f=$(steps $'15,25,25,25,0,0,,0,0\n30,26,26,26,0,0,,0,0')
eq "exactly a quarter of VMs slow is not a knee, a little more is" "$(perf_knee "$f")" "30"

f=$(steps $'15,0,0,0,0,0,,0,0\n30,10,10,0,0,0,,0,0\n45,20,20,10,0,0,,0,0')
eq "no knee is reported as none" "$(perf_knee "$f")" "none"
eq "with no knee the recommendation says more than the last step" "$(perf_recommended "$f")" "more than 45"

f=$(steps $'15,40,40,40,0,0,,0,0\n30,60,60,60,0,0,,0,0')
eq "a knee at the first step gives no recommendation" "$(perf_recommended "$f")" "below 15"

f=$(steps $'15,0,0,0,0,0,,0,0\n30,0,0,0,0,0,,0,40\n45,0,0,0,0,0,,0,0')
eq "a step where the VMs could not keep their rate is listed as unreliable" "$(perf_unreliable "$f")" "30"
f=$(steps $'15,0,0,0,0,0,,0,0\n30,0,0,0,0,0,,0,0')
eq "no unreliable step gives nothing" "$(perf_unreliable "$f")" ""

# --- the table row ---
f=$(steps $'15,0,0,0,0,0,,0,0\n30,10,10,5,0,0,,0,0\n45,40,30,10,0,0,,0,0\n60,70,60,30,0,0,,0,0')
row=$(perf_table_row "10000 IOPS" "$f")
eq "the table row gives the knee at 20 ms, the recommendation, then the knees at 10 and 30 ms" "$row" "| 10000 IOPS | 45 | 30 | 45 | 60 |"

# --- the share counters from the node agents ---
before='# HELP vpc_file_pool_share_nfs_operations_total NFS operations
vpc_file_pool_share_nfs_operations_total{pool="p",share="s1",op="read"} 600
vpc_file_pool_share_nfs_operations_total{pool="p",share="s1",op="write"} 400
vpc_file_pool_share_nfs_operations_total{pool="p",share="s1",op="other"} 9000
vpc_file_pool_share_nfs_execute_seconds_total{pool="p",share="s1",op="read"} 6
vpc_file_pool_share_nfs_execute_seconds_total{pool="p",share="s1",op="write"} 4
vpc_file_pool_share_nfs_execute_seconds_total{pool="p",share="s1",op="other"} 500'
after='vpc_file_pool_share_nfs_operations_total{pool="p",share="s1",op="read"} 2100
vpc_file_pool_share_nfs_operations_total{pool="p",share="s1",op="write"} 1900
vpc_file_pool_share_nfs_operations_total{pool="p",share="s1",op="other"} 19000
vpc_file_pool_share_nfs_execute_seconds_total{pool="p",share="s1",op="read"} 21
vpc_file_pool_share_nfs_execute_seconds_total{pool="p",share="s1",op="write"} 19
vpc_file_pool_share_nfs_execute_seconds_total{pool="p",share="s1",op="other"} 900'
eq "the totals count reads and writes, not other calls" "$(perf_share_totals <<<"$before")" "1000 10"
eq "IOPS and average latency over a window" "$(perf_share_rate "$(perf_share_totals <<<"$before")" "$(perf_share_totals <<<"$after")" 300)" "10 10.0"
eq "a window with no operations gives zero, not a division error" "$(perf_share_rate "5 1" "5 1" 300)" "0 0.0"

# --- the guest side of the reconciliation: what the VMs say they did, added up ---
# Each line is an average over its phase; a steady phase is longer than a burst, so lines are weighted by phase length.
dir2="$WORK/logs2"; mkdir -p "$dir2"
for n in 1 2; do
    {
        printf 'PERF 1700000100 steady %s\n' "$(fio_json 20 10 4000000 9000000 12000000 2000000 3000000)"
        printf 'PERF 1700000200 burst %s\n' "$(fio_json 200 100 4000000 9000000 12000000 2000000 3000000)"
    } >"$dir2/vm-$n.log"
done
# steady 30 IOPS for 100 s and burst 300 IOPS for 20 s: (30*100 + 300*20) / 120 = 75 for each VM, 150 for two
eq "guest IOPS are summed over the VMs, weighted by phase length" "$(PERF_STEADY_SECS=100 PERF_BURST_SECS=20 perf_guest_iops "$dir2" 1700000000 1700001000)" "150"

# --- the VM definition from run-tier.sh ---
vm=$(PERF_ID=T1 bash "$HERE/run-tier.sh" --iops 3000 --print-vm 7 2>&1)
has "the VM has a numbered name" "$vm" "name: perf-007"
has "the VM definition is a VirtualMachine" "$vm" "kind: VirtualMachine"
has "its root disk is on the pool under test" "$vm" "storageClassName: perf-T1"
has "its root disk is as large as the catalog image it is copied from (30 GiB)" "$vm" "storage: 30Gi"
has "its load disk is large enough for the 3G test area" "$vm" "storage: 4Gi"
has "its load disk is its own claim" "$vm" "claimName: perf-007-data"
has "its cloud-init comes from a Secret, not inline" "$vm" "secretRef:"
lacks "it does not use a field name KubeVirt does not know" "$vm" "userDataSecretRef"
has "everything carries the run label" "$vm" 'perf-test-run: "T1"'
has "the serial console is logged" "$vm" "logSerialConsole: true"
has "the load disk claim is in the pool too" "$vm" "kind: PersistentVolumeClaim"
if bash "$HERE/run-tier.sh" --print-vm 1 >/dev/null 2>&1; then fail "run-tier.sh needs --iops"; else pass "run-tier.sh refuses to run without --iops"; fi

ud=$(PERF_ID=T1 bash "$HERE/run-tier.sh" --iops 3000 --print-user-data 2>&1)
has "the cloud-init installs fio" "$ud" "dnf install -y -q fio"
has "the cloud-init fills the disk before it says ready" "$ud" "--name=fill"
has "the VM says PERF-READY after the fill" "$ud" "PERF-READY"
fill_at=$(grep -n -- "--name=fill" <<<"$ud" | head -1 | cut -d: -f1)
ready_at=$(grep -n "echo PERF-READY" <<<"$ud" | head -1 | cut -d: -f1)
if [ -n "$fill_at" ] && [ -n "$ready_at" ] && [ "$fill_at" -lt "$ready_at" ]; then pass "the fill comes before PERF-READY"; else fail "the fill comes before PERF-READY (fill line '$fill_at', ready line '$ready_at')"; fi

exit "$FAILED"
