# Spec: export backups to IBM Cloud Object Storage (COS)

Date: 2026-10-21. Status: draft for review. This is step 9 of `2026-10-20-vm-safe-replication.md`. It builds on `2026-10-21-backup-to-snapshot-pool.md` (step 8, built and merged). Read both first. The main spec already sets the design (Kopia, Key Protect, Compliance lock). This spec turns it into things we can build and test.

## Goal

When this step is done:
- A `BackupPolicy` can send its backups to a COS bucket, as well as keep them in the snapshot pool.
- The copy in COS is encrypted before it leaves the cluster, and nobody can delete or change it before its keep time ends. That includes a person who has taken over the cluster.
- If the VPC, the cluster and both regions lose their pools, you can restore a volume from COS into a new cluster that runs the driver.
- Sending to COS never slows a running VM. It reads from the snapshot pool, not from the VM.

## What you see

**On the policy.** A new optional block `spec.export`:

- `bucket`, `endpoint` and `prefix` (the bucket and where it is)
- `credentialsSecret` (the driver's COS key, made by the setup command)
- `keepFor` (how long COS backups are kept, default `30d`) and `lock` (`Compliance` or `None`, default `Compliance`). Both are set on each policy, so a test policy can use a short lock and a real policy a long one. The webhook refuses a keep time below the minimum (see "The shortest lock")
- `schedule` (how often to send, default after each local backup moment)

Without `export`, nothing changes from step 8.

**Commands.** Three, all in `kubectl backup`:

- `kubectl backup cos-setup` — run once, with your own IBM Cloud login. Makes the COS instance (or uses yours), the bucket with object lock, the driver's key, and the backup key. Shows the readable backup key once.
- `kubectl backup restore --from-cos ...` — makes a new PVC from a COS backup, in any cluster. Same flags as the local restore, plus the bucket.
- `kubectl backup cos-list` — lists what is in the bucket for a policy: volumes and moments.

**Status.** Each volume in the policy status gets `exportedTime` (the moment of the newest copy in COS), `exportedBytes` and `exportError`. The console shows them next to the local backup.

**Metrics and alerts.** A gauge for the age of the oldest unexported moment, and one for the age of the newest export. An alert for "export late", using twice the schedule, as for local backups.

## How it works

1. A local backup of a volume finishes (step 8). It has a moment.
2. If the policy has `export`, the controller starts an **export Job** in the driver namespace for that volume. The Job mounts the snapshot pool's share **read-only**, at the volume's backup folder and its `.moments` folder.
3. The Job unlocks the backup key (below), opens the Kopia repository in the bucket (the area of this policy only), and makes a Kopia snapshot of the two folders. It is tagged with the moment and the volume name. Kopia sends only chunks that COS does not have yet.
4. The Job writes its result (bytes sent, moment) in its termination message. The controller puts it in the status, as it does for other Jobs.
5. Kopia's maintenance run (a second Job, once a day) extends the locks of chunks still in use and removes snapshots past their keep time. The keep time of the lock is at least the maintenance interval plus 24 hours (Kopia's rule, tested on 20 Oct).

One export Job runs at a time for each policy, so a slow upload does not pile up. A moment that is not yet exported when the next one is made is exported as part of the next snapshot (Kopia sees both folders), so nothing is skipped; the older moment is still tagged and restorable.

**What goes to COS:** the whole backup folder (the latest whole copy) and the earlier moments (undo files) of every volume. A restore from COS therefore gives the same choice of moments as a local restore.

**Where Kopia comes from.** We add the Kopia command-line program to the driver image, at a pinned version and checked by its SHA-256. The Job runs it as a child process. We do not link Kopia into the driver as a Go library, because that would add a large number of Go dependencies to the controller, which must stay small and easy to review. Kopia is only used by the Job.

**Credentials.** Three secrets, all in the driver namespace:

- the COS key of the driver (an HMAC key for the S3 interface, limited to the one bucket by the custom role)
- the driver's IBM Cloud API key with the Reader role on the Key Protect root key
- the backup key, encrypted by Key Protect (the readable key is never stored)

The Job, not the controller, asks Key Protect to unlock the backup key. The key stays in the Job's memory. The controller never sees it.

## Setup command

