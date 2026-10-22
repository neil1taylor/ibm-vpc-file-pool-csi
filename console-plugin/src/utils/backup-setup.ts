import { DRIVER_NAMESPACE } from '../constants';
import { BackupExport, BackupPolicy } from '../types';
import { keepForMs, parseGoDuration } from './backup-policy-form';
import { LABEL_MADE_BY_CONSOLE } from './receiver-objects';
import { exportStateText } from './backup-helpers';

// ── The command ──

export interface CosSetupInput {
  region: string;
  resourceGroup: string;
  bucket: string;
  rootKey: string;
  vpcCRN: string;
  adminAddresses: string;
  prefix: string;
  keepFor: string;
  lock: string;
  dryRun?: boolean;
}

const shellWord = (v: string) => (/^[A-Za-z0-9._,/=@%+-]+$/.test(v) ? v : `'${v.replace(/'/g, `'\\''`)}'`);

/**
 * The `kubectl backup cos-setup` command with the person's names filled in, one flag to a line. The console never
 * runs it: it needs the person's own IBM Cloud login.
 */
export function buildCosSetupCommand(input: CosSetupInput): string {
  const flags: Array<[string, string]> = [
    ['--region', input.region],
    ['--resource-group', input.resourceGroup],
    ['--bucket', input.bucket],
    ['--root-key', input.rootKey],
    ['--vpc-crn', input.vpcCRN],
    ['--admin-address', input.adminAddresses.split(/[,\s]+/).filter(Boolean).join(',')],
    ['--prefix', input.prefix],
  ];
  if (input.keepFor && input.keepFor !== '30d') flags.push(['--keep-for', input.keepFor]);
  if (input.lock && input.lock !== 'Compliance') flags.push(['--lock', input.lock]);
  flags.push(['--key-file', 'cos-key.yaml'], ['--secret-file', 'cos-wrapped-key.yaml']);
  const lines = flags.filter(([, v]) => v !== '').map(([k, v]) => `${k} ${shellWord(v)}`);
  if (input.dryRun) lines.push('--dry-run');
  return ['kubectl backup cos-setup', ...lines].map((l, i) => (i === 0 ? l : `  ${l}`)).map((l, i, all) => (i < all.length - 1 ? `${l} \\` : l)).join('\n');
}

/** What each flag does, in plain words. */
export const COS_SETUP_FLAGS: Array<{ flag: string; text: string }> = [
  { flag: '--region, --resource-group, --bucket', text: 'Where the command makes the COS bucket (and the COS instance, if there is none).' },
  { flag: '--root-key', text: 'The CRN of an existing Key Protect root key. It wraps the backup key. The command does not make Key Protect instances or root keys.' },
  { flag: '--vpc-crn, --admin-address', text: 'The cluster VPC and your own public address (and other admin addresses), for a context-based restriction that lets only them reach the bucket.' },
  { flag: '--prefix', text: 'A folder in the bucket. The area of each policy is below it.' },
  { flag: '--keep-for, --lock', text: 'How long COS keeps exported backups, and whether the lock (Compliance) stops anyone from deleting or shortening it. A Compliance lock cannot be undone, even by IBM.' },
  { flag: '--key-file, --secret-file', text: 'Files the command writes with the COS key and the wrapped backup key as Secret manifests (mode 0600). You paste their contents below.' },
  { flag: '--dry-run', text: 'Shows each step and the exact objects, and changes nothing. Run it first.' },
];

// ── Pasted Secret manifests ──

export interface ParsedSecret {
  name: string;
  namespace: string;
  data: Record<string, string>;
}

const KEY_SETS: Array<{ keys: string[]; what: string }> = [
  { keys: ['access_key_id', 'secret_access_key'], what: 'the COS key' },
  { keys: ['wrappedKey'], what: 'the wrapped backup key' },
  { keys: ['apikey'], what: 'the Key Protect API key' },
];

function unquote(value: string): string {
  const v = value.trim();
  if (v.startsWith('"')) {
    try {
      return JSON.parse(v) as string;
    } catch {
      return v.slice(1, -1);
    }
  }
  if (v.startsWith("'") && v.endsWith("'")) return v.slice(1, -1).replace(/''/g, "'");
  return v;
}

/**
 * Reads the Secret manifests that `cos-setup` writes (stringData, in the driver namespace). It accepts only the
 * keys the export uses, and refuses the readable backup key: that key must never be put in the cluster here.
 */
export function parseSecretManifests(text: string): { secrets: ParsedSecret[]; errors: string[] } {
  const secrets: ParsedSecret[] = [];
  const errors: string[] = [];
  for (const doc of text.split(/^---\s*$/m)) {
    if (!doc.trim()) continue;
    let kind = '';
    let name = '';
    let namespace = '';
    const data: Record<string, string> = {};
    let section = '';
    for (const raw of doc.split('\n')) {
      if (!raw.trim() || raw.trim().startsWith('#')) continue;
      const indent = raw.length - raw.trimStart().length;
      const m = raw.trim().match(/^([A-Za-z_][\w.-]*):\s*(.*)$/);
      if (!m) continue;
      const [, key, value] = m;
      if (indent === 0) {
        section = key;
        if (key === 'kind') kind = unquote(value);
      } else if (section === 'metadata' && key === 'name') name = unquote(value);
      else if (section === 'metadata' && key === 'namespace') namespace = unquote(value);
      else if (section === 'stringData') data[key] = unquote(value);
      else if (section === 'data') data[`data:${key}`] = value;
    }
    if (kind !== 'Secret') {
      errors.push('One of the manifests is not a Secret.');
      continue;
    }
    if (!name) {
      errors.push('A Secret in the manifests has no name.');
      continue;
    }
    if (namespace !== DRIVER_NAMESPACE) {
      errors.push(`Secret ${name} is for the namespace "${namespace}". The console makes Secrets in ${DRIVER_NAMESPACE} only.`);
      continue;
    }
    const keys = Object.keys(data).sort();
    if (keys.includes('backupKey')) {
      errors.push(`Secret ${name} holds the readable backup key. Do not put it in the cluster here: keep it outside IBM Cloud.`);
      continue;
    }
    const set = KEY_SETS.find((s) => keys.some((k) => s.keys.includes(k)));
    if (!set || keys.some((k) => !set.keys.includes(k))) {
      errors.push(`Secret ${name} has keys it does not know. The export uses access_key_id and secret_access_key, wrappedKey, and apikey, written as stringData.`);
      continue;
    }
    if (set.keys.some((k) => !data[k])) {
      errors.push(`Secret ${name} needs both ${set.keys.join(' and ')} (${set.what}).`);
      continue;
    }
    if (secrets.some((s) => s.name === name)) {
      errors.push(`Secret ${name} is in the manifests twice.`);
      continue;
    }
    secrets.push({ name, namespace, data });
  }
  return { secrets, errors };
}

export interface SecretObject {
  apiVersion: string;
  kind: string;
  type: string;
  metadata: { name: string; namespace: string; labels: Record<string, string> };
  stringData: Record<string, string>;
}

export function buildSecret(s: ParsedSecret): SecretObject {
  return {
    apiVersion: 'v1',
    kind: 'Secret',
    type: 'Opaque',
    metadata: { name: s.name, namespace: s.namespace, labels: { [LABEL_MADE_BY_CONSOLE]: 'true' } },
    stringData: s.data,
  };
}

/** The Secret with the IBM Cloud API key that may use the Key Protect root key (cos-setup does not make it). */
export function apiKeySecret(name: string, apiKey: string): SecretObject {
  return buildSecret({ name, namespace: DRIVER_NAMESPACE, data: { apikey: apiKey.trim() } });
}

// ── The pasted spec.export block ──

const EXPORT_KEYS = [
  'bucket',
  'endpoint',
  'prefix',
  'credentialsSecret',
  'backupKeySecret',
  'keyProtect',
  'emergencyKeySecret',
  'keepFor',
  'lock',
  'schedule',
];
const KP_KEYS = ['instanceID', 'region', 'rootKeyID', 'apiKeySecret'];

type Tree = { [key: string]: string | Tree };

/** Reads the `spec.export` block that cos-setup prints: nested keys, two spaces apart. */
export function parseExportBlock(text: string): { export?: BackupExport; errors: string[] } {
  const lines = text.split('\n');
  if (!text.trim()) return { errors: ['Paste the spec.export block that cos-setup printed.'] };
  const root: Tree = {};
  const stack: Array<{ indent: number; node: Tree }> = [{ indent: -1, node: root }];
  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i];
    if (!raw.trim() || raw.trim().startsWith('#')) continue;
    const indent = raw.length - raw.trimStart().length;
    const m = raw.trim().match(/^([A-Za-z_][\w.-]*):\s*(.*)$/);
    if (!m) return { errors: [`Cannot read line ${i + 1}: "${raw.trim()}".`] };
    while (stack.length > 1 && indent <= stack[stack.length - 1].indent) stack.pop();
    const parent = stack[stack.length - 1].node;
    const [, key, value] = m;
    if (value === '') {
      const child: Tree = {};
      parent[key] = child;
      stack.push({ indent, node: child });
    } else {
      parent[key] = unquote(value);
    }
  }
  let node: Tree = root;
  if (typeof node.spec === 'object') node = node.spec;
  if (typeof node.export === 'object') node = node.export;
  const unknown = Object.keys(node).filter((k) => !EXPORT_KEYS.includes(k));
  if (unknown.length > 0) return { errors: [`spec.export has no key "${unknown[0]}".`] };
  const kp = typeof node.keyProtect === 'object' ? node.keyProtect : {};
  const unknownKp = Object.keys(kp).filter((k) => !KP_KEYS.includes(k));
  if (unknownKp.length > 0) return { errors: [`spec.export.keyProtect has no key "${unknownKp[0]}".`] };
  const str = (k: string) => (typeof node[k] === 'string' ? (node[k] as string) : undefined);
  const out: Record<string, unknown> = {};
  for (const k of EXPORT_KEYS) {
    if (k === 'keyProtect') continue;
    if (str(k) !== undefined) out[k] = str(k);
  }
  out.keyProtect = {
    instanceID: (kp.instanceID as string) || '',
    region: (kp.region as string) || '',
    rootKeyID: (kp.rootKeyID as string) || '',
    apiKeySecret: (kp.apiKeySecret as string) || '',
  };
  return { export: out as unknown as BackupExport, errors: [] };
}

const SECRET_NAME = /^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$/;
const REGION = /^[a-z0-9]+(-[a-z0-9]+)*$/;
const KEY_ID = /^[A-Za-z0-9][A-Za-z0-9-]*$/;
const HOUR = 3600 * 1000;

/** The checks of the admission webhook (api/v1alpha1/backuppolicy_export.go Validate), shown before the policy is changed. */
export function validateExport(ex: BackupExport): string[] {
  const errors: string[] = [];
  for (const k of ['bucket', 'endpoint', 'credentialsSecret', 'backupKeySecret'] as const) {
    if (!ex[k]) errors.push(`spec.export.${k} is required.`);
  }
  for (const k of ['instanceID', 'region', 'rootKeyID', 'apiKeySecret'] as const) {
    if (!ex.keyProtect?.[k]) errors.push(`spec.export.keyProtect.${k} is required.`);
  }
  if (ex.keyProtect?.region && !REGION.test(ex.keyProtect.region)) errors.push('spec.export.keyProtect.region is not a region name such as eu-de.');
  if (ex.keyProtect?.instanceID && !KEY_ID.test(ex.keyProtect.instanceID)) errors.push('spec.export.keyProtect.instanceID is not valid.');
  if (ex.keyProtect?.rootKeyID && !KEY_ID.test(ex.keyProtect.rootKeyID)) errors.push('spec.export.keyProtect.rootKeyID is not valid.');
  for (const [k, v] of [
    ['credentialsSecret', ex.credentialsSecret],
    ['backupKeySecret', ex.backupKeySecret],
    ['keyProtect.apiKeySecret', ex.keyProtect?.apiKeySecret],
    ['emergencyKeySecret', ex.emergencyKeySecret],
  ] as const) {
    if (v && !SECRET_NAME.test(v)) errors.push(`spec.export.${k} "${v}" is not a valid Secret name.`);
  }
  const lock = ex.lock || 'Compliance';
  if (lock !== 'Compliance' && lock !== 'None') errors.push(`spec.export.lock must be Compliance or None, got "${ex.lock}".`);
  const keep = keepForMs(ex.keepFor || '30d');
  if (keep === undefined) {
    errors.push(`spec.export.keepFor "${ex.keepFor}" is not a number of days such as 30d or a duration such as 36h.`);
  } else if (keep < HOUR) {
    errors.push('spec.export.keepFor must be at least 1h.');
  } else if (lock === 'Compliance') {
    const interval = Math.min(Math.max(keep - 24 * HOUR, HOUR), 24 * HOUR);
    const min = interval + 24 * HOUR;
    if (keep < min) errors.push(`spec.export.keepFor must be at least ${min / HOUR}h with a Compliance lock.`);
  }
  if (ex.schedule) {
    const s = parseGoDuration(ex.schedule);
    if (s === undefined || s < 60 * 1000) errors.push('spec.export.schedule must be a duration of at least 1m, such as 6h.');
  }
  return errors;
}

export function exportPatch(ex: BackupExport): Array<{ op: 'add'; path: string; value: BackupExport }> {
  return [{ op: 'add', path: '/spec/export', value: ex }];
}

export interface ExportHealth {
  state: 'off' | 'waiting' | 'working' | 'failing';
  total: number;
  sent: number;
  failed: number;
  text: string;
}

/** Whether the export works, from the export state of each volume. */
export function exportHealth(policy: BackupPolicy): ExportHealth {
  const volumes = (policy.status?.volumes || []).filter((v) => !v.retained);
  const states = volumes.map((v) => exportStateText(v, policy));
  const total = volumes.length;
  const failed = states.filter((s) => s.severity === 'danger').length;
  const sent = volumes.filter((v) => !!v.exportedTime).length;
  if (!policy.spec?.export) return { state: 'off', total, sent, failed, text: 'The policy does not export to COS.' };
  const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? '' : 's'}`;
  if (failed > 0) {
    return {
      state: 'failing',
      total,
      sent,
      failed,
      text: `The export has a problem: ${plural(failed, 'volume')} failed, ${sent} of ${total} sent. Read the error of the volume below.`,
    };
  }
  if (sent === 0) {
    return { state: 'waiting', total, sent, failed, text: 'The export has not sent anything yet. The first export runs after the next local backup.' };
  }
  if (sent < total) {
    return { state: 'waiting', total, sent, failed, text: `${sent} of ${total} volumes have been sent to COS so far.` };
  }
  return { state: 'working', total, sent, failed, text: `The export works: ${sent} of ${total} volumes have been sent to COS.` };
}
