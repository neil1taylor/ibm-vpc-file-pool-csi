# Part 13: Failover of Running VMs

> **Tutorial Series:** [Index](TUTORIAL.md) | [Part 12: Replicating Running VMs](TUTORIAL-12-REPLICATING-RUNNING-VMS.md) | **Part 13**

Start a replicated VM on the destination (DR) cluster. Choose the restore point it starts from: the newest one, or an earlier restore point. The words failover, rewind and restore point are defined in [CONTEXT.md](CONTEXT.md).

**Time:** about 30 minutes, plus the time to copy a disk if you rewind.
**Before you start:** finish [Part 12](TUTORIAL-12-REPLICATING-RUNNING-VMS.md), so the destination holds copies of a VM with at least one good copy of every disk.

What the failover tool does and does not do:

- It reads the metadata that replication left on the destination share. It creates a SubVolume, a PersistentVolume and a PVC for each volume on the DR cluster.
- For a running VM, it picks the newest restore point that all disks of the VM can show, so the disks fit each other. It rolls back a copy that stopped half way.
- It does **not** make the VM, start the workload or change DNS. You apply the VM definition yourself.

![The failover tool reads the replicated folders on the destination share and makes a SubVolume, PV and PVC for each volume; you apply the VM](images/tutorial-13-failover.drawio.svg)

> **Rewind before you start the VM.** A VM that runs on the latest disk writes to it. After that, the disk no longer matches its restore points, and the tool refuses to rewind it. If you may want an earlier restore point, rewind first.

---

## Prerequisites

- The destination cluster from Part 12: the receiver, the pool `dr-vm-pool`, and the volume `repl-destination-data` in `kube-system`.
- A place to run the failover tool where the destination share is mounted. The tool reads the replicated folders from a mounted share. There are two ways:
  - **A host** that can reach the destination share over NFS and the DR cluster's API. A VSI in the destination VPC works. A host in another VPC usually cannot reach the share (the NFS port times out).
  - **A pod next to the share.** The receiver pod cannot do this: it has no rights on the Kubernetes API. Make a pod that mounts the receiver volume (`repl-destination-data`, at `/data`) and runs under a service account that may create SubVolumes, PersistentVolumes and PersistentVolumeClaims and read FileSharePools. This is the way used in the steps below.
- The failover tool, built for Linux x86 (the failover pod and most hosts):

```bash
make build-failover        # makes bin/kubectl-failover; set GOOS=linux GOARCH=amd64 on another system
```

For a host, copy it to `/usr/local/bin/` and check `kubectl failover version`.

**The failover pod.** Make a service account with these rights and a pod that mounts the receiver volume. Any image with a shell works; the driver image does:

```bash
cat <<EOF | dr apply -f -
apiVersion: v1
kind: ServiceAccount
metadata: {name: failover-tool, namespace: kube-system}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: failover-tool}
rules:
  - {apiGroups: ["storage.ibmcloud.io"], resources: ["subvolumes"], verbs: ["get", "list", "create"]}
  - {apiGroups: ["storage.ibmcloud.io"], resources: ["filesharepools"], verbs: ["get", "list"]}
  - {apiGroups: [""], resources: ["persistentvolumes", "persistentvolumeclaims"], verbs: ["get", "list", "create"]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: failover-tool}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: failover-tool}
subjects: [{kind: ServiceAccount, name: failover-tool, namespace: kube-system}]
---
apiVersion: v1
kind: Pod
metadata: {name: failover-tool, namespace: kube-system}
spec:
  serviceAccountName: failover-tool
  imagePullSecrets: [{name: all-icr-io}]
  securityContext: {runAsUser: 107, runAsGroup: 107, runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: tool
      image: de.icr.io/ibm-vpc-file-pool-csi/driver:<your image tag>
      command: ["sleep", "infinity"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: ["ALL"]}}
      volumeMounts: [{name: data, mountPath: /data}]
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: repl-destination-data}
EOF
dr wait --for=condition=Ready pod/failover-tool -n kube-system
dr exec -i -n kube-system failover-tool -- sh -c 'cat > /tmp/kubectl-failover && chmod +x /tmp/kubectl-failover' < bin/kubectl-failover
failover() { dr exec -n kube-system failover-tool -- /tmp/kubectl-failover "$@"; }
failover version
```

