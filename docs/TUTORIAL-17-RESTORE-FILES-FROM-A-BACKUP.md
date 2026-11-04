# Part 17: Get Back One File from a Backup

> **Tutorial Series:** [Index](TUTORIAL.md) | [Part 10: Backup and Restore](TUTORIAL-10-BACKUP-AND-RESTORE.md) | **Part 17**

Look inside a backup, read one file, and get back a file that you deleted. You do this without restoring the whole volume. At the end, do the same inside the disk of a VM, without starting the VM. The words moment, restore point and snapshot pool are defined in [CONTEXT.md](CONTEXT.md).

**Time:** about 20 minutes, after Part 10 has two moments. The VM part adds about 15 minutes.
**Status:** file restore is Experimental. See [BACKUP-RECOVERY.md](BACKUP-RECOVERY.md#browse-a-backup-and-restore-files-in-development) for the full reference.

![A browse session reads one backup, and a file restore copies chosen files into a new PVC or a new folder of a PVC](images/tutorial-17-restore-files.drawio.svg)

---

## Prerequisites

- You finished Steps 1 to 6 of [Part 10](TUTORIAL-10-BACKUP-AND-RESTORE.md). The policy has backed up the volume `files` twice, so it has two moments, and the pod `files-writer` still runs. Do not do the clean up of Part 10 yet.
- The backup gateway is on (the chart default: `backupGateway.enabled` and `backupBrowse.enabled`). Check it:

```bash
oc get configmap backup-gateway-info -n kube-system
```

- The `kubectl-backup` plugin is on your path, as in Part 10.
- You are an admin of the namespace `pool-tutorial-backup`. The built-in `admin` role may browse and restore files with no setup. A user with only `view` or `edit` may not.

---

## Step 1: Set variables and list the moments

```bash
export NS=pool-tutorial-backup
export POLICY=tut-nightly

kubectl backup ls --policy ${POLICY} --pvc ${NS}/files --moments
```

**Expected:** a list of moments, newest first. The oldest is the one in `FIRST` in Part 10. If you start a new shell, read it again:

```bash
export FIRST=$(oc get backuppolicy ${POLICY} -o jsonpath='{.status.volumes[0].lastBackupTime}')
```

---

## Step 2: Look inside the backup

The first command opens a browse session. The driver starts a read-only helper pod for it, which takes a few seconds. Later commands reuse the session. The session ends after 15 minutes without use.

```bash
kubectl backup ls --policy ${POLICY} --pvc ${NS}/files
kubectl backup ls --policy ${POLICY} --pvc ${NS}/files /a
```

**Expected:** the first command shows `a`, `b` and `written`. The file `keep` is not there, because Part 10 deleted it before the second backup. The second command shows `1`.

Now look at the oldest moment:

```bash
kubectl backup ls --policy ${POLICY} --pvc ${NS}/files --moment ${FIRST}
```

**Expected:** `a`, `keep` and `written`. The file `b` is not there yet. This is the volume as it was at the first moment.

---

## Step 3: Download one file

```bash
mkdir -p /tmp/restored
kubectl backup get --policy ${POLICY} --pvc ${NS}/files --moment ${FIRST} /keep --out /tmp/restored/
cat /tmp/restored/keep
```

**Expected:** `first`. You got back a file that no longer exists on the volume. A file is saved under its own name. A file that exists is never replaced.

To read a file on the screen, use `--out -`:

```bash
kubectl backup get --policy ${POLICY} --pvc ${NS}/files /a/1 --out -
```

**Expected:** `second`. With no `--moment`, you read the latest copy. A folder is saved as a `.tar` file, or unpacked with `--extract DIRECTORY`.

---

## Step 4: Restore files into a new PVC

Download is for a person. To give the files back to a pod, restore them into a PVC. The driver makes a new PVC, sized to the files plus 20%, and copies the files in with a Job.

```bash
kubectl backup restore-files --policy ${POLICY} --pvc ${NS}/files --moment ${FIRST} \
  --path /keep --path /a --to ${NS}/files-back --wait
```

**Expected:** the command follows the restore and ends with `Done` and the counts: 2 files and 1 folder. The tree is kept, so the files are at `/keep` and `/a/1` in the new PVC.

Read them with a pod:

```bash
cat <<EOF | oc apply -n ${NS} -f -
apiVersion: v1
kind: Pod
metadata:
  name: files-back-check
spec:
  containers:
    - name: check
      image: registry.access.redhat.com/ubi9/ubi-minimal:latest
      command: ["/bin/bash", "-c", "find /data -type f | sort; cat /data/keep /data/a/1; sleep 600"]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: files-back
EOF

oc wait -n ${NS} --for=condition=Ready pod/files-back-check --timeout=300s
oc logs -n ${NS} files-back-check
```

**Expected:** `/data/a/1` and `/data/keep`, then `first` twice. The file `a/1` has the old content (`first`), not the content now on the volume (`second`).

A restore never writes over a file that exists. A claim name that exists is refused.

---

## Step 5: Restore files into a PVC that exists

Use `--existing` to put the files into a new folder of a PVC that you already use. Nothing else in that PVC changes.

```bash
kubectl backup restore-files --policy ${POLICY} --pvc ${NS}/files --moment ${FIRST} \
  --path /keep --to ${NS}/files --existing --wait
```

**Expected:** `Done`. The folder is named `restored-<policy>-<moment>` and is at the top of the claim. The command prints its name. Check it in the running pod:

```bash
oc exec -n ${NS} files-writer -- ls /data
oc exec -n ${NS} files-writer -- cat /data/restored-${POLICY}-${FIRST}/keep
```

**Expected:** the listing has the new folder next to `a`, `b` and `written`, and `cat` shows `first`. If the folder name differs, copy it from the `ls` output.

Your own `/data/a/1` and `/data/b` did not change. To put a file back in its old place, copy it from the restored folder yourself. The driver never does that for you.

---

## Step 6: Do it in the console

1. Open **Storage**, then **IBM VPC File Pools**, then the **Backups** tab.
2. Open the policy `tut-nightly`. In the menu of the volume `files`, choose **Browse files**.
3. The page shows "Starting the reader", then the folders. The picker above the table offers **Latest copy** and every moment. Choose the first moment.
4. Open `a`. Tick `1` and press **Download**, or tick files and folders and press **Restore selected**.
5. In the dialog, choose **A new claim**, type the name `files-console`, and confirm. The dialog shows the phase (Measuring, Copying the files) and then Done, with a link to the claim.

**Expected:** `oc get pvc -n pool-tutorial-backup files-console` shows `Bound`. Console restore asks the same questions as `restore-files`, with the same rights. Part 16 shows the rest of the Backups tab: [Part 16](TUTORIAL-16-CONSOLE-BACKUPS-CLONES-RECEIVER.md).

---

## Step 7: Files inside a VM disk

The backup of a VM disk is one raw disk. The driver reads the file systems inside it with a small read-only appliance, so you can get back one file of the guest without starting the VM.

This step needs a VM with a disk that a `BackupPolicy` backs up (see [Part 10](TUTORIAL-10-BACKUP-AND-RESTORE.md#vms)) and VM-disk browsing on in the install:

```bash
helm upgrade ibm-vpc-file-pool-csi charts/ibm-vpc-file-pool-csi -n kube-system \
  --reset-then-reuse-values \
  --set backupBrowse.guest.enabled=true \
  --set backupBrowse.guest.image.repository=REGISTRY/browse-guest \
  --set backupBrowse.guest.image.tag=TAG
```

Set `backupBrowse.guest.kvmDevice=true` where the nodes have the KVM device. A first listing then takes about 25 seconds instead of about 45. To browse or restore a VM disk that is in COS, `backupBrowse.cos` must also be on (the default).

Set the claim of the VM disk and list it. The first listing shows the file systems of the guest as folders:

```bash
export VMNS=vms
export DISK=my-vm-rootdisk

kubectl backup ls --policy ${POLICY} --pvc ${VMNS}/${DISK}
```

**Expected:** a few folders such as `sda1` or `rhel-root`, each with its type and label. A volume that cannot be read (BitLocker, LUKS, swap) is listed with the reason and cannot be opened.

Open a folder inside one file system, download a file, and restore a folder. `--fs` puts the file system name in front of the path:

```bash
kubectl backup ls  --policy ${POLICY} --pvc ${VMNS}/${DISK} --fs rhel-root /etc
kubectl backup get --policy ${POLICY} --pvc ${VMNS}/${DISK} --fs rhel-root /etc/hosts --out /tmp/restored/
kubectl backup restore-files --policy ${POLICY} --pvc ${VMNS}/${DISK} --fs rhel-root \
  --path /etc/hosts --to ${VMNS}/vm-files --wait
```

**Expected:** the folder list, then the file `/tmp/restored/hosts`, then `Done`. In the new claim the file is at `/rhel-root/etc/hosts`: the name of the file system is kept at the top.

Things to know:

- **The next backup of that disk waits** while a session is open, for at most 2 hours. A VM backup copies all its disks together, so they wait together. Close the session with `kubectl backup browse --close NAME` when you finish.
- **A running VM is backed up as a crash copy** unless the freeze worked. The command prints `note: ...` on standard error when a file system was read without its journal. The newest changes may be missing.
- **Speed.** Each folder you open and each file you read starts the appliance (about 1.5 seconds with KVM, about 12 without). Restore the files you need. To get back thousands of small files, restore the whole disk (Part 10).
- **From COS.** Add `--from-cos` to the same commands. Only the newest moment of an export can be read, and the export must stay in COS for at least 3 more hours.

---

## Who may do this

| You | Browse and restore files of namespace N |
|-----|------------------------------------------|
| Admin of N (the built-in `admin` role) | Yes, with no setup |
| A user with only `view` or `edit` in N | No |
| A user with a RoleBinding to `backup-file-reader` in N | Yes |
| Admin of another namespace | No |

To let a person read the backups of N:

```bash
oc create rolebinding backup-readers -n ${NS} --clusterrole=backup-file-reader --user=alice
```

A restore into a new claim also needs `create` on `persistentvolumeclaims` in the target namespace. A restore into an existing claim also needs `update` on `persistentvolumeclaims` and `create` on `pods` there. Every start, download and restore writes an audit Event that holds the user and the paths, never the contents.

---

## Clean up

```bash
oc delete pod -n ${NS} files-back-check --ignore-not-found --force --grace-period=0
oc delete pvc -n ${NS} files-back files-console --ignore-not-found
rm -rf /tmp/restored
```

Then clean up Part 10. A browse session ends by itself after 15 minutes without use.

---

## What you learned

- `kubectl backup ls` and `get` read one file from any moment, without a restore of the volume.
- `kubectl backup restore-files` copies chosen files into a new PVC, or into a new folder of a PVC that exists. It never writes over a file.
- The console does the same with **Browse files** and **Restore selected**.
- The disk of a VM is browsed and restored by file system, and the next backup of that disk waits while a session is open.
- The built-in `admin` role of the namespace has the right with no setup. Others need `backup-file-reader`.

## Next

- [Part 10: Backup and Restore](TUTORIAL-10-BACKUP-AND-RESTORE.md) for the whole-volume restore and the clean up.
- [Part 9: A COS bucket for backups](TUTORIAL-09-COS-BACKUP-BUCKET.md) to keep backups outside the VPC.
