import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sCreate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import PoolBenchCreatePage from './PoolBenchCreatePage';

const push = jest.fn();
jest.mock('react-router-dom', () => ({
  useHistory: () => ({ push }),
}));

const node = (name: string, memGiB: number) => ({
  metadata: { name, labels: { 'node-role.kubernetes.io/worker': '' } },
  status: { allocatable: { memory: `${memGiB}Gi` } },
});

let nodes = [node('w1', 128), node('w2', 128), node('w3', 128)];
let images = [{ metadata: { name: 'centos-stream9' } }, { metadata: { name: 'win2k22-perf' } }];

beforeEach(() => {
  jest.clearAllMocks();
  nodes = [node('w1', 128), node('w2', 128), node('w3', 128)];
  images = [{ metadata: { name: 'centos-stream9' } }, { metadata: { name: 'win2k22-perf' } }];
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sCreate as jest.Mock).mockResolvedValue({});
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind?: { kind: string } } | null) => {
    if (req?.groupVersionKind?.kind === 'Node') return [nodes, true, undefined];
    if (req?.groupVersionKind?.kind === 'DataSource') return [images, true, undefined];
    return [[], true, undefined];
  });
});

const runButton = () => screen.getByRole('button', { name: /Start the run/ });

describe('PoolBenchCreatePage', () => {
  it('offers the presets', () => {
    render(<PoolBenchCreatePage />);
    for (const p of ['Quick check', 'One tier', 'Three tiers']) {
      expect(screen.getByRole('radio', { name: new RegExp(p) })).toBeTruthy();
    }
  });

  it('shows the IOPS IBM allows for the share size, live', () => {
    render(<PoolBenchCreatePage />);
    expect(screen.getByText(/IBM allows 200 to 40000 IOPS for 3900 GB/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText('Share size (GB)'), { target: { value: '200' } });
    expect(screen.getByText(/IBM allows 100 to 6000 IOPS for 200 GB/)).toBeTruthy();
  });

  it('shows the load of each step against the share limit, and the run time', () => {
    render(<PoolBenchCreatePage />);
    const table = screen.getByRole('grid', { name: 'Load per step' });
    expect(table.textContent).toContain('45');
    expect(table.textContent).toContain('2160');
    expect(table.textContent).toContain('22%');
    expect(screen.getByText(/about 6 h 30 min/)).toBeTruthy();
  });

  it('checks the images and the room on the nodes with one node down', () => {
    render(<PoolBenchCreatePage />);
    const checks = screen.getByTestId('poolbench-checks');
    expect(checks.textContent).toContain('centos-stream9');
    expect(checks.textContent).toContain('win2k22-perf');
    expect(checks.textContent).toMatch(/267 GiB needed, 256 GiB on the nodes with one node down/);
    expect(runButton()).toHaveProperty('disabled', true);
  });

  it('starts the run when every check passes', async () => {
    nodes = [node('w1', 256), node('w2', 256), node('w3', 256)];
    render(<PoolBenchCreatePage />);
    expect(runButton()).toHaveProperty('disabled', false);
    fireEvent.click(runButton());
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    const call = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(call.data.metadata.name).toBe('perf-test');
    expect(call.data.spec.iops).toBe(10000);
    expect(push).toHaveBeenCalledWith('/vpc-file-pools/poolbench/perf-test');
  });

  it('makes one run per tier for the three tiers', async () => {
    nodes = [node('w1', 256), node('w2', 256), node('w3', 256)];
    render(<PoolBenchCreatePage />);
    fireEvent.click(screen.getByRole('radio', { name: /Three tiers/ }));
    fireEvent.click(runButton());
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(3));
    const names = (k8sCreate as jest.Mock).mock.calls.map((c) => c[0].data.metadata.name);
    expect(names).toEqual(['perf-test-3000', 'perf-test-10000', 'perf-test-20000']);
  });

  it('blocks the run when an image is missing', () => {
    nodes = [node('w1', 256), node('w2', 256), node('w3', 256)];
    images = [{ metadata: { name: 'centos-stream9' } }];
    render(<PoolBenchCreatePage />);
    expect(screen.getByTestId('poolbench-checks').textContent).toMatch(/win2k22-perf.*missing/);
    expect(runButton()).toHaveProperty('disabled', true);
  });

  it('shows the error of the admission check', async () => {
    nodes = [node('w1', 256), node('w2', 256), node('w3', 256)];
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('admission webhook denied the request: spec.iops ...'));
    render(<PoolBenchCreatePage />);
    fireEvent.click(runButton());
    await waitFor(() => expect(screen.getByText(/admission webhook denied/)).toBeTruthy());
  });

  it('says who may start a run', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<PoolBenchCreatePage />);
    expect(screen.getByText(/needs the right to create PoolBench objects/)).toBeTruthy();
  });
});
