// The Jobs that measure and remove a volume's backup, and list a COS bucket, are the same ones the Go
// runner makes for `kubectl backup delete` and `kubectl backup cos-list` (pkg/pool/share_job.go,
// share_job_backup.go, share_job_cosrestore.go, share_job_export.go). pkg/pool/share_job_backup_golden_test.go
// writes them to a golden file, and backup-jobs.test.ts compares these builders with that file, so the two
// cannot drift apart. Change one side, and the other test fails until it follows.

export const SHARE_JOB_LABEL = 'storage.ibmcloud.io/share-data-job';
/** The image of the Jobs that run a shell script on a share (pkg/pool DefaultCloneImage). */
export const SHARE_JOB_IMAGE = 'quay.io/centos/centos:stream9';

const SHARE_JOB_NAME_MAX = 56;
const SHARE_JOB_BACKOFF_LIMIT = 3;
const COS_LIST_LIMIT = 3500;
const DRIVER_BINARY = '/usr/local/bin/vpc-file-pool-csi';

export interface ShareMount {
  mountTargetIP: string;
  exportPath: string;
}

export interface KubeObject {
  apiVersion: string;
  kind: string;
  metadata: { name: string; namespace?: string; labels?: Record<string, string> };
  spec: Record<string, unknown>;
}

export interface ShareJobObjects {
  pv: KubeObject;
  pvc: KubeObject;
  job: KubeObject;
}

export interface COSRepo {
  endpoint: string;
  bucket: string;
  /** The area of the policy inside the bucket, with a closing slash (cosAreaPrefix). */
  prefix: string;
  credentialsSecret: string;
  backupKeySecret: string;
  keyProtect: { instanceID: string; region: string; rootKeyID: string; apiKeySecret: string };
  /** Empty when the policy has no emergency key. */
  emergencyKeySecret: string;
}

// ── SHA-256, for the name of a Job with a long key (the Go runner cuts it with a hash) ──

const K = [
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5, 0xd807aa98,
  0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786,
  0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8,
  0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13,
  0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819,
  0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a,
  0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7,
  0xc67178f2,
];

function utf8(text: string): number[] {
  const out: number[] = [];
  for (const ch of text) {
    const c = ch.codePointAt(0)!;
    if (c < 0x80) out.push(c);
    else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 63));
    else if (c < 0x10000) out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
    else out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
  }
  return out;
}

/** The SHA-256 of a string, as lower-case hex. */
export function sha256Hex(text: string): string {
  const bytes = utf8(text);
  const bitLength = bytes.length * 8;
  bytes.push(0x80);
  while (bytes.length % 64 !== 56) bytes.push(0);
  for (let i = 7; i >= 0; i--) bytes.push(i >= 4 ? 0 : (bitLength >>> (i * 8)) & 0xff);
  const h = [0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19];
  const rotr = (x: number, n: number) => (x >>> n) | (x << (32 - n));
  for (let offset = 0; offset < bytes.length; offset += 64) {
    const w: number[] = [];
    for (let i = 0; i < 16; i++) {
      const j = offset + i * 4;
      w[i] = ((bytes[j] << 24) | (bytes[j + 1] << 16) | (bytes[j + 2] << 8) | bytes[j + 3]) >>> 0;
    }
    for (let i = 16; i < 64; i++) {
      const s0 = rotr(w[i - 15], 7) ^ rotr(w[i - 15], 18) ^ (w[i - 15] >>> 3);
      const s1 = rotr(w[i - 2], 17) ^ rotr(w[i - 2], 19) ^ (w[i - 2] >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) >>> 0;
    }
    let [a, b, c, d, e, f, g, hh] = h;
    for (let i = 0; i < 64; i++) {
      const t1 = (hh + (rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25)) + ((e & f) ^ (~e & g)) + K[i] + w[i]) >>> 0;
      const t2 = ((rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22)) + ((a & b) ^ (a & c) ^ (b & c))) >>> 0;
      hh = g;
      g = f;
      f = e;
      e = (d + t1) >>> 0;
      d = c;
      c = b;
      b = a;
      a = (t1 + t2) >>> 0;
    }
    [a, b, c, d, e, f, g, hh].forEach((v, i) => {
      h[i] = (h[i] + v) >>> 0;
    });
  }
  return h.map((v) => v.toString(16).padStart(8, '0')).join('');
}

