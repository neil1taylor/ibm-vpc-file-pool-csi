import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import {
  GatewayError,
  browsePath,
  canDownload,
  closeSession,
  createSession,
  downloadUrl,
  entryKind,
  formatSize,
  escapeName,
  fromB64,
  gatewayErrorText,
  getSession,
  listFolder,
  listMoments,
  momentLabel,
  momentToISO,
  parseBrowseQuery,
  toB64,
  BrowseEntry,
} from './backup-browse';

const bytes = (...n: number[]) => new Uint8Array(n);
const text = (s: string) => new Uint8Array(Buffer.from(s, 'utf8'));

const reply = (status: number, body: unknown) =>
  ({ ok: status < 400, status, json: () => Promise.resolve(body) } as unknown as Response);

describe('base64 of names', () => {
  it('keeps bytes that are not text exactly', () => {
    const raw = bytes(0xff, 0x00, 0x2f, 0x41);
    expect(Array.from(fromB64(toB64(raw)))).toEqual(Array.from(raw));
  });
});

describe('escapeName', () => {
  // The same rule as browse.Escape in Go, so the console and kubectl show the same text.
  it.each([
    ['plain text stays', text('index.php'), 'index.php'],
    ['a space stays', text('my file'), 'my file'],
    ['an accented letter stays', text('café'), 'café'],
    ['an emoji stays', text('\u{1F600}'), '\u{1F600}'],
    ['a backslash is doubled', text('a\\b'), 'a\\\\b'],
    ['a newline is written as a byte', text('a\nb'), 'a\\x0ab'],
    ['byte 0xff is written as a byte', bytes(0x61, 0xff), 'a\\xff'],
    ['a right-to-left override is written as bytes', text('a‮b'), 'a\\xe2\\x80\\xaeb'],
    ['a no-break space is written as bytes', text('a b'), 'a\\xc2\\xa0b'],
    ['a NUL is written as a byte', bytes(0x61, 0x00), 'a\\x00'],
    ['a cut multi-byte letter is written as bytes', bytes(0xe2, 0x82), '\\xe2\\x82'],
    ['an overlong form is written as bytes', bytes(0xc0, 0xaf), '\\xc0\\xaf'],
  ])('%s', (_n, input, want) => {
    expect(escapeName(input)).toBe(want);
  });
});

describe('moments', () => {
  it('turns a moment folder name into a time', () => {
    expect(momentToISO('20261022T091500Z')).toBe('2026-10-22T09:15:00Z');
  });
  it('refuses a name that is not a moment', () => {
    expect(momentToISO('lost+found')).toBeUndefined();
    expect(momentToISO('20261322T091500Z')).toBeUndefined();
  });
  it('labels a moment in UTC', () => {
    expect(momentLabel('2026-10-22T09:15:00Z')).toBe('2026-10-22 09:15:00 UTC');
  });
});

describe('browse links', () => {
  it('names the page of a policy, claim and moment', () => {
    expect(browsePath('nightly', 'apps/db')).toBe('/vpc-file-pools/backups/nightly/browse?claim=apps%2Fdb');
    expect(browsePath('nightly', 'apps/db', '2026-10-22T09:15:00Z')).toBe(
      '/vpc-file-pools/backups/nightly/browse?claim=apps%2Fdb&moment=2026-10-22T09%3A15%3A00Z',
    );
  });
  it('reads the query back', () => {
    expect(parseBrowseQuery('?claim=apps%2Fdb&moment=2026-10-22T09%3A15%3A00Z')).toEqual({
      namespace: 'apps',
      pvc: 'db',
      moment: '2026-10-22T09:15:00Z',
    });
    expect(parseBrowseQuery('')).toEqual({ namespace: '', pvc: '', moment: '' });
    expect(parseBrowseQuery('?claim=nonsense')).toEqual({ namespace: '', pvc: '', moment: '' });
  });
});

describe('download links', () => {
  it('has one p value for each name, in the names as the gateway sent them', () => {
    const url = downloadUrl('bb-1', 'file', ['dmFy', 'd3d3']);
    expect(url).toBe('/api/proxy/plugin/ibm-vpc-file-pool-csi/backup-gateway/v1/sessions/bb-1/download?type=file&p=dmFy&p=d3d3');
  });
  it('encodes base64 characters that are special in a URL', () => {
    expect(downloadUrl('bb-1', 'tar', ['a+/=='])).toContain('&p=a%2B%2F%3D%3D');
  });
  it('holds no token', () => {
    expect(downloadUrl('bb-1', 'tar', [])).not.toMatch(/token|bearer|authorization/i);
  });
});

const entry = (over: Partial<BrowseEntry>): BrowseEntry => ({
  name: toB64(text('x')),
  type: 'file',
  size: 1,
  mode: 0o644,
  uid: 0,
  gid: 0,
  mtime: '2026-10-22T09:15:00Z',
  readable: true,
  ...over,
});

describe('entries', () => {
  it('says what an entry is', () => {
    expect(entryKind(entry({ type: 'dir' }))).toBe('folder');
    expect(entryKind(entry({ type: 'symlink' }))).toBe('link');
    expect(entryKind(entry({ type: 'other' }))).toBe('other');
    expect(entryKind(entry({}))).toBe('file');
  });
  it('allows a download of a readable file or folder only', () => {
    expect(canDownload(entry({}))).toBe(true);
    expect(canDownload(entry({ type: 'dir' }))).toBe(true);
    expect(canDownload(entry({ type: 'symlink' }))).toBe(false);
    expect(canDownload(entry({ type: 'other' }))).toBe(false);
    expect(canDownload(entry({ readable: false }))).toBe(false);
  });
});

