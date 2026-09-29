import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { fieldHelp, GLOSSARY, GlossaryList, HELP_REGISTRY, HelpTooltip } from './help';

describe('help registry', () => {
  it('covers the canonical order fields', () => {
    for (const f of ['symbol', 'side', 'type', 'quantity', 'price', 'time_in_force']) {
      expect(HELP_REGISTRY[f]?.helpText).toBeTruthy();
    }
    expect(fieldHelp('nonexistent')).toBeUndefined();
  });
  it('glossary defines core FX terms', () => {
    const terms = GLOSSARY.map((g) => g.term);
    for (const t of ['pip', 'lot', 'spread', 'mark price', 'value date']) {
      expect(terms).toContain(t);
    }
  });
});

describe('<HelpTooltip>', () => {
  it('opens on click, exposes tooltip, dismisses on Escape', () => {
    render(<HelpTooltip help={fieldHelp('price')!} />);
    const btn = screen.getByRole('button', { name: 'Field help' });
    fireEvent.click(btn);
    expect(screen.getByRole('tooltip')).toHaveTextContent('multiple of the tick size');
    fireEvent.keyDown(btn, { key: 'Escape' });
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();
  });
  it('renders glossary link when a term is attached', () => {
    render(<HelpTooltip help={fieldHelp('price')!} />);
    fireEvent.click(screen.getByRole('button', { name: 'Field help' }));
    expect(screen.getByRole('link', { name: /Glossary: tick size/ })).toHaveAttribute(
      'href',
      '/help#glossary-tick-size',
    );
  });
});

describe('<GlossaryList>', () => {
  it('renders every term with an anchor id', () => {
    render(<GlossaryList />);
    for (const g of GLOSSARY) {
      const el = document.getElementById(`glossary-${g.term.replace(/\s+/g, '-')}`);
      expect(el).not.toBeNull();
    }
  });
});
