#!/usr/bin/env bash
# 15-metrics — Prometheus (user workload monitoring) has the driver's metrics,
# with non-zero values after the earlier stages, and each pool reconciles
# about once a minute.

RT_STAGE_TIMEOUT=600

PROM_NS="${RT_PROM_NS:-openshift-user-workload-monitoring}"
PROM_POD="${RT_PROM_POD:-prometheus-user-workload-0}"

# prom_value QUERY — the first value of an instant query, or empty.
prom_value() {
    oc exec -n "$PROM_NS" "$PROM_POD" -c prometheus -- \
        curl -s --get --data-urlencode "query=$1" http://localhost:9090/api/v1/query |
        jq -r '.data.result[0].value[1] // empty'
}

# positive QUERY — the query's value is above zero.
positive() {
    local v; v=$(prom_value "$1")
    echo "$1 = ${v:-no data}"
    [ -n "$v" ] && awk -v v="$v" 'BEGIN { exit !(v > 0) }'
}

has_series() {
    local v; v=$(prom_value "count($1)")
    echo "$1 series: ${v:-0}"
    [ -n "$v" ] && [ "$v" -gt 0 ]
}

# Reconciles per pool per minute, over the last 10 minutes. The old requeue
# loop made it about 30.
reconcile_rate_ok() {
    local pools rate
    pools=$(oc get filesharepools -o name | wc -l)
    rate=$(prom_value 'sum(rate(controller_runtime_reconcile_total{controller="filesharepool"}[10m])) * 60')
    echo "pools: $pools, reconciles per minute: ${rate:-no data}"
    [ "$pools" -gt 0 ] && [ -n "$rate" ] && awk -v r="$rate" -v p="$pools" 'BEGIN { exit !(r / p <= 3) }'
}

run() {
    oc get pod -n "$PROM_NS" "$PROM_POD" >/dev/null 2>&1 || skip "no user workload Prometheus ($PROM_NS/$PROM_POD)"
    has_series vpc_file_pool_capacity_gb >/dev/null 2>&1 || skip "Prometheus does not scrape the driver (metrics.serviceMonitor.enabled)"

    check "Prometheus has the pool capacity metrics" has_series vpc_file_pool_capacity_gb
    check "Prometheus has the share count metric" has_series vpc_file_pool_share_count
    check "volumes were allocated" positive 'sum(vpc_file_pool_allocations_total)'
    check "the driver called the VPC API" positive 'sum(vpc_file_pool_api_calls_total)'
    if [ "$(oc get filesharepools -o name | wc -l)" -gt 0 ]; then
        check "each pool reconciles about once a minute" reconcile_rate_ok
    else
        not_checked "each pool reconciles about once a minute" "no pools exist"
    fi
}

cleanup() { :; }
