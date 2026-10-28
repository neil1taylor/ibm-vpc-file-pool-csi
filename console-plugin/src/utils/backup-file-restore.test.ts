import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { GatewayError, toB64 } from './backup-browse';
import {
  MAX_PATHS,
  Selected,
  buildRestoreRequest,
  createRestore,
  defaultClaimName,
  getRestore,
  isEnded,
  phaseText,
  pathText,
  restoreErrorText,
  selectionKey,
  selectionSummary,
  suggestName,
  toggleSelected,
} from './backup-file-restore';
import { BrowseEntry } from './backup-browse';

const text = (s: string) => new Uint8Array(Buffer.from(s, 'utf8'));
const entry = (name: string | Uint8Array, over: Partial<BrowseEntry> = {}): BrowseEntry => ({
  name: toB64(typeof name === 'string' ? text(name) : name),
  type: 'file',
  size: 100,
  mode: 0o644,
  uid: 0,
  gid: 0,
  mtime: '2026-10-22T09:15:00Z',
  readable: true,
  ...over,
});
const sel = (path: string[], e: BrowseEntry): Selected => ({ path: [...path, e.name], entry: e });

describe('pathText', () => {
  it('writes the path in the escaped form kubectl backup ls prints', () => {
    expect(pathText([toB64(text('var')), toB64(text('www'))])).toBe('/var/www');
  });
  it('escapes a backslash and bytes that are not text', () => {
    expect(pathText([toB64(text('a\\b')), toB64(new Uint8Array([0x61, 0xff])), toB64(text('x\ny'))])).toBe('/a\\\\b/a\\xff/x\\x0ay');
  });
  it('writes the root as a slash', () => {
    expect(pathText([])).toBe('/');
  });
  it('escapes a slash inside a name so it stays one name', () => {
    // A Linux name never holds a slash, so this only guards a hostile reply.
    expect(pathText([toB64(text('a/b'))])).toBe('/a\\x2fb');
  });
});

describe('selection', () => {
  const a = entry('a.txt');
  it('keys a selection by the path, so the same name in two folders is two choices', () => {
    expect(selectionKey([toB64(text('x')), a.name])).not.toBe(selectionKey([toB64(text('y')), a.name]));
  });
  it('toggles an entry on and off without changing the old map', () => {
    const s1 = sel([], a);
    const on = toggleSelected(new Map(), s1);
    expect(on.size).toBe(1);
    const off = toggleSelected(on, s1);
    expect(off.size).toBe(0);
    expect(on.size).toBe(1);
  });
  it('counts files, folders and links, and adds up the file sizes', () => {
    const m = new Map<string, Selected>();
    [
      sel([], entry('a', { size: 1000 })),
      sel([], entry('b', { size: 24 })),
      sel([], entry('d', { type: 'dir', size: 4096 })),
      sel([], entry('l', { type: 'symlink', size: 4 })),
    ].forEach((s) => m.set(selectionKey(s.path), s));
    expect(selectionSummary(m)).toEqual({ files: 2, folders: 1, links: 1, count: 4, fileBytes: 1024 });
  });
  it('does not count the size of a link or a folder', () => {
    const m = new Map<string, Selected>();
    const s = sel([], entry('d', { type: 'dir', size: 4096 }));
    m.set(selectionKey(s.path), s);
    expect(selectionSummary(m).fileBytes).toBe(0);
  });
});

describe('defaults', () => {
  it('names the new claim after the source claim', () => {
    expect(defaultClaimName('db')).toBe('db-restored');
  });
  it('keeps the default name inside the claim name limit', () => {
    expect(defaultClaimName('x'.repeat(253)).length).toBeLessThanOrEqual(253);
  });
  it('suggests another name when one is taken', () => {
    expect(suggestName('db-restored')).toBe('db-restored-2');
    expect(suggestName('db-restored-2')).toBe('db-restored-3');
  });
  it('keeps a suggested name inside the limit', () => {
    expect(suggestName('x'.repeat(253)).length).toBeLessThanOrEqual(253);
  });
});

