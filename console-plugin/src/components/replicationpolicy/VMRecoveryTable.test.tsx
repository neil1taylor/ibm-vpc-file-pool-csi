import React from 'react';
import { render, screen } from '@testing-library/react';
import VMRecoveryTable from './VMRecoveryTable';

describe('VMRecoveryTable', () => {
  beforeEach(() => jest.useFakeTimers().setSystemTime(new Date('2026-10-20T12:00:00Z')));
  afterEach(() => jest.useRealTimers());

  it('shows nothing when the policy has no running VM', () => {
    const { container } = render(<VMRecoveryTable vms={undefined} schedule="1h" />);
    expect(container.textContent).toBe('');
  });

  it('shows the copy moment, consistency and common moments of a VM', () => {
    render(
      <VMRecoveryTable
        schedule="2h"
        vms={[
          {
            name: 'default/web',
            subVolumes: ['pvc-a', 'pvc-b'],
            copyMoment: '2026-10-20T11:00:00Z',
            consistency: 'FileSystemConsistent',
            commonMoments: ['2026-10-20T11:00:00Z', '2026-10-20T10:00:00Z'],
          },
        ]}
      />,
    );
    expect(screen.getByText('default/web')).toBeTruthy();
    expect(screen.getByText('File-system consistent')).toBeTruthy();
    expect(screen.getByText('2 moments: 1h, 2h')).toBeTruthy();
    expect(screen.queryByText(/older than twice/)).toBeNull();
  });

  it('warns about stale data and a crash consistent copy', () => {
    render(
      <VMRecoveryTable
        schedule="1h"
        vms={[
          {
            name: 'default/db',
            subVolumes: ['pvc-a'],
            copyMoment: '2026-10-20T08:00:00Z',
            consistency: 'CrashConsistent',
            commonMoments: ['2026-10-20T08:00:00Z'],
          },
        ]}
      />,
    );
    expect(screen.getByText('Crash consistent')).toBeTruthy();
    expect(screen.getByText(/older than twice the 1h schedule/)).toBeTruthy();
  });

  it('says when the disks of a VM share no moment', () => {
    render(
      <VMRecoveryTable
        schedule="1h"
        vms={[{ name: 'default/db', subVolumes: ['a', 'b'], copyMoment: '2026-10-20T11:30:00Z', commonMoments: [] }]}
      />,
    );
    expect(screen.getByText(/disks share no moment/)).toBeTruthy();
  });
});