As in the main spec: the user runs it with their own login. It uses the IBM Cloud APIs for COS, Key Protect and IAM. In order:

1. Check the account: Key Protect instance exists, and the account is in good standing (locks need that).
2. Make or reuse the COS instance. Make the bucket: object lock on, no default keep time, encrypted with the user's Key Protect root key. The bucket's region is the user's choice; we show the cost note below.
3. Make the custom role (read, write, extend locks; no delete of versions, no skip of locks, no change of bucket settings) and a service ID with that role on the one bucket. Make an HMAC key for it.
4. Give the service ID the Reader role on the root key.
5. Make the backup key, have Key Protect wrap it, store the wrapped copy in a Secret and in the bucket. Show the readable key once. Ask the user to confirm they saved it.
6. Print the `spec.export` block to paste into the policy.

The setup command never runs in the cluster, and the driver never creates or deletes a bucket or an instance.

## Restore from COS

1. The command reads the wrapped key from the bucket (or from the Secret in this cluster) and unlocks it with Key Protect, or with the emergency copy if the user passes it with `--backup-key-file`.
2. With the user's own login, it removes any delete markers in the bucket area (the driver's key cannot; a delete by that key only hides data).
3. It starts a Job that opens the repository read-only, restores the chosen snapshot into a scratch folder on a share, then the existing restore Job (step 8) copies the volume or applies the undo files, as for a local restore. This reuses the tested path.
4. The new PVC is held back from pods until the copy ends, as for a local restore. It never writes over an existing volume.

In a new cluster the policy and the snapshot pool need not exist. The command needs only the bucket, the endpoint, the keys, and a pool to restore into.

## The shortest lock

Each policy sets its own lock mode and keep time, so tests use a short lock. **The shortest possible lock is a little over 1 day, not 1 day.** Kopia needs the keep time to be at least its maintenance interval plus 24 hours, because it extends the locks of chunks still in use at each maintenance run (tested 20 Oct). The driver sets the maintenance interval from the keep time, with 1 hour as the shortest. So:

- the webhook refuses a keep time below 25 hours
- release tests and lab runs use `keepFor: 25h` with `lock: Compliance`, and the hourly maintenance run. Their data is then billed for 25 hours and can be deleted after that
- a real policy defaults to 30 days

## Not in this step

- No Governance lock (the main spec explains why).
- No export of volumes that are not in a `BackupPolicy`.
- No console screen for the setup or the restore from COS (the status is shown; the screens come later).
- No other object stores. IBM COS only. The Kopia S3 interface would allow others later.
- No restore of a VM definition. Only disks and volumes, as in step 8.

## Build order

Status: part 1 built and merged (tested against a local Kopia repository, not yet against MinIO or a real bucket, and the image was not built). Part 2 built (PR open): `spec.export`, the status fields, the webhook rules, the export and maintenance Jobs driven by the controller, Kopia retention, the two metrics and the alert examples; tested with fakes and a local Kopia repository, not on a cluster and not against a bucket. Parts 3 to 6 not started. The Job still reads the backup key from a Secret (part 3 replaces this with the Key Protect unlock). Part 2 decisions: the maintenance Job runs every maintenance interval (a day, or an hour for a 25-hour keep time) rather than a fixed once a day, so the lock extension keeps pace with a short lock; snapshots past the keep time are deleted by age in the maintenance Job, because Kopia can expire only by count; a failed export is retried after 5 minutes; the controller keeps exporting while a policy is suspended.

Each part is a PR with its tests, and updates the docs and the release test run in the same PR.

1. **Kopia in the image and the export Job.** Image change (pinned, checked). A new mode of the Job runner: `export`, run against a local test repository (a folder), then against MinIO in CI for the S3 path. Status fields.
2. **The controller, status, metrics, alerts.** `spec.export`, the webhook rules (lock mode, keep time at least the maintenance interval plus 24 hours), the export Jobs and the maintenance Job.
3. **Key handling.** Wrapped key, Key Protect unlock in the Job, emergency key. Tested against a Key Protect instance in the lab.
4. **`cos-setup`.** Built against the real IBM Cloud APIs with a throw-away account section; the custom role and the permissions are verified by a test that tries to do what the role must refuse.
5. **Restore from COS and `cos-list`.** Includes the delete marker clean-up.
6. **Release test: stage 18, second part.** Export two moments of the three volumes, then restore from COS into a second namespace, boot the VM, check the checksums. A second run restores into the other cluster of the lab pair.

