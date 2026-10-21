#!/usr/bin/env bash
# 16-console — The console plugin's browser tests (Playwright) pass, and a
# screenshot of each plugin tab is saved in the run folder.
#
# The cluster's own console signs in through IBM's login page, so a browser
# cannot sign in with a token. This stage runs the console itself in a
# container on the build server (OpenShift's origin-console image, with user
# sign-in switched off) and gives it a read-only token of a temporary service
# account. It serves the same plugin image the cluster runs. The browser tests
# then run against that local console, in Microsoft's Playwright image. Both
# run with podman, so the build server needs no Node.
#
# What this does not test: IBM's sign-in step, which the plugin does not own.

RT_STAGE_TIMEOUT=1800

POOL=$(rt_name console)
NS=$(rt_name console)
SA=console-test
PLAYWRIGHT_VERSION="${RT_PLAYWRIGHT_VERSION:-1.50.1}"
PLAYWRIGHT_IMAGE="${RT_PLAYWRIGHT_IMAGE:-mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-jammy}"
CONSOLE_IMAGE="${RT_CONSOLE_IMAGE:-quay.io/openshift/origin-console:4.21}"
PLUGIN_NAME=ibm-vpc-file-pool-csi
PLUGIN_PORT=9001
CONSOLE_PORT=9000
TEST_DIR="$(cd "$(dirname "$RT_LIB")/../../console-plugin/integration-tests" 2>/dev/null && pwd)"
SHOTS="$RT_OUT/console-screenshots"
TOKEN_FILE="${TMPDIR:-/tmp}/rt-$RT_ID-console-token"
PLUGIN_CONTAINER="rt-$RT_ID-plugin"
CONSOLE_CONTAINER="rt-$RT_ID-console"

create_pool() {
    oc apply -f - >/dev/null <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: $POOL
  labels:
    $RT_LABEL: "$RT_ID"
spec:
  zone: $(detect_zone)
  profile: dp2
  shareSizeGB: 200
  iops: 3000
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
YAML
}

