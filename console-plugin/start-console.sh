#!/usr/bin/env bash
#
# Local development helper: downloads and runs the OCP console bridge
# with the plugin served from webpack-dev-server on :9001.
#
set -euo pipefail

CONSOLE_IMAGE=${CONSOLE_IMAGE:-"quay.io/openshift/origin-console:latest"}
CONSOLE_PORT=${CONSOLE_PORT:-9000}
PLUGIN_PORT=${PLUGIN_PORT:-9001}

echo "Starting console on http://localhost:${CONSOLE_PORT}"
echo "Plugin dev server expected on http://localhost:${PLUGIN_PORT}"
echo ""
echo "Make sure 'yarn dev' is running in another terminal."
echo ""

# Ensure logged into an OpenShift cluster
if ! oc whoami &>/dev/null; then
    echo "ERROR: Not logged into an OpenShift cluster. Run 'oc login' first."
    exit 1
fi

BRIDGE_K8S_MODE_OFF_CLUSTER_ENDPOINT=$(oc whoami --show-server)
BRIDGE_K8S_AUTH_BEARER_TOKEN=$(oc whoami --show-token 2>/dev/null || true)
BRIDGE_USER_AUTH="disabled"
BRIDGE_K8S_MODE="off-cluster"
BRIDGE_PLUGINS="ibm-vpc-file-pool-csi=http://localhost:${PLUGIN_PORT}"

echo "API server: ${BRIDGE_K8S_MODE_OFF_CLUSTER_ENDPOINT}"
echo "Plugin proxy: ${BRIDGE_PLUGINS}"
echo ""

podman run --rm -it \
    --name ocp-console \
    -p "${CONSOLE_PORT}:9000" \
    -e BRIDGE_K8S_MODE="${BRIDGE_K8S_MODE}" \
    -e BRIDGE_K8S_MODE_OFF_CLUSTER_ENDPOINT="${BRIDGE_K8S_MODE_OFF_CLUSTER_ENDPOINT}" \
    -e BRIDGE_K8S_AUTH_BEARER_TOKEN="${BRIDGE_K8S_AUTH_BEARER_TOKEN}" \
    -e BRIDGE_USER_AUTH="${BRIDGE_USER_AUTH}" \
    -e BRIDGE_PLUGINS="${BRIDGE_PLUGINS}" \
    -e BRIDGE_K8S_MODE_OFF_CLUSTER_SKIP_VERIFY_TLS=true \
    "${CONSOLE_IMAGE}"
