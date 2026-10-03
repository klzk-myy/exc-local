/**
 * Order-draft store — the cross-panel prefill seam.
 *
 * Clicking a depth level (Task 10.3.12) or dragging an overlay (Task
 * 10.3.15) writes here; the order panel (Task 10.3.7) reads it. Keeping
 * the draft in Zustand means panels compose without prop drilling and
 * the workspace layout can reorder/remount panels without losing the
 * in-progress ticket.
 */
import { create } from 'zustand';
import { persist } from 'zustand/middleware';

export interface OrderDraft {
  symbol: string;
  side: 'BUY' | 'SELL';
  price: string;
  quantity: string;
}

interface OrderDraftState {
  draft: OrderDraft;
  setDraft: (d: Partial<OrderDraft>) => void;
  setSymbol: (symbol: string) => void;
}

export const useOrderDraft = create<OrderDraftState>()(
  persist(
    (set) => ({
      draft: { symbol: '', side: 'BUY', price: '', quantity: '' },
      setDraft: (d) => set((s) => ({ draft: { ...s.draft, ...d } })),
      setSymbol: (symbol) => set((s) => ({ draft: { ...s.draft, symbol } })),
    }),
    {
      name: 'exc.order-draft.v1',
      // Only the symbol survives a reload — a stale price/quantity draft
      // would be dangerous; a remembered symbol is just a preference.
      partialize: (s) => ({
        draft: { ...s.draft, side: 'BUY', price: '', quantity: '' },
      }),
    },
  ),
);
