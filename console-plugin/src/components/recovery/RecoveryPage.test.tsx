import React from 'react';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { k8sCreate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import RecoveryPage from './RecoveryPage';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const NOW = new Date('2026-10-28T12:00:00Z');

const volume = (subVolume: string, pvcName: string, ns = 'apps') => ({
  subVolume,
  pvcName,
  pvcNamespace: ns,
  requestedGB: 10,
  subPath: `/${subVolume}`,
  policy: 'nightly',
  lastSync: '2026-10-28T11:30:00Z',
});

const plannedPlan = (over: Record<string, unknown> = {}) => ({
  metadata: { name: 'console-plan-1', creationTimestamp: '2026-10-28T11:55:00Z' },
  spec: { action: 'Plan', source: 'dr-data' },
  status: {
    phase: 'Planned',
    source: 'dr-data',
    startedAt: '2026-10-28T11:57:00Z',
    finishedAt: '2026-10-28T11:58:00Z',
    rpo: '30m0s',
    vms: [
      {
        name: 'apps/web',
        moment: '2026-10-28T11:30:00Z',
        moments: ['2026-10-28T11:30:00Z', '2026-10-28T11:00:00Z'],
        disks: [
          { subVolume: 'pvc-a', rewind: false },
          { subVolume: 'pvc-b', rewind: true },
        ],
      },
    ],
    volumes: [volume('pvc-a', 'web-root'), volume('pvc-b', 'web-data')],
  },
  ...over,
});

let data: Record<string, unknown[]>;
const receiver = (claim: string) => ({
  metadata: { name: `r-${claim}`, labels: { 'app.kubernetes.io/component': 'replication-receiver' } },
  spec: { template: { spec: { volumes: [{ name: 'data', persistentVolumeClaim: { claimName: claim } }] } } },
});

beforeEach(() => {
  jest.useFakeTimers({ doNotFake: ['setTimeout', 'clearTimeout', 'setImmediate', 'nextTick', 'queueMicrotask'] }).setSystemTime(NOW);
  (k8sCreate as jest.Mock).mockReset();
  (k8sCreate as jest.Mock).mockImplementation(async ({ data: d }) => d);
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  data = {
    Failover: [plannedPlan()],
    FileSharePool: [{ metadata: { name: 'dr-pool' }, spec: {}, status: { phase: 'Ready' } }],
    ReplicationPolicy: [{ metadata: { name: 'nightly' }, spec: { schedule: '1h' } }],
    Deployment: [receiver('dr-data')],
  };
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } } | null) => [
    req ? data[req.groupVersionKind.kind] ?? [] : undefined,
    true,
    undefined,
  ]);
});
afterEach(() => jest.useRealTimers());

const created = () => (k8sCreate as jest.Mock).mock.calls.map((c) => c[0].data);

describe('RecoveryPage access', () => {
  it('says which role is needed when the user cannot create a Failover', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<RecoveryPage />);
    expect(screen.getByText(/not allowed to fail over/i)).toBeTruthy();
    expect(screen.getByText(/failover-admin/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Refresh' })).toBeNull();
    expect(k8sCreate).not.toHaveBeenCalled();
  });
});

