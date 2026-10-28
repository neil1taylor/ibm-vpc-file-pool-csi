import { consoleFetch } from '@openshift-console/dynamic-plugin-sdk';
import { PLUGIN_NAME, ROUTES } from '../constants';

/**
 * The file browser of the Backups pages talks to the backup gateway through the console's plugin proxy
 * (alias "backup-gateway"). The proxy adds the person's own token; the gateway checks it and the person's
 * right to read the backups of the volume's namespace on every call. The browser holds no other credential,
 * and no token or file content is ever put in a URL.
 */
export const GATEWAY_BASE = `/api/proxy/plugin/${PLUGIN_NAME}/backup-gateway/v1`;

/** The most entries one request for a folder asks for. */
export const PAGE_SIZE = 1000;

// ── Names ──

/** A name in a backup is raw bytes (a Linux file name is not always text). The gateway sends it as base64. */
export function fromB64(s: string): Uint8Array {
  const raw = atob(s);
  const out = new Uint8Array(raw.length);
  for (let i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
  return out;
}

export function toB64(bytes: Uint8Array): string {
  let s = '';
  for (let i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
  return btoa(s);
}

const PRINTABLE = /^[\p{L}\p{M}\p{N}\p{P}\p{S}]$/u;

/** Reads one UTF-8 character as Go's utf8.DecodeRune does: an invalid byte gives size 1 and no code point. */
function decodeRune(b: Uint8Array, i: number): { cp: number; size: number } {
  const b0 = b[i];
  if (b0 < 0x80) return { cp: b0, size: 1 };
  const cont = (k: number): number => (i + k < b.length && (b[i + k] & 0xc0) === 0x80 ? b[i + k] & 0x3f : -1);
  if (b0 >= 0xc2 && b0 <= 0xdf) {
    const c1 = cont(1);
    if (c1 >= 0) return { cp: ((b0 & 0x1f) << 6) | c1, size: 2 };
  } else if (b0 >= 0xe0 && b0 <= 0xef) {
    const c1 = cont(1);
    const c2 = cont(2);
    if (c1 >= 0 && c2 >= 0) {
      const cp = ((b0 & 0x0f) << 12) | (c1 << 6) | c2;
      if (cp >= 0x800 && !(cp >= 0xd800 && cp <= 0xdfff)) return { cp, size: 3 };
    }
  } else if (b0 >= 0xf0 && b0 <= 0xf4) {
    const c1 = cont(1);
    const c2 = cont(2);
    const c3 = cont(3);
    if (c1 >= 0 && c2 >= 0 && c3 >= 0) {
      const cp = ((b0 & 0x07) << 18) | (c1 << 12) | (c2 << 6) | c3;
      if (cp >= 0x10000 && cp <= 0x10ffff) return { cp, size: 4 };
    }
  }
  return { cp: -1, size: 1 };
}

const hex = (n: number): string => `\\x${n.toString(16).padStart(2, '0')}`;

/**
 * Shows a name as text, by the same rule as browse.Escape in the driver, so the console and kubectl show the
 * same text: printable UTF-8 stays, a backslash is doubled, and every other byte is written as \xNN. A name
 * is only ever shown as text (React escapes it); it is never put in HTML.
 */
export function escapeName(name: Uint8Array): string {
  let out = '';
  for (let i = 0; i < name.length; ) {
    const { cp, size } = decodeRune(name, i);
    if (cp === 0x5c) {
      out += '\\\\';
    } else if (cp < 0 || !(cp === 0x20 || PRINTABLE.test(String.fromCodePoint(cp)))) {
      for (let k = 0; k < size; k++) out += hex(name[i + k]);
    } else {
      out += String.fromCodePoint(cp);
    }
    i += size;
  }
  return out;
}

// ── Entries ──

export interface BrowseEntry {
  /** base64 of the raw name. Send it back unchanged in a path. */
  name: string;
  type: 'file' | 'dir' | 'symlink' | 'other' | string;
  size: number;
  mode: number;
  uid: number;
  gid: number;
  mtime: string;
  /** base64 of the text of a symbolic link. It is shown and never followed. */
  link?: string;
  readable: boolean;
}

export interface ListPage {
  entries: BrowseEntry[];
  /** base64 cursor of the next page. Empty on the last page. */
  next?: string;
}

export type EntryKind = 'folder' | 'file' | 'link' | 'other';

export function entryKind(e: BrowseEntry): EntryKind {
  switch (e.type) {
    case 'dir':
      return 'folder';
    case 'symlink':
      return 'link';
    case 'file':
      return 'file';
    default:
      return 'other';
  }
}

/** A link, a device, a pipe or a socket is never downloaded, and neither is an entry the helper may not read. */
export function canDownload(e: BrowseEntry): boolean {
  const kind = entryKind(e);
  return e.readable && (kind === 'file' || kind === 'folder');
}

/** 2048 -> "2.0 KiB". */
export function formatSize(bytes: number): string {
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = bytes;
  let u = 0;
  while (v >= 1024 && u < units.length - 1) {
    v /= 1024;
    u++;
  }
  return u === 0 ? `${v} B` : `${v.toFixed(1)} ${units[u]}`;
}

export const entryText = (e: BrowseEntry): string => escapeName(fromB64(e.name));
export const linkText = (e: BrowseEntry): string => (e.link ? escapeName(fromB64(e.link)) : '');

// ── Moments ──

/** A moment folder is named like 20261022T091500Z. Returns the time as RFC 3339, or undefined if it is not a moment. */
export function momentToISO(folder: string): string | undefined {
  const m = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$/.exec(folder);
  if (!m) return undefined;
  const iso = `${m[1]}-${m[2]}-${m[3]}T${m[4]}:${m[5]}:${m[6]}Z`;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime()) || d.toISOString().slice(0, 19) !== iso.slice(0, 19)) return undefined;
  return iso;
}

