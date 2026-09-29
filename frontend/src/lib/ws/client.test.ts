/**
 * WsClient integration tests — FakeClock + MockSocket drive the Task
 * 10.3.19 state machine end-to-end: handshake, subscribe grammar,
 * last_seq resume, stale transitions, backoff, gap/resync, 4019 refresh,
 * and the server.shutdown advisory path.
 */
import { describe, expect, it } from 'vitest';

import { WsClient, type WsClientOptions } from './client';
import type { EventFrame, SnapshotFrame } from './protocol';
import { FakeClock, SocketFactory } from './testkit';

const flush = () => new Promise<void>((r) => setTimeout(r, 0));

interface Harness {
  client: WsClient;
  clock: FakeClock;
  sockets: SocketFactory;
  opts: WsClientOptions;
}

type WsOverrides = Omit<WsClientOptions, 'url'>;

function make(overrides: WsOverrides = {}): Harness {
  const clock = new FakeClock();
  const sockets = new SocketFactory();
  const opts: WsClientOptions = {
    url: 'wss://test/ws/v1',
    socketFactory: sockets.make,
    clock,
    rng: () => 0.5, // zero jitter → delays match RECONNECT_SCHEDULE exactly
    isTradingSession: () => true,
    ...overrides,
  };
  return { client: new WsClient(opts), clock, sockets, opts };
}

/** Drive the client to AUTHENTICATED with a token-based handshake. */
async function connectAuth(h: Harness) {
  h.client.start();
  const sock = h.sockets.latest();
  expect(h.client.getStatus().state).toBe('CONNECTING');
  sock.open();
  await flush();
  expect(sock.sentFrames()).toContainEqual(
    expect.objectContaining({ action: 'authenticate', token: 'tok', protocol_version: 1 }),
  );
  sock.recv({ type: 'response', action: 'authenticate', status: 'ACK', data: {}, ts_ms: 1 });
  expect(h.client.getStatus().state).toBe('AUTHENTICATED');
  return sock;
}

const authOpts: WsOverrides = { tokenProvider: () => 'tok' };

describe('handshake + subscribe grammar', () => {
  it('dials the endpoint and sends authenticate with protocol_version', async () => {
    const h = make(authOpts);
    const sock = await connectAuth(h);
    expect(sock.url).toBe('wss://test/ws/v1');
  });

  it('anonymous session (no token) reaches AUTHENTICATED on open', async () => {
    const h = make();
    h.client.start();
    const sock = h.sockets.latest();
    sock.open();
    await flush();
    expect(h.client.getStatus().state).toBe('AUTHENTICATED');
    expect(sock.sentFrames().every((f) => f['action'] !== 'authenticate')).toBe(true);
  });

  it('sends {"action":"subscribe","params":[...]} for live channels', async () => {
    const h = make(authOpts);
    const sock = await connectAuth(h);
    const received: (EventFrame | SnapshotFrame)[] = [];
    h.client.subscribe('bbo@EUR/USD', (f) => received.push(f));
    expect(sock.sentFrames()).toContainEqual(
      expect.objectContaining({ action: 'subscribe', params: ['bbo@EUR/USD'] }),
    );
    sock.recv({ type: 'subscribed', channels: ['bbo@EUR/USD'], total: 1, ts_ms: 1 });
    sock.recv({ type: 'event', channel: 'bbo@EUR/USD', seq: 1, data: { b: 1.08 }, ts_ms: 2 });
    expect(received).toHaveLength(1);
    expect(h.client.lastSeq('bbo@EUR/USD')).toBe(1);
  });

  it('re-establishes channels after reconnect (fresh → subscribe frame)', async () => {
    const h = make(authOpts);
    const sock = await connectAuth(h);
    h.client.subscribe('bbo@EUR/USD', () => undefined);
    sock.serverClose(1006);
    expect(h.client.getStatus().state).toBe('RECONNECTING');
    h.clock.advance(100); // first backoff slot
    const sock2 = h.sockets.latest();
    expect(sock2).not.toBe(sock);
    sock2.open();
    await flush();
    sock2.recv({ type: 'response', action: 'authenticate', status: 'ACK', data: {}, ts_ms: 1 });
    // Channel had no seq (never ticked) → subscribe, state → RESYNCING until
    // the subscribed ack converges it.
    expect(h.client.getStatus().state).toBe('RESYNCING');
    expect(sock2.sentFrames()).toContainEqual(
      expect.objectContaining({ action: 'subscribe', params: ['bbo@EUR/USD'] }),
    );
    sock2.recv({ type: 'subscribed', channels: ['bbo@EUR/USD'], total: 1, ts_ms: 2 });
    expect(h.client.getStatus().state).toBe('AUTHENTICATED');
  });
});

