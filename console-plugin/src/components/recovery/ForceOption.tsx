import React, { useState } from 'react';
import { Alert, Button, Checkbox } from '@patternfly/react-core';

interface Props {
  onPlanWithForce: () => void;
  isBusy: boolean;
}

/** Offered when the driver refuses because a copy is still being applied to a disk. Forcing needs a second, explicit step. */
const ForceOption: React.FC<Props> = ({ onPlanWithForce, isBusy }) => {
  const [accepted, setAccepted] = useState(false);
  return (
    <Alert variant="warning" isInline title="You can go on anyway, but a copy that is not finished is lost">
      <p>
        Waiting is safer: the copy ends by itself, and then you can plan again. If you go on now, the copy that is
        still being applied is rolled back first, and the disk shows the moment before it.
      </p>
      <Checkbox
        id="failover-force-ack"
        label="I understand that the unfinished copy is rolled back"
        isChecked={accepted}
        onChange={(_e, checked) => setAccepted(checked)}
      />
      <Button variant="warning" onClick={onPlanWithForce} isDisabled={!accepted || isBusy}>
        Plan again with force
      </Button>
    </Alert>
  );
};

export default ForceOption;
