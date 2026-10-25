import React, { Suspense, useMemo } from 'react';
import { Route, Switch, useHistory, useLocation } from 'react-router-dom';
import {
  PageSection,
  Title,
  Tabs,
  Tab,
  TabTitleText,
  Spinner,
  Bullseye,
  Split,
  SplitItem,
} from '@patternfly/react-core';
import { ROUTES } from '../constants';
import { helpUrlForPath } from '../utils/help-links';
import HelpLink from './common/HelpLink';

// Lazy-load all sub-pages for webpack code-splitting
const DashboardPage = React.lazy(
  () => import('./dashboard/DashboardPage'),
);
const FileSharePoolListPage = React.lazy(
  () => import('./filesharepool/FileSharePoolListPage'),
);
const FileSharePoolDetailsPage = React.lazy(
  () => import('./filesharepool/FileSharePoolDetailsPage'),
);
const FileSharePoolCreatePage = React.lazy(
  () => import('./filesharepool/FileSharePoolCreatePage'),
);
const SubVolumeListPage = React.lazy(
  () => import('./subvolume/SubVolumeListPage'),
);
const SubVolumeDetailsPage = React.lazy(
  () => import('./subvolume/SubVolumeDetailsPage'),
);
const SubVolumeCreatePage = React.lazy(
  () => import('./subvolume/SubVolumeCreatePage'),
);
const SnapshotListPage = React.lazy(
  () => import('./snapshot/SnapshotListPage'),
);
const SnapshotDetailsPage = React.lazy(
  () => import('./snapshot/SnapshotDetailsPage'),
);
const MigrateVolumesPage = React.lazy(
  () => import('./migrate/MigrateVolumesPage'),
);
const CloneVolumePage = React.lazy(
  () => import('./subvolume/CloneVolumePage'),
);
const RestoreSnapshotPage = React.lazy(
  () => import('./snapshot/RestoreSnapshotPage'),
);
const SnapshotCreatePage = React.lazy(
  () => import('./snapshot/SnapshotCreatePage'),
);
const ReplicationPolicyListPage = React.lazy(
  () => import('./replicationpolicy/ReplicationPolicyListPage'),
);
const ReplicationPolicyDetailsPage = React.lazy(
  () => import('./replicationpolicy/ReplicationPolicyDetailsPage'),
);
const ReplicationPolicyCreatePage = React.lazy(
  () => import('./replicationpolicy/ReplicationPolicyCreatePage'),
);
const BackupListPage = React.lazy(
  () => import('./backup/BackupListPage'),
);
const BackupDetailsPage = React.lazy(
  () => import('./backup/BackupDetailsPage'),
);
const BackupPolicyFormPage = React.lazy(
  () => import('./backup/BackupPolicyFormPage'),
);
const BackupCOSRestorePage = React.lazy(
  () => import('./backup/BackupCOSRestorePage'),
);
const BackupRestorePage = React.lazy(
  () => import('./backup/BackupRestorePage'),
);
const MonitoringPage = React.lazy(
  () => import('./monitoring/MonitoringPage'),
);
const PoolBenchListPage = React.lazy(
  () => import('./poolbench/PoolBenchListPage'),
);
const PoolBenchRunPage = React.lazy(
  () => import('./poolbench/PoolBenchRunPage'),
);
const PoolBenchCreatePage = React.lazy(
  () => import('./poolbench/PoolBenchCreatePage'),
);

/** Paths that render inside the tabbed layout. */
const TAB_PATHS = [
  ROUTES.DASHBOARD,
  ROUTES.POOLS,
  ROUTES.SUBVOLUMES,
  ROUTES.SNAPSHOTS,
  ROUTES.REPLICATION,
  ROUTES.BACKUPS,
  ROUTES.MONITORING,
  ROUTES.POOLBENCH,
] as const;

type TabKey = (typeof TAB_PATHS)[number];

const TAB_LABELS: Record<TabKey, string> = {
  [ROUTES.DASHBOARD]: 'Overview',
  [ROUTES.POOLS]: 'Pools',
  [ROUTES.SUBVOLUMES]: 'SubVolumes',
  [ROUTES.SNAPSHOTS]: 'Snapshots',
  [ROUTES.REPLICATION]: 'Replication',
  [ROUTES.BACKUPS]: 'Backups',
  [ROUTES.MONITORING]: 'Monitoring',
  [ROUTES.POOLBENCH]: 'PoolBench',
};

const Loading: React.FC = () => (
  <Bullseye>
    <Spinner size="xl" />
  </Bullseye>
);