**Expected:** `pod/failover-tool condition met`, then a version line such as `kubectl-failover v0.19.4` (a binary made with `make build-failover`; a plain `go build` prints `kubectl-failover dev`). Making the ClusterRole and the binding needs a cluster admin.

Below, `failover <args>` is the pod way. On a host, read `kubectl failover <args>` instead, and add `--kubeconfig ${DR_KUBECONFIG}` to `execute` and `status`. Delete the pod, the ClusterRole and the binding when the failover is done.

Set the names from Part 12:

```bash
export DR_KUBECONFIG=~/.kube/dr-config
export DR_POOL=dr-vm-pool
dr() { oc --kubeconfig "${DR_KUBECONFIG}" "$@"; }
```

---

## Step 1: Stop replication (only if the source still runs)

If the source cluster is alive, pause the policy, so no copy arrives while you fail over:

```bash
oc --kubeconfig ${SRC_KUBECONFIG:-~/.kube/source-config} annotate replicationpolicy vm-to-dr storage.ibmcloud.io/paused=true
```

If the source cluster is lost, skip this step.

---

## Step 2: Find the destination share

Find the share's address and the folder of the receiver volume on the DR cluster:

```bash
export DR_IP=$(dr get filesharepool ${DR_POOL} -o jsonpath='{.status.shares[0].mountTargetIP}')
export DR_EXPORT=$(dr get filesharepool ${DR_POOL} -o jsonpath='{.status.shares[0].exportPath}')
export RECV_PV=$(dr get pvc -n kube-system repl-destination-data -o jsonpath='{.spec.volumeName}')
export RECV_SUB=$(dr get subvolume ${RECV_PV} -o jsonpath='{.spec.subPath}')
echo "${DR_IP} export='${DR_EXPORT}' receiver folder=${RECV_SUB}"
```

**Expected** from the first `echo` above, for example: `10.251.0.18 export='/87ed0356_0c3e_4d81_bf77_fa644b6a49d5' receiver folder=/pvcs/pvc-44f85e63-d860-4157-9edf-65268a2a5f2d`.

**In the failover pod,** the receiver volume is mounted at `/data`, so the replicated folders are in `/data/repl/pvcs`:

```bash
dr exec -n kube-system failover-tool -- ls /data/repl/pvcs
export FAILOVER_PATH=/data/repl
```

**On a host,** mount the whole share. The share needs `sec=sys`:

```bash
sudo mkdir -p /mnt/dr
sudo mount -t nfs4 -o nfsvers=4.1,sec=sys,soft,timeo=600,retrans=3 ${DR_IP}:${DR_EXPORT:-/} /mnt/dr
ls /mnt/dr${RECV_SUB}/repl/pvcs
export FAILOVER_PATH=/mnt/dr${RECV_SUB}/repl
```

**Expected:** one folder `pvc-<uuid>` for each replicated volume (two in the lab). In Part 12 the policy used `destinationBasePath: /repl`, so the failover path is the receiver folder plus `/repl`.

---

## Step 3: Plan the failover

```bash
failover plan --nfs-mount-path ${FAILOVER_PATH}
```

**Expected:** a table with one row for each volume. The columns are SubVolume, PVC, namespace, size, age of the last copy and policy. Then the RPO (the age of the oldest last copy) and a list of the VMs with the restore point each will show:

