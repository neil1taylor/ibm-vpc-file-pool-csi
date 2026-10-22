# Part 13: Failover of Running VMs

> **Tutorial Series:** [Index](TUTORIAL.md) | [Part 12: Replicating Running VMs](TUTORIAL-12-REPLICATING-RUNNING-VMS.md) | **Part 13**

Start a replicated VM on the destination (DR) cluster. Choose the moment it starts from: the newest one, or an earlier restore point. The words failover, rewind, moment and restore point are defined in [CONTEXT.md](CONTEXT.md).

**Time:** about 30 minutes, plus the time to copy a disk if you rewind.
**Before you start:** finish [Part 12](TUTORIAL-12-REPLICATING-RUNNING-VMS.md), so the destination holds copies of a VM with at least one good copy of every disk.

What the failover tool does and does not do:

- It reads the metadata that replication left on the destination share. It creates a SubVolume, a PersistentVolume and a PVC for each volume on the DR cluster.
- For a running VM, it picks the newest moment that all disks of the VM can show, so the disks fit each other. It rolls back a copy that stopped half way.
- It does **not** make the VM, start the workload or change DNS. You apply the VM definition yourself.

> **Rewind before you start the VM.** A VM that runs on the latest disk writes to it. After that, the disk no longer matches its restore points, and the tool refuses to rewind it. If you may want an earlier moment, rewind first.

---

## Prerequisites

- The destination cluster from Part 12: the receiver, the pool `dr-vm-pool`, and the volume `repl-destination-data` in `kube-system`.
- A host that can reach the destination share over NFS and reach the DR cluster's API. A VSI in the VPC works. The tool reads the replicated folders from a mounted share, so it must run where the share is mounted.
- The failover tool, built for that host:

```bash
make build-failover        # makes bin/kubectl-failover; set GOOS and GOARCH for another system
sudo cp bin/kubectl-failover /usr/local/bin/
kubectl failover version
```

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

## Step 2: Mount the destination share

Find the share's address and the folder of the receiver volume on the DR cluster:

```bash
export DR_IP=$(dr get filesharepool ${DR_POOL} -o jsonpath='{.status.shares[0].mountTargetIP}')
export DR_EXPORT=$(dr get filesharepool ${DR_POOL} -o jsonpath='{.status.shares[0].exportPath}')
export RECV_PV=$(dr get pvc -n kube-system repl-destination-data -o jsonpath='{.spec.volumeName}')
export RECV_SUB=$(dr get subvolume ${RECV_PV} -o jsonpath='{.spec.subPath}')
echo "${DR_IP} export='${DR_EXPORT}' receiver folder=${RECV_SUB}"
```

Mount the whole share on the host. The share needs `sec=sys`:

```bash
sudo mkdir -p /mnt/dr
sudo mount -t nfs4 -o nfsvers=4.1,sec=sys,soft,timeo=600,retrans=3 ${DR_IP}:${DR_EXPORT:-/} /mnt/dr
ls /mnt/dr${RECV_SUB}/repl/pvcs
```

**Expected:** one folder `pvc-<uuid>` for each replicated volume. In Part 12 the policy used `destinationBasePath: /repl`, so the failover path is the receiver folder plus `/repl`:

```bash
export FAILOVER_PATH=/mnt/dr${RECV_SUB}/repl
```

---

## Step 3: Plan the failover

```bash
kubectl failover plan --nfs-mount-path ${FAILOVER_PATH}
```

**Expected:** a table with one row for each volume. The columns are SubVolume, PVC, namespace, size, age of the last copy and policy. Then the RPO (the age of the oldest last copy) and a list of the VMs with the moment each will show:

```
Running VM disks
  VM vms/my-vm shows the moment 2026-10-22T11:15:00Z
    pvc-aaa...: in place
    pvc-bbb...: in place
```

`in place` means the disk already shows that moment, so it is used as it is. The tool refuses with "a copy is still being applied" if a copy is in progress. Wait for it to end, or add `--force`, which rolls the unfinished copy back first.

To use an earlier moment, list the moments that all disks of each VM can show (newest first):

```bash
kubectl failover plan --nfs-mount-path ${FAILOVER_PATH} --list-moments
```

The list reaches back as far as `restorePointKeepHours` of the policy (24 hours by default). Choose one:

```bash
export MOMENT=2026-10-22T10:45:00Z
kubectl failover plan --nfs-mount-path ${FAILOVER_PATH} --rewind-to ${MOMENT}
```

