import { ROUTES } from '../constants';
import {
  GUIDE_BASE,
  SUPPORT_URL,
  DIAGNOSTICS_URL,
  TROUBLESHOOTING,
  helpUrlForPath,
} from './help-links';

describe('helpUrlForPath', () => {
  const cases: Array<[string, string]> = [
    [ROUTES.DASHBOARD, 'SUPPORT.md'],
    [ROUTES.POOLS, 'TUTORIAL-01-POOL-CREATION.md'],
    [ROUTES.POOL_CREATE, 'TUTORIAL-01-POOL-CREATION.md'],
    ['/vpc-file-pools/pools/general-purpose', 'TUTORIAL-01-POOL-CREATION.md'],
    [ROUTES.SUBVOLUMES, 'USER-GUIDE.md'],
    ['/vpc-file-pools/subvolumes/pvc-1/clone', 'USER-GUIDE.md'],
    [ROUTES.SNAPSHOTS, 'TUTORIAL-02-SNAPSHOTS-AND-CLONES.md'],
    ['/vpc-file-pools/snapshots/snap-1/restore', 'TUTORIAL-02-SNAPSHOTS-AND-CLONES.md'],
    [ROUTES.REPLICATION, 'TUTORIAL-06-REPLICATION-AND-FAILOVER.md'],
    ['/vpc-file-pools/replication/dr', 'TUTORIAL-06-REPLICATION-AND-FAILOVER.md'],
    [ROUTES.MONITORING, 'MONITORING.md'],
    [ROUTES.MIGRATE, 'TUTORIAL-08-MIGRATION-AND-CONSOLE.md'],
  ];

  it.each(cases)('%s opens %s', (path, file) => {
    expect(helpUrlForPath(path)).toBe(`${GUIDE_BASE}${file}`);
  });

  it('falls back to the support guide for an unknown path', () => {
    expect(helpUrlForPath('/vpc-file-pools/unknown')).toBe(SUPPORT_URL);
  });
});

describe('links', () => {
  it('all point at the repository on GitHub', () => {
    const all = [SUPPORT_URL, DIAGNOSTICS_URL, ...Object.values(TROUBLESHOOTING)];
    all.forEach((u) => expect(u.startsWith(GUIDE_BASE)).toBe(true));
  });

  it('diagnostics link goes to the diagnostics section of the support guide', () => {
    expect(DIAGNOSTICS_URL).toBe(`${GUIDE_BASE}SUPPORT.md#3-collect-diagnostics-for-a-support-case`);
  });
});
