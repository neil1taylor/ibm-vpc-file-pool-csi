#!/usr/bin/env bash
# sidecar-gate.sh — Turn the KubeVirt "Sidecar" feature gate on or off.
#
# Instant VM snapshots need this gate (see VM-INSTANT-SNAPSHOTS.md). The
# script edits only its own entry in the HyperConverged jsonpatch annotation;
# other entries stay. It refuses to run on a cluster other than CLUSTER.
#
# Usage: hack/sidecar-gate.sh on|off CLUSTER [KUBECONFIG]
#   CLUSTER     the cluster name that the current context must start with
#   KUBECONFIG  the kubeconfig file; default: $KUBECONFIG
# Requires: oc, jq.

set -euo pipefail

mode=${1:-}
cluster=${2:-}
[ -n "${3:-}" ] && export KUBECONFIG="$3"
case "$mode" in on|off) ;; *) echo "usage: $0 on|off CLUSTER [KUBECONFIG]" >&2; exit 1 ;; esac
[ -n "$cluster" ] || { echo "usage: $0 on|off CLUSTER [KUBECONFIG]" >&2; exit 1; }

current=$(oc config current-context | cut -d/ -f1)
[ "$current" = "$cluster" ] || { echo "the current cluster is $current, not $cluster" >&2; exit 1; }

cur=$(oc get hyperconverged kubevirt-hyperconverged -n openshift-cnv -o jsonpath='{.metadata.annotations.kubevirt\.kubevirt\.io/jsonpatch}')
[ -n "$cur" ] || cur='[]'
op='{"op":"add","path":"/spec/configuration/developerConfiguration/featureGates/-","value":"Sidecar"}'
case "$mode" in
    on)  new=$(echo "$cur" | jq -c --argjson op "$op" 'map(select(. != $op)) + [$op]') ;;
    off) new=$(echo "$cur" | jq -c --argjson op "$op" 'map(select(. != $op))') ;;
esac
echo "before: $cur"
echo "after:  $new"
oc annotate --overwrite hyperconverged kubevirt-hyperconverged -n openshift-cnv kubevirt.kubevirt.io/jsonpatch="$new"

gates=""
for _ in $(seq 1 40); do
    gates=$(oc get kubevirt -n openshift-cnv -o jsonpath='{.items[0].spec.configuration.developerConfiguration.featureGates}')
    if [ "$mode" = on ] && echo "$gates" | grep -q '"Sidecar"'; then break; fi
    if [ "$mode" = off ] && ! echo "$gates" | grep -q '"Sidecar"'; then break; fi
    sleep 3
done
echo "KubeVirt feature gates now: $gates"
