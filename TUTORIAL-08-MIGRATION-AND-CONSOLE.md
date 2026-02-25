# Part 8: Migration and Console Plugin

> **Tutorial Series:** [Index](TUTORIAL.md) | [Part 1: Pool Creation](TUTORIAL-01-POOL-CREATION.md) | [Part 2: Snapshots & Clones](TUTORIAL-02-SNAPSHOTS-AND-CLONES.md) | [Part 3: Group Snapshots & Hooks](TUTORIAL-03-GROUP-SNAPSHOTS-AND-HOOKS.md) | [Part 4: Pool Configuration](TUTORIAL-04-POOL-CONFIGURATION.md) | [Part 5: Golden Images](TUTORIAL-05-GOLDEN-IMAGES.md) | [Part 6: Replication & Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md) | [Part 7: Monitoring](TUTORIAL-07-MONITORING.md) | **Part 8**

Migrate PVCs from the stock IBM VPC File CSI driver to the pool-based driver, then enable and explore the OpenShift Console dynamic plugin for visual pool management.

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

For the migration section, the stock IBM VPC File CSI driver should also be installed (standard on ROKS clusters):

```bash
oc get csidriver vpc.file.csi.ibm.io
```

For the console plugin section, you need OpenShift 4.14+ with the ConsolePlugin v1 API:

```bash
oc api-resources | grep consoleplugins
```

---

## Step 1: Set Up Variables and Namespace

```bash
export POOL_NAME=ops-pool
export TUTORIAL_NS=pool-tutorial-ops

oc create namespace ${TUTORIAL_NS}
```

---

## Step 2: Create a Pool

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
  maxShares: 3
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultPermissions: "0777"
  defaultUID: 107
  defaultGID: 107
EOF
```

Wait for the pool to reach `Ready`:

```bash
oc get filesharepools ${POOL_NAME} -w
```

Verify the auto-created StorageClass:

```bash
oc get sc ${POOL_NAME}
```

---

## Step 3: Install kubectl-migrate

The migration CLI is a kubectl plugin that discovers PVCs on the stock driver and copies their data to pool-backed PVCs.

### Build from Source

```bash
cd /Users/neiltaylor/Projects/roks_new_file_csi
go build -o kubectl-migrate ./cmd/migrate/
```

### Install to PATH

```bash
sudo cp kubectl-migrate /usr/local/bin/
chmod +x /usr/local/bin/kubectl-migrate
```

### Verify

```bash
kubectl migrate version
```

Expected output:

```
kubectl-migrate dev
```

---

## Step 4: Create a Test PVC on the Stock Driver

To demonstrate migration, create a PVC using the stock IBM VPC File CSI driver's StorageClass and write test data to it:

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: legacy-app-data
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ibm-vpc-block-5iops-tier
  resources:
    requests:
      storage: 10Gi
---
apiVersion: v1
kind: Pod
metadata:
  name: legacy-writer
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
          echo "Important application data - migration test" > /data/app-config.txt
          echo "Database export 2024-01-15" > /data/db-export.sql
          dd if=/dev/urandom of=/data/binary-blob.bin bs=1M count=5
          echo "Migration source verification checksum:"
          md5sum /data/*.txt /data/*.sql /data/*.bin
          ls -la /data/
          sleep 3600
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: legacy-app-data
EOF
```

**What this does:**
- Creates a PVC on the stock IBM VPC File CSI driver
- Writes several test files (text config, SQL export, binary blob)
- Prints checksums for verification after migration

Wait for the pod to be running, then capture the checksums:

```bash
oc get pod legacy-writer -n ${TUTORIAL_NS} -w
# Wait until Running, then:
oc logs legacy-writer -n ${TUTORIAL_NS}
```

Note the checksums from the output -- you will compare these after migration to verify data integrity.

Stop the writer pod (migration requires the PVC to not be actively mounted by running pods):

```bash
oc delete pod legacy-writer -n ${TUTORIAL_NS}
```

---

## Step 5: Migration Plan

Discover which PVCs are eligible for migration:

```bash
kubectl migrate plan \
  --namespace ${TUTORIAL_NS} \
  --storage-class ibm-vpc-block-5iops-tier \
  --target-pool ${POOL_NAME}
```

Expected output:

```
Migration Plan
==============
Namespace:          pool-tutorial-ops
Source StorageClass: ibm-vpc-block-5iops-tier
Target Pool:        ops-pool
PVCs found:         1
Total size:         10 GiB

NAME              SIZE (GiB)  PHASE   SHARE ID        PODS
legacy-app-data   10          Bound   r010-xxxxxxxx   (none)

To migrate a PVC, run:
  kubectl migrate execute --namespace pool-tutorial-ops --pvc <PVC_NAME> --target-pool ops-pool --target-storage-class <POOL_SC>
```

