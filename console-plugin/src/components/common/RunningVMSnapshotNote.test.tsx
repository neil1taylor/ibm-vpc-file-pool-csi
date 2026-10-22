import React from 'react';
import { render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import RunningVMSnapshotNote from './RunningVMSnapshotNote';

const BASE =
  'To snapshot a running VM, the driver works directly with QEMU, which runs the VM, not through OpenShift. Red Hat does not support this. While this has been tested, an OpenShift update could stop snapshots until the driver is updated.';
const INSTANT = 'Instant snapshots also need a KubeVirt setting that Red Hat does not support.';

function kubevirtWith(gates: string[] | null, error?: unknown) {
  const data = gates ? [{ metadata: { name: 'kubevirt' }, spec: { configuration: { developerConfiguration: { featureGates: gates } } } }] : [];
  (useK8sWatchResource as jest.Mock).mockReturnValue([data, true, error]);
}

describe('RunningVMSnapshotNote', () => {
  it('says snapshots of running VMs are not supported by Red Hat', () => {
    kubevirtWith(['Snapshot']);
    render(<RunningVMSnapshotNote />);
    expect(screen.getByText('Not supported by Red Hat')).toBeTruthy();
    expect(screen.getByText(BASE)).toBeTruthy();
    expect(screen.queryByText(INSTANT)).toBeNull();
  });

  it('adds that instant snapshots need an unsupported KubeVirt setting when they are on', () => {
    kubevirtWith(['Sidecar']);
    render(<RunningVMSnapshotNote />);
    expect(screen.getByText(BASE)).toBeTruthy();
    expect(screen.getByText(INSTANT)).toBeTruthy();
  });

  it('still gives the note when the KubeVirt settings cannot be read', () => {
    kubevirtWith(null, new Error('forbidden'));
    render(<RunningVMSnapshotNote />);
    expect(screen.getByText(BASE)).toBeTruthy();
  });
});