describe('gateway calls', () => {
  beforeEach(() => (consoleFetch as jest.Mock).mockReset());

  it('opens a session without waiting, with a JSON body', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply(202, { name: 'bb-1', phase: 'Pending' }));
    const s = await createSession({ policy: 'nightly', namespace: 'apps', pvc: 'db', moment: '2026-10-22T09:15:00Z' });
    expect(s.name).toBe('bb-1');
    const [url, opts] = (consoleFetch as jest.Mock).mock.calls[0];
    expect(url).toBe('/api/proxy/plugin/ibm-vpc-file-pool-csi/backup-gateway/v1/sessions');
    expect(url).not.toContain('wait');
    expect(opts.method).toBe('POST');
    expect(JSON.parse(opts.body)).toEqual({ policy: 'nightly', namespace: 'apps', pvc: 'db', moment: '2026-10-22T09:15:00Z' });
  });

  it('leaves the moment out for the latest copy', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply(202, { name: 'bb-1', phase: 'Pending' }));
    await createSession({ policy: 'nightly', namespace: 'apps', pvc: 'db' });
    expect(JSON.parse((consoleFetch as jest.Mock).mock.calls[0][1].body)).toEqual({ policy: 'nightly', namespace: 'apps', pvc: 'db' });
  });

  it('reads the status of a session', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply(200, { name: 'bb-1', phase: 'Ready' }));
    await expect(getSession('bb-1')).resolves.toEqual({ name: 'bb-1', phase: 'Ready' });
  });

  it('turns an error body into a GatewayError', async () => {
    (consoleFetch as jest.Mock).mockRejectedValue({ response: reply(410, { code: 'session_ended', message: 'the session has ended' }) });
    await expect(listFolder('bb-1', [])).rejects.toMatchObject({ status: 410, code: 'session_ended' });
  });

  it('gives a GatewayError for a failure that has no body', async () => {
    (consoleFetch as jest.Mock).mockRejectedValue(new Error('network down'));
    const err = await getSession('bb-1').catch((e) => e);
    expect(err).toBeInstanceOf(GatewayError);
    expect(err.status).toBe(0);
  });

  it('lists a folder, with the cursor and page size', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply(200, { entries: [], next: 'Y3Vy' }));
    const page = await listFolder('bb-1', ['dmFy'], 'Y3Vy0');
    expect(page.next).toBe('Y3Vy');
    const [url, opts] = (consoleFetch as jest.Mock).mock.calls[0];
    expect(url).toMatch(/sessions\/bb-1\/list$/);
    expect(JSON.parse(opts.body)).toEqual({ path: ['dmFy'], cursor: 'Y3Vy0', limit: 1000 });
  });

  it('omits the cursor on the first page', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply(200, { entries: [] }));
    await listFolder('bb-1', []);
    expect(JSON.parse((consoleFetch as jest.Mock).mock.calls[0][1].body)).toEqual({ path: [], limit: 1000 });
  });

  it('lists the moments', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply(200, { moments: ['20261022T091500Z'] }));
    await expect(listMoments('bb-1')).resolves.toEqual(['20261022T091500Z']);
  });

  it('closes a session, and can keep the request alive when the page goes away', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply(200, { name: 'bb-1', phase: 'Closed' }));
    await closeSession('bb-1', true);
    const [url, opts] = (consoleFetch as jest.Mock).mock.calls[0];
    expect(url).toMatch(/sessions\/bb-1$/);
    expect(opts).toMatchObject({ method: 'DELETE', keepalive: true });
  });

  it('never puts a token or a credential on a request', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue(reply(200, { entries: [] }));
    await listFolder('bb-1', []);
    const [url, opts] = (consoleFetch as jest.Mock).mock.calls[0];
    expect(JSON.stringify([url, opts])).not.toMatch(/authorization|bearer|token/i);
  });
});

describe('gatewayErrorText', () => {
  const e = (status: number, code = '', message = '') => new GatewayError(status, code, message);
  it('explains a missing right', () => {
    const text = gatewayErrorText(e(403, 'forbidden'), 'apps');
    expect(text).toContain('apps');
    expect(text).toContain('get backupfiles');
  });
  it('explains a session that ended', () => {
    expect(gatewayErrorText(e(410, 'session_ended'))).toMatch(/ended/);
  });
  it('explains a missing session', () => {
    expect(gatewayErrorText(e(404, 'not_found', 'no such session'))).toMatch(/no longer|not found|ended/i);
  });
  it('explains a gateway that cannot be reached', () => {
    expect(gatewayErrorText(e(0))).toMatch(/cannot reach/i);
    expect(gatewayErrorText(e(502, 'helper_unreachable'))).toMatch(/reader/i);
  });
  it('says a not-readable file cannot be read', () => {
    expect(gatewayErrorText(e(403, 'not_readable'), 'apps')).toMatch(/not readable/i);
  });
  it('passes the gateway message for anything else', () => {
    expect(gatewayErrorText(e(500, 'internal', 'cannot list backups'))).toContain('cannot list backups');
  });
});

describe('formatSize', () => {
  it('writes sizes in binary units', () => {
    expect(formatSize(0)).toBe('0 B');
    expect(formatSize(1023)).toBe('1023 B');
    expect(formatSize(2048)).toBe('2.0 KiB');
    expect(formatSize(5 * 1024 ** 3)).toBe('5.0 GiB');
  });
});
