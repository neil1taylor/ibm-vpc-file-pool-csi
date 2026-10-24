#!/usr/bin/env bash
# collect.sh — Reads what the VMs of the performance test print on their serial
# console (docs/specs/2026-10-24-performance-test-30-vms.md) and turns it into
# the numbers of one step. The functions here need no cluster. Sourced by the
# other perf scripts and by test-perf.sh.
#
# A VM prints one line after each phase:
#   PERF <epoch seconds> <steady|burst> <fio result as one line of JSON>

# A single I/O that takes longer than this many milliseconds is a stall.
PERF_STALL_MS=5000
# A VM is slow in a step when its highest steady p95 latency (read or write) is over this many
# milliseconds. The report counts the VMs over 10, 20 and 30 ms, so the answer can be read at each.
PERF_SLOW_MS_LOW=10
PERF_SLOW_MS=20
PERF_SLOW_MS_HIGH=30
# A burst is slow when its p95 is over this. Bursts are short and expected to be slower, so the limit is looser.
PERF_BURST_SLOW_MS=50
# A steady phase is "short" when the VM did less than this share of its target IOPS (percent). A load
# generator that cannot keep its rate has been held back, which hides latency (coordinated omission).
PERF_SHORT_PCT=90

# perf_parse_line LINE — one CSV row for a PERF line, nothing for any other line
# or a line whose JSON is broken:
#   epoch,phase,read_iops,write_iops,read_p95_ms,write_p95_ms,read_p99_ms,max_ms
# Latency is the total latency (lat_ns, which includes waiting before submission) when
# fio reported it, and the completion latency (clat_ns) otherwise. max_ms is the longest
# single I/O, read or write. Times are rounded to 0.1 ms.
perf_parse_line() {
    local line="$1" epoch phase json
    case "$line" in PERF\ *) ;; *) return 0 ;; esac
    read -r _ epoch phase json <<<"$line"
    printf '%s' "$json" | jq -r --arg e "$epoch" --arg p "$phase" '
        def ms: ./1000000 | . * 10 | round / 10;
        def lat: .lat_ns // .clat_ns;
        .jobs[0] as $j
        | [$e, $p, ($j.read.iops | floor), ($j.write.iops | floor),
           ($j.read | lat | .percentile["95.000000"] | ms),
           ($j.write | lat | .percentile["95.000000"] | ms),
           ($j.read | lat | .percentile["99.000000"] | ms),
           ([($j.read | lat | .max), ($j.write | lat | .max)] | max | ms)]
        | @csv' 2>/dev/null | tr -d '"'
}

# perf_vm_stats LOG START END — "<steady worst p95> <burst worst p95> <stalls> <short>" for
# the PERF lines of one VM between two epoch times.
# - worst p95 is the larger of the read and write p95 of any one phase, in ms, over the
#   steady phases and over the burst phases separately; -1 if there is none.
# - stalls: lines, of either phase, whose longest I/O was over PERF_STALL_MS.
# - short: steady phases whose achieved IOPS (reads plus writes) were under PERF_SHORT_PCT
#   percent of PERF_TARGET_IOPS. Zero when PERF_TARGET_IOPS is empty.
perf_vm_stats() {
    local log="$1" start="$2" end="$3" line row epoch phase riops wiops rp95 wp95 mx worst
    local steady=-1 burst=-1 stalls=0 short=0
    while IFS= read -r line; do
        row=$(perf_parse_line "$line") || true
        [ -n "$row" ] || continue
        IFS=, read -r epoch phase riops wiops rp95 wp95 _ mx <<<"$row"
        { [ "$epoch" -ge "$start" ] && [ "$epoch" -le "$end" ]; } || continue
        if awk -v m="$mx" -v t="$PERF_STALL_MS" 'BEGIN { exit !(m > t) }'; then stalls=$((stalls + 1)); fi
        worst=$(awk -v a="$rp95" -v b="$wp95" 'BEGIN { print (a > b) ? a : b }')
        if [ "$phase" = steady ]; then
            if awk -v a="$worst" -v b="$steady" 'BEGIN { exit !(a > b) }'; then steady=$worst; fi
            if [ -n "${PERF_TARGET_IOPS:-}" ] && awk -v r="$riops" -v w="$wiops" -v t="$PERF_TARGET_IOPS" -v p="$PERF_SHORT_PCT" 'BEGIN { exit !(r + w < t * p / 100) }'; then
                short=$((short + 1))
            fi
        elif [ "$phase" = burst ]; then
            if awk -v a="$worst" -v b="$burst" 'BEGIN { exit !(a > b) }'; then burst=$worst; fi
        fi
    done <"$log"
    echo "$steady $burst $stalls $short"
}

