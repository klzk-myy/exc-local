/**
 * Trading cockpit — `/trade/:symbol` (Task 10.3.3 item 4 surface).
 * Order book on the left, order ticket on the right; clicking a book
 * level prefills the ticket's price (asks → BUY, bids → SELL).
 */
import { useState } from 'react';
import { useParams } from 'react-router';

import { OrderBook } from '../order-book/OrderBook';

import { OrderEntry } from './OrderEntry';

export default function TradePage() {
  const { symbol = 'EUR/USD' } = useParams();
  const [prefill, setPrefill] = useState<{
    price: string;
    from: 'bid' | 'ask';
    tick: number;
  } | null>(null);
  return (
    <div className="mx-auto grid max-w-4xl gap-4 p-4 md:grid-cols-2">
      <OrderBook
        symbol={symbol}
        viewportHeight={360}
        onPriceClick={(price, side) =>
          setPrefill((p) => ({ price, from: side, tick: (p?.tick ?? 0) + 1 }))
        }
      />
      <OrderEntry symbol={symbol} prefill={prefill} />
    </div>
  );
}
