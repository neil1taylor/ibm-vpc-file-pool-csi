import React from 'react';
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import RestoreFilesDialog from './RestoreFilesDialog';
import { BrowseEntry, GatewayError, toB64 } from '../../utils/backup-browse';
import { Selected, SelectionMap, createRestore, getRestore, selectionKey } from '../../utils/backup-file-restore';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));
jest.mock('../../utils/backup-file-restore', () => ({
  ...jest.requireActual('../../utils/backup-file-restore'),
  createRestore: jest.fn(),
  getRestore: jest.fn(),
}));

const text = (s: string) => new Uint8Array(Buffer.from(s, 'utf8'));
const entry = (name: string, over: Partial<BrowseEntry> = {}): BrowseEntry => ({
  name: toB64(text(name)),
  type: 'file',
  size: 2048,
  mode: 0o644,
  uid: 0,
  gid: 0,
  mtime: '2026-10-22T09:15:00Z',
  readable: true,
  ...over,
});
const pick = (items: Array<[string[], BrowseEntry]>): SelectionMap => {
  const m: SelectionMap = new Map();
  items.forEach(([parents, e]) => {
    const s: Selected = { path: [...parents.map((p) => toB64(text(p))), e.name], entry: e };
    m.set(selectionKey(s.path), s);
  });
  return m;
};

const onClose = jest.fn();
const renderDialog = (selected: SelectionMap, moment = '2026-10-22T09:15:00Z') =>
  render(<RestoreFilesDialog policy="nightly" namespace="apps" pvc="db" moment={moment} selected={selected} onClose={onClose} />);

const rights = (denied: string[] = []) =>
  (useAccessReview as jest.Mock).mockImplementation((a: { resource: string; verb: string }) => [!denied.includes(`${a.resource}:${a.verb}`), false]);

const selected = () => pick([[[], entry('a.txt')], [['var'], entry('index.php', { size: 1024 })], [[], entry('etc', { type: 'dir' })], [[], entry('ln', { type: 'symlink' })]]);

beforeEach(() => {
  jest.clearAllMocks();
  jest.useRealTimers();
  rights();
});

