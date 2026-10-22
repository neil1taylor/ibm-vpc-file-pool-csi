#!/usr/bin/env bash
# test-detect-zone.sh — Check detect_zone against a stub kubectl.
#
# Usage: bash test/e2e/lib/test-detect-zone.sh

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=zone.sh
source "$HERE/zone.sh"
FAILED=0
STUB="$(mktemp -d)"
trap 'rm -rf "$STUB"' EXIT

# stub_nodes writes a kubectl that prints the given zone labels, one per worker.
stub_nodes() {
    printf '#!/usr/bin/env bash\nprintf "%%s\\n" %s\n' "$*" > "$STUB/kubectl"
    chmod +x "$STUB/kubectl"
}

expect() {
    local desc="$1" want="$2" got
    got="$(PATH="$STUB:$PATH" detect_zone 2>/dev/null || echo "<error>")"
    if [[ "$got" == "$want" ]]; then echo "PASS  $desc"; else echo "FAIL  $desc: got '$got', want '$want'"; FAILED=1; fi
}

stub_nodes eu-de-3 eu-de-3 eu-de-3
expect "all workers in one zone" "eu-de-3"

stub_nodes eu-de-1 eu-de-2 eu-de-2
expect "the zone with the most workers" "eu-de-2"

stub_nodes us-south-2 us-south-1
expect "a tie picks the first zone by name" "us-south-1"

stub_nodes ""
expect "no zone labels is an error" "<error>"

exit "$FAILED"
