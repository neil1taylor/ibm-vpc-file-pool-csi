import React from 'react';
import { render, screen, fireEvent } from '@testing-library/react';
import SchedulePresets from './SchedulePresets';

const presets = [
  { label: 'Every 5m', value: '5m' },
  { label: 'Every 15m', value: '15m' },
  { label: 'Every 1h', value: '1h' },
];

describe('SchedulePresets', () => {
  it('renders all preset buttons', () => {
    render(
      <SchedulePresets presets={presets} value="5m" onChange={jest.fn()} inputId="test" />,
    );
    expect(screen.getByText('Every 5m')).toBeTruthy();
    expect(screen.getByText('Every 15m')).toBeTruthy();
    expect(screen.getByText('Every 1h')).toBeTruthy();
    expect(screen.getByText('Custom')).toBeTruthy();
  });

  it('calls onChange with preset value when preset is clicked', () => {
    const onChange = jest.fn();
    render(
      <SchedulePresets presets={presets} value="5m" onChange={onChange} inputId="test" />,
    );
    fireEvent.click(screen.getByText('Every 15m'));
    expect(onChange).toHaveBeenCalledWith('15m');
  });

  it('shows text input when value is custom (not matching any preset)', () => {
    render(
      <SchedulePresets presets={presets} value="30m" onChange={jest.fn()} inputId="test" />,
    );
    // Custom mode: text input should be visible
    const input = document.getElementById('test');
    expect(input).toBeTruthy();
    expect((input as HTMLInputElement).value).toBe('30m');
  });

  it('does not show text input when value matches a preset', () => {
    render(
      <SchedulePresets presets={presets} value="5m" onChange={jest.fn()} inputId="test" />,
    );
    const input = document.getElementById('test');
    expect(input).toBeNull();
  });

  it('calls onChange with empty string when Custom is clicked', () => {
    const onChange = jest.fn();
    render(
      <SchedulePresets presets={presets} value="5m" onChange={onChange} inputId="test" />,
    );
    fireEvent.click(screen.getByText('Custom'));
    expect(onChange).toHaveBeenCalledWith('');
  });

  it('calls onChange when typing in custom input', () => {
    const onChange = jest.fn();
    render(
      <SchedulePresets presets={presets} value="30m" onChange={onChange} inputId="test" />,
    );
    const input = document.getElementById('test') as HTMLInputElement;
    fireEvent.change(input, { target: { value: '45m' } });
    expect(onChange).toHaveBeenCalledWith('45m');
  });
});
