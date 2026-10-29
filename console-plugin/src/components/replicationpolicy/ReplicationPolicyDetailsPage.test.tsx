import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import ReplicationPolicyDetailsPage from './ReplicationPolicyDetailsPage';

jest.mock('react-router-dom', () => ({
  useHistory: () => ({ push: jest.fn() }),
  useParams: () => ({ name: 'dr' }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const policy = (name: string, auth: string, ca: string) => ({
  metadata: { name },
  spec: {
    sourcePoolName: 'pool-a',
    destinationBasePath: '/pvcs',
    schedule: '15m',
    destinationEndpoint: 'https://repl.example.com',
    destinationAuthSecretRef: auth,
    destinationCACertSecretRef: ca,
  },
});

function watch(current: unknown, all: unknown[]) {
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { isList?: boolean } | null) => {
    if (!req) return [undefined, true, undefined];
    return req.isList ? [all, true, undefined] : [current, true, undefined];
  });
}

describe('ReplicationPolicyDetailsPage delete', () => {
  it('offers to delete the Secrets the receiver setup made', () => {
    const dr = policy('dr', 'dr-receiver-token', 'dr-receiver-ca');
    watch(dr, [dr]);
    render(<ReplicationPolicyDetailsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    expect(screen.getByRole('checkbox', { name: /dr-receiver-token, dr-receiver-ca/ })).toBeTruthy();
  });

  it('offers nothing for Secrets that were named by hand', () => {
    const dr = policy('dr', 'my-token', 'my-ca');
    watch(dr, [dr]);
    render(<ReplicationPolicyDetailsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    expect(screen.queryByRole('checkbox')).toBeNull();
  });
});

describe('ReplicationPolicyDetailsPage receiver status', () => {
  it('shows why the receiver is not reachable', () => {
    const dr = {
      ...policy('dr', 'dr-receiver-token', 'dr-receiver-ca'),
      status: {
        consecutiveFailures: 0,
        conditions: [{ type: 'ReceiverReachable', status: 'False', reason: 'TokenRefused', message: 'token refused' }],
      },
    };
    watch(dr, [dr]);
    render(<ReplicationPolicyDetailsPage />);
    expect(screen.getByText('Not reachable')).toBeTruthy();
    expect(screen.getByText(/token refused/)).toBeTruthy();
  });
});

describe('ReplicationPolicyDetailsPage last error', () => {
  it('shows the last error as a danger alert', () => {
    const dr = {
      ...policy('dr', 'my-token', 'my-ca'),
      status: { consecutiveFailures: 2, lastError: 'rsync failed: connection refused' },
    };
    watch(dr, [dr]);
    const { container } = render(<ReplicationPolicyDetailsPage />);
    expect(screen.getByText('rsync failed: connection refused')).toBeTruthy();
    expect(container.querySelector('[class*="c-alert"][class*="pf-m-danger"]')).toBeTruthy();
    expect(screen.getByText(/Danger alert/)).toBeTruthy();
  });

  it('shows no alert when there is no error', () => {
    const dr = { ...policy('dr', 'my-token', 'my-ca'), status: { consecutiveFailures: 0 } };
    watch(dr, [dr]);
    const { container } = render(<ReplicationPolicyDetailsPage />);
    expect(container.querySelector('[class*="c-alert"][class*="pf-m-danger"]')).toBeNull();
  });
});
