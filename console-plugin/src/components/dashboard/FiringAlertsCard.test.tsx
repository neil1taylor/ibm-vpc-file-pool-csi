import React from 'react';
import { render, screen } from '@testing-library/react';
import FiringAlertsCard from './FiringAlertsCard';

describe('FiringAlertsCard', () => {
  it('lists firing alerts with severity, pool and share', () => {
    render(
      <FiringAlertsCard
        loading={false}
        error={null}
        rulesLoaded
        firing={[
          { name: 'VPCFilePoolShareNearlyFull', severity: 'warning', pool: 'gp', share: 's1' },
          { name: 'VPCFilePoolControllerDown', severity: 'critical' },
        ]}
      />,
    );
    expect(screen.getByText('VPCFilePoolShareNearlyFull')).toBeTruthy();
    expect(screen.getByText('gp / s1')).toBeTruthy();
    expect(screen.getByText('critical')).toBeTruthy();
    expect(screen.getByText('warning')).toBeTruthy();
  });

  it('says no alerts are firing when the rules are loaded and none fire', () => {
    render(<FiringAlertsCard loading={false} error={null} rulesLoaded firing={[]} />);
    expect(screen.getByText('No alerts firing')).toBeTruthy();
  });

  it('does not say "no alerts firing" when the cluster Prometheus has not loaded the rules', () => {
    render(<FiringAlertsCard loading={false} error={null} rulesLoaded={false} firing={[]} />);
    expect(screen.queryByText('No alerts firing')).toBeNull();
    expect(screen.getByText(/does not evaluate the driver's alert rules/)).toBeTruthy();
  });

  it('shows the error when Prometheus cannot be read', () => {
    render(<FiringAlertsCard loading={false} error="boom" rulesLoaded={false} firing={[]} />);
    expect(screen.getByText(/boom/)).toBeTruthy();
    expect(screen.queryByText('No alerts firing')).toBeNull();
  });
});
