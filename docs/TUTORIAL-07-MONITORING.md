# Part 7: Monitoring

> **Tutorial Series:** [Index](TUTORIAL.md) | [Part 1: Pool Creation](TUTORIAL-01-POOL-CREATION.md) | [Part 2: Snapshots & Clones](TUTORIAL-02-SNAPSHOTS-AND-CLONES.md) | [Part 3: Group Snapshots & Hooks](TUTORIAL-03-GROUP-SNAPSHOTS-AND-HOOKS.md) | [Part 4: Pool Configuration](TUTORIAL-04-POOL-CONFIGURATION.md) | [Part 5: Golden Images](TUTORIAL-05-GOLDEN-IMAGES.md) | [Part 6: Replication & Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md) | **Part 7** | [Part 8: Migration & Console](TUTORIAL-08-MIGRATION-AND-CONSOLE.md) | [Part 9: COS Backup Bucket](TUTORIAL-09-COS-BACKUP-BUCKET.md)

Set up Prometheus monitoring for the CSI driver: enable metric scraping, explore the main metrics, deploy alerting rules, trigger a test alert, and import a Grafana dashboard.

**Cluster:** ROKS eu-de (OpenShift Virtualization enabled)
**Zone:** eu-de-1

---

## Prerequisites

Verify the CSI driver is running:

```bash
# Controller pod (6/6 Running)
oc get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=controller

# Node pods (3/3 Running on each schedulable node)
oc get pods -n kube-system -l app.kubernetes.io/name=ibm-vpc-file-pool-csi,app.kubernetes.io/component=node

# CSI Driver registered
oc get csidriver vpc-file-pool.csi.ibm.io
```

Verify the Prometheus Operator is installed (standard on OpenShift):

```bash
# Should show the prometheus-operator pod Running
oc get pods -n openshift-monitoring -l app.kubernetes.io/name=prometheus-operator
```

---

## Step 1: Set Up Variables and Namespace

```bash
export POOL_NAME=monitored-pool
export TUTORIAL_NS=pool-tutorial-monitoring

oc create namespace ${TUTORIAL_NS}
```

---

## Step 2: Create a Pool and Provision PVCs

Create a pool to generate metrics:

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
  autoExpand: false
  allocationStrategy: spread
  defaultPermissions: "0777"
  defaultUID: 107
  defaultGID: 107
EOF
```

`autoExpand` is off so that the pool stays at 100 GB. In Step 5 you fill it past 85%. With `autoExpand: true` the pool would add a second share at 80% and the alert would never fire.

Wait for the pool to reach `Ready`:

```bash
oc get filesharepools ${POOL_NAME} -w
```

Provision several PVCs to generate allocation metrics:

```bash
for i in 1 2 3; do
cat <<EOF | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: monitoring-test-${i}
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 10Gi
EOF
done
```

Verify all PVCs are bound:

```bash
oc get pvc -n ${TUTORIAL_NS}
```

Expected output:

```
NAME                 STATUS   VOLUME           CAPACITY   ACCESS MODES   STORAGECLASS     AGE
monitoring-test-1    Bound    pvc-xxxxxxxx     10Gi       RWX            monitored-pool   10s
monitoring-test-2    Bound    pvc-yyyyyyyy     10Gi       RWX            monitored-pool   10s
monitoring-test-3    Bound    pvc-zzzzzzzz     10Gi       RWX            monitored-pool   10s
```

Verify pool capacity:

```bash
oc get filesharepools ${POOL_NAME}
# ALLOCATED should show 30, PVCS should show 3
```

---

## Step 3: Enable ServiceMonitor

The controller serves Prometheus metrics on `:8080/metrics`. A ServiceMonitor tells Prometheus where to scrape.

### Method A: Helm Values (Recommended)

The chart creates the ServiceMonitors by default (`metrics.serviceMonitor.enabled` is `true`). It does so only when the cluster has the Prometheus Operator API, which OpenShift has. On OpenShift the chart puts them in its own namespace, `ibm-vpc-file-pool-csi-monitoring` (`metrics.monitoring.namespace`), and OpenShift's user workload monitoring must be on. See [MONITORING.md](MONITORING.md). Check that they exist:

```bash
oc get servicemonitors -n ibm-vpc-file-pool-csi-monitoring
```

If you turned it off, enable it again:

```bash
helm upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi \
  -n kube-system \
  --reuse-values \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.serviceMonitor.interval=30s