/** The Job name for an operation key; it fits the Kubernetes name limits with room for the PV and claim suffix. */
export function shareJobName(key: string): string {
  const name = `share-data-${key}`;
  if (name.length <= SHARE_JOB_NAME_MAX) return name;
  return `${name.slice(0, SHARE_JOB_NAME_MAX - 9)}-${sha256Hex(key).slice(0, 8)}`;
}

// ── Paths ──

const POLICY_DIR = '[a-z0-9]([-a-z0-9.]{0,61}[a-z0-9])?';
const BACKUP_PATH = new RegExp(`^/backup/${POLICY_DIR}/pvcs/pvc-[a-f0-9-]{36}$`);
const MOMENTS_PATH = new RegExp(`^/backup/${POLICY_DIR}/\\.moments/pvcs/pvc-[a-f0-9-]{36}$`);
const POLICY_NAME = new RegExp(`^${POLICY_DIR}$`);
const VOLUME_SUBPATH = /^\/pvcs\/pvc-[a-f0-9-]{36}$/;

/** The backup folder and the moments folder of one volume of a policy; subPath is /pvcs/pvc-... from the status. */
export function backupFolders(policy: string, subPath: string): { backup: string; moments: string } {
  if (!POLICY_NAME.test(policy)) throw new Error(`"${policy}" is not a policy name`);
  if (!VOLUME_SUBPATH.test(subPath)) throw new Error(`"${subPath}" is not a volume folder`);
  return { backup: `/backup/${policy}${subPath}`, moments: `/backup/${policy}/.moments${subPath}` };
}

function checkBackupFolders(backupPath: string, momentsPath: string): void {
  if (!BACKUP_PATH.test(backupPath)) throw new Error(`backup path "${backupPath}" does not match expected pattern`);
  if (!MOMENTS_PATH.test(momentsPath)) {
    throw new Error(`backup moments path "${momentsPath}" does not match expected pattern`);
  }
  const [policyDir, volume] = backupPath.slice('/backup/'.length).split('/pvcs/');
  if (momentsPath !== `/backup/${policyDir}/.moments/pvcs/${volume}`) {
    throw new Error(`moments folder "${momentsPath}" is not the one of backup folder "${backupPath}"`);
  }
}

// ── Jobs that mount one share ──

interface ShareJobInput {
  key: string;
  namespace: string;
  share: ShareMount;
  backupPath: string;
  momentsPath: string;
  image?: string;
}

