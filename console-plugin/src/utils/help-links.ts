import { ROUTES } from '../constants';

/**
 * Every link from the console to a guide is in this file. If the guides move,
 * change GUIDE_BASE and the paths here; nothing else needs to change.
 * test/chart/test-doc-links.sh checks that each path and heading exists.
 */
export const GUIDE_BASE = 'https://github.com/neil1taylor/ibm-vpc-file-pool-csi/blob/main/docs/';

export const SUPPORT_URL = `${GUIDE_BASE}SUPPORT.md`;
export const DIAGNOSTICS_URL = `${GUIDE_BASE}SUPPORT.md#3-collect-diagnostics-for-a-support-case`;

/** Troubleshooting sections for the banners. */
export const TROUBLESHOOTING = {
  pvc: `${GUIDE_BASE}TROUBLESHOOTING.md#pvc-issues`,
  mount: `${GUIDE_BASE}TROUBLESHOOTING.md#mount-issues`,
  controller: `${GUIDE_BASE}TROUBLESHOOTING.md#controller-issues`,
  metrics: `${GUIDE_BASE}TROUBLESHOOTING.md#metrics-issues`,
  replication: `${GUIDE_BASE}TROUBLESHOOTING.md#replication-is-late-or-failing`,
} as const;

/** The guide for each area of the console, by the start of the path. */
const AREA_GUIDES: Array<[string, string]> = [
  [ROUTES.POOLS, `${GUIDE_BASE}TUTORIAL-01-POOL-CREATION.md`],
  [ROUTES.SUBVOLUMES, `${GUIDE_BASE}USER-GUIDE.md`],
  [ROUTES.SNAPSHOTS, `${GUIDE_BASE}TUTORIAL-02-SNAPSHOTS-AND-CLONES.md`],
  [ROUTES.REPLICATION, `${GUIDE_BASE}TUTORIAL-06-REPLICATION-AND-FAILOVER.md`],
  [ROUTES.BACKUPS, `${GUIDE_BASE}TUTORIAL-10-BACKUP-AND-RESTORE.md`],
  [ROUTES.MONITORING, `${GUIDE_BASE}MONITORING.md`],
  [ROUTES.MIGRATE, `${GUIDE_BASE}TUTORIAL-08-MIGRATION-AND-CONSOLE.md`],
];

/** The guide that matches a console path. An unknown path gets the support guide. */
export const helpUrlForPath = (pathname: string): string => {
  const match = AREA_GUIDES.find(([prefix]) => pathname === prefix || pathname.startsWith(`${prefix}/`));
  return match ? match[1] : SUPPORT_URL;
};
