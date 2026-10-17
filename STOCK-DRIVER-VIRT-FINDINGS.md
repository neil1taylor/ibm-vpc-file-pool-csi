# IBM VPC File CSI Driver with OpenShift Virtualization: Field Findings

This page records problems and workarounds that an IBM team found when it ran OpenShift Virtualization on IBM's standard VPC File CSI driver (`vpc.file.csi.ibm.io`). It explains what this pool driver must handle.

**Source and status.** These findings come from notes in the IBM VPCMigrationFactory Box folder, read on 17 Oct 2026. This project did not reproduce these results. Treat them as reports, not verified facts.

| Source note | Folder | Date |
|-------------|--------|------|
| ROKS VPC File CSI Issues Follow-Up | Storage | 10 Apr 2026 |
| IBM VPC File mods for ROKS | Storage / Sandy-sync | 12 May 2026 |
| Scale testing - VPC File | Scale and Performance | 10 Sep 2026 |

For how this driver handles `cloneStrategy`, `StorageProfile` and UID 107, see [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md).

## 1. Issues with the stock driver and OpenShift Virtualization

Source: "ROKS VPC File CSI Issues Follow-Up", 10 Apr 2026.

### Only three IOPS storage classes are pre-installed

The pre-installed IOPS classes are 500, 1000 and 3000. Workaround: create your own StorageClass with provisioner `vpc.file.csi.ibm.io`, profile `dp2` and `iops <n>`. The note leaves one open question for the driver team: will more classes be added?

### VM creation fails when the storage class differs from the template's

The pre-installed storage profiles use `snapshot` as the clone strategy. Creating a VM fails when its StorageClass differs from the StorageClass of the template's bootable volume (DataSource). The note lists three workarounds:

- Set `cloneStrategy: copy` on the StorageProfile, with `claimPropertySets` of `accessModes: ReadWriteMany` and `volumeMode: Filesystem`.
- Create a second set of StorageClasses and profiles for `copy`. Name them so you can tell clone from snapshot.
- Keep one bootable image per size and IOPS combination. For example, a `rhel10` image of 100 GiB suits StorageClasses of 100 to 6000 IOPS. A 500 GiB image suits 100 to 10k.

### CDI does not set access mode and volume mode

CDI does not populate `accessModes` and `volumeMode` for the IBM file provisioner. PVC creation then fails when you create a VM. Workaround: add `accessModes: ReadWriteMany` and `volumeMode: Filesystem` to the storage profiles. The note lists these affected profiles:

- `ibmc-vpc-file-{500,1000,3000}-iops`
- `-eit`
- `-metro-*` and `-metro-retain-*`
- `-min-iops`
- `-regional`
- `-regional-max-bandwidth` and `-regional-max-bandwidth-sds`
- `-retain-*`

### Pre-installed regional storage classes need a uid

The pre-installed regional StorageClasses do not work unless `uid` is set. The VM fails to start with a permission error on the boot volume. Workaround: create a new StorageClass with parameter `uid` set to `107`. You cannot edit the parameters of an existing StorageClass.

## 2. Configuring OpenShift Virtualization on VPC File

Source: "IBM VPC File mods for ROKS" (Storage / Sandy-sync), 12 May 2026.

- Install OpenShift Virtualization on top of the VPC File provider. Preferably have no ODF in the cluster. With ODF, you need extra HyperConverged configuration to change the default StorageClass.
- Use a retain-reclaim StorageClass as the default for both Kubernetes and KubeVirt, for example `ibmc-vpc-file-retain-1000-iops`. Only `dp2` shares have a default retain StorageClass. RFS has none.
- Exactly one StorageClass may carry both annotations `storageclass.kubernetes.io/is-default-class: "true"` and `storageclass.kubevirt.io/is-default-virt-class: "true"`. Duplicates make the storage checkup fail. Find the current holder:

  ```
  oc get storageclass -o json | jq -r '.items[] | select(.metadata.annotations["storageclass.kubevirt.io/is-default-virt-class"] == "true") | .metadata.name'
  ```

  Set the annotation:

  ```
  oc patch storageclass <sc> -p '{"metadata": {"annotations":{"storageclass.kubevirt.io/is-default-virt-class":"true"}}}'
  ```

- Changing the KubeVirt default StorageClass makes CDI clone all boot images to it. Check that all bootable images are ready, not Pending or Cloning.
- vTPM, which the storage checkup uses, needs a custom StorageClass with these settings. Name it, for example, `ibmc-vpc-file-vtpm`.
  - provisioner `vpc.file.csi.ibm.io`
  - `csi.storage.k8s.io/fstype`: `ext4`
  - profile `dp2` (match the default)
  - `isENIEnabled`: `'true'`
  - `mountOptions`: `hard`, `nfsvers=4.1`, `sec=sys`
  - `allowVolumeExpansion: true`
  - `volumeBindingMode: Immediate`

  Then set `claimPropertySets` on its StorageProfile with the `ReadWriteOnce` access mode.
- Set `claimPropertySets` on all StorageProfiles: access modes, volume modes, clone strategy, and data import cron source format `snapshot`.
- Annotate StorageProfiles with a minimum PVC size where IOPS needs it. For example, 3k IOPS needs at least 80 Gi.

## 3. Scale test failure

Source: "Scale testing - VPC File" (Scale and Performance), failure dated 4 Sep 2026, note dated 10 Sep 2026.

Creating a VM with `ibmc-vpc-file-3000-iops` in `br-sao-2` failed. The PVC stayed Pending with this error:

```
Volume provisioning failed with infeasible error … BackendError: {Code:SubnetFindFailed, Description:A subnet with the specified zone 'br-sao-2' and available cluster subnet list '<id>' could not be found..no subnet found, RC:404}
```

The DataVolume was cloning from a template root disk in another namespace. The note gives no cause or fix.

## Decisions I made for you

- I put this on its own page instead of adding it to KNOWN-LIMITATIONS.md, because it describes the stock driver, not this one.
- I kept the source notes' wording for commands, names and error text, and added no analysis of my own.
- `cloneStrategy: copy`, `claimPropertySets` and UID 107 agree with the repo. One difference: the vTPM StorageClass uses `mountOptions` `hard`, while CLAUDE.md says this driver must never use `hard` (pods hang on NFS failures). I kept both. The vTPM class is a stock-driver class, not a pool class.
