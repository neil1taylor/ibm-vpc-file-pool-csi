import React from 'react';
import { render, screen } from '@testing-library/react';
import ConditionsTable from './ConditionsTable';
import { Condition } from '../../types';

describe('ConditionsTable', () => {
  it('renders "No conditions reported." for undefined', () => {
    render(<ConditionsTable conditions={undefined} />);
    expect(screen.getByText('No conditions reported.')).toBeTruthy();
  });

  it('renders "No conditions reported." for empty array', () => {
    render(<ConditionsTable conditions={[]} />);
    expect(screen.getByText('No conditions reported.')).toBeTruthy();
  });

  it('renders table rows for each condition', () => {
    const conditions: Condition[] = [
      {
        type: 'Ready',
        status: 'True',
        reason: 'AllSharesHealthy',
        message: 'Pool is operating normally',
        lastTransitionTime: new Date(Date.now() - 5 * 60 * 1000).toISOString(),
      },
      {
        type: 'Expanding',
        status: 'False',
        reason: 'NotExpanding',
        message: 'No expansion in progress',
      },
    ];

    render(<ConditionsTable conditions={conditions} />);

    // Check that condition types are rendered
    expect(screen.getByText('Ready')).toBeTruthy();
    expect(screen.getByText('Expanding')).toBeTruthy();

    // Check status labels
    expect(screen.getByText('True')).toBeTruthy();
    expect(screen.getByText('False')).toBeTruthy();

    // Check reasons
    expect(screen.getByText('AllSharesHealthy')).toBeTruthy();
    expect(screen.getByText('NotExpanding')).toBeTruthy();

    // Check messages
    expect(screen.getByText('Pool is operating normally')).toBeTruthy();
    expect(screen.getByText('No expansion in progress')).toBeTruthy();
  });

  it('renders green label for True status', () => {
    const conditions: Condition[] = [
      { type: 'Ready', status: 'True' },
    ];
    const { container } = render(<ConditionsTable conditions={conditions} />);
    // PatternFly label with color="green"
    const label = container.querySelector('[class*="label"][class*="green"]');
    expect(label).toBeTruthy();
  });

  it('renders red label for False status', () => {
    const conditions: Condition[] = [
      { type: 'Error', status: 'False' },
    ];
    const { container } = render(<ConditionsTable conditions={conditions} />);
    const label = container.querySelector('[class*="label"][class*="red"]');
    expect(label).toBeTruthy();
  });

  it('renders a label for Unknown status (neither green nor red)', () => {
    const conditions: Condition[] = [
      { type: 'Check', status: 'Unknown' },
    ];
    render(<ConditionsTable conditions={conditions} />);
    // The status text should render
    expect(screen.getByText('Unknown')).toBeTruthy();
  });

  it('renders "-" for missing reason and message', () => {
    const conditions: Condition[] = [
      { type: 'Ready', status: 'True' },
    ];
    render(<ConditionsTable conditions={conditions} />);
    // Two "-" cells: reason and message
    const dashes = screen.getAllByText('-');
    expect(dashes.length).toBeGreaterThanOrEqual(2);
  });
});
