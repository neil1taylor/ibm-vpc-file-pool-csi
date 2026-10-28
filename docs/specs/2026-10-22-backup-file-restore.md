# Spec: browse a backup and restore single files (issue #92)

Date: 2026-10-22. Status: draft for review. It builds on `2026-10-21-backup-to-snapshot-pool.md` (step 8) and `2026-10-21-backup-cos-export.md` (step 9). Read both first. Facts here come from the code on `main` on 22 Oct 2026.

## Goal

The owner's goal is: **like Veeam**. Look inside a backup, pick one file or folder, and get it back, without restoring the whole volume.

When this is done:
- Plain volumes: a user lists folders and files of any backup moment, reads or downloads a file, and restores files or folders to a new or an existing claim. In the console and with `kubectl backup`.
- VM disks: the same, for the files inside the guest's file systems (XFS, ext4, NTFS, and LVM volumes inside the guest). The VM is not started and does not need to exist.
- Browsing is read-only. It never changes a backup, and it never writes to a VM disk.
- It works for backups in the snapshot pool and for backups exported to IBM Cloud Object Storage (COS).
- Who may browse follows Kubernetes access rules (RBAC).

## What is not reachable on this platform

Said first, so there is no surprise later. Size of each gap:

| Part of the Veeam experience | Can we meet it? | Gap |
|---|---|---|
| Plain volumes, local and COS | Yes, fully | None |
| VM guest: ext4, XFS, btrfs, NTFS, LVM | Yes, in a helper pod with no special rights, by running a tiny read-only appliance VM (see "VM disks"). Proven on the lab on 22 Oct 2026 (see "Phase 0 results") | A disk opens and a file reads in about 1.5 seconds with KVM, and about 12 seconds without it |
| Older moments of a VM disk, instantly | Yes, with a small read-only disk server that rebuilds the old view on the fly | New code, the riskiest part of this spec |
| VM guest file systems that need a key: BitLocker, LUKS | **No.** Without the guest's key nobody can read them, Veeam included | You can only restore the whole disk (works today) |
| VM guest: ReFS, Storage Spaces, ZFS | **No.** Linux has no safe reader for them | Same: whole disk only. Veeam reads ReFS through a Windows helper. We have none |
| Put a file back into the guest's own disk, in place | **No.** A file system that a VM is using must not be written from outside, and writing into a stopped guest from a pod is not safe either | The file goes to a claim or to a download. The user copies it into the VM |
| A mounted Windows disk that was not shut down cleanly (hibernate, fast start) | Partly. Read-only works. Recent writes may be missing | Shown as a warning on the session |

## What exists today (from the code)

- **Layout of a backup on the snapshot pool.** One folder per volume: `/backup/<policy>/pvcs/<volume>` holds the latest whole copy. Earlier moments are in `/backup/<policy>/.moments/pvcs/<volume>/`. For a plain volume a moment is a folder of hard links, named like `20261022T091500Z`. For a VM disk the latest copy is one raw file, `disk.img`, and earlier moments are "undo files" (the old contents of the blocks that changed). `replication.RestoreDisk` rebuilds an older disk by applying undo files to a copy of the latest. The status of each policy lists the volumes with their sub path, share and moment times (`BackupVolumeStatus`).
- **Restore today** is a whole volume: `BackupRestore` (cluster scoped) makes a new claim, holds pods back, and a Job copies the volume in. Nothing lists files.
- **Helper pods** are Kubernetes Jobs in the driver namespace (`kube-system` by default). They mount the NFS shares themselves, may mount them read-only (`jobShape.readOnly`), and run as root. The controller has no NFS access. On OpenShift, `kube-system` has no security rules applied, so these Jobs need no special SCC today (the chart comment says so, and the release tests prove it).
- **Path safety.** `util.ValidateBackupPath` and `ValidateBackupMomentsPath` check the shape of every backup path. The disk code opens files through a folder handle that refuses symbolic links (`openVolume`).
- **COS export.** A Job copies the backup folder and the moments folder of one volume into a Kopia repository in the bucket, encrypted with a backup key that the Job unlocks with Key Protect in memory. One policy has one area in the bucket. A restore from COS (`BackupRestore.spec.fromCOS`) runs a Job that restores one Kopia snapshot into a scratch folder and then reuses the normal restore Job.
- **Console.** The console plugin is a static page set served by nginx. It has no page for backups at all today (no `BackupPolicy` model, no list). It has no server side to stream a file.
- **KubeVirt images.** The controller already finds the virt-launcher image from virt-operator (`discoverLauncherImage`). The driver image has no `qemu-img` and no file system tools for guests.

## Design that meets the goal

Three parts: a **browse session** (a helper pod per open backup), a **gateway** (the one door that users talk to, which checks access), and a **file restore** (a Job that copies chosen paths out).

### 1. The browse session