function shareJobObjects(input: ShareJobInput, script: string): ShareJobObjects {
  const jobName = shareJobName(input.key);
  const pvName = `${jobName}-s0`;
  const labels = { [SHARE_JOB_LABEL]: jobName };
  const storage = { storage: '1Gi' };
  return {
    pv: {
      apiVersion: 'v1',
      kind: 'PersistentVolume',
      metadata: { name: pvName, labels },
      spec: {
        capacity: storage,
        accessModes: ['ReadWriteMany'],
        // Retain: deleting this PV must never touch the share.
        persistentVolumeReclaimPolicy: 'Retain',
        mountOptions: ['nfsvers=4.1', 'soft', 'timeo=600', 'retrans=3', 'sec=sys'],
        nfs: { server: input.share.mountTargetIP, path: input.share.exportPath || '/' },
        claimRef: { name: pvName, namespace: input.namespace },
      },
    },
    pvc: {
      apiVersion: 'v1',
      kind: 'PersistentVolumeClaim',
      metadata: { name: pvName, namespace: input.namespace, labels },
      spec: {
        accessModes: ['ReadWriteMany'],
        storageClassName: '',
        volumeName: pvName,
        resources: { requests: storage },
      },
    },
    job: {
      apiVersion: 'batch/v1',
      kind: 'Job',
      metadata: { name: jobName, namespace: input.namespace, labels },
      spec: {
        backoffLimit: SHARE_JOB_BACKOFF_LIMIT,
        template: {
          metadata: { labels },
          spec: {
            automountServiceAccountToken: true,
            restartPolicy: 'OnFailure',
            securityContext: { runAsUser: 0 },
            containers: [
              {
                name: 'share-data',
                image: input.image || SHARE_JOB_IMAGE,
                command: ['/bin/sh', '-c', script],
                resources: {
                  requests: { cpu: '100m', memory: '128Mi' },
                  limits: { cpu: '1', memory: '512Mi' },
                },
                volumeMounts: [{ name: 'share0', mountPath: '/share0' }],
              },
            ],
            volumes: [{ name: 'share0', persistentVolumeClaim: { claimName: pvName } }],
          },
        },
      },
    },
  };
}

/** The objects of the Job that reports how many bytes the backup of one volume uses. It changes nothing. */
export function buildMeasureBackupObjects(input: ShareJobInput): ShareJobObjects {
  checkBackupFolders(input.backupPath, input.momentsPath);
  // One du run over both folders: each moment of a plain volume is a folder of hard links to the backup
  // folder's files, and a single run counts each file once.
  const script = `set -eu
set --
for d in "/share0${input.backupPath}" "/share0${input.momentsPath}"; do
  if [ -d "$d" ]; then
    set -- "$@" "$d"
  fi
done
total=0
if [ "$#" -gt 0 ]; then
  for kb in $(du -sk "$@" | cut -f1); do
    total=$((total + kb))
  done
fi
echo "$total" > /dev/termination-log
`;
  return shareJobObjects(input, script);
}

/** The objects of the Job that removes the backup folder and the moments folder of one volume, for good. */
export function buildRemoveBackupObjects(input: ShareJobInput): ShareJobObjects {
  checkBackupFolders(input.backupPath, input.momentsPath);
  const script = `set -eu\nrm -r -f -- "/share0${input.backupPath}"\nrm -r -f -- "/share0${input.momentsPath}"\n`;
  return shareJobObjects(input, script);
}

// ── The Job that lists a COS bucket ──

const ENDPOINT = /^(https?:\/\/)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$/;
const BUCKET = /^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/;
const PREFIX = /^[A-Za-z0-9._/-]*$/;
const REGION = /^[a-z0-9]+(-[a-z0-9]+)*$/;
const KEY_ID = /^[A-Za-z0-9][A-Za-z0-9-]*$/;
const SECRET_NAME = /^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$/;
const COS_VOLUME = /^pvc-[a-f0-9-]{36}$/;