```
Failover Plan
=============
NFS Mount Path:  /data/repl
SubVolumes:      2
RPO (max age):   21m11s

SUBVOLUME                                 PVC             NAMESPACE  SIZE (GiB)  LAST SYNC   POLICY
pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708  my-vm-rootdisk  tutf-vms   20          21m11s ago  tutf-vm-to-dr
pvc-becf191e-0669-45da-b29f-c08d8708ad8b  my-vm-datadisk  tutf-vms   2           21m11s ago  tutf-vm-to-dr

Running VM disks
  VM tutf-vms/my-vm shows the restore point 2026-11-05T11:21:45Z
    pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708: in place
    pvc-becf191e-0669-45da-b29f-c08d8708ad8b: in place

To execute failover, run:
  kubectl failover execute --nfs-mount-path /data/repl --dr-pool <POOL> --dr-share-ip <IP>
```

`in place` means the disk already shows that restore point, so it is used as it is. The tool refuses with "a copy is still being applied" if a copy is in progress. Wait for it to end, or add `--force`, which rolls the unfinished copy back first.

To use an earlier restore point, list the restore points that all disks of each VM can show (newest first):

```bash
failover plan --nfs-mount-path ${FAILOVER_PATH} --list-restore-points
```

**Expected:**

```
VM tutf-vms/my-vm
  2026-11-05T11:21:45Z
  2026-11-05T11:06:15Z
```

The list reaches back as far as `restorePointKeepHours` of the policy (24 hours by default). Choose one:

```bash
export RESTORE_POINT=2026-11-05T11:06:15Z
failover plan --nfs-mount-path ${FAILOVER_PATH} --rewind-to ${RESTORE_POINT}
```

**Expected:** the plan now says `VM tutf-vms/my-vm shows the restore point 2026-11-05T11:06:15Z` and, for each disk, `rewind (copies the whole disk into a new folder)`. The RPO grows to the age of that restore point. The tool uses the newest restore point at or before `--rewind-to`.

### Or use the Failover resource and the console

The same plan and failover can run without a shell. On the DR cluster, a cluster admin creates a `Failover` (or uses the Recovery page in the console). The receiver's data claim is a claim that the driver provisioned, and the controller finds its share address by itself. Part 12 used `destinationBasePath: /repl`, so the copies are in the `repl` folder of the claim, and the Failover must say so with `basePath`:

```bash
dr apply -f - <<'YAML'
apiVersion: storage.ibmcloud.io/v1alpha1
kind: Failover
metadata:
  name: plan-1
spec:
  action: Plan
  basePath: /repl
YAML
dr get failover plan-1 -o yaml
```

**Expected:** `phase: Planned` after about 5 seconds, with the VMs and the restore points in the status:

```
status:
  phase: Planned
  rpo: 1m14s
  source: repl-destination-data
  vms:
  - disks:
    - rewind: false
      subVolume: pvc-65968d12-062b-4b51-9590-269905b09e51
    - rewind: false
      subVolume: pvc-fbe4c104-cd1e-48a7-b465-0a77466d979b
    restorePoint: "2026-11-05T08:32:15Z"
    restorePoints:
    - "2026-11-05T08:32:15Z"
    - "2026-11-05T08:17:15Z"
    name: tutf-vms/my-vm
  volumes: [one entry for each disk, with its PVC name, namespace, size and subPath]
```

To plan an earlier restore point, add `rewindTo: "2026-11-05T08:17:15Z"` to the spec. The plan then says `rewind: true` for each disk and `rpo: 17m4s`.

To run the failover, make the namespace of the VM on the DR cluster, then apply an Execute with the same `basePath` and `rewindTo` and with the pool:

```bash
dr create namespace vms --dry-run=client -o yaml | dr apply -f -
dr apply -f - <<'YAML'
apiVersion: storage.ibmcloud.io/v1alpha1
kind: Failover
metadata:
  name: exec-1
spec:
  action: Execute
  basePath: /repl
  drPool: ${DR_POOL}
  rewindTo: "2026-11-05T08:17:15Z"
YAML
dr get failover exec-1 -o jsonpath='{.status.phase}{"\n"}'
dr get pvc -n vms
```