**What this does:**
- Scans the namespace for PVCs using the specified StorageClass
- Shows each PVC's size, phase, backing share ID, and any pods currently mounting it
- The `PODS` column shows `(none)` because we deleted the writer pod -- migration requires no active mounts

---

## Step 6: Migration Dry Run

Preview what the migration will do without making any changes:

```bash
kubectl migrate execute \
  --namespace ${TUTORIAL_NS} \
  --pvc legacy-app-data \
  --target-pool ${POOL_NAME} \
  --target-storage-class ${POOL_NAME} \
  --dry-run
```

**What this does:**
- Validates the source PVC exists and is not actively mounted
- Validates the target pool exists and has capacity
- Shows the migration plan without creating any resources

---

## Step 7: Migration Execute

Run the actual migration:

```bash
kubectl migrate execute \
  --namespace ${TUTORIAL_NS} \
  --pvc legacy-app-data \
  --target-pool ${POOL_NAME} \
  --target-storage-class ${POOL_NAME}
```

**What this does:**
- Creates a new PVC (`legacy-app-data-pool`) on the pool driver
- Spawns a temporary migration pod that mounts both the source and target PVCs
- Uses rsync to copy all data from the source PVC to the target PVC
- Reports success when the copy completes

Watch the migration pod progress:

```bash
kubectl migrate status --namespace ${TUTORIAL_NS}
```

Expected output during migration:

```
POD                              SOURCE PVC       TARGET PVC            PHASE      STARTED
migrate-legacy-app-data-xxxxx    legacy-app-data  legacy-app-data-pool  Running    2026-02-25 10:30:15
```

Wait for the phase to show `Succeeded`:

```bash
# Poll until complete
while true; do
    kubectl migrate status --namespace ${TUTORIAL_NS} 2>/dev/null | grep -q Succeeded && break
    sleep 5
done
kubectl migrate status --namespace ${TUTORIAL_NS}
```

Expected output on completion:

```
POD                              SOURCE PVC       TARGET PVC            PHASE       STARTED
migrate-legacy-app-data-xxxxx    legacy-app-data  legacy-app-data-pool  Succeeded   2026-02-25 10:30:15

SUCCESS: PVC legacy-app-data migrated to legacy-app-data-pool on pool ops-pool
```

### Verify Data Integrity

Mount the new pool-backed PVC and compare checksums:

```bash
cat <<EOF | oc apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: migration-verifier
  namespace: ${TUTORIAL_NS}
spec:
  restartPolicy: Never
  containers:
    - name: verifier
      image: busybox
      command:
        - sh
        - -c
        - |
          echo "Migrated data verification checksums:"
          md5sum /data/*.txt /data/*.sql /data/*.bin
          echo ""
          echo "File listing:"
          ls -la /data/
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: legacy-app-data-pool
EOF
```

Wait for the verifier to complete, then compare:

```bash
oc get pod migration-verifier -n ${TUTORIAL_NS} -w
# Wait for Completed, then:
oc logs migration-verifier -n ${TUTORIAL_NS}
```

The checksums should match the ones from Step 4. All files should be present with identical sizes.

Verify the new PVC is on the pool:

```bash
# New PVC is on the pool StorageClass
oc get pvc legacy-app-data-pool -n ${TUTORIAL_NS}

# SubVolume exists
oc get subvolumes -o wide | grep legacy-app-data-pool

# Pool shows the allocation
oc get filesharepools ${POOL_NAME}
```

Clean up the migration pod and verifier:

```bash
oc delete pod migration-verifier -n ${TUTORIAL_NS}
```

> **Production note:** After verifying the migrated data, update your application's PVC reference from `legacy-app-data` to `legacy-app-data-pool`, then delete the original PVC to free the stock VPC file share.

---

## Step 8: Console Plugin -- Enable

The OpenShift Console dynamic plugin provides a visual interface for managing pools, SubVolumes, snapshots, group snapshots, and replication policies.

### Enable via Helm

```bash
helm upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi \
  -n kube-system \
  --reuse-values \
  --set consolePlugin.enabled=true \
  --set consolePlugin.replicas=2
```

**What this does:**
- Deploys the console plugin as a 2-replica Deployment in `kube-system`
- Creates a Service on port 9443 with TLS certificates auto-provisioned by the OpenShift service-ca operator
- Registers a ConsolePlugin CR with the OpenShift Console

### Register the Plugin

Enable the plugin in the OpenShift Console operator:

```bash
oc patch console.operator.openshift.io cluster \
  --patch '{"spec":{"plugins":["ibm-vpc-file-pool-csi"]}}' \
  --type=merge
```

### Verify

```bash
# ConsolePlugin CR exists
oc get consoleplugin ibm-vpc-file-pool-csi

# Plugin pods are running
oc get pods -n kube-system -l app.kubernetes.io/component=console-plugin

# Plugin is registered with the console
oc get console.operator.openshift.io cluster -o jsonpath='{.spec.plugins}'
```

