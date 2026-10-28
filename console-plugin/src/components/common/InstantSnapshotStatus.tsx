import React, { useState } from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Alert, ExpandableSection, List, ListItem } from '@patternfly/react-core';
import { SubVolumeModel, VirtualMachineModel } from '../../models';
import { SubVolume } from '../../types';
import { VirtualMachineLike } from '../../utils/k8s-objects';
import {
  disksWithInstantSnapshots,
  instantSnapshotState,
  KubeVirtLike,
  SETTINGS_CONFIGMAP,
  SettingsConfigMapLike,
  snapshotMode,
  vmSnapshotMethod,
} from '../../utils/instant-snapshots';

/**
 * Shows how the driver takes VM disk snapshots: instant, or full copies. Instant needs two things: the driver
 * set to auto (read from the settings ConfigMap the Helm chart renders) and the KubeVirt Sidecar feature gate on.
 * It only shows the mode: turning the gate on or off is a cluster administrator's step.
 */
const InstantSnapshotStatus: React.FC = () => {
  const [expanded, setExpanded] = useState(false);
  const [kubevirts, kvLoaded, kvError] = useK8sWatchResource<KubeVirtLike[]>({
    groupVersionKind: { group: 'kubevirt.io', version: 'v1', kind: 'KubeVirt' },
    isList: true,
  });
  const [subvolumes] = useK8sWatchResource<SubVolume[]>({
    groupVersionKind: { group: SubVolumeModel.apiGroup, version: SubVolumeModel.apiVersion, kind: SubVolumeModel.kind },
    isList: true,
  });
  const [vms] = useK8sWatchResource<VirtualMachineLike[]>({
    groupVersionKind: {
      group: VirtualMachineModel.apiGroup,
      version: VirtualMachineModel.apiVersion,
      kind: VirtualMachineModel.kind,
    },
    isList: true,
  });

  const [settings] = useK8sWatchResource<SettingsConfigMapLike>({
    groupVersionKind: { version: 'v1', kind: 'ConfigMap' },
    name: SETTINGS_CONFIGMAP.name,
    namespace: SETTINGS_CONFIGMAP.namespace,
  });

  const gate = instantSnapshotState(kubevirts, kvLoaded, kvError);
  const mode = snapshotMode(vmSnapshotMethod(settings), gate);

  if (mode === 'unknown-setting') {
    return (
      <Alert variant="custom" title="VM snapshots: unknown" isInline isPlain>
        The driver does not report its setting. A driver older than this console, or one the console cannot read the
        settings of, does not publish it. The Helm value <code>vmSnapshots.method</code> says how it takes VM disk
        snapshots.
      </Alert>
    );
  }

  if (mode === 'unknown') {
    return (
      <Alert variant="custom" title="VM snapshots: unknown" isInline isPlain>
        The console cannot read the KubeVirt settings. Reading them needs permission to read KubeVirt in the
        OpenShift Virtualization namespace, and OpenShift Virtualization must be installed.
      </Alert>
    );
  }

  if (mode === 'copy-setting') {
    return (
      <Alert variant="info" title="VM snapshots: copy" isInline>
        The driver is set to copy (the Helm value <code>vmSnapshots.method=copy</code>). Each VM disk snapshot copies
        the disk: it takes longer and uses as much space as the disk. Nothing else on the cluster is changed.
      </Alert>
    );
  }

  if (mode === 'copy-gate') {
    return (
      <Alert variant="info" title="VM snapshots: copy" isInline>
        The driver would take instant snapshots, but the KubeVirt Sidecar feature gate is off, so each VM disk
        snapshot copies the disk. A copy takes longer and uses as much space as the disk. To make snapshots instant,
        a cluster administrator turns on the gate: install the driver with the Helm value{' '}
        <code>vmSnapshots.enableSidecarGate=true</code>, or run <code>hack/sidecar-gate.sh on</code> from the driver
        repository. Red Hat does not support this setting.
      </Alert>
    );
  }

  const disks = disksWithInstantSnapshots(subvolumes, vms);
  const count = `${disks.length} VM disk${disks.length === 1 ? ' has' : 's have'} instant snapshots`;
  return (
    <Alert variant="success" title="VM snapshots: instant" isInline>
      <p>
        VM disk snapshots are instant: each snapshot is a new overlay file and nothing is copied. The driver is set to
        auto and the KubeVirt Sidecar feature gate is on. If the gate is turned off, the VMs listed here would not
        start until their disks are flattened with{' '}
        <code>kubectl pool-snapshots flatten &lt;vm&gt; -n &lt;namespace&gt;</code>.
      </p>
      {disks.length === 0 ? (
        <p>No VM disk has instant snapshots now.</p>
      ) : (
        <ExpandableSection toggleText={count} isExpanded={expanded} onToggle={(_e, v) => setExpanded(v)}>
          <List>
            {disks.map((d) => (
              <ListItem key={`${d.namespace}/${d.claim}`}>
                {d.vm ? `${d.namespace}/${d.vm}` : `${d.namespace} (no VM found)`}: disk {d.claim},{' '}
                {d.snapshots} snapshot{d.snapshots === 1 ? '' : 's'}
              </ListItem>
            ))}
          </List>
        </ExpandableSection>
      )}
    </Alert>
  );
};

export default InstantSnapshotStatus;
