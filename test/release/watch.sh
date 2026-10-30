#!/usr/bin/env bash
# watch.sh — Follow a release test run on the build server and print one line per event:
#   START   a stage started (with its time limit)
#   FAIL    a check failed (at once, not at the end of the stage)
#   STAGE   a stage ended, with its result and duration
#   STALLED no new check result for STALL_MIN minutes (default 60)
#   OVERDUE a stage ran past its time limit
#   UNREACHABLE / REACHABLE  the build server stopped or started answering
#   FINISHED the run ended, with its counts; the script then exits
#
# It keeps how far it has read in a state file, so a restarted watcher goes on where the
# last one stopped: no event is missed and none is printed twice.
#
# Usage: bash test/release/watch.sh RUN_ID
set -uo pipefail

ID="${1:?usage: watch.sh RUN_ID}"
BUILD_HOST="${BUILD_HOST:-ubuntu@10.250.0.46}"
BUILD_KEY="${BUILD_KEY:-$HOME/.ssh/pool-csi-build}"
STALL_MIN="${STALL_MIN:-60}"
POLL="${POLL:-30}"
STATE_DIR="${WATCH_STATE_DIR:-$HOME/.cache/pool-csi-watch}"
STATE="$STATE_DIR/$ID"
mkdir -p "$STATE_DIR"

build() { ssh -q -o ConnectTimeout=10 -o BatchMode=yes -i "$BUILD_KEY" -o IdentitiesOnly=yes "$BUILD_HOST" "$@"; }

# State: lines read from results.tsv, stages.tsv and the run log; when a result last arrived;
# the running stage, its start and limit; and which alarms were already given.
results=0 stages=0 logl=0 last_result=$(date +%s) stage="" stage_start=0 stage_limit=0
stalled_for="" overdue_for="" down_since=0 down_said=0
# shellcheck disable=SC1090
[ -f "$STATE" ] && . "$STATE"
save() {
    printf 'results=%s stages=%s logl=%s last_result=%s stage=%q stage_start=%s stage_limit=%s stalled_for=%q overdue_for=%q\n' \
        "$results" "$stages" "$logl" "$last_result" "$stage" "$stage_start" "$stage_limit" "$stalled_for" "$overdue_for" >"$STATE"
}

O="release-test/out/$ID"
while true; do
    # One call returns the new lines of the three files, split by markers.
    # Stage starts come first, so a catch-up prints a start before that stage's results and end.
    if ! out=$(build "O=$O; echo @@L; tail -n +$((logl + 1)) \$O.log 2>/dev/null | grep -E ': start \\(limit [0-9]+s\\)' ; echo @@L2 \$(wc -l < \$O.log 2>/dev/null || echo $logl); echo @@R; tail -n +$((results + 1)) \$O/results.tsv 2>/dev/null; echo @@S; tail -n +$((stages + 1)) \$O/stages.tsv 2>/dev/null; test -f \$O/report.md && echo @@DONE; echo @@END"); then
        now=$(date +%s)
        [ "$down_since" -eq 0 ] && down_since=$now
        if [ "$down_said" -eq 0 ] && [ $((now - down_since)) -ge 300 ]; then
            echo "UNREACHABLE: the build server has not answered for 5 minutes"
            down_said=1
        fi
        sleep "$POLL"; continue
    fi
    [ "$down_said" -eq 1 ] && echo "REACHABLE: the build server answers again"
    down_since=0 down_said=0
    case "$out" in *@@END*) ;; *) sleep "$POLL"; continue ;; esac

    part=none
    while IFS= read -r line; do
        case "$line" in
            @@R) part=results; continue ;;
            @@S) part=stages; continue ;;
            @@L) part=log; continue ;;
            @@L2*) logl=${line#@@L2 }; continue ;;
            @@DONE) part=done; continue ;;
            @@END) break ;;
        esac
        case "$part" in
            results)
                [ -z "$line" ] && continue
                results=$((results + 1)); last_result=$(date +%s); stalled_for=""
                case "$line" in FAIL*) echo "FAIL ${line#FAIL	}" | tr '\t' ' ' ;; esac ;;
            stages)
                [ -z "$line" ] && continue
                stages=$((stages + 1))
                echo "STAGE $line" | tr '\t' ' '
                stage="" ;;
            log)
                [ -z "$line" ] && continue
                stage=$(sed -E 's/^\[[^]]*\] ([^:]+): start.*/\1/' <<<"$line")
                stage_limit=$(sed -E 's/.*limit ([0-9]+)s.*/\1/' <<<"$line")
                stage_start=$(date +%s); overdue_for=""; last_result=$stage_start
                echo "START $stage (limit $((stage_limit / 60)) min)" ;;
        esac
    done <<<"$out"

    if [ "$part" = done ]; then
        save
        counts=$(build "O=$O; printf '%s pass, %s fail, %s skip' \$(grep -c '^PASS' \$O/results.tsv) \$(grep -c '^FAIL' \$O/results.tsv) \$(grep -c '^SKIP' \$O/results.tsv)")
        echo "FINISHED: run $ID, $counts"
        exit 0
    fi

    now=$(date +%s)
    if [ -n "$stage" ]; then
        if [ "$stalled_for" != "$stage" ] && [ $((now - last_result)) -ge $((STALL_MIN * 60)) ]; then
            echo "STALLED: no new result in $stage for $STALL_MIN minutes"
            stalled_for=$stage
        fi
        if [ "$overdue_for" != "$stage" ] && [ "$stage_limit" -gt 0 ] && [ $((now - stage_start)) -gt "$stage_limit" ]; then
            echo "OVERDUE: $stage ran past its limit of $((stage_limit / 60)) minutes"
            overdue_for=$stage
        fi
    fi
    save
    sleep "$POLL"
done
