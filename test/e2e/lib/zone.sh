#!/usr/bin/env bash
# zone.sh — Helpers for picking the zone of an E2E test pool.

# detect_zone prints the zone that has the most worker nodes, from the
# topology.kubernetes.io/zone label. A tie goes to the first zone by name.
# Returns 1 if no worker has a zone label.
detect_zone() {
    local zone
    zone="$(kubectl get nodes -l node-role.kubernetes.io/worker \
              -o jsonpath='{range .items[*]}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' \
            | grep -v '^$' | sort | uniq -c | sort -k1,1nr -k2,2 | awk 'NR==1 {print $2}')"
    if [[ -z "$zone" ]]; then
        echo "no worker node has a topology.kubernetes.io/zone label; pass --zone" >&2
        return 1
    fi
    echo "$zone"
}