A new cluster-scoped resource, `BackupBrowse`, describes one open session: the policy, the volume (`namespace/name` of the claim), the moment (default: latest), whether it reads the snapshot pool or COS, and for a VM disk which guest file system (see below). **Users never create or see this resource.** The gateway makes it after the access check. The backup controller (the same leader that runs backups) reconciles it:

1. It starts a **helper pod** (a Job with one pod, in the driver namespace) that mounts only this volume's folders, read-only, down to the NFS export. For COS it mounts nothing from NFS and gets the COS and Key Protect Secrets as read-only files, as the export Job does.
2. The pod runs the browse binary (`cmd/browse`, mode `browse-server`). It serves a small API on a private port **over TLS**, with a random session token and a certificate made for this session alone (the controller makes a key and a self-signed certificate, keeps them with the token in a Secret that the `BackupBrowse` owns, and mounts it into the pod; the gateway trusts exactly that certificate, no other and no CA): list a folder (paged, 1000 entries), stat, read a byte range, stream a file, stream a folder as a tar. The API takes paths **relative to the volume only**. It cannot be asked for anything outside it.
3. The controller writes `status.phase` (`Starting`, `Ready`, `Closed`, `Failed`), the pod address and a message. A session ends when it is idle for 15 minutes (default), when it has lived 2 hours (hard limit), or when the gateway closes it. Then the Job is deleted.

**Path safety in the helper** (the main risk for a plain volume, since a backup is attacker-supplied data):
- Symbolic links are listed as links, with their text, and are **never followed**. A download of a link is refused. A restore copies it as a link.
- Every open goes through a folder handle that refuses links and `..` (the same method as `openVolume`; Linux `openat2` with `RESOLVE_BENEATH`). A test uses a backup that holds a link to `/etc` and to `../../other-volume`.
- File names are sent as raw bytes encoded in the API, shown escaped in the console, and never put into a shell command. The restore Job gets paths as a list in a file, not as arguments.
- The pod has no service account token (`automountServiceAccountToken: false`), no host paths, no extra capabilities, a read-only root file system, and a NetworkPolicy that lets only the gateway reach its port, and traffic to it is encrypted with TLS (no plain HTTP in the cluster).

**Files that the pod cannot read.** NFS on VPC always squashes root to an ordinary user. A backup file with mode 0600 owned by another user may then be unreadable to the helper. This must be tested in Phase 1 on the lab before the design is called done. The list shows such files as "not readable" instead of failing. The same question applies to the existing restore Job, which copies as root; Phase 1 settles both.

**Idle and cleanup.** Every object of a session (the Job, its share volume and claim, the Secret) is named and labelled with the UID of the `BackupBrowse`, never its name, so a session deleted and made again with the same name cannot get the old session's Secret or pod. Pods carry a label and an owner reference to the `BackupBrowse`. The controller removes sessions past their limits, pods whose session is gone, and any session older than 2 hours after a restart. A limit on open sessions (default 5 in the cluster, 2 per namespace) makes extra requests wait with a clear message.

### 2. Plain volumes

The helper reads the latest copy at `.../pvcs/<volume>` or a moment folder under `.moments`. A moment folder is already a normal folder of files, so listing is instant. The list of moments comes from the policy status plus the folder names. A moment that the keep time removes while someone browses it gives "this moment was removed by the keep time", not a crash. The latest copy is rewritten by the next backup, one file at a time and with a rename, so a file is whole or old, never half-written; the page says "the latest copy may change while you look".

### 3. VM disks

A backup of a VM disk is one raw disk. The file systems are inside it, so something must understand partitions, LVM and the guest's file systems, without starting the VM and without root on the node.

**Recommended: a small read-only appliance VM inside the helper pod (libguestfs).** libguestfs is the library that KubeVirt's own `virtctl guestfs` and Red Hat's VM migration tool use. It starts a tiny VM with its own Linux kernel, attaches the disk read-only, and that kernel reads XFS, ext4, btrfs, NTFS (read-only) and LVM. The pod needs **no extra rights**: no privileged mode, no loop devices, no `CAP_SYS_ADMIN`, no FUSE, no host paths. It runs as root only inside its own pod, like today's Jobs, and works under the OpenShift rules if the driver namespace has them (see "OpenShift rules"). With the KVM device (`devices.kubevirt.io/kvm`, the device KubeVirt already uses) the appliance is fast. Without KVM (a ROKS cluster with virtual workers) it works with software emulation, slower. Both were tested in Phase 0 (see "Phase 0 results").

How it is wired:
1. **A disk server (new code).** The pod starts a read-only disk server in Go that shows the VM disk to the appliance as a network block device on a local socket. For the latest moment it reads `disk.img`. For an older moment it reads `disk.img` and the undo files newer than that moment, newest first, and returns the old contents of each changed block. This is the same rule as `Rewind`, but without writing a copy. No scratch space and no wait, however big the disk is. The server only answers reads; its export is marked read-only.
2. **The appliance** gets that device as a read-only drive and mounts the guest file systems read-only. libguestfs gives the list: partitions, LVM volumes, type, size, and which looks like the system volume. The console shows them as a picker.
3. **The browse API** in the pod asks the appliance for lists and file contents and relays them in the same API as a plain volume. So the console and the command line see one kind of session.