Replace `${DR_POOL}` with the pool name (the heredoc is quoted, so the shell does not do it). **Expected:** `Done` after 5 minutes for a rewind of a 20 GiB and a 2 GiB disk (without `rewindTo` it is much faster), and the PVCs `Bound` with PVs named `dr-pvc-<uuid>`. In the lab, the VM definition from Step 6 then started on the rewound disks in 20 seconds, the guest agent connected, the file written after the chosen restore point was not there, and the kernel log held no file-system errors.

If `basePath` is wrong, the Plan fails and says which folder it looked for; if it finds copies in another folder it names the `basePath` to use. An Execute needs a Plan with the same `basePath` and a `drPool`. The Recovery page does not set `basePath`: with a `destinationBasePath` other than `/`, make the Plan and the Execute with `kubectl` as above, or use `destinationBasePath: /` for the policy.

---

## Step 4: Rewind the disks (optional)

Skip this step if the plan says `in place` for every disk and you want the newest restore point.

```bash
failover execute --nfs-mount-path ${FAILOVER_PATH} --prepare-only --rewind-to ${RESTORE_POINT}
```

`--prepare-only` rolls back unfinished copies and rewinds the disks, then stops. It needs no cluster and no pool name. Each rewound disk goes into a new folder next to the original, named `<volume>.rewind-<epoch>`. The latest disk is not changed. A rewind copies the whole disk, so it can take minutes (about 6 minutes for a 20 GiB and a 2 GiB disk in the lab).

**Expected:** the plan, then one line for each rewound disk. The tool prints `Warning: skipping ...rewind-...: metadata file not found` for these folders in later commands. This is harmless.

```
Preparing running VM disks (rolling back unfinished copies, rewinding where needed)...
  pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708 rewound into /pvcs/pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708.rewind-1793876775
  pvc-becf191e-0669-45da-b29f-c08d8708ad8b rewound into /pvcs/pvc-becf191e-0669-45da-b29f-c08d8708ad8b.rewind-1793876775
```

---

## Step 5: Make the volumes on the DR cluster

Each PV must name its folder by the path from the root of the share. The replicated volumes are inside the receiver volume's folder, so pass that path with `--dr-export-path`. The tool adds `/pvcs/pvc-<uuid>` to it.

```bash
export DR_EXPORT_PATH="${DR_EXPORT%/}${RECV_SUB}/repl"
```

The tool makes each PVC in the namespace it had on the source cluster, so make that namespace first (or pass `--namespace` to put every PVC in one namespace):

```bash
dr create namespace vms --dry-run=client -o yaml | dr apply -f -
```

### The newest restore point

```bash
failover execute --nfs-mount-path ${FAILOVER_PATH} \
  --dr-pool ${DR_POOL} --dr-share-ip ${DR_IP} --dr-export-path "${DR_EXPORT_PATH}" --dry-run

failover execute --nfs-mount-path ${FAILOVER_PATH} \
  --dr-pool ${DR_POOL} --dr-share-ip ${DR_IP} --dr-export-path "${DR_EXPORT_PATH}"
failover status
```

**Expected:** `Created SubVolume`, `Created PV` and `Created PVC` for each volume (the dry run shows three lines for each volume and makes nothing). The PV path ends in `.../repl/pvcs/pvc-<uuid>`. The status table shows each PVC as `Bound` after a few seconds. The PVs have the prefix `dr-`. The PVCs keep the namespace and name they had on the source cluster. Running `execute` again skips what exists.

```
[dry-run] Would create SubVolume pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708
[dry-run] Would create PV dr-pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708 (nfs://10.251.0.18/87ed0356_.../repl/pvcs/pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708, mount options nfsvers=4.1,soft,timeo=600,retrans=3,sec=sys)
[dry-run] Would create PVC tutf-vms/my-vm-rootdisk (bound to dr-pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708)
...
Created SubVolume pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708
Created PV dr-pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708
Created PVC tutf-vms/my-vm-rootdisk
...
SUBVOLUME                                 PVC             NAMESPACE  PVC PHASE  POLICY
pvc-7361d4f2-3a72-4eb3-911e-1afe41ce8708  my-vm-rootdisk  tutf-vms   Bound      tutf-vm-to-dr
pvc-becf191e-0669-45da-b29f-c08d8708ad8b  my-vm-datadisk  tutf-vms   Bound      tutf-vm-to-dr
```

