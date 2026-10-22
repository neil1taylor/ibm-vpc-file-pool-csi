#!/usr/bin/env bash
# test-console-plugin.sh — Render the Helm chart and check that the console
# plugin is on by default on OpenShift, is turned on in the console by a hook
# Job, and is turned off again on uninstall or when it is disabled.
#
# Usage: bash test/chart/test-console-plugin.sh
# Requires: helm.

set -euo pipefail

CHART="$(cd "$(dirname "$0")/../.." && pwd)/charts/ibm-vpc-file-pool-csi"
FAILED=0
OPENSHIFT=(--api-versions console.openshift.io/v1/ConsolePlugin)

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }

render() {
    helm template test "$CHART" --namespace kube-system "$@"
}

expect_contains() {
    local desc="$1" out="$2" needle="$3"
    if grep -qF -- "$needle" <<<"$out"; then pass "$desc"; else fail "$desc (missing: $needle)"; fi
}

expect_absent() {
    local desc="$1" out="$2" needle="$3"
    if grep -qF -- "$needle" <<<"$out"; then fail "$desc (found: $needle)"; else pass "$desc"; fi
}

# job NAME OUT: print the rendered Job document called NAME.
job() {
    awk -v n="name: $1" '/^---/{if (doc ~ /kind: Job/ && index(doc, n)) print doc; doc=""; next} {doc = doc $0 "\n"} END{if (doc ~ /kind: Job/ && index(doc, n)) print doc}' <<<"$2"
}

# OpenShift, default values: plugin on, registered after install and upgrade, removed before delete.
out=$(render "${OPENSHIFT[@]}")
expect_contains "default on OpenShift: ConsolePlugin" "$out" "kind: ConsolePlugin"
expect_contains "default on OpenShift: plugin Deployment" "$out" "name: test-ibm-vpc-file-pool-csi-console-plugin"
reg=$(job test-ibm-vpc-file-pool-csi-console-plugin-register "$out")
expect_contains "register Job runs the register mode" "$reg" '"--mode=register-console-plugin"'
expect_contains "register Job names the plugin" "$reg" '"--console-plugin-name=ibm-vpc-file-pool-csi"'
expect_contains "register Job runs after install and upgrade" "$reg" '"helm.sh/hook": post-install,post-upgrade'
expect_contains "register Job uses the driver image" "$reg" 'image: "de.icr.io/ibm-vpc-file-pool-csi/driver:latest"'
unreg=$(job test-ibm-vpc-file-pool-csi-console-plugin-unregister "$out")
expect_contains "unregister Job runs the unregister mode" "$unreg" '"--mode=unregister-console-plugin"'
expect_contains "unregister Job runs before delete" "$unreg" '"helm.sh/hook": pre-delete'
expect_contains "RBAC: only the console operator configuration" "$out" 'resources: ["consoles"]'
expect_contains "RBAC: only the cluster object" "$out" 'resourceNames: ["cluster"]'
expect_contains "RBAC: get and update only" "$out" 'verbs: ["get", "update"]'

# OpenShift, plugin off: nothing deployed, and an upgrade removes a registration left from before.
out=$(render "${OPENSHIFT[@]}" --set consolePlugin.enabled=false)
expect_absent "off: no ConsolePlugin" "$out" "kind: ConsolePlugin"
expect_absent "off: no register Job" "$out" '"--mode=register-console-plugin"'
unreg=$(job test-ibm-vpc-file-pool-csi-console-plugin-unregister "$out")
expect_contains "off: unregister Job runs after upgrade" "$unreg" '"helm.sh/hook": post-upgrade,pre-delete'

# Plain Kubernetes: no console, so no plugin, no Jobs and no RBAC for them.
out=$(render)
expect_absent "no OpenShift console: no ConsolePlugin" "$out" "kind: ConsolePlugin"
expect_absent "no OpenShift console: no plugin Jobs" "$out" "console-plugin-register"
expect_absent "no OpenShift console: no plugin Jobs" "$out" "console-plugin-unregister"
expect_absent "no OpenShift console: no console RBAC" "$out" 'resources: ["consoles"]'

exit "$FAILED"