Why not the cheaper routes:
- **Restore the whole disk to a scratch claim, then look inside** (`RestoreDisk` plus a mount). *Does not meet the goal:* it restores the whole volume, which is what the owner asked to avoid. A 500 GB disk takes minutes to hours and needs 500 GB of scratch.
- **Mount the disk with a loop device in a privileged pod.** *Meets the goal for ext4 and XFS only if the node allows it, but it is a security risk:* a privileged pod, with a guest kernel-parser bug running in the node's kernel. A hostile VM disk can then reach the node. We will not use it.
- **Pure Go readers (go-ext4, go-ntfs, go-xfs).** *Partly meets the goal:* no appliance, faster start, but each file system needs a separate, immature reader, none does LVM, and we would own their bugs. It is a possible later fast path for ext4 only.

**Safety of reading a guest file system.** The disk is read-only at four layers: the NFS mount, the disk server, the appliance's drive flag, and the mount flag. The appliance is thrown away when the session ends. A broken or hostile file system can crash the appliance, not the node. The appliance has no network.

**Dirty file systems.** A backup of a running VM is a crash copy unless the freeze worked (the status says which: `FileSystemConsistent` or crash-consistent). A read-only mount of a crash copy may not be allowed to replay the journal. The helper then mounts with the "do not replay" option (`norecovery` for XFS, `noload` for ext4). The page shows a notice: "this copy was taken while the VM was running; the newest changes may be missing." Phase 0 showed that a read-only XFS mount with `norecovery` works in the appliance.

**Not read: encrypted and unusual file systems.** If the helper finds BitLocker, LUKS, ReFS, Storage Spaces or ZFS it lists the volume as "cannot be read here" with the reason. Nothing is skipped silently.

**Backups and browsing of the same VM disk.** The next backup copy writes into `disk.img` and adds undo files. A session on a VM disk holds a **lease** on that volume, so the backup controller waits for that volume's next copy while a session is open, for at most 2 hours (the hard limit of a session). The policy status shows "waiting for a browse session". Reason: a half-applied copy has the disk and the undo files out of step, and the disk server must see a steady state. Cost: a backup of that one volume can be late by up to 2 hours. Other volumes are not held back. (A later version could let the disk server follow a copy in progress, as `Recover` does, and remove the wait. It is not needed for the goal.)

### 4. Backups in COS

COS backups are in a Kopia repository. The helper opens it **read-only** with the same Secrets the export Job uses: the backup key is unlocked by Key Protect in the helper's memory, as in step 9. The controller never holds it.

- **Plain volumes.** Kopia lists a folder in a snapshot and fetches the chunks of one file, so listing and getting one file does not download the volume. *Meets the goal fully.*
- **VM disks.** The disk server reads `disk.img` and the undo files as objects in the repository, by byte range, with a read cache on an `emptyDir` (default 5 GiB). The rest of the path is the same as for the snapshot pool. *Meets the goal*, with slower reads than the pool (each read may go to COS); a guest start of several minutes is likely. To be measured in Phase 6.
- **Where Kopia comes from.** The step 9 spec keeps Kopia out of the controller. This is kept: the helper is a **separate binary** (`cmd/browse`) in its own image, not the controller. For plain volumes it runs the pinned Kopia program as a child, as the export Job does. For VM disks, a range read needs Kopia as a Go library in that separate binary. This is the one new dependency, and it is limited to the browse image. If Phase 6 shows the library is not workable, the fall-back is to fetch `disk.img` and the undo files of the one moment into scratch space first. *That fall-back does not meet the goal for large disks*, and we would say so in the product.
- **Whole area of the bucket.** The helper's credentials can read the whole policy area, which has other namespaces' volumes. The API accepts only the one volume's path, and a test tries to reach another volume with traversal and with crafted snapshot names. This is the weakest point of COS browsing and is tested hard (see "Tests").
- **A new cluster, no policy.** `kubectl backup browse --from-cos` takes the same flags as `restore --from-cos` (bucket, endpoint, volume). It is for cluster administrators because it needs the Secrets in the driver namespace.

### 5. Where restored files go

A new resource, `BackupFileRestore`, cluster scoped like `BackupRestore`, made by the gateway for the user. It holds a session reference or the volume and moment, a list of paths, and the target. Three targets:

