import { readFileSync } from 'fs';
import { join } from 'path';
import {
  backupFolders,
  buildCOSListJob,
  buildMeasureBackupObjects,
  buildRemoveBackupObjects,
  cosAreaPrefix,
  sha256Hex,
  shareJobName,
} from './backup-jobs';

// The Go side (pkg/pool/share_job_backup_golden_test.go) writes this file from its own Job builders.
// If either side changes alone, this test or the Go test fails.
const golden = JSON.parse(
  readFileSync(join(__dirname, '../../../pkg/pool/testdata/backup-jobs.golden.json'), 'utf8'),
);
const input = golden.input;

describe('sha256Hex', () => {
  it('matches the known test vectors', () => {
    expect(sha256Hex('abc')).toBe('ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad');
    expect(sha256Hex('')).toBe('e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855');
    expect(sha256Hex('a'.repeat(100))).toBe('2816597888e4a0d3a36b82b83316ab32680eb8f00f8cd3b904d681246d285a0e');
  });
});

describe('shareJobName', () => {
  it('keeps a short key, and cuts a long one with a hash, as the Go runner does', () => {
    expect(shareJobName('rm-pvc-1')).toBe('share-data-rm-pvc-1');
    expect(shareJobName(input.measureKey)).toBe(golden.measure.job.metadata.name);
    expect(shareJobName(input.deleteKey)).toBe(golden.remove.job.metadata.name);
    expect(shareJobName(input.cosListKey)).toBe(golden.cosList.job.metadata.name);
  });
});

describe('backupFolders', () => {
  it('is the backup folder and the moments folder of a volume of a policy', () => {
    expect(backupFolders('nightly', '/pvcs/pvc-11111111-2222-3333-4444-555555555555')).toEqual({
      backup: input.backupPath,
      moments: input.momentsPath,
    });
  });

  it('refuses a path that is not the folder of a volume', () => {
    expect(() => backupFolders('nightly', '/pvcs/../etc')).toThrow(/not a volume folder/);
    expect(() => backupFolders('Night/ly', '/pvcs/pvc-11111111-2222-3333-4444-555555555555')).toThrow(/policy name/);
  });
});

describe('cosAreaPrefix', () => {
  it('is the prefix of the export block and the name of the policy', () => {
    expect(cosAreaPrefix('', 'nightly')).toBe('nightly/');
    expect(cosAreaPrefix('team/backups/', 'nightly')).toBe('team/backups/nightly/');
    expect(cosAreaPrefix('/team', 'nightly')).toBe('team/nightly/');
  });
});

describe('the measure Job', () => {
  const objects = buildMeasureBackupObjects({
    key: input.measureKey,
    namespace: input.namespace,
    share: input.share,
    backupPath: input.backupPath,
    momentsPath: input.momentsPath,
  });

  it('builds the same PV as the Go runner', () => {
    expect(objects.pv).toEqual(golden.measure.pv);
  });

  it('builds the same claim as the Go runner', () => {
    expect(objects.pvc).toEqual(golden.measure.pvc);
  });

  it('builds the same Job as the Go runner', () => {
    expect(objects.job).toEqual(golden.measure.job);
  });
});

describe('the delete Job', () => {
  const objects = buildRemoveBackupObjects({
    key: input.deleteKey,
    namespace: input.namespace,
    share: input.share,
    backupPath: input.backupPath,
    momentsPath: input.momentsPath,
  });

  it('builds the same PV, claim and Job as the Go runner', () => {
    expect(objects.pv).toEqual(golden.remove.pv);
    expect(objects.pvc).toEqual(golden.remove.pvc);
    expect(objects.job).toEqual(golden.remove.job);
  });

  it('refuses folders that are not the backup of one volume', () => {
    expect(() =>
      buildRemoveBackupObjects({
        key: 'k',
        namespace: 'kube-system',
        share: input.share,
        backupPath: '/backup/nightly/pvcs/../../etc',
        momentsPath: input.momentsPath,
      }),
    ).toThrow();
    expect(() =>
      buildRemoveBackupObjects({
        key: 'k',
        namespace: 'kube-system',
        share: input.share,
        backupPath: input.backupPath,
        momentsPath: '/backup/other/.moments/pvcs/pvc-11111111-2222-3333-4444-555555555555',
      }),
    ).toThrow(/not the one of/);
  });
});

describe('the COS list Job', () => {
  it('builds the same Job as the Go runner, for a volume', () => {
    const job = buildCOSListJob({
      key: input.cosListKey,
      namespace: input.namespace,
      driverImage: input.driverImage,
      repo: input.cosRepo,
      volume: input.cosVolume,
    });
    expect(job).toEqual(golden.cosList.job);
  });

  it('builds the same Job as the Go runner, with an emergency key, an http endpoint and no volume', () => {
    const job = buildCOSListJob({
      key: input.cosListKey,
      namespace: input.namespace,
      driverImage: input.driverImage,
      repo: input.cosRepoVariant,
      volume: '',
    });
    expect(job).toEqual(golden.cosListVariant.job);
  });

  it('refuses a volume that is not a volume name', () => {
    expect(() =>
      buildCOSListJob({ key: 'k', namespace: 'kube-system', driverImage: 'i', repo: input.cosRepo, volume: 'x; ls' }),
    ).toThrow(/not a volume name/);
  });

  it('refuses a repository with a bad endpoint, bucket or prefix', () => {
    const base = { key: 'k', namespace: 'kube-system', driverImage: 'i', volume: '' };
    expect(() => buildCOSListJob({ ...base, repo: { ...input.cosRepo, endpoint: 'bad host;' } })).toThrow(/endpoint/);
    expect(() => buildCOSListJob({ ...base, repo: { ...input.cosRepo, bucket: 'Bad_Bucket' } })).toThrow(/bucket/);
    expect(() => buildCOSListJob({ ...base, repo: { ...input.cosRepo, prefix: '../x' } })).toThrow(/prefix/);
  });

  it('refuses without a driver image', () => {
    expect(() =>
      buildCOSListJob({ key: 'k', namespace: 'kube-system', driverImage: '', repo: input.cosRepo, volume: '' }),
    ).toThrow(/driver image/);
  });
});
