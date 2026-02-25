# Part 3: Group Snapshots and Hooks

> **Tutorial Series:** [Index](TUTORIAL.md) | [Part 1: Pool Creation](TUTORIAL-01-POOL-CREATION.md) | [Part 2: Snapshots & Clones](TUTORIAL-02-SNAPSHOTS-AND-CLONES.md) | **Part 3** | [Part 4: Pool Configuration](TUTORIAL-04-POOL-CONFIGURATION.md) | [Part 5: Golden Images](TUTORIAL-05-GOLDEN-IMAGES.md) | [Part 6: Replication & Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md) | [Part 7: Monitoring](TUTORIAL-07-MONITORING.md) | [Part 8: Migration & Console](TUTORIAL-08-MIGRATION-AND-CONSOLE.md)

Step-by-step guide to create coordinated multi-PVC snapshots using VolumeGroupSnapshot CRs, with pre/post lifecycle hooks (exec and HTTP) for application-consistent backups.

**Cluster:** ROKS eu-de (OpenShift Virtualization enabled)
**Zone:** eu-de-1

---

## Prerequisites

Verify the CSI driver is running:

```bash
# Controller pod (6/6 Running)
oc get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi -l app.kubernetes.io/component=controller

# Node pods (3/3 Running on each schedulable node)
oc get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi -l app.kubernetes.io/component=node

# CSI Driver registered
oc get csidriver vpc-file-pool.csi.ibm.io
```

Verify the VolumeGroupSnapshot CRD is installed:

```bash
oc get crd volumegroupsnapshots.storage.ibmcloud.io
```

---

## Step 1: Clean Up Any Previous Test Resources

```bash
POOL_NAME=groupsnap-pool
TUTORIAL_NS=pool-tutorial-groupsnap

# 1. Delete pods and services
oc delete pods --all -n ${TUTORIAL_NS} --force --grace-period=0 --ignore-not-found
oc delete svc --all -n ${TUTORIAL_NS} --ignore-not-found

# 2. Delete VolumeGroupSnapshots
oc delete volumegroupsnapshots.storage.ibmcloud.io --all --ignore-not-found --timeout=60s

# 3. Delete PVCs
oc delete pvc --all -n ${TUTORIAL_NS} --timeout=60s --ignore-not-found

# 4. Grace period for CSI to cascade deletes
sleep 5

# 5. Force-clean orphan Snapshots
for snap in $(oc get snapshots.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} -o name 2>/dev/null); do
    oc patch "$snap" --type=merge -p '{"metadata":{"finalizers":null}}'
done
oc delete snapshots.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} --ignore-not-found

# 6. Force-clean orphan SubVolumes
for sv in $(oc get subvolumes.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} -o name 2>/dev/null); do
    oc patch "$sv" --type=merge -p '{"metadata":{"finalizers":null}}'
done
oc delete subvolumes.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} --ignore-not-found

# 7. Delete pool (force-remove finalizer if stuck)
oc delete filesharepool ${POOL_NAME} --timeout=60s --ignore-not-found || {
    oc patch filesharepools.storage.ibmcloud.io ${POOL_NAME} \
        --type=merge -p '{"metadata":{"finalizers":null}}'
    oc delete filesharepool ${POOL_NAME} --ignore-not-found
}

# 8. Delete namespace
oc delete namespace ${TUTORIAL_NS} --timeout=60s --ignore-not-found
```

---

## Step 2: Set Up Variables, Namespace, and Pool

```bash
export POOL_NAME=groupsnap-pool
export TUTORIAL_NS=pool-tutorial-groupsnap
oc create namespace ${TUTORIAL_NS}
```

Create a FileSharePool:

```bash
cat <<EOF | oc apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: ${POOL_NAME}
spec:
  zone: eu-de-1
  profile: dp2
  shareSizeGB: 100
  iops: 100
  maxShares: 2
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultPermissions: "0755"
EOF
```

Watch the pool initialize:

```bash
oc get filesharepools -w
```

Wait until `PHASE` shows `Ready`:

