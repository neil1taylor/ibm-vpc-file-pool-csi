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
# The first run uses the read-only token. A second part then starts a second
# console with a write token for the test namespace only, and restores a
# snapshot to a new claim in the browser (the restore form of issue #45). The
# read-only token is never widened.
#
# What this does not test: IBM's sign-in step, which the plugin does not own.

RT_STAGE_TIMEOUT=4500

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

# The restore part: its own service account, token and console.
WRITE_SA=console-restore
WRITE_PORT=9100
WRITE_TOKEN_FILE="${TMPDIR:-/tmp}/rt-$RT_ID-console-write-token"
WRITE_CONSOLE_CONTAINER="rt-$RT_ID-console-write"
SNAPSHOT_CLASS="${RT_SNAPSHOT_CLASS:-ibm-vpc-file-pool}"
RESTORED=restored-in-console

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
  # cluster-reader does not cover snapshot contents, and the restore page waits for them.
  - apiGroups: ["snapshot.storage.k8s.io"]
    resources: ["volumesnapshotcontents"]
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

# make_write_token — a ServiceAccount for the restore test. It reads like the
# read-only one, and it may change objects in the test namespace only (the
# built-in edit role, bound by a RoleBinding). Prints a 2-hour token.
make_write_token() {
    oc create serviceaccount "$WRITE_SA" -n "$NS" >/dev/null
    oc apply -f - >/dev/null <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS-restore
  labels:
    $RT_LABEL: "$RT_ID"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-reader
subjects:
  - kind: ServiceAccount
    name: $WRITE_SA
    namespace: $NS
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS-restore-pools
  labels:
    $RT_LABEL: "$RT_ID"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: $NS
subjects:
  - kind: ServiceAccount
    name: $WRITE_SA
    namespace: $NS
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: restore-edit
  namespace: $NS
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: edit
subjects:
  - kind: ServiceAccount
    name: $WRITE_SA
    namespace: $NS
YAML
    oc create token "$WRITE_SA" -n "$NS" --duration=2h
}

write_sa_can() { oc auth can-i "$@" --as="system:serviceaccount:$NS:$WRITE_SA" >/dev/null 2>&1; }
write_token_can_create_claims_here() { write_sa_can create persistentvolumeclaims -n "$NS"; }
# It must not reach beyond the test namespace, nor change the driver's objects.
write_token_stays_in_namespace() {
    ! write_sa_can create persistentvolumeclaims -n default &&
        ! write_sa_can create pods -n default &&
        ! write_sa_can delete filesharepools.storage.ibmcloud.io &&
        ! write_sa_can create filesharepools.storage.ibmcloud.io
}

snapshot_is_ready() { [ "$(oc get volumesnapshot -n "$NS" console-snap -o jsonpath='{.status.readyToUse}' 2>/dev/null)" = "true" ]; }
restored_is_bound() { [ "$(oc get pvc -n "$NS" "$RESTORED" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Bound" ]; }
restored_files_match() { oc exec -n "$NS" console-reader -- sh -c 'cd /data && sha256sum -c original.sha256'; }

# snapshot_object_name — the name of the driver's Snapshot object for console-snap,
# which is the last part of the snapshot handle of its VolumeSnapshotContent.
snapshot_object_name() {
    local content handle
    content=$(oc get volumesnapshot -n "$NS" console-snap -o jsonpath='{.status.boundVolumeSnapshotContentName}')
    handle=$(oc get volumesnapshotcontent "$content" -o jsonpath='{.status.snapshotHandle}')
    echo "${handle##*/}"
}

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

console_is_up() { curl -fsS "http://localhost:${1:-$CONSOLE_PORT}/health" >/dev/null 2>&1; }

