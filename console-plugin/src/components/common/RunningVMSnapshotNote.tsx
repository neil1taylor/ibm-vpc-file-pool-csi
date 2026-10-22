import React from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Alert } from '@patternfly/react-core';
import { instantSnapshotState, KubeVirtLike } from '../../utils/instant-snapshots';

export const RUNNING_VM_SNAPSHOT_NOTE =
  'To snapshot a running VM, the driver works directly with QEMU, which runs the VM, not through OpenShift. Red Hat does not support this. While this has been tested, an OpenShift update could stop snapshots until the driver is updated.';
export const INSTANT_SNAPSHOT_NOTE = 'Instant snapshots also need a KubeVirt setting that Red Hat does not support.';

/** Says that snapshots of running VMs are not supported by Red Hat, and why instant ones add to that. */
const RunningVMSnapshotNote: React.FC = () => {
  const [kubevirts, loaded, error] = useK8sWatchResource<KubeVirtLike[]>({
    groupVersionKind: { group: 'kubevirt.io', version: 'v1', kind: 'KubeVirt' },
    isList: true,
  });
  const instantOn = instantSnapshotState(kubevirts, loaded, error) === 'on';
  return (
    <Alert variant="warning" title="Not supported by Red Hat" isInline style={{ marginTop: 16 }}>
      <p>{RUNNING_VM_SNAPSHOT_NOTE}</p>
      {instantOn && <p>{INSTANT_SNAPSHOT_NOTE}</p>}
    </Alert>
  );
};

export default RunningVMSnapshotNote;
