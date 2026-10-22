import React from 'react';
import { Alert } from '@patternfly/react-core';

export const RUNNING_VM_COPY_NOTE =
  'To copy a running VM, the driver works directly with QEMU, which runs the VM, not through OpenShift. Red Hat does not support this. While this has been tested, an OpenShift update could stop copies until the driver is updated.';

/** Says that copies of running VMs are not supported by Red Hat. */
const RunningVMCopyNote: React.FC<{ style?: React.CSSProperties }> = ({ style }) => (
  <Alert variant="warning" title="Not supported by Red Hat" isInline style={style}>
    {RUNNING_VM_COPY_NOTE}
  </Alert>
);

export default RunningVMCopyNote;
