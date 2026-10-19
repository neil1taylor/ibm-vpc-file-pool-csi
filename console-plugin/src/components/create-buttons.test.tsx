import React from 'react';
import { render, screen } from '@testing-library/react';
import SubVolumeListPage from './subvolume/SubVolumeListPage';
import SnapshotListPage from './snapshot/SnapshotListPage';
import VolumeGroupSnapshotListPage from './volumegroupsnapshot/VolumeGroupSnapshotListPage';

jest.mock('react-router-dom', () => ({
  Link: ({ children }: { children?: React.ReactNode }) => <a>{children}</a>,
  useHistory: () => ({ push: jest.fn() }),
}));

describe('list page create buttons say what they create', () => {
  it('volumes', () => {
    render(<SubVolumeListPage />);
    expect(screen.getByTestId('list-page-create-link').textContent).toBe('Create volume (PVC)');
  });

  it('snapshots', () => {
    render(<SnapshotListPage />);
    expect(screen.getByTestId('list-page-create-link').textContent).toBe('Create snapshot');
  });

  it('group snapshots', () => {
    render(<VolumeGroupSnapshotListPage />);
    expect(screen.getByTestId('list-page-create-link').textContent).toBe('Create group snapshot');
  });
});
