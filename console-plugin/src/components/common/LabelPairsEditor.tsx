import React, { useState } from 'react';
import { Button, Label, Split, SplitItem, Stack, StackItem, TextInput } from '@patternfly/react-core';
import { LabelPair } from '../../utils/backup-policy-form';

interface Props {
  labels: LabelPair[];
  onChange: (labels: LabelPair[]) => void;
  isDisabled?: boolean;
}

/** A list of key=value labels, shown as removable tags, with a row to add one. */
const LabelPairsEditor: React.FC<Props> = ({ labels, onChange, isDisabled }) => {
  const [key, setKey] = useState('');
  const [value, setValue] = useState('');

  const add = () => {
    if (!key.trim() || !value.trim()) return;
    onChange([...labels.filter((l) => l.key !== key.trim()), { key: key.trim(), value: value.trim() }]);
    setKey('');
    setValue('');
  };

  return (
    <Stack hasGutter>
      {labels.length > 0 && (
        <StackItem>
          <Split hasGutter>
            {labels.map((l) => (
              <SplitItem key={l.key}>
                <Label onClose={() => onChange(labels.filter((x) => x.key !== l.key))} closeBtnAriaLabel={`Remove ${l.key}`}>
                  {l.key}={l.value}
                </Label>
              </SplitItem>
            ))}
          </Split>
        </StackItem>
      )}
      <StackItem>
        <Split hasGutter>
          <SplitItem>
            <TextInput
              aria-label="Label key"
              value={key}
              onChange={(_e, v) => setKey(v)}
              placeholder="key"
              isDisabled={isDisabled}
              style={{ width: 180 }}
            />
          </SplitItem>
          <SplitItem>=</SplitItem>
          <SplitItem>
            <TextInput
              aria-label="Label value"
              value={value}
              onChange={(_e, v) => setValue(v)}
              placeholder="value"
              isDisabled={isDisabled}
              style={{ width: 180 }}
              onKeyDown={(e) => {
                if (e.key === 'Enter') {
                  e.preventDefault();
                  add();
                }
              }}
            />
          </SplitItem>
          <SplitItem>
            <Button variant="secondary" size="sm" onClick={add} isDisabled={isDisabled || !key.trim() || !value.trim()}>
              Add label
            </Button>
          </SplitItem>
        </Split>
      </StackItem>
    </Stack>
  );
};

export default LabelPairsEditor;