1. **New claim.** The controller makes a new claim (same method as `BackupRestore`: size, class, held back from pods until the copy ends) and a Job copies the chosen paths into it. The tree under the volume's root is kept: restoring `/var/www/html/index.php` gives `/var/www/html/index.php` in the new claim. Size of the claim defaults to the total of the chosen files plus 20%, at least 1 GiB, and the user can raise it. For a VM guest the claim is a plain file-system claim; a VM disk is never the target.
2. **Existing claim, as a new folder.** Files go to a new folder in the claim, `restored-<policy>-<moment>/`, never over existing files. A name that exists gets a suffix. The Job mounts the claim's folder on the share, like the other share Jobs. It refuses if the claim is a VM disk claim (it holds `disk.img`), if the claim is being restored, or if the free space is not enough (checked before it copies). The files get the claim's configured owner (`defaultUID` and `defaultGID`, for example 107 on KubeVirt pools), as the existing restore does.
3. **Download.** From the console or `kubectl backup get`: one file as a stream; a folder or several files as one tar stream (a tar file; no zip). The stream passes through the gateway and the helper. Limits are in the next section.

**Putting a file back where it was** (in place, over the original) is not offered, for plain volumes either. A restore never writes over existing files. This matches step 8 ("it never writes over a volume that exists"). The user copies from the restored folder.

For a VM guest, targets 1 and 2 give plain files in a claim. The user attaches that claim to the VM or a pod and copies in the guest. This is the gap in the table.

### 6. The gateway and access control

