/**
 * Standalone order-book view — `/book/:symbol`. The trading cockpit
 * (order-entry feature) embeds the same component beside the form.
 */
import { useParams } from 'react-router';

import { OrderBook } from './OrderBook';

export default function OrderBookPage() {
  const { symbol = 'EUR/USD' } = useParams();
  return (
    <div className="mx-auto max-w-xl p-4">
      <OrderBook symbol={symbol} viewportHeight={360} />
    </div>
  );
}
