import { BackupPolicy, FileSharePool, SubVolume } from '../types';
import { PersistentVolumeClaim, VirtualMachineLike } from './k8s-objects';
import {
  BackupPolicyForm,
  buildBackupPolicy,
  parseGoDuration,
  parseNamespaces,
  policyToForm,
  selectionPreview,
  snapshotPoolChoices,
  validateBackupPolicyForm,
} from './backup-policy-form';

const MIN = 60 * 1000;
const HOUR = 60 * MIN;

const form = (over: Partial<BackupPolicyForm> = {}): BackupPolicyForm => ({
  name: 'nightly',
  snapshotPool: 'snaps',
  schedule: '6h',
  keepFor: '7d',
  labels: [{ key: 'backup', value: 'yes' }],
  namespaces: '',
  vmCopySpeed: 20,
  suspend: false,
  ...over,
});

const ctx = { existingNames: ['other'], editing: false, hasExpressions: false };

describe('parseGoDuration', () => {
  it('reads Go durations', () => {
    expect(parseGoDuration('6h')).toBe(6 * HOUR);
    expect(parseGoDuration('1h30m')).toBe(90 * MIN);
    expect(parseGoDuration('90s')).toBe(90 * 1000);
    expect(parseGoDuration('1.5h')).toBe(90 * MIN);
    expect(parseGoDuration('500ms')).toBe(500);
  });

  it('gives undefined for anything else', () => {
    for (const bad of ['', 'soon', '6', '6x', '-1h', '1h 30m']) {
      expect(parseGoDuration(bad)).toBeUndefined();
    }
  });
});

describe('parseNamespaces', () => {
  it('splits on commas and spaces and drops empty names', () => {
    expect(parseNamespaces('apps, web  db,,')).toEqual(['apps', 'web', 'db']);
    expect(parseNamespaces('')).toEqual([]);
  });
});

describe('validateBackupPolicyForm', () => {
  it('accepts a good form', () => {
    expect(validateBackupPolicyForm(form(), ctx)).toEqual({});
  });

  it('needs a name that is a Kubernetes name, free, and short enough to be a label value', () => {
    expect(validateBackupPolicyForm(form({ name: '' }), ctx).name).toBeTruthy();
    expect(validateBackupPolicyForm(form({ name: 'Night_ly' }), ctx).name).toBeTruthy();
    expect(validateBackupPolicyForm(form({ name: 'other' }), ctx).name).toMatch(/already exists/);
    expect(validateBackupPolicyForm(form({ name: 'a'.repeat(64) }), ctx).name).toBeTruthy();
  });

  it('does not check the name when editing', () => {
    expect(validateBackupPolicyForm(form({ name: 'other' }), { ...ctx, editing: true }).name).toBeUndefined();
  });

  it('needs a snapshot pool', () => {
    expect(validateBackupPolicyForm(form({ snapshotPool: '' }), ctx).snapshotPool).toBeTruthy();
  });

  it('needs a schedule that is a duration of at least one minute', () => {
    expect(validateBackupPolicyForm(form({ schedule: '' }), ctx).schedule).toBeTruthy();
    expect(validateBackupPolicyForm(form({ schedule: 'daily' }), ctx).schedule).toBeTruthy();
    expect(validateBackupPolicyForm(form({ schedule: '30s' }), ctx).schedule).toMatch(/at least 1 minute/);
    expect(validateBackupPolicyForm(form({ schedule: '1m' }), ctx).schedule).toBeUndefined();
  });

  it('needs a keep time of at least one hour, as days or a duration', () => {
    expect(validateBackupPolicyForm(form({ keepFor: '30d' }), ctx).keepFor).toBeUndefined();
    expect(validateBackupPolicyForm(form({ keepFor: '36h' }), ctx).keepFor).toBeUndefined();
    expect(validateBackupPolicyForm(form({ keepFor: '30m' }), ctx).keepFor).toMatch(/at least 1 hour/);
    expect(validateBackupPolicyForm(form({ keepFor: 'forever' }), ctx).keepFor).toBeTruthy();
  });

  it('refuses an empty selector, as it would back up every volume', () => {
    expect(validateBackupPolicyForm(form({ labels: [] }), ctx).labels).toMatch(/every volume/);
  });

  it('accepts no labels when the policy has match expressions that the form keeps', () => {
    expect(validateBackupPolicyForm(form({ labels: [] }), { ...ctx, hasExpressions: true }).labels).toBeUndefined();
  });

  it('needs a copy speed that is a whole number of at least 0', () => {
    expect(validateBackupPolicyForm(form({ vmCopySpeed: -1 }), ctx).vmCopySpeed).toBeTruthy();
    expect(validateBackupPolicyForm(form({ vmCopySpeed: 2.5 }), ctx).vmCopySpeed).toBeTruthy();
    expect(validateBackupPolicyForm(form({ vmCopySpeed: 0 }), ctx).vmCopySpeed).toBeUndefined();
  });
});