describe('RecoveryPage plan table', () => {
  it('shows the VMs of the newest plan without making a new one', () => {
    render(<RecoveryPage />);
    expect(screen.getByText('apps/web')).toBeTruthy();
    expect(screen.getByText('30m')).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
  });

  it('makes a Plan on load when there is none, with the only source filled in', async () => {
    data.Failover = [];
    render(<RecoveryPage />);
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec).toEqual({ action: 'Plan', source: 'dr-data' });
  });

  it('makes a new Plan when Refresh is clicked', async () => {
    render(<RecoveryPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Refresh' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec).toEqual({ action: 'Plan', source: 'dr-data' });
  });

  it('asks the operator to choose when there are several sources, and makes no plan before', async () => {
    data.Failover = [];
    data.Deployment = [receiver('a-data'), receiver('b-data')];
    render(<RecoveryPage />);
    expect(screen.getByLabelText('Source')).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
    fireEvent.change(screen.getByLabelText('Source'), { target: { value: 'b-data' } });
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec.source).toBe('b-data');
  });

  it('leaves the source out when no receiver is found, so the driver default applies', async () => {
    data.Failover = [];
    data.Deployment = [];
    render(<RecoveryPage />);
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec).toEqual({ action: 'Plan' });
  });

  it('shows a still-working state while the plan is made', () => {
    data.Failover = [plannedPlan({ status: { phase: 'Planning', startedAt: '2026-10-28T11:59:00Z' } })];
    render(<RecoveryPage />);
    expect(screen.getByText(/still working/i)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Refresh' })).toBeDisabled();
  });

  it('shows the Failover message when the plan failed', () => {
    data.Failover = [plannedPlan({ status: { phase: 'Failed', message: 'claim "dr-data" not found in namespace kube-system' } })];
    render(<RecoveryPage />);
    expect(screen.getByText(/claim "dr-data" not found/)).toBeTruthy();
  });

  it('warns when the newest copy is older than twice the schedule', () => {
    const p = plannedPlan();
    (p.status as { vms: Array<{ moment: string }> }).vms[0].moment = '2026-10-28T08:00:00Z';
    data.Failover = [p];
    render(<RecoveryPage />);
    expect(screen.getByText(/older than twice the 1h schedule/)).toBeTruthy();
  });

  it('warns when the plan is too old to fail over from', () => {
    const p = plannedPlan();
    (p.status as { finishedAt: string }).finishedAt = '2026-10-28T11:30:00Z';
    data.Failover = [p];
    render(<RecoveryPage />);
    expect(screen.getByText(/too old/i)).toBeTruthy();
  });
});

describe('RecoveryPage busy copy and force', () => {
  const busy = 'a copy is still being applied to pvc-a; wait for it to end, or use --force (an unfinished copy is rolled back first)';

  it('shows the refusal and plans with force only after the extra warning is accepted', async () => {
    data.Failover = [plannedPlan({ status: { phase: 'Failed', message: busy } })];
    render(<RecoveryPage />);
    expect(screen.getByText(new RegExp('a copy is still being applied to pvc-a'))).toBeTruthy();
    const button = screen.getByRole('button', { name: /plan again with force/i });
    expect(button).toBeDisabled();
    fireEvent.click(screen.getByRole('checkbox', { name: /rolled back/i }));
    expect(button).toBeEnabled();
    fireEvent.click(button);
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec).toEqual({ action: 'Plan', source: 'dr-data', force: true });
  });

  it('offers no force option for other errors', () => {
    data.Failover = [plannedPlan({ status: { phase: 'Failed', message: 'something else' } })];
    render(<RecoveryPage />);
    expect(screen.queryByRole('button', { name: /with force/i })).toBeNull();
  });
});

/** A finished Plan for one VM: what the driver makes when the page asks for that VM only. */
const vmPlan = (name: string, specOver: Record<string, unknown> = {}, vm = 'apps/web') => {
  const p = plannedPlan({
    metadata: { name, creationTimestamp: '2026-10-28T12:00:00Z' },
    spec: { action: 'Plan', source: 'dr-data', vms: [vm], ...specOver },
  }) as { status: { vms: Array<{ name: string; disks: Array<{ subVolume: string }> }>; volumes: Array<{ subVolume: string }>; finishedAt: string } };
  p.status.vms = p.status.vms.filter((v) => v.name === vm);
  const disks = new Set(p.status.vms.flatMap((v) => v.disks.map((d) => d.subVolume)));
  p.status.volumes = p.status.volumes.filter((v) => disks.has(v.subVolume));
  p.status.finishedAt = '2026-10-28T12:00:00Z';
  return p;
};

/** The driver finishes the Plan the page just made for a VM. */
const finishVMPlan = (specOver: Record<string, unknown> = {}) => {
  const name = created().find((c) => c.spec.action === 'Plan').metadata.name;
  data.Failover = [...(data.Failover as unknown[]), vmPlan(name, specOver)];
  act(() => {
    jest.advanceTimersByTime(1500);
  });
};

