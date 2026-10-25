import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sPatch, useAccessReview } from '@openshift-console/dynamic-plugin-sdk';
import RunControls from './RunControls';
import { PoolBench } from '../../types';

jest.mock('react-router-dom', () => ({ useHistory: () => ({ push: jest.fn() }) }));

jest.mock('../common/DeleteModal', () => ({
  __esModule: true,
  default: ({ resourceName, note }: { resourceName: string; note: React.ReactNode }) => (
    <div data-testid="delete-modal">
      {resourceName} {note}
    </div>
  ),
}));

const measuring = (): PoolBench => ({
  metadata: { name: 't3000' },
  spec: { shareSizeGB: 3900, iops: 3000, control: {} },
  status: { phase: 'Running', stage: 'Measuring', step: 2, totalSteps: 6, steps: [{ vms: 15, pass: true, ended: 'x' }] },
});

beforeEach(() => {
  jest.clearAllMocks();
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sPatch as jest.Mock).mockResolvedValue({});
});

describe('RunControls', () => {
  it('shows the controls that fit the run now, each with what it does', () => {
    render(<RunControls pb={measuring()} />);
    for (const name of ['Pause', 'Hold at this step', 'Measure 15 min longer', 'Skip to the next step', 'Stop and clean up']) {
      expect(screen.getByRole('button', { name })).toBeTruthy();
    }
    expect(screen.queryByRole('button', { name: 'Resume' })).toBeNull();
  });

  it('pauses the run at once', async () => {
    render(<RunControls pb={measuring()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Pause' }));
    await waitFor(() => expect(k8sPatch).toHaveBeenCalled());
    expect((k8sPatch as jest.Mock).mock.calls[0][0].data).toEqual([
      { op: 'add', path: '/spec/control', value: { paused: true } },
    ]);
  });

  it('asks before it stops the run', async () => {
    render(<RunControls pb={measuring()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Stop and clean up' }));
    expect(k8sPatch).not.toHaveBeenCalled();
    expect(screen.getByText(/deletes the test VMs, the namespace and the pool/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Stop the run' }));
    await waitFor(() => expect(k8sPatch).toHaveBeenCalled());
    expect((k8sPatch as jest.Mock).mock.calls[0][0].data[0].value).toEqual({ stop: true });
  });

  it('downloads the step table and the report', () => {
    const create = jest.fn(() => 'blob:x');
    const revoke = jest.fn();
    Object.assign(URL, { createObjectURL: create, revokeObjectURL: revoke });
    render(<RunControls pb={measuring()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Step table (CSV)' }));
    fireEvent.click(screen.getByRole('button', { name: 'Report (Markdown)' }));
    expect(create).toHaveBeenCalledTimes(2);
    expect(screen.getByRole('button', { name: 'Print or save as PDF' })).toBeTruthy();
  });

  it('says why the controls are off without the right to change the run', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<RunControls pb={measuring()} />);
    expect(screen.getByRole('button', { name: 'Pause' })).toHaveProperty('disabled', true);
    expect(screen.getByText(/need the right to update PoolBench objects/)).toBeTruthy();
  });

  it('shows the error when the change fails', async () => {
    (k8sPatch as jest.Mock).mockRejectedValue(new Error('forbidden'));
    render(<RunControls pb={measuring()} />);
    fireEvent.click(screen.getByRole('button', { name: 'Pause' }));
    await waitFor(() => expect(screen.getByText(/forbidden/)).toBeTruthy());
  });

  it('offers to delete a run that has ended, and says the results go with it', () => {
    const pb = measuring();
    pb.status = { phase: 'Complete', cleanedUp: true };
    render(<RunControls pb={pb} />);
    fireEvent.click(screen.getByRole('button', { name: 'Delete run' }));
    expect(screen.getByTestId('delete-modal').textContent).toMatch(/results/);
  });
});