export function momentLabel(iso: string): string {
  return `${iso.slice(0, 10)} ${iso.slice(11, 19)} UTC`;
}

// ── Links ──

/** The file browser page of a claim ("namespace/name") in a policy, at a moment (RFC 3339) or the latest copy. */
export function browsePath(policy: string, claim: string, moment?: string): string {
  const query = new URLSearchParams();
  query.set('claim', claim);
  if (moment) query.set('moment', moment);
  return `${ROUTES.BACKUP_BROWSE.replace(':name', encodeURIComponent(policy))}?${query.toString()}`;
}

export function parseBrowseQuery(search: string): { namespace: string; pvc: string; moment: string } {
  const q = new URLSearchParams(search);
  const claim = q.get('claim') || '';
  const i = claim.indexOf('/');
  const none = { namespace: '', pvc: '', moment: '' };
  if (i <= 0 || i === claim.length - 1) return none;
  return { namespace: claim.slice(0, i), pvc: claim.slice(i + 1), moment: q.get('moment') || '' };
}

/**
 * The link that downloads one file ("file") or one folder as a tar ("tar"). The browser follows it with its
 * console session, and the plugin proxy adds the person's token, so the link holds no token. It holds the
 * names of the path, so the console and browser logs can show them.
 */
export function downloadUrl(session: string, type: 'file' | 'tar', path: string[]): string {
  const p = path.map((n) => `&p=${encodeURIComponent(n)}`).join('');
  return `${GATEWAY_BASE}/sessions/${encodeURIComponent(session)}/download?type=${type}${p}`;
}

// ── Calls ──

export class GatewayError extends Error {
  status: number;
  code: string;
  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = 'GatewayError';
    this.status = status;
    this.code = code;
  }
}

export interface SessionRequest {
  policy: string;
  namespace: string;
  pvc: string;
  /** RFC 3339. Left out for the latest copy. */
  moment?: string;
}

export interface Session {
  name: string;
  /** Pending, Starting, Ready, Closed or Failed. */
  phase: string;
  message?: string;
}

/** consoleFetch throws on a failed reply and keeps the reply on the error; the gateway's error body is read from it. */
async function toGatewayError(err: unknown): Promise<GatewayError> {
  const res = (err as { response?: Response } | undefined)?.response;
  if (!res || typeof res.status !== 'number') {
    return new GatewayError(0, '', err instanceof Error ? err.message : 'the request failed');
  }
  let code = '';
  let message = '';
  try {
    const body = (await res.json()) as { code?: string; message?: string };
    code = body.code || '';
    message = body.message || '';
  } catch {
    // A reply that is not JSON (a proxy error page) has no code.
  }
  return new GatewayError(res.status, code, message);
}

async function call<T>(method: string, path: string, body?: unknown, extra: RequestInit = {}): Promise<T> {
  try {
    const res = await consoleFetch(`${GATEWAY_BASE}${path}`, {
      method,
      ...(body === undefined ? {} : { body: JSON.stringify(body), headers: { 'Content-Type': 'application/json' } }),
      ...extra,
    });
    return (await res.json()) as T;
  } catch (err) {
    throw await toGatewayError(err);
  }
}

const sessionPath = (name: string): string => `/sessions/${encodeURIComponent(name)}`;

/** Asks for a session, or the user's open one for the same backup. It does not wait: the page polls. */
export const createSession = (req: SessionRequest): Promise<Session> => call<Session>('POST', '/sessions', req);

export const getSession = (name: string): Promise<Session> => call<Session>('GET', sessionPath(name));

/** Closes a session. With keepalive the request outlives the page, for use when the page is closed. */
export const closeSession = (name: string, keepalive = false): Promise<Session> =>
  call<Session>('DELETE', sessionPath(name), undefined, keepalive ? { keepalive: true } : {});

export const listFolder = (session: string, path: string[], cursor?: string): Promise<ListPage> =>
  call<ListPage>('POST', `${sessionPath(session)}/list`, { path, ...(cursor ? { cursor } : {}), limit: PAGE_SIZE });

export const listMoments = async (session: string): Promise<string[]> =>
  (await call<{ moments: string[] }>('POST', `${sessionPath(session)}/moments`, {})).moments || [];

/** Plain words for a failed call. */
export function gatewayErrorText(err: GatewayError, namespace?: string): string {
  switch (err.code) {
    case 'forbidden':
      return `You may not read the backups of namespace ${namespace || 'this claim'}. Ask for the right to get backupfiles (group storage.ibmcloud.io) in that namespace; the admin role of the namespace has it.`;
    case 'not_readable':
      return 'This file is not readable: the backup keeps it for another user, and the browse reader cannot open it.';
    case 'session_ended':
      return 'The session has ended. Open the backup again to keep browsing.';
    case 'helper_unreachable':
      return 'The reader of this backup does not answer. Close the session and open it again.';
    case 'unauthenticated':
      return 'Your sign-in is no longer valid. Sign in to the console again.';
  }
  if (err.status === 0) {
    return 'The console cannot reach the backup gateway. Check that the driver was installed with the gateway on, and try again.';
  }
  if (err.status === 404) {
    return err.code === 'not_found' && err.message === 'no such session'
      ? 'The session was not found. It may have ended; open the backup again.'
      : err.message || 'Not found.';
  }
  return err.message || `The gateway answered with status ${err.status}.`;
}