describe('a folder and the files inside it', () => {
  it('leaves out a path that a chosen folder already holds, so nothing is written twice', () => {
    const dir = sel([], entry('var', { type: 'dir' }));
    const inner = sel([dir.path[0]], entry('index.php'));
    const other = sel([], entry('varnish'));
    const m = new Map([dir, inner, other].map((x) => [selectionKey(x.path), x] as [string, Selected]));
    expect(buildRestoreRequest({
      policy: 'p', namespace: 'a', pvc: 'b', moment: '', selected: m, targetNamespace: 'a', targetName: 'c', existing: false, sizeGB: '', storageClassName: '',
    }).paths).toEqual(['/var', '/varnish']);
  });
  it('does not count the covered entries in the summary of what is written', () => {
    const dir = sel([], entry('var', { type: 'dir' }));
    const inner = sel([dir.path[0]], entry('index.php', { size: 500 }));
    const m = new Map([dir, inner].map((x) => [selectionKey(x.path), x] as [string, Selected]));
    expect(selectionSummary(m)).toMatchObject({ folders: 1, files: 0, count: 1, fileBytes: 0 });
  });
});

describe('buildRestoreRequest', () => {
  const s = [sel([], entry('a.txt')), sel([toB64(text('var'))], entry('index.php'))];
  const base = {
    policy: 'nightly',
    namespace: 'apps',
    pvc: 'db',
    moment: '2026-10-22T09:15:00Z',
    selected: new Map(s.map((x) => [selectionKey(x.path), x])),
    targetNamespace: 'apps',
    targetName: 'db-files',
    existing: false,
    sizeGB: '',
    storageClassName: '',
  };
  it('sends the escaped paths and the target of a new claim', () => {
    expect(buildRestoreRequest(base)).toEqual({
      policy: 'nightly',
      namespace: 'apps',
      pvc: 'db',
      moment: '2026-10-22T09:15:00Z',
      paths: ['/a.txt', '/var/index.php'],
      targetNamespace: 'apps',
      targetName: 'db-files',
    });
  });
  it('leaves out the moment for the latest copy', () => {
    expect(buildRestoreRequest({ ...base, moment: '' }).moment).toBeUndefined();
  });
  it('sends size and class only for a new claim', () => {
    const r = buildRestoreRequest({ ...base, sizeGB: '20', storageClassName: 'fast' });
    expect(r.sizeGB).toBe(20);
    expect(r.storageClassName).toBe('fast');
    const e = buildRestoreRequest({ ...base, existing: true, sizeGB: '20', storageClassName: 'fast' });
    expect(e.existing).toBe(true);
    expect(e.sizeGB).toBeUndefined();
    expect(e.storageClassName).toBeUndefined();
  });
  it('sorts the paths so the same choice is always the same request', () => {
    const r = buildRestoreRequest({ ...base, selected: new Map([...base.selected].reverse()) });
    expect(r.paths).toEqual(['/a.txt', '/var/index.php']);
  });
  it('allows at most 1000 paths', () => {
    expect(MAX_PATHS).toBe(1000);
  });
});

describe('calls', () => {
  beforeEach(() => jest.clearAllMocks());
  it('posts the request to the gateway and returns the restore', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue({ json: async () => ({ name: 'bfr-1', phase: 'Pending' }) });
    const r = await createRestore({ policy: 'p', namespace: 'a', pvc: 'b', paths: ['/x'], targetNamespace: 'a', targetName: 'c' });
    expect(r.name).toBe('bfr-1');
    const [url, init] = (consoleFetch as jest.Mock).mock.calls[0];
    expect(url).toMatch(/\/backup-gateway\/v1\/restores$/);
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body).paths).toEqual(['/x']);
  });
  it('reads a restore by its name', async () => {
    (consoleFetch as jest.Mock).mockResolvedValue({ json: async () => ({ name: 'bfr-1/x', phase: 'Done' }) });
    await getRestore('bfr-1/x');
    expect((consoleFetch as jest.Mock).mock.calls[0][0]).toMatch(/\/restores\/bfr-1%2Fx$/);
  });
  it('turns a failed reply into a GatewayError with the code', async () => {
    (consoleFetch as jest.Mock).mockRejectedValue({
      response: { status: 409, json: async () => ({ code: 'exists', message: 'that claim exists' }) },
    });
    await expect(createRestore({ policy: 'p', namespace: 'a', pvc: 'b', paths: ['/x'], targetNamespace: 'a', targetName: 'c' })).rejects.toMatchObject({
      status: 409,
      code: 'exists',
    });
  });
});

