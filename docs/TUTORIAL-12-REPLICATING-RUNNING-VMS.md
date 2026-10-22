# Part 12: Replicating Running VMs

> **Tutorial Series:** [Index](TUTORIAL.md) | [Part 6: Replication and Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md) | **Part 12** | [Part 13: Failover of Running VMs](TUTORIAL-13-FAILOVER-OF-RUNNING-VMS.md)

Copy a running VM with all its disks to a second cluster, while the VM keeps running. Check that every copy shows one moment for all disks, and see which copies are file-system consistent. The words moment, restore point, change list and receiver are defined in [CONTEXT.md](CONTEXT.md).

**Time:** about 1 hour, most of it waiting for the first copy.
**Status:** Experimental. Read [CROSS-REGION-DR.md](CROSS-REGION-DR.md), "Running VM disks", for the design and the lab results.

How it works, in short:

1. The controller freezes the guest file systems for a moment. QEMU (the program that runs the VM) starts a copy of every disk of the VM at that one moment. The guest thaws.
2. QEMU writes the copy slowly, 20 MB/s for each disk by default, so the VM keeps its speed.
3. A Job sends the copy to the receiver on the destination cluster. The receiver applies it.
4. QEMU keeps a change list. The next copy holds only the blocks that changed.
5. Before the receiver changes a block, it saves the old contents in an undo file. The undo files are restore points.

---

## Prerequisites

- Two clusters, with the driver on both: the **source** (where the VM runs) and the **destination** (the DR cluster). The clusters reach each other over HTTPS. The source cluster needs no NFS route to the destination.
- A running VM on a pool StorageClass on the source cluster, with the guest agent (`qemu-guest-agent`) running. Without the guest agent, copies are crash consistent.
- `oc`, `helm`, `jq`, `openssl`, and the repository checkout (for the chart).

Set the names. This tutorial calls the VM `my-vm` in the namespace `vms` and the source pool `vm-pool`:

```bash
export SRC_KUBECONFIG=~/.kube/source-config
export DR_KUBECONFIG=~/.kube/dr-config
export NS=vms
export VM=my-vm
export SRC_POOL=vm-pool
export DR_POOL=dr-vm-pool
src() { oc --kubeconfig "${SRC_KUBECONFIG}" "$@"; }
dr() { oc --kubeconfig "${DR_KUBECONFIG}" "$@"; }
```

Check the VM on the source cluster:

```bash
src get vmi -n ${NS} ${VM} -o jsonpath='{.status.phase}{" agent="}{.status.conditions[?(@.type=="AgentConnected")].status}{"\n"}'
```

**Expected:** `Running agent=True`.

---

## Step 1: Make the destination pool and the receiver volume

On the **destination** cluster, make a pool. The receiver runs as UID 107, the same user as QEMU. Give the pool the same owner, so a failed-over VM can open its disk.

```bash
export ZONE=$(dr get nodes -o jsonpath='{range .items[*]}{.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' | sort | uniq -c | sort -rn | awk 'NR==1{print $2}')

cat <<EOF | dr apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: ${DR_POOL}
spec:
  zone: ${ZONE}
  profile: dp2
  shareSizeGB: 500
  iops: 3000
  maxShares: 1
  initialShares: 1
  autoExpand: false
  defaultUID: 107
  defaultGID: 107
  defaultPermissions: "0777"
EOF

dr get filesharepool ${DR_POOL}
```

Wait for `Ready`. The receiver needs a volume that is big enough for all the disks you replicate, plus room for restore points:

```bash
cat <<EOF | dr apply -f -
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: repl-destination-data
  namespace: kube-system
spec:
  accessModes: [ReadWriteMany]
  storageClassName: ${DR_POOL}
  resources:
    requests:
      storage: 200Gi
EOF
```

---

## Step 2: Start the receiver

Make a token. Both clusters need it.

```bash
export REPL_TOKEN=$(openssl rand -hex 32)

dr create secret generic repl-receiver-token -n kube-system --from-literal=token="${REPL_TOKEN}"
```

Enable the receiver on the destination cluster. Use the release name and chart path of your install:

```bash
helm upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi \
  --kubeconfig ${DR_KUBECONFIG} -n kube-system --reuse-values \
  --set replicationReceiver.enabled=true \
  --set replicationReceiver.pvcName=repl-destination-data \
  --set replicationReceiver.authSecretName=repl-receiver-token \
  --set replicationReceiver.tls.enabled=true
```

The chart makes a Deployment, a Service, a TLS certificate (it needs cert-manager) and, on OpenShift, a Route. Check them and get the address:

```bash
dr get pods -n kube-system -l app.kubernetes.io/component=replication-receiver
export RECEIVER_HOST=$(dr get route -n kube-system -l app.kubernetes.io/component=replication-receiver -o jsonpath='{.items[0].spec.host}')
echo https://${RECEIVER_HOST}
```

**Expected:** one pod `1/1 Running`, and a host name.

Copy the CA certificate and the token to the **source** cluster:

```bash
dr get secret ibm-vpc-file-pool-csi-receiver-certs -n kube-system -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/receiver-ca.crt

src create secret generic repl-receiver-ca -n kube-system --from-file=ca.crt=/tmp/receiver-ca.crt
src create secret generic repl-source-token -n kube-system --from-literal=token="${REPL_TOKEN}"
```

