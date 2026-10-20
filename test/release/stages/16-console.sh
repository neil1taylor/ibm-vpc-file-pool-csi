#!/usr/bin/env bash
# 16-console — The console plugin's browser tests (Playwright) pass against the
# cluster's console, and a screenshot of each plugin tab is saved in the run
# folder. The browser signs in with a token, so no IBMid sign-in is needed: the
# stage makes a temporary user with a short-lived token and deletes both at
# the end. The tests run in Microsoft's Playwright container image, with
# podman, so the build server needs no Node.

RT_STAGE_TIMEOUT=1800

POOL=$(rt_name console)
TEST_USER=$(rt_name console-user)
PLAYWRIGHT_VERSION="${RT_PLAYWRIGHT_VERSION:-1.50.1}"
PLAYWRIGHT_IMAGE="${RT_PLAYWRIGHT_IMAGE:-mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-jammy}"
TEST_DIR="$(cd "$(dirname "$RT_LIB")/../../console-plugin/integration-tests" 2>/dev/null && pwd)"
SHOTS="$RT_OUT/console-screenshots"
TOKEN_FILE="${TMPDIR:-/tmp}/rt-$RT_ID-console-token"

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

# make_login_token — a User, a cluster-admin binding and a 2-hour OAuth access
# token for it. Prints the token. Everything has the run's label, so cleanup
# can delete it by label.
make_login_token() {
    local random token hash uid
    random=$(head -c 32 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')
    token="sha256~$random"
    hash=$(printf %s "$random" | openssl dgst -sha256 -binary | base64 | tr '+/' '-_' | tr -d '=\n')
    oc apply -f - >/dev/null <<YAML
apiVersion: user.openshift.io/v1
kind: User
metadata:
  name: $TEST_USER
  labels:
    $RT_LABEL: "$RT_ID"
YAML
    uid=$(oc get user "$TEST_USER" -o jsonpath='{.metadata.uid}')
    oc apply -f - >/dev/null <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $TEST_USER
  labels:
    $RT_LABEL: "$RT_ID"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - apiGroup: rbac.authorization.k8s.io
    kind: User
    name: $TEST_USER
YAML
    oc apply -f - >/dev/null <<YAML
apiVersion: oauth.openshift.io/v1
kind: OAuthAccessToken
metadata:
  name: sha256~$hash
  labels:
    $RT_LABEL: "$RT_ID"
clientName: openshift-browser-client
expiresIn: 7200
scopes: ["user:full"]
redirectURI: https://oauth-openshift.invalid/oauth/token/implicit
userName: $TEST_USER
userUID: $uid
YAML
    echo "$token"
}

token_works() {
    [ "$(oc --token="$(cat "$TOKEN_FILE")" whoami 2>/dev/null)" = "$TEST_USER" ]
}

# playwright_summary FILE — "passed failed" counts from Playwright's JSON report.
playwright_summary() {
    jq -r '"\(.stats.expected) \(.stats.unexpected + .stats.flaky)"' "$1"
}

run() {
    [ -d "$TEST_DIR" ] || skip "the console plugin tests are not in this copy of the scripts"
    command -v podman >/dev/null || skip "podman is not installed"
    local console_url work counts passed failed

    create_pool
    check "pool reaches Ready" wait_until 600 10 pool_is_ready "$POOL"

    (umask 077; make_login_token >"$TOKEN_FILE")
    check "the test user's token is accepted by the cluster" token_works

    console_url="https://$(oc get route -n openshift-console console -o jsonpath='{.spec.host}')"
    work=$(mktemp -d)
    cp -R "$TEST_DIR" "$work/integration-tests"
    mkdir -p "$SHOTS"

    podman run --rm \
        -v "$work:/work:Z" -v "$SHOTS:/screenshots:Z" \
        -e CONSOLE_URL="$console_url" -e OC_TOKEN="$(cat "$TOKEN_FILE")" \
        -e SCREENSHOT_DIR=/screenshots -e PLAYWRIGHT_JSON_OUTPUT_NAME=/work/result.json \
        -w /work "$PLAYWRIGHT_IMAGE" \
        sh -c "npm init -y >/dev/null && npm install --no-save --silent @playwright/test@$PLAYWRIGHT_VERSION && npx playwright test --config integration-tests/playwright.config.ts --reporter=json" \
        >"$RT_OUT/console-playwright.log" 2>&1 || true

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
    rm -f "$TOKEN_FILE"
    oc delete oauthaccesstokens,clusterrolebinding,user -l "$RT_LABEL=$RT_ID" --ignore-not-found --wait=true --timeout=120s
    oc delete filesharepool "$POOL" --ignore-not-found --wait=true --timeout=600s
}