```
NAME              ZONE      PROFILE   SHARES   CAPACITY   ALLOCATED   PVCS   PHASE
groupsnap-pool    eu-de-1   dp2       1        100        0           0      Ready
```

Verify the auto-created StorageClass:

```bash
oc get sc ${POOL_NAME}
```

---

## Step 3: Create a Multi-Component Application

Simulate a 3-component application with separate PVCs for data, write-ahead log, and configuration. This mirrors a real-world database or stateful application where all components must be snapshotted together for consistency.

Create the 3 PVCs:

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: app-data
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 5Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: app-wal
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 2Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: app-config
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 1Gi
EOF
```

Wait for all PVCs to bind:

```bash
oc get pvc -n ${TUTORIAL_NS}
# All 3 should show STATUS=Bound
```

Populate each PVC with known test data:

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: app-data-writer
  namespace: ${TUTORIAL_NS}
spec:
  restartPolicy: Never
  containers:
    - name: writer
      image: busybox
      command:
        - sh
        - -c
        - |
          echo '{"table":"users","rows":1000}' > /data/users.json
          echo '{"table":"orders","rows":5000}' > /data/orders.json
          echo "Data files written"
          ls -la /data/
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: app-data
---
apiVersion: v1
kind: Pod
metadata:
  name: app-wal-writer
  namespace: ${TUTORIAL_NS}
spec:
  restartPolicy: Never
  containers:
    - name: writer
      image: busybox
      command:
        - sh
        - -c
        - |
          echo "WAL entry 001: INSERT users" > /wal/wal-001.log
          echo "WAL entry 002: UPDATE orders" > /wal/wal-002.log
          echo "WAL entry 003: DELETE temp" > /wal/wal-003.log
          echo "WAL files written"
          ls -la /wal/
      volumeMounts:
        - name: wal
          mountPath: /wal
  volumes:
    - name: wal
      persistentVolumeClaim:
        claimName: app-wal
---
apiVersion: v1
kind: Pod
metadata:
  name: app-config-writer
  namespace: ${TUTORIAL_NS}
spec:
  restartPolicy: Never
  containers:
    - name: writer
      image: busybox
      command:
        - sh
        - -c
        - |
          echo "max_connections=100" > /config/app.conf
          echo "log_level=info" >> /config/app.conf
          echo "Config files written"
          ls -la /config/
      volumeMounts:
        - name: config
          mountPath: /config
  volumes:
    - name: config
      persistentVolumeClaim:
        claimName: app-config
EOF
```

Wait for all writers to complete:

```bash
oc get pods -n ${TUTORIAL_NS} -w
# Wait for all 3 writer pods to show STATUS=Completed

oc logs app-data-writer -n ${TUTORIAL_NS}
oc logs app-wal-writer -n ${TUTORIAL_NS}
oc logs app-config-writer -n ${TUTORIAL_NS}
```

Clean up writer pods:

```bash
oc delete pod app-data-writer app-wal-writer app-config-writer -n ${TUTORIAL_NS} --force --grace-period=0
```

Now get the SubVolume names. The VolumeGroupSnapshot `sourcePVCs` field takes **SubVolume names**, not PVC names:

```bash
oc get subvolumes -o wide
```

Expected output (names will differ):

```
NAME                        POOL              SHARE          SIZE   PVC          NAMESPACE                  PHASE
sv-app-config-xxxxxxxx      groupsnap-pool    r010-...       1      app-config   pool-tutorial-groupsnap    Bound
sv-app-data-xxxxxxxx        groupsnap-pool    r010-...       5      app-data     pool-tutorial-groupsnap    Bound
sv-app-wal-xxxxxxxx         groupsnap-pool    r010-...       2      app-wal      pool-tutorial-groupsnap    Bound
```

Save the SubVolume names:

```bash
SV_DATA=$(oc get subvolumes -o json | jq -r '.items[] | select(.spec.pvcName=="app-data") | .metadata.name')
SV_WAL=$(oc get subvolumes -o json | jq -r '.items[] | select(.spec.pvcName=="app-wal") | .metadata.name')
SV_CONFIG=$(oc get subvolumes -o json | jq -r '.items[] | select(.spec.pvcName=="app-config") | .metadata.name')

echo "Data SV:   ${SV_DATA}"
echo "WAL SV:    ${SV_WAL}"
echo "Config SV: ${SV_CONFIG}"
```

