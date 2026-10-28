import React from 'react';
import { render, screen } from '@testing-library/react';
import ReplicationPolicyListPage from './ReplicationPolicyListPage';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

describe('ReplicationPolicyListPage', () => {
  it('links to the Recovery page', () => {
    render(<ReplicationPolicyListPage />);
    const link = screen.getByRole('link', { name: 'Recovery' });
    expect(link.getAttribute('href')).toBe('/vpc-file-pools/replication/~recovery');
  });
});