The PVs carry the pool's NFS mount options, including `sec=sys`, and the PVCs name no StorageClass, so the cluster default class does not apply. Check: `dr get pv dr-pvc-<uuid> -o jsonpath='{.spec.mountOptions}'`. **Expected:** `["nfsvers=4.1","soft","timeo=600","retrans=3","sec=sys"]`.

### An earlier restore point (after Step 4)

Add `--rewind-to ${RESTORE_POINT}`. A dry run shows the rewound folder (`...pvc-<uuid>.rewind-<epoch>`) in each PV path and checks it the same way as a real run:

```bash
failover execute --nfs-mount-path ${FAILOVER_PATH} \
  --dr-pool ${DR_POOL} --dr-share-ip ${DR_IP} --dr-export-path "${DR_EXPORT_PATH}" \
  --rewind-to ${RESTORE_POINT} --dry-run

failover execute --nfs-mount-path ${FAILOVER_PATH} \
  --dr-pool ${DR_POOL} --dr-share-ip ${DR_IP} --dr-export-path "${DR_EXPORT_PATH}" \
  --rewind-to ${RESTORE_POINT}
```

Apply the current SubVolume CRD (`config/crd`) first: older ones refuse the `.rewind-<epoch>` folder.

---

## Step 6: Start the VM

Apply the VM definition on the DR cluster. Keep these in version control, or export them from the source cluster while it is alive. Remove `status`, `metadata.uid`, `metadata.resourceVersion` and the `macAddress` of the interface, and set `spec.runStrategy` to `Halted`, so the VM does not start before you say so. Every volume must name the PVC that you made above.

```bash
dr apply -n vms -f my-vm.yaml
virtctl start my-vm -n vms --kubeconfig ${DR_KUBECONFIG}
dr get vmi -n vms -w
```

**Expected:** `VM my-vm was scheduled to start`. In the lab the VM was `Running` with the guest agent connected 17 seconds later. The guest agent connects. The kernel log shows no file-system errors if the copy was `FileSystemConsistent`.

Check your data. For a VM that logs files, count the files and compare their checksums to the log. In a lab test with a VM that wrote a file and logged its checksum every 2 seconds, a VM started from the rewound disk had exactly the files up to the chosen restore point (and not the file written later), and a VM started from the newest restore point had all the files up to the last copy. All checksums matched. Both VMs started in under a minute. In a second lab test, the VM started from the newest restore point had the 50 MB file `/var/tmp/big` that was written before the last copy and the kernel log held no file-system errors.

> **After the VM runs, the disk no longer fits its restore points.** `failover execute --prepare-only --rewind-to ...` and `failover execute --rewind-to ...` now refuse with `Error: pvc-... has changed since its last copy (a VM has probably run on it), so its restore points no longer fit it and it cannot be rewound`. To try another restore point, fail over again from copies that no VM has used.

---

## Clean up

```bash
dr delete vm -n vms my-vm
dr delete pvc -n vms --all
sudo umount /mnt/dr      # only if you mounted the share on a host
```

The PVs that failover made have names that start with `dr-`. Their SubVolumes have the label `storage.ibmcloud.io/failover-source`. Both have the reclaim policy `Retain`, so deleting a PVC keeps the data on the share. Delete the PVs and SubVolumes last, when you no longer need the data. The data of the rewound disks is in the `.rewind-<epoch>` folders of the receiver volume. They are deleted with the receiver volume.

---

## What you learned

- `kubectl failover plan` shows the RPO and the restore point each VM will show.
- `--list-restore-points` and `--rewind-to` choose an earlier restore point.
- `--prepare-only` rewinds the disks before any VM runs on them.
- `execute` makes the PVCs. You apply the VM definition and start the VM.

## See also

- [Part 6: Replication and Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md) for plain volumes.
- [CROSS-REGION-DR.md](CROSS-REGION-DR.md), "Running VM disks".