describe('last_seq resume + resync (§10.7/§10.9)', () => {
  async function tickedChannel(h: Harness) {
    const sock = await connectAuth(h);
    h.client.subscribe('depth@EUR/USD:5:100', () => undefined);
    sock.recv({ type: 'subscribed', channels: ['depth@EUR/USD:5:100'], total: 1, ts_ms: 1 });
    sock.recv({ type: 'event', channel: 'depth@EUR/USD:5:100', seq: 40, data: {}, ts_ms: 2 });
    sock.recv({ type: 'event', channel: 'depth@EUR/USD:5:100', seq: 41, data: {}, ts_ms: 3 });
    return sock;
  }

  it('sends {"action":"resume","channel":…,"last_seq":N} on reconnect', async () => {
    const h = make(authOpts);
    await tickedChannel(h);
    const sock = h.sockets.latest();
    sock.serverClose(1006);
    h.clock.advance(100);
    const sock2 = h.sockets.latest();
    sock2.open();
    await flush();
    sock2.recv({ type: 'response', action: 'authenticate', status: 'ACK', data: {}, ts_ms: 1 });
    expect(h.client.getStatus().state).toBe('RESYNCING');
    expect(sock2.sentFrames()).toContainEqual({
      action: 'resume',
      channel: 'depth@EUR/USD:5:100',
      last_seq: 41,
    });
  });

  it('RESYNCING → AUTHENTICATED on `resumed`; cursor follows to_seq', async () => {
    const h = make(authOpts);
    await tickedChannel(h);
    h.sockets.latest().serverClose(1006);
    h.clock.advance(100);
    const sock2 = h.sockets.latest();
    sock2.open();
    await flush();
    sock2.recv({ type: 'response', action: 'authenticate', status: 'ACK', data: {}, ts_ms: 1 });
    sock2.recv({ type: 'event', channel: 'depth@EUR/USD:5:100', seq: 42, data: {}, ts_ms: 2 });
    sock2.recv({
      type: 'resumed',
      channel: 'depth@EUR/USD:5:100',
      from_seq: 42,
      to_seq: 43,
      count: 2,
      ts_ms: 3,
    });
    expect(h.client.getStatus().state).toBe('AUTHENTICATED');
    expect(h.client.lastSeq('depth@EUR/USD:5:100')).toBe(43);
  });

  it('RESYNCING converges via snapshot frame after a resync directive', async () => {
    const h = make(authOpts);
    await tickedChannel(h);
    h.sockets.latest().serverClose(1006);
    // The server decides the cursor is outside its 60s ring horizon — the
    // client-side clock is irrelevant; just clear the first backoff slot.
    h.clock.advance(100);
    const sock2 = h.sockets.latest();
    sock2.open();
    await flush();
    sock2.recv({ type: 'response', action: 'authenticate', status: 'ACK', data: {}, ts_ms: 1 });
    sock2.recv({
      type: 'resync',
      channel: 'depth@EUR/USD:5:100',
      reason: 'gap_too_large',
      last_seq: 41,
      ts_ms: 2,
    });
    sock2.recv({
      type: 'snapshot',
      channel: 'depth@EUR/USD:5:100',
      seq: 500,
      data: { levels: [] },
      ts_ms: 3,
    });
    expect(h.client.getStatus().state).toBe('AUTHENTICATED');
    expect(h.client.lastSeq('depth@EUR/USD:5:100')).toBe(500);
  });

  it('detects a live-stream seq gap → sends resync, drops the frame, fires onGap', async () => {
    const gaps: [string, number, number][] = [];
    const h = make({ ...authOpts, onGap: (c, exp, got) => gaps.push([c, exp, got]) });
    const sock = await connectAuth(h);
    const received: (EventFrame | SnapshotFrame)[] = [];
    h.client.subscribe('bbo@EUR/USD', (f) => received.push(f));
    sock.recv({ type: 'subscribed', channels: ['bbo@EUR/USD'], total: 1, ts_ms: 1 });
    sock.recv({ type: 'event', channel: 'bbo@EUR/USD', seq: 1, data: {}, ts_ms: 2 });
    sock.recv({ type: 'event', channel: 'bbo@EUR/USD', seq: 3, data: {}, ts_ms: 3 }); // gap: expected 2
    expect(received).toHaveLength(1); // gap frame dropped — fail-closed
    expect(gaps).toEqual([['bbo@EUR/USD', 2, 3]]);
    expect(sock.sentFrames()).toContainEqual({
      action: 'resync',
      channel: 'bbo@EUR/USD',
      last_seq: 1,
    });
  });
});

