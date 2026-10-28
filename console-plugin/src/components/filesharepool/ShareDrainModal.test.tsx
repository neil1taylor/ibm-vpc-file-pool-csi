import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sPatch } from '@openshift-console/dynamic-plugin-sdk';
import ShareDrainModal from './ShareDrainModal';
import { FileSharePool, PoolShareStatus } from '../../types';

const pool = (drain?: string[]) =>
  ({ metadata: { name: 'pool-a' }, spec: { drainShares: drain } }) as unknown as FileSharePool;
const share = { shareID: 'share-1', shareName: 'pool-a-share-1', pvcCount: 2 } as PoolShareStatus;

beforeEach(() => jest.clearAllMocks());

describe('ShareDrainModal', () => {
  it('explains what draining does before it does it', () => {
    render(<ShareDrainModal isOpen pool={pool()} share={share} action="drain" onClose={jest.fn()} onDone={jest.fn()} />);
    expect(screen.getByText(/Drain share pool-a-share-1\?/)).toBeTruthy();
    expect(screen.getByText(/2 existing volumes/)).toBeTruthy();
    expect(k8sPatch).not.toHaveBeenCalled();
  });

  it('adds the share to the drain list and keeps the others', async () => {
    (k8sPatch as jest.Mock).mockResolvedValue({});
    const onDone = jest.fn();
    render(<ShareDrainModal isOpen pool={pool(['share-9'])} share={share} action="drain" onClose={jest.fn()} onDone={onDone} />);
    fireEvent.click(screen.getByRole('button', { name: 'Drain share' }));
    await waitFor(() => expect(k8sPatch).toHaveBeenCalled());
    const arg = (k8sPatch as jest.Mock).mock.calls[0][0];
    expect(arg.resource.metadata.name).toBe('pool-a');
    expect(arg.data).toEqual([{ op: 'add', path: '/spec/drainShares', value: ['share-9', 'share-1'] }]);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
  });

  it('takes the share off the list on a return to service', async () => {
    (k8sPatch as jest.Mock).mockResolvedValue({});
    render(<ShareDrainModal isOpen pool={pool(['share-1', 'share-9'])} share={share} action="return" onClose={jest.fn()} onDone={jest.fn()} />);
    expect(screen.getByText(/Return share pool-a-share-1 to service\?/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Return to service' }));
    await waitFor(() => expect(k8sPatch).toHaveBeenCalled());
    expect((k8sPatch as jest.Mock).mock.calls[0][0].data).toEqual([
      { op: 'add', path: '/spec/drainShares', value: ['share-9'] },
    ]);
  });

  it('shows the error and stays open when the update fails', async () => {
    (k8sPatch as jest.Mock).mockRejectedValue(new Error('forbidden'));
    const onDone = jest.fn();
    render(<ShareDrainModal isOpen pool={pool()} share={share} action="drain" onClose={jest.fn()} onDone={onDone} />);
    fireEvent.click(screen.getByRole('button', { name: 'Drain share' }));
    await waitFor(() => expect(screen.getByText(/forbidden/)).toBeTruthy());
    expect(onDone).not.toHaveBeenCalled();
  });

  it('renders nothing when closed', () => {
    render(<ShareDrainModal isOpen={false} pool={pool()} share={share} action="drain" onClose={jest.fn()} onDone={jest.fn()} />);
    expect(screen.queryByText(/Drain share/)).toBeNull();
  });
});
