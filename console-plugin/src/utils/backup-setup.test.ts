import { BackupPolicy } from '../types';
import {
  apiKeySecret,
  buildCosSetupCommand,
  buildSecret,
  exportHealth,
  exportPatch,
  parseExportBlock,
  parseSecretManifests,
  validateExport,
} from './backup-setup';

const credsManifest = `apiVersion: v1
kind: Secret
metadata:
  name: cos-credentials
  namespace: kube-system
type: Opaque
stringData:
  access_key_id: "AKIA123"
  secret_access_key: "s3cr\\"et"
`;
const keyManifest = `apiVersion: v1
kind: Secret
metadata:
  name: cos-backup-key
  namespace: kube-system
type: Opaque
stringData:
  wrappedKey: "eyJ3cmFwIjoxfQ=="
`;

describe('buildCosSetupCommand', () => {
  const base = {
    region: 'eu-de',
    resourceGroup: 'default',
    bucket: 'pool-backups',
    rootKey: 'crn:v1:bluemix:public:kms:eu-de:a/acc:inst:key:keyid',
    vpcCRN: 'crn:v1:bluemix:public:is:eu-de:a/acc::vpc:r010-1',
    adminAddresses: '203.0.113.5, 198.51.100.7',
    prefix: '',
    keepFor: '30d',
    lock: 'Compliance',
  };

  it('is the command with the names filled in, one flag to a line', () => {
    const cmd = buildCosSetupCommand(base);
    expect(cmd).toBe(
      [
        'kubectl backup cos-setup \\',
        '  --region eu-de \\',
        '  --resource-group default \\',
        '  --bucket pool-backups \\',
        "  --root-key 'crn:v1:bluemix:public:kms:eu-de:a/acc:inst:key:keyid' \\",
        "  --vpc-crn 'crn:v1:bluemix:public:is:eu-de:a/acc::vpc:r010-1' \\",
        '  --admin-address 203.0.113.5,198.51.100.7 \\',
        '  --key-file cos-key.yaml \\',
        '  --secret-file cos-wrapped-key.yaml',
      ].join('\n'),
    );
  });

  it('adds the prefix, a changed keep time and a lock of None; --dry-run on request', () => {
    const cmd = buildCosSetupCommand({ ...base, prefix: 'lab', keepFor: '60d', lock: 'None', dryRun: true });
    expect(cmd).toContain('--prefix lab');
    expect(cmd).toContain('--keep-for 60d');
    expect(cmd).toContain('--lock None');
    expect(cmd.trimEnd().endsWith('--dry-run')).toBe(true);
  });

  it('quotes a value with a character the shell would act on', () => {
    expect(buildCosSetupCommand({ ...base, resourceGroup: 'my group; ls' })).toContain("--resource-group 'my group; ls'");
  });

  it('leaves out a flag whose value is empty', () => {
    const cmd = buildCosSetupCommand({ ...base, vpcCRN: '', adminAddresses: '' });
    expect(cmd).not.toContain('--vpc-crn');
    expect(cmd).not.toContain('--admin-address');
  });
});

describe('parseSecretManifests', () => {
  it('reads the Secrets that cos-setup writes', () => {
    const r = parseSecretManifests(`${credsManifest}---\n${keyManifest}`);
    expect(r.errors).toEqual([]);
    expect(r.secrets).toEqual([
      { name: 'cos-credentials', namespace: 'kube-system', data: { access_key_id: 'AKIA123', secret_access_key: 's3cr"et' } },
      { name: 'cos-backup-key', namespace: 'kube-system', data: { wrappedKey: 'eyJ3cmFwIjoxfQ==' } },
    ]);
  });

  it('accepts one Secret, and an empty box', () => {
    expect(parseSecretManifests(keyManifest).secrets).toHaveLength(1);
    expect(parseSecretManifests('  \n')).toEqual({ secrets: [], errors: [] });
  });

  it('refuses a Secret for another namespace', () => {
    const r = parseSecretManifests(keyManifest.replace('kube-system', 'default'));
    expect(r.errors[0]).toMatch(/kube-system only/);
    expect(r.secrets).toEqual([]);
  });

  it('refuses a document that is not a Secret', () => {
    expect(parseSecretManifests('kind: ConfigMap\nmetadata:\n  name: x\n').errors[0]).toMatch(/not a Secret/);
  });

  it('refuses keys it does not expect, and says why for the readable backup key', () => {
    expect(parseSecretManifests(keyManifest.replace('wrappedKey', 'other')).errors[0]).toMatch(/keys it does not know/);
    const readable = parseSecretManifests(keyManifest.replace('wrappedKey', 'backupKey'));
    expect(readable.errors[0]).toMatch(/readable backup key/);
    expect(readable.secrets).toEqual([]);
  });

  it('refuses a Secret with missing keys, no name, or a name used twice', () => {
    expect(parseSecretManifests(credsManifest.replace('  secret_access_key: "s3cr\\"et"\n', '')).errors[0]).toMatch(/needs both/);
    expect(parseSecretManifests(keyManifest.replace('  name: cos-backup-key\n', '')).errors[0]).toMatch(/no name/);
    expect(parseSecretManifests(`${keyManifest}---\n${keyManifest}`).errors.join(' ')).toMatch(/twice/);
  });
});

describe('buildSecret and apiKeySecret', () => {
  it('makes a Secret marked as made by the console', () => {
    expect(buildSecret({ name: 'cos-backup-key', namespace: 'kube-system', data: { wrappedKey: 'abc' } })).toEqual({
      apiVersion: 'v1',
      kind: 'Secret',
      type: 'Opaque',
      metadata: {
        name: 'cos-backup-key',
        namespace: 'kube-system',
        labels: { 'vpc-file-pool-csi.ibm.io/made-by-console': 'true' },
      },
      stringData: { wrappedKey: 'abc' },
    });
  });

  it('makes the Key Protect API key Secret with the key apikey', () => {
    expect(apiKeySecret('keyprotect-apikey', ' k ').stringData).toEqual({ apikey: 'k' });
  });
});

