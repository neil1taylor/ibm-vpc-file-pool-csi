#!/usr/bin/env bash
# test-runner.sh — Tests for the release test runner itself, with fake stages
# and a stub `oc`. Needs no cluster.
#
# Usage: bash test/release/test-runner.sh

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

expect_contains() {
    local desc="$1" file="$2" needle="$3"
    if grep -qF -- "$needle" "$file"; then pass "$desc"; else fail "$desc (missing: $needle)"; cat "$file"; fi
}

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

# Stub oc: reports the cluster context, and answers every other call with nothing.
mkdir -p "$WORK/bin"
cat >"$WORK/bin/oc" <<'EOF'
#!/usr/bin/env bash
if [ "$1 $2" = "config current-context" ]; then echo "${STUB_CONTEXT}/abc/admin"; fi
exit 0
EOF
chmod +x "$WORK/bin/oc"

# Fake stages.
mkdir -p "$WORK/stages"
cat >"$WORK/stages/01-mixed.sh" <<'EOF'
run() {
    check "true is true" true
    check "false is true" false
}
cleanup() { echo cleaned >"$RT_OUT/01.cleanup"; }
EOF
cat >"$WORK/stages/02-good.sh" <<'EOF'
run() { check "still runs after a failed stage" true; }
cleanup() { :; }
EOF
cat >"$WORK/stages/03-skip.sh" <<'EOF'
run() { skip "no group snapshot API"; check "never reached" false; }
cleanup() { :; }
EOF
cat >"$WORK/stages/04-slow.sh" <<'EOF'
RT_STAGE_TIMEOUT=2
run() { sleep 30; }
cleanup() { echo cleaned >"$RT_OUT/04.cleanup"; }
EOF
cat >"$WORK/stages/05-crash.sh" <<'EOF'
run() { exit 3; }
cleanup() { :; }
EOF
cat >"$WORK/stages/06-partial.sh" <<'EOF'
run() {
    check "a check that can run" true
    not_checked "share access mode" "no VPC access"
}
cleanup() { :; }
EOF

run_runner() {
    local out="$1"; shift
    PATH="$WORK/bin:$PATH" RT_STAGE_DIR="$WORK/stages" RT_SWEEP=0 \
        bash "$HERE/run.sh" --out "$out" "$@" >"$out.log" 2>&1
}

# Full run against the right cluster.
OUT="$WORK/run1"
if STUB_CONTEXT=lab-cluster run_runner "$OUT" --cluster lab-cluster; then
    fail "run exits non-zero when a stage fails"
else
    pass "run exits non-zero when a stage fails"
fi
R="$OUT/report.md"
expect_contains "mixed stage is FAIL" "$R" "| 01-mixed | FAIL |"
expect_contains "failed check is named" "$R" "false is true"
expect_contains "next stage still runs" "$R" "| 02-good | PASS |"
expect_contains "skip gives its reason" "$R" "| 03-skip | SKIP |"
expect_contains "skip reason is shown" "$R" "no group snapshot API"
expect_contains "slow stage times out" "$R" "| 04-slow | FAIL |"
expect_contains "timeout is named" "$R" "timed out after 2s"
expect_contains "crashing stage is FAIL" "$R" "| 05-crash | FAIL |"
expect_contains "exit code is named" "$R" "stage exited with 3"
expect_contains "a check that cannot run leaves the stage PASS" "$R" "| 06-partial | PASS |"
expect_contains "a check that cannot run is reported" "$R" "not checked: share access mode (no VPC access)"
if [ -f "$OUT/01.cleanup" ]; then pass "cleanup runs after a failed stage"; else fail "cleanup runs after a failed stage"; fi
if [ -f "$OUT/04.cleanup" ]; then pass "cleanup runs after a timeout"; else fail "cleanup runs after a timeout"; fi

# Only chosen stages, all passing: exit code zero.
OUT="$WORK/run2"
if STUB_CONTEXT=lab-cluster run_runner "$OUT" --cluster lab-cluster --stages 02,03; then
    pass "run exits zero when every stage passes or skips"
else
    fail "run exits zero when every stage passes or skips"
fi
if grep -qF "01-mixed" "$OUT/report.md"; then fail "--stages runs only the chosen stages"; else pass "--stages runs only the chosen stages"; fi

# Wrong cluster: refuse to start and run nothing.
OUT="$WORK/run3"
if STUB_CONTEXT=other-cluster run_runner "$OUT" --cluster lab-cluster; then
    fail "run refuses a different cluster"
else
    pass "run refuses a different cluster"
fi
expect_contains "refusal names both clusters" "$OUT.log" "is other-cluster, not lab-cluster"
if [ -f "$OUT/01.cleanup" ]; then fail "no stage runs on the wrong cluster"; else pass "no stage runs on the wrong cluster"; fi

# Helpers that read the log of an end-to-end script (used by stage 18).
source "$HERE/lib.sh"
E2ELOG="$WORK/e2e.log"
printf '[10:00:01] making pools\n[10:05:00] PASS: three volumes backed up, two moments each\n[10:09:00] FAIL: the root disk backup is not FileSystemConsistent\n[10:09:01] FAIL: a later one\n' >"$E2ELOG"
if [ "$(first_fail_line "$E2ELOG")" = "FAIL: the root disk backup is not FileSystemConsistent" ]; then
    pass "first_fail_line prints the first FAIL line without the time"
else
    fail "first_fail_line prints the first FAIL line without the time"
fi
printf '[10:00:01] PASS: all good\n' >"$WORK/e2e-ok.log"
if first_fail_line "$WORK/e2e-ok.log" >/dev/null; then fail "first_fail_line fails when there is no FAIL line"; else pass "first_fail_line fails when there is no FAIL line"; fi
if first_fail_line "$WORK/missing.log" >/dev/null; then fail "first_fail_line fails when the log is missing"; else pass "first_fail_line fails when the log is missing"; fi
if log_has_pass "$E2ELOG" "three volumes backed up"; then pass "log_has_pass finds a PASS line"; else fail "log_has_pass finds a PASS line"; fi
if log_has_pass "$E2ELOG" "the latest restore"; then fail "log_has_pass is false for a PASS line that is not there"; else pass "log_has_pass is false for a PASS line that is not there"; fi
if log_has_pass "$E2ELOG" "making pools"; then fail "log_has_pass ignores lines that are not PASS lines"; else pass "log_has_pass ignores lines that are not PASS lines"; fi

exit "$FAILED"
