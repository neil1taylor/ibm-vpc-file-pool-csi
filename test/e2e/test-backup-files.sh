#!/usr/bin/env bash
# End-to-end test of the browse helper on a real share (issue #92, phase 1).
#
# Makes a pool and a pod volume, fills it as three different users (a file with
# mode 0600, a folder with mode 0700, a link to /etc, a name with a newline),
# then runs the browse helper the way the controller will: as root, with every
# capability dropped, on a read-only NFS mount of the volume's folder. It checks
# that the helper lists and reads another user's private files, refuses a link,
# cannot write, and that a tar of the volume holds everything.
#
# Usage: KUBECONFIG=... [BROWSE_IMAGE=...] test/e2e/test-backup-files.sh [--keep]
# BROWSE_IMAGE defaults to the controller's image with "driver" changed to "browse".
set -euo pipefail

KEEP=false
[ "${1:-}" = "--keep" ] && KEEP=true
PREFIX=${PREFIX:-bf}
NS=${NS:-$PREFIX-test}
POOL=$PREFIX-pool
DRIVER_NS=${DRIVER_NS:-kube-system}
TOKEN=0123456789abcdef0123456789abcdef0123
PORT=${PORT:-18081}
PF_PID=""

log() { echo "[$(date -u +%H:%M:%S)] $*"; }
pass() { log "PASS: $*"; }
fail() { log "FAIL: $*"; exit 1; }

cleanup() {
    [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
    if $KEEP; then log "keeping test resources (--keep)"; return; fi
    log "cleaning up"
    oc delete pod "$PREFIX-browse" -n "$DRIVER_NS" --ignore-not-found --wait=true || true
    oc delete pvc "$PREFIX-share" -n "$DRIVER_NS" --ignore-not-found || true
    oc delete pv "$PREFIX-share" --ignore-not-found || true
    oc delete secret "$PREFIX-token" -n "$DRIVER_NS" --ignore-not-found || true
    oc delete ns "$NS" --ignore-not-found --wait=true --timeout=15m || true
    oc delete filesharepool "$POOL" --ignore-not-found --wait=false || true
    oc wait --for=delete "filesharepool/$POOL" --timeout=900s || log "the pool $POOL is not gone yet"
}
trap cleanup EXIT

IMG=${BROWSE_IMAGE:-$(oc get deploy -n "$DRIVER_NS" ibm-vpc-file-pool-csi-controller -o jsonpath='{.spec.template.spec.containers[0].image}' | sed 's#/driver:#/browse:#')}
log "browse image: $IMG"

zone() {
    oc get nodes -o jsonpath='{range .items[*]}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' |
        sort | uniq -c | sort -rn | awk 'NR==1{print $2}'
}

oc apply -f - <<YAML
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata: {name: $POOL}
spec:
  zone: $(zone)
  profile: dp2
  shareSizeGB: 100
  iops: 3000
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
YAML
end=$((SECONDS + 1200))
until [ "$(oc get filesharepool "$POOL" -o jsonpath='{.status.phase}' 2>/dev/null)" = Ready ]; do
    [ $SECONDS -ge $end ] && fail "the pool did not become Ready"
    sleep 10
done

oc create ns "$NS"
oc adm policy add-scc-to-user anyuid -z default -n "$NS" >/dev/null
oc get secret all-icr-io -n "$DRIVER_NS" -o json | jq 'del(.metadata.namespace,.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp)' | oc apply -n "$NS" -f - >/dev/null
oc apply -n "$NS" -f - <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: data}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: $POOL
  resources: {requests: {storage: 2Gi}}
YAML
until [ "$(oc get pvc data -n "$NS" -o jsonpath='{.status.phase}')" = Bound ]; do sleep 5; done

writer() { # writer <uid>
    cat <<YAML
  - name: w$1
    image: $IMG
    command: ["/bin/sh","-c"]
    args:
    - |
      set -e
      cd /data; mkdir -p u$1; cd u$1
      echo secret > secret0600; chmod 600 secret0600
      echo public > public0644
      mkdir -p private0700; chmod 700 private0700; echo inside > private0700/inner0600; chmod 600 private0700/inner0600
      ln -sfn /etc to-etc
      nl=\$(printf '\\n'); printf x > "line\${nl}break"
    securityContext: {runAsUser: $1, runAsGroup: $1, allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts: [{name: d, mountPath: /data}]
YAML
}
{
    cat <<YAML
apiVersion: v1
kind: Pod
metadata: {name: writer}
spec:
  restartPolicy: Never
  imagePullSecrets: [{name: all-icr-io}]
  volumes: [{name: d, persistentVolumeClaim: {claimName: data}}]
  initContainers:
YAML
    writer 1000; writer 2000; writer 0
    cat <<YAML
  containers:
  - name: done
    image: $IMG
    command: ["/bin/true"]
YAML
} | oc apply -n "$NS" -f -
end=$((SECONDS + 600))
until [ "$(oc get pod writer -n "$NS" -o jsonpath='{.status.phase}')" = Succeeded ]; do
    [ "$(oc get pod writer -n "$NS" -o jsonpath='{.status.phase}')" = Failed ] && fail "the writer pod failed"
    [ $SECONDS -ge $end ] && fail "the writer pod did not finish"
    sleep 5
done
pass "three users wrote their files"

SHARE=$(oc get filesharepool "$POOL" -o json)
IP=$(jq -r '.status.shares[0].mountTargetIP' <<<"$SHARE")
EXPORT=$(jq -r '.status.shares[0].exportPath' <<<"$SHARE")
PV=$(oc get pvc data -n "$NS" -o jsonpath='{.spec.volumeName}')
SUB=$(oc get subvolume "$PV" -o jsonpath='{.spec.subPath}' | sed 's#^/##')
oc create secret generic "$PREFIX-token" -n "$DRIVER_NS" --from-literal=token="$TOKEN" >/dev/null
oc apply -f - <<YAML
apiVersion: v1
kind: PersistentVolume
metadata: {name: $PREFIX-share}
spec:
  capacity: {storage: 1Gi}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  mountOptions: [nfsvers=4.1, soft, timeo=600, retrans=3, sec=sys]
  nfs: {server: $IP, path: $EXPORT, readOnly: true}
  claimRef: {name: $PREFIX-share, namespace: $DRIVER_NS}
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $PREFIX-share, namespace: $DRIVER_NS}
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ""
  volumeName: $PREFIX-share
  resources: {requests: {storage: 1Gi}}
