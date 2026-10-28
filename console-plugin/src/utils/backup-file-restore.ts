import { BrowseEntry, GatewayError, call, entryKind, escapeName, fromB64 } from './backup-browse';
import { pvcPath } from './k8s-objects';

/**
 * Restoring chosen files from a backup into a claim, through the backup gateway. The names of the chosen paths
 * stay base64 (as the gateway sent them) until the request is built, and they are written in the escaped form
 * that `kubectl backup ls` prints. A name is only ever shown as text.
 */

/** The most paths one restore may hold. The gateway refuses more. */
export const MAX_PATHS = 1000;

// ── Selection ──

export interface Selected {
  /** base64 names from the backup root down to the chosen entry. */
  path: string[];
  entry: BrowseEntry;
}

export type SelectionMap = Map<string, Selected>;

/** base64 has no "/", so joining with it cannot make two paths look alike. */
export const selectionKey = (path: string[]): string => path.join('/');

/** An entry may be restored when the reader can open it and it is a file, a folder or a link. */
export function canSelect(e: BrowseEntry): boolean {
  return e.readable && entryKind(e) !== 'other';
}

export function toggleSelected(old: SelectionMap, s: Selected): SelectionMap {
  const out = new Map(old);
  const key = selectionKey(s.path);
  if (out.has(key)) out.delete(key);
  else out.set(key, s);
  return out;
}

export interface SelectionSummary {
  files: number;
  folders: number;
  links: number;
  count: number;
  /** The size of the chosen files. A folder's content is not added: it is measured when the restore runs. */
  fileBytes: number;
}

/**
 * What is really written: an entry inside a chosen folder is already in the folder, and writing it twice
 * would fail the restore (a restore never writes over a file).
 */
export function uncovered(selected: SelectionMap): Selected[] {
  const folders = new Set<string>();
  selected.forEach((s) => {
    if (entryKind(s.entry) === 'folder') folders.add(selectionKey(s.path));
  });
  return [...selected.values()].filter((s) => {
    for (let n = 1; n < s.path.length; n++) {
      if (folders.has(selectionKey(s.path.slice(0, n)))) return false;
    }
    return true;
  });
}

export function selectionSummary(selected: SelectionMap): SelectionSummary {
  const out: SelectionSummary = { files: 0, folders: 0, links: 0, count: 0, fileBytes: 0 };
  uncovered(selected).forEach(({ entry }) => {
    out.count++;
    switch (entryKind(entry)) {
      case 'file':
        out.files++;
        out.fileBytes += entry.size;
        break;
      case 'folder':
        out.folders++;
        break;
      case 'link':
        out.links++;
        break;
    }
  });
  return out;
}

/** The path in the escaped form of `kubectl backup ls`: "/var/www". A "/" in a name is escaped so it stays one name. */
export function pathText(path: string[]): string {
  if (path.length === 0) return '/';
  return path.map((seg) => `/${escapeName(fromB64(seg)).replace(/\//g, '\\x2f')}`).join('');
}

// ── Names ──

const NAME_LIMIT = 253;

export function defaultClaimName(pvc: string): string {
  return `${pvc}-restored`.slice(0, NAME_LIMIT).replace(/[-.]+$/, '');
}

/** "db-restored" -> "db-restored-2" -> "db-restored-3". */
export function suggestName(name: string): string {
  const m = /^(.*)-(\d+)$/.exec(name);
  const base = m ? m[1] : name;
  const n = m ? Number(m[2]) + 1 : 2;
  const suffix = `-${n}`;
  return `${base.slice(0, NAME_LIMIT - suffix.length).replace(/[-.]+$/, '')}${suffix}`;
}

// ── Request ──

export interface RestoreRequest {
  policy: string;
  namespace: string;
  pvc: string;
  moment?: string;
  paths: string[];
  targetNamespace: string;
  targetName: string;
  existing?: boolean;
  sizeGB?: number;
  storageClassName?: string;
}

export interface RestoreInput {
  policy: string;
  namespace: string;
  pvc: string;
  /** RFC 3339, or empty for the latest copy. */
  moment: string;
  selected: SelectionMap;
  targetNamespace: string;
  targetName: string;
  existing: boolean;
  /** Whole GiB as typed, or empty to let the driver measure. */
  sizeGB: string;
  storageClassName: string;
}

