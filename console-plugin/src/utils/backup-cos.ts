import { API_GROUP_VERSION, ROUTES } from '../constants';
import { BackupExport, BackupPolicy, BackupRestore } from '../types';
import { AccessRight } from './backup-delete';
import { COSRepo, cosAreaPrefix } from './backup-jobs';
import { restoreRequestName } from './backup-helpers';
import { isValidK8sName } from './k8s-objects';

/** The repository of a policy's export: its area in the bucket, and the names of the Secrets (never their values). */
export function exportRepoOf(policy: BackupPolicy): COSRepo | undefined {
  const ex = policy.spec?.export;
  if (!ex) return undefined;
  return {
    endpoint: ex.endpoint,
    bucket: ex.bucket,
    prefix: cosAreaPrefix(ex.prefix || '', policy.metadata?.name || ''),
    credentialsSecret: ex.credentialsSecret,
    backupKeySecret: ex.backupKeySecret,
    keyProtect: { ...ex.keyProtect },
    emergencyKeySecret: ex.emergencyKeySecret || '',
  };
}

export interface ExportRow {
  label: string;
  value: string;
}

/** The export settings of a policy as rows of plain words. Secrets are shown by name only. */
export function describeExport(ex: BackupExport): ExportRow[] {
  const lock = ex.lock || 'Compliance';
  const lockText = lock === 'Compliance' ? 'Compliance: nobody can delete or shorten it' : 'None';
  return [
    { label: 'Bucket', value: ex.bucket },
    { label: 'Endpoint', value: ex.endpoint },
    { label: 'Folder', value: ex.prefix ? `${ex.prefix.replace(/\/+$/, '')}/` : 'The top of the bucket' },
    { label: 'Keep in COS', value: ex.keepFor || '30d (default)' },
    { label: 'Lock', value: ex.lock ? lockText : `${lockText} (default)` },
    { label: 'Send at most every', value: ex.schedule || 'After each local backup (default)' },
    { label: 'COS key (Secret)', value: ex.credentialsSecret },
    { label: 'Backup key, wrapped (Secret)', value: ex.backupKeySecret },
    {
      label: 'Key Protect',
      value: `instance ${ex.keyProtect.instanceID}, region ${ex.keyProtect.region}, root key ${ex.keyProtect.rootKeyID}`,
    },
    { label: 'Key Protect API key (Secret)', value: ex.keyProtect.apiKeySecret },
    { label: 'Emergency key (Secret)', value: ex.emergencyKeySecret || 'None' },
  ];
}

export interface COSListing {
  volumes: Array<{ name: string; moments: string[] }>;
  /** True when there were more moments than the Job could report. Ask for one volume to see all of its moments. */
  truncated: boolean;
}

/** The termination message of the list Job. Undefined if it is not a listing. */
export function parseCOSListing(message: string): COSListing | undefined {
  try {
    const m = JSON.parse(message) as { volumes?: Array<{ volume: string; moments?: string[] }>; truncated?: boolean };
    if (!Array.isArray(m.volumes)) return undefined;
    return {
      volumes: m.volumes.map((v) => ({ name: v.volume, moments: v.moments || [] })),
      truncated: !!m.truncated,
    };
  } catch {
    return undefined;
  }
}

interface DeploymentLike {
  spec?: { template?: { spec?: { containers?: Array<{ name?: string; image?: string }> } } };
}

/** The image of the driver's controller container, which the list Job runs. Empty if there is none. */
export function driverImageOf(deployments: DeploymentLike[] | undefined): string {
  for (const d of deployments || []) {
    const c = (d.spec?.template?.spec?.containers || []).find((x) => x.name === 'csi-controller');
    if (c?.image) return c.image;
  }
  return '';
}

/** What the list Job needs: it makes and removes a Job, and the console reads its pod. */
export function cosListAccessRights(driverNamespace: string): AccessRight[] {
  return [
    { group: 'batch', resource: 'jobs', verb: 'create', namespace: driverNamespace },
    { group: 'batch', resource: 'jobs', verb: 'delete', namespace: driverNamespace },
    { group: '', resource: 'pods', verb: 'list', namespace: driverNamespace },
  ];
}

// ── Restore from COS ──

export interface COSRestoreForm {
  /** The policy whose area in the bucket holds the backups. It need not exist in this cluster. */
  policy: string;
  bucket: string;
  endpoint: string;
  prefix: string;
  credentialsSecret: string;
  backupKeySecret: string;
  kpInstance: string;
  kpRegion: string;
  kpRootKey: string;
  kpApiKeySecret: string;
  /** Optional: a Secret that holds the readable backup key, used only when Key Protect cannot be reached. */
  emergencyKeySecret: string;
  volume: string;
  /** An ISO time. Empty means the newest export. */
  moment: string;
  targetNamespace: string;
  targetName: string;
  storageClass: string;
  sizeGB: number;
}

/** The settings of a policy's export (or the usual Secret names, when it has none) as a restore form. */
export function restoreFormFromPolicy(policy: BackupPolicy): COSRestoreForm {
  const ex = policy.spec?.export;
  return {
    policy: policy.metadata?.name || '',
    bucket: ex?.bucket || '',
    endpoint: ex?.endpoint || '',
    prefix: ex?.prefix || '',
    credentialsSecret: ex?.credentialsSecret || 'cos-credentials',
    backupKeySecret: ex?.backupKeySecret || 'cos-backup-key',
    kpInstance: ex?.keyProtect.instanceID || '',
    kpRegion: ex?.keyProtect.region || '',
    kpRootKey: ex?.keyProtect.rootKeyID || '',
    kpApiKeySecret: ex?.keyProtect.apiKeySecret || 'keyprotect-apikey',
    emergencyKeySecret: ex?.emergencyKeySecret || '',
    volume: '',
    moment: '',
    targetNamespace: '',
    targetName: '',
    storageClass: '',
    sizeGB: 1,
  };
}

