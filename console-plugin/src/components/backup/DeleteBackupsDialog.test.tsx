import React from 'react';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { k8sCreate, k8sDelete, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import DeleteBackupsDialog from './DeleteBackupsDialog';
import { buildMeasureBackupObjects, buildRemoveBackupObjects, shareJobName } from '../../utils/backup-jobs';
import { BackupPolicy, BackupVolumeStatus } from '../../types';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const SUB_PATH = '/pvcs/pvc-11111111-2222-3333-4444-555555555555';
const MEASURE_KEY = 'bkmeasure-pvc-11111111-2222-3333-4444-555555555555';
const DELETE_KEY = 'bkdelete-pvc-11111111-2222-3333-4444-555555555555';
const BACKUP = `/backup/nightly/pvcs/pvc-11111111-2222-3333-4444-555555555555`;
const MOMENTS = `/backup/nightly/.moments/pvcs/pvc-11111111-2222-3333-4444-555555555555`;
const SHARE = { mountTargetIP: '10.0.0.1', exportPath: '/share_a' };

const policy = {
  metadata: { name: 'nightly' },
  spec: { snapshotPool: 'snaps', schedule: '6h', selector: {} },
} as BackupPolicy;

const volume = (over: Partial<BackupVolumeStatus> = {}): BackupVolumeStatus => ({
  pvcNamespace: 'apps',
  pvcName: 'old',
  subVolumeName: 'pvc-3',
  subPath: SUB_PATH,
  shareID: 'share-a',
  retained: true,
  restorePoints: 8,
  lastBackupTime: '2026-10-20T00:00:00Z',
  ...over,
});

const pod = (message: string) => ({ status: { containerStatuses: [{ state: { terminated: { message } } }] } });

let restores: unknown[];
let pools: unknown;
let jobs: Record<string, unknown>;
let pods: Record<string, unknown[]>;
const onClose = jest.fn();

beforeEach(() => {
  jest.clearAllMocks();
  restores = [];
  pools = { metadata: { name: 'snaps' }, status: { shares: [{ shareID: 'share-a', mountTargetIP: '10.0.0.1', exportPath: '/share_a' }] } };
  jobs = {
    [shareJobName(MEASURE_KEY)]: { metadata: { name: shareJobName(MEASURE_KEY) }, status: { succeeded: 1 } },
    [shareJobName(DELETE_KEY)]: { metadata: { name: shareJobName(DELETE_KEY) }, status: { succeeded: 1 } },
  };
  pods = { [shareJobName(MEASURE_KEY)]: [pod('12582912')], [shareJobName(DELETE_KEY)]: [] };
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sCreate as jest.Mock).mockResolvedValue({});
  (k8sDelete as jest.Mock).mockResolvedValue({});
  (useK8sWatchResource as jest.Mock).mockImplementation(
    (req: { groupVersionKind: { kind: string }; name?: string; selector?: { matchLabels: Record<string, string> } } | null) => {
      if (!req) return [undefined, true, undefined];
      switch (req.groupVersionKind.kind) {
        case 'BackupRestore':
          return [restores, true, undefined];
        case 'FileSharePool':
          return [pools, true, undefined];
        case 'Job':
          return [jobs[req.name || ''], true, undefined];
        case 'Pod':
          return [pods[req.selector?.matchLabels['storage.ibmcloud.io/share-data-job'] || ''] || [], true, undefined];
        default:
          return [[], true, undefined];
      }
    },
  );
});

const show = (v: BackupVolumeStatus = volume()) => render(<DeleteBackupsDialog policy={policy} volume={v} onClose={onClose} />);
const createdKinds = () => (k8sCreate as jest.Mock).mock.calls.map((c) => c[0].data.kind);

describe('DeleteBackupsDialog refusals', () => {
  it('refuses while the policy still backs up the volume, and starts no Job', async () => {
    show(volume({ retained: false }));
    expect(await screen.findByText(/still backs up apps\/old/)).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
  });

  it('refuses while a restore of the volume is running, and starts no Job', async () => {
    restores = [
      {
        metadata: { name: 'restore-old-abc' },
        spec: { policy: 'nightly', sourceNamespace: 'apps', sourceName: 'old' },
        status: { phase: 'Restoring' },
      },
    ];
    show();
    expect(await screen.findByText(/Restore restore-old-abc of apps\/old has not ended/)).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
  });

  it('says when the share of the backups cannot be found', async () => {
    pools = { metadata: { name: 'snaps' }, status: { shares: [] } };
    show();
    expect(await screen.findByText(/has no share/)).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
  });

  it('names the missing rights and shows the YAML for an administrator, and starts no Job', async () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    show();
    expect(await screen.findByText(/You are not allowed to make and remove Jobs, claims and volumes/)).toBeTruthy();
    expect(screen.getByText(/kind: Job/)).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
  });
});

