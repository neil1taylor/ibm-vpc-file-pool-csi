import { API_GROUP_VERSION } from '../constants';
import { BackupPolicy, FileSharePool, SubVolume } from '../types';
import { PersistentVolumeClaim, VirtualMachineLike, isValidK8sName } from './k8s-objects';
import { DEFAULT_VM_COPY_SPEED_MBPS } from './replication-helpers';

const MIN_MS = 60 * 1000;
const HOUR_MS = 60 * MIN_MS;
const DAY_MS = 24 * HOUR_MS;

export interface LabelPair {
  key: string;
  value: string;
}

export interface BackupPolicyForm {
  name: string;
  snapshotPool: string;
  schedule: string;
  keepFor: string;
  labels: LabelPair[];
  /** Namespaces, separated by commas or spaces. Empty means all. */
  namespaces: string;
  vmCopySpeed: number;
  suspend: boolean;
}

export const DEFAULT_KEEP_FOR = '7d';

export const EMPTY_BACKUP_FORM: BackupPolicyForm = {
  name: '',
  snapshotPool: '',
  schedule: '6h',
  keepFor: DEFAULT_KEEP_FOR,
  labels: [],
  namespaces: '',
  vmCopySpeed: DEFAULT_VM_COPY_SPEED_MBPS,
  suspend: false,
};

const UNITS_MS: Record<string, number> = { h: HOUR_MS, m: MIN_MS, s: 1000, ms: 1 };

/** A Go duration such as "6h", "1h30m" or "1.5h", in milliseconds. Undefined if it is not one. */
export function parseGoDuration(value: string): number | undefined {
  if (!/^(\d+(\.\d+)?(h|ms|m|s))+$/.test(value)) return undefined;
  let total = 0;
  for (const [, amount, unit] of value.matchAll(/(\d+(?:\.\d+)?)(h|ms|m|s)/g)) {
    total += parseFloat(amount) * UNITS_MS[unit];
  }
  return Math.round(total);
}

export function parseNamespaces(text: string): string[] {
  return text.split(/[,\s]+/).filter(Boolean);
}

/** A keep time in milliseconds: a whole number of days such as "30d", or a Go duration. Undefined if it is neither. */
function keepForMs(value: string): number | undefined {
  const days = value.match(/^(\d+)d$/);
  return days ? parseInt(days[1], 10) * DAY_MS : parseGoDuration(value);
}

export interface BackupPolicyFormErrors {
  name?: string;
  snapshotPool?: string;
  schedule?: string;
  keepFor?: string;
  labels?: string;
  vmCopySpeed?: string;
}

/** The common mistakes, shown at once. The admission webhook has the final say, and its message is shown too. */
export function validateBackupPolicyForm(
  form: BackupPolicyForm,
  context: { existingNames: string[]; editing: boolean; hasExpressions: boolean },
): BackupPolicyFormErrors {
  const errors: BackupPolicyFormErrors = {};
  if (!context.editing) {
    if (!isValidK8sName(form.name)) {
      errors.name = 'Use lower-case letters, digits and dashes, at most 63 characters. The name is used as a label value.';
    } else if (context.existingNames.includes(form.name)) {
      errors.name = 'A policy with this name already exists.';
    }
  }
  if (!form.snapshotPool) errors.snapshotPool = 'Choose the pool that holds the backups.';

  const schedule = parseGoDuration(form.schedule);
  if (schedule === undefined) {
    errors.schedule = 'Use a duration such as 30m, 6h or 1h30m.';
  } else if (schedule < MIN_MS) {
    errors.schedule = 'The schedule must be at least 1 minute.';
  }

  const keep = keepForMs(form.keepFor);
  if (keep === undefined) {
    errors.keepFor = 'Use a number of days such as 7d, or a duration such as 36h.';
  } else if (keep < HOUR_MS) {
    errors.keepFor = 'The keep time must be at least 1 hour.';
  }

  if (form.labels.length === 0 && !context.hasExpressions) {
    errors.labels = 'Add at least one label. A policy with no labels would back up every volume, so it is refused.';
  }
  if (!Number.isInteger(form.vmCopySpeed) || form.vmCopySpeed < 0) {
    errors.vmCopySpeed = 'Use a whole number of 0 or more. 0 means no limit.';
  }
  return errors;
}

