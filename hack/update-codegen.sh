#!/usr/bin/env bash
set -euo pipefail

# Regenerate CRD manifests and DeepCopy methods.
# Run after modifying types in api/v1alpha1/.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

echo "Generating DeepCopy methods..."
controller-gen object paths="${ROOT_DIR}/api/..."

echo "Generating CRD manifests..."
controller-gen crd paths="${ROOT_DIR}/api/..." output:crd:dir="${ROOT_DIR}/config/crd"

echo "Done."
