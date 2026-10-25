import React from 'react';
import { Button, Popover } from '@patternfly/react-core';
import { OutlinedQuestionCircleIcon } from '@patternfly/react-icons';

/** A small help button that opens a short explanation. */
const InfoPopover: React.FC<{ title: string; text: string; ariaLabel?: string }> = ({ title, text, ariaLabel }) => (
  <Popover headerContent={title} bodyContent={text}>
    <Button
      variant="plain"
      aria-label={ariaLabel || `About ${title}`}
      icon={<OutlinedQuestionCircleIcon />}
      style={{ padding: 0 }}
    />
  </Popover>
);

export default InfoPopover;
