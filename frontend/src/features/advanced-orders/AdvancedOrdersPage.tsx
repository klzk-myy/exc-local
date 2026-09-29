/**
 * Standalone advanced-order page (`/advanced/:symbol?`) — the full
 * Task 10.3.7 panel + sub-account switcher + positions panel + chart
 * overlays, composed without the workspace grid so the surface works
 * outside the customizable cockpit too.
 */
import { useParams } from 'react-router';
import { useEffect } from 'react';

import { wsClient } from '@/app/runtime';
import { useMarketFeed } from '@/lib/trading/marketStore';
import { useOrderDraft } from '@/lib/trading/orderDraft';

import { AdvancedOrderPanel } from './AdvancedOrderPanel';
import { PositionsPanel } from './PositionsPanel';
import { SubAccountSwitcher } from './SubAccountSwitcher';
import { TradingChart } from './TradingChart';

export default function AdvancedOrdersPage() {
  const { symbol = 'EUR/USD' } = useParams<{ symbol: string }>();
  const setSymbol = useOrderDraft((s) => s.setSymbol);

  useEffect(() => {
    setSymbol(symbol);
  }, [symbol, setSymbol]);

  useMarketFeed(symbol, wsClient, { depth: true });

  return (
    <div className="mx-auto max-w-6xl p-4">
      <div className="mb-4 flex items-center justify-between">
        <h1 className="text-xl font-semibold">{symbol}</h1>
        <SubAccountSwitcher />
      </div>
      <div className="grid gap-4 lg:grid-cols-[380px_1fr]">
        <AdvancedOrderPanel />
        <TradingChart symbol={symbol} />
      </div>
      <div className="mt-4">
        <PositionsPanel />
      </div>
    </div>
  );
}
