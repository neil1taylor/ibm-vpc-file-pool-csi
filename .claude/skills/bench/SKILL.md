---
name: bench
description: Create a benchmark pool, run fio sequential and random I/O tests, report results, and clean up
disable-model-invocation: true
argument-hint: "[share-size-gb] (default: 2000)"
allowed-tools: Bash(kubectl *), Bash(sleep *), Bash(bash -c *)
---

Run an end-to-end storage benchmark using the procedure from PERFORMANCE-TUNING.md.

Share size: $ARGUMENTS (default to 2000 GB if no argument provided).

## Steps

### 1. Create benchmark pool

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: bench-pool
spec:
  zone: <zone>   # bash -c "source test/e2e/lib/zone.sh; detect_zone"
  profile: dp2
  shareSizeGB: <share-size>
  maxShares: 1
  initialShares: 1
  autoExpand: false
  allocationStrategy: spread
  defaultUID: 1000
  defaultGID: 1000
  defaultPermissions: "0755"
```

Apply and wait for Phase: Ready (poll every 10s, timeout after 5 minutes).

### 2. Create benchmark PVC

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: bench-pvc
  namespace: default
spec:
  accessModes: [ReadWriteMany]
  storageClassName: bench-pool
  resources:
    requests:
      storage: 100Gi
```

Wait for Bound status.

### 3. Run sequential write test (60s)

Create a pod with `nixery.dev/fio` image:
- `fio --name=seqwrite --rw=write --bs=1M --size=1G --numjobs=1 --time_based --runtime=60 --group_reporting --filename=/data/testfile --output-format=json`
- Wait for completion, capture logs

### 4. Run random read test (60s)

Create a pod with `nixery.dev/fio` image:
- `fio --name=randread --rw=randread --bs=4K --size=1G --numjobs=4 --time_based --runtime=60 --group_reporting --iodepth=32 --filename=/data/testfile --output-format=json`
- Wait for completion, capture logs

### 5. Report results

Parse the fio JSON output and present a summary table:

| Metric | Result |
|--------|--------|
| Sequential write throughput (MB/s) | |
| Sequential write avg latency (ms) | |
| Random read IOPS | |
| Random read avg latency (ms) | |

Compare against expected ranges from PERFORMANCE-TUNING.md.

### 6. Clean up

```bash
kubectl delete pod fio-sequential fio-random --ignore-not-found
kubectl delete pvc bench-pvc --ignore-not-found
kubectl delete filesharepools bench-pool --ignore-not-found
```

Confirm cleanup is complete.
