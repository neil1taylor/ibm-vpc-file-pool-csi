import { BackupPolicy } from '../types';
import {
  COSRestoreForm,
  buildCOSRestore,
  cosRestorePath,
  parseCOSRestoreQuery,
  cosListAccessRights,
  describeExport,
  driverImageOf,
  exportRepoOf,
  parseCOSListing,
  restoreFormFromPolicy,
  validateCOSRestore,
} from './backup-cos';

const policy = (over: Partial<BackupPolicy['spec']> = {}) =>
  ({
    metadata: { name: 'nightly' },
    spec: {
      snapshotPool: 'snaps',
      schedule: '6h',
      selector: {},
      export: {
        bucket: 'pool-backups',
        endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
        prefix: 'lab',
        credentialsSecret: 'cos-credentials',
        backupKeySecret: 'cos-backup-key',
        keyProtect: { instanceID: 'inst-1', region: 'eu-de', rootKeyID: 'root-1', apiKeySecret: 'keyprotect-apikey' },
        keepFor: '30d',
        lock: 'Compliance',
        schedule: '6h',
      },
      ...over,
    },
  }) as BackupPolicy;

describe('exportRepoOf', () => {
  it('is the repository of the policy: the area below the prefix, and the names of the Secrets', () => {
    expect(exportRepoOf(policy())).toEqual({
      endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
      bucket: 'pool-backups',
      prefix: 'lab/nightly/',
      credentialsSecret: 'cos-credentials',
      backupKeySecret: 'cos-backup-key',
      keyProtect: { instanceID: 'inst-1', region: 'eu-de', rootKeyID: 'root-1', apiKeySecret: 'keyprotect-apikey' },
      emergencyKeySecret: '',
    });
  });

  it('is undefined for a policy with no export', () => {
    expect(exportRepoOf(policy({ export: undefined }))).toBeUndefined();
  });

  it('keeps the emergency key Secret when there is one', () => {
    const p = policy();
    p.spec.export!.emergencyKeySecret = 'cos-emergency';
    expect(exportRepoOf(p)?.emergencyKeySecret).toBe('cos-emergency');
  });
});

describe('describeExport', () => {
  it('lists the settings in plain words, with Secret names and never values', () => {
    const rows = Object.fromEntries(describeExport(policy().spec.export!).map((r) => [r.label, r.value]));
    expect(rows).toMatchObject({
      Bucket: 'pool-backups',
      Endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
      Folder: 'lab/',
      'Keep in COS': '30d',
      Lock: 'Compliance: nobody can delete or shorten it',
      'Send at most every': '6h',
      'COS key (Secret)': 'cos-credentials',
      'Backup key, wrapped (Secret)': 'cos-backup-key',
      'Key Protect': 'instance inst-1, region eu-de, root key root-1',
      'Key Protect API key (Secret)': 'keyprotect-apikey',
      'Emergency key (Secret)': 'None',
    });
  });

  it('uses the defaults for what is not set', () => {
    const ex = policy().spec.export!;
    delete ex.keepFor;
    delete ex.lock;
    delete ex.schedule;
    delete ex.prefix;
    const rows = Object.fromEntries(describeExport(ex).map((r) => [r.label, r.value]));
    expect(rows['Keep in COS']).toBe('30d (default)');
    expect(rows.Lock).toBe('Compliance: nobody can delete or shorten it (default)');
    expect(rows['Send at most every']).toBe('After each local backup (default)');
    expect(rows.Folder).toBe('The top of the bucket');
  });

  it('says when there is no lock', () => {
    const ex = policy().spec.export!;
    ex.lock = 'None';
    expect(Object.fromEntries(describeExport(ex).map((r) => [r.label, r.value])).Lock).toBe('None');
  });
});