describe('buildBackupPolicy', () => {
  it('makes the policy from the form, leaving out what is at its default or empty', () => {
    const bp = buildBackupPolicy(form());
    expect(bp).toEqual({
      apiVersion: 'storage.ibmcloud.io/v1alpha1',
      kind: 'BackupPolicy',
      metadata: { name: 'nightly' },
      spec: {
        snapshotPool: 'snaps',
        schedule: '6h',
        keepFor: '7d',
        selector: { matchLabels: { backup: 'yes' } },
      },
    });
  });

  it('adds namespaces, a changed copy speed (even 0) and suspend', () => {
    const bp = buildBackupPolicy(form({ namespaces: 'apps, web', vmCopySpeed: 0, suspend: true }));
    expect(bp.spec.namespaces).toEqual(['apps', 'web']);
    expect(bp.spec.vmCopySpeedLimitMBps).toBe(0);
    expect(bp.spec.suspend).toBe(true);
    expect(buildBackupPolicy(form({ vmCopySpeed: 50 })).spec.vmCopySpeedLimitMBps).toBe(50);
  });

  const existing = (): BackupPolicy =>
    ({
      apiVersion: 'storage.ibmcloud.io/v1alpha1',
      kind: 'BackupPolicy',
      metadata: { name: 'nightly', resourceVersion: '12', uid: 'u1' },
      spec: {
        snapshotPool: 'old',
        schedule: '1h',
        selector: {
          matchLabels: { old: 'label' },
          matchExpressions: [{ key: 'tier', operator: 'In', values: ['gold'] }],
        },
        vmCopySpeedLimitMBps: 5,
        export: { bucket: 'b', endpoint: 'e' },
      },
      status: { phase: 'Active' },
    }) as unknown as BackupPolicy;

  it('keeps the metadata, the export block and the match expressions of the policy it edits', () => {
    const bp = buildBackupPolicy(form({ labels: [{ key: 'backup', value: 'yes' }] }), existing());
    expect(bp.metadata).toEqual({ name: 'nightly', resourceVersion: '12', uid: 'u1' });
    expect(bp.spec.export).toEqual({ bucket: 'b', endpoint: 'e' });
    expect(bp.spec.selector).toEqual({
      matchLabels: { backup: 'yes' },
      matchExpressions: [{ key: 'tier', operator: 'In', values: ['gold'] }],
    });
    expect(bp.spec.snapshotPool).toBe('snaps');
  });

  it('removes a setting that the form puts back to its default or empty', () => {
    const bp = buildBackupPolicy(form({ vmCopySpeed: 20, labels: [] }), existing());
    expect(bp.spec.vmCopySpeedLimitMBps).toBeUndefined();
    expect(bp.spec.selector.matchLabels).toBeUndefined();
  });
});

describe('policyToForm', () => {
  it('fills the form from a policy, with defaults for what is not set', () => {
    const f = policyToForm({
      metadata: { name: 'nightly' },
      spec: {
        snapshotPool: 'snaps',
        schedule: '6h',
        selector: { matchLabels: { a: 'b', c: 'd' } },
        namespaces: ['apps', 'web'],
        suspend: true,
      },
    } as BackupPolicy);
    expect(f).toEqual({
      name: 'nightly',
      snapshotPool: 'snaps',
      schedule: '6h',
      keepFor: '7d',
      labels: [
        { key: 'a', value: 'b' },
        { key: 'c', value: 'd' },
      ],
      namespaces: 'apps, web',
      vmCopySpeed: 20,
      suspend: true,
    });
  });
});

describe('snapshotPoolChoices', () => {
  const pool = (name: string, shares: unknown[] = [{ state: 'stable', mountTargetIP: '10.0.0.1' }]) =>
    ({ metadata: { name }, status: { shares } }) as unknown as FileSharePool;
  const sv = (poolName: string) => ({ spec: { poolName } }) as SubVolume;

  it('offers a pool that holds no workload claims and has a ready share', () => {
    expect(snapshotPoolChoices([pool('snaps')], [sv('data')])).toEqual([{ name: 'snaps', eligible: true, reason: '' }]);
  });

  it('refuses a pool that holds claims, and says how many', () => {
    const [c] = snapshotPoolChoices([pool('data')], [sv('data'), sv('data')]);
    expect(c.eligible).toBe(false);
    expect(c.reason).toBe('holds 2 workload claims');
  });

  it('refuses a pool with no ready share', () => {
    const [c] = snapshotPoolChoices([pool('new', [{ state: 'creating', mountTargetIP: '' }])], []);
    expect(c).toEqual({ name: 'new', eligible: false, reason: 'has no ready share yet' });
  });

  it('lists the choices by name', () => {
    expect(snapshotPoolChoices([pool('b'), pool('a')], []).map((c) => c.name)).toEqual(['a', 'b']);
  });
});

describe('selectionPreview', () => {
  const pvc = (namespace: string, name: string) => ({ metadata: { namespace, name } }) as PersistentVolumeClaim;
  const vm = (namespace: string, name: string, claims: string[]) =>
    ({
      metadata: { namespace, name },
      spec: { template: { spec: { volumes: claims.map((c) => ({ persistentVolumeClaim: { claimName: c } })) } } },
    }) as VirtualMachineLike;

  it('counts the matching claims', () => {
    const p = selectionPreview([pvc('a', 'x'), pvc('a', 'y')], [], []);
    expect(p).toMatchObject({ claims: 2, vms: 0, text: '2 claims match.' });
  });

  it('counts one claim in the singular, and none', () => {
    expect(selectionPreview([pvc('a', 'x')], [], []).text).toBe('1 claim matches.');
    expect(selectionPreview([], [], []).text).toBe('No claim matches yet. Check the labels.');
  });

  it('limits the claims to the namespaces', () => {
    const p = selectionPreview([pvc('a', 'x'), pvc('b', 'y')], [], ['a']);
    expect(p.claims).toBe(1);
  });

  it('adds the other disks of a VM when one disk matches', () => {
    const p = selectionPreview([pvc('a', 'root')], [vm('a', 'web', ['root', 'data']), vm('a', 'other', ['zzz'])], []);
    expect(p).toMatchObject({ claims: 2, vms: 1 });
    expect(p.text).toBe('2 claims match, including all disks of 1 VM.');
  });
});
