import React from 'react';
import { render, screen } from '@testing-library/react';
import GetHelpCard from './GetHelpCard';
import { SUPPORT_URL, DIAGNOSTICS_URL, TROUBLESHOOTING, GUIDE_BASE } from '../../utils/help-links';

describe('GetHelpCard', () => {
  it('links the support guide, the diagnostics steps and the troubleshooting guide', () => {
    render(<GetHelpCard />);
    expect(screen.getByTestId('help-support').getAttribute('href')).toBe(SUPPORT_URL);
    expect(screen.getByTestId('help-diagnostics').getAttribute('href')).toBe(DIAGNOSTICS_URL);
    expect(screen.getByTestId('help-troubleshooting').getAttribute('href')).toBe(`${GUIDE_BASE}TROUBLESHOOTING.md`);
  });

  it('opens every link in a new tab', () => {
    render(<GetHelpCard />);
    ['help-support', 'help-diagnostics', 'help-troubleshooting'].forEach((id) => {
      expect(screen.getByTestId(id).getAttribute('target')).toBe('_blank');
    });
  });
});

describe('banner links', () => {
  it('have a Troubleshooting section for each banner', () => {
    expect(TROUBLESHOOTING.metrics).toContain('#metrics-issues');
  });
});