---

## Step 4: Basic Group Snapshot

Create a VolumeGroupSnapshot that coordinates snapshots across all 3 PVCs:

```bash
cat <<EOF | oc apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: app-group-snap-basic
spec:
  poolName: ${POOL_NAME}
  sourcePVCs:
    - ${SV_DATA}
    - ${SV_WAL}
    - ${SV_CONFIG}
  failurePolicy: Abort
EOF
```

**What this does:**
- Creates a coordinated group snapshot of all 3 SubVolumes
- `sourcePVCs` lists the SubVolume names (not PVC names) to include
- `failurePolicy: Abort` means if any individual snapshot fails, the entire operation is rolled back
- The controller snapshots each SubVolume in order and tracks the time window between first and last copy

Watch the phase progression:

```bash
oc get volumegroupsnapshots.storage.ibmcloud.io app-group-snap-basic -w
```

Expected progression: `Pending` -> `InProgress` -> `Complete`

```
NAME                   POOL              MEMBERS   READY   CONSISTENCY     PHASE
app-group-snap-basic   groupsnap-pool    3         3       crash           Complete
```

Inspect the group snapshot details:

```bash
oc describe volumegroupsnapshot.storage.ibmcloud.io app-group-snap-basic
```

Key fields to note:
- `status.phase: Complete` -- all member snapshots succeeded
- `status.memberCount: 3` -- total members in the group
- `status.readyCount: 3` -- all 3 are ready
- `status.failedCount: 0` -- none failed
- `status.consistencyLevel` -- the consistency guarantee achieved (e.g., `crash`)
- `status.inconsistencyWindowMs` -- milliseconds between first and last member snapshot copy

Check the individual member status:

```bash
oc get volumegroupsnapshot.storage.ibmcloud.io app-group-snap-basic \
    -o json | jq '.status.members'
```

Expected output:

```json
[
  {
    "subVolumeName": "sv-app-data-xxxxxxxx",
    "snapshotName": "snap-xxxxxxxx",
    "phase": "Ready",
    "error": ""
  },
  {
    "subVolumeName": "sv-app-wal-xxxxxxxx",
    "snapshotName": "snap-yyyyyyyy",
    "phase": "Ready",
    "error": ""
  },
  {
    "subVolumeName": "sv-app-config-xxxxxxxx",
    "snapshotName": "snap-zzzzzzzz",
    "phase": "Ready",
    "error": ""
  }
]
```

Verify the individual Snapshot CRs were created:

```bash
oc get snapshots.storage.ibmcloud.io -o wide
```

Each member should have a corresponding Snapshot CR with `phase: Ready` and `readyToUse: true`.

> **Consistency level:** NFS directory-level copies are crash-consistent, not application-consistent. The `inconsistencyWindowMs` tells you the time gap between the first and last member copy. For true application consistency, use pre-snapshot hooks to quiesce the application (see Step 5).

---

## Step 5: Group Snapshot with Exec Hooks

Exec hooks run commands inside pods before and after the snapshot operation. This enables application-consistent snapshots by quiescing writes (e.g., flushing buffers, pausing WAL) before the snapshot and resuming afterwards.

Deploy an application pod with a freeze/thaw script:

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: myapp
  namespace: ${TUTORIAL_NS}
  labels:
    app: myapp
