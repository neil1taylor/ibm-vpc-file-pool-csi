import React from 'react';
import { Alert } from '@patternfly/react-core';
import YAMLEditorFallback from './YAMLEditorFallback';

interface Props {
  /** The right that is missing, as a phrase after "allowed to": "create BackupRestore objects". */
  right: string;
  /** What an administrator can apply to do the action for the user. */
  yaml?: string;
  /** A command an administrator runs instead, with a sentence that says what it does. */
  command?: { text: string; note: string };
}

/** Says which right is missing, and gives the YAML for an administrator to apply in the user's place. */
const NoAccessNote: React.FC<Props> = ({ right, yaml, command }) => (
  <Alert variant="warning" isInline title={`You are not allowed to ${right}`}>
    <p>This action is off because the cluster says your account may not do it.</p>
    {yaml && (
      <>
        <p>Give this YAML to an administrator. They apply it with the right you do not have.</p>
        <YAMLEditorFallback yaml={yaml} />
      </>
    )}
    {command && (
      <>
        <p>Give this command to an administrator. {command.note}</p>
        <pre>
          <code>{command.text}</code>
        </pre>
      </>
    )}
  </Alert>
);

export default NoAccessNote;