// The rules of api/v1alpha1/backuprestore_cos.go and pkg/pool/share_job_export.go.
const ENDPOINT = /^(https:\/\/)?s3\.(direct\.|private\.)?[a-z0-9-]+\.cloud-object-storage\.appdomain\.cloud$/;
const BUCKET = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/;
const PREFIX = /^[A-Za-z0-9._/-]*$/;
const SECRET_NAME = /^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$/;
const REGION = /^[a-z0-9]+(-[a-z0-9]+)*$/;
const KEY_ID = /^[A-Za-z0-9][A-Za-z0-9-]*$/;
const VOLUME = /^pvc-[a-f0-9-]{36}$/;
const POLICY = /^[a-z0-9]([-a-z0-9.]{0,61}[a-z0-9])?$/;

export type COSRestoreErrors = Partial<Record<keyof COSRestoreForm, string>>;

/** The common mistakes, shown at once. The admission webhook has the final say. */
export function validateCOSRestore(form: COSRestoreForm, existingClaims: string[]): COSRestoreErrors {
  const e: COSRestoreErrors = {};
  if (!POLICY.test(form.policy)) e.policy = 'Enter the name of the policy whose area in the bucket holds the backups.';
  if (!BUCKET.test(form.bucket)) e.bucket = 'This is not a valid bucket name.';
  if (!ENDPOINT.test(form.endpoint)) {
    e.endpoint = 'This is not a COS S3 host such as s3.eu-de.cloud-object-storage.appdomain.cloud.';
  }
  if (!PREFIX.test(form.prefix) || form.prefix.includes('..')) e.prefix = 'This folder has characters that are not allowed.';
  for (const [field, label] of [
    ['credentialsSecret', 'COS key'],
    ['backupKeySecret', 'wrapped backup key'],
    ['kpApiKeySecret', 'Key Protect API key'],
  ] as const) {
    if (!SECRET_NAME.test(form[field])) e[field] = `Enter the name of the Secret with the ${label}.`;
  }
  if (form.emergencyKeySecret && !SECRET_NAME.test(form.emergencyKeySecret)) {
    e.emergencyKeySecret = 'This is not a valid Secret name.';
  }
  if (!KEY_ID.test(form.kpInstance)) e.kpInstance = 'Enter the Key Protect instance ID.';
  if (!REGION.test(form.kpRegion)) e.kpRegion = 'Enter the region of the Key Protect instance, such as eu-de.';
  if (!KEY_ID.test(form.kpRootKey)) e.kpRootKey = 'Enter the ID of the Key Protect root key.';
  if (!VOLUME.test(form.volume)) e.volume = 'Choose a volume, or enter its name: pvc- and a UUID.';
  if (!Number.isInteger(form.sizeGB) || form.sizeGB < 1) e.sizeGB = 'COS does not keep the size: enter a whole number of GB, at least 1.';
  if (!form.storageClass) e.storageClass = 'COS does not keep the storage class: choose one.';
  if (!isValidK8sName(form.targetNamespace)) e.targetNamespace = 'Use lower-case letters, digits and dashes.';
  if (!isValidK8sName(form.targetName)) {
    e.targetName = 'Use lower-case letters, digits and dashes.';
  } else if (existingClaims.includes(form.targetName)) {
    e.targetName = 'A claim with this name already exists in the namespace. A restore never writes over a volume: choose a new name.';
  }
  if (form.moment && Number.isNaN(new Date(form.moment).getTime())) e.moment = 'This is not a time.';
  return e;
}

/** The BackupRestore that `kubectl backup restore --from-cos` makes. */
export function buildCOSRestore(form: COSRestoreForm, nowMs: number): BackupRestore {
  const fromCOS: NonNullable<BackupRestore['spec']['fromCOS']> = {
    bucket: form.bucket,
    endpoint: form.endpoint,
    credentialsSecret: form.credentialsSecret,
    volume: form.volume,
    sizeGB: form.sizeGB,
    backupKeySecret: form.backupKeySecret,
    keyProtect: {
      instanceID: form.kpInstance,
      region: form.kpRegion,
      rootKeyID: form.kpRootKey,
      apiKeySecret: form.kpApiKeySecret,
    },
  };
  if (form.prefix) fromCOS.prefix = form.prefix;
  if (form.emergencyKeySecret) fromCOS.emergencyKeySecret = form.emergencyKeySecret;
  const spec: BackupRestore['spec'] = {
    policy: form.policy,
    targetNamespace: form.targetNamespace,
    targetName: form.targetName,
    storageClassName: form.storageClass,
    fromCOS,
  };
  if (form.moment) spec.moment = form.moment.replace(/\.\d+Z$/, 'Z');
  return {
    apiVersion: API_GROUP_VERSION,
    kind: 'BackupRestore',
    metadata: { name: restoreRequestName(form.volume, nowMs) },
    spec,
  };
}

/** The link to the restore-from-COS page. The policy, the volume and the moment are query values, so other pages can link to it. */
export function cosRestorePath(input: { policy?: string; volume?: string; moment?: string }): string {
  const query = new URLSearchParams();
  if (input.policy) query.set('policy', input.policy);
  if (input.volume) query.set('volume', input.volume);
  if (input.moment) query.set('moment', input.moment);
  const q = query.toString();
  return `${ROUTES.BACKUP_COS_RESTORE}${q ? `?${q}` : ''}`;
}

export function parseCOSRestoreQuery(search: string): { policy: string; volume: string; moment: string } {
  const q = new URLSearchParams(search);
  return { policy: q.get('policy') || '', volume: q.get('volume') || '', moment: q.get('moment') || '' };
}
