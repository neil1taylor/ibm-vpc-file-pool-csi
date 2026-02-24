import React from 'react';
import { render, screen } from '@testing-library/react';
import InlineCapacityBar from './InlineCapacityBar';

describe('InlineCapacityBar', () => {
  it('renders text label with allocated/total/unit/percent', () => {
    render(<InlineCapacityBar allocated={50} total={100} />);
    expect(screen.getByText('50 / 100 GB (50%)')).toBeTruthy();
  });

  it('uses custom unit', () => {
    render(<InlineCapacityBar allocated={10} total={20} unit="TiB" />);
    expect(screen.getByText('10 / 20 TiB (50%)')).toBeTruthy();
  });

  it('applies danger variant at >=95%', () => {
    const { container } = render(<InlineCapacityBar allocated={96} total={100} />);
    expect(container.querySelector('.pf-m-danger')).toBeTruthy();
  });

  it('applies warning variant at >=80%', () => {
    const { container } = render(<InlineCapacityBar allocated={85} total={100} />);
    expect(container.querySelector('.pf-m-warning')).toBeTruthy();
  });

  it('uses default variant below 80%', () => {
    const { container } = render(<InlineCapacityBar allocated={50} total={100} />);
    expect(container.querySelector('.pf-m-danger')).toBeNull();
    expect(container.querySelector('.pf-m-warning')).toBeNull();
  });

  it('handles 0/0 case', () => {
    render(<InlineCapacityBar allocated={0} total={0} />);
    expect(screen.getByText('0 / 0 GB (0%)')).toBeTruthy();
  });

  it('renders aria-label on progress bar', () => {
    render(<InlineCapacityBar allocated={30} total={100} />);
    expect(screen.getByLabelText('30 of 100 GB used')).toBeTruthy();
  });
});
