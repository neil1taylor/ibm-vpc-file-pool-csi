import React from 'react';
import { Popover, FormGroup, FormGroupLabelHelp } from '@patternfly/react-core';

interface FieldPopoverProps {
  fieldId: string;
  label: string;
  description: string;
  isRequired?: boolean;
  children: React.ReactNode;
}

const FieldPopover: React.FC<FieldPopoverProps> = ({
  fieldId,
  label,
  description,
  isRequired,
  children,
}) => {
  const labelHelp = (
    <Popover bodyContent={description}>
      <FormGroupLabelHelp aria-label={`More info for ${label}`} />
    </Popover>
  );

  return (
    <FormGroup
      label={label}
      isRequired={isRequired}
      fieldId={fieldId}
      labelHelp={labelHelp}
    >
      {children}
    </FormGroup>
  );
};

export default FieldPopover;