## Tests

- Unit tests with fakes for the controller, the status, the webhook and the Job script.
- A local Kopia repository test in CI (folder), and an S3 test against MinIO in CI.
- On the lab and a real bucket: export, a second export that sends only changed chunks (check the bytes), a delete with the driver's key (check only a marker is made and the restore command repairs it), an attempt to shorten a lock with every key we hold (must fail), and a restore into another cluster.
- A test that the logs and the status never contain a key or secret.

## Cost and risk

- **COS storage.** Paid for each GB stored. The first upload is the size of the data; later ones add only changed chunks and the undo files. **Locked data is billed until its lock ends,** so a 30 day Compliance lock on a policy means 30 days of billing even after you delete the policy. A cost table for the lab (size, keep time, price per month) goes in this spec after you choose keep times; the formula is: stored GB × price per GB per month × (keep days ÷ 30), plus the changed data per day × keep days.
- **Compliance lock cannot be undone.** A wrong keep time (for example 10 years by mistake) costs money for that time and cannot be shortened, even by IBM. The setup command and the webhook ask for a clear confirmation of the keep time above 90 days.
- **Key loss.** Without Key Protect and without the emergency copy, nobody can read the backups. The setup command makes the user confirm the copy.
- **New dependency.** Kopia (a program in the image, used only by the export and restore Jobs). It is open source, used by other backup products, and tested in step 1.
- **Same account.** COS in the same IBM Cloud account as the cluster is better than nothing, but an account takeover reaches both. The lock protects against deletes by a stolen driver key, not against a stolen account owner. A bucket in a second account is possible (the user gives the endpoint and key); we do not set it up.

## Decisions I made for you

- Export is a block on the existing `BackupPolicy`, not a new resource. One policy, one schedule, one place to look.
- The export reads from the snapshot pool, mounted read-only, never from a running VM.
- We run the Kopia program as a child of the Job. We do not link it into the driver.
- The Job unlocks the backup key. The controller never holds it.
- Export is off unless the policy has `spec.export`.
- A new export block defaults to Compliance lock and 30 days. Lock mode and keep time are set on each policy. Tests use Compliance for 25 hours, the shortest Kopia allows (the owner asked for 1 day; 1 day is 1 hour short of Kopia's rule).
- A keep time above 90 days needs a typed confirmation, because it cannot be undone.
- One export Job runs at a time for each policy. A missed moment goes out with the next one.
- Restore from COS reuses the step 8 restore Job, so there is one restore path to test.
- COS restore into a new cluster needs no policy and no snapshot pool in that cluster.
- Only IBM COS for now.
- The bucket is locked to the cluster with a context-based restriction (CBR) rule: the cluster VPC and the admin addresses only. A virtual private endpoint (VPE) for COS in the cluster's VPC is recommended for real backups and not needed for lab tests. The setup command offers both, and `docs` tutorial 9 shows the manual steps. The admin addresses must be in the zone, or `cos-setup`, `cos-list` and `restore --from-cos` fail from a laptop.
- The setup command is built last of the key parts, after the export works against MinIO and a real bucket by hand.
- Maintenance runs every maintenance interval, not on a fixed daily timer: a day for a normal keep time, an hour for the 25-hour test lock. This keeps lock extension ahead of a short lock.
- Snapshots past the keep time are deleted by age in the maintenance Job. Kopia can only expire by count, so the export tells Kopia to keep every snapshot and the Job does the expiry.
- A failed export is tried again after 5 minutes, and a volume that keeps failing does not hold up the others.
- The controller keeps exporting while a policy is suspended: suspend stops new local backups, not the copy of moments that already exist.
- A failed maintenance run is written to the controller log and tried again; it is not in the status yet.

## Needs from you

- **A COS bucket for the release test, and a Key Protect root key in the lab account.** Needed from build part 3 on. I will build parts 1 and 2 without them (local repository and MinIO). Say if you want me to make them with the setup command once it exists instead.
- **Keep times** (local and COS) that your pilot customer would choose, to fill in the cost table.