const shellQuote = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`;

/** The folder of one policy inside the bucket: the prefix of the export block, then the policy name. */
export function cosAreaPrefix(prefix: string, policy: string): string {
  const trimmed = prefix.replace(/^\/+|\/+$/g, '');
  return trimmed === '' ? `${policy}/` : `${trimmed}/${policy}/`;
}

function validateRepo(repo: COSRepo): void {
  if (!ENDPOINT.test(repo.endpoint)) {
    throw new Error(`export endpoint "${repo.endpoint}" is not a host name with an optional port`);
  }
  if (!BUCKET.test(repo.bucket)) throw new Error(`export bucket "${repo.bucket}" is not a valid bucket name`);
  if (!PREFIX.test(repo.prefix) || repo.prefix.includes('..')) {
    throw new Error(`export prefix "${repo.prefix}" has characters that are not allowed`);
  }
  const secrets: Array<[string, string]> = [
    ['credentials', repo.credentialsSecret],
    ['backup key', repo.backupKeySecret],
    ['Key Protect API key', repo.keyProtect.apiKeySecret],
  ];
  if (repo.emergencyKeySecret) secrets.push(['emergency key', repo.emergencyKeySecret]);
  for (const [what, name] of secrets) {
    if (!SECRET_NAME.test(name)) throw new Error(`the ${what} Secret name "${name}" is not a valid name`);
  }
  if (!REGION.test(repo.keyProtect.region)) {
    throw new Error(`the Key Protect region "${repo.keyProtect.region}" is not a region name such as eu-de`);
  }
  if (!KEY_ID.test(repo.keyProtect.instanceID) || !KEY_ID.test(repo.keyProtect.rootKeyID)) {
    throw new Error('the Key Protect instance ID and root key ID are required and must be IDs');
  }
}

// kopia snapshot list as lines of "<id> <path> <moment> <volume>" (pkg/pool snapshotLines).
const SNAPSHOT_LINES = String.raw`kopia snapshot list --all --json | sed -n 's/^ *{"id":"\([^"]*\)".*"path":"\([^"]*\)".*"tag:moment":"\([^"]*\)","tag:volume":"\([^"]*\)".*/\1 \2 \3 \4/p'`;

// The listing as JSON, cut so that it fits a termination message (pkg/pool listAwk).
const LIST_AWK = String.raw`$2 ~ /\/\.moments\// { next }
vol != "" && $4 != vol { next }
{
  if (!($4 in seen)) { seen[$4] = 1; order[++n] = $4 }
  if (!(($4, $3) in has)) { has[$4, $3] = 1; ms[$4] = ms[$4] " " $3 }
}
END {
  out = "{\"volumes\":["; trunc = "false"; first = 1
  for (i = 1; i <= n && trunc == "false"; i++) {
    v = order[i]; k = split(ms[v], arr, " ")
    item = "{\"volume\":\"" v "\",\"moments\":["
    for (j = 1; j <= k; j++) {
      piece = (j > 1 ? "," : "") "\"" arr[j] "\""
      if (length(out) + length(item) + length(piece) + 40 > limit) { trunc = "true"; break }
      item = item piece
    }
    out = out (first ? "" : ",") item "]}"; first = 0
  }
  print out "],\"truncated\":" trunc "}"
}`;

function cosListScript(repo: COSRepo, volume: string): string {
  const endpoint = repo.endpoint.replace(/^https:\/\//, '').replace(/^http:\/\//, '');
  const backend = ['s3', `--bucket=${shellQuote(repo.bucket)}`, `--endpoint=${shellQuote(endpoint)}`];
  if (repo.endpoint.startsWith('http://')) backend.push('--disable-tls');
  if (repo.prefix) backend.push(`--prefix=${shellQuote(repo.prefix)}`);

  const unwrap = [
    DRIVER_BINARY,
    '--mode=unwrap-backup-key',
    `--kp-region=${shellQuote(repo.keyProtect.region)}`,
    `--kp-instance=${shellQuote(repo.keyProtect.instanceID)}`,
    `--kp-root-key=${shellQuote(repo.keyProtect.rootKeyID)}`,
    `--wrapped-key-file=${shellQuote('/var/run/pool-csi/backup-key-wrapped/wrappedKey')}`,
    `--api-key-file=${shellQuote('/var/run/pool-csi/keyprotect-apikey/apikey')}`,
  ];
  if (repo.emergencyKeySecret) {
    unwrap.push(`--emergency-key-file=${shellQuote('/var/run/pool-csi/backup-key-emergency/backupKey')}`);
  }

  return `set -eu