describe('DeleteBackupsDialog measure step', () => {
  it('starts the measure Job with the same objects as the driver builds, PV first', async () => {
    show();
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(3));
    expect(createdKinds()).toEqual(['PersistentVolume', 'PersistentVolumeClaim', 'Job']);
    const expected = buildMeasureBackupObjects({ key: MEASURE_KEY, namespace: 'kube-system', share: SHARE, backupPath: BACKUP, momentsPath: MOMENTS });
    expect((k8sCreate as jest.Mock).mock.calls.map((c) => c[0].data)).toEqual([expected.pv, expected.pvc, expected.job]);
  });

  it('carries on when the Job exists already', async () => {
    (k8sCreate as jest.Mock).mockRejectedValue({ code: 409 });
    show();
    expect(await screen.findByText(/12\.0 GiB/)).toBeTruthy();
  });

  it('shows the size and the number of moments, and cleans up the measure Job', async () => {
    show();
    expect(await screen.findByText(/12\.0 GiB, 9 moments/)).toBeTruthy();
    await waitFor(() => expect(k8sDelete).toHaveBeenCalledTimes(3));
    expect((k8sDelete as jest.Mock).mock.calls.map((c) => c[0].model.kind)).toEqual(['Job', 'PersistentVolumeClaim', 'PersistentVolume']);
  });

  it('says there is nothing to remove when the folders are empty', async () => {
    pods[shareJobName(MEASURE_KEY)] = [pod('0')];
    show();
    expect(await screen.findByText(/Nothing to remove/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Delete backups' })).toBeNull();
  });

  it('shows an error when the measure Job fails, and offers no delete', async () => {
    jobs[shareJobName(MEASURE_KEY)] = { metadata: { name: 'x' }, status: { failed: 4 } };
    show();
    expect(await screen.findByText(/measuring the backups failed/i)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Delete backups' })).toBeNull();
    expect(createdKinds()).not.toContain(undefined);
  });

  it('shows an error when the Job reports no size', async () => {
    pods[shareJobName(MEASURE_KEY)] = [pod('')];
    show();
    expect(await screen.findByText(/did not report a size/)).toBeTruthy();
  });

  it('shows the error when a Job cannot be started', async () => {
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('jobs.batch is forbidden'));
    show();
    expect(await screen.findByText(/jobs.batch is forbidden/)).toBeTruthy();
  });
});

describe('DeleteBackupsDialog delete step', () => {
  const reachConfirm = async () => {
    show();
    await screen.findByText(/12\.0 GiB, 9 moments/);
    (k8sCreate as jest.Mock).mockClear();
  };
  const dialog = () => screen.getByRole('dialog');
  const deleteButton = () => within(dialog()).getByRole('button', { name: 'Delete backups' }) as HTMLButtonElement;

  it('says what is removed, for good, and keeps Delete off until the claim is typed exactly', async () => {
    await reachConfirm();
    expect(within(dialog()).getByText(/for good/)).toBeTruthy();
    expect(within(dialog()).getByText(BACKUP)).toBeTruthy();
    expect(deleteButton().disabled).toBe(true);
    const input = within(dialog()).getByLabelText('Type the claim to confirm');
    fireEvent.change(input, { target: { value: 'old' } });
    expect(deleteButton().disabled).toBe(true);
    fireEvent.change(input, { target: { value: 'apps/old' } });
    expect(deleteButton().disabled).toBe(false);
  });

  it('starts the delete Job only after the typed confirmation, with the same objects as the driver', async () => {
    await reachConfirm();
    expect(k8sCreate).not.toHaveBeenCalled();
    fireEvent.change(within(dialog()).getByLabelText('Type the claim to confirm'), { target: { value: 'apps/old' } });
    fireEvent.click(deleteButton());
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(3));
    const expected = buildRemoveBackupObjects({ key: DELETE_KEY, namespace: 'kube-system', share: SHARE, backupPath: BACKUP, momentsPath: MOMENTS });
    expect((k8sCreate as jest.Mock).mock.calls.map((c) => c[0].data)).toEqual([expected.pv, expected.pvc, expected.job]);
  });

  it('reports the removal and cleans up the delete Job', async () => {
    await reachConfirm();
    (k8sDelete as jest.Mock).mockClear();
    fireEvent.change(within(dialog()).getByLabelText('Type the claim to confirm'), { target: { value: 'apps/old' } });
    fireEvent.click(deleteButton());
    expect(await screen.findByText(/Removed the backups of apps\/old \(12\.0 GiB\)/)).toBeTruthy();
    await waitFor(() => expect(k8sDelete).toHaveBeenCalledTimes(3));
  });

  it('shows an error when the delete Job fails', async () => {
    jobs[shareJobName(DELETE_KEY)] = { metadata: { name: 'x' }, status: { failed: 4 } };
    await reachConfirm();
    fireEvent.change(within(dialog()).getByLabelText('Type the claim to confirm'), { target: { value: 'apps/old' } });
    fireEvent.click(deleteButton());
    expect(await screen.findByText(/removing the backups failed/i)).toBeTruthy();
  });

  it('closes without removing anything on Cancel', async () => {
    await reachConfirm();
    fireEvent.click(within(dialog()).getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalled();
    expect(k8sCreate).not.toHaveBeenCalled();
  });
});
