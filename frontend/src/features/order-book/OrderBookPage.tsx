/**
 * Standalone order-book view — `/book/:symbol` (deep-link popout; the
 * primary surface is the workspace's order-book panel).
 */
import { useParams } from 'react-router';

import { OrderBook } from './OrderBook';

export default function OrderBookPage() {
  const { symbol = 'EUR/USD' } = useParams();
  return (
    <div className="mx-auto max-w-xl p-4">
      <h1 className="mb-4 text-2xl font-semibold">Order Book</h1>
      <OrderBook symbol={symbol} viewportHeight={360} />
    </div>
  );
}
