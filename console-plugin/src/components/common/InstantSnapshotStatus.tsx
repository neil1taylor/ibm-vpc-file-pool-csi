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
} from '../../utils/instant-snapshots';

/**
 * Shows whether VM disk snapshots are instant (the KubeVirt Sidecar feature gate is on) or full copies.
 * It only shows the setting: turning the gate on or off is a cluster administrator's step.
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

  const state = instantSnapshotState(kubevirts, kvLoaded, kvError);

  if (state === 'unknown') {
    return (
      <Alert variant="custom" title="Instant VM snapshots: unknown" isInline isPlain>
        The console cannot read the KubeVirt settings. Reading them needs permission to read KubeVirt in the
        OpenShift Virtualization namespace, and OpenShift Virtualization must be installed.
      </Alert>
    );
  }

  if (state === 'off') {
    return (
      <Alert variant="info" title="Instant VM snapshots: off" isInline>
        VM disk snapshots are full copies. A copy takes longer and uses as much space as the disk. To make
        snapshots instant, a cluster administrator turns on the KubeVirt Sidecar feature gate: install the driver
        with the Helm value <code>vmSnapshots.enableSidecarGate=true</code>, or run{' '}
        <code>hack/sidecar-gate.sh on</code> from the driver repository. Red Hat does not support this setting.
      </Alert>
    );
  }

  const disks = disksWithInstantSnapshots(subvolumes, vms);
  const count = `${disks.length} VM disk${disks.length === 1 ? ' has' : 's have'} instant snapshots`;
  return (
    <Alert variant="success" title="Instant VM snapshots: on" isInline>
      <p>
        VM disk snapshots are instant: each snapshot is a new overlay file and nothing is copied (unless the driver
        was installed with <code>vmSnapshots.method=copy</code>). If the KubeVirt Sidecar feature gate is turned
        off, the VMs listed here would not start until their disks are flattened with{' '}
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
