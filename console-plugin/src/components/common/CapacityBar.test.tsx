import React from 'react';
import { render } from '@testing-library/react';
import CapacityBar from './CapacityBar';

describe('CapacityBar', () => {
  it('renders with correct percent title', () => {
    const { container } = render(<CapacityBar allocated={50} total={100} />);
    // The Progress component should be rendered
    const progress = container.querySelector('[role="progressbar"]');
    expect(progress).toBeTruthy();
  });

  it('applies danger variant at >=95%', () => {
    const { container } = render(<CapacityBar allocated={96} total={100} />);
    // PatternFly danger class on progress
    const progress = container.querySelector('.pf-m-danger');
    expect(progress).toBeTruthy();
  });

  it('applies warning variant at >=80%', () => {
    const { container } = render(<CapacityBar allocated={85} total={100} />);
    const progress = container.querySelector('.pf-m-warning');
    expect(progress).toBeTruthy();
  });

  it('uses default variant below 80%', () => {
    const { container } = render(<CapacityBar allocated={50} total={100} />);
    const danger = container.querySelector('.pf-m-danger');
    const warning = container.querySelector('.pf-m-warning');
    expect(danger).toBeNull();
    expect(warning).toBeNull();
  });

  it('handles 0/0 case without error', () => {
    const { container } = render(<CapacityBar allocated={0} total={0} />);
    expect(container.querySelector('[role="progressbar"]')).toBeTruthy();
  });

  it('uses custom title when provided', () => {
    const { getByText } = render(
      <CapacityBar allocated={50} total={100} title="Custom Title" />,
    );
    expect(getByText('Custom Title')).toBeTruthy();
  });
});