# make_read_only_token — a ServiceAccount that can only read (the built-in
# cluster-reader role: get, list and watch on everything except Secrets), and
# a 2-hour token for it. Prints the token.
make_read_only_token() {
    rt_namespace "$NS"
    oc create serviceaccount "$SA" -n "$NS" >/dev/null
    # cluster-reader does not cover the driver's own resource types, so a small
    # read-only role for them is added.
    oc apply -f - >/dev/null <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: $NS
  labels:
    $RT_LABEL: "$RT_ID"
rules:
  - apiGroups: ["storage.ibmcloud.io"]
    resources: ["*"]
    verbs: ["get", "list", "watch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS
  labels:
    $RT_LABEL: "$RT_ID"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-reader
subjects:
  - kind: ServiceAccount
    name: $SA
    namespace: $NS
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS-pools
  labels:
    $RT_LABEL: "$RT_ID"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: $NS
subjects:
  - kind: ServiceAccount
    name: $SA
    namespace: $NS
YAML
    oc create token "$SA" -n "$NS" --duration=2h
}

sa_can() { oc auth can-i "$@" --as="system:serviceaccount:$NS:$SA" >/dev/null 2>&1; }
token_reads_pools() { sa_can list filesharepools.storage.ibmcloud.io; }
token_cannot_write() { ! sa_can create pods -n default && ! sa_can delete filesharepools.storage.ibmcloud.io; }

# A volume, so that the SubVolumes tab and the dashboard have a row to show.
create_claim() {
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: console-test
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $POOL
  resources:
    requests:
      storage: 1Gi
YAML
}
claim_is_bound() { [ "$(oc get pvc -n "$NS" console-test -o jsonpath='{.status.phase}')" = "Bound" ]; }

console_is_up() { curl -fsS "http://localhost:$CONSOLE_PORT/health" >/dev/null 2>&1; }

# playwright_summary FILE — "passed failed" counts from Playwright's JSON report.
playwright_summary() {
    jq -r '"\(.stats.expected) \(.stats.unexpected + .stats.flaky)"' "$1"
}

run() {
    [ -d "$TEST_DIR" ] || skip "the console plugin tests are not in this copy of the scripts"
    command -v podman >/dev/null || skip "podman is not installed"
    local plugin_image api work counts passed failed

    plugin_image=$(oc get deploy -n "$RT_DRIVER_NS" -l "app.kubernetes.io/component=console-plugin" -o jsonpath='{.items[0].spec.template.spec.containers[0].image}' 2>/dev/null)
    [ -n "$plugin_image" ] || skip "the console plugin is not installed (Helm consolePlugin.enabled)"

    create_pool
    check "pool reaches Ready" wait_until 600 10 pool_is_ready "$POOL"

    (umask 077; make_read_only_token >"$TOKEN_FILE")
    check "the test token can read pools" token_reads_pools
    check "the test token cannot create or delete anything" token_cannot_write
    create_claim
    check "a test volume is bound" wait_until 300 5 claim_is_bound

    api=$(oc whoami --show-server)
    work=$(mktemp -d)
    cp -R "$TEST_DIR" "$work/integration-tests"
    mkdir -p "$SHOTS" "$RT_OUT/console-test-results"

    # The console always checks the cluster's certificate. It uses the CA of the kubeconfig when there is one.
    # IBM's cluster addresses use a publicly trusted certificate, so the kubeconfig may have none: then the
    # console uses the system trust store of its image. Its token goes in a file, not on the command line.
    ca_args=()
    oc config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d >"$work/ca.crt" 2>/dev/null
    if [ -s "$work/ca.crt" ]; then
        ca_args=(-v "$work/ca.crt:/ca.crt:ro,Z" -e BRIDGE_K8S_MODE_OFF_CLUSTER_CA_FILE=/ca.crt)
    fi
    (umask 077; printf 'BRIDGE_K8S_AUTH_BEARER_TOKEN=%s\n' "$(cat "$TOKEN_FILE")" >"$work/console.env")

    cat >"$work/nginx.conf" <<NGINX
pid /tmp/nginx.pid;
events { worker_connections 64; }
http {
    include /etc/nginx/mime.types;
    server {
        listen $PLUGIN_PORT;
        root /opt/app-root/src;
        location / { try_files \$uri \$uri/ =404; }
    }
}
NGINX

    podman run -d --rm --network host --name "$PLUGIN_CONTAINER" \
        -v "$work/nginx.conf:/etc/nginx/nginx.conf:ro,Z" "$plugin_image" >/dev/null
    podman run -d --rm --network host --name "$CONSOLE_CONTAINER" \
        -e BRIDGE_LISTEN="http://127.0.0.1:$CONSOLE_PORT" \
        -e BRIDGE_USER_AUTH=disabled \
        -e BRIDGE_K8S_MODE=off-cluster \
        -e BRIDGE_K8S_MODE_OFF_CLUSTER_ENDPOINT="$api" \
        "${ca_args[@]}" \
        -e BRIDGE_K8S_AUTH=bearer-token \
        --env-file "$work/console.env" \
        -e BRIDGE_PLUGINS="$PLUGIN_NAME=http://127.0.0.1:$PLUGIN_PORT" \
        "$CONSOLE_IMAGE" >/dev/null
    check "the local console starts" wait_until 120 3 console_is_up

    podman run --rm --network host \
        -v "$work:/work:Z" -v "$SHOTS:/screenshots:Z" -v "$RT_OUT/console-test-results:/work/test-results:Z" \
        -e CONSOLE_URL="http://localhost:$CONSOLE_PORT" -e CONSOLE_AUTH=none \
        -e SCREENSHOT_DIR=/screenshots -e PLAYWRIGHT_JSON_OUTPUT_NAME=/work/result.json \
        -w /work "$PLAYWRIGHT_IMAGE" \
        sh -c "npm init -y >/dev/null && npm install --no-save --silent @playwright/test@$PLAYWRIGHT_VERSION && npx playwright test --config integration-tests/playwright.config.ts --reporter=json" \
        >"$RT_OUT/console-playwright.log" 2>&1 || true
    podman logs "$CONSOLE_CONTAINER" >"$RT_OUT/console-bridge.log" 2>&1 || true

    check "Playwright wrote a result file" test -s "$work/result.json"
    if [ -s "$work/result.json" ]; then
        counts=$(playwright_summary "$work/result.json")
        passed=${counts% *}
        failed=${counts#* }
        echo "browser tests: $passed passed, $failed failed"
        check "no browser test failed ($passed passed)" test "$failed" -eq 0
        check "at least one browser test ran" test "$passed" -gt 0
    fi
    check "a screenshot of each plugin tab was saved" \
        test "$(find "$SHOTS" -name '*.png' | wc -l)" -ge 7
    rm -rf "$work"
}

cleanup() {
    podman stop -t 5 "$CONSOLE_CONTAINER" "$PLUGIN_CONTAINER" >/dev/null 2>&1 || true
    rm -f "$TOKEN_FILE"
    oc delete clusterrolebinding,clusterrole -l "$RT_LABEL=$RT_ID" --ignore-not-found --wait=true --timeout=120s
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=300s
    oc delete filesharepool "$POOL" --ignore-not-found --wait=true --timeout=600s
}
