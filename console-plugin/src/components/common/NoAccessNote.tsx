import React from 'react';
import { Alert } from '@patternfly/react-core';
import YAMLEditorFallback from './YAMLEditorFallback';

interface Props {
  /** The right that is missing, as a phrase after "allowed to": "create BackupRestore objects". */
  right: string;
  /** What an administrator can apply to do the action for the user. */
  yaml?: string;
}

/** Says which right is missing, and gives the YAML for an administrator to apply in the user's place. */
const NoAccessNote: React.FC<Props> = ({ right, yaml }) => (
  <Alert variant="warning" isInline title={`You are not allowed to ${right}`}>
    <p>This action is off because the cluster says your account may not do it.</p>
    {yaml && (
      <>
        <p>Give this YAML to an administrator. They apply it with the right you do not have.</p>
        <YAMLEditorFallback yaml={yaml} />
      </>
    )}
  </Alert>
);

export default NoAccessNote;