/** The policy the form describes. When it edits a policy, everything the form does not show (export, match expressions) is kept. */
export function buildBackupPolicy(form: BackupPolicyForm, existing?: BackupPolicy): BackupPolicy {
  const spec: BackupPolicy['spec'] = { ...(existing?.spec as BackupPolicy['spec']) };
  spec.snapshotPool = form.snapshotPool;
  spec.schedule = form.schedule;
  spec.keepFor = form.keepFor;

  const selector: BackupPolicy['spec']['selector'] = {};
  if (form.labels.length > 0) {
    selector.matchLabels = Object.fromEntries(form.labels.map((l) => [l.key, l.value]));
  }
  const expressions = existing?.spec?.selector?.matchExpressions;
  if (expressions && expressions.length > 0) selector.matchExpressions = expressions;
  spec.selector = selector;

  const namespaces = parseNamespaces(form.namespaces);
  if (namespaces.length > 0) spec.namespaces = namespaces;
  else delete spec.namespaces;

  if (form.vmCopySpeed !== DEFAULT_VM_COPY_SPEED_MBPS) spec.vmCopySpeedLimitMBps = form.vmCopySpeed;
  else delete spec.vmCopySpeedLimitMBps;

  if (form.suspend) spec.suspend = true;
  else delete spec.suspend;

  return {
    ...existing,
    apiVersion: API_GROUP_VERSION,
    kind: 'BackupPolicy',
    metadata: existing?.metadata ?? { name: form.name },
    spec,
  };
}

export function policyToForm(policy: BackupPolicy): BackupPolicyForm {
  const spec = policy.spec;
  return {
    name: policy.metadata?.name || '',
    snapshotPool: spec.snapshotPool,
    schedule: spec.schedule,
    keepFor: spec.keepFor || DEFAULT_KEEP_FOR,
    labels: Object.entries(spec.selector?.matchLabels || {}).map(([key, value]) => ({ key, value })),
    namespaces: (spec.namespaces || []).join(', '),
    vmCopySpeed: spec.vmCopySpeedLimitMBps ?? DEFAULT_VM_COPY_SPEED_MBPS,
    suspend: !!spec.suspend,
  };
}

export interface SnapshotPoolChoice {
  name: string;
  eligible: boolean;
  /** Why the pool cannot be the snapshot pool, or empty. */
  reason: string;
}

// The controller (pkg/pool/backup_shares.go snapshotShares) refuses a pool that holds workload claims
// or has no ready share; the console shows the same two reasons before the policy is made.
export function snapshotPoolChoices(pools: FileSharePool[], subvolumes: SubVolume[]): SnapshotPoolChoice[] {
  return pools
    .map((pool): SnapshotPoolChoice => {
      const name = pool.metadata?.name || '';
      const claims = subvolumes.filter((sv) => sv.spec?.poolName === name).length;
      if (claims > 0) {
        return { name, eligible: false, reason: `holds ${claims} workload ${claims === 1 ? 'claim' : 'claims'}` };
      }
      const ready = (pool.status?.shares || []).some((s) => s.state === 'stable' && !!s.mountTargetIP);
      return ready ? { name, eligible: true, reason: '' } : { name, eligible: false, reason: 'has no ready share yet' };
    })
    .sort((a, b) => a.name.localeCompare(b.name));
}

export interface SelectionPreview {
  claims: number;
  vms: number;
  text: string;
}

const vmClaimNames = (vm: VirtualMachineLike): string[] =>
  (vm.spec?.template?.spec?.volumes || [])
    .map((v) => v.persistentVolumeClaim?.claimName || v.dataVolume?.name || '')
    .filter(Boolean);

/**
 * What a selector picks. `matching` are the claims whose labels match (already filtered by the cluster); the
 * namespaces limit them. A VM is picked through its disks: if any disk matches, all its disks join.
 */
export function selectionPreview(
  matching: PersistentVolumeClaim[],
  vms: VirtualMachineLike[],
  namespaces: string[],
): SelectionPreview {
  const key = (ns: string | undefined, name: string | undefined) => `${ns}/${name}`;
  const selected = new Set(
    matching
      .filter((p) => namespaces.length === 0 || namespaces.includes(p.metadata?.namespace || ''))
      .map((p) => key(p.metadata?.namespace, p.metadata?.name)),
  );
  const initial = new Set(selected);
  let vmCount = 0;
  for (const vm of vms) {
    const claims = vmClaimNames(vm);
    if (claims.some((c) => initial.has(key(vm.metadata?.namespace, c)))) {
      vmCount++;
      claims.forEach((c) => selected.add(key(vm.metadata?.namespace, c)));
    }
  }
  const n = selected.size;
  if (n === 0) return { claims: 0, vms: 0, text: 'No claim matches yet. Check the labels.' };
  const base = n === 1 ? '1 claim matches' : `${n} claims match`;
  const text = vmCount > 0 ? `${base}, including all disks of ${vmCount} ${vmCount === 1 ? 'VM' : 'VMs'}.` : `${base}.`;
  return { claims: n, vms: vmCount, text };
}
