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
} from '@patternfly/react-core';
import { ROUTES } from '../constants';

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
const SnapshotCreatePage = React.lazy(
  () => import('./snapshot/SnapshotCreatePage'),
);
const VolumeGroupSnapshotListPage = React.lazy(
  () => import('./volumegroupsnapshot/VolumeGroupSnapshotListPage'),
);
const VolumeGroupSnapshotDetailsPage = React.lazy(
  () => import('./volumegroupsnapshot/VolumeGroupSnapshotDetailsPage'),
);
const VolumeGroupSnapshotCreatePage = React.lazy(
  () => import('./volumegroupsnapshot/VolumeGroupSnapshotCreatePage'),
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
const MonitoringPage = React.lazy(
  () => import('./monitoring/MonitoringPage'),
);

/** Paths that render inside the tabbed layout. */
const TAB_PATHS = [
  ROUTES.DASHBOARD,
  ROUTES.POOLS,
  ROUTES.SUBVOLUMES,
  ROUTES.SNAPSHOTS,
  ROUTES.GROUP_SNAPSHOTS,
  ROUTES.REPLICATION,
  ROUTES.MONITORING,
] as const;

type TabKey = (typeof TAB_PATHS)[number];

const TAB_LABELS: Record<TabKey, string> = {
  [ROUTES.DASHBOARD]: 'Overview',
  [ROUTES.POOLS]: 'Pools',
  [ROUTES.SUBVOLUMES]: 'SubVolumes',
  [ROUTES.SNAPSHOTS]: 'Snapshots',
  [ROUTES.GROUP_SNAPSHOTS]: 'Group Snapshots',
  [ROUTES.REPLICATION]: 'Replication',
  [ROUTES.MONITORING]: 'Monitoring',
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
      <Suspense fallback={<Loading />}>
        <Switch>
          {/* Create pages (must come before detail :name routes) */}
          <Route exact path={ROUTES.POOL_CREATE} component={FileSharePoolCreatePage} />
          <Route exact path={ROUTES.SUBVOLUME_CREATE} component={SubVolumeCreatePage} />
          <Route exact path={ROUTES.SNAPSHOT_CREATE} component={SnapshotCreatePage} />
          <Route exact path={ROUTES.GROUP_SNAPSHOT_CREATE} component={VolumeGroupSnapshotCreatePage} />
          <Route exact path={ROUTES.REPLICATION_CREATE} component={ReplicationPolicyCreatePage} />
          {/* Detail pages */}
          <Route exact path={ROUTES.POOL_DETAIL} component={FileSharePoolDetailsPage} />
          <Route exact path={ROUTES.SUBVOLUME_DETAIL} component={SubVolumeDetailsPage} />
          <Route exact path={ROUTES.SNAPSHOT_DETAIL} component={SnapshotDetailsPage} />
          <Route exact path={ROUTES.GROUP_SNAPSHOT_DETAIL} component={VolumeGroupSnapshotDetailsPage} />
          <Route exact path={ROUTES.REPLICATION_DETAIL} component={ReplicationPolicyDetailsPage} />
        </Switch>
      </Suspense>
    );
  }

  // Tab layout
  return (
    <>
      <PageSection style={{ paddingBottom: 0 }}>
        <Title headingLevel="h1" style={{ marginBottom: 16 }}>
          IBM VPC File Pools
        </Title>
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
          <Route exact path={ROUTES.GROUP_SNAPSHOTS} component={VolumeGroupSnapshotListPage} />
          <Route exact path={ROUTES.REPLICATION} component={ReplicationPolicyListPage} />
          <Route exact path={ROUTES.MONITORING} component={MonitoringPage} />
          <Route path={ROUTES.DASHBOARD} component={DashboardPage} />
        </Switch>
      </Suspense>
    </>
  );
};

export default VPCFilePoolsPage;
