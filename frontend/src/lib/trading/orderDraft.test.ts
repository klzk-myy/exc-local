import { beforeEach, describe, expect, it } from 'vitest';

import { useOrderDraft } from './orderDraft';

const STORAGE_KEY = 'exc.order-draft.v1';

beforeEach(() => {
  localStorage.clear();
  useOrderDraft.setState({ draft: { symbol: '', side: 'BUY', price: '', quantity: '' } });
});

describe('orderDraft persistence', () => {
  it('persists the symbol so a reload restores the traded pair', () => {
    useOrderDraft.getState().setDraft({ symbol: 'GBP/USD' });
    const raw = localStorage.getItem(STORAGE_KEY);
    expect(raw).not.toBeNull();
    expect(raw).toContain('GBP/USD');
  });

  it('never persists side, price, or quantity — stale order fields are dangerous', () => {
    useOrderDraft
      .getState()
      .setDraft({ symbol: 'GBP/USD', side: 'SELL', price: '1.23456', quantity: '50000' });
    const persisted = JSON.parse(localStorage.getItem(STORAGE_KEY) ?? '{}') as {
      state: { draft: { side: string; price: string; quantity: string } };
    };
    expect(persisted.state.draft.side).toBe('BUY');
    expect(persisted.state.draft.price).toBe('');
    expect(persisted.state.draft.quantity).toBe('');
  });
});
