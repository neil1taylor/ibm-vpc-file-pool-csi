#!/usr/bin/env bash
# run.sh — Run the release test stages against one cluster and write a report.
#
# Usage: bash test/release/run.sh --cluster NAME [--stages 01,02] [--version TAG] [--out DIR]
#
# Each file in stages/ is one stage. A stage that fails does not stop the
# others, every stage's cleanup runs, and a final sweep deletes anything
# labelled with this run's ID. The exit code is 1 if any stage failed.

set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
source "$HERE/lib.sh"

CLUSTER=""
STAGES=""
export RT_EXPECTED_VERSION=""
RT_OUT=""
while [ $# -gt 0 ]; do
    case "$1" in
        --cluster) CLUSTER="$2"; shift 2 ;;
        --stages) STAGES="$2"; shift 2 ;;
        --version) RT_EXPECTED_VERSION="$2"; shift 2 ;;
        --out) RT_OUT="$2"; shift 2 ;;
        *) echo "unknown option: $1" >&2; exit 2 ;;
    esac
done
[ -n "$CLUSTER" ] || { echo "--cluster is required" >&2; exit 2; }

export RT_ID="${RT_ID:-$(date -u +%m%d-%H%M)}"
export RT_OUT="${RT_OUT:-$HERE/out/$RT_ID}"
export RT_CLUSTER="$CLUSTER"
export RT_LIB="$HERE/lib.sh"
STAGE_DIR="${RT_STAGE_DIR:-$HERE/stages}"
mkdir -p "$RT_OUT"
: >"$RT_OUT/results.tsv"
: >"$RT_OUT/stages.tsv"

current=$(oc config current-context 2>/dev/null | cut -d/ -f1)
if [ "$current" != "$CLUSTER" ]; then
    echo "Refusing to run: the current cluster is ${current:-unknown}, not $CLUSTER" >&2
    exit 2
fi

# run_limited SECONDS STAGE_FILE FUNCTION — run one stage function in its own
# shell, killed after SECONDS. Returns its exit code, or 124 on timeout.
run_limited() {
    local limit="$1" file="$2" fn="$3"
    bash -c 'source "$RT_LIB"; source "$1"; "$2"' _ "$file" "$fn" >>"$RT_OUT/$RT_STAGE.log" 2>&1 &
    local pid=$! waited=0
    while kill -0 "$pid" 2>/dev/null; do
        if [ "$waited" -ge "$limit" ]; then
            pkill -P "$pid" 2>/dev/null
            kill "$pid" 2>/dev/null
            wait "$pid" 2>/dev/null
            return 124
        fi
        sleep 1
        waited=$((waited + 1))
    done
    wait "$pid"
}

stage_result() {
    local stage="$1"
    if grep -q "^FAIL	$stage	" "$RT_OUT/results.tsv"; then echo FAIL
    elif grep -q "^SKIP	$stage	" "$RT_OUT/results.tsv"; then echo SKIP
    elif grep -q "^PASS	$stage	" "$RT_OUT/results.tsv"; then echo PASS
    else echo FAIL; _record FAIL "the stage recorded no checks"
    fi
}

run_stage() {
    local file="$1"
    export RT_STAGE
    RT_STAGE=$(basename "$file" .sh)
    local limit start rc
    limit=$(RT_STAGE_TIMEOUT=1800; source "$file" >/dev/null 2>&1; echo "$RT_STAGE_TIMEOUT")
    start=$(date +%s)
    rt_log "start (limit ${limit}s)"
    run_limited "$limit" "$file" run
    rc=$?
    if [ "$rc" -eq 124 ]; then _record FAIL "timed out after ${limit}s"
    elif [ "$rc" -ne 0 ]; then _record FAIL "stage exited with $rc"
    fi
    run_limited 600 "$file" cleanup || rt_log "cleanup did not finish cleanly"
    printf '%s\t%s\t%s\n' "$RT_STAGE" "$(stage_result "$RT_STAGE")" "$(( $(date +%s) - start ))" >>"$RT_OUT/stages.tsv"
}

sweep() {
    [ "${RT_SWEEP:-1}" = "0" ] && return 0
    export RT_STAGE="99-leftovers"
    rt_log "deleting everything labelled $(rt_labels)"
    oc delete namespace -l "$(rt_labels)" --wait=true --timeout=600s >/dev/null 2>&1
    oc delete filesharepools -l "$(rt_labels)" --wait=true --timeout=600s >/dev/null 2>&1
    # Browse sessions and file restores have no namespace, and the gateway makes them without the
    # run's label, so they are found by the run's policy names (rt-<run>-...).
    local backup_objs
    backup_objs=$(oc get backupbrowse,backupfilerestore -o json 2>/dev/null |
        jq -r --arg p "$(rt_name '')" '.items[] | select((.spec.policy // "") | startswith($p)) | "\(.kind | ascii_downcase)/\(.metadata.name)"')
    if [ -n "$backup_objs" ]; then
        rt_log "a stage left these behind, deleting: $(echo "$backup_objs" | tr '\n' ' ')"
        echo "$backup_objs" | xargs oc delete --ignore-not-found --wait=true --timeout=300s >/dev/null 2>&1
    fi
    local left
    left=$(oc get namespaces,filesharepools -l "$(rt_labels)" -o name 2>/dev/null)
    left="$left$(oc get backupbrowse,backupfilerestore -o json 2>/dev/null |
        jq -r --arg p "$(rt_name '')" '.items[] | select((.spec.policy // "") | startswith($p)) | " \(.kind)/\(.metadata.name)"')"
    if [ -n "$left" ]; then _record FAIL "left behind: $(echo "$left" | tr '\n' ' ')"
    else _record PASS "nothing left on the cluster"
    fi
    if declare -F vpc_leftovers >/dev/null; then vpc_leftovers; fi
    printf '%s\t%s\t%s\n' "$RT_STAGE" "$(stage_result "$RT_STAGE")" 0 >>"$RT_OUT/stages.tsv"
}

write_report() {
    {
        echo "# Release test run $RT_ID"
        echo
        echo "Cluster: $RT_CLUSTER. Expected driver version: ${RT_EXPECTED_VERSION:-not given}."
        echo
        echo "| Stage | Result | Time | Notes |"
        echo "|---|---|---|---|"
        while IFS=$'\t' read -r stage result secs; do
            notes=$(awk -F'\t' -v s="$stage" '$2 == s && ($1 == "FAIL" || $1 == "SKIP" || $1 == "NOTE") {print $3}' "$RT_OUT/results.tsv" | paste -sd ';' - | sed 's/;/; /g')
            echo "| $stage | $result | $((secs / 60))m$((secs % 60))s | $notes |"
        done <"$RT_OUT/stages.tsv"
    } >"$RT_OUT/report.md"
}

for file in "$STAGE_DIR"/[0-9][0-9]-*.sh; do
    num=$(basename "$file" | cut -c1-2)
    if [ -n "$STAGES" ] && ! echo ",$STAGES," | grep -q ",$num,"; then continue; fi
    run_stage "$file"
done
sweep
write_report
cat "$RT_OUT/report.md"

if grep -q "	FAIL	" "$RT_OUT/stages.tsv"; then exit 1; fi
exit 0