describe('parseCOSListing', () => {
  it('reads the termination message of the list Job', () => {
    const msg = '{"volumes":[{"volume":"pvc-1","moments":["2026-10-20T09:00:00Z","2026-10-21T09:00:00Z"]},{"volume":"pvc-2","moments":[]}],"truncated":false}';
    expect(parseCOSListing(msg)).toEqual({
      volumes: [
        { name: 'pvc-1', moments: ['2026-10-20T09:00:00Z', '2026-10-21T09:00:00Z'] },
        { name: 'pvc-2', moments: [] },
      ],
      truncated: false,
    });
  });

  it('keeps the truncated flag', () => {
    expect(parseCOSListing('{"volumes":[],"truncated":true}')?.truncated).toBe(true);
  });

  it('gives undefined for anything that is not a listing', () => {
    expect(parseCOSListing('')).toBeUndefined();
    expect(parseCOSListing('not json')).toBeUndefined();
    expect(parseCOSListing('{"other":1}')).toBeUndefined();
  });
});

describe('driverImageOf', () => {
  it('is the image of the csi-controller container', () => {
    const deployments = [
      { spec: { template: { spec: { containers: [{ name: 'sidecar', image: 'a' }, { name: 'csi-controller', image: 'icr.io/x/driver:v1' }] } } } },
    ];
    expect(driverImageOf(deployments)).toBe('icr.io/x/driver:v1');
  });

  it('is empty when there is no controller', () => {
    expect(driverImageOf([])).toBe('');
    expect(driverImageOf(undefined)).toBe('');
  });
});

describe('cosListAccessRights', () => {
  it('needs to make and remove Jobs, and to list pods, in the driver namespace', () => {
    expect(cosListAccessRights('kube-system').map((r) => `${r.verb} ${r.resource}`)).toEqual(['create jobs', 'delete jobs', 'list pods']);
  });
});

const form = (over: Partial<COSRestoreForm> = {}): COSRestoreForm => ({
  policy: 'nightly',
  bucket: 'pool-backups',
  endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
  prefix: 'lab',
  credentialsSecret: 'cos-credentials',
  backupKeySecret: 'cos-backup-key',
  kpInstance: 'inst-1',
  kpRegion: 'eu-de',
  kpRootKey: 'root-1',
  kpApiKeySecret: 'keyprotect-apikey',
  emergencyKeySecret: '',
  volume: 'pvc-11111111-2222-3333-4444-555555555555',
  moment: '',
  targetNamespace: 'apps',
  targetName: 'data-restored',
  storageClass: 'pool-a',
  sizeGB: 20,
  ...over,
});

describe('restoreFormFromPolicy', () => {
  it('fills the COS settings from the export of a policy', () => {
    const f = restoreFormFromPolicy(policy());
    expect(f).toMatchObject({
      policy: 'nightly',
      bucket: 'pool-backups',
      endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
      prefix: 'lab',
      credentialsSecret: 'cos-credentials',
      backupKeySecret: 'cos-backup-key',
      kpInstance: 'inst-1',
      kpRegion: 'eu-de',
      kpRootKey: 'root-1',
      kpApiKeySecret: 'keyprotect-apikey',
    });
  });

  it('gives the usual Secret names when there is no export', () => {
    const f = restoreFormFromPolicy(policy({ export: undefined }));
    expect(f).toMatchObject({ credentialsSecret: 'cos-credentials', backupKeySecret: 'cos-backup-key', kpApiKeySecret: 'keyprotect-apikey' });
    expect(f.bucket).toBe('');
  });
});