Expected output:

```
["ibm-vpc-file-pool-csi"]
```

After a few seconds, refresh the OpenShift Console. A new "VPC File Pools" navigation group appears in the left sidebar.

---

## Step 9: Console Plugin -- Tour

The console plugin adds the following pages to the OpenShift Console. Each page is accessible from the "VPC File Pools" navigation group.

### Dashboard

The landing page shows a high-level overview of all pools:

- **Stat cards** at the top: Total Pools (with health indicator), Total Capacity (allocated vs total), Total SubVolumes, and Active Alerts (pools not in Ready state)
- **Performance metrics row**: Allocation rate (per hour), P95 allocation latency (color-coded: green < 100ms, yellow < 500ms, red > 500ms), and VPC API health (calls/min, error rate, P95 latency)
- **Capacity breakdown**: Aggregate capacity donut chart on the left, per-pool capacity bars on the right with share count indicators
- **Replication status** (conditional -- only shown when ReplicationPolicy CRs exist): Replication lag per policy (color-coded), sync rate, and consecutive failures
- **Operations activity**: Snapshot count, clone count with average duration, and group snapshot count
- **Recent activity table**: The 10 most recently created SubVolumes with name, pool, PVC, size, phase, and age

### Pools (FileSharePool)

- **List page**: All FileSharePools with capacity bars, share counts, phase status, zone, and profile. Click any pool name to view details.
- **Create page**: A step-by-step wizard with preset configurations (Standard Workloads, KubeVirt VMs, High Performance). Configure zone, profile, share size, IOPS, max shares, allocation strategy, auto-expand settings, and default permissions. Includes a YAML editor fallback for advanced users.
- **Details page**: Full pool specification, status summary, list of shares with IDs and mount target IPs, capacity bar, and associated SubVolumes table.

### SubVolumes

- **List page**: Filterable list of all SubVolumes with columns for name, pool, PVC namespace/name, size, share ID, and phase. Filter by pool name or phase.
- **Create page**: Create a SubVolume manually (rarely needed -- PVCs create them automatically).
- **Details page**: SubVolume spec (pool, PVC reference, requested GB, share ID, sub-path), status (phase, clone status), and conditions table.

### Snapshots

- **List page**: All Snapshot CRs with pool, source SubVolume, creation time, size, and phase.
- **Create page**: Select a SubVolume to snapshot, provide a name, and create. Validates that the source SubVolume exists and is in a healthy state.
- **Details page**: Snapshot metadata, source SubVolume reference, creation timestamp, and restore action.

### Group Snapshots (VolumeGroupSnapshot)

- **List page**: All VolumeGroupSnapshot CRs with member count, phase, and creation time.
- **Create page**: Multi-step wizard to select PVCs for coordinated snapshots. Optionally configure lifecycle hooks (exec commands or HTTP webhooks) for pre-snapshot quiescing and post-snapshot actions.
- **Details page**: Member list with individual snapshot status, inconsistency window, hook execution results, and conditions table.

### Replication Policies

- **List page**: All ReplicationPolicy CRs with source pool, destination, mode (direct-nfs or driver-to-driver), schedule, lag indicator (color-coded), and phase.
- **Create page**: Wizard for configuring replication mode, source and destination pools, schedule (cron expression), parallel sync count, and hook configuration. Validates destination connectivity.
- **Details page**: Policy spec, current lag (gauge), last sync time, consecutive failure count, replicated SubVolume list, and conditions table.

### Monitoring

- **Time range selector** at the top (1h, 6h, 24h, 7d)
- **Capacity overview**: Donut chart and per-pool capacity bars with PVC and share counts
- **Stat cards**: Total PVCs, utilization percentage (color-coded), VPC API health, and replication status
- **Time series charts**: Capacity utilization over time, PVC count over time, VPC API call rate (by status), VPC API P95 latency
- **Allocation activity** (expandable section): Allocation rate over time and P95 allocation latency over time
- **Replication charts** (conditional): Replication lag and sync rate over time
- **Prometheus connection diagnostic**: Shows a warning banner if Prometheus is unreachable or if no CSI driver metrics are found, with instructions to enable the ServiceMonitor

---

## Cleanup

