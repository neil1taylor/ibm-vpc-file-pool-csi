import React from 'react';
import { render, screen } from '@testing-library/react';
import VolumeStatusTable from './VolumeStatusTable';
import { SubVolumeReplicationStatus } from '../../types';

describe('VolumeStatusTable', () => {
  it('says so when no status has been reported', () => {
    render(<VolumeStatusTable statuses={undefined} />);
    expect(screen.getByText(/No SubVolume sync statuses/)).toBeTruthy();
  });

  it('shows the copy moment, consistency and restore points of a VM disk', () => {
    const statuses: SubVolumeReplicationStatus[] = [
      {
        subVolumeName: 'pvc-vm-disk',
        copyMoment: '2026-10-20T10:00:00Z',
        consistency: 'FileSystemConsistent',
        restorePoints: 2,
        oldestRestorePoint: '2026-10-19T12:00:00Z',
        bytesSynced: 2048,
        vmChangeList: 'repl-abc',
      },
    ];
    jest.useFakeTimers().setSystemTime(new Date('2026-10-20T12:00:00Z'));
    render(<VolumeStatusTable statuses={statuses} />);
    jest.useRealTimers();
    expect(screen.getByText('pvc-vm-disk')).toBeTruthy();
    expect(screen.getByText('2h')).toBeTruthy();
    expect(screen.getByText('File-system consistent')).toBeTruthy();
    expect(screen.getByText('2 (oldest 1d)')).toBeTruthy();
    expect(screen.getByText('changes only')).toBeTruthy();
  });

  it('flags a crash consistent copy and gives the reason', () => {
    render(
      <VolumeStatusTable
        statuses={[
          { subVolumeName: 'pvc-a', copyMoment: '2026-10-20T10:00:00Z', consistency: 'CrashConsistent', consistencyReason: 'no guest agent' },
        ]}
      />,
    );
    expect(screen.getByText('Crash consistent')).toBeTruthy();
    expect(screen.getByText(/no guest agent/)).toBeTruthy();
  });

  it('shows a waiting message as a warning and a failure as an error', () => {
    render(
      <VolumeStatusTable
        statuses={[
          { subVolumeName: 'pvc-wait', lastError: 'copy waits: the VM is moving to another worker' },
          { subVolumeName: 'pvc-bad', lastError: 'apply Job failed for pvc-bad' },
        ]}
      />,
    );
    const warn = screen.getByText('copy waits: the VM is moving to another worker');
    const bad = screen.getByText('apply Job failed for pvc-bad');
    expect(warn.closest('[data-severity]')?.getAttribute('data-severity')).toBe('warning');
    expect(bad.closest('[data-severity]')?.getAttribute('data-severity')).toBe('danger');
  });

  it('shows a plain volume without VM fields', () => {
    render(<VolumeStatusTable statuses={[{ subVolumeName: 'pvc-files', lastSyncTime: '2026-10-20T10:00:00Z', bytesSynced: 1024 }]} />);
    expect(screen.getByText('pvc-files')).toBeTruthy();
    expect(screen.queryByText('File-system consistent')).toBeNull();
  });

  it('shows the phase of a copy in progress', () => {
    render(<VolumeStatusTable statuses={[{ subVolumeName: 'pvc-a', vmCopy: { id: 'x', phase: 'Sending' } }]} />);
    expect(screen.getByText('Sending')).toBeTruthy();
  });
});

describe('VolumeStatusTable Red Hat support note', () => {
  it('says that copies of running VMs are not supported by Red Hat when a VM disk is in the policy', () => {
    render(<VolumeStatusTable statuses={[{ subVolumeName: 'pvc-vm', copyMoment: '2026-10-20T10:00:00Z' }]} />);
    expect(screen.getByText('Not supported by Red Hat')).toBeTruthy();
    expect(
      screen.getByText(
        'To copy a running VM, the driver works directly with QEMU, which runs the VM, not through OpenShift. Red Hat does not support this. While this has been tested, an OpenShift update could stop copies until the driver is updated.',
      ),
    ).toBeTruthy();
  });

  it('says nothing about Red Hat when no VM disk is in the policy', () => {
    render(<VolumeStatusTable statuses={[{ subVolumeName: 'pvc-a', lastSyncTime: '2026-10-20T10:00:00Z' }]} />);
    expect(screen.queryByText('Not supported by Red Hat')).toBeNull();
  });
});