# start_console CONTAINER PORT TOKEN_FILE — a console that signs in nobody and
# uses the token for every call to the cluster. Needs $api, $work and ca_args
# from run().
start_console() {
    local name=$1 port=$2 token_file=$3
    (umask 077; printf 'BRIDGE_K8S_AUTH_BEARER_TOKEN=%s\n' "$(cat "$token_file")" >"$work/$name.env")
    podman run -d --rm --network host --name "$name" \
        -e BRIDGE_LISTEN="http://127.0.0.1:$port" \
        -e BRIDGE_USER_AUTH=disabled \
        -e BRIDGE_K8S_MODE=off-cluster \
        -e BRIDGE_K8S_MODE_OFF_CLUSTER_ENDPOINT="$api" \
        "${ca_args[@]}" \
        -e BRIDGE_K8S_AUTH=bearer-token \
        --env-file "$work/$name.env" \
        -e BRIDGE_PLUGINS="$PLUGIN_NAME=http://127.0.0.1:$PLUGIN_PORT" \
        "$CONSOLE_IMAGE" >/dev/null
}

# run_playwright PORT RESULT LOG [FILE [PODMAN_ARG...]] — the browser tests
# against the console on PORT, as a JSON report in $work/RESULT. FILE limits the
# run to one test file; extra arguments (such as -e NAME=value) go to podman.
run_playwright() {
    local port=$1 result=$2 log=$3 file=${4:-}
    shift $(($# < 4 ? $# : 4))
    podman run --rm --network host \
        -v "$work:/work:Z" -v "$SHOTS:/screenshots:Z" -v "$RT_OUT/console-test-results:/work/test-results:Z" \
        -e CONSOLE_URL="http://localhost:$port" -e CONSOLE_AUTH=none \
        -e SCREENSHOT_DIR=/screenshots -e PLAYWRIGHT_JSON_OUTPUT_NAME="/work/$result" \
        "$@" \
        -w /work "$PLAYWRIGHT_IMAGE" \
        sh -c "npm init -y >/dev/null && npm install --no-save --silent @playwright/test@$PLAYWRIGHT_VERSION && npx playwright test --config integration-tests/playwright.config.ts --reporter=json $file" \
        >"$log" 2>&1 || true
}

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
    start_console "$CONSOLE_CONTAINER" "$CONSOLE_PORT" "$TOKEN_FILE"
    check "the local console starts" wait_until 120 3 console_is_up

    run_playwright "$CONSOLE_PORT" result.json "$RT_OUT/console-playwright.log"
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

    restore_in_browser
    # Clone and migrate use the console of the restore part (a write token for the test namespace).
    if console_is_up "$WRITE_PORT"; then
        clone_in_browser
        migrate_in_browser
    else
        echo "SKIP: the restore part did not run, so the clone and migrate tests are not run"
    fi
    drain_in_browser
    rm -rf "$work"
}

# restore_in_browser — Part two. Data goes into the test claim and a snapshot is
# taken. A console with a write token for the test namespace restores the
# snapshot to a new claim in the browser. Then a pod reads the new claim and
# the files must match the data at snapshot time. Needs $work, $api and ca_args
# from run().
restore_in_browser() {
    local node snap result counts passed failed
    oc get volumesnapshotclass "$SNAPSHOT_CLASS" >/dev/null 2>&1 || {
        echo "SKIP: no VolumeSnapshotClass $SNAPSHOT_CLASS, so the restore test is not run"
        return 0
    }
    node=$(worker_nodes | sed -n 1p)

    (umask 077; make_write_token >"$WRITE_TOKEN_FILE")
    check "the restore token can create claims in the test namespace" write_token_can_create_claims_here
    check "the restore token can do nothing outside the test namespace" write_token_stays_in_namespace

    pod_on_node "$NS" console-writer "$node" console-test \
        "head -c 5242880 /dev/urandom > /data/payload && cd /data && sha256sum payload > original.sha256 && touch /data/written && sleep infinity"
    check "the writer runs" wait_until 300 5 pod_is_running "$NS" console-writer
    check "the writer wrote its files" wait_until 120 5 oc exec -n "$NS" console-writer -- test -f /data/written

    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: console-snap
spec:
  volumeSnapshotClassName: $SNAPSHOT_CLASS
  source:
    persistentVolumeClaimName: console-test
YAML
    check "the snapshot becomes ready to use" wait_until 600 10 snapshot_is_ready
    snap=$(snapshot_object_name)
    echo "snapshot object: $snap"
    # The restored claim must hold the data of snapshot time, not of later writes.
    oc exec -n "$NS" console-writer -- sh -c 'head -c 1048576 /dev/urandom > /data/payload'

    start_console "$WRITE_CONSOLE_CONTAINER" "$WRITE_PORT" "$WRITE_TOKEN_FILE"
    check "the console with the restore token starts" wait_until 120 3 console_is_up "$WRITE_PORT"

    run_playwright "$WRITE_PORT" result-restore.json "$RT_OUT/console-playwright-restore.log" \
        integration-tests/tests/restore.spec.ts \
        -e RESTORE_NS="$NS" -e RESTORE_SNAPSHOT="$snap" -e RESTORE_SOURCE=console-test -e RESTORE_NAME="$RESTORED"
    podman logs "$WRITE_CONSOLE_CONTAINER" >"$RT_OUT/console-bridge-restore.log" 2>&1 || true

    result="$work/result-restore.json"
    check "Playwright wrote a result file for the restore test" test -s "$result"
    if [ -s "$result" ]; then
        counts=$(playwright_summary "$result")
        passed=${counts% *}
        failed=${counts#* }
        echo "restore browser tests: $passed passed, $failed failed"
        check "no restore browser test failed ($passed passed)" test "$failed" -eq 0
        check "the restore browser tests ran" test "$passed" -ge 4
    fi

    # The browser said it worked. Check the cluster itself.
    check "the claim made in the browser is bound" wait_until 900 10 restored_is_bound
    pod_on_node "$NS" console-reader "$node" "$RESTORED" "sleep infinity"
    check "the reader of the restored claim runs" wait_until 300 5 pod_is_running "$NS" console-reader
    check "the restored files match the data at snapshot time" restored_files_match
}

# The drain, clone and migrate parts (#46 items 5, 6 and 7). Each part gives
# the browser only the rights it needs.
DRAIN_SA=console-drain
DRAIN_PORT=9200
DRAIN_TOKEN_FILE="${TMPDIR:-/tmp}/rt-$RT_ID-console-drain-token"
DRAIN_CONSOLE_CONTAINER="rt-$RT_ID-console-drain"
CLONED=cloned-in-console
MIGRATE_SC="${RT_MIGRATE_SC:-ocs-storagecluster-cephfs}"
MIGRATE_CLAIM=migrate-src

# make_drain_token — a ServiceAccount that reads like the read-only one and may
# patch the test pool, and no other object. Prints a 2-hour token.
make_drain_token() {
    oc create serviceaccount "$DRAIN_SA" -n "$NS" >/dev/null
    oc apply -f - >/dev/null <<YAML
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: $NS-drain
  labels:
    $RT_LABEL: "$RT_ID"
rules:
  - apiGroups: ["storage.ibmcloud.io"]
    resources: ["filesharepools"]
    resourceNames: ["$POOL"]
    verbs: ["patch"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS-drain
  labels:
    $RT_LABEL: "$RT_ID"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: $NS-drain
subjects:
  - kind: ServiceAccount
    name: $DRAIN_SA
    namespace: $NS
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS-drain-read
  labels:
    $RT_LABEL: "$RT_ID"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-reader
subjects:
  - kind: ServiceAccount
    name: $DRAIN_SA
    namespace: $NS
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $NS-drain-pools
  labels:
    $RT_LABEL: "$RT_ID"
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: $NS
subjects:
  - kind: ServiceAccount
    name: $DRAIN_SA
    namespace: $NS
YAML
    oc create token "$DRAIN_SA" -n "$NS" --duration=2h
}

drain_sa_can() { oc auth can-i "$@" --as="system:serviceaccount:$NS:$DRAIN_SA" >/dev/null 2>&1; }
drain_token_patches_only_the_test_pool() {
    drain_sa_can patch "filesharepools.storage.ibmcloud.io/$POOL" &&
        ! drain_sa_can patch "filesharepools.storage.ibmcloud.io/not-$POOL" &&
        ! drain_sa_can create filesharepools.storage.ibmcloud.io &&
        ! drain_sa_can delete filesharepools.storage.ibmcloud.io "$POOL" &&
        ! drain_sa_can create persistentvolumeclaims -n "$NS"
}

pool_does_not_drain() { ! pool_drains "$1"; }
pool_drains() { oc get filesharepool "$POOL" -o jsonpath='{.spec.drainShares}' | grep -q "$1"; }
share_state_is() { [ "$(oc get filesharepool "$POOL" -o jsonpath='{.status.shares[0].state}')" = "$1" ]; }
share_is_not_draining() { [ "$(oc get filesharepool "$POOL" -o jsonpath='{.status.shares[0].state}')" != "draining" ]; }

# browser_result RESULT LABEL MIN — the checks on one Playwright JSON report.
browser_result() {
    local result="$work/$1" counts passed failed
    check "Playwright wrote a result file for $2" test -s "$result"
    if [ -s "$result" ]; then
        counts=$(playwright_summary "$result")
        passed=${counts% *}
        failed=${counts#* }
        echo "$2: $passed passed, $failed failed"
        check "no $2 failed ($passed passed)" test "$failed" -eq 0
        check "the $2 ran" test "$passed" -ge "$3"
    fi
}

# drain_in_browser — Drain the pool's share in the browser, check the pool, then
# return it to service in the browser, and check the pool again.
drain_in_browser() {
    local share_id share_name
    share_id=$(oc get filesharepool "$POOL" -o jsonpath='{.status.shares[0].shareID}')
    share_name=$(oc get filesharepool "$POOL" -o jsonpath='{.status.shares[0].shareName}')
    check "the pool has a share to drain" test -n "$share_id" -a -n "$share_name"

    (umask 077; make_drain_token >"$DRAIN_TOKEN_FILE")
    check "the drain token can change the test pool and nothing else" drain_token_patches_only_the_test_pool
    start_console "$DRAIN_CONSOLE_CONTAINER" "$DRAIN_PORT" "$DRAIN_TOKEN_FILE"
    check "the console with the drain token starts" wait_until 120 3 console_is_up "$DRAIN_PORT"

    run_playwright "$DRAIN_PORT" result-drain.json "$RT_OUT/console-playwright-drain.log" \
        integration-tests/tests/drain.spec.ts \
        -e DRAIN_POOL="$POOL" -e DRAIN_SHARE="$share_name" -e DRAIN_PHASE=drain
    browser_result result-drain.json "drain browser test" 1
    check "the share is on the pool's drain list" pool_drains "$share_id"
    check "the pool reports the share as draining" wait_until 120 5 share_state_is draining

    run_playwright "$DRAIN_PORT" result-return.json "$RT_OUT/console-playwright-return.log" \
        integration-tests/tests/drain.spec.ts \
        -e DRAIN_POOL="$POOL" -e DRAIN_SHARE="$share_name" -e DRAIN_PHASE=return
    browser_result result-return.json "return-to-service browser test" 1
    check "the share is off the drain list" pool_does_not_drain "$share_id"
    check "the pool no longer reports the share as draining" wait_until 120 5 share_is_not_draining
    podman logs "$DRAIN_CONSOLE_CONTAINER" >"$RT_OUT/console-bridge-drain.log" 2>&1 || true
}

cloned_is_bound() { [ "$(oc get pvc -n "$NS" "$CLONED" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Bound" ]; }
cloned_files_match() { oc exec -n "$NS" console-clone-reader -- sh -c 'cd /data && sha256sum -c current.sha256'; }

# clone_in_browser — Clone the test volume in the browser (the write console from
# the restore part is still running), then a pod reads the new claim and the
# files must match the source.
clone_in_browser() {
    local node pv
    node=$(worker_nodes | sed -n 1p)
    oc exec -n "$NS" console-writer -- sh -c 'cd /data && sha256sum payload > current.sha256 && sync'
    pv=$(oc get pvc -n "$NS" console-test -o jsonpath='{.spec.volumeName}')
    echo "source volume: $pv"

    run_playwright "$WRITE_PORT" result-clone.json "$RT_OUT/console-playwright-clone.log" \
        integration-tests/tests/clone.spec.ts \
        -e CLONE_SUBVOLUME="$pv" -e CLONE_NS="$NS" -e CLONE_SOURCE=console-test -e CLONE_NAME="$CLONED"
    browser_result result-clone.json "clone browser test" 4

    check "the claim made in the browser is bound" wait_until 900 10 cloned_is_bound
    pod_on_node "$NS" console-clone-reader "$node" "$CLONED" "sleep infinity"
    check "the reader of the cloned claim runs" wait_until 300 5 pod_is_running "$NS" console-clone-reader
    check "the cloned files match the source" cloned_files_match
}

migrate_source_is_bound() { [ "$(oc get pvc -n "$NS" "$MIGRATE_CLAIM" -o jsonpath='{.status.phase}')" = "Bound" ]; }
migrated_is_bound() { [ "$(oc get pvc -n "$NS" "$MIGRATE_CLAIM-pooled" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Bound" ]; }
class_of() { oc get pvc -n "$NS" "$1" -o jsonpath='{.spec.storageClassName}'; }
migrated_files_match() { oc exec -n "$NS" console-migrate-reader -- sh -c 'cd /data && sha256sum -c original.sha256'; }

# migrate_in_browser — Move a claim of another storage class into the pool in the
# browser. The new claim must be bound on the pool and hold the same data, and
# the old claim must be untouched.
migrate_in_browser() {
    local node
    oc get storageclass "$MIGRATE_SC" >/dev/null 2>&1 || {
        echo "SKIP: no StorageClass $MIGRATE_SC to migrate from, so the migrate test is not run"
        return 0
    }
    node=$(worker_nodes | sed -n 1p)
    oc apply -n "$NS" -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $MIGRATE_CLAIM
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $MIGRATE_SC
  resources:
    requests:
      storage: 1Gi
YAML
    check "the claim to migrate is bound on $MIGRATE_SC" wait_until 300 5 migrate_source_is_bound
    pod_on_node "$NS" console-migrate-writer "$node" "$MIGRATE_CLAIM" \
        "head -c 3145728 /dev/urandom > /data/payload && cd /data && sha256sum payload > original.sha256 && touch /data/written && sleep infinity"
    check "the writer of the claim to migrate runs" wait_until 300 5 pod_is_running "$NS" console-migrate-writer
    check "it wrote its files" wait_until 120 5 oc exec -n "$NS" console-migrate-writer -- test -f /data/written
    # The page does not offer a claim that a pod uses, so the writer goes first.
    oc delete pod -n "$NS" console-migrate-writer --wait=true --timeout=120s >/dev/null

    # The copy pod needs to run as root (issue #196: it fails for a user without that right). The grant is
    # a role binding in the test namespace and goes with it.
    oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null
    run_playwright "$WRITE_PORT" result-migrate.json "$RT_OUT/console-playwright-migrate.log" \
        integration-tests/tests/migrate.spec.ts \
        -e MIGRATE_NS="$NS" -e MIGRATE_CLAIM="$MIGRATE_CLAIM" -e MIGRATE_CLASS="$MIGRATE_SC" -e MIGRATE_POOL="$POOL"
    browser_result result-migrate.json "migrate browser test" 2
    podman logs "$WRITE_CONSOLE_CONTAINER" >"$RT_OUT/console-bridge-write.log" 2>&1 || true

    check "the new claim is bound" wait_until 300 5 migrated_is_bound
    check "the new claim is on the pool" test "$(class_of "$MIGRATE_CLAIM-pooled")" = "$POOL"
    check "the old claim is still on $MIGRATE_SC" test "$(class_of "$MIGRATE_CLAIM")" = "$MIGRATE_SC"
    pod_on_node "$NS" console-migrate-reader "$node" "$MIGRATE_CLAIM-pooled" "sleep infinity"
    check "the reader of the new claim runs" wait_until 300 5 pod_is_running "$NS" console-migrate-reader
    check "the new claim has the files of the old claim" migrated_files_match
}

cleanup() {
    podman stop -t 5 "$DRAIN_CONSOLE_CONTAINER" "$WRITE_CONSOLE_CONTAINER" "$CONSOLE_CONTAINER" "$PLUGIN_CONTAINER" >/dev/null 2>&1 || true
    rm -f "$TOKEN_FILE" "$WRITE_TOKEN_FILE" "$DRAIN_TOKEN_FILE"
    oc delete clusterrolebinding,clusterrole -l "$RT_LABEL=$RT_ID" --ignore-not-found --wait=true --timeout=120s
    oc delete namespace "$NS" --ignore-not-found --wait=true --timeout=300s
    oc delete filesharepool "$POOL" --ignore-not-found --wait=true --timeout=600s
}
