import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import CardHelpTitle from './CardHelpTitle';
import { CARD_HELP } from '../../utils/card-help';

describe('CardHelpTitle', () => {
  it('shows the card title and a labelled help button that opens the help text', () => {
    render(<CardHelpTitle helpKey="p95AllocationLatency" />);
    expect(screen.getByText('P95 Allocation Latency')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'About P95 Allocation Latency' }));
    expect(screen.getByText(/95 out of 100/)).toBeTruthy();
  });
});

describe('CARD_HELP', () => {
  it('has a title and one to three plain sentences for every card', () => {
    for (const [key, help] of Object.entries(CARD_HELP)) {
      expect(help.title).toBeTruthy();
      const sentences = help.text.split(/(?<=[.!?])\s+/).filter(Boolean);
      expect({ key, count: sentences.length >= 1 && sentences.length <= 3 }).toEqual({ key, count: true });
    }
  });

  it('has no two cards with the same title', () => {
    const titles = Object.values(CARD_HELP).map((h) => h.title);
    expect(new Set(titles).size).toBe(titles.length);
  });
});
