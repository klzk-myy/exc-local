/**
 * Deep-link shim — `/trade/:symbol` and `/advanced/:symbol` fold into
 * the workspace (IA consolidation 2026-10). The symbol param lands in
 * the order-draft store so every panel (ticket, book, chart, depth)
 * resolves to the same instrument before the workspace mounts.
 */
import { useEffect } from 'react';
import { Navigate, useParams } from 'react-router';

import { useOrderDraft } from '@/lib/trading/orderDraft';

export default function SymbolRedirect() {
  const { symbol } = useParams<{ symbol: string }>();
  const setSymbol = useOrderDraft((s) => s.setSymbol);

  useEffect(() => {
    if (symbol !== undefined && symbol !== '') setSymbol(symbol);
  }, [symbol, setSymbol]);

  return <Navigate to="/workspace" replace />;
}