const VPCFilePoolsPage: React.FC = () => {
  const history = useHistory();
  const location = useLocation();

  const isTabPath = useMemo(
    () => TAB_PATHS.includes(location.pathname as TabKey),
    [location.pathname],
  );

  const activeTab = useMemo<TabKey>(() => {
    // Find the matching tab path (exact match)
    const match = TAB_PATHS.find((p) => p === location.pathname);
    return match || ROUTES.DASHBOARD;
  }, [location.pathname]);

  // Detail/create pages render standalone (no tabs)
  if (!isTabPath) {
    return (
      <>
      <PageSection style={{ paddingBottom: 0, textAlign: 'right' }}>
        <HelpLink href={helpUrlForPath(location.pathname)} />
      </PageSection>
      <Suspense fallback={<Loading />}>
        <Switch>
          {/* Create pages (must come before detail :name routes) */}
          <Route exact path={ROUTES.POOL_CREATE} component={FileSharePoolCreatePage} />
          <Route exact path={ROUTES.SUBVOLUME_CREATE} component={SubVolumeCreatePage} />
          <Route exact path={ROUTES.SNAPSHOT_CREATE} component={SnapshotCreatePage} />
          <Route exact path={ROUTES.REPLICATION_CREATE} component={ReplicationPolicyCreatePage} />
          <Route exact path={ROUTES.SNAPSHOT_RESTORE} component={RestoreSnapshotPage} />
          <Route exact path={ROUTES.MIGRATE} component={MigrateVolumesPage} />
          <Route exact path={ROUTES.SUBVOLUME_CLONE} component={CloneVolumePage} />
          {/* Detail pages */}
          <Route exact path={ROUTES.POOL_DETAIL} component={FileSharePoolDetailsPage} />
          <Route exact path={ROUTES.SUBVOLUME_DETAIL} component={SubVolumeDetailsPage} />
          <Route exact path={ROUTES.SNAPSHOT_DETAIL} component={SnapshotDetailsPage} />
          <Route exact path={ROUTES.REPLICATION_DETAIL} component={ReplicationPolicyDetailsPage} />
          <Route exact path={ROUTES.BACKUP_COS_RESTORE} component={BackupCOSRestorePage} />
          <Route exact path={ROUTES.BACKUP_CREATE} component={BackupPolicyFormPage} />
          <Route exact path={ROUTES.BACKUP_EDIT} component={BackupPolicyFormPage} />
          <Route exact path={ROUTES.BACKUP_RESTORE} component={BackupRestorePage} />
          <Route exact path={ROUTES.BACKUP_DETAIL} component={BackupDetailsPage} />
          <Route exact path={ROUTES.POOLBENCH_CREATE} component={PoolBenchCreatePage} />
          <Route exact path={ROUTES.POOLBENCH_DETAIL} component={PoolBenchRunPage} />
        </Switch>
      </Suspense>
      </>
    );
  }

  // Tab layout
  return (
    <>
      <PageSection style={{ paddingBottom: 0 }}>
        <Split style={{ marginBottom: 16 }}>
          <SplitItem isFilled>
            <Title headingLevel="h1">IBM VPC File Pools</Title>
          </SplitItem>
          <SplitItem>
            <HelpLink href={helpUrlForPath(location.pathname)} />
          </SplitItem>
        </Split>
        <Tabs
          activeKey={activeTab}
          onSelect={(_e, key) => history.push(key as string)}
          mountOnEnter
          unmountOnExit={false}
        >
          {TAB_PATHS.map((path) => (
            <Tab
              key={path}
              eventKey={path}
              title={<TabTitleText>{TAB_LABELS[path]}</TabTitleText>}
            />
          ))}
        </Tabs>
      </PageSection>

      <Suspense fallback={<Loading />}>
        <Switch>
          <Route exact path={ROUTES.POOLS} component={FileSharePoolListPage} />
          <Route exact path={ROUTES.SUBVOLUMES} component={SubVolumeListPage} />
          <Route exact path={ROUTES.SNAPSHOTS} component={SnapshotListPage} />
          <Route exact path={ROUTES.REPLICATION} component={ReplicationPolicyListPage} />
          <Route exact path={ROUTES.BACKUPS} component={BackupListPage} />
          <Route exact path={ROUTES.MONITORING} component={MonitoringPage} />
          <Route exact path={ROUTES.POOLBENCH} component={PoolBenchListPage} />
          <Route path={ROUTES.DASHBOARD} component={DashboardPage} />
        </Switch>
      </Suspense>
    </>
  );
};

export default VPCFilePoolsPage;