describe('phases', () => {
  it('has plain words for every phase', () => {
    expect(phaseText('Pending')).toMatch(/waiting/i);
    expect(phaseText('Measuring')).toMatch(/size/i);
    expect(phaseText('Provisioning')).toMatch(/claim/i);
    expect(phaseText('Restoring')).toMatch(/copying/i);
    expect(phaseText('Done')).toMatch(/done/i);
    expect(phaseText('Failed')).toMatch(/failed/i);
  });
  it('knows which phases end', () => {
    expect(isEnded('Done')).toBe(true);
    expect(isEnded('Failed')).toBe(true);
    expect(isEnded('Restoring')).toBe(false);
  });
});

describe('restoreErrorText', () => {
  const err = (status: number, code = '', message = '') => new GatewayError(status, code, message);
  it('says which right is missing for a 403 about the source', () => {
    expect(restoreErrorText(err(403, 'forbidden', 'forbidden'), { namespace: 'apps', targetNamespace: 'web', existing: false })).toMatch(
      /apps.*get backupfiles/s,
    );
  });
  it('says which right is missing for a 403 about claims', () => {
    const t = restoreErrorText(err(403, 'forbidden', 'you may not create or change claims in that namespace'), {
      namespace: 'apps',
      targetNamespace: 'web',
      existing: true,
    });
    expect(t).toMatch(/persistentvolumeclaims/);
    expect(t).toMatch(/web/);
  });
  it('says which right is missing for a 403 about pods', () => {
    expect(
      restoreErrorText(err(403, 'forbidden', 'you may not start pods in that namespace, and writing files into an existing claim needs that right'), {
        namespace: 'apps',
        targetNamespace: 'web',
        existing: true,
      }),
    ).toMatch(/create.*pods/s);
  });
  it('says the backup or claim was not found for a 404', () => {
    expect(restoreErrorText(err(404, 'not_found', 'no backup of that volume'), { namespace: 'a', targetNamespace: 'a', existing: false })).toMatch(
      /no backup of this volume/i,
    );
    expect(restoreErrorText(err(404, 'not_found', 'no such claim'), { namespace: 'a', targetNamespace: 'a', existing: true })).toMatch(/claim.*does not exist/i);
  });
  it('says the name is taken for a 409', () => {
    expect(restoreErrorText(err(409, 'exists'), { namespace: 'a', targetNamespace: 'a', existing: false })).toMatch(/already.*name/is);
  });
  it('says there are too many open restores for a 429', () => {
    expect(restoreErrorText(err(429, 'too_many', 'you have 10 restores that have not ended; the limit is 10'), { namespace: 'a', targetNamespace: 'a', existing: false })).toMatch(
      /too many|10/i,
    );
  });
  it('says the console cannot reach the gateway for status 0', () => {
    expect(restoreErrorText(err(0), { namespace: 'a', targetNamespace: 'a', existing: false })).toMatch(/cannot reach/i);
  });
  it('shows the gateway message for a 400', () => {
    expect(restoreErrorText(err(400, 'bad_request', 'sizeGB must be between 0 and 32000'), { namespace: 'a', targetNamespace: 'a', existing: false })).toMatch(
      /32000/,
    );
  });
});