**One door.** A new small Deployment, `backup-gateway`, in the driver namespace. The console reaches it through the console plugin proxy (the OpenShift console forwards the signed-in user's token). `kubectl backup` reaches it through an OpenShift Route (or an Ingress on other clusters) with the user's token. Users never need rights in the driver namespace, and never see the helper pods, `BackupBrowse` or the Secrets.

**Who may do what.** The gateway checks every call with the Kubernetes API, as the user, by a token review and a `SubjectAccessReview`. The rule is by the **namespace of the backed-up claim**, because that is who owned the data:

| Action | Needs |
|---|---|
| List backups and moments of volumes of namespace N | `get` on `backupfiles` (group `storage.ibmcloud.io`) in N |
| Browse a backup, read and download files of N | `get` on `backupfiles` in N |
| Restore files out of N's backup to a claim | `get` on `backupfiles` in N, plus `create` on `persistentvolumeclaims` in the target namespace T. For an existing claim, also `update` on `persistentvolumeclaims` in T |
| Browse a COS backup without a policy (`--from-cos`) | cluster administrator (it needs the driver's Secrets) |

`backupfiles` is a **rights-only name**: no such object is ever stored. It exists so that RBAC can say "may read backups of this namespace". The chart ships two ClusterRoles, `backup-file-reader` (get) and `backup-file-restorer` (get), and aggregates the reader into the built-in `admin` role, so a namespace admin can read their own namespace's backups with no setup. Another namespace's backup needs a RoleBinding made by someone who has rights in that namespace. A deleted claim's backup still belongs to its old namespace. If the namespace itself is gone, only a cluster administrator can browse it.

A person in namespace B who wants a file from the backup of namespace A, to put into B, needs `get backupfiles` in A and the PVC rights in B. Without the first, they get a plain "forbidden", the same as any other Kubernetes object. The gateway never shows the existence of volumes in a namespace to a person without rights there.

**Audit.** Each session start, download, and restore writes a Kubernetes Event on the `BackupBrowse` and a log line: user, namespace, volume, moment, and the **paths** (not the contents). A user cannot read the Events of the driver namespace; the cluster administrator can. A test checks that no API key, token or file content reaches logs or Events.

### 7. Limits

| Limit | Default | Why |
|---|---|---|
| Folder listing page | 1000 entries | Large folders stay fast |
| Session idle time | 15 min | Frees the pod and the lease |
| Session hard limit | 2 hours | Stops a forgotten session from holding backups |
| Open sessions | 5 in the cluster, 2 per namespace | Each VM session used about 550 MiB in the lab (Phase 0) |
| Helper pod | plain: 200m CPU, 256 MiB; VM guest: 1 CPU, 1 GiB | Appliance memory (lab peak 550 MiB) |
| One file download | 50 GiB | Stream, no temporary copy |
| One restore | 200 GiB or the free space of the target, whichever is less | The Job checks first |
| Appliance start | 10 minutes, then the session fails with the reason | Software emulation is slow |
| Restore Job time | 4 hours | A hard stop for a stuck copy |

All defaults are Helm values (`backupBrowse.*`) and are listed in `HELM-VALUES.md` with the phase that adds them.

### 8. User stories

Console:
1. As a namespace admin, I open **Backups**, pick a policy, see my volumes, pick a volume and a moment, and press **Browse files**. A plain volume opens at once. A VM disk shows "Starting the reader" for a few seconds and then a picker of the disk's file systems.
2. I click into folders, search the current folder by name, tick files and folders, and press **Download** (one file, or a tar for several) or **Restore to a claim** (new claim, or a folder in an existing one).
3. I see the status of the restore (copying, done, failed) and a link to the new claim.
4. A VM backup shows the notice about a crash copy, and a lock icon on volumes that cannot be read, with the reason.

Command line (`kubectl backup`, same plugin as today):
- `kubectl backup ls --policy P --pvc NS/NAME [--moment TIME] [--disk N] [--fs /dev/sda2] [PATH]`, lists a folder. For a VM disk without `--fs` it lists the file systems it found.
- `kubectl backup get --policy P --pvc NS/NAME [--moment TIME] [--fs ...] PATH [--out DIR]` downloads a file, or a folder as a tar or an unpacked folder.
- `kubectl backup restore-files --policy P --pvc NS/NAME [--moment TIME] [--fs ...] --path A --path B --to NS/CLAIM [--new-claim --size-gb N | --existing] [--wait]`.
- `kubectl backup browse ...` opens a session by hand and prints its name, so repeated `ls` and `get` calls reuse it. Without it each command reuses an open session of the same user, volume and moment, or opens one.
- The same commands take `--from-cos --bucket B --endpoint HOST --volume pvc-...` for a cluster administrator.

## OpenShift rules (security context constraints)

- **Plain volume helper:** runs as root in its pod with all capabilities dropped, on a read-only NFS mount. No special rule. It is the same as today's share Jobs.
- **VM guest helper:** also unprivileged. It asks for the KVM device through the KubeVirt device plugin (`devices.kubevirt.io/kvm: 1`) when the node has it. Without it, no request, and software emulation. It needs a writable `/tmp` and `/var/tmp` (an `emptyDir` with a size limit). It needs no SCC beyond what the driver namespace already allows.
- **If the driver is installed in a namespace where OpenShift does apply SCCs** (not `kube-system`), the chart ships one SCC, `pool-csi-browse`, that allows running as root with no capabilities and no host access, and the controller gets `use` on it only. The release test checks that this SCC and the pod pass admission.
- **Image.** The VM helper image is our own image, built from CentOS Stream 9 with the `libguestfs`, `libguestfs-winsupport` and `guestfs-tools` packages, pinned by digest, with our static Go binary copied in (`CGO_ENABLED=0`). Phase 0 ruled out the libguestfs image of OpenShift Virtualization: it has no NTFS support. It is not built from UBI: UBI repositories do not have the qemu and libguestfs packages (the same reason the converter pod uses the CentOS image).
- **NTFS rule.** The RHEL and CentOS builds of libguestfs read NTFS only when one of the `virt-` tools runs it (for example `virt-ls` and `virt-cat`). A program that uses the library directly gets "unsupported filesystem type". So the helper reads Windows disks by running `virt-ls` and `virt-cat`, as the packaging intends. It must not pretend to be one of those tools.
- **Never:** privileged pods, `hostPath`, loop devices, FUSE, or kernel mounts of a guest file system on the node.

## Tests

- **Unit tests (every phase).** The disk server against `Rewind`: for each moment, a read must equal `RestoreDisk` byte for byte (property test, random writes, several moments, an unfinished copy ignored). Path rules: links, `..`, very long names, names with newlines and invalid UTF-8, a link to the helper's own files. Gateway: every row of the access table, with a fake `SubjectAccessReview`, including "forbidden gives no hint that the volume exists". Controller: session start, idle timeout, hard limit, limits on open sessions, orphan cleanup after a restart, the lease on VM volumes. `BackupFileRestore`: all three targets, a name clash, a target that is a VM disk claim, not enough space.
- **COS.** Kopia against a local repository and MinIO in CI: list, one file, one volume's path only, a hostile snapshot name, a range read of a large file, the cache size.
- **Chart tests (`make test-scripts`).** The gateway and its NetworkPolicy render; the two ClusterRoles and the aggregation; the SCC only when asked for; the console proxy entry; Helm values documented.
- **Console tests.** The new pages: list, breadcrumb, selection, the picker, disabled states, the warning texts; no file content is put into a URL.
- **Release test, new stage 20** (stage 19 is the instant snapshots stage) in `test/e2e`, run on the lab pair:
  1. Plain pod volume with a known tree (links, a 0600 file owned by another user, a name with spaces and an accented letter). Two backup moments. Browse both, download a file and check its checksum, restore a folder to a new claim and to an existing one, and compare checksums. Check that the backup folder is byte-identical before and after (a recorded hash of the tree).
  2. Linux VM with an XFS root on LVM and an ext4 data disk, with a log of files and checksums (as stage 18). Browse the latest and the first moment without starting the VM; stop the VM first for one run. Restore a file; compare. Check the disk image is byte-identical after.
  3. A Windows or NTFS disk image (a small prepared image; the lab has no Windows licence, so the image is made with `ntfs-3g` tools and checked as a disk, not as a boot).
  4. COS: browse and restore the same plain volume and the VM disk from the bucket of stage 18, in the second cluster of the pair.
  5. Access: a test user with only a RoleBinding in the other namespace gets "forbidden", gets access after a binding, and loses it after the binding is removed.
  6. Cleanup: no helper pod, `BackupBrowse` or scratch left after the run, and a killed controller leaves none after a restart.
  7. A backup due during a VM session waits and then runs after it closes.
- **What the lab cannot test:** a customer's own Windows disks, BitLocker/ReFS (checked only that they are reported as unreadable, using a small image header), and the speed of very large disks. The report says so.

## Build plan: phases, one PR each

Sizes: S is a day or less of work, M a few days, L a week or more. Each PR has its tests, updates `FEATURES.md`, `BACKUP-RECOVERY.md`, `HELM-VALUES.md`, `CRD-SPEC.md`, `MONITORING.md` and `CHANGELOG.md`, and extends stage 20 for its part.

0. **Spike on the lab (S, risk check).** Run the libguestfs image in a pod on ROVS with and without the KVM device. Measure: start time, listing, reading a file, memory. Try XFS, ext4, NTFS, LVM, a crash copy, a hibernated NTFS, and a disk served over a socket from a small test server. Decide the image and how to find it. Results are added to this spec in a PR that holds no product code. **If it fails (for example the appliance cannot start in a pod under the cluster's rules), the VM part of the goal is re-stated with the real gap before more is built.**
1. **Plain volumes, snapshot pool, command line (L).** `BackupBrowse`, the helper in `cmd/browse` and its image, the controller part, the gateway, the access rules and ClusterRoles, `kubectl backup ls/get/browse`, limits, cleanup, audit Events. Settles the "squashed root cannot read 0600 files" question.
2. **Restore of files (M).** `BackupFileRestore`, the Job, the three targets, `kubectl backup restore-files`, the space check, ownership.
3. **Console (L).** The file browser, download, the file restore dialog, help texts and the plugin proxy entry. The Backups pages themselves (policies, volumes, moments) come from `2026-10-22-console-backup-ui.md`, which reserves a "Browse files" action on each volume row and moment for this phase. Build those pages first.
4. **COS, plain volumes (M).** Kopia read-only helper, `--from-cos` for ls/get/restore-files, the tests that block reaching another volume.
5. **VM disks in the snapshot pool (L, the riskiest).** The disk server with undo-file rebuild and its property tests; the lease and the backup wait; the appliance helper; the file system picker; `norecovery` handling; the console picker; the crash-copy notices; the unreadable-volume list.
6. **VM disks in COS (M).** Kopia library range reads and the cache; speed measurements; the fall-back decision if needed.
7. **Release test stage 20 final and docs (S).** The stage is extended per phase; this PR makes it one run for the lab pair, adds the two-cluster COS run and the cleanup checks, and writes the user tutorial.

Total: about 4 to 6 weeks of agent work, with the lab as the slowest part (stage 20 runs about an hour). Phases 1 to 4 deliver the whole plain-volume goal and can be deployed and tested before phases 5 and 6 are done.

## Cost and risk

- **Biggest technical risk:** the VM path (phases 5 and 6). It has three new parts: the disk server, the appliance in a pod, and the COS range reads. Phase 0 removed the first doubt: the appliance runs in an ordinary pod on ROVS. If that ever changes, the fall-back is a helper that restores one volume to scratch space and then reads it. That does not meet the goal; the gap would be shown to you before we build it.
- **Security risk:** reading hostile data (a backup of a compromised VM). The appliance is the main defence. For plain volumes the risk is path tricks, handled by handle-based opens and tests.
- **Backups held back:** a VM session delays that volume's next backup by up to 2 hours. A noisy user could do it repeatedly; the per-namespace limit and the audit trail bound it.
- **Resources:** each VM session takes about 2 CPU and 2 GiB on a worker for its life. The limit of 5 open sessions bounds it.
- **COS cost:** reads from COS cost egress and requests. Browsing one file is cheap; browsing a VM disk reads only the blocks the file system asks for, but a first mount reads a lot of metadata. Measured in phase 6 and added here.
- **New dependencies:** the libguestfs image (pinned, from Red Hat's own OpenShift Virtualization release) and, in phase 6 only, Kopia as a library in the browse binary. Nothing new in the controller.
- **Time to first value:** the end of phase 2 gives command-line browse and restore of plain volumes.

## Phase 0 results (22 Oct 2026, rovslab-dr-eude)

The test ran in a new namespace, in a pod with no special rights: a random non-root user, all capabilities dropped, no privilege escalation, the default seccomp profile, and only `/tmp` and `/var/tmp` as `emptyDir` folders. The pod asked for the KVM device (`devices.kubevirt.io/kvm: 1`). The workers are bare-metal `bx2d.metal.96x384` nodes, each with the KVM device.

| Check | Result |
|---|---|
| The appliance starts in an ordinary pod | Yes. No privileged mode, no host paths, no extra rights |
| Open a disk, mount read-only and read one file, with KVM | About 1.3 to 1.6 seconds for XFS, ext4 on LVM, and LVM listing |
| The same without KVM (software only) | About 12 seconds |
| Memory | The appliance used about 260 MiB; the whole container peaked at about 550 MiB |
| XFS, ext4, LVM | Read correctly with the OpenShift Virtualization image |
| NTFS with the OpenShift Virtualization image | **No.** The image has no NTFS support: no `libguestfs-winsupport`, no `ntfs-3g`, and its kernel has no NTFS driver |
| NTFS with CentOS Stream 9 packages (`libguestfs-winsupport`) | Yes, through `virt-ls` and `virt-cat`. A direct library call is refused by design ("unsupported filesystem type") |
| `virt-copy-out` and `virt-tar-out` on an NTFS data disk with no operating system | No: they need to find an operating system. Use `virt-ls` and `virt-cat` per file for data disks |
| Read-only XFS mount with `norecovery` | Works |
| Disk served over a local socket by `nbdkit -r` | Reads work. A write through it is refused |

What this changes in the spec: the VM path is proven to start quickly; the helper image is our own CentOS Stream 9 image; Windows disks go through the `virt-` tools; the VM helper needs 1 CPU and 1 GiB, not 2 and 2.

Not yet tested: a real Windows disk with BitLocker or a hibernated system, btrfs, and very large disks. Phase 5 covers them.

## Decisions I made for you

- Users talk to one small gateway, never to helper pods or driver resources. The gateway checks Kubernetes access on every call.
- Access follows the namespace of the backed-up claim, through a rights-only name `backupfiles`. Namespace admins get read access to their own namespace by default; everything else needs a RoleBinding.
- Restoring a file out needs read rights on the source namespace and create rights on the target namespace. No new super-user role.
- Browsing from COS without a policy is for cluster administrators only.
- A VM disk is browsed through a small read-only appliance VM in an unprivileged pod, not through a privileged mount on the node. We do not use privileged pods for this.
- Older moments of a VM disk are shown by a read-only disk server that rebuilds the old view on the fly. We do not restore a scratch copy, because that would restore the whole volume.
- A VM session holds the next backup of that one volume for up to 2 hours. Other volumes are not held.
- Files are never put back in place, and nothing is ever written into a VM's own disk. Files go to a new claim, to a new folder in an existing claim, or to a download.
- A restore to an existing claim goes into a new folder and never over existing files; it refuses a VM disk claim.
- BitLocker, LUKS, ReFS, Storage Spaces and ZFS guests are listed as "cannot be read here". Their only route is the whole-disk restore that exists.
- Symbolic links in a backup are shown and copied as links and never followed.
- Defaults: 15 minutes idle, 2 hours hard limit, 5 sessions in the cluster and 2 per namespace, 50 GiB for one download, a listing page of 1000 entries.
- The browse helper is a separate binary and image. Kopia stays out of the controller. Kopia as a library is allowed in the browse image only.
- A short spike (phase 0) came before any VM work. It passed on 22 Oct 2026.
- The VM helper image is built by us from CentOS Stream 9, because the OpenShift Virtualization libguestfs image cannot read NTFS.
- Windows disks are read through the `virt-ls` and `virt-cat` tools, which is the use the packaging allows.
- The Backups pages in the console are built by the backup screens spec (`2026-10-22-console-backup-ui.md`). This work adds the file browser to them.
- Plain volumes are finished and usable (phases 1 to 4) before VM disks start.
- No restore of single mail items or database rows (Veeam's application-level explorers). Files and folders only.

## Needs from you

- **Nothing blocks phases 1 to 4.** Phase 0 is done.
- Whether phases 5 and 6 (VM disks) must be in the first deployment, or can follow it. I assumed they follow.

## Changes made while building (23 Oct 2026)

- The helper API is served over TLS with a per-session self-signed certificate, pinned by the gateway. Decided by the coordinator after a security review: the stream carries other namespaces' file contents and the token, and pod traffic is not encrypted by default.
- Objects of a session are named by the UID of the `BackupBrowse`, not its name (security review: a reused name must never reuse an old Secret or pod).
- The release test stage is 20, not 19.
- The helper image is separate; the gateway runs in the driver image.
- Helper pods, their Secrets and the NetworkPolicy live in their own namespace, `ibm-vpc-file-pool-csi-browse` (chart value `backupBrowse.namespace`), which holds nothing else. Reason (security review of part 3): the gateway must read the session Secrets, and the driver namespace (`kube-system`) holds the cluster's IBM Cloud API key and other Secrets that a compromised gateway could then read. The gateway's Role for Secrets is in the browse namespace only; the controller has a Role there, and copies the image pull secrets into it. The helper needs root (to read backups made by other users), so the namespace is labelled Pod Security `baseline` (not `restricted`), and on OpenShift the chart adds one SCC (`runAsUser: RunAsAny`, all capabilities dropped, no privilege escalation, no host path, only PVC/secret/projected/emptyDir/configMap/downwardAPI volumes) for the namespace's default account. This replaces the earlier text that said the controller gets `use` on the SCC.
- The gateway reads the rights as `get backupfiles` on the claim's namespace and runs in the driver image (`--mode=backup-gateway`). Users reach it through a reencrypt Route on OpenShift (a passthrough Route would show the service certificate, which is for the service name and not for the Route host); `kubectl backup` finds the address in the ConfigMap `backup-gateway-info`.
- The audit Events are written in the driver namespace on the `BackupBrowse` (a cluster-scoped object).
- A session belongs to the user who made it; another user, even with the same rights, sees "no such session".
- The helper has a `/v1/moments` call so that `kubectl backup ls --moments` can list the moment folders.
- The console file browser (phase 3, first half) downloads with a GET route on the gateway, `GET /v1/sessions/{name}/download?type=file|tar&p=<base64 name>...`, because a browser can stream a large download to disk only from a GET. It takes the same steps as the POST calls. Every file and tar reply now carries `Content-Disposition: attachment`, `nosniff`, a sandbox Content-Security-Policy and `no-store`, since the console proxy serves it from the console's own origin. The console shows the action when the settings ConfigMap says `fileBrowse: "true"`.

## Changes made while building phase 2 (28 Oct 2026)

- The restore Job is the driver program (`--mode=restore-files`, package `pkg/filerestore`), not a shell script with `cp -a`. It reads the backup through the browse helper's handle-based reader and writes the target the same way (`mkdirat`, `symlinkat`, `openat` with `O_EXCL` and `O_NOFOLLOW`, no link followed). Reason: an existing claim is open to its users, so the target is as untrusted as the backup.
- Paths are `spec.paths`, a list of escaped strings (the form `kubectl backup ls` prints), kept in a ConfigMap that the restore owns and mounted as a file. At most 1000 paths, 256 KiB together.
- The squashed-root question, write side: the order is times, then mode, then owner for a file, and a folder is finished only after everything in it is written (like `cp -a`, which the lab proved). The owner is the one the pool configures for the claim (`defaultUID`, `defaultGID`); without one, the backup's owner is kept. If an owner cannot be set, the restore fails and removes what it wrote. Stage 20 part 4 checks both cases on the lab (a pool with owner 107 and a pool with none, files of users 1000 and 2000).
- Into an existing claim the Job writes a hidden folder `.restoring-<restore UID>` and renames it to `restored-<policy>-<moment>` (a suffix if the name exists). NFS has no rename that refuses to replace; the name is checked just before.
- The Job mounts the whole share that holds the target claim (the claim's folder may not exist before the Job, and a folder that does not exist cannot be mounted), and opens only the claim's folder below it, one name at a time. An existing claim whose folder does not exist yet is refused.
- A new claim is sized by a read-only measure Job first (skipped when `sizeGB` is given), because the controller has no NFS access.
- The claim's UID is pinned (`spec.targetUID`, set by the gateway; otherwise the first UID the controller sees), so a claim deleted and made again is refused. The new claim made for a restore carries the restore's name and UID as annotations, and both must match.
- The Job keeps only the `CHOWN` and `FOWNER` capabilities. It has a 4 hour deadline.
- Limit of one restore: the least of 200 GiB (`backupBrowse.restoreMaxBytes`), the free space of the share, and, for an existing claim, its size less what it holds.
- The setuid and setgid bits are removed from everything restored.
- At most 5 file restores run at once in the cluster (`backupBrowse.restoreMaxRunning`); the others wait, oldest first. Without a limit, any user with the rights could start unlimited Jobs that mount shares.
- A refusal by the Job (no room, a VM disk claim, a missing path, an owner it cannot set) ends the Job with exit code 0 and the reason in its termination message. A Job that failed with a non-zero code is started again by Kubernetes in the same pod, which loses the message. The controller reads `error` in the outcome as a failed restore. Only a Job that was stopped, or ran out of its 4 hours, ends as a failed Job, and then the reason is "the copy Job failed or ran out of time".
- One rare repeat can happen: a Job's objects are removed when it succeeds, before the controller saves `Done`. If that save fails, the next pass runs the Job again, and an existing claim gets a second folder (`-2`). Nothing is lost or written over.
- The gateway call is `POST /v1/restores` (answer 202) and `GET /v1/restores/{name}`. It checks `get backupfiles` in the source namespace first, then `create` on claims in the target namespace (and, for an existing claim, `update` on claims and `create` on pods: writing into a claim is the same power as mounting it), and only then looks at the target claim and takes its UID. Paths are stored in one canonical escaped form. A restore is shown only to the user who asked. A user may have 10 restores that have not ended (`backupGateway.maxOpenRestores`).
- `kubectl backup restore-files` defaults to a new claim; `--existing` chooses a new folder in an existing claim.