# perf_step_row VMS LOGDIR START END BOOT_SECS SHARE_AT_LIMIT — one row of steps.csv
# (see report.sh) from the console logs of every VM in LOGDIR (one *.log file for each VM):
#   vms,slow10_pct,slow20_pct,slow30_pct,burst_slow_pct,stalls,boot_storm_secs,share_at_limit,short_pct
# slowN_pct is the percent of VMs whose highest steady p95 was over N ms. A VM with no steady
# line in the window counts as slow at every limit: it could not be shown to be fast. A VM
# with no burst line is not counted in burst_slow_pct. short_pct is the percent of VMs that
# had a steady phase far under their target rate.
perf_step_row() {
    local vms="$1" dir="$2" start="$3" end="$4" boot="$5" limit="$6"
    local f out steady burst st sh total=0 s10=0 s20=0 s30=0 sb=0 stalls=0 shorts=0
    for f in "$dir"/*.log; do
        [ -e "$f" ] || continue
        total=$((total + 1))
        out=$(perf_vm_stats "$f" "$start" "$end")
        read -r steady burst st sh <<<"$out"
        stalls=$((stalls + st))
        if awk -v a="$steady" -v t="$PERF_SLOW_MS_LOW" 'BEGIN { exit !(a < 0 || a > t) }'; then s10=$((s10 + 1)); fi
        if awk -v a="$steady" -v t="$PERF_SLOW_MS" 'BEGIN { exit !(a < 0 || a > t) }'; then s20=$((s20 + 1)); fi
        if awk -v a="$steady" -v t="$PERF_SLOW_MS_HIGH" 'BEGIN { exit !(a < 0 || a > t) }'; then s30=$((s30 + 1)); fi
        if awk -v a="$burst" -v t="$PERF_BURST_SLOW_MS" 'BEGIN { exit !(a > t) }'; then sb=$((sb + 1)); fi
        [ "$sh" -le 0 ] || shorts=$((shorts + 1))
    done
    pct() { awk -v s="$1" -v t="$total" 'BEGIN { if (t == 0) print 0; else printf "%d", (s * 100 + t / 2) / t }'; }
    echo "$vms,$(pct "$s10"),$(pct "$s20"),$(pct "$s30"),$(pct "$sb"),$stalls,$boot,$limit,$(pct "$shorts")"
}

# perf_guest_iops LOGDIR START END — the IOPS the VMs say they did, added up over
# all the VMs: for each VM the mean of its PERF lines in the window, weighted by the
# length of the phase (PERF_STEADY_SECS for a steady line, PERF_BURST_SECS for a burst line).
# Compare it with the share's own count to see whether a cache answered some reads.
perf_guest_iops() {
    local dir="$1" start="$2" end="$3" f line row epoch phase riops wiops
    for f in "$dir"/*.log; do
        [ -e "$f" ] || continue
        while IFS= read -r line; do
            row=$(perf_parse_line "$line") || true
            [ -n "$row" ] || continue
            IFS=, read -r epoch phase riops wiops _ <<<"$row"
            { [ "$epoch" -ge "$start" ] && [ "$epoch" -le "$end" ]; } || continue
            echo "$f $phase $((riops + wiops))"
        done <"$f"
    done | awk -v sw="${PERF_STEADY_SECS:-1}" -v bw="${PERF_BURST_SECS:-1}" '
        { w = ($2 == "steady") ? sw : bw; num[$1] += $3 * w; den[$1] += w }
        END { total = 0; for (f in num) total += num[f] / den[f]; printf "%.0f\n", total }'
}

# perf_share_totals — reads the metrics text of the node agents on stdin and
# prints "<operations> <seconds>": the reads and writes done, and the seconds
# they took, added up over every share and node. Metadata calls ("other") are
# left out, because they are not I/O to the disk.
perf_share_totals() {
    awk '
        /^vpc_file_pool_share_nfs_operations_total\{/ && /op="(read|write)"/ { ops += $NF }
        /^vpc_file_pool_share_nfs_execute_seconds_total\{/ && /op="(read|write)"/ { sec += $NF }
        END { printf "%s %s\n", ops + 0, sec + 0 }'
}

# perf_share_rate BEFORE AFTER SECONDS — "<IOPS> <average latency in ms>" over a
# window, from two "<operations> <seconds>" readings of perf_share_totals.
perf_share_rate() {
    local b_ops b_sec a_ops a_sec
    read -r b_ops b_sec <<<"$1"
    read -r a_ops a_sec <<<"$2"
    awk -v bo="$b_ops" -v bs="$b_sec" -v ao="$a_ops" -v as="$a_sec" -v secs="$3" 'BEGIN {
        ops = ao - bo
        if (ops <= 0 || secs <= 0) { printf "0 0.0\n"; exit }
        printf "%.0f %.1f\n", ops / secs, (as - bs) / ops * 1000
    }'
}