```

**What this does:**
- Creates the ServiceMonitors for the controller and the node agents
- Tells Prometheus to scrape the controller's `:8080/metrics` endpoint every 30 seconds

### Method B: Without Helm

The controller serves its metrics over HTTPS and wants a bearer token that may `get` `/metrics`, so a plain ServiceMonitor with no TLS or token settings does not work. Copy the settings from the chart's `servicemonitor.yaml`, or see [MONITORING.md](MONITORING.md).

### Verify Scraping

Port-forward to the controller pod and check raw metrics:

```bash
oc port-forward -n kube-system deploy/ibm-vpc-file-pool-csi-controller 8080:8080 &
PF_PID=$!
sleep 2

# The endpoint is HTTPS and needs a token that may "get" /metrics.
# A plain http:// call fails with "Client sent an HTTP request to an HTTPS server".
TOKEN=$(oc create token prometheus-k8s -n openshift-monitoring --duration=10m)
curl -sk -H "Authorization: Bearer ${TOKEN}" https://localhost:8080/metrics | grep vpc_file_pool | head -20

kill $PF_PID
```

Expected output (after allocating 3 PVCs; the `allocated_gb` and `pvc_count` gauges can lag a minute behind):

```
# HELP vpc_file_pool_allocations_total Total number of subvolume allocations
# TYPE vpc_file_pool_allocations_total counter
vpc_file_pool_allocations_total{pool="monitored-pool",status="success"} 3
# HELP vpc_file_pool_allocated_gb Total allocated capacity in GB
# TYPE vpc_file_pool_allocated_gb gauge
vpc_file_pool_allocated_gb{pool="monitored-pool"} 30
# HELP vpc_file_pool_capacity_gb Total pool capacity in GB
# TYPE vpc_file_pool_capacity_gb gauge
vpc_file_pool_capacity_gb{pool="monitored-pool"} 100
# HELP vpc_file_pool_pvc_count Number of PVCs in the pool
# TYPE vpc_file_pool_pvc_count gauge
vpc_file_pool_pvc_count{pool="monitored-pool"} 3
# HELP vpc_file_pool_share_count Number of VPC file shares in the pool
# TYPE vpc_file_pool_share_count gauge
vpc_file_pool_share_count{pool="monitored-pool"} 1
```

---

## Step 4: Key Metrics Walkthrough

The driver exposes 31 metrics across eight categories. This section walks through the first six categories (24 metrics) with PromQL queries and explains what to look for. The backup and CDI metrics are in [MONITORING.md](MONITORING.md).

### Allocation Metrics (9 metrics)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_allocations_total` | Counter | `pool`, `status` | Total allocations. `status`=success or error |
| `vpc_file_pool_allocation_duration_seconds` | Histogram | `pool` | Time to allocate. Buckets: 10ms-163s |
| `vpc_file_pool_capacity_gb` | Gauge | `pool` | Total pool capacity in GB |
| `vpc_file_pool_allocated_gb` | Gauge | `pool` | Total allocated capacity in GB |
| `vpc_file_pool_share_count` | Gauge | `pool` | Number of VPC shares in the pool |
| `vpc_file_pool_max_shares` | Gauge | `pool` | Most shares the pool may have (`spec.maxShares`) |
| `vpc_file_pool_share_total_gb` | Gauge | `pool`, `share` | Provisioned size of one share in GB |
| `vpc_file_pool_share_allocated_gb` | Gauge | `pool`, `share` | Capacity allocated to PVCs on one share in GB |
| `vpc_file_pool_pvc_count` | Gauge | `pool` | Number of active PVCs |

**Key PromQL queries:**

```promql
# Allocation success rate (per minute over 5m window)
rate(vpc_file_pool_allocations_total{status="success"}[5m]) * 60

# Pool utilization percentage
vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb * 100

# Free capacity per pool (GB)
vpc_file_pool_capacity_gb - vpc_file_pool_allocated_gb

# P99 allocation latency
histogram_quantile(0.99, rate(vpc_file_pool_allocation_duration_seconds_bucket[5m]))
```

**What to look for:**
- `allocations_total{status="error"}` should be 0 in steady state. Any errors indicate pool issues.
- `allocated_gb / capacity_gb > 0.85` means the pool is nearing capacity. Verify `autoExpand` is enabled.
- P99 allocation latency should be under 1 second. Values above 5 seconds indicate the pool is expanding or degraded.

