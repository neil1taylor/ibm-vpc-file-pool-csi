import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import InstantSnapshotStatus from './InstantSnapshotStatus';

type Watched = { data?: unknown; error?: unknown };

function watch(byKind: Record<string, Watched>) {
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } }) => {
    const w = byKind[req.groupVersionKind.kind] || {};
    return [w.data ?? [], true, w.error];
  });
}

const kubevirt = (gates: string[]) => ({
  metadata: { name: 'kubevirt', namespace: 'openshift-cnv' },
  spec: { configuration: { developerConfiguration: { featureGates: gates } } },
});

// The settings ConfigMap the Helm chart renders (the driver's vmSnapshots.method).
const settings = (method: string) => ({
  metadata: { name: 'ibm-vpc-file-pool-csi-settings', namespace: 'kube-system' },
  data: { vmSnapshotMethod: method },
});

const chained = {
  metadata: { name: 'pvc-1' },
  spec: { pvcNamespace: 'apps', pvcName: 'web-root' },
  status: { diskChain: ['s2.qcow2', 's1.qcow2', 'disk.img'] },
};

const webVM = {
  metadata: { namespace: 'apps', name: 'web' },
  spec: { template: { spec: { volumes: [{ persistentVolumeClaim: { claimName: 'web-root' } }] } } },
};

describe('InstantSnapshotStatus', () => {
  it('says snapshots are instant when the driver is set to auto and the gate is on, and lists the VMs', () => {
    watch({
      ConfigMap: { data: settings('auto') },
      KubeVirt: { data: [kubevirt(['Sidecar'])] },
      SubVolume: { data: [chained] },
      VirtualMachine: { data: [webVM] },
    });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('VM snapshots: instant')).toBeTruthy();
    expect(screen.getByText(/would not start/)).toBeTruthy();
    fireEvent.click(screen.getByText(/1 VM disk has instant snapshots/));
    expect(screen.getByText(/apps\/web/)).toBeTruthy();
    expect(screen.getByText(/web-root/)).toBeTruthy();
    expect(screen.getByText(/2 snapshots/)).toBeTruthy();
  });

  it('says so when no VM disk has instant snapshots', () => {
    watch({ ConfigMap: { data: settings('auto') }, KubeVirt: { data: [kubevirt(['Sidecar'])] } });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText(/No VM disk has instant snapshots now/)).toBeTruthy();
  });

  it('says copy, not instant, when the driver is set to copy even though the gate is on', () => {
    watch({
      ConfigMap: { data: settings('copy') },
      KubeVirt: { data: [kubevirt(['Sidecar'])] },
      SubVolume: { data: [chained] },
      VirtualMachine: { data: [webVM] },
    });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('VM snapshots: copy')).toBeTruthy();
    expect(screen.getByText(/The driver is set to copy/)).toBeTruthy();
    expect(screen.queryByText('VM snapshots: instant')).toBeNull();
    expect(screen.queryByText(/1 VM disk has instant snapshots/)).toBeNull();
  });

  it('says copy and that the driver would be instant when the driver is set to auto but the gate is off', () => {
    watch({ ConfigMap: { data: settings('auto') }, KubeVirt: { data: [kubevirt(['Snapshot'])] } });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('VM snapshots: copy')).toBeTruthy();
    expect(screen.getByText(/would take instant snapshots, but the KubeVirt Sidecar feature gate is off/)).toBeTruthy();
    expect(screen.getByText(/vmSnapshots.enableSidecarGate=true/)).toBeTruthy();
    expect(screen.getByText(/Red Hat does not support/)).toBeTruthy();
  });

  it('says copy when the driver is set to copy and the gate is off', () => {
    watch({ ConfigMap: { data: settings('copy') }, KubeVirt: { data: [kubevirt(['Snapshot'])] } });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('VM snapshots: copy')).toBeTruthy();
    expect(screen.getByText(/The driver is set to copy/)).toBeTruthy();
  });

  it('says copy when the driver is set to copy and the user may not read KubeVirt', () => {
    watch({ ConfigMap: { data: settings('copy') }, KubeVirt: { error: { code: 403 } } });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('VM snapshots: copy')).toBeTruthy();
  });

  it('says the mode is unknown when the driver is set to auto and the user may not read KubeVirt', () => {
    watch({ ConfigMap: { data: settings('auto') }, KubeVirt: { error: { code: 403 } } });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('VM snapshots: unknown')).toBeTruthy();
    expect(screen.getByText(/cannot read the KubeVirt settings/)).toBeTruthy();
    expect(screen.queryByText(/full copies/)).toBeNull();
  });

  it('says the mode is unknown, and why, when the driver does not report its setting', () => {
    watch({ KubeVirt: { data: [kubevirt(['Sidecar'])] } });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('VM snapshots: unknown')).toBeTruthy();
    expect(screen.getByText(/does not report its setting/)).toBeTruthy();
    expect(screen.queryByText('VM snapshots: instant')).toBeNull();
  });

  it('has no button or link that changes the setting', () => {
    watch({ ConfigMap: { data: settings('auto') }, KubeVirt: { data: [kubevirt(['Snapshot'])] } });
    render(<InstantSnapshotStatus />);
    expect(screen.queryByRole('button', { name: /turn on|enable/i })).toBeNull();
  });
});