state='/tmp/kopia'
mkdir -p "$state"
export HOME="$state"
export KOPIA_CONFIG_PATH="$state/repository.config"
export KOPIA_CACHE_DIRECTORY="$state/cache"
export KOPIA_LOG_DIR="$state/logs"
export KOPIA_CHECK_FOR_UPDATES=false
keyfile='/run/backup-key/key'
${unwrap.join(' ')} --key-out="$keyfile"
KOPIA_PASSWORD=$(cat "$keyfile")
export KOPIA_PASSWORD
rm -f "$keyfile"
ident="--no-persist-credentials --override-username=driver --override-hostname=pool-csi"
kopia repository connect ${backend.join(' ')} $ident --readonly
${SNAPSHOT_LINES} | awk -v vol=${shellQuote(volume)} -v limit=${COS_LIST_LIMIT} ${shellQuote(LIST_AWK)} > ${shellQuote('/dev/termination-log')}
`;
}

const secretFileVolume = (name: string, secret: string, key: string) => ({
  name,
  secret: { secretName: secret, items: [{ key, path: key }] },
});

/**
 * The read-only Job that lists the volumes and the export moments in a policy's area of the bucket. It runs the
 * driver image, which holds Kopia. The Job holds only references to Secrets; the keys are never in its spec.
 * volume, if set, limits the list to one volume.
 */
export function buildCOSListJob(input: {
  key: string;
  namespace: string;
  driverImage: string;
  repo: COSRepo;
  volume: string;
}): KubeObject {
  if (input.volume && !COS_VOLUME.test(input.volume)) throw new Error(`volume "${input.volume}" is not a volume name`);
  validateRepo(input.repo);
  if (!input.driverImage) throw new Error('no driver image set for COS list Jobs');

  const { repo } = input;
  const jobName = shareJobName(input.key);
  const labels = { [SHARE_JOB_LABEL]: jobName };
  const secretRef = (name: string, key: string) => ({
    name,
    valueFrom: { secretKeyRef: { name: repo.credentialsSecret, key } },
  });

  const volumes = [
    secretFileVolume('backup-key-wrapped', repo.backupKeySecret, 'wrappedKey'),
    secretFileVolume('keyprotect-apikey', repo.keyProtect.apiKeySecret, 'apikey'),
  ];
  const mounts = [
    { name: 'backup-key-wrapped', mountPath: '/var/run/pool-csi/backup-key-wrapped', readOnly: true },
    { name: 'keyprotect-apikey', mountPath: '/var/run/pool-csi/keyprotect-apikey', readOnly: true },
  ];
  if (repo.emergencyKeySecret) {
    volumes.push(secretFileVolume('backup-key-emergency', repo.emergencyKeySecret, 'backupKey'));
    mounts.push({ name: 'backup-key-emergency', mountPath: '/var/run/pool-csi/backup-key-emergency', readOnly: true });
  }

  return {
    apiVersion: 'batch/v1',
    kind: 'Job',
    metadata: { name: jobName, namespace: input.namespace, labels },
    spec: {
      backoffLimit: SHARE_JOB_BACKOFF_LIMIT,
      template: {
        metadata: { labels },
        spec: {
          automountServiceAccountToken: true,
          restartPolicy: 'OnFailure',
          securityContext: { runAsUser: 0 },
          containers: [
            {
              name: 'share-data',
              image: input.driverImage,
              command: ['/bin/sh', '-c', cosListScript(repo, input.volume)],
              env: [
                secretRef('AWS_ACCESS_KEY_ID', 'access_key_id'),
                secretRef('AWS_SECRET_ACCESS_KEY', 'secret_access_key'),
              ],
              resources: {
                requests: { cpu: '200m', memory: '512Mi' },
                limits: { cpu: '2', memory: '2Gi' },
              },
              volumeMounts: [...mounts, { name: 'backup-key-memory', mountPath: '/run/backup-key' }],
            },
          ],
          volumes: [...volumes, { name: 'backup-key-memory', emptyDir: { medium: 'Memory', sizeLimit: '1Mi' } }],
        },
      },
    },
  };
}
