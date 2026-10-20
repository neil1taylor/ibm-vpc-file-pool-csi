# Spec: golden images are made by CDI, not by our converter

Date: 2026-10-20. Status: approved in chat ("if we don't need the converter, we should get rid of it").

## Problem

A pool with golden images turned on makes a ready-to-boot disk image of each OS in each target
namespace. Today our own converter Job does this: it installs tools, downloads the container disk,
unpacks it, converts it to raw, and patches the boot sector. It was written because CDI, the
cluster's own image importer, was thought not to work with this driver.

The release test run (stage 08, 20 October) and a check next to it found:

| Finding | Consequence |
|---|---|
| The converter runs as root in the app's namespace | No golden image unless that namespace has the `anyuid` permission |
| The syncer makes the golden disks again while their pool is being deleted | A pool with golden images can never be deleted |
| Catalog entries (DataSources) stay after their pool is deleted | The VM catalog shows images whose disks do not exist. Five such entries were on rovslab |
| Every catalog entry for an image has the same name | Only the first namespace, of the first pool, gets a catalog entry |

## What was measured on rovslab, 20 October

- **CDI import from the registry onto pool storage works.** CentOS Stream 9 from
  `quay.io/containerdisks/centos-stream:9` imported in about 40 seconds. A VM booted from it with
  BIOS, and its guest agent connected. RHEL 9 from `registry.redhat.io/rhel9/rhel-guest-image`:
  see "RHEL 9 result" below.
- **CDI clone of the cluster's own image onto pool storage does not work reliably.** Cloning the
  RHEL 9 image from Ceph: CDI's upload pod (memory limit 600 MB) was killed for lack of memory after
  10 seconds, then could not restart because the half-written `disk.img` existed. The same failure
  was seen once on 18 October.
- **VMs on pool storage work.** A VM made from the golden image template booted, its guest agent
  connected, and it live-migrated to another worker without a restart.

## Design

- The syncer keeps its job: for each image and each target namespace, make a golden disk, a
  Template, and a catalog entry. Only the way the disk is made changes.
- **The golden disk is a CDI DataVolume** in the target namespace, on the pool's StorageClass,
  named `golden-<image>` as before. CDI downloads and converts the image itself. Its source is the
  image's registry address, pulled by the node (`pullMethod: node`), the same way the cluster's own
  image jobs pull it.
  - For images whose cluster job uses an internal ImageStream (the RHEL images), the syncer uses
    the external address that the ImageStream itself imports from, for example
    `registry.redhat.io/rhel9/rhel-guest-image`. The node's pull secret already allows it.
- The golden disk is ready when the DataVolume has succeeded. CDI retries a failed import itself.
- **Catalog entries** are named `<image>-<namespace>-nfs-pool`, one per golden disk.
- **Deleting a pool** first deletes its golden DataVolumes, claims, Templates and catalog entries,
  in every namespace. The syncer skips a pool that is being deleted.
- **Removed:** the converter Job and its script, the `goldenImages.converterImage` setting, the
  internal registry permission (RoleBinding) the converter needed, and the controller's permissions
  that only the converter used.

## Testing

- Unit tests: the DataVolume the syncer makes (source, StorageClass, size, labels), readiness from
  the DataVolume phase, the external address for ImageStream images, pool deletion clean-up, and the
  skip of a pool being deleted.
- Release test stage 08 runs in a namespace with no extra permissions and checks: golden image
  ready, Template, catalog entry pointing at this pool's disk, VM boot and guest agent, live
  migration, pool deletion, and nothing left in the shared images namespace.

## Decisions I made for you

- CDI downloads from the registry. It does not clone the cluster's existing image, because that
  clone ran out of memory on this cluster twice.
- The golden disk is made once and not refreshed when a newer OS image comes out. This is the same
  as today. Delete the golden disk to get a fresh one on the next sync.
- Disk images are no longer patched to set the boot flag. The CDI-made CentOS disk booted without
  it. If an image does not boot, stage 08 will show it.
- The `converterImage` setting is removed, not kept as an unused field. This is a new app, so no
  existing pools need it.
- Two pools must not send golden images to the same namespace. That is documented, not blocked.