describe('backoff schedule (§21.3)', () => {
  it('walks RECONNECT_SCHEDULE on consecutive failures', async () => {
    const h = make(authOpts);
    await connectAuth(h);
    const expected = [100, 250, 500, 1000, 2000, 10_000, 10_000];
    h.sockets.latest().serverClose(1006); // live conn lost → attempt 1
    let dialed = 1;
    for (const delay of expected) {
      expect(h.client.getStatus().state).toBe('RECONNECTING');
      h.clock.advance(delay - 1);
      expect(h.sockets.sockets).toHaveLength(dialed); // not yet
      h.clock.advance(1);
      dialed += 1;
      expect(h.sockets.sockets).toHaveLength(dialed);
      expect(h.client.getStatus().state).toBe('CONNECTING');
      // refuse the dial → next backoff slot
      h.sockets.latest().serverClose(1006);
    }
  });
});

describe('stale-data surface (§21.3 item 3)', () => {
  it('AUTHENTICATED → STALE after >3.0s silence in trading hours', async () => {
    const h = make(authOpts);
    const sock = await connectAuth(h);
    h.client.subscribe('bbo@EUR/USD', () => undefined);
    sock.recv({ type: 'event', channel: 'bbo@EUR/USD', seq: 1, data: {}, ts_ms: 1 });
    h.clock.advance(3_000 + 250 + 1);
    expect(h.client.getStatus().state).toBe('STALE');
    expect(h.client.getStatus().orderEntryEnabled).toBe(true); // STALE stays enabled
    sock.recv({ type: 'event', channel: 'bbo@EUR/USD', seq: 2, data: {}, ts_ms: 2 });
    expect(h.client.getStatus().state).toBe('AUTHENTICATED');
  });

  it('never goes STALE outside trading hours (24/5 gate)', async () => {
    const h = make({ ...authOpts, isTradingSession: () => false });
    await connectAuth(h);
    h.clock.advance(10_000);
    expect(h.client.getStatus().state).toBe('AUTHENTICATED');
  });

  it('exposes per-channel staleness timestamps to consumers', async () => {
    const h = make(authOpts);
    const sock = await connectAuth(h);
    h.client.subscribe('bbo@EUR/USD', () => undefined);
    sock.recv({ type: 'event', channel: 'bbo@EUR/USD', seq: 1, data: {}, ts_ms: 1 });
    const t1 = h.client.lastTickAt('bbo@EUR/USD');
    expect(t1).toBeGreaterThan(0);
    h.clock.advance(4_000);
    expect(h.client.getStatus().health['bbo@EUR/USD']?.stale).toBe(true);
    expect(h.client.getStatus().health['bbo@EUR/USD']?.lastTickAt).toBe(t1);
  });
});

