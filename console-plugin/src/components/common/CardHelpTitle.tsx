import React from 'react';
import { Button, CardTitle, Popover } from '@patternfly/react-core';
import { OutlinedQuestionCircleIcon } from '@patternfly/react-icons';
import { CARD_HELP, CardHelpKey } from '../../utils/card-help';

/** A card title with a help button that explains the number in plain words. */
const CardHelpTitle: React.FC<{ helpKey: CardHelpKey }> = ({ helpKey }) => {
  const { title, text } = CARD_HELP[helpKey];
  return (
    <CardTitle>
      {title}{' '}
      <Popover headerContent={title} bodyContent={text}>
        <Button variant="plain" aria-label={`About ${title}`} icon={<OutlinedQuestionCircleIcon />} style={{ padding: 0 }} />
      </Popover>
    </CardTitle>
  );
};

export default CardHelpTitle;
