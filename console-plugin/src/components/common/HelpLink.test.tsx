import React from 'react';
import { render, screen } from '@testing-library/react';
import HelpLink from './HelpLink';

describe('HelpLink', () => {
  it('opens the guide in a new tab, safely', () => {
    render(<HelpLink href="https://example.com/guide.md#part" />);
    const a = screen.getByTestId('help-link') as HTMLAnchorElement;
    expect(a.getAttribute('href')).toBe('https://example.com/guide.md#part');
    expect(a.getAttribute('target')).toBe('_blank');
    expect(a.getAttribute('rel')).toBe('noopener noreferrer');
    expect(a.textContent).toContain('Help');
  });

  it('can carry another label', () => {
    render(<HelpLink href="https://example.com/x" label="Troubleshooting" testId="t-link" />);
    expect(screen.getByTestId('t-link').textContent).toContain('Troubleshooting');
  });
});