describe('validateCOSRestore', () => {
  it('accepts a good form', () => {
    expect(validateCOSRestore(form(), [])).toEqual({});
  });

  it('checks the bucket, the endpoint, the prefix and the Secret names', () => {
    expect(validateCOSRestore(form({ bucket: 'Bad_Bucket' }), []).bucket).toBeTruthy();
    expect(validateCOSRestore(form({ endpoint: 'example.com' }), []).endpoint).toMatch(/COS S3 host/);
    expect(validateCOSRestore(form({ endpoint: 's3.private.eu-de.cloud-object-storage.appdomain.cloud' }), []).endpoint).toBeUndefined();
    expect(validateCOSRestore(form({ prefix: '../x' }), []).prefix).toBeTruthy();
    expect(validateCOSRestore(form({ credentialsSecret: 'Bad Name' }), []).credentialsSecret).toBeTruthy();
    expect(validateCOSRestore(form({ backupKeySecret: '' }), []).backupKeySecret).toBeTruthy();
    expect(validateCOSRestore(form({ kpApiKeySecret: '' }), []).kpApiKeySecret).toBeTruthy();
  });

  it('needs the Key Protect instance, region and root key', () => {
    expect(validateCOSRestore(form({ kpInstance: '' }), []).kpInstance).toBeTruthy();
    expect(validateCOSRestore(form({ kpRegion: 'EU DE' }), []).kpRegion).toBeTruthy();
    expect(validateCOSRestore(form({ kpRootKey: '' }), []).kpRootKey).toBeTruthy();
  });

  it('needs a policy name, a volume name, a size and a storage class, because COS keeps none of them', () => {
    expect(validateCOSRestore(form({ policy: '' }), []).policy).toBeTruthy();
    expect(validateCOSRestore(form({ volume: 'data' }), []).volume).toMatch(/pvc-/);
    expect(validateCOSRestore(form({ sizeGB: 0 }), []).sizeGB).toBeTruthy();
    expect(validateCOSRestore(form({ sizeGB: 1.5 }), []).sizeGB).toBeTruthy();
    expect(validateCOSRestore(form({ storageClass: '' }), []).storageClass).toMatch(/not keep/);
  });

  it('refuses a claim name that exists, and a moment that is not a time', () => {
    expect(validateCOSRestore(form({ targetName: 'taken' }), ['taken']).targetName).toMatch(/already exists/);
    expect(validateCOSRestore(form({ moment: 'soon' }), []).moment).toBeTruthy();
  });
});

describe('buildCOSRestore', () => {
  it('makes the BackupRestore that `kubectl backup restore --from-cos` makes', () => {
    const br = buildCOSRestore(form(), 1761000000000);
    expect(br).toEqual({
      apiVersion: 'storage.ibmcloud.io/v1alpha1',
      kind: 'BackupRestore',
      metadata: { name: `restore-pvc-11111111-2222-3333-4444-555555555555-${(1761000000).toString(36)}`.slice(0, 63).replace(/[-.]+$/, '') },
      spec: {
        policy: 'nightly',
        targetNamespace: 'apps',
        targetName: 'data-restored',
        storageClassName: 'pool-a',
        fromCOS: {
          bucket: 'pool-backups',
          endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
          prefix: 'lab',
          credentialsSecret: 'cos-credentials',
          volume: 'pvc-11111111-2222-3333-4444-555555555555',
          sizeGB: 20,
          backupKeySecret: 'cos-backup-key',
          keyProtect: { instanceID: 'inst-1', region: 'eu-de', rootKeyID: 'root-1', apiKeySecret: 'keyprotect-apikey' },
        },
      },
    });
  });

  it('adds the moment without fractions, the emergency Secret, and leaves out an empty prefix', () => {
    const br = buildCOSRestore(
      form({ moment: '2026-10-20T09:00:00.000Z', emergencyKeySecret: 'cos-emergency', prefix: '' }),
      1761000000000,
    );
    expect(br.spec.moment).toBe('2026-10-20T09:00:00Z');
    expect(br.spec.fromCOS).toMatchObject({ emergencyKeySecret: 'cos-emergency' });
    expect(br.spec.fromCOS).not.toHaveProperty('prefix');
  });
});

describe('cosRestorePath and parseCOSRestoreQuery', () => {
  it('builds the link to the restore-from-COS page with the policy, the volume and the moment as query values', () => {
    expect(cosRestorePath({})).toBe('/vpc-file-pools/backups/~cos-restore');
    expect(cosRestorePath({ policy: 'nightly', volume: 'pvc-1', moment: '2026-10-20T09:00:00Z' })).toBe(
      '/vpc-file-pools/backups/~cos-restore?policy=nightly&volume=pvc-1&moment=2026-10-20T09%3A00%3A00Z',
    );
  });

  it('reads them back', () => {
    expect(parseCOSRestoreQuery('?policy=nightly&volume=pvc-1&moment=2026-10-20T09%3A00%3A00Z')).toEqual({
      policy: 'nightly',
      volume: 'pvc-1',
      moment: '2026-10-20T09:00:00Z',
    });
    expect(parseCOSRestoreQuery('')).toEqual({ policy: '', volume: '', moment: '' });
  });
});