spec:
  containers:
    - name: app
      image: busybox
      command:
        - sh
        - -c
        - |
          echo "Application started"
          while true; do
            if [ -f /data/frozen ]; then
              echo "[$(date)] Application is frozen for snapshot"
            fi
            sleep 5
          done
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: app-data
EOF
```

Wait for the pod to start:

```bash
oc get pod myapp -n ${TUTORIAL_NS} -w
# Wait for STATUS=Running
```

Now create a VolumeGroupSnapshot with exec hooks. The pre-snapshot hook creates a `/data/frozen` marker file (simulating an application freeze), and the post-snapshot hook removes it (simulating a thaw):

```bash
cat <<EOF | oc apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: app-group-snap-hooks
spec:
  poolName: ${POOL_NAME}
  sourcePVCs:
    - ${SV_DATA}
    - ${SV_WAL}
    - ${SV_CONFIG}
  failurePolicy: Abort
  preSnapshotHooks:
    - name: freeze-app
      type: exec
      exec:
        podSelector:
          app: myapp
        namespace: ${TUTORIAL_NS}
        command:
          - sh
          - -c
          - "echo 'Freezing application...' && touch /data/frozen && echo 'Frozen.'"
      timeoutSeconds: 30
      onError: Abort
  postSnapshotHooks:
    - name: thaw-app
      type: exec
      exec:
        podSelector:
          app: myapp
        namespace: ${TUTORIAL_NS}
        command:
          - sh
          - -c
          - "echo 'Thawing application...' && rm -f /data/frozen && echo 'Thawed.'"
      timeoutSeconds: 30
      onError: Continue
EOF
```

**What this does:**
- `preSnapshotHooks` -- Before any snapshots are taken, the controller exec's into the first pod matching `app: myapp` in the tutorial namespace and runs the freeze command
- `postSnapshotHooks` -- After all snapshots complete (or fail), the controller runs the thaw command
- `onError: Abort` on the pre-hook means if the freeze fails, the snapshot operation is cancelled
- `onError: Continue` on the post-hook means if the thaw fails, the group snapshot still reports success (the snapshots are valid)

Watch the group snapshot complete:

```bash
oc get volumegroupsnapshots.storage.ibmcloud.io app-group-snap-hooks -w
```

Wait until phase is `Complete`, then inspect the hook results:

```bash
oc get volumegroupsnapshot.storage.ibmcloud.io app-group-snap-hooks \
    -o json | jq '.status.hookResults'
```

Expected output:

```json
[
  {
    "name": "freeze-app",
    "phase": "Pre",
    "success": true,
    "message": "Freezing application...\nFrozen.\n",
    "startedAt": "2026-02-25T10:45:00Z",
    "duration": "1.2s"
  },
  {
    "name": "thaw-app",
    "phase": "Post",
    "success": true,
    "message": "Thawing application...\nThawed.\n",
    "startedAt": "2026-02-25T10:45:05Z",
    "duration": "0.8s"
  }
]
```

**Verify both hooks executed successfully:**
- `freeze-app` with `phase: Pre` and `success: true` -- the application was quiesced before snapshots
- `thaw-app` with `phase: Post` and `success: true` -- the application was resumed after snapshots
- Both show timing information (`startedAt`, `duration`)

Verify the application pod is running normally (no frozen file):

```bash
oc exec myapp -n ${TUTORIAL_NS} -- ls -la /data/frozen 2>&1 || echo "No frozen file -- app is thawed"
```

Expected: "No such file or directory" -- the post-hook successfully removed the frozen marker.

---

## Step 6: Group Snapshot with HTTP Hooks

HTTP hooks call an HTTP endpoint before or after the snapshot operation. This is useful for applications that expose a freeze/thaw API or for integrating with external backup coordinators.

Deploy a minimal HTTP server that logs incoming requests:

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: hook-server
  namespace: ${TUTORIAL_NS}
  labels:
    app: hook-server
spec:
  containers:
    - name: server
      image: python:3.11-slim
      command:
        - python3
        - -c
        - |
          from http.server import HTTPServer, BaseHTTPRequestHandler
          import json, datetime

          class Handler(BaseHTTPRequestHandler):
              def do_POST(self):
                  length = int(self.headers.get('Content-Length', 0))
                  body = self.rfile.read(length) if length > 0 else b''
                  print(f"[{datetime.datetime.now()}] POST {self.path} - {body.decode()}")
                  self.send_response(200)
                  self.send_header('Content-Type', 'application/json')
                  self.end_headers()
                  self.wfile.write(json.dumps({"status": "ok", "path": self.path}).encode())

              def do_GET(self):
                  print(f"[{datetime.datetime.now()}] GET {self.path}")
                  self.send_response(200)
                  self.send_header('Content-Type', 'application/json')
                  self.end_headers()
                  self.wfile.write(json.dumps({"status": "ok", "path": self.path}).encode())

          server = HTTPServer(('0.0.0.0', 8080), Handler)
          print("Hook server listening on :8080")
          server.serve_forever()
      ports:
        - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: hook-server
  namespace: ${TUTORIAL_NS}
spec:
  selector:
    app: hook-server
  ports:
    - port: 8080
      targetPort: 8080
EOF
```