### VPC API Metrics (2 metrics)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_api_calls_total` | Counter | `operation`, `status` | API calls by operation (create_share, get_share, etc.) |
| `vpc_file_pool_api_call_duration_seconds` | Histogram | `operation` | API call duration |

**Key PromQL queries:**

```promql
# API error rate (percentage over 5m)
rate(vpc_file_pool_api_calls_total{status="error"}[5m])
/ rate(vpc_file_pool_api_calls_total[5m]) * 100

# P95 API latency by operation
histogram_quantile(0.95, rate(vpc_file_pool_api_call_duration_seconds_bucket[5m]))
```

**What to look for:**
- Sustained error rates above 5% indicate VPC API issues (check API key validity, VPC quotas, service status).
- `create_share` calls take 30-90 seconds (normal). `get_share` should be under 5 seconds.

### Snapshot Metrics (2 metrics)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_snapshots_total` | Counter | `pool`, `operation`, `status` | operation=create, delete, restore |
| `vpc_file_pool_snapshot_duration_seconds` | Histogram | `pool`, `operation` | Snapshot operation duration |

### Clone Metrics (2 metrics)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_clones_total` | Counter | `pool`, `status` | Clone operations by outcome |
| `vpc_file_pool_clone_duration_seconds` | Histogram | `pool` | Time from the clone worker picking up a clone to the end of its copy Job |

### Group Snapshot Metrics (4 metrics)

