---
name: test-e2e
description: Run E2E VM clone test with pre-flight cleanup of stale resources. Pass flags through to test-vm-clone.sh.
argument-hint: "[--keep] [--zone ZONE] [--image FILTER] [--timeout SECS]"
allowed-tools: Bash(oc *), Bash(kubectl *), Bash(bash test/e2e/*), Bash(jq *)
---

Run the E2E VM clone test (`test/e2e/test-vm-clone.sh`) with a hardened pre-flight cleanup.

## Pre-flight cleanup

Before invoking the test script, run a standalone cleanup pass to remove stale resources
from previous runs that may have crashed or timed out. This uses the same sequence as
the test script's own Stage 2 cleanup:

```bash
NAMESPACE="${NAMESPACE:-e2e-vm-test}"
POOL_NAME="${POOL_NAME:-e2e-vm-pool}"
SC_NAME="$POOL_NAME"

echo "=== Pre-flight cleanup ==="

# 1. Disable golden image syncer (stop it re-creating PVCs)
oc patch filesharepools.storage.ibmcloud.io "$POOL_NAME" \
    --type=merge -p '{"spec":{"goldenImages":{"enabled":false}}}' 2>/dev/null || true

# 2. Clean namespace workloads
if oc get namespace "$NAMESPACE" &>/dev/null; then
    oc delete vm --all -n "$NAMESPACE" --ignore-not-found --timeout=30s 2>/dev/null || true
    sleep 5
    oc delete jobs --all -n "$NAMESPACE" --ignore-not-found --timeout=30s 2>/dev/null || true
    oc delete pods --all -n "$NAMESPACE" --force --grace-period=0 2>/dev/null || true
    oc delete pvc --all -n "$NAMESPACE" --ignore-not-found --timeout=60s 2>/dev/null || true
    oc delete templates --all -n "$NAMESPACE" --ignore-not-found 2>/dev/null || true
fi

# 3. Grace period
sleep 5

# 4. Force-clean orphan SubVolumes
SV_COUNT=0
for sv in $(oc get subvolumes.storage.ibmcloud.io -l storage.ibmcloud.io/pool="$POOL_NAME" -o name 2>/dev/null); do
    oc patch "$sv" --type=merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
    SV_COUNT=$((SV_COUNT + 1))
done
if (( SV_COUNT > 0 )); then
    echo "  Cleared $SV_COUNT orphan SubVolume(s)"
    oc delete subvolumes.storage.ibmcloud.io -l storage.ibmcloud.io/pool="$POOL_NAME" \
        --ignore-not-found --timeout=30s 2>/dev/null || true
fi

# 5. Delete pool-owned PVs
PV_COUNT=0
for pv in $(oc get pv -o json 2>/dev/null | jq -r '
    .items[]
    | select(.spec.csi.driver == "vpc-file-pool.csi.ibm.io")
    | select(.spec.csi.volumeAttributes.pool == "'"$POOL_NAME"'")
    | .metadata.name' 2>/dev/null); do
    oc delete pv "$pv" --ignore-not-found --timeout=30s 2>/dev/null || true
    PV_COUNT=$((PV_COUNT + 1))
done
oc delete job,pvc -n kube-system -l storage.ibmcloud.io/share-data-job --ignore-not-found 2>/dev/null || true
oc delete pv -l storage.ibmcloud.io/share-data-job --ignore-not-found 2>/dev/null || true
if (( PV_COUNT > 0 )); then echo "  Deleted $PV_COUNT pool PV(s)"; fi

# 6. Delete pool (force-remove finalizer if stuck)
if oc get filesharepools.storage.ibmcloud.io "$POOL_NAME" &>/dev/null 2>&1; then
    oc delete filesharepools.storage.ibmcloud.io "$POOL_NAME" \
        --ignore-not-found --timeout=30s 2>/dev/null || {
        oc patch filesharepools.storage.ibmcloud.io "$POOL_NAME" \
            --type=merge -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
        oc delete filesharepools.storage.ibmcloud.io "$POOL_NAME" \
            --ignore-not-found --timeout=30s 2>/dev/null || true
    }
fi

# 7. Delete StorageClass
oc delete storageclass "$SC_NAME" --ignore-not-found 2>/dev/null || true

# 8. Delete namespace
if oc get namespace "$NAMESPACE" &>/dev/null; then
    oc delete namespace "$NAMESPACE" --timeout=60s 2>/dev/null || true
    for i in $(seq 1 60); do
        if ! oc get namespace "$NAMESPACE" &>/dev/null; then break; fi
        if (( i == 30 )); then
            for pvc in $(oc get pvc -n "$NAMESPACE" -o name 2>/dev/null); do
                oc patch "$pvc" -n "$NAMESPACE" --type=merge \
                    -p '{"metadata":{"finalizers":null}}' 2>/dev/null || true
            done
        fi
        sleep 2
    done
fi

echo "=== Pre-flight cleanup complete (SubVolumes: $SV_COUNT, PVs: $PV_COUNT) ==="
```

Report what was cleaned (SubVolume count, PV count, stuck resources removed).

## Run test

Then invoke the test script with any user-provided arguments:

```bash
bash test/e2e/test-vm-clone.sh $ARGUMENTS
```

## After running

- Report pass/fail with total elapsed time
- If the test failed, suggest running `/diagnose` to collect full diagnostics
- If the test passed, show the summary (golden image used, VM name, console result)

## Examples

- `/test-e2e` — pre-flight cleanup + full test with defaults
- `/test-e2e --keep` — keep resources after test for debugging
- `/test-e2e --zone us-south-1 --image fedora` — test with specific zone and image filter
- `/test-e2e --timeout 1800` — longer timeout for slow environments