(`base64 -d` is `base64 -D` on older macOS.)

---

## Step 3: Make the replication policy

On the **source** cluster, make a policy for the pool that holds the VM disks. The policy copies every volume of the pool. To copy only some, add `subVolumeSelector` (see [Part 6](TUTORIAL-06-REPLICATION-AND-FAILOVER.md#step-4-direct-nfs-replication)).

```bash
cat <<EOF | src apply -f -
apiVersion: storage.ibmcloud.io/v1alpha1
kind: ReplicationPolicy
metadata:
  name: vm-to-dr
spec:
  sourcePoolName: ${SRC_POOL}
  destinationEndpoint: https://${RECEIVER_HOST}
  destinationAuthSecretRef: repl-source-token
  destinationCACertSecretRef: repl-receiver-ca
  destinationBasePath: /repl
  schedule: 15m
  maxRetries: 3
  maxParallelSyncs: 2
  vmCopySpeedLimitMBps: 20
  restorePointKeepHours: 24
EOF
```

The fields that matter for a VM:

- `vmCopySpeedLimitMBps` limits each disk copy. At 20 MB/s a VM kept 93% of its write speed in a lab test. The limit also counts empty space, so a first copy of a large disk takes long. `0` means no limit.
- `restorePointKeepHours` is how long the destination keeps earlier moments. It is `24` by default. The disk uses more space the more the VM writes.
- `schedule` is how often a new copy starts.

---

## Step 4: Watch the first copy

```bash
src get replicationpolicy vm-to-dr
src get replicationpolicy vm-to-dr -o json | jq -c '.status.subVolumeStatuses[] | {sv: .subVolumeName[0:16], phase: .vmCopy.phase, moment: .copyMoment, consistency, err: .lastError}'
```

The phase of a copy is `Copying` while QEMU writes the staging file, and `Sending` while a Job applies it at the destination. The first copy is a full copy. At 20 MB/s, a 20 GB disk needs about 17 minutes, and a 100 GB disk about 1.5 hours.

**Expected after the first copy:** every disk of the VM has the same `copyMoment`, `consistency` is `FileSystemConsistent`, and `vmCopy` is empty.

`consistency` can also be `CrashConsistent`. Then `consistencyReason` says why, for example that the guest agent did not answer. A VM that starts from a crash consistent copy may need a file-system check, as after a power loss.

---

## Step 5: Watch an incremental copy and the restore points

Write something in the guest, and wait for the next cycle (15 minutes):

```bash
virtctl ssh -n ${NS} cloud-user@vmi/${VM} --kubeconfig ${SRC_KUBECONFIG} -c 'head -c 50000000 /dev/urandom | sudo tee /var/tmp/big >/dev/null; sync'
src get replicationpolicy vm-to-dr -o json | jq -c '.status.subVolumeStatuses[] | {sv: .subVolumeName[0:16], moment: .copyMoment, bytes: .bytesSynced, points: .restorePoints, oldest: .oldestRestorePoint}'
```

Change `cloud-user` to the user of your guest image.

**Expected:** the second copy sends far less than the disk size (about the 50 MB you wrote, plus whatever the guest wrote). `restorePoints` is now `1`, and `oldestRestorePoint` is the first moment. Each further copy adds a restore point, until `restorePointKeepHours` passes.

---

## What changes a copy

- **Live migration** of the VM cancels the copy.
- **A restart or a move** of the VM loses the change list. The next copy sends only the blocks that differ from the destination disk.
- **A hot-plugged disk** is skipped.
- **A snapshot** of the VM at the same time makes the copy wait. Copies and snapshots of one VM take turns.
- **A stopped VM** is copied as a folder, as a plain volume is. A stopped VM whose disk has instant snapshots is not copied (see [Part 11](TUTORIAL-11-INSTANT-VM-SNAPSHOTS.md)).
- **A running VM that has instant snapshots** gets a full copy after each snapshot.

## See it in the console

Open the replication policy page. For each disk, it shows the moment of the last copy and whether it is file-system or crash consistent. It also shows the restore points and the phase of a copy in progress. See [Part 8](TUTORIAL-08-MIGRATION-AND-CONSOLE.md).

---

## Clean up

On the source cluster:

```bash
src delete replicationpolicy vm-to-dr
src delete secret -n kube-system repl-receiver-ca repl-source-token
```

On the destination cluster, keep the receiver and the data if you go on to [Part 13](TUTORIAL-13-FAILOVER-OF-RUNNING-VMS.md). To remove them:

```bash
helm upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi --kubeconfig ${DR_KUBECONFIG} -n kube-system --reuse-values --set replicationReceiver.enabled=false
dr delete pvc -n kube-system repl-destination-data
dr delete secret -n kube-system repl-receiver-token
dr delete filesharepool ${DR_POOL}
```

---

## What you learned

- A policy copies a running VM at one moment for all disks, and sends only changed blocks after the first copy.
- The status says which moment a copy shows and how consistent it is.
- The destination keeps restore points so you can start the VM from an earlier moment.

## Next

- [Part 13: Failover of running VMs](TUTORIAL-13-FAILOVER-OF-RUNNING-VMS.md)