describe('AUTH_EXPIRED (close 4019) refresh path', () => {
  it('refreshes the token then reconnects without backoff', async () => {
    let refreshed = 0;
    const h = make({
      ...authOpts,
      tokenRefresher: () => {
        refreshed += 1;
        return Promise.resolve('tok2');
      },
    });
    const sock = await connectAuth(h);
    sock.serverClose(4019);
    expect(h.client.getStatus().state).toBe('DISCONNECTED');
    await flush();
    expect(refreshed).toBe(1);
    expect(h.client.getStatus().state).toBe('CONNECTING');
    const sock2 = h.sockets.latest();
    sock2.open();
    await flush();
    sock2.recv({ type: 'response', action: 'authenticate', status: 'ACK', data: {}, ts_ms: 1 });
    expect(h.client.getStatus().state).toBe('AUTHENTICATED');
  });

  it('refresh failure flushes optimistic state and notifies (→ /login)', async () => {
    let flushed = 0;
    let notified = 0;
    const h = make({
      ...authOpts,
      tokenRefresher: () => Promise.reject(new Error('expired')),
      onOptimisticFlush: () => {
        flushed += 1;
      },
      onAuthFailure: () => {
        notified += 1;
      },
    });
    const sock = await connectAuth(h);
    sock.serverClose(4019);
    await flush();
    expect(h.client.getStatus().state).toBe('DISCONNECTED');
    expect(flushed).toBeGreaterThanOrEqual(1);
    expect(notified).toBe(1);
  });

  it('flushes optimistic state across every re-authentication (invariant)', async () => {
    let flushed = 0;
    const h = make({
      ...authOpts,
      onOptimisticFlush: () => {
        flushed += 1;
      },
    });
    const sock = await connectAuth(h);
    expect(flushed).toBe(0); // first auth — nothing to flush
    sock.serverClose(1006);
    h.clock.advance(100);
    const sock2 = h.sockets.latest();
    sock2.open();
    await flush(); // re-authenticate → flush fires
    expect(flushed).toBe(1);
  });
});

describe('server.shutdown advisory (Task 6.3.19)', () => {
  it('moves to RECONNECTING on the advisory endpoint, attempt counter kept', async () => {
    const h = make(authOpts);
    const sock = await connectAuth(h);
    sock.recv({
      type: 'server.shutdown',
      reason: 'drain',
      retry_after_ms: 50,
      endpoint: 'wss://b/ws/v1',
      ts_ms: 1,
    });
    expect(h.client.getStatus().state).toBe('RECONNECTING');
    expect(sock.closed).toBe(true);
    h.clock.advance(50);
    const sock2 = h.sockets.latest();
    expect(sock2.url).toBe('wss://b/ws/v1');
    sock2.open();
    await flush();
    sock2.recv({ type: 'response', action: 'authenticate', status: 'ACK', data: {}, ts_ms: 1 });
    expect(h.client.getStatus().state).toBe('AUTHENTICATED');
  });
});

describe('heartbeat + teardown', () => {
  it('pings on the heartbeat interval while live', async () => {
    const h = make(authOpts);
    const sock = await connectAuth(h);
    h.clock.advance(25_000);
    expect(sock.sentFrames()).toContainEqual({ action: 'ping' });
  });

  it('heartbeat silence beyond the timeout drops the socket → reconnect cycle', async () => {
    const h = make(authOpts);
    const sock = await connectAuth(h);
    // Heartbeat ticks every 25s; the ≥60s silence check trips on the 75s
    // tick, closing the socket → RECONNECTING (+100ms backoff → CONNECTING).
    h.clock.advance(75_050);
    expect(h.client.getStatus().state).toBe('RECONNECTING');
    expect(sock.closed).toBe(true);
    h.clock.advance(60);
    expect(h.client.getStatus().state).toBe('CONNECTING');
    expect(h.sockets.sockets.length).toBeGreaterThan(1);
  });

  it('stop() lands in DISCONNECTED with no reconnect scheduled', async () => {
    const h = make(authOpts);
    const sock = await connectAuth(h);
    h.client.stop();
    expect(h.client.getStatus().state).toBe('DISCONNECTED');
    h.clock.advance(60_000);
    expect(h.sockets.sockets).toHaveLength(1);
    expect(sock.closed).toBe(true);
  });
});
