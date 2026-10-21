#!/usr/bin/env bash
# remote.sh — Start a release test run on the build server and return at once.
# The run keeps going when this machine sleeps.
#
# Usage:
#   bash test/release/remote.sh start CLUSTER [VERSION]   start a run, print its ID
#     (set RT_STAGES=01,08 to run only some stages)
#   bash test/release/remote.sh report RUN_ID             copy the report folder back and print it
#
# Needs: ibmcloud (logged in), go, ssh access to the build server.

set -euo pipefail

BUILD_HOST="${BUILD_HOST:-ubuntu@10.250.0.46}"
BUILD_KEY="${BUILD_KEY:-$HOME/.ssh/pool-csi-build}"
BUILD_VSI="${BUILD_VSI:-pool-csi-build}"
VPC_PROFILE="${RT_VPC_PROFILE:-pool-csi-build-vpc-reader}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"

build() { ssh -q -o ConnectTimeout=10 -i "$BUILD_KEY" -o IdentitiesOnly=yes "$BUILD_HOST" "$@"; }

ensure_server() {
    if build true 2>/dev/null; then return 0; fi
    echo "Starting build server $BUILD_VSI..."
    ibmcloud is instance-start "$BUILD_VSI" -q >/dev/null
    for _ in $(seq 1 30); do build true 2>/dev/null && return 0; sleep 10; done
    echo "build server did not answer over SSH" >&2
    return 1
}

start() {
    local cluster="$1" version="${2:-}" id kubeconfig profile_id
    id="$(date -u +%m%d-%H%M)"
    ensure_server
    build 'command -v jq >/dev/null || sudo apt-get install -y -q jq >/dev/null'

    echo "Building kubectl-migrate for the build server..."
    mkdir -p "$ROOT/test/release/bin"
    (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o test/release/bin/kubectl-migrate ./cmd/migrate/)

    echo "Copying the test scripts..."
    (cd "$ROOT" && tar -czf - test/release test/e2e/lib console-plugin/integration-tests) |
        build "rm -rf ~/release-test/src && mkdir -p ~/release-test/src ~/release-test/out && tar -xzf - -C ~/release-test/src"

    # An admin kubeconfig for this run only. The run deletes it when it ends.
    kubeconfig=$(mktemp -d)
    ibmcloud ks cluster config --cluster "$cluster" --admin --output yaml >"$kubeconfig/config"
    build "umask 077 && cat > ~/release-test/kubeconfig-$id" <"$kubeconfig/config"
    rm -rf "$kubeconfig"

    # The metadata service needs the trusted profile's ID. Without it the VPC
    # checks report "not checked" and the run goes on.
    profile_id=$(ibmcloud iam trusted-profile "$VPC_PROFILE" --id 2>/dev/null | tail -1 | grep '^Profile-' || true)

    # Only the run goes to the background, with every stream redirected, so
    # this SSH session can end at once.
    build "cd ~/release-test/src || exit 1; nohup bash -c 'KUBECONFIG=\$HOME/release-test/kubeconfig-$id RT_ID=$id \
        RT_VPC_PROFILE_ID=$profile_id \
        bash test/release/run.sh --cluster $cluster ${version:+--version $version} ${RT_STAGES:+--stages $RT_STAGES} --out \$HOME/release-test/out/$id; \
        rm -f \$HOME/release-test/kubeconfig-$id' >\$HOME/release-test/out/$id.log 2>&1 </dev/null &"
    echo "Started run $id on $cluster."
    echo "Report: bash test/release/remote.sh report $id"
}

report() {
    local id="$1" dest="$ROOT/test/release/out"
    mkdir -p "$dest"
    if build "test -f ~/release-test/out/$id/report.md"; then
        build "tar -czf - -C ~/release-test/out $id $id.log" | tar -xzf - -C "$dest"
        cat "$dest/$id/report.md"
    else
        echo "Run $id has not finished. Last lines of its log:"
        build "tail -5 ~/release-test/out/$id.log"
    fi
}

case "${1:-}" in
    start) shift; start "$@" ;;
    report) shift; report "$@" ;;
    *) sed -n '2,10p' "$0"; exit 2 ;;
esac