Wait for the server to start:

```bash
oc get pod hook-server -n ${TUTORIAL_NS} -w
# Wait for STATUS=Running
```

Test the server is reachable:

```bash
oc exec myapp -n ${TUTORIAL_NS} -- sh -c \
    'wget -q -O - http://hook-server.pool-tutorial-groupsnap.svc.cluster.local:8080/health'
```

Expected: `{"status": "ok", "path": "/health"}`

Create a VolumeGroupSnapshot with HTTP hooks:

```bash
cat <<EOF | oc apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: app-group-snap-http
spec:
  poolName: ${POOL_NAME}
  sourcePVCs:
    - ${SV_DATA}
    - ${SV_WAL}
    - ${SV_CONFIG}
  failurePolicy: Abort
  preSnapshotHooks:
    - name: notify-pre-snapshot
      type: http
      http:
        url: "http://hook-server.${TUTORIAL_NS}.svc.cluster.local:8080/pre-snapshot"
        method: POST
        headers:
          X-Snapshot-Group: "app-group-snap-http"
          Content-Type: "application/json"
      timeoutSeconds: 15
      onError: Continue
  postSnapshotHooks:
    - name: notify-post-snapshot
      type: http
      http:
        url: "http://hook-server.${TUTORIAL_NS}.svc.cluster.local:8080/post-snapshot"
        method: POST
        headers:
          X-Snapshot-Group: "app-group-snap-http"
          Content-Type: "application/json"
      timeoutSeconds: 15
      onError: Continue
EOF
```

**What this does:**
- The pre-snapshot hook sends a POST request to the hook server's `/pre-snapshot` endpoint
- The post-snapshot hook sends a POST to `/post-snapshot`
- Both hooks use `onError: Continue` -- if the HTTP server is unavailable, the snapshot operation still proceeds
- Custom headers are passed to the server (useful for identifying which snapshot group triggered the hook)

Watch the group snapshot complete:

```bash
oc get volumegroupsnapshots.storage.ibmcloud.io app-group-snap-http -w
# Wait for PHASE=Complete
```

Inspect the hook results:

```bash
oc get volumegroupsnapshot.storage.ibmcloud.io app-group-snap-http \
    -o json | jq '.status.hookResults'
```

Expected: Both hooks show `success: true` with timing information.

Check the hook server logs to confirm it received the requests:

```bash
oc logs hook-server -n ${TUTORIAL_NS}
```

Expected output:

```
Hook server listening on :8080
[2026-02-25 10:50:00.123] POST /pre-snapshot -
[2026-02-25 10:50:02.456] POST /post-snapshot -
```

### onError: Continue vs Abort

The `onError` field controls what happens when a hook fails:

- **`onError: Abort`** (default) -- If the hook fails (timeout, non-2xx response, connection refused), the entire snapshot operation is cancelled. Use this for critical pre-snapshot hooks where proceeding without quiescing would produce inconsistent data.
- **`onError: Continue`** -- If the hook fails, the snapshot operation continues. The failure is recorded in `hookResults` but does not block the snapshots. Use this for non-critical notifications.

---

## Step 7: Failure Policy Demo

The `failurePolicy` field controls what happens when an individual member snapshot fails within a group.

### failurePolicy: Abort

Create a VolumeGroupSnapshot that references a non-existent SubVolume name. With `failurePolicy: Abort`, the operation stops immediately:

```bash
cat <<EOF | oc apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: app-group-snap-abort
spec:
  poolName: ${POOL_NAME}
  sourcePVCs:
    - ${SV_DATA}
    - nonexistent-subvolume
    - ${SV_CONFIG}
  failurePolicy: Abort
EOF
```

Watch the status:

