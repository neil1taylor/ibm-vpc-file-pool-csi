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
  it('says instant snapshots are on and lists the VMs whose disks have them', () => {
    watch({
      KubeVirt: { data: [kubevirt(['Sidecar'])] },
      SubVolume: { data: [chained] },
      VirtualMachine: { data: [webVM] },
    });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('Instant VM snapshots: on')).toBeTruthy();
    expect(screen.getByText(/would not start/)).toBeTruthy();
    fireEvent.click(screen.getByText(/1 VM disk has instant snapshots/));
    expect(screen.getByText(/apps\/web/)).toBeTruthy();
    expect(screen.getByText(/web-root/)).toBeTruthy();
    expect(screen.getByText(/2 snapshots/)).toBeTruthy();
  });

  it('says so when no VM disk has instant snapshots', () => {
    watch({ KubeVirt: { data: [kubevirt(['Sidecar'])] } });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText(/No VM disk has instant snapshots now/)).toBeTruthy();
  });

  it('says snapshots are full copies when the gate is off, and how an administrator turns it on', () => {
    watch({ KubeVirt: { data: [kubevirt(['Snapshot'])] } });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('Instant VM snapshots: off')).toBeTruthy();
    expect(screen.getByText(/full copies/)).toBeTruthy();
    expect(screen.getByText(/vmSnapshots.enableSidecarGate=true/)).toBeTruthy();
    expect(screen.getByText(/Red Hat does not support/)).toBeTruthy();
  });

  it('says the state is unknown when the user may not read KubeVirt', () => {
    watch({ KubeVirt: { error: { code: 403 } } });
    render(<InstantSnapshotStatus />);
    expect(screen.getByText('Instant VM snapshots: unknown')).toBeTruthy();
    expect(screen.queryByText(/full copies/)).toBeNull();
  });

  it('has no button or link that changes the setting', () => {
    watch({ KubeVirt: { data: [kubevirt(['Snapshot'])] } });
    render(<InstantSnapshotStatus />);
    expect(screen.queryByRole('button', { name: /turn on|enable/i })).toBeNull();
  });
});
