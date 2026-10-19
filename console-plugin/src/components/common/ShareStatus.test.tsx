import React from 'react';
import { render, screen } from '@testing-library/react';
import ShareStatus from './ShareStatus';
import { PoolShareStatus } from '../../types';

const share: PoolShareStatus = {
  shareID: 's1',
  shareName: 'share-1',
  mountTargetIP: '',
  mountTargetID: '',
  totalGB: 100,
  allocatedGB: 0,
  pvcCount: 0,
  state: 'stable',
  zone: 'z',
};

describe('ShareStatus', () => {
  it('warns instead of showing a green "stable" when there is no mount target', () => {
    render(<ShareStatus share={share} />);
    expect(screen.getByText('No mount target')).toBeTruthy();
    expect(screen.queryByText('stable')).toBeNull();
  });

  it('shows stable when the share has a mount target', () => {
    render(<ShareStatus share={{ ...share, mountTargetIP: '10.0.0.4' }} />);
    expect(screen.getByText('stable')).toBeTruthy();
  });
});