```bash
oc get volumegroupsnapshots.storage.ibmcloud.io app-group-snap-abort -w
```

Expected: Phase goes to `Failed`:

```
NAME                    POOL              MEMBERS   READY   CONSISTENCY   PHASE
app-group-snap-abort    groupsnap-pool    3         0                     Failed
```

Inspect the failure details:

```bash
oc get volumegroupsnapshot.storage.ibmcloud.io app-group-snap-abort \
    -o json | jq '.status.members'
```

Expected: The non-existent member shows `phase: Failed` with an error message, and the entire group is `Failed` because `failurePolicy: Abort` rolled back any completed snapshots.

### failurePolicy: Continue

Now create the same group snapshot with `failurePolicy: Continue`. The operation completes for valid members and marks the group as `PartialFailure`:

```bash
cat <<EOF | oc apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: app-group-snap-continue
spec:
  poolName: ${POOL_NAME}
  sourcePVCs:
    - ${SV_DATA}
    - nonexistent-subvolume
    - ${SV_CONFIG}
  failurePolicy: Continue
EOF
```

Watch the status:

```bash
oc get volumegroupsnapshots.storage.ibmcloud.io app-group-snap-continue -w
```

Expected: Phase reaches `PartialFailure`:

```
NAME                       POOL              MEMBERS   READY   CONSISTENCY   PHASE
app-group-snap-continue    groupsnap-pool    3         2                     PartialFailure
```

Inspect the members:

```bash
oc get volumegroupsnapshot.storage.ibmcloud.io app-group-snap-continue \
    -o json | jq '.status'
```

Expected:
- `memberCount: 3` -- 3 members were attempted
- `readyCount: 2` -- 2 succeeded (app-data and app-config)
- `failedCount: 1` -- 1 failed (nonexistent-subvolume)
- `members[0].phase: Ready` -- the valid SubVolume snapshots completed
- `members[1].phase: Failed` -- the non-existent SubVolume failed with an error
- `members[2].phase: Ready` -- the operation continued past the failure

**When to use each policy:**
- **Abort** -- Critical applications where partial snapshots are useless (e.g., database + WAL must both succeed for a valid restore point)
- **Continue** -- Applications where partial snapshots are acceptable (e.g., snapshotting multiple independent microservices, where some failing is tolerable)

---

## Step 8: Verify Final Pool State

Review the pool to see all allocations:

```bash
oc get filesharepools
```

List all VolumeGroupSnapshots:

```bash
oc get volumegroupsnapshots.storage.ibmcloud.io
```

List all individual Snapshot CRs created by group snapshots:

```bash
oc get snapshots.storage.ibmcloud.io -o wide
```

List all SubVolumes:

```bash
oc get subvolumes -o wide
```

---

## Cleanup

Resources must be deleted in dependency order.

### Full Cleanup

```bash
# 1. Delete all pods and services
oc delete pods --all -n ${TUTORIAL_NS} --force --grace-period=0
oc delete svc --all -n ${TUTORIAL_NS}

# 2. Delete VolumeGroupSnapshots (this triggers deletion of member Snapshot CRs)
oc delete volumegroupsnapshots.storage.ibmcloud.io --all --timeout=60s

# 3. Delete PVCs (CSI cascades SubVolume deletes)
oc delete pvc --all -n ${TUTORIAL_NS} --timeout=60s

# 4. Grace period for CSI to cascade deletes
sleep 5

# 5. Force-clean orphan Snapshots
for snap in $(oc get snapshots.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} -o name 2>/dev/null); do
    oc patch "$snap" --type=merge -p '{"metadata":{"finalizers":null}}'
done
oc delete snapshots.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} --ignore-not-found

# 6. Force-clean orphan SubVolumes
for sv in $(oc get subvolumes.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} -o name 2>/dev/null); do
    oc patch "$sv" --type=merge -p '{"metadata":{"finalizers":null}}'
done
oc delete subvolumes.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} --ignore-not-found

# 7. Delete pool-owned PVs
oc get pv -o json | jq -r "
    .items[]
    | select(.spec.csi.driver == \"vpc-file-pool.csi.ibm.io\")
    | select(.spec.csi.volumeAttributes.pool == \"${POOL_NAME}\")
    | .metadata.name" | xargs -r oc delete pv

# 8. Delete pool (force-remove finalizer if stuck)
oc delete filesharepool ${POOL_NAME} --timeout=60s || {
    oc patch filesharepools.storage.ibmcloud.io ${POOL_NAME} \
        --type=merge -p '{"metadata":{"finalizers":null}}'
    oc delete filesharepool ${POOL_NAME}
}

# 9. Delete namespace
oc delete namespace ${TUTORIAL_NS} --timeout=60s

# 10. Verify VPC shares are being deleted
ibmcloud is shares
```