**Expected:** the plan now says `rewind (copies the whole disk into a new folder)` for the disks that are ahead of the moment. The tool uses the newest moment at or before `--rewind-to`.

---

## Step 4: Rewind the disks (optional)

Skip this step if the plan says `in place` for every disk and you want the newest moment.

```bash
kubectl failover execute --nfs-mount-path ${FAILOVER_PATH} --prepare-only --rewind-to ${MOMENT}
```

`--prepare-only` rolls back unfinished copies and rewinds the disks, then stops. It needs no cluster and no pool name. Each rewound disk goes into a new folder next to the original, named `<volume>.rewind-<epoch>`. The latest disk is not changed. A rewind copies the whole disk, so it can take minutes.

---

## Step 5: Run the dry run, then execute

Each PV must name its folder by the path from the root of the share. The replicated volumes are inside the receiver volume's folder, so pass that path with `--dr-export-path`. The tool adds `/pvcs/pvc-<uuid>` to it.

```bash
export DR_EXPORT_PATH="${DR_EXPORT%/}${RECV_SUB}/repl"
```

```bash
kubectl failover execute --nfs-mount-path ${FAILOVER_PATH} \
  --dr-pool ${DR_POOL} --dr-share-ip ${DR_IP} --dr-export-path "${DR_EXPORT_PATH}" \
  --rewind-to ${MOMENT} --kubeconfig ${DR_KUBECONFIG} --dry-run
```

**Expected:** for each volume, three lines: the SubVolume, the PV (with `nfs://` and the full path) and the PVC it binds to. Check that every PV path ends in `.../repl/pvcs/pvc-<uuid>` or `...rewind-<epoch>`. Nothing is made in a dry run.

Leave out `--rewind-to` if you want the newest moment. Run it for real:

```bash
kubectl failover execute --nfs-mount-path ${FAILOVER_PATH} \
  --dr-pool ${DR_POOL} --dr-share-ip ${DR_IP} --dr-export-path "${DR_EXPORT_PATH}" \
  --rewind-to ${MOMENT} --kubeconfig ${DR_KUBECONFIG}
kubectl failover status --kubeconfig ${DR_KUBECONFIG}
```

**Expected:** `Created SubVolume`, `Created PV` and `Created PVC` for each volume. The status table shows each PVC as `Bound`. The PVs have the prefix `dr-`. The PVCs keep the namespace and name they had on the source cluster. Running `execute` again skips what exists.

The tool creates the PVCs in the namespaces they had on the source cluster. Make sure those namespaces exist first, or pass `--namespace` to put them all in one namespace.

---

## Step 6: Start the VM

Apply the VM definition on the DR cluster. Keep these in version control, or export them from the source cluster while it is alive. Remove `status`, `metadata.uid`, `metadata.resourceVersion` and the `macAddress` of the interface. Every volume must name the PVC that the failover made.

```bash
dr create namespace vms --dry-run=client -o yaml | dr apply -f -
dr apply -n vms -f my-vm.yaml
virtctl start my-vm -n vms --kubeconfig ${DR_KUBECONFIG}
dr get vmi -n vms -w
```

**Expected:** the VM boots. The guest agent connects. The kernel log shows no file-system errors if the copy was `FileSystemConsistent`.

Check your data. For a VM that logs files, count the files and compare their checksums to the log.

> **After the VM runs, the disk no longer fits its restore points.** `kubectl failover execute --prepare-only --rewind-to ...` now refuses with "has changed since its last copy". To try another moment, fail over again from copies that no VM has used.

---

## Clean up

```bash
dr delete vm -n vms my-vm
dr delete pvc -n vms --all
sudo umount /mnt/dr
```

The PVs that failover made have names that start with `dr-`. Their SubVolumes have the label `storage.ibmcloud.io/failover-source`. Both have the reclaim policy `Retain`, so deleting a PVC keeps the data on the share. Delete the PVs and SubVolumes last, when you no longer need the data.

---

## What you learned

- `kubectl failover plan` shows the RPO and the moment each VM will show.
- `--list-moments` and `--rewind-to` choose an earlier restore point.
- `--prepare-only` rewinds the disks before any VM runs on them.
- `execute` makes the PVCs. You apply the VM definition and start the VM.

## See also

- [Part 6: Replication and Failover](TUTORIAL-06-REPLICATION-AND-FAILOVER.md) for plain volumes.
- [CROSS-REGION-DR.md](CROSS-REGION-DR.md), "Running VM disks".
