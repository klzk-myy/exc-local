/**
 * Calculator page (`/calculator/:symbol?`) — deep-link host for the
 * PositionCalculator widget (Task 10.3.8); the same widget opens in a
 * modal from the order ticket.
 */
import { useParams } from 'react-router';

import { wsClient } from '@/app/runtime';
import type { WsClient } from '@/lib/ws';

import { PositionCalculator } from './PositionCalculator';

export default function CalculatorPage({ client = wsClient }: { client?: WsClient }) {
  const { symbol: routeSymbol } = useParams<{ symbol: string }>();

  return (
    <div className="mx-auto max-w-3xl p-6">
      <h1 className="mb-1 text-xl font-semibold">Position &amp; margin calculator</h1>
      <p className="mb-5 text-sm text-neutral-400">
        Notional, margin, pip value, and a maintenance-model liquidation estimate. Amounts are in
        the pair&apos;s quote currency unless noted — all figures are estimates, not guarantees.
      </p>
      <PositionCalculator client={client} initialSymbol={routeSymbol ?? 'EUR/USD'} />
    </div>
  );
}