---

## Quick Reference: Useful Commands

| What | Command |
|------|---------|
| List VolumeGroupSnapshots | `oc get volumegroupsnapshots.storage.ibmcloud.io` |
| Group snapshot details | `oc describe volumegroupsnapshot.storage.ibmcloud.io <name>` |
| Group snapshot members | `oc get vgs <name> -o json \| jq '.status.members'` |
| Group snapshot hooks | `oc get vgs <name> -o json \| jq '.status.hookResults'` |
| List Snapshot CRs | `oc get snapshots.storage.ibmcloud.io -o wide` |
| List SubVolumes | `oc get subvolumes -o wide` |
| Get SubVolume name for PVC | `oc get sv -o json \| jq -r '.items[] \| select(.spec.pvcName=="<pvc>") \| .metadata.name'` |
| Pool capacity | `oc get filesharepools` |
| Controller logs | `oc logs -n kube-system -l app.kubernetes.io/component=controller -c csi-controller --tail=50` |

---

## Key Concepts

### VolumeGroupSnapshot Fields

| Field | Purpose |
|-------|---------|
| `spec.poolName` | The FileSharePool containing source SubVolumes (required) |
| `spec.sourcePVCs` | SubVolume names to include (not PVC names) |
| `spec.copyOrder` | Custom snapshot ordering (default: alphabetical) |
| `spec.failurePolicy` | `Abort` (rollback on failure) or `Continue` (partial success) |
| `spec.preSnapshotHooks` | Hooks to run before snapshots begin |
| `spec.postSnapshotHooks` | Hooks to run after snapshots complete |
| `status.phase` | `Pending` / `InProgress` / `Complete` / `PartialFailure` / `Failed` |
| `status.memberCount` | Total members in the group |
| `status.readyCount` | Members with successful snapshots |
| `status.failedCount` | Members that failed |
| `status.consistencyLevel` | Consistency guarantee achieved |
| `status.inconsistencyWindowMs` | Time gap between first and last snapshot (ms) |
| `status.hookResults` | Outcome of each hook execution |

### Hook Types

| Type | Use Case | Configuration |
|------|----------|---------------|
| `exec` | Run commands in application pods (freeze/thaw, flush buffers) | `podSelector`, `namespace`, `container`, `command` |
| `http` | Call external APIs (backup coordinators, monitoring) | `url`, `method`, `headers` |

### Hook Error Handling

| `onError` | Behavior |
|-----------|----------|
| `Abort` (default) | Hook failure cancels the snapshot operation |
| `Continue` | Hook failure is logged but the snapshot proceeds |

---

## What's Next

Now that you can coordinate multi-PVC snapshots with lifecycle hooks, explore pool configuration options:

- **[Part 4: Pool Configuration](TUTORIAL-04-POOL-CONFIGURATION.md)** -- Multi-tier pools, allocation strategies, auto-expansion, and share draining
- **[Part 5: Golden Images](TUTORIAL-05-GOLDEN-IMAGES.md)** -- Automatic golden image provisioning for KubeVirt
- **[Part 6: Replication & Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md)** -- Cross-region DR with direct NFS and driver-to-driver modes
- **[Part 7: Monitoring](TUTORIAL-07-MONITORING.md)** -- Prometheus metrics, alerts, and Grafana dashboards
- **[Part 8: Migration & Console](TUTORIAL-08-MIGRATION-AND-CONSOLE.md)** -- PVC migration tool and OpenShift console plugin
