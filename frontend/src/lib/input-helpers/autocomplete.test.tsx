import { fireEvent, render, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { Autocomplete, fuzzyRank, fuzzyScore, presetAmount } from './autocomplete';

describe('fuzzyScore / fuzzyRank', () => {
  it('substring beats subsequence', () => {
    expect(fuzzyScore('eur', 'EUR/USD')).toBeGreaterThan(fuzzyScore('eud', 'EUR/USD'));
    expect(fuzzyScore('zzz', 'EUR/USD')).toBe(-1);
    expect(fuzzyScore('', 'anything')).toBe(0);
  });
  it('ranks and limits', () => {
    const items = ['EUR/USD', 'USD/JPY', 'EUR/GBP', 'GBP/USD'];
    expect(fuzzyRank('eur', items, (s) => [s])).toEqual(['EUR/USD', 'EUR/GBP']);
    expect(fuzzyRank('usd', items, (s) => [s], 2)).toHaveLength(2);
  });
});

describe('<Autocomplete> ARIA combobox', () => {
  const items = [
    { id: 'a', label: 'EUR/USD' },
    { id: 'b', label: 'EUR/GBP' },
  ];
  function setup() {
    const onSelect = vi.fn();
    const onChange = vi.fn();
    render(
      <Autocomplete
        items={items}
        value="eu"
        onChange={onChange}
        onSelect={onSelect}
        itemKey={(i) => i.id}
        itemLabel={(i) => i.label}
      />,
    );
    const input = screen.getByRole('combobox');
    fireEvent.focus(input);
    return { input, onSelect, onChange };
  }

  it('opens the listbox and navigates with arrows + Enter', () => {
    const { input, onSelect } = setup();
    expect(screen.getByRole('listbox')).toBeInTheDocument();
    fireEvent.keyDown(input, { key: 'ArrowDown' });
    expect(input.getAttribute('aria-activedescendant')).toContain('-opt-1');
    fireEvent.keyDown(input, { key: 'ArrowUp' });
    expect(input.getAttribute('aria-activedescendant')).toContain('-opt-0');
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onSelect).toHaveBeenCalledWith(items[0]);
  });

  it('Escape closes the listbox', () => {
    const { input } = setup();
    fireEvent.keyDown(input, { key: 'Escape' });
    expect(screen.queryByRole('listbox')).not.toBeInTheDocument();
    expect(input.getAttribute('aria-expanded')).toBe('false');
  });

  it('mouse click selects an option', () => {
    const { onSelect } = setup();
    fireEvent.mouseDown(screen.getAllByRole('option')[1]!);
    expect(onSelect).toHaveBeenCalledWith(items[1]);
  });
});

describe('presetAmount', () => {
  it('computes 25/50/75/100% in fixed point', () => {
    expect(presetAmount('1000', '25')).toBe('250');
    expect(presetAmount('33.333', '50')).toBe('16.6665');
    expect(presetAmount('bad', '50')).toBeNull();
  });
});