```bash
POOL_NAME=ops-pool
TUTORIAL_NS=pool-tutorial-ops

# 1. Delete pods and PVCs
oc delete pods --all -n ${TUTORIAL_NS} --force --grace-period=0 --ignore-not-found
oc delete pvc --all -n ${TUTORIAL_NS} --timeout=60s

# 2. Grace period for CSI to cascade SubVolume deletes
sleep 5

# 3. Force-clean orphan SubVolumes
for sv in $(oc get subvolumes.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} -o name 2>/dev/null); do
    oc patch "$sv" --type=merge -p '{"metadata":{"finalizers":null}}'
done
oc delete subvolumes.storage.ibmcloud.io \
    -l storage.ibmcloud.io/pool=${POOL_NAME} --ignore-not-found

# 4. Delete pool (force-remove finalizer if stuck)
oc delete filesharepool ${POOL_NAME} --timeout=60s || {
    oc patch filesharepools.storage.ibmcloud.io ${POOL_NAME} \
        --type=merge -p '{"metadata":{"finalizers":null}}'
    oc delete filesharepool ${POOL_NAME}
}

# 5. Delete namespace
oc delete namespace ${TUTORIAL_NS} --timeout=60s

# 6. Verify VPC shares are being deleted
ibmcloud is shares
```

> **Note:** The console plugin remains enabled after cleanup. To disable it:
> ```bash
> oc patch console.operator.openshift.io cluster \
>   --patch '{"spec":{"plugins":[]}}' \
>   --type=merge
> helm upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi \
>   -n kube-system \
>   --reuse-values \
>   --set consolePlugin.enabled=false
> ```

---

## Quick Reference

| What | Command |
|------|---------|
| Build migrate CLI | `go build -o kubectl-migrate ./cmd/migrate/` |
| Migration plan | `kubectl migrate plan --namespace <ns> --storage-class <sc> --target-pool <pool>` |
| Migrate PVC (dry run) | `kubectl migrate execute --namespace <ns> --pvc <name> --target-pool <pool> --target-storage-class <sc> --dry-run` |
| Migrate PVC | `kubectl migrate execute --namespace <ns> --pvc <name> --target-pool <pool> --target-storage-class <sc>` |
| Migration status | `kubectl migrate status --namespace <ns>` |
| Enable console plugin | `helm upgrade ... --set consolePlugin.enabled=true` |
| Register plugin | `oc patch console.operator.openshift.io cluster --patch '{"spec":{"plugins":["ibm-vpc-file-pool-csi"]}}' --type=merge` |
| Check plugin status | `oc get consoleplugin ibm-vpc-file-pool-csi` |
| Plugin pods | `oc get pods -n kube-system -l app.kubernetes.io/component=console-plugin` |
| Disable plugin | `oc patch console.operator.openshift.io cluster --patch '{"spec":{"plugins":[]}}' --type=merge` |

---

## Helm Values Reference (Console Plugin)

```yaml
consolePlugin:
  enabled: false                    # Enable the console plugin
  image:
    repository: de.icr.io/ibm-vpc-file-pool-csi/console-plugin
    tag: latest
  replicas: 2                       # Plugin pod replicas
  port: 9443                        # HTTPS port (TLS auto-provisioned by service-ca)
  resources:
    requests:
      cpu: 10m
      memory: 50Mi
    limits:
      memory: 128Mi
```

---

## Series Summary

This concludes the 8-part tutorial series for the IBM VPC File Pool CSI Driver. Here is a summary of what each part covers:

| Part | Tutorial | What You Learned |
|------|----------|------------------|
| 1 | [Pool Creation, PVCs, and KubeVirt VMs](TUTORIAL-01-POOL-CREATION.md) | Created a pool, provisioned PVCs, launched a VM with NFS-backed disks |
| 2 | [Snapshots and Clones](TUTORIAL-02-SNAPSHOTS-AND-CLONES.md) | Point-in-time snapshots, restore from snapshot, sync and async clones |
| 3 | [Group Snapshots and Hooks](TUTORIAL-03-GROUP-SNAPSHOTS-AND-HOOKS.md) | Coordinated multi-PVC snapshots with exec and HTTP lifecycle hooks |
| 4 | [Pool Configuration](TUTORIAL-04-POOL-CONFIGURATION.md) | Multi-tier pools, allocation strategies, auto-expansion, share draining |
| 5 | [Golden Images](TUTORIAL-05-GOLDEN-IMAGES.md) | Automatic KubeVirt golden image provisioning from CDI and OCI sources |
| 6 | [Replication and Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md) | Cross-region DR with direct NFS and driver-to-driver modes, failover |
| 7 | [Monitoring](TUTORIAL-07-MONITORING.md) | Prometheus metrics, ServiceMonitor, alerting rules, Grafana dashboard |
| 8 | [Migration and Console Plugin](TUTORIAL-08-MIGRATION-AND-CONSOLE.md) | PVC migration from stock driver, OpenShift console plugin walkthrough |

For production operations, see:
- [Day 2 Operations](DAY2-OPERATIONS.md) -- Health checks, draining, failover procedures
- [Troubleshooting](TROUBLESHOOTING.md) -- Common issues and solutions
- [Monitoring](MONITORING.md) -- Full metrics reference and PromQL cookbook
- [Capacity Planning](CAPACITY-PLANNING.md) -- Sizing guidance and cost estimation
