import { SubVolume } from '../types';
import { VirtualMachineLike } from './k8s-objects';
import { disksWithInstantSnapshots, instantSnapshotState, KubeVirtLike } from './instant-snapshots';

const kubevirt = (gates?: string[]): KubeVirtLike => ({
  metadata: { name: 'kubevirt-kubevirt-hyperconverged', namespace: 'openshift-cnv' },
  spec: { configuration: { developerConfiguration: { featureGates: gates } } },
});

const subvolume = (namespace: string, claim: string, diskChain?: string[]): SubVolume =>
  ({
    metadata: { name: `pvc-${claim}` },
    spec: { pvcNamespace: namespace, pvcName: claim },
    status: { diskChain },
  }) as SubVolume;

const vm = (namespace: string, name: string, claims: string[]): VirtualMachineLike => ({
  metadata: { namespace, name },
  spec: { template: { spec: { volumes: claims.map((c) => ({ persistentVolumeClaim: { claimName: c } })) } } },
});

describe('instantSnapshotState', () => {
  it('is on when a KubeVirt has the Sidecar feature gate', () => {
    expect(instantSnapshotState([kubevirt(['Snapshot', 'Sidecar'])], true, undefined)).toBe('on');
  });
  it('is off when KubeVirt has no Sidecar feature gate', () => {
    expect(instantSnapshotState([kubevirt(['Snapshot'])], true, undefined)).toBe('off');
    expect(instantSnapshotState([kubevirt()], true, undefined)).toBe('off');
  });
  it('is unknown when the user may not read KubeVirt', () => {
    expect(instantSnapshotState([], true, { code: 403 })).toBe('unknown');
  });
  it('is unknown while loading', () => {
    expect(instantSnapshotState([], false, undefined)).toBe('unknown');
  });
  it('is unknown when there is no KubeVirt (OpenShift Virtualization not installed)', () => {
    expect(instantSnapshotState([], true, undefined)).toBe('unknown');
  });
});

describe('disksWithInstantSnapshots', () => {
  it('lists only disks with a chain longer than disk.img, with the VM that uses them', () => {
    const svs = [
      subvolume('apps', 'web-root', ['snap-2.qcow2', 'snap-1.qcow2', 'disk.img']),
      subvolume('apps', 'plain', undefined),
      subvolume('apps', 'flat', ['disk.img']),
    ];
    expect(disksWithInstantSnapshots(svs, [vm('apps', 'web', ['web-root'])])).toEqual([
      { namespace: 'apps', claim: 'web-root', snapshots: 2, vm: 'web' },
    ]);
  });
  it('leaves the VM out when no VM uses the disk or VMs cannot be read', () => {
    const svs = [subvolume('apps', 'orphan', ['a.qcow2', 'disk.img'])];
    expect(disksWithInstantSnapshots(svs, [vm('other', 'web', ['orphan'])])).toEqual([
      { namespace: 'apps', claim: 'orphan', snapshots: 1, vm: undefined },
    ]);
    expect(disksWithInstantSnapshots(svs, undefined)).toEqual([
      { namespace: 'apps', claim: 'orphan', snapshots: 1, vm: undefined },
    ]);
  });
  it('sorts by namespace, then VM, then disk', () => {
    const svs = [
      subvolume('b', 'd1', ['x', 'disk.img']),
      subvolume('a', 'd2', ['x', 'disk.img']),
      subvolume('a', 'd1', ['x', 'disk.img']),
    ];
    const vms = [vm('a', 'vm-z', ['d1']), vm('a', 'vm-a', ['d2'])];
    expect(disksWithInstantSnapshots(svs, vms).map((d) => `${d.namespace}/${d.vm}/${d.claim}`)).toEqual([
      'a/vm-a/d2',
      'a/vm-z/d1',
      'b/undefined/d1',
    ]);
  });
});
