/**
 * WS harness for feature tests (trading-ux cluster) — a real WsClient
 * wired to the lib/ws testkit ports: FakeClock + MockSocket. Component
 * tests drive the full handshake so `orderEntryEnabled` behaves exactly
 * as production (enabled only in AUTHENTICATED/STALE).
 */
import { act } from '@testing-library/react';

import { WsClient, type WsClientOptions } from '@/lib/ws';
import { FakeClock, SocketFactory, type MockSocket } from '@/lib/ws/testkit';

export interface WsHarness {
  client: WsClient;
  clock: FakeClock;
  sockets: SocketFactory;
}

export function makeWsHarness(overrides: Partial<WsClientOptions> = {}): WsHarness {
  const clock = new FakeClock();
  const sockets = new SocketFactory();
  const client = new WsClient({
    url: 'wss://test/ws/v1',
    socketFactory: sockets.make,
    clock,
    rng: () => 0.5,
    isTradingSession: () => true,
    tokenProvider: () => 'tok',
    ...overrides,
  });
  return { client, clock, sockets };
}

/** Start + complete the auth handshake → AUTHENTICATED (order entry on).
 * Everything runs inside act() so React commits the status updates before
 * the test asserts on enabled/disabled controls. */
export async function connectWs(h: WsHarness): Promise<MockSocket> {
  let sock!: MockSocket;
  await act(async () => {
    h.client.start();
    sock = h.sockets.latest();
    sock.open();
    await new Promise<void>((r) => setTimeout(r, 0));
  });
  await act(async () => {
    sock.recv({ type: 'response', action: 'authenticate', status: 'ACK', data: {}, ts_ms: 1 });
    await new Promise<void>((r) => setTimeout(r, 0));
  });
  // Channels subscribed before the auth ACK land the client in RESYNCING
  // pending their `subscribed` acks — converge them so the handshake
  // actually reaches AUTHENTICATED (order entry on) per the docstring.
  const requested = sock
    .sentFrames()
    .filter((f) => f['action'] === 'subscribe')
    .flatMap((f) => (Array.isArray(f['params']) ? (f['params'] as string[]) : []));
  if (requested.length > 0) {
    await act(async () => {
      sock.recv({ type: 'subscribed', channels: requested, total: requested.length, ts_ms: 2 });
      await new Promise<void>((r) => setTimeout(r, 0));
    });
  }
  return sock;
}

/** Ack a subscription the client requested. */
export function ackSub(sock: MockSocket, channel: string, tsMs = 2): void {
  act(() => {
    sock.recv({ type: 'subscribed', channels: [channel], total: 1, ts_ms: tsMs });
  });
}

/** Push a sequenced event frame. */
export function pushEvent(
  sock: MockSocket,
  channel: string,
  seq: number,
  data: unknown,
  tsMs = 10,
): void {
  act(() => {
    sock.recv({ type: 'event', channel, seq, data, ts_ms: tsMs });
  });
}
