# Monitoring Guide — IBM VPC File Pool CSI Driver

Complete metrics reference, alerting rules, and dashboard templates for operating the pool CSI driver in production.

---

## Overview

The controller exposes Prometheus metrics on `:8080/metrics`. All metrics use the `vpc_file_pool_` or `pool_csi_` prefix and follow the [Prometheus naming conventions](https://prometheus.io/docs/practices/naming/).

The driver emits 21 metrics across five categories:

| Category | Metrics | Purpose |
|----------|---------|---------|
| Allocation | 6 | PVC provisioning health and capacity |
| VPC API | 2 | API call success rate and latency |
| Snapshots | 2 | Snapshot operation tracking |
| Clones | 2 | Clone operation tracking |
| Group Snapshots | 4 | Multi-PVC snapshot coordination |
| Replication | 5 | Cross-region DR health |

---

## Scrape Configuration

### Prometheus Operator (ServiceMonitor)

If your cluster runs the Prometheus Operator (standard on OpenShift), create a `ServiceMonitor`:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: vpc-file-pool-csi
  namespace: kube-system
  labels:
    app: vpc-file-pool-csi-controller
spec:
  selector:
    matchLabels:
      app: vpc-file-pool-csi-controller
  endpoints:
    - port: metrics
      interval: 30s
      path: /metrics
  namespaceSelector:
    matchNames:
      - kube-system
```

### OpenShift User Workload Monitoring

On OpenShift clusters with user workload monitoring enabled:

```bash
# Verify user workload monitoring is enabled
kubectl get configmap cluster-monitoring-config -n openshift-monitoring -o yaml

# The ServiceMonitor above works automatically if user workload monitoring
# is configured to scrape the kube-system namespace
```

### Manual Scrape Config (prometheus.yml)

If not using the Prometheus Operator, add a scrape job:

```yaml
scrape_configs:
  - job_name: 'vpc-file-pool-csi'
    kubernetes_sd_configs:
      - role: pod
        namespaces:
          names: ['kube-system']
    relabel_configs:
      - source_labels: [__meta_kubernetes_pod_label_app]
        regex: vpc-file-pool-csi-controller
        action: keep
      - source_labels: [__meta_kubernetes_pod_container_port_number]
        regex: "8080"
        action: keep
      - source_labels: [__meta_kubernetes_namespace]
        target_label: namespace
      - source_labels: [__meta_kubernetes_pod_name]
        target_label: pod
```

### Verifying Scrape

```bash
# Port-forward to the metrics endpoint
kubectl port-forward -n kube-system svc/vpc-file-pool-csi-controller 8080:8080 &

# Check that metrics are being exported
curl -s http://localhost:8080/metrics | grep vpc_file_pool | head -20

# Clean up
kill %1
```

---

## Complete Metric Reference

### Allocation Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_allocations_total` | Counter | `pool`, `status` | Total subvolume allocations. `status` is `success` or `error`. |
| `vpc_file_pool_allocation_duration_seconds` | Histogram | `pool` | Time to allocate a subvolume. Buckets: 10ms to ~163s (exponential). |
| `vpc_file_pool_capacity_gb` | Gauge | `pool` | Total pool capacity in GB across all shares. |
| `vpc_file_pool_allocated_gb` | Gauge | `pool` | Total allocated (requested) capacity in GB. |
| `vpc_file_pool_share_count` | Gauge | `pool` | Number of VPC file shares in the pool. |
| `vpc_file_pool_pvc_count` | Gauge | `pool` | Number of active PVCs (SubVolumes) in the pool. |

**Normal ranges:**

| Metric | Healthy Range | Investigate If |
|--------|--------------|----------------|
| `allocations_total{status="error"}` | 0 | Any errors — check controller logs |
| `allocation_duration_seconds` | < 1s (p99) | > 5s — pool may be expanding or unhealthy |
| `capacity_gb - allocated_gb` | > 20% of capacity | < 10% — pool approaching full |
| `share_count` | 1 to `maxShares` | At `maxShares` — cannot auto-expand |

### VPC API Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_api_calls_total` | Counter | `operation`, `status` | Total VPC API calls. `operation` is the API method (e.g., `create_share`, `get_share`). `status` is `success` or `error`. |
| `vpc_file_pool_api_call_duration_seconds` | Histogram | `operation` | VPC API call duration. Buckets: 100ms to ~1638s (exponential). |

**Normal ranges:**

| Metric | Healthy Range | Investigate If |
|--------|--------------|----------------|
| `api_calls_total{status="error"}` | 0 in steady state | Sustained errors — check VPC API status, auth, quotas |
| `api_call_duration_seconds` | 1-90s for `create_share`, < 5s for `get_share` | > 120s — VPC API may be degraded |

### Snapshot Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_snapshots_total` | Counter | `pool`, `operation`, `status` | Snapshot operations. `operation`: `create`, `delete`, `restore`. `status`: `success`, `error`. |
| `vpc_file_pool_snapshot_duration_seconds` | Histogram | `pool`, `operation` | Time for snapshot operations. Proportional to data size. |

### Clone Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_clones_total` | Counter | `pool`, `status` | Clone operations by outcome. |
| `vpc_file_pool_clone_duration_seconds` | Histogram | `pool` | Time for synchronous clone operations. Async clones are tracked via SubVolume CR status. |

### Group Snapshot Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `vpc_file_pool_group_snapshots_total` | Counter | `pool`, `operation`, `status` | Group snapshot operations. `operation`: `create`, `delete`. |
| `vpc_file_pool_group_snapshot_duration_seconds` | Histogram | `pool`, `operation` | Time for group snapshot operations (total, all members). |
| `vpc_file_pool_group_snapshot_member_count` | Histogram | `pool` | Number of PVCs per group snapshot. Buckets: 1 to 20. |
| `vpc_file_pool_group_snapshot_inconsistency_window_ms` | Histogram | `pool` | Time between first and last member copy in milliseconds. Lower is better for consistency. |

### Replication Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `pool_csi_replication_sync_total` | Counter | `policy`, `source_pool`, `result` | Replication sync attempts. `result`: `success`, `error`, `skipped`. |
| `pool_csi_replication_sync_duration_seconds` | Histogram | `policy`, `source_pool` | Duration of replication sync cycles. |
| `pool_csi_replication_lag_seconds` | Gauge | `policy`, `source_pool` | Current replication lag (seconds since last successful sync). This is your RPO indicator. |
| `pool_csi_replication_consecutive_failures` | Gauge | `policy`, `source_pool` | Current consecutive failure count. Policy pauses after exceeding `maxRetries`. |
| `pool_csi_replication_subvolume_count` | Gauge | `policy`, `source_pool` | Number of SubVolumes being replicated per policy. |

**Normal ranges:**

| Metric | Healthy Range | Investigate If |
|--------|--------------|----------------|
| `replication_lag_seconds` | < 2x the schedule interval | > 3x schedule — sync is falling behind |
| `replication_consecutive_failures` | 0 | > 0 — check connectivity to destination NFS |
| `replication_sync_duration_seconds` | < schedule interval | > schedule — syncs overlap, data too large |

---

## Alerting Rules

### Critical Alerts

These require immediate attention — they indicate data loss risk or service degradation.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: vpc-file-pool-csi-critical
  namespace: kube-system
  labels:
    app: vpc-file-pool-csi
spec:
  groups:
    - name: vpc-file-pool-csi.critical
      rules:

        - alert: PoolAtMaxCapacity
          expr: |
            vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb > 0.95
            and
            vpc_file_pool_share_count >= vpc_file_pool_share_count
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "Pool {{ $labels.pool }} is over 95% allocated"
            description: "Pool {{ $labels.pool }} has less than 5% capacity remaining. New PVC requests will fail if the pool cannot expand. Check maxShares and VPC quota."
            runbook_url: "TROUBLESHOOTING.md#pool-full--maxshares-reached"

        - alert: PoolAtMaxShares
          expr: vpc_file_pool_share_count >= 300
          for: 1m
          labels:
            severity: critical
          annotations:
            summary: "Pool {{ $labels.pool }} may be at VPC account share limit (300)"
            description: "The pool has 300+ shares, which is the VPC account default limit. New share creation will fail."
            runbook_url: "TROUBLESHOOTING.md#quota-exceeded-300-shares"

        - alert: AllocationFailureRate
          expr: rate(vpc_file_pool_allocations_total{status="error"}[5m]) > 0.1
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "High allocation failure rate in pool {{ $labels.pool }}"
            description: "More than 0.1 allocation failures per second sustained for 5 minutes. PVC creation is failing."
            runbook_url: "TROUBLESHOOTING.md#pvc-issues"

        - alert: ControllerDown
          expr: absent(up{job="vpc-file-pool-csi"} == 1)
          for: 5m
          labels:
            severity: critical
          annotations:
            summary: "VPC File Pool CSI controller is down"
            description: "No metrics received from the controller for 5 minutes. New PVC operations are blocked."

        - alert: ReplicationPaused
          expr: pool_csi_replication_consecutive_failures > 3
          for: 10m
          labels:
            severity: critical
          annotations:
            summary: "Replication policy {{ $labels.policy }} is paused due to failures"
            description: "Replication has {{ $value }} consecutive failures and is likely paused. DR data is stale."
            runbook_url: "CROSS-REGION-DR.md"
```

### Warning Alerts

These indicate potential issues that should be investigated during business hours.

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: vpc-file-pool-csi-warning
  namespace: kube-system
  labels:
    app: vpc-file-pool-csi
spec:
  groups:
    - name: vpc-file-pool-csi.warning
      rules:

        - alert: PoolNearCapacity
          expr: vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb > 0.85
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "Pool {{ $labels.pool }} is over 85% allocated"
            description: "Pool is nearing capacity. Verify autoExpand is enabled and maxShares has room."

        - alert: AllocationSlowdown
          expr: histogram_quantile(0.99, rate(vpc_file_pool_allocation_duration_seconds_bucket[5m])) > 5
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "Slow PVC allocations in pool {{ $labels.pool }}"
            description: "P99 allocation latency is {{ $value }}s (expected < 1s). Pool may be expanding or degraded."

        - alert: VPCAPIErrorRate
          expr: rate(vpc_file_pool_api_calls_total{status="error"}[10m]) > 0.05
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "VPC API errors for {{ $labels.operation }}"
            description: "Sustained VPC API errors detected. Check API key validity and VPC service status."

        - alert: ReplicationLagHigh
          expr: pool_csi_replication_lag_seconds > 7200
          for: 30m
          labels:
            severity: warning
          annotations:
            summary: "Replication lag exceeds 2 hours for policy {{ $labels.policy }}"
            description: "DR data is {{ $value | humanizeDuration }} behind. RPO may be violated."

        - alert: SnapshotFailures
          expr: rate(vpc_file_pool_snapshots_total{status="error"}[10m]) > 0
          for: 10m
          labels:
            severity: warning
          annotations:
            summary: "Snapshot failures in pool {{ $labels.pool }}"
            description: "Snapshot {{ $labels.operation }} operations are failing. Check share health and capacity."

        - alert: GroupSnapshotHighInconsistency
          expr: histogram_quantile(0.95, rate(vpc_file_pool_group_snapshot_inconsistency_window_ms_bucket[1h])) > 60000
          for: 30m
          labels:
            severity: warning
          annotations:
            summary: "Group snapshot inconsistency window exceeds 60s in pool {{ $labels.pool }}"
            description: "Large data volumes are causing wide inconsistency windows. Consider using pre-snapshot hooks for quiescing."
```

---

## PromQL Query Cookbook

### Capacity Planning

```promql
# Pool utilization percentage
vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb * 100

# Free capacity per pool (GB)
vpc_file_pool_capacity_gb - vpc_file_pool_allocated_gb

# Allocation rate (PVCs per hour)
rate(vpc_file_pool_allocations_total{status="success"}[1h]) * 3600

# Projected hours to full at current allocation rate
(vpc_file_pool_capacity_gb - vpc_file_pool_allocated_gb)
/ (deriv(vpc_file_pool_allocated_gb[1h]) + 0.001)

# Average PVC size (GB)
vpc_file_pool_allocated_gb / (vpc_file_pool_pvc_count > 0)

# PVCs per share
vpc_file_pool_pvc_count / (vpc_file_pool_share_count > 0)
```

### Performance

```promql
# P50/P95/P99 allocation latency
histogram_quantile(0.5, rate(vpc_file_pool_allocation_duration_seconds_bucket[5m]))
histogram_quantile(0.95, rate(vpc_file_pool_allocation_duration_seconds_bucket[5m]))
histogram_quantile(0.99, rate(vpc_file_pool_allocation_duration_seconds_bucket[5m]))

# VPC API call rate by operation
rate(vpc_file_pool_api_calls_total[5m])

# P95 VPC API latency by operation
histogram_quantile(0.95, rate(vpc_file_pool_api_call_duration_seconds_bucket[5m]))

# API error ratio
rate(vpc_file_pool_api_calls_total{status="error"}[5m])
/ rate(vpc_file_pool_api_calls_total[5m])
```

### Replication

```promql
# Current replication lag (RPO indicator)
pool_csi_replication_lag_seconds

# Sync success rate
rate(pool_csi_replication_sync_total{result="success"}[1h])
/ rate(pool_csi_replication_sync_total[1h])

# Average sync duration
rate(pool_csi_replication_sync_duration_seconds_sum[1h])
/ rate(pool_csi_replication_sync_duration_seconds_count[1h])

# SubVolumes per replication policy
pool_csi_replication_subvolume_count
```

### Anomaly Detection

```promql
# Allocation rate spike (> 3x normal over 24h baseline)
rate(vpc_file_pool_allocations_total{status="success"}[5m])
> 3 * avg_over_time(rate(vpc_file_pool_allocations_total{status="success"}[5m])[24h:5m])

# Sudden capacity drop (possible mass deletion)
delta(vpc_file_pool_allocated_gb[5m]) < -100

# Share count instability (shares being created/removed frequently)
changes(vpc_file_pool_share_count[1h]) > 5
```

---

## Grafana Dashboard

### Import Instructions

1. Copy the JSON below
2. In Grafana, go to Dashboards > Import
3. Paste the JSON and click Load
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
        "title": "Capacity (GB)",
        "type": "stat",
        "gridPos": {"h": 8, "w": 6, "x": 18, "y": 0},
        "targets": [
          {
            "expr": "vpc_file_pool_capacity_gb - vpc_file_pool_allocated_gb",
            "legendFormat": "{{ pool }} free"
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

---

## Integration: IBM Cloud Monitoring (Sysdig)

If your cluster uses IBM Cloud Monitoring (powered by Sysdig), the Prometheus metrics are automatically collected if Sysdig agent is configured to scrape Prometheus endpoints:

```yaml
# In the Sysdig agent configmap, ensure Prometheus scraping is enabled:
prometheus:
  enabled: true
  histograms: true
```

Create Sysdig alerts using PromQL syntax in the IBM Cloud Monitoring dashboard. The same PromQL queries from the cookbook above work in Sysdig.

---

## Integration: Alertmanager Routing

Route CSI driver alerts to the storage team:

```yaml
# alertmanager.yml
route:
  routes:
    - match:
        job: vpc-file-pool-csi
      receiver: storage-team
      group_by: ['alertname', 'pool']
      group_wait: 30s
      group_interval: 5m
      repeat_interval: 4h

receivers:
  - name: storage-team
    slack_configs:
      - channel: '#storage-alerts'
        title: '{{ .GroupLabels.alertname }}'
        text: '{{ range .Alerts }}{{ .Annotations.description }}{{ end }}'
    pagerduty_configs:
      - service_key: '<your-pagerduty-key>'
        severity: '{{ if eq .GroupLabels.severity "critical" }}critical{{ else }}warning{{ end }}'
```

---

## See Also

- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — Metrics-specific troubleshooting (stale metrics, scrape failures)
- [PERFORMANCE-TUNING.md](PERFORMANCE-TUNING.md) — Capacity planning PromQL queries
- [HELM-VALUES.md](HELM-VALUES.md) — Metrics endpoint configuration
- [CAPACITY-PLANNING.md](CAPACITY-PLANNING.md) — Using metrics for growth projection
- [`pkg/metrics/metrics.go`](pkg/metrics/metrics.go) — Metric definitions in source code
