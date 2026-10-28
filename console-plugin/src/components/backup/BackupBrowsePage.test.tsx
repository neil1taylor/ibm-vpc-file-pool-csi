import React from 'react';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import BackupBrowsePage from './BackupBrowsePage';
import { BrowseEntry, GatewayError, listFolder, listMoments, toB64 } from '../../utils/backup-browse';
import { useBrowseSession } from '../../utils/use-browse-session';

let mockSearch = '?claim=apps%2Fdb';
const mockReplace = jest.fn();
jest.mock('react-router-dom', () => ({
  useParams: () => ({ name: 'nightly' }),
  useLocation: () => ({ search: mockSearch }),
  useHistory: () => ({ push: jest.fn(), replace: mockReplace }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

jest.mock('../../utils/use-browse-session', () => ({ useBrowseSession: jest.fn() }));
jest.mock('../../utils/backup-browse', () => ({
  ...jest.requireActual('../../utils/backup-browse'),
  listFolder: jest.fn(),
  listMoments: jest.fn(),
}));

const text = (s: string) => new Uint8Array(Buffer.from(s, 'utf8'));
const entry = (name: string | Uint8Array, over: Partial<BrowseEntry> = {}): BrowseEntry => ({
  name: toB64(typeof name === 'string' ? text(name) : name),
  type: 'file',
  size: 2048,
  mode: 0o644,
  uid: 0,
  gid: 0,
  mtime: '2026-10-22T09:15:00Z',
  readable: true,
  ...over,
});

const reopen = jest.fn();
const ready = { status: 'ready', session: 'bb-1', reopen };

beforeEach(() => {
  jest.clearAllMocks();
  mockSearch = '?claim=apps%2Fdb';
  (useBrowseSession as jest.Mock).mockReturnValue(ready);
  (listMoments as jest.Mock).mockResolvedValue(['20261021T091500Z', '20261022T091500Z']);
  (listFolder as jest.Mock).mockResolvedValue({ entries: [] });
});

describe('BackupBrowsePage states', () => {
  it('asks for a claim when the link has none', () => {
    mockSearch = '';
    render(<BackupBrowsePage />);
    expect(screen.getByText(/No volume chosen/)).toBeTruthy();
    expect(useBrowseSession).toHaveBeenCalledWith(undefined);
  });

  it('shows the starting state with the reader message', () => {
    (useBrowseSession as jest.Mock).mockReturnValue({ status: 'starting', message: 'starting the reader', reopen });
    render(<BackupBrowsePage />);
    expect(screen.getByText(/Starting the reader/)).toBeTruthy();
    expect(screen.getByText(/starting the reader/)).toBeTruthy();
    expect(listFolder).not.toHaveBeenCalled();
  });

  it('shows an ended session and opens it again', () => {
    (useBrowseSession as jest.Mock).mockReturnValue({ status: 'ended', message: 'idle for 15 minutes', reopen });
    render(<BackupBrowsePage />);
    expect(screen.getByText(/session has ended/i)).toBeTruthy();
    expect(screen.getByText(/idle for 15 minutes/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Open again' }));
    expect(reopen).toHaveBeenCalled();
  });

  it('explains a missing right in plain words', () => {
    (useBrowseSession as jest.Mock).mockReturnValue({
      status: 'error',
      error: new GatewayError(403, 'forbidden', 'you may not read the backups of that namespace'),
      reopen,
    });
    render(<BackupBrowsePage />);
    expect(screen.getByText(/get backupfiles/)).toBeTruthy();
    expect(screen.getByText(/apps/, { selector: 'p, div' })).toBeTruthy();
  });

  it('opens the session for the claim and moment of the link', () => {
    mockSearch = '?claim=apps%2Fdb&moment=2026-10-22T09%3A15%3A00Z';
    render(<BackupBrowsePage />);
    expect(useBrowseSession).toHaveBeenCalledWith({
      policy: 'nightly',
      namespace: 'apps',
      pvc: 'db',
      moment: '2026-10-22T09:15:00Z',
    });
  });
});

describe('BackupBrowsePage folder listing', () => {
  it('lists the root folder with sizes and times', async () => {
    (listFolder as jest.Mock).mockResolvedValue({ entries: [entry('var', { type: 'dir' }), entry('index.php')] });
    render(<BackupBrowsePage />);
    expect(await screen.findByText('index.php')).toBeTruthy();
    expect(listFolder).toHaveBeenCalledWith('bb-1', [], undefined);
    expect(screen.getByText('2.0 KiB')).toBeTruthy();
    expect(within(screen.getByRole('grid')).getAllByText('2026-10-22 09:15:00 UTC').length).toBe(2);
  });

  it('shows names as text and never as HTML', async () => {
    const hostile = '<img src=x onerror=alert(1)>';
    (listFolder as jest.Mock).mockResolvedValue({
      entries: [entry(hostile), entry(text('line\nbreak')), entry(new Uint8Array([0x61, 0xff])), entry('a‮b')],
    });
    const { container } = render(<BackupBrowsePage />);
    expect(await screen.findByText(hostile)).toBeTruthy();
    expect(container.querySelector('img')).toBeNull();
    expect(screen.getByText('line\\x0abreak')).toBeTruthy();
    expect(screen.getByText('a\\xff')).toBeTruthy();
    expect(screen.getByText('a\\xe2\\x80\\xaeb')).toBeTruthy();
  });

  it('shows a symbolic link as a link, with its text, and never opens or downloads it', async () => {
    (listFolder as jest.Mock).mockResolvedValue({
      entries: [entry('etc-link', { type: 'symlink', link: toB64(text('/etc')), size: 4 })],
    });
    render(<BackupBrowsePage />);
    expect(await screen.findByText(/etc-link/)).toBeTruthy();
    expect(screen.getByText(/\/etc/)).toBeTruthy();
    expect(screen.getByText(/Link, not followed/i)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'etc-link' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Download' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Download as tar' })).toBeNull();
  });

  it('marks an entry the reader cannot open and offers no download', async () => {
    (listFolder as jest.Mock).mockResolvedValue({ entries: [entry('secret.key', { readable: false })] });
    render(<BackupBrowsePage />);
    expect(await screen.findByText('secret.key')).toBeTruthy();
    expect(screen.getByText('Not readable')).toBeTruthy();
    expect(screen.queryByRole('link', { name: 'Download' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Download as tar' })).toBeNull();
  });

  it('opens a folder, shows breadcrumbs and goes back', async () => {
    (listFolder as jest.Mock).mockImplementation((_s: string, path: string[]) =>
      Promise.resolve({ entries: path.length === 0 ? [entry('var', { type: 'dir' })] : [entry('index.php')] }),
    );
    render(<BackupBrowsePage />);
    fireEvent.click(await screen.findByRole('button', { name: 'var' }));
    expect(await screen.findByText('index.php')).toBeTruthy();
    expect(listFolder).toHaveBeenLastCalledWith('bb-1', [toB64(text('var'))], undefined);
    const crumbs = screen.getByRole('navigation', { name: 'Folders' });
    expect(within(crumbs).getByText('var')).toBeTruthy();
    fireEvent.click(within(crumbs).getByRole('button', { name: 'Backup root' }));
    expect(await screen.findByRole('button', { name: 'var' })).toBeTruthy();
  });

  it('loads the next page with the cursor', async () => {
    (listFolder as jest.Mock)
      .mockResolvedValueOnce({ entries: [entry('a')], next: 'Y3Vy' })
      .mockResolvedValueOnce({ entries: [entry('b')] });
    render(<BackupBrowsePage />);
    fireEvent.click(await screen.findByRole('button', { name: 'Load more' }));
    expect(await screen.findByText('b')).toBeTruthy();
    expect(screen.getByText('a')).toBeTruthy();
    expect(listFolder).toHaveBeenLastCalledWith('bb-1', [], 'Y3Vy');
    expect(screen.queryByRole('button', { name: 'Load more' })).toBeNull();
  });

  it('says an empty folder is empty', async () => {
    render(<BackupBrowsePage />);
    expect(await screen.findByText(/This folder is empty/)).toBeTruthy();
  });

  it('shows a failed listing, and an ended session as ended', async () => {
    (listFolder as jest.Mock).mockRejectedValue(new GatewayError(410, 'session_ended', 'gone'));
    render(<BackupBrowsePage />);
    expect(await screen.findByText(/session has ended/i)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Open again' }));
    expect(reopen).toHaveBeenCalled();
  });
});

describe('BackupBrowsePage downloads', () => {
  it('links a file and a folder to the gateway download, with no token in the link', async () => {
    (listFolder as jest.Mock).mockResolvedValue({ entries: [entry('var', { type: 'dir' }), entry('index.php')] });
    render(<BackupBrowsePage />);
    await screen.findByText('index.php');
    const links = screen.getAllByRole('link', { name: /Download/ }).map((a) => a.getAttribute('href') || '');
    expect(links).toContain(
      `/api/proxy/plugin/ibm-vpc-file-pool-csi/backup-gateway/v1/sessions/bb-1/download?type=file&p=${encodeURIComponent(toB64(text('index.php')))}`,
    );
    expect(links.some((l) => l.includes('type=tar') && l.includes(encodeURIComponent(toB64(text('var')))))).toBe(true);
    for (const l of links) expect(l).not.toMatch(/token|bearer|authorization/i);
  });

  it('downloads the current folder as a tar from the file actions, and holds no restore button yet', async () => {
    (listFolder as jest.Mock).mockResolvedValue({ entries: [entry('var', { type: 'dir' })] });
    render(<BackupBrowsePage />);
    fireEvent.click(await screen.findByRole('button', { name: 'var' }));
    const actions = await screen.findByTestId('file-actions');
    const link = within(actions).getByRole('link', { name: /Download this folder/ });
    expect(link.getAttribute('href')).toContain('type=tar');
    expect(link.getAttribute('href')).toContain(encodeURIComponent(toB64(text('var'))));
    expect(screen.queryByText(/Restore/)).toBeNull();
  });

  it('downloads the whole backup from the root', async () => {
    render(<BackupBrowsePage />);
    const actions = await screen.findByTestId('file-actions');
    expect(within(actions).getByRole('link', { name: /Download this folder/ }).getAttribute('href')).toMatch(/type=tar$/);
  });
});

describe('BackupBrowsePage moments', () => {
  it('offers the latest copy and each moment, and moves to the chosen one', async () => {
    render(<BackupBrowsePage />);
    const select = (await screen.findByLabelText('Moment')) as HTMLSelectElement;
    await waitFor(() => expect(within(select).getAllByRole('option').length).toBe(3));
    const options = within(select).getAllByRole('option').map((o) => o.textContent);
    expect(options[0]).toMatch(/Latest copy/);
    expect(options[1]).toBe('2026-10-22 09:15:00 UTC');
    fireEvent.change(select, { target: { value: '2026-10-21T09:15:00Z' } });
    expect(mockReplace).toHaveBeenCalledWith('/vpc-file-pools/backups/nightly/browse?claim=apps%2Fdb&moment=2026-10-21T09%3A15%3A00Z');
  });

  it('warns that the latest copy may change while you look', async () => {
    render(<BackupBrowsePage />);
    expect(await screen.findByText(/latest copy may change/i)).toBeTruthy();
  });

  it('does not warn for an older moment', async () => {
    mockSearch = '?claim=apps%2Fdb&moment=2026-10-21T09%3A15%3A00Z';
    render(<BackupBrowsePage />);
    await screen.findByTestId('file-actions');
    await waitFor(() => expect(listMoments).toHaveBeenCalled());
    expect(screen.queryByText(/latest copy may change/i)).toBeNull();
  });
});

describe('BackupBrowsePage help', () => {
  it('links to the guide and explains the page', async () => {
    render(<BackupBrowsePage />);
    const help = await screen.findByTestId('browse-help-link');
    expect(help.getAttribute('href')).toContain('BACKUP-RECOVERY.md#browse-and-download-files-in-the-console');
    expect(help.getAttribute('rel')).toContain('noopener');
  });
});
