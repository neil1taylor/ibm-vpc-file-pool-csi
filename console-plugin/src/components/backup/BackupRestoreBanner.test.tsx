import React from 'react';
import { render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import BackupRestoreBanner from './BackupRestoreBanner';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const watch = (br: unknown) =>
  (useK8sWatchResource as jest.Mock).mockImplementation((req: unknown) => [req ? br : undefined, true, undefined]);

const show = (status?: Record<string, unknown>) => {
  watch({ metadata: { name: 'r1' }, spec: {}, status });
  render(<BackupRestoreBanner restoreName="r1" targetNamespace="apps" targetName="data-restored" />);
};

describe('BackupRestoreBanner', () => {
  it('says the request is starting before the driver has a status', () => {
    watch(undefined);
    render(<BackupRestoreBanner restoreName="r1" targetNamespace="apps" targetName="data-restored" />);
    expect(screen.getByText(/the request is starting/)).toBeTruthy();
  });

  it('shows the working phases', () => {
    show({ phase: 'Provisioning' });
    expect(screen.getByText(/making the new volume/)).toBeTruthy();
  });

  it('shows the copy phase', () => {
    show({ phase: 'Restoring' });
    expect(screen.getByText(/copying the backup into the new volume/)).toBeTruthy();
  });

  it('shows the restored moment and a link to the new claim when done', () => {
    show({ phase: 'Done', restoredMoment: '2026-10-20T09:00:00Z' });
    expect(screen.getByText('The restored volume is ready')).toBeTruthy();
    expect(screen.getByText(/Restored the backup from 2026-10-20T09:00:00Z/)).toBeTruthy();
    expect(screen.getByRole('link', { name: 'data-restored' })).toBeTruthy();
  });

  it('shows the message and the troubleshooting link on failure, and does not say the claim was deleted', () => {
    show({ phase: 'Failed', message: 'no backup at that moment' });
    expect(screen.getByText('The restore failed')).toBeTruthy();
    expect(screen.getByText(/no backup at that moment/)).toBeTruthy();
    expect(screen.getByTestId('banner-help')).toBeTruthy();
  });
});
