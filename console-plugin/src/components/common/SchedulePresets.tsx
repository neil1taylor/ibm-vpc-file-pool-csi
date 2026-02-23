import React from 'react';
import { ToggleGroup, ToggleGroupItem, TextInput, Split, SplitItem } from '@patternfly/react-core';

interface Preset {
  label: string;
  value: string;
}

interface SchedulePresetsProps {
  presets: Preset[];
  value: string;
  onChange: (value: string) => void;
  inputId: string;
}

const SchedulePresets: React.FC<SchedulePresetsProps> = ({
  presets,
  value,
  onChange,
  inputId,
}) => {
  const isCustom = !presets.some((p) => p.value === value);

  return (
    <Split hasGutter>
      <SplitItem>
        <ToggleGroup aria-label="Schedule presets">
          {presets.map((preset) => (
            <ToggleGroupItem
              key={preset.value}
              text={preset.label}
              buttonId={`${inputId}-preset-${preset.value}`}
              isSelected={value === preset.value}
              onChange={() => onChange(preset.value)}
            />
          ))}
          <ToggleGroupItem
            text="Custom"
            buttonId={`${inputId}-preset-custom`}
            isSelected={isCustom}
            onChange={() => {
              if (!isCustom) onChange('');
            }}
          />
        </ToggleGroup>
      </SplitItem>
      {isCustom && (
        <SplitItem>
          <TextInput
            id={inputId}
            value={value}
            onChange={(_event, val) => onChange(val)}
            placeholder="e.g. 30m, 2h"
            style={{ width: 120 }}
          />
        </SplitItem>
      )}
    </Split>
  );
};

export default SchedulePresets;
