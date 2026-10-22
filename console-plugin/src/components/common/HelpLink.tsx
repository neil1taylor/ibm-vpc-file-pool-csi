import React from 'react';
import { ExternalLinkAltIcon } from '@patternfly/react-icons';

interface HelpLinkProps {
  href: string;
  label?: string;
  testId?: string;
}

/** A link to a guide. It opens in a new tab. */
const HelpLink: React.FC<HelpLinkProps> = ({ href, label = 'Help', testId = 'help-link' }) => (
  <a href={href} target="_blank" rel="noopener noreferrer" data-test={testId} data-testid={testId}>
    {label} <ExternalLinkAltIcon />
  </a>
);

export default HelpLink;
