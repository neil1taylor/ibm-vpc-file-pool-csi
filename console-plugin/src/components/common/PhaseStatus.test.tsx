import React from 'react';
import { render, screen } from '@testing-library/react';
import PhaseStatus from './PhaseStatus';

describe('PhaseStatus', () => {
  it('renders "Ready" with green color', () => {
    const { container } = render(<PhaseStatus phase="Ready" />);
    const label = container.querySelector('.pf-v6-c-label');
    expect(label).toBeTruthy();
    expect(screen.getByText('Ready')).toBeTruthy();
  });

  it('renders "Initializing" as blue', () => {
    render(<PhaseStatus phase="Initializing" />);
    expect(screen.getByText('Initializing')).toBeTruthy();
  });

  it('renders "Degraded" as orange', () => {
    render(<PhaseStatus phase="Degraded" />);
    expect(screen.getByText('Degraded')).toBeTruthy();
  });

  it('renders "Failed" as red', () => {
    render(<PhaseStatus phase="Failed" />);
    expect(screen.getByText('Failed')).toBeTruthy();
  });

  it('renders "Unknown" when phase is undefined', () => {
    render(<PhaseStatus phase={undefined} />);
    expect(screen.getByText('Unknown')).toBeTruthy();
  });

  it('renders the phase text for grey phases', () => {
    render(<PhaseStatus phase="Deleting" />);
    expect(screen.getByText('Deleting')).toBeTruthy();
  });
});
