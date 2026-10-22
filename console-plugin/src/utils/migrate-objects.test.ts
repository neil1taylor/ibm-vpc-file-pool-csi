import { readFileSync } from 'fs';
import { join } from 'path';
import { buildMigrationPod, buildTargetPVC, migrationNames } from './migrate-objects';

// The Go side (pkg/migrate/golden_test.go) writes this file from its own builders.
// If either side changes alone, this test or the Go test fails.
const golden = JSON.parse(
  readFileSync(join(__dirname, '../../../pkg/migrate/testdata/migration-objects.golden.json'), 'utf8'),
);

describe('migration objects', () => {
  const { namespace, sourcePVC, pool, storageClass, size } = golden.input;
  const names = migrationNames(sourcePVC);

  it('names the new claim and the pod as the kubectl migrate tool does', () => {
    expect(names).toEqual({ targetPVC: 'data-pooled', pod: 'migrate-data' });
  });

  it('builds the same target claim as the Go builder', () => {
    expect(buildTargetPVC({ namespace, sourcePVC, pool, storageClass, size })).toEqual(golden.targetPVC);
  });

  it('builds the same migration pod as the Go builder', () => {
    expect(buildMigrationPod({ namespace, sourcePVC })).toEqual(golden.migrationPod);
  });
});