describe('RecoveryPage VM detail', () => {
  const openDetail = () => {
    render(<RecoveryPage />);
    fireEvent.click(screen.getByRole('button', { name: 'apps/web' }));
  };

  it('shows the disks, the rewind note and the moments newest first', () => {
    openDetail();
    expect(screen.getByText('apps/web-root')).toBeTruthy();
    expect(screen.getByText('Copies the whole disk; takes minutes.')).toBeTruthy();
    expect(screen.getByText('Used in place.')).toBeTruthy();
    const moments = screen.getAllByRole('radio').map((r) => r.getAttribute('value'));
    expect(moments).toEqual(['2026-10-28T11:30:00Z', '2026-10-28T11:00:00Z']);
  });

  it('goes back to the table', () => {
    openDetail();
    fireEvent.click(screen.getByRole('button', { name: /back to the vms/i }));
    expect(screen.getByRole('button', { name: 'apps/web' })).toBeTruthy();
  });

  it('makes a Plan for this VM only, and opens the dialog when it is ready', async () => {
    openDetail();
    fireEvent.click(screen.getByRole('button', { name: 'Fail over this VM' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec).toEqual({ action: 'Plan', source: 'dr-data', vms: ['apps/web'] });
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByText(/still working/i)).toBeTruthy();
    // The table still shows the plan for all VMs, and not the plan for this VM.
    finishVMPlan();
    await waitFor(() => expect(screen.getByRole('dialog')).toBeTruthy());
    expect(screen.getByText('Disks of apps/web')).toBeTruthy();
  });

  it('makes a Plan with rewindTo for an older moment', async () => {
    openDetail();
    fireEvent.click(screen.getByRole('radio', { name: /2026-10-28T11:00:00Z|11:00/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Fail over this VM' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec).toEqual({ action: 'Plan', source: 'dr-data', vms: ['apps/web'], rewindTo: '2026-10-28T11:00:00Z' });

    const replanned = vmPlan(created()[0].metadata.name, { rewindTo: '2026-10-28T11:00:00Z' });
    (replanned.status.vms[0] as unknown as { moment: string }).moment = '2026-10-28T11:00:00Z';
    data.Failover = [plannedPlan(), replanned];
    act(() => {
      jest.advanceTimersByTime(1500);
    });
    await waitFor(() => expect(screen.getByRole('dialog')).toBeTruthy());
  });

  it('does not open the dialog when the plan for the VM shows another moment than the chosen one', async () => {
    openDetail();
    fireEvent.click(screen.getByRole('radio', { name: /2026-10-28T11:00:00Z|11:00/ }));
    fireEvent.click(screen.getByRole('button', { name: 'Fail over this VM' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    finishVMPlan({ rewindTo: '2026-10-28T11:00:00Z' }); // it shows 11:30, not 11:00
    await waitFor(() => expect(screen.getByText(/does not show the moment you chose/i)).toBeTruthy());
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('shows the refusal when the plan for the VM fails, and offers force for a busy copy', async () => {
    openDetail();
    fireEvent.click(screen.getByRole('button', { name: 'Fail over this VM' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    const name = created()[0].metadata.name;
    data.Failover = [
      plannedPlan(),
      { ...vmPlan(name), status: { phase: 'Failed', message: 'a copy is still being applied to pvc-a; wait for it to end, or use --force' } },
    ];
    act(() => {
      jest.advanceTimersByTime(1500);
    });
    await waitFor(() => expect(screen.getByText(/a copy is still being applied to pvc-a/)).toBeTruthy());
    expect(screen.queryByRole('dialog')).toBeNull();
    fireEvent.click(screen.getByRole('checkbox', { name: /rolled back/i }));
    (k8sCreate as jest.Mock).mockClear();
    fireEvent.click(screen.getByRole('button', { name: /plan again with force/i }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec).toEqual({ action: 'Plan', source: 'dr-data', vms: ['apps/web'], force: true });
  });
});

describe('RecoveryPage confirm dialog and execute', () => {
  const openDialog = async (): Promise<HTMLElement> => {
    render(<RecoveryPage />);
    fireEvent.click(screen.getByRole('button', { name: 'apps/web' }));
    fireEvent.click(screen.getByRole('button', { name: 'Fail over this VM' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    finishVMPlan();
    const dialog = await screen.findByRole('dialog');
    (k8sCreate as jest.Mock).mockClear();
    return dialog;
  };
  const executes = () => created().filter((c) => c.spec.action === 'Execute');

  it('names the VM, the moment, the disks to rewind and the PVCs that will be made', async () => {
    const dialog = await openDialog();
    expect(within(dialog).getAllByText(/apps\/web/).length).toBeGreaterThan(0);
    expect(within(dialog).getByText(/2026-10-28T11:30:00Z/)).toBeTruthy();
    expect(within(dialog).getByText(/1 disk to rewind/)).toBeTruthy();
    expect(within(dialog).getByText('apps/web-root')).toBeTruthy();
    expect(within(dialog).getByText('apps/web-data')).toBeTruthy();
    expect(within(dialog).queryByText(/fails over all/i)).toBeNull();
  });

  it('needs the VM name typed, and the DR pool chosen when there are several', async () => {
    data.FileSharePool = [
      { metadata: { name: 'dr-pool' }, spec: {}, status: { phase: 'Ready' } },
      { metadata: { name: 'dr-pool-2' }, spec: {}, status: { phase: 'Ready' } },
    ];
    const dialog = await openDialog();
    const go = within(dialog).getByRole('button', { name: 'Fail over' });
    expect(go).toBeDisabled();
    fireEvent.change(within(dialog).getByLabelText(/type apps\/web/i), { target: { value: 'apps/web' } });
    expect(go).toBeDisabled();
    fireEvent.change(within(dialog).getByLabelText('DR pool'), { target: { value: 'dr-pool-2' } });
    expect(go).toBeEnabled();
  });

  it('asks for the VM name even when the copy holds other VMs, and does not fail them over', async () => {
    const p = plannedPlan();
    (p.status as { vms: unknown[] }).vms.push({
      name: 'apps/db',
      moment: '2026-10-28T11:30:00Z',
      disks: [{ subVolume: 'pvc-c', rewind: false }],
    });
    data.Failover = [p];
    const dialog = await openDialog();
    expect(within(dialog).getByLabelText(/type apps\/web/i)).toBeTruthy();
    expect(within(dialog).queryByText(/apps\/db/)).toBeNull();
    fireEvent.change(within(dialog).getByLabelText(/type apps\/web/i), { target: { value: 'apps/web' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Fail over' }));
    await waitFor(() => expect(executes()).toHaveLength(1));
    expect(executes()[0].spec.vms).toEqual(['apps/web']);
  });

  it('creates an Execute for the VM with the source of the confirmed plan', async () => {
    const dialog = await openDialog();
    fireEvent.change(within(dialog).getByLabelText(/type apps\/web/i), { target: { value: 'apps/web' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Fail over' }));
    await waitFor(() => expect(executes()).toHaveLength(1));
    expect(executes()[0].spec).toEqual({ action: 'Execute', source: 'dr-data', vms: ['apps/web'], drPool: 'dr-pool' });
  });

  it('copies rewindTo, force and vms from the plan the dialog was opened for', async () => {
    data.Failover = [plannedPlan({ spec: { action: 'Plan', source: 'dr-data', force: true } })];
    render(<RecoveryPage />);
    fireEvent.click(screen.getByRole('button', { name: 'apps/web' }));
    fireEvent.click(screen.getByRole('button', { name: 'Fail over this VM' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec).toEqual({ action: 'Plan', source: 'dr-data', vms: ['apps/web'], force: true });
    finishVMPlan({ rewindTo: '2026-10-28T11:30:00Z', force: true });
    const dialog = await screen.findByRole('dialog');
    (k8sCreate as jest.Mock).mockClear();
    fireEvent.change(within(dialog).getByLabelText(/type apps\/web/i), { target: { value: 'apps/web' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Fail over' }));
    await waitFor(() => expect(executes()).toHaveLength(1));
    expect(executes()[0].spec).toEqual({
      action: 'Execute',
      source: 'dr-data',
      rewindTo: '2026-10-28T11:30:00Z',
      force: true,
      vms: ['apps/web'],
      drPool: 'dr-pool',
    });
  });

  it('shows the refusal of the webhook in the dialog', async () => {
    const dialog = await openDialog();
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('the Plan for source "dr-data" is older than 15 minutes'));
    fireEvent.change(within(dialog).getByLabelText(/type apps\/web/i), { target: { value: 'apps/web' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Fail over' }));
    await waitFor(() => expect(within(dialog).getByText(/older than 15 minutes/)).toBeTruthy());
  });
});

describe('RecoveryPage fail over all VMs', () => {
  const withTwoVMs = () => {
    const p = plannedPlan();
    (p.status as { vms: unknown[] }).vms.push({
      name: 'apps/db',
      moment: '2026-10-28T11:30:00Z',
      disks: [{ subVolume: 'pvc-c', rewind: false }],
    });
    (p.status as { volumes: unknown[] }).volumes.push(volume('pvc-c', 'db-root'));
    data.Failover = [p];
  };

  it('offers an explicit all-VMs option that uses the plan on screen and asks for the source name', async () => {
    withTwoVMs();
    render(<RecoveryPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Fail over all VMs' }));
    const dialog = screen.getByRole('dialog');
    expect(k8sCreate).not.toHaveBeenCalled();
    expect(within(dialog).getByText(/all 2 VMs/i)).toBeTruthy();
    expect(within(dialog).getByText('apps/db-root')).toBeTruthy();
    fireEvent.change(within(dialog).getByLabelText(/type dr-data/i), { target: { value: 'dr-data' } });
    fireEvent.click(within(dialog).getByRole('button', { name: 'Fail over' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect(created()[0].spec).toEqual({ action: 'Execute', source: 'dr-data', drPool: 'dr-pool' });
  });
});

describe('RecoveryPage progress and result', () => {
  const withExecute = (status: Record<string, unknown>) => {
    data.Failover = [
      plannedPlan(),
      {
        metadata: { name: 'console-execute-1', creationTimestamp: '2026-10-28T11:59:00Z' },
        spec: { action: 'Execute', source: 'dr-data', drPool: 'dr-pool', vms: ['apps/web'] },
        status,
      },
    ];
  };
  const runExecute = async () => {
    render(<RecoveryPage />);
    fireEvent.click(screen.getByRole('button', { name: 'apps/web' }));
    fireEvent.click(screen.getByRole('button', { name: 'Fail over this VM' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    finishVMPlan();
    const dialog = await screen.findByRole('dialog');
    fireEvent.change(within(dialog).getByLabelText(/type apps\/web/i), { target: { value: 'apps/web' } });
    (k8sCreate as jest.Mock).mockImplementation(async ({ data: d }) => ({ ...d, metadata: { ...d.metadata, name: 'console-execute-1' } }));
    fireEvent.click(within(dialog).getByRole('button', { name: 'Fail over' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(2));
  };

  it('shows a clear still-working state while the Execute runs', async () => {
    withExecute({ phase: 'Executing', startedAt: '2026-10-28T11:57:30Z' });
    await runExecute();
    await waitFor(() => expect(screen.getByText(/the failover is running/i)).toBeTruthy());
    expect(screen.getByText(/can take minutes/i)).toBeTruthy();
    expect(screen.getByText(/2m 3\ds/)).toBeTruthy();
  });

  it('lists the created PVCs with links when done', async () => {
    withExecute({
      phase: 'Done',
      createdPVCs: [{ namespace: 'apps', name: 'web-root', pv: 'pv-1', subVolume: 'pvc-a' }],
    });
    await runExecute();
    await waitFor(() => expect(screen.getByText(/the failover is done/i)).toBeTruthy());
    const link = screen.getByRole('link', { name: 'apps/web-root' });
    expect(link.getAttribute('href')).toBe('/k8s/ns/apps/persistentvolumeclaims/web-root');
  });

  it('shows the message when the Execute failed', async () => {
    withExecute({ phase: 'Failed', message: 'VM apps/web now shows 2026-10-28T11:40:00Z, the plan showed 2026-10-28T11:30:00Z' });
    await runExecute();
    await waitFor(() => expect(screen.getByText(/now shows 2026-10-28T11:40:00Z/)).toBeTruthy());
  });
});