---
apiVersion: v1
kind: Pod
metadata: {name: $PREFIX-browse, namespace: $DRIVER_NS}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  imagePullSecrets: [{name: all-icr-io}]
  securityContext: {runAsUser: 0}
  volumes:
  - {name: share, persistentVolumeClaim: {claimName: $PREFIX-share, readOnly: true}}
  - {name: token, secret: {secretName: $PREFIX-token}}
  containers:
  - name: browse
    image: $IMG
    args: ["--mode=browse-server","--root=/data","--listen=:8080","--token-file=/etc/browse/token"]
    securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
    volumeMounts:
    - {name: share, mountPath: /data, subPath: $SUB, readOnly: true}
    - {name: token, mountPath: /etc/browse, readOnly: true}
YAML
oc wait pod "$PREFIX-browse" -n "$DRIVER_NS" --for=condition=Ready --timeout=300s >/dev/null || {
    # Say why, before cleanup deletes the pod.
    oc get pod "$PREFIX-browse" -n "$DRIVER_NS" -o wide 2>&1 || true
    oc describe pod "$PREFIX-browse" -n "$DRIVER_NS" 2>&1 | grep -E "^Node:|State:|Reason:|Exit Code|Message:|Warning|Failed|Back-off|Unhealthy" || true
    oc logs "$PREFIX-browse" -n "$DRIVER_NS" --tail=40 2>&1 || true
    fail "the helper pod did not start"
}
oc port-forward -n "$DRIVER_NS" "pod/$PREFIX-browse" "$PORT:8080" >/dev/null 2>&1 &
PF_PID=$!
sleep 3

b64() { printf '%s' "$1" | base64; }
api() { curl -s -H "Authorization: Bearer $TOKEN" -X POST "localhost:$PORT$1" -d "$2"; }
file() { api /v1/file "{\"path\":[$(for p in "$@"; do printf '"%s",' "$(b64 "$p")"; done | sed 's/,$//')],\"length\":-1}"; }

[ "$(file u2000 secret0600)" = secret ] && pass "the helper reads another user's file with mode 0600" || fail "cannot read u2000/secret0600"
[ "$(file u2000 private0700 inner0600)" = inside ] && pass "the helper reads a file in another user's folder with mode 0700" || fail "cannot read u2000/private0700/inner0600"
[ "$(file u0 secret0600)" = secret ] && pass "the helper reads a file made by root" || fail "cannot read u0/secret0600"
api /v1/file "{\"path\":[\"$(b64 u1000)\",\"$(b64 to-etc)\",\"$(b64 passwd)\"],\"length\":-1}" | jq -e '.code == "link_refused"' >/dev/null && pass "a path through a link to /etc is refused" || fail "a link was followed"
api /v1/list "{\"path\":[\"$(b64 u1000)\"]}" | jq -e '([.entries[] | select(.readable | not)] | length) == 0 and ([.entries[] | select(.type == "symlink" and (.link | @base64d) == "/etc")] | length) == 1' >/dev/null && pass "the list shows a link as a link and every entry as readable" || fail "the list is wrong"
n=$(curl -s -H "Authorization: Bearer $TOKEN" -X POST "localhost:$PORT/v1/tar" -d '{"path":[]}' | tar tf - | wc -l | tr -d ' ')
[ "$n" -ge 21 ] && pass "a tar of the volume holds all $n entries" || fail "the tar holds only $n entries"
out=$(oc exec -n "$DRIVER_NS" "$PREFIX-browse" -- sh -c 'touch /data/x' 2>&1 || true)
[[ "$out" == *"Read-only"* ]] && pass "the helper cannot write to the backup" || fail "the helper could write"
oc exec -n "$DRIVER_NS" "$PREFIX-browse" -- grep -q '^CapEff:.0000000000000000' /proc/self/status && pass "the helper has no capabilities" || fail "the helper has capabilities"
log "all checks passed"
