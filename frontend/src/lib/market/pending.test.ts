import { beforeEach, describe, expect, it } from 'vitest';

import { usePendingOrders } from './pending';

const base = {
  symbol: 'EUR/USD',
  side: 'BUY' as const,
  type: 'LIMIT',
  quantity: '1000',
  price: '1.0850',
  epoch: 0,
};

beforeEach(() => {
  usePendingOrders.getState().reset();
});

describe('optimistic pending orders (Task 10.3.19 residual)', () => {
  it('renders submitted orders optimistically', () => {
    usePendingOrders.getState().add({ ...base, clientOrderId: 'web-1' });
    expect(usePendingOrders.getState().pending).toHaveLength(1);
    expect(usePendingOrders.getState().pending[0]?.clientOrderId).toBe('web-1');
  });

  it('confirms on ack — entry removed, notice emitted when message given', () => {
    usePendingOrders.getState().add({ ...base, clientOrderId: 'web-1' });
    usePendingOrders.getState().confirm('web-1', 'Order accepted (id 55)');
    const s = usePendingOrders.getState();
    expect(s.pending).toHaveLength(0);
    expect(s.notices[0]?.kind).toBe('confirmed');
  });

  it('rolls back + notice on reject with §23 code and request_id', () => {
    usePendingOrders.getState().add({ ...base, clientOrderId: 'web-2' });
    usePendingOrders.getState().reject('web-2', {
      message: 'insufficient balance',
      code: 'INSUFFICIENT_BALANCE',
      requestId: 'req-9',
    });
    const s = usePendingOrders.getState();
    expect(s.pending).toHaveLength(0);
    expect(s.notices[0]).toMatchObject({
      kind: 'rejected',
      code: 'INSUFFICIENT_BALANCE',
      requestId: 'req-9',
    });
  });

  it('rolls back + notice on timeout', () => {
    usePendingOrders.getState().add({ ...base, clientOrderId: 'web-3' });
    usePendingOrders.getState().timeout('web-3');
    const s = usePendingOrders.getState();
    expect(s.pending).toHaveLength(0);
    expect(s.notices[0]?.kind).toBe('timeout');
  });

  it('flushes pending submitted under an older auth epoch (re-auth invariant)', () => {
    const st = usePendingOrders.getState();
    st.add({ ...base, clientOrderId: 'old-1', epoch: 0 });
    st.add({ ...base, clientOrderId: 'old-2', epoch: 0 });
    st.add({ ...base, clientOrderId: 'new-1', epoch: 1 });
    usePendingOrders.getState().flushEpoch(1);
    const s = usePendingOrders.getState();
    expect(s.pending.map((p) => p.clientOrderId)).toEqual(['new-1']);
    expect(s.notices[0]?.kind).toBe('flushed');
  });

  it('flush is a no-op when nothing is stale', () => {
    usePendingOrders.getState().add({ ...base, clientOrderId: 'cur', epoch: 2 });
    usePendingOrders.getState().flushEpoch(2);
    expect(usePendingOrders.getState().pending).toHaveLength(1);
    expect(usePendingOrders.getState().notices).toHaveLength(0);
  });

  it('reject on an unknown client_order_id still surfaces the notice', () => {
    usePendingOrders.getState().reject('ghost', { message: 'gone' });
    expect(usePendingOrders.getState().notices[0]?.kind).toBe('rejected');
  });
});