const block = `spec:
  export:
    backupKeySecret: cos-backup-key
    bucket: pool-backups
    credentialsSecret: cos-credentials
    endpoint: s3.eu-de.cloud-object-storage.appdomain.cloud
    keepFor: 30d
    keyProtect:
      apiKeySecret: keyprotect-apikey
      instanceID: inst-1
      region: eu-de
      rootKeyID: root-1
    lock: Compliance
    prefix: lab
`;

describe('parseExportBlock', () => {
  it('reads the block that cos-setup prints', () => {
    const r = parseExportBlock(block);
    expect(r.errors).toEqual([]);
    expect(r.export).toEqual({
      backupKeySecret: 'cos-backup-key',
      bucket: 'pool-backups',
      credentialsSecret: 'cos-credentials',
      endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
      keepFor: '30d',
      keyProtect: { apiKeySecret: 'keyprotect-apikey', instanceID: 'inst-1', region: 'eu-de', rootKeyID: 'root-1' },
      lock: 'Compliance',
      prefix: 'lab',
    });
  });

  it('reads the block with only the export: key, or with no wrapper, and with indented pasting', () => {
    const inner = block.split('\n').slice(1).join('\n');
    expect(parseExportBlock(inner).export?.bucket).toBe('pool-backups');
    const bare = inner.split('\n').map((l) => l.replace(/^ {4}/, '')).slice(1).join('\n');
    expect(parseExportBlock(bare).export?.bucket).toBe('pool-backups');
    expect(parseExportBlock(block.split('\n').map((l) => `    ${l}`).join('\n')).export?.bucket).toBe('pool-backups');
  });

  it('reads quoted values', () => {
    expect(parseExportBlock(block.replace('keepFor: 30d', 'keepFor: "30d"')).export?.keepFor).toBe('30d');
  });

  it('reports a line it cannot read, and nothing to read', () => {
    expect(parseExportBlock('').errors[0]).toMatch(/Paste/);
    expect(parseExportBlock('spec:\n  export:\n    bucket\n').errors[0]).toMatch(/line 3/);
  });

  it('refuses a key that spec.export does not have', () => {
    expect(parseExportBlock(block + '    extra: 1\n').errors[0]).toMatch(/extra/);
  });
});

describe('validateExport', () => {
  const good = () => parseExportBlock(block).export!;

  it('accepts what cos-setup prints', () => {
    expect(validateExport(good())).toEqual([]);
  });

  it('needs the required fields', () => {
    const ex = good();
    ex.bucket = '';
    ex.keyProtect.rootKeyID = '';
    const msgs = validateExport(ex).join(' ');
    expect(msgs).toMatch(/bucket/);
    expect(msgs).toMatch(/rootKeyID/);
  });

  it('checks the lock and the keep time, with the 25 hour rule for Compliance', () => {
    const ex = good();
    ex.keepFor = '24h';
    expect(validateExport(ex).join(' ')).toMatch(/at least 25h/);
    ex.lock = 'None';
    expect(validateExport(ex)).toEqual([]);
    ex.keepFor = '30m';
    expect(validateExport(ex).join(' ')).toMatch(/at least 1h/);
    ex.keepFor = '30d';
    ex.lock = 'Forever' as never;
    expect(validateExport(ex).join(' ')).toMatch(/lock must be/);
  });

  it('checks the schedule and the names', () => {
    const ex = good();
    ex.schedule = '10s';
    expect(validateExport(ex).join(' ')).toMatch(/schedule/);
    ex.schedule = '6h';
    ex.credentialsSecret = 'Bad Name';
    expect(validateExport(ex).join(' ')).toMatch(/credentialsSecret/);
  });
});

describe('exportPatch', () => {
  it('writes spec.export with a JSON patch', () => {
    const ex = parseExportBlock(block).export!;
    expect(exportPatch(ex)).toEqual([{ op: 'add', path: '/spec/export', value: ex }]);
  });
});

describe('exportHealth', () => {
  const ago = new Date(Date.now() - 3600 * 1000).toISOString();
  const policy = (volumes: unknown[], withExport = true) =>
    ({ metadata: { name: 'p' }, spec: { export: withExport ? {} : undefined }, status: { volumes } }) as unknown as BackupPolicy;

  it('is "off" without an export', () => {
    expect(exportHealth(policy([], false))).toMatchObject({ state: 'off' });
  });

  it('is "waiting" when no volume has been sent yet', () => {
    expect(exportHealth(policy([{ pvcNamespace: 'a', pvcName: 'x' }]))).toMatchObject({ state: 'waiting', sent: 0, total: 1 });
  });

  it('is "working" when every volume has been sent', () => {
    const h = exportHealth(policy([{ exportedTime: ago }, { exportedTime: ago }]));
    expect(h).toMatchObject({ state: 'working', sent: 2, total: 2 });
    expect(h.text).toBe('The export works: 2 of 2 volumes have been sent to COS.');
  });

  it('is "failing" when a volume has an export error, and names how many', () => {
    const h = exportHealth(policy([{ exportedTime: ago }, { exportError: 'denied' }]));
    expect(h).toMatchObject({ state: 'failing', failed: 1, sent: 1 });
    expect(h.text).toBe('The export has a problem: 1 volume failed, 1 of 2 sent. Read the error of the volume below.');
  });
});