describe('RestoreFilesDialog form', () => {
  it('shows what will be written, the moment and that nothing is overwritten', () => {
    renderDialog(selected());
    expect(screen.getByText(/2 files/)).toBeTruthy();
    expect(screen.getByText(/1 folder/)).toBeTruthy();
    expect(screen.getByText(/1 link/)).toBeTruthy();
    expect(screen.getByText(/3\.0 KiB/)).toBeTruthy();
    expect(screen.getByText(/2026-10-22 09:15:00 UTC/)).toBeTruthy();
    expect(screen.getByText(/Nothing is overwritten/i)).toBeTruthy();
    expect(screen.getByText(/with everything in it/i)).toBeTruthy();
  });

  it('explains a restore from inside a VM disk: the file system stays at the top, and it can be slow', () => {
    render(<RestoreFilesDialog policy="nightly" namespace="apps" pvc="web-root" moment="" guest selected={selected()} onClose={onClose} />);
    expect(screen.getByText('Files from inside a VM disk')).toBeTruthy();
    expect(screen.getByText(/name of the file system stays at the top/)).toBeTruthy();
    expect(screen.queryByText('You chose the Latest copy')).toBeNull();
  });

  it('warns that the latest copy can change', () => {
    renderDialog(selected(), '');
    expect(screen.getByText(/You chose the Latest copy/)).toBeTruthy();
    expect(screen.getByText(/can change while it is copied/i)).toBeTruthy();
  });

  it('defaults to a new claim in the namespace of the source, named after the source claim', () => {
    renderDialog(selected());
    expect((screen.getByLabelText('Claim name') as HTMLInputElement).value).toBe('db-restored');
    expect((screen.getByLabelText('Namespace') as HTMLInputElement).value).toBe('apps');
    expect((screen.getByLabelText('A new claim') as HTMLInputElement).checked).toBe(true);
  });

  it('sends the request with escaped paths', async () => {
    (createRestore as jest.Mock).mockResolvedValue({ name: 'bfr-1', phase: 'Pending', targetNamespace: 'apps', targetName: 'db-restored' });
    (getRestore as jest.Mock).mockResolvedValue({ name: 'bfr-1', phase: 'Pending', targetNamespace: 'apps', targetName: 'db-restored' });
    renderDialog(selected());
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }));
    await waitFor(() => expect(createRestore).toHaveBeenCalled());
    expect((createRestore as jest.Mock).mock.calls[0][0]).toEqual({
      policy: 'nightly',
      namespace: 'apps',
      pvc: 'db',
      moment: '2026-10-22T09:15:00Z',
      paths: ['/a.txt', '/etc', '/ln', '/var/index.php'],
      targetNamespace: 'apps',
      targetName: 'db-restored',
    });
  });

  it('sends the size and the class of a new claim', async () => {
    (createRestore as jest.Mock).mockResolvedValue({ name: 'bfr-1', phase: 'Pending', targetNamespace: 'apps', targetName: 'x' });
    (getRestore as jest.Mock).mockResolvedValue({ name: 'bfr-1', phase: 'Pending', targetNamespace: 'apps', targetName: 'x' });
    (useK8sWatchResource as jest.Mock).mockReturnValue([[{ metadata: { name: 'fast' } }], true, undefined]);
    renderDialog(selected());
    fireEvent.change(screen.getByLabelText('Size in GiB'), { target: { value: '20' } });
    fireEvent.change(screen.getByLabelText('Storage class'), { target: { value: 'fast' } });
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }));
    await waitFor(() => expect(createRestore).toHaveBeenCalled());
    const req = (createRestore as jest.Mock).mock.calls[0][0];
    expect(req.sizeGB).toBe(20);
    expect(req.storageClassName).toBe('fast');
  });

  it('restores into a new folder of an existing claim', async () => {
    (createRestore as jest.Mock).mockResolvedValue({ name: 'bfr-1', phase: 'Pending', targetNamespace: 'apps', targetName: 'db' });
    (getRestore as jest.Mock).mockResolvedValue({ name: 'bfr-1', phase: 'Pending', targetNamespace: 'apps', targetName: 'db' });
    renderDialog(selected());
    fireEvent.click(screen.getByLabelText('A new folder in an existing claim'));
    fireEvent.change(screen.getByLabelText('Claim name'), { target: { value: 'db' } });
    expect(screen.queryByLabelText('Size in GiB')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }));
    await waitFor(() => expect(createRestore).toHaveBeenCalled());
    const req = (createRestore as jest.Mock).mock.calls[0][0];
    expect(req.existing).toBe(true);
    expect(req.targetName).toBe('db');
    expect(req.sizeGB).toBeUndefined();
  });

  it('turns off the existing-claim option and says which rights are missing', () => {
    rights(['persistentvolumeclaims:update', 'pods:create']);
    renderDialog(selected());
    expect((screen.getByLabelText('A new folder in an existing claim') as HTMLInputElement).disabled).toBe(true);
    expect(screen.getByText(/update.*persistentvolumeclaims/s)).toBeTruthy();
    expect(screen.getByText(/create.*pods/s)).toBeTruthy();
  });

  it('turns off the whole restore and names the right when a new claim may not be made', () => {
    rights(['persistentvolumeclaims:create']);
    renderDialog(selected());
    expect((screen.getByRole('button', { name: 'Restore' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/You may not make a claim here/)).toBeTruthy();
  });

  it('asks for the rights of the namespace that is typed', () => {
    renderDialog(selected());
    fireEvent.change(screen.getByLabelText('Namespace'), { target: { value: 'web' } });
    const last = (useAccessReview as jest.Mock).mock.calls.slice(-3).map((c) => c[0].namespace);
    expect(last).toEqual(['web', 'web', 'web']);
  });

  it('refuses a bad claim name before it asks the gateway', () => {
    renderDialog(selected());
    fireEvent.change(screen.getByLabelText('Claim name'), { target: { value: 'Bad Name' } });
    expect((screen.getByRole('button', { name: 'Restore' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/lower case letters/)).toBeTruthy();
  });

  it('refuses more than 1000 paths', () => {
    const m: SelectionMap = new Map();
    for (let i = 0; i < 1001; i++) {
      const e = entry(`f${i}`);
      m.set(selectionKey([e.name]), { path: [e.name], entry: e });
    }
    renderDialog(m);
    expect((screen.getByRole('button', { name: 'Restore' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/at most 1000/i)).toBeTruthy();
  });

  it('shows chosen names as text and never as HTML', () => {
    const hostile = '<img src=x onerror=alert(1)>';
    const { container } = renderDialog(pick([[[], entry(hostile)]]));
    expect(screen.getByText(`/${hostile}`)).toBeTruthy();
    expect(container.ownerDocument.body.querySelector('img')).toBeNull();
  });

  it('closes with Cancel', () => {
    renderDialog(selected());
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onClose).toHaveBeenCalled();
  });
});

describe('RestoreFilesDialog refusals', () => {
  const refuse = async (err: GatewayError) => {
    (createRestore as jest.Mock).mockRejectedValue(err);
    renderDialog(selected());
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }));
    await screen.findByRole('heading', { name: /could not be started/i, hidden: true }).catch(() => undefined);
  };

  it('offers another name when the name is taken', async () => {
    await refuse(new GatewayError(409, 'exists', 'that claim exists'));
    expect(await screen.findByText(/already exists/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Use db-restored-2' }));
    expect((screen.getByLabelText('Claim name') as HTMLInputElement).value).toBe('db-restored-2');
  });

  it('says which right is missing for a 403', async () => {
    await refuse(new GatewayError(403, 'forbidden', 'you may not create or change claims in that namespace'));
    expect(await screen.findByText(/persistentvolumeclaims/)).toBeTruthy();
  });

  it('says there are too many open restores for a 429', async () => {
    await refuse(new GatewayError(429, 'too_many', 'you have 10 restores that have not ended; the limit is 10'));
    expect(await screen.findByText(/too many open restores/i)).toBeTruthy();
  });

  it('says the backup is gone for a 404', async () => {
    await refuse(new GatewayError(404, 'not_found', 'no backup of that volume'));
    expect(await screen.findByText(/no backup of this volume/i)).toBeTruthy();
  });
});

describe('RestoreFilesDialog progress', () => {
  const base = { name: 'bfr-1', targetNamespace: 'apps', targetName: 'db-restored', files: 0, dirs: 0, links: 0, skipped: 0, unreadable: 0, bytes: 0 };

  it('polls with short requests until the restore ends, then shows the result with links', async () => {
    jest.useFakeTimers();
    (createRestore as jest.Mock).mockResolvedValue({ ...base, phase: 'Pending' });
    (getRestore as jest.Mock)
      .mockResolvedValueOnce({ ...base, phase: 'Restoring' })
      .mockResolvedValue({
        ...base,
        phase: 'Done',
        files: 2,
        dirs: 3,
        links: 1,
        bytes: 3072,
        sizeGB: 1,
        folder: 'restored-nightly-20261022',
        restoredMoment: '2026-10-22T09:15:00Z',
      });
    renderDialog(selected());
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }));
    expect(await screen.findByText(/Waiting to start/)).toBeTruthy();
    await act(async () => {
      jest.advanceTimersByTime(2100);
    });
    expect(await screen.findByText(/Copying the files/)).toBeTruthy();
    await act(async () => {
      jest.advanceTimersByTime(2100);
    });
    expect(await screen.findByText(/Done/)).toBeTruthy();
    expect(getRestore).toHaveBeenCalledWith('bfr-1');
    expect(screen.getByRole('link', { name: 'apps/db-restored' }).getAttribute('href')).toBe('/k8s/ns/apps/persistentvolumeclaims/db-restored');
    expect(screen.getByText('restored-nightly-20261022')).toBeTruthy();
    expect(screen.getByText(/2 files/)).toBeTruthy();
    expect(screen.getByText(/3 folders/)).toBeTruthy();
    expect(screen.getByText(/3\.0 KiB/)).toBeTruthy();
    const calls = (getRestore as jest.Mock).mock.calls.length;
    await act(async () => {
      jest.advanceTimersByTime(10000);
    });
    expect((getRestore as jest.Mock).mock.calls.length).toBe(calls);
  });

  it('shows why a restore failed', async () => {
    jest.useFakeTimers();
    (createRestore as jest.Mock).mockResolvedValue({ ...base, phase: 'Pending' });
    (getRestore as jest.Mock).mockResolvedValue({ ...base, phase: 'Failed', message: 'not enough room in the claim' });
    renderDialog(selected());
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }));
    await screen.findByText(/Waiting to start/);
    await act(async () => {
      jest.advanceTimersByTime(2100);
    });
    expect(await screen.findByText(/not enough room in the claim/)).toBeTruthy();
    expect(screen.getByText(/Failed/)).toBeTruthy();
  });

  it('stops polling when the dialog is closed', async () => {
    jest.useFakeTimers();
    (createRestore as jest.Mock).mockResolvedValue({ ...base, phase: 'Pending' });
    (getRestore as jest.Mock).mockResolvedValue({ ...base, phase: 'Restoring' });
    const { unmount } = renderDialog(selected());
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }));
    await screen.findByText(/Waiting to start/);
    await act(async () => {
      jest.advanceTimersByTime(2100);
    });
    unmount();
    const calls = (getRestore as jest.Mock).mock.calls.length;
    await act(async () => {
      jest.advanceTimersByTime(10000);
    });
    expect((getRestore as jest.Mock).mock.calls.length).toBe(calls);
  });

  it('says the check failed after several failed polls, and keeps the restore name', async () => {
    jest.useFakeTimers();
    (createRestore as jest.Mock).mockResolvedValue({ ...base, phase: 'Pending' });
    (getRestore as jest.Mock).mockRejectedValue(new GatewayError(0, '', 'network'));
    renderDialog(selected());
    fireEvent.click(screen.getByRole('button', { name: 'Restore' }));
    await screen.findByText(/Waiting to start/);
    for (let i = 0; i < 4; i++) {
      await act(async () => {
        jest.advanceTimersByTime(2100);
      });
    }
    expect(await screen.findByText(/cannot reach the backup gateway/i)).toBeTruthy();
    expect(screen.getByText(/bfr-1/)).toBeTruthy();
  });
});