> **Note:** VolumeGroupSnapshot is not active. Nothing acts on it yet (issue #100), so these four metrics stay empty.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_group_snapshots_total` | Counter | `pool`, `operation`, `status` | operation=create, delete |
| `vpc_file_pool_group_snapshot_duration_seconds` | Histogram | `pool`, `operation` | Total group snapshot duration |
| `vpc_file_pool_group_snapshot_member_count` | Histogram | `pool` | Members per group snapshot |
| `vpc_file_pool_group_snapshot_inconsistency_window_ms` | Histogram | `pool` | Time between first/last member copy |

### Replication Metrics (5 metrics)

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `pool_csi_replication_sync_total` | Counter | `policy`, `source_pool`, `result` | result=success, error, skipped |
| `pool_csi_replication_sync_duration_seconds` | Histogram | `policy`, `source_pool` | Sync cycle duration |
| `pool_csi_replication_lag_seconds` | Gauge | `policy`, `source_pool` | Current replication lag (RPO indicator) |
| `pool_csi_replication_consecutive_failures` | Gauge | `policy`, `source_pool` | Consecutive failure count |
| `pool_csi_replication_subvolume_count` | Gauge | `policy`, `source_pool` | SubVolumes per policy |

**Key PromQL queries:**

```promql
# Replication lag (RPO indicator) — should be less than 2x schedule interval
pool_csi_replication_lag_seconds

# Replication sync success rate
rate(pool_csi_replication_sync_total{result="success"}[1h])
/ rate(pool_csi_replication_sync_total[1h])

# Consecutive failures — any value above 0 needs investigation
pool_csi_replication_consecutive_failures
```

**What to look for:**
- `lag_seconds > 7200` (2 hours) means RPO may be violated.
- `consecutive_failures > 3` means the policy is likely paused. Check connectivity to the destination NFS mount or receiver endpoint.
- Sync duration should be less than the schedule interval. If syncs overlap, the data volume is too large for the configured interval.

---

## Step 5: Alerting Rules

The chart already ships a PrometheusRule named `ibm-vpc-file-pool-csi` (`metrics.alerts.enabled` is `true` by default). Its alerts are named `VPCFilePool...`, for example `VPCFilePoolUtilizationWarning`. See [MONITORING.md](MONITORING.md) for the full list. Check it:

```bash
oc get prometheusrules -n ibm-vpc-file-pool-csi-monitoring
```

The rules below are examples to show how an alert is written. If the chart rules are active, you do not need them, and applying both sends two alerts for one problem. Apply them only for learning, and delete them in Cleanup. They go in the chart's monitoring namespace: user workload monitoring limits a rule to the metrics of its own namespace, and the driver's metrics are in that one.

### Critical Alerts

```bash
cat <<EOF | oc apply -f -
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: vpc-file-pool-csi-critical
  namespace: ibm-vpc-file-pool-csi-monitoring
  labels:
    app: vpc-file-pool-csi
spec:
  groups:
    - name: vpc-file-pool-csi.critical
      rules:
        - alert: PoolAtMaxCapacity
          expr: |
            (vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb) > 0.95
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "Pool {{ \$labels.pool }} is over 95% allocated"
            description: "Pool has less than 5% capacity remaining. New PVC requests will fail if the pool cannot expand."

        - alert: AllocationFailureRate
          expr: rate(vpc_file_pool_allocations_total{status="error"}[5m]) > 0.1
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "High allocation failure rate in pool {{ \$labels.pool }}"
            description: "More than 0.1 allocation failures per second sustained for 5 minutes."

        - alert: ControllerDown
          expr: absent(up{job="ibm-vpc-file-pool-csi-metrics"} == 1)
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "VPC File Pool CSI controller is down"
            description: "No metrics received from the controller for 5 minutes. PVC operations are blocked."

        - alert: ReplicationPaused
          expr: pool_csi_replication_consecutive_failures > 3
          for: 10m
          labels:
            severity: critical
          annotations:
            summary: "Replication policy {{ \$labels.policy }} is paused due to failures"
            description: "Replication has {{ \$value }} consecutive failures. DR data is stale."
EOF
```

**What this does:**
- Defines four critical alerts that fire when pool capacity, allocation health, controller availability, or replication health is compromised
- Uses the `for` clause to avoid transient spikes triggering alerts

### Warning Alerts

```bash
cat <<EOF | oc apply -f -
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: vpc-file-pool-csi-warning
  namespace: ibm-vpc-file-pool-csi-monitoring
  labels:
    app: vpc-file-pool-csi
spec:
  groups:
    - name: vpc-file-pool-csi.warning
      rules:
        - alert: PoolNearCapacity
          expr: (vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb) > 0.85
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "Pool {{ \$labels.pool }} is over 85% allocated"
            description: "Pool is nearing capacity. Verify autoExpand is enabled and maxShares has room."

        - alert: ReplicationLagHigh
          expr: pool_csi_replication_lag_seconds > 7200
          for: 30m
          labels:
            severity: warning
          annotations:
            summary: "Replication lag exceeds 2 hours for policy {{ \$labels.policy }}"
            description: "DR data is stale. RPO may be violated."
EOF
```

Verify the rules were created:

```bash
oc get prometheusrules -n ibm-vpc-file-pool-csi-monitoring -l app=vpc-file-pool-csi
```

Expected output:

```
NAME                            AGE
vpc-file-pool-csi-critical      10s
vpc-file-pool-csi-warning       5s
```

### Trigger a Test Alert

Allocate more PVCs until the pool exceeds 85% utilization to trigger the `PoolNearCapacity` warning.

The pool has 100 GB total and currently 30 GB allocated (3 PVCs at 10 GB each). To exceed 85%, allocate 60 more GB:

```bash
for i in 4 5 6 7 8 9; do
cat <<EOF | oc apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: monitoring-test-${i}
  namespace: ${TUTORIAL_NS}
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ${POOL_NAME}
  resources:
    requests:
      storage: 10Gi
EOF
done
```

Verify the pool utilization:

```bash
oc get filesharepools ${POOL_NAME}
# ALLOCATED should show 90, which is 90% of 100 GB (SHARES stays 1)
```

After the `for: 10m` hold-off period, the `PoolNearCapacity` alert fires. The chart's `VPCFilePoolUtilizationWarning` alert (default threshold 80%, hold-off 5 minutes) fires too. Check in the OpenShift Console under Observe > Alerting, or query Prometheus directly:

```bash
# Port-forward to Prometheus (OpenShift thanos-querier: HTTPS on 9091, needs a token)
oc port-forward -n openshift-monitoring svc/thanos-querier 9091:9091 &
PQ_PID=$!
sleep 2

TOKEN=$(oc create token prometheus-k8s -n openshift-monitoring --duration=10m)
curl -sk -H "Authorization: Bearer ${TOKEN}" 'https://localhost:9091/api/v1/rules' | \
  python3 -c "import sys,json; rules=json.load(sys.stdin); [print(r['name'],r.get('state','')) for g in rules['data']['groups'] for r in g['rules'] if 'Pool' in r['name']]"

kill $PQ_PID
```

---

## Step 6: Grafana Dashboard

Import a pre-built dashboard for a visual overview of pool health.

### Import Instructions

1. Open Grafana (OpenShift Console > Observe > Dashboards, or your standalone Grafana instance)
2. Go to Dashboards > Import
3. Paste the JSON below and click Load
4. Select your Prometheus data source

### Dashboard JSON

```json
{
  "dashboard": {
    "title": "IBM VPC File Pool CSI Driver",
    "uid": "vpc-file-pool-csi",
    "tags": ["storage", "csi", "vpc"],
    "timezone": "browser",
    "refresh": "30s",
    "panels": [
      {
        "title": "Pool Utilization",
        "type": "gauge",
        "gridPos": {"h": 8, "w": 6, "x": 0, "y": 0},
        "targets": [
          {
            "expr": "vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb * 100",
            "legendFormat": "{{ pool }}"
          }
        ],
        "fieldConfig": {
          "defaults": {
            "unit": "percent",
            "min": 0,
            "max": 100,
            "thresholds": {
              "steps": [
                {"color": "green", "value": 0},
                {"color": "yellow", "value": 70},
                {"color": "orange", "value": 85},
                {"color": "red", "value": 95}
              ]
            }
          }
        }
      },
      {
        "title": "PVC Count by Pool",
        "type": "stat",
        "gridPos": {"h": 8, "w": 6, "x": 6, "y": 0},
        "targets": [
          {
            "expr": "vpc_file_pool_pvc_count",
            "legendFormat": "{{ pool }}"
          }
        ]
      },
      {
        "title": "Share Count by Pool",
        "type": "stat",
        "gridPos": {"h": 8, "w": 6, "x": 12, "y": 0},
        "targets": [
          {
            "expr": "vpc_file_pool_share_count",
            "legendFormat": "{{ pool }}"
          }
        ]
      },
      {
        "title": "Free Capacity (GB)",
        "type": "stat",
        "gridPos": {"h": 8, "w": 6, "x": 18, "y": 0},
        "targets": [
          {
            "expr": "vpc_file_pool_capacity_gb - vpc_file_pool_allocated_gb",
            "legendFormat": "{{ pool }}"
          }
        ],
        "fieldConfig": {
          "defaults": {
            "unit": "decgbytes"
          }
        }
      },
      {
        "title": "Allocation Rate (per minute)",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 8},
        "targets": [
          {
            "expr": "rate(vpc_file_pool_allocations_total{status=\"success\"}[5m]) * 60",
            "legendFormat": "{{ pool }} success"
          },
          {
            "expr": "rate(vpc_file_pool_allocations_total{status=\"error\"}[5m]) * 60",
            "legendFormat": "{{ pool }} error"
          }
        ]
      },
      {
        "title": "Allocation Latency (P50/P95/P99)",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 12, "y": 8},
        "targets": [
          {
            "expr": "histogram_quantile(0.5, rate(vpc_file_pool_allocation_duration_seconds_bucket[5m]))",
            "legendFormat": "{{ pool }} p50"
          },
          {
            "expr": "histogram_quantile(0.95, rate(vpc_file_pool_allocation_duration_seconds_bucket[5m]))",
            "legendFormat": "{{ pool }} p95"
          },
          {
            "expr": "histogram_quantile(0.99, rate(vpc_file_pool_allocation_duration_seconds_bucket[5m]))",
            "legendFormat": "{{ pool }} p99"
          }
        ],
        "fieldConfig": {
          "defaults": {
            "unit": "s"
          }
        }
      },
      {
        "title": "VPC API Calls (per minute)",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 16},
        "targets": [
          {
            "expr": "rate(vpc_file_pool_api_calls_total{status=\"success\"}[5m]) * 60",
            "legendFormat": "{{ operation }} success"
          },
          {
            "expr": "rate(vpc_file_pool_api_calls_total{status=\"error\"}[5m]) * 60",
            "legendFormat": "{{ operation }} error"
          }
        ]
      },
      {
        "title": "VPC API Latency P95",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 12, "y": 16},
        "targets": [
          {
            "expr": "histogram_quantile(0.95, rate(vpc_file_pool_api_call_duration_seconds_bucket[5m]))",
            "legendFormat": "{{ operation }}"
          }
        ],
        "fieldConfig": {
          "defaults": {
            "unit": "s"
          }
        }
      },
      {
        "title": "Replication Lag",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 0, "y": 24},
        "targets": [
          {
            "expr": "pool_csi_replication_lag_seconds",
            "legendFormat": "{{ policy }}"
          }
        ],
        "fieldConfig": {
          "defaults": {
            "unit": "s"
          }
        }
      },
      {
        "title": "Replication Sync Duration",
        "type": "timeseries",
        "gridPos": {"h": 8, "w": 12, "x": 12, "y": 24},
        "targets": [
          {
            "expr": "rate(pool_csi_replication_sync_duration_seconds_sum[5m]) / rate(pool_csi_replication_sync_duration_seconds_count[5m])",
            "legendFormat": "{{ policy }} avg"
          }
        ],
        "fieldConfig": {
          "defaults": {
            "unit": "s"
          }
        }
      }
    ]
  }
}
```

### Dashboard Tour

After importing, the dashboard has five rows:

- **Row 1 (Top stats):** Pool utilization gauge (color-coded: green/yellow/orange/red at 0/70/85/95%), PVC count, share count, and free capacity per pool.
- **Row 2 (Allocation activity):** Allocation rate over time (success vs error) and allocation latency percentiles (P50/P95/P99). Spikes in the error series or elevated P99 latency indicate pool issues.
- **Row 3 (VPC API):** API call rate by operation and status, plus P95 API latency by operation. Use this to detect VPC API degradation.
- **Row 4 (Replication lag):** Current replication lag per policy. The chart should stay below your RPO target. Rising lag means syncs are falling behind.
- **Row 5 (Replication sync):** Average sync duration per policy. If this exceeds the schedule interval, syncs are overlapping and the data volume is too large.

---

## Cleanup

> **Note:** In production, keep the ServiceMonitor and PrometheusRules running. This cleanup removes only the examples you applied by hand.

```bash
POOL_NAME=monitored-pool
TUTORIAL_NS=pool-tutorial-monitoring

# 1. Delete PVCs
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

# 5. Delete the example alerting rules from Step 5
oc delete prometheusrule vpc-file-pool-csi-critical -n ibm-vpc-file-pool-csi-monitoring --ignore-not-found
oc delete prometheusrule vpc-file-pool-csi-warning -n ibm-vpc-file-pool-csi-monitoring --ignore-not-found

# 6. The chart's own ServiceMonitors stay

# 7. Delete namespace
oc delete namespace ${TUTORIAL_NS} --timeout=60s

# 8. Verify VPC shares are being deleted
ibmcloud is shares
```

---

## Quick Reference

| What | Command |
|------|---------|
| Raw metrics endpoint | `curl -sk -H "Authorization: Bearer $TOKEN" https://localhost:8080/metrics \| grep vpc_file_pool` |
| Port-forward to controller | `oc port-forward -n kube-system deploy/ibm-vpc-file-pool-csi-controller 8080:8080` |
| List ServiceMonitors | `oc get servicemonitors -n ibm-vpc-file-pool-csi-monitoring` |
| List PrometheusRules | `oc get prometheusrules -n ibm-vpc-file-pool-csi-monitoring -l app=vpc-file-pool-csi` |
| Check active alerts | `oc get prometheusrules -n ibm-vpc-file-pool-csi-monitoring -o yaml` |
| Pool utilization query | `vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb * 100` |
| Replication lag query | `pool_csi_replication_lag_seconds` |
| P99 allocation latency | `histogram_quantile(0.99, rate(vpc_file_pool_allocation_duration_seconds_bucket[5m]))` |
| API error rate | `rate(vpc_file_pool_api_calls_total{status="error"}[5m])` |
| Helm enable monitoring (on by default) | `helm upgrade ... --set metrics.serviceMonitor.enabled=true --set metrics.alerts.enabled=true` |

---

## Helm Values Reference

The full set of monitoring-related Helm values:

```yaml
metrics:
  port: 8080                     # Metrics endpoint port
  serviceMonitor:
    enabled: true                # Create ServiceMonitor CR (needs the Prometheus Operator API)
    interval: 30s                # Scrape interval
  alerts:
    enabled: true                # Create the PrometheusRule (needs the Prometheus Operator API)
    utilizationWarning: 80       # Warning threshold (%)
    utilizationCritical: 95      # Critical threshold (%)
```

The chart's PrometheusRule has more alerts than the examples in Step 5, and more thresholds under `metrics.alerts`. See [HELM-VALUES.md](HELM-VALUES.md) for all of them.

---

## What's Next

- **[Part 8: Migration & Console](TUTORIAL-08-MIGRATION-AND-CONSOLE.md)** -- Migrate PVCs from the stock IBM VPC File CSI driver and explore the OpenShift console plugin
