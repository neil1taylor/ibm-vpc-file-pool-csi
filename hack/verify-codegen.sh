#!/usr/bin/env bash
set -euo pipefail

# Verify that generated code is up to date.
# Fails if running update-codegen.sh would produce changes.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

TMPDIR=$(mktemp -d)
trap 'rm -rf "${TMPDIR}"' EXIT

# Copy current generated files
cp -r "${ROOT_DIR}/config/crd" "${TMPDIR}/crd-before"

# Regenerate
"${SCRIPT_DIR}/update-codegen.sh"

# Compare
if ! diff -r "${TMPDIR}/crd-before" "${ROOT_DIR}/config/crd" > /dev/null 2>&1; then
    echo "ERROR: Generated CRD files are out of date. Run 'make generate' and commit the result."
    exit 1
fi

echo "Generated code is up to date."
