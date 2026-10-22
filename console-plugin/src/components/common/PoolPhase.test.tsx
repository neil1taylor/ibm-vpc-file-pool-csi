import React from 'react';
import { render, screen } from '@testing-library/react';
import PoolPhase from './PoolPhase';
import { FileSharePool } from '../../types';

function pool(phase: string, conditions: NonNullable<FileSharePool['status']>['conditions']): FileSharePool {
  return {
    metadata: { name: 'p' },
    spec: {} as FileSharePool['spec'],
    status: { phase, conditions } as FileSharePool['status'],
  };
}

describe('PoolPhase', () => {
  it('shows the reason under a phase that is not Ready', () => {
    render(
      <PoolPhase
        pool={pool('Degraded', [
          { type: 'SharesReady', status: 'False', reason: 'NoMountTarget', message: 'share-1 has no mount target' },
        ])}
      />,
    );
    expect(screen.getByText('Degraded')).toBeTruthy();
    expect(screen.getByText('share-1 has no mount target')).toBeTruthy();
  });

  it('shows only the phase when the pool is Ready', () => {
    const { container } = render(<PoolPhase pool={pool('Ready', [])} />);
    expect(screen.getByText('Ready')).toBeTruthy();
    expect(container.querySelector('small')).toBeNull();
  });
});