export function buildRestoreRequest(i: RestoreInput): RestoreRequest {
  const paths = uncovered(i.selected).map((s) => pathText(s.path)).sort();
  const req: RestoreRequest = {
    policy: i.policy,
    namespace: i.namespace,
    pvc: i.pvc,
    ...(i.moment ? { moment: i.moment } : {}),
    paths,
    targetNamespace: i.targetNamespace,
    targetName: i.targetName,
  };
  if (i.existing) {
    req.existing = true;
  } else {
    const gb = parseInt(i.sizeGB, 10);
    if (gb > 0) req.sizeGB = gb;
    if (i.storageClassName) req.storageClassName = i.storageClassName;
  }
  return req;
}

/** What is wrong with the typed values, in plain words, or an empty string. Names follow the rules of a claim name. */
export function validateRestoreForm(i: { targetNamespace: string; targetName: string; sizeGB: string; existing: boolean }): Record<string, string> {
  const errors: Record<string, string> = {};
  if (!/^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$/.test(i.targetNamespace)) {
    errors.targetNamespace = 'Use the name of a namespace: lower case letters, digits and "-".';
  }
  if (!/^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/.test(i.targetName) || i.targetName.length > NAME_LIMIT) {
    errors.targetName = 'Use a claim name: lower case letters, digits, "-" and ".", starting and ending with a letter or digit.';
  }
  if (!i.existing && i.sizeGB !== '') {
    const gb = Number(i.sizeGB);
    if (!Number.isInteger(gb) || gb < 1 || gb > 32000) errors.sizeGB = 'Use a whole number of GiB from 1 to 32000, or leave it empty.';
  }
  return errors;
}

// ── Restore ──

export interface Restore {
  name: string;
  phase: string;
  message?: string;
  targetNamespace: string;
  targetName: string;
  existing?: boolean;
  folder?: string;
  sizeGB?: number;
  restoredMoment?: string;
  latest?: boolean;
  files: number;
  dirs: number;
  links: number;
  skipped: number;
  unreadable: number;
  bytes: number;
}

/** Asks for a restore. The answer comes at once (status 202); the page polls for the end. */
export const createRestore = (req: RestoreRequest): Promise<Restore> => call<Restore>('POST', '/restores', req);

export const getRestore = (name: string): Promise<Restore> => call<Restore>('GET', `/restores/${encodeURIComponent(name)}`);

export const POLL_MS = 2000;

const PHASES: Record<string, string> = {
  Pending: 'Waiting to start',
  Measuring: 'Measuring the size of the chosen files',
  Provisioning: 'Making the claim',
  Restoring: 'Copying the files',
  Done: 'Done',
  Failed: 'Failed',
};
export const PHASE_ORDER = ['Pending', 'Measuring', 'Provisioning', 'Restoring', 'Done'];

export const phaseText = (phase: string): string => PHASES[phase] || phase;

export const isEnded = (phase: string): boolean => phase === 'Done' || phase === 'Failed';

export const claimLink = (namespace: string, name: string): string => pvcPath(namespace, name);

export interface RestoreTarget {
  namespace: string;
  targetNamespace: string;
  existing: boolean;
}

/** Plain words for a refused request. */
export function restoreErrorText(err: GatewayError, t: RestoreTarget): string {
  switch (err.status) {
    case 0:
      return 'The console cannot reach the backup gateway. Check that the driver was installed with the gateway on, and try again.';
    case 401:
      return 'Your sign-in is no longer valid. Sign in to the console again.';
    case 403:
      if (/pods/.test(err.message)) {
        return `You may not start pods in namespace ${t.targetNamespace}, and writing files into an existing claim needs that right (create on pods). Choose a new claim, or ask for the right.`;
      }
      if (/claims/.test(err.message)) {
        return `You may not create${t.existing ? ' or change' : ''} persistentvolumeclaims in namespace ${t.targetNamespace}. Ask for the right to create${t.existing ? ' and update' : ''} persistentvolumeclaims there.`;
      }
      return `You may not read the backups of namespace ${t.namespace}. Ask for the right to get backupfiles (group storage.ibmcloud.io) in that namespace; the admin role of the namespace has it.`;
    case 404:
      if (/claim/.test(err.message)) {
        return `The claim you chose does not exist in namespace ${t.targetNamespace}, or you cannot see it. Choose a claim that exists, or restore into a new claim.`;
      }
      if (/restore/.test(err.message)) return 'This restore was not found. It may have been removed 24 hours after it ended.';
      return 'There is no backup of this volume any more. It may have been removed after its keep time.';
    case 409:
      return 'A claim with this name already exists in that namespace, and a restore into a new claim never writes over one. Choose another name.';
    case 429:
      return `You have too many open restores. ${err.message ? `The gateway says: ${err.message}. ` : ''}Wait for one to end and try again.`;
  }
  return err.message || `The gateway answered with status ${err.status}.`;
}
