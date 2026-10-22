#!/usr/bin/env bash
# test-diagnostics.sh — Run scripts/collect-diagnostics.sh against a fake kubectl.
# Checks: it writes one file, it never reads a Secret, and it removes secret-looking values.
#
# Usage: bash test/chart/test-diagnostics.sh

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SCRIPT="$ROOT/scripts/collect-diagnostics.sh"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILED=0

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

# A fake kubectl: it records each call and prints a line that holds a fake API key.
mkdir -p "$WORK/bin"
cat > "$WORK/bin/kubectl" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >> "$FAKE_CALLS"
case "$*" in
  *"get deployment"*"-o jsonpath"*) printf 'de.icr.io/ibm-vpc-file-pool-csi/driver:v9.9.9' ;;
  *"get pods"*"jsonpath"*) printf 'node-a node-b' ;;
  *) echo "output of: $*"; echo 'apiKey: abc123SECRETvalue'; echo 'token=eyJhbGciOi.payload.sig' ;;
esac
FAKE
chmod +x "$WORK/bin/kubectl"
export FAKE_CALLS="$WORK/calls.log"
: > "$FAKE_CALLS"

OUT="$WORK/out"
mkdir -p "$OUT"
if PATH="$WORK/bin:$PATH" bash "$SCRIPT" "$OUT" >"$WORK/run.log" 2>&1; then
    pass "the script runs"
else
    cat "$WORK/run.log"; fail "the script failed"
fi

FILE="$(ls "$OUT"/csi-diagnostics-*.txt 2>/dev/null | head -1 || true)"
if [ -n "$FILE" ]; then pass "it writes csi-diagnostics-<time>.txt"; else fail "no output file"; exit 1; fi

if grep -qi "secret" "$FAKE_CALLS" && grep -qiE "get (secret|secrets)( |$)" "$FAKE_CALLS"; then
    fail "it reads a Secret"
else
    pass "it never reads a Secret"
fi

for section in "Driver version" "Pools" "SubVolumes" "Snapshots" "Backup policies" "Replication policies" "Controller logs" "Node agent logs" "Events"; do
    if grep -qF "=== $section" "$FILE"; then pass "section: $section"; else fail "missing section: $section"; fi
done

if grep -qE "abc123SECRETvalue|eyJhbGciOi" "$FILE"; then
    fail "a secret-looking value is in the file"
else
    pass "secret-looking values are removed"
fi

if grep -qF "v9.9.9" "$FILE"; then pass "it records the driver version"; else fail "no driver version"; fi

exit "$FAILED"
