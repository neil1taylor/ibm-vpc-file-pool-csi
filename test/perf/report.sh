#!/usr/bin/env bash
# report.sh — The knee rule and the table row of the performance test report
# (docs/specs/2026-10-24-performance-test-30-vms.md). Pure functions over a CSV
# of steps; no cluster needed. Sourced by the other perf scripts and by test-perf.sh.
#
# steps.csv has one row for each step of a run, in order, with the header:
#   vms,slow10_pct,slow20_pct,slow30_pct,burst_slow_pct,stalls,boot_storm_secs,share_at_limit,short_pct
# vms              VMs running at this step
# slow10_pct       percent of the VMs whose highest steady p95 latency (read or write) was over 10 ms
# slow20_pct       the same over 20 ms
# slow30_pct       the same over 30 ms
# burst_slow_pct   percent of the VMs whose highest burst p95 was over 50 ms
# stalls           I/Os that took over 5 s
# boot_storm_secs  seconds until the last VM was ready after a stop-and-start of all (empty if not run)
# share_at_limit   1 if the share ran at its IOPS limit
# short_pct        percent of the VMs that could not keep their target rate (an unreliable step)

# perf_knee STEPS_CSV [MS] — the first step where any one of these is true:
# more than 25 percent of VMs are slow at MS (10, 20 or 30; default 20), more than 25
# percent of VMs have a slow burst, any stall, a boot storm that takes more than twice as
# long per VM as the first boot storm, or the share at its IOPS limit. "none" if no step is a knee.
# Boot storms are compared per VM because a storm of more VMs is longer by itself.
perf_knee() {
    local col
    case "${2:-20}" in
        10) col=2 ;;
        20) col=3 ;;
        30) col=4 ;;
        *) echo "perf_knee: the limit must be 10, 20 or 30" >&2; return 2 ;;
    esac
    awk -F, -v col="$col" '
        NR == 1 { next }
        {
            knee = 0
            if ($col + 0 > 25) knee = 1
            if ($5 + 0 > 25) knee = 1
            if ($6 + 0 > 0) knee = 1
            if ($7 != "") {
                per = $7 / $1
                if (!have) { base = per; have = 1 } else if (per > 2 * base) knee = 1
            }
            if ($8 + 0 == 1) knee = 1
            if (knee) { print $1; found = 1; exit }
        }
        END { if (!found) print "none" }
    ' "$1"
}

# perf_recommended STEPS_CSV [MS] — the largest tested step at or below 80 percent of
# the knee. With no knee: "more than <last step>". With a knee at the first
# step: "below <first step>".
perf_recommended() {
    local csv="$1" knee first last limit best
    knee=$(perf_knee "$csv" "${2:-20}") || return
    first=$(awk -F, 'NR == 2 { print $1 }' "$csv")
    last=$(awk -F, 'NR > 1 { l = $1 } END { print l }' "$csv")
    if [ "$knee" = none ]; then
        echo "more than $last"
        return
    fi
    limit=$((knee * 80 / 100))
    best=$(awk -F, -v lim="$limit" 'NR > 1 && $1 + 0 <= lim { b = $1 } END { print b }' "$csv")
    if [ -z "$best" ]; then echo "below $first"; else echo "$best"; fi
}

# perf_unreliable STEPS_CSV — the steps where some VMs could not keep their target
# rate, as a space-separated list (nothing if there is none). Their latency is flattered,
# because a load generator that slows down measures less waiting.
perf_unreliable() {
    awk -F, 'NR > 1 && $9 + 0 > 0 { printf "%s%s", sep, $1; sep = " " } END { if (sep != "") print "" }' "$1"
}

# perf_table_row TIER_NAME STEPS_CSV — one row of the sizing table: the knee at 20 ms,
# the recommendation, then the knees at 10 ms and at 30 ms, so the answer can be read at each.
perf_table_row() {
    echo "| $1 | $(perf_knee "$2") | $(perf_recommended "$2") | $(perf_knee "$2" 10) | $(perf_knee "$2" 30) |"
}
