import React from 'react';
import { Card, CardTitle, CardBody, Stack, StackItem } from '@patternfly/react-core';
import HelpLink from './HelpLink';
import { SUPPORT_URL, DIAGNOSTICS_URL, GUIDE_BASE } from '../../utils/help-links';

/** A card with the links a stuck administrator needs. */
const GetHelpCard: React.FC = () => (
  <Card isFullHeight>
    <CardTitle>Get help</CardTitle>
    <CardBody>
      <Stack hasGutter>
        <StackItem>
          <HelpLink href={SUPPORT_URL} label="Help and support guide" testId="help-support" />
        </StackItem>
        <StackItem>
          <HelpLink href={DIAGNOSTICS_URL} label="Collect diagnostics for a support case" testId="help-diagnostics" />
        </StackItem>
        <StackItem>
          <HelpLink href={`${GUIDE_BASE}TROUBLESHOOTING.md`} label="Troubleshooting guide" testId="help-troubleshooting" />
        </StackItem>
      </Stack>
    </CardBody>
  </Card>
);

export default GetHelpCard;
