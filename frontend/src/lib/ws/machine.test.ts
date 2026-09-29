/**
 * Pure state-machine tests — Task 10.3.19 normative table.
 * Deterministic jitter: rng = () => 0.5 ⇒ zero jitter offset.
 */
import { describe, expect, it } from 'vitest';

import {
  backoffDelayMs,
  bannerRequired,
  initialSnapshot,
  orderEntryEnabled,
  transition,
  type WsMachineCtx,
  type WsState,
} from './machine';

const rng = () => 0.5; // zero jitter

interface Snap {
  state: WsState;
  ctx: WsMachineCtx;
}
const snap = (state: WsState, ctx: Partial<WsMachineCtx> = {}): Snap => ({
  state,
  ctx: { attempt: 0, pendingConvergence: 0, ...ctx },
});

describe('RECONNECT_SCHEDULE + jitter (§21.3)', () => {
  it('walks 100/250/500/1s/2s then caps at 10s', () => {
    const expected = [100, 250, 500, 1000, 2000, 10_000, 10_000, 10_000];
    for (let attempt = 0; attempt < expected.length; attempt++) {
      expect(backoffDelayMs(attempt, rng)).toBe(expected[attempt]);
    }
  });

  it('applies ±20% jitter', () => {
    const lo = backoffDelayMs(0, () => 0); // −20%
    const hi = backoffDelayMs(0, () => 1); // +20%
    expect(lo).toBe(80);
    expect(hi).toBe(120);
  });
});

describe('transitions', () => {
  it('DISCONNECTED → connect → CONNECTING + dial', () => {
    const s = transition(initialSnapshot(), { type: 'connect' }, rng);
    expect(s.state).toBe('CONNECTING');
    expect(s.effects.map((e) => e.kind)).toContain('dial');
  });

  it('CONNECTING → auth-ack (no resume) → AUTHENTICATED', () => {
    const s = transition(snap('CONNECTING'), { type: 'auth-ack', resumeCount: 0 }, rng);
    expect(s.state).toBe('AUTHENTICATED');
    expect(s.effects.map((e) => e.kind)).toEqual(
      expect.arrayContaining(['re-establish-channels', 'start-stale-monitor']),
    );
  });

  it('CONNECTING → auth-ack (resume pending) → RESYNCING', () => {
    const s = transition(snap('CONNECTING'), { type: 'auth-ack', resumeCount: 2 }, rng);
    expect(s.state).toBe('RESYNCING');
    expect(s.ctx.pendingConvergence).toBe(2);
  });

  it('RESYNCING → converges per channel → AUTHENTICATED', () => {
    let s = transition(
      snap('RESYNCING', { pendingConvergence: 2 }),
      { type: 'channel-converged' },
      rng,
    );
    expect(s.state).toBe('RESYNCING');
    s = transition({ state: s.state, ctx: s.ctx }, { type: 'channel-converged' }, rng);
    expect(s.state).toBe('AUTHENTICATED');
  });

  it('AUTHENTICATED ↔ STALE via stale-timer / tick', () => {
    let s = transition(snap('AUTHENTICATED'), { type: 'stale-timer' }, rng);
    expect(s.state).toBe('STALE');
    s = transition({ state: s.state, ctx: s.ctx }, { type: 'tick' }, rng);
    expect(s.state).toBe('AUTHENTICATED');
  });

  it('stale-timer is a no-op when not AUTHENTICATED', () => {
    const s = transition(snap('CONNECTING'), { type: 'stale-timer' }, rng);
    expect(s.state).toBe('CONNECTING');
    expect(s.effects).toHaveLength(0);
  });

  it('socket close traverses DISCONNECTED → RECONNECTING with backoff', () => {
    const s = transition(snap('AUTHENTICATED'), { type: 'socket-closed', code: 1006 }, rng);
    expect(s.visited).toEqual(['AUTHENTICATED', 'DISCONNECTED', 'RECONNECTING']);
    expect(s.state).toBe('RECONNECTING');
    expect(s.ctx.attempt).toBe(1);
    const backoff = s.effects.find((e) => e.kind === 'schedule-backoff');
    expect(backoff).toMatchObject({ delayMs: 100 });
  });

  it('RECONNECTING → backoff-elapsed → CONNECTING + dial', () => {
    const s = transition(snap('RECONNECTING', { attempt: 1 }), { type: 'backoff-elapsed' }, rng);
    expect(s.state).toBe('CONNECTING');
    expect(s.effects).toContainEqual({ kind: 'dial' });
  });

  it('successful handshake (socket-open) resets the attempt counter', () => {
    const s = transition(snap('CONNECTING', { attempt: 4 }), { type: 'socket-open' }, rng);
    expect(s.ctx.attempt).toBe(0);
    expect(s.effects.map((e) => e.kind)).toContain('send-authenticate');
  });

  it('close 4019 → DISCONNECTED + refresh-auth (not backoff)', () => {
    const s = transition(snap('AUTHENTICATED'), { type: 'socket-closed', code: 4019 }, rng);
    expect(s.state).toBe('DISCONNECTED');
    expect(s.effects.map((e) => e.kind)).toContain('refresh-auth');
    expect(s.effects.map((e) => e.kind)).not.toContain('schedule-backoff');
  });

  it('auth-refresh-ok → CONNECTING immediately', () => {
    const s = transition(snap('DISCONNECTED'), { type: 'auth-refresh-ok' }, rng);
    expect(s.state).toBe('CONNECTING');
    expect(s.effects).toContainEqual({ kind: 'dial' });
  });

  it('auth-refresh-failed → DISCONNECTED + flush + notify', () => {
    const s = transition(snap('DISCONNECTED'), { type: 'auth-refresh-failed' }, rng);
    expect(s.state).toBe('DISCONNECTED');
    expect(s.effects.map((e) => e.kind)).toEqual(
      expect.arrayContaining(['flush-optimistic', 'notify-auth-failure']),
    );
  });

  it('server.shutdown → RECONNECTING, keeps attempt counter, honors retry_after', () => {
    const s = transition(
      snap('AUTHENTICATED', { attempt: 3 }),
      { type: 'shutdown-advisory', retryAfterMs: 5000 },
      rng,
    );
    expect(s.state).toBe('RECONNECTING');
    expect(s.ctx.attempt).toBe(3); // neither reset nor incremented
    expect(s.effects).toContainEqual({ kind: 'schedule-backoff', delayMs: 5000 });
    expect(s.effects).toContainEqual({ kind: 'close-socket' });
  });

  it('the socket-closed that follows a shutdown advisory does not double-schedule', () => {
    const after = transition(
      snap('AUTHENTICATED', { attempt: 0 }),
      { type: 'shutdown-advisory' },
      rng,
    );
    const s = transition(
      { state: after.state, ctx: after.ctx },
      { type: 'socket-closed', code: 1001 },
      rng,
    );
    expect(s.state).toBe('RECONNECTING');
    expect(s.effects).toHaveLength(0);
  });

  it('heartbeat-missed closes the socket (close event drives the transition)', () => {
    const s = transition(snap('AUTHENTICATED'), { type: 'heartbeat-missed' }, rng);
    expect(s.state).toBe('AUTHENTICATED'); // unchanged — wait for close
    expect(s.effects).toContainEqual({ kind: 'close-socket' });
  });

  it('user disconnect → DISCONNECTED, no backoff', () => {
    const s = transition(snap('AUTHENTICATED'), { type: 'disconnect' }, rng);
    expect(s.state).toBe('DISCONNECTED');
    expect(s.effects.map((e) => e.kind)).not.toContain('schedule-backoff');
  });
});

describe('order-entry lock + banner semantics (Task 10.3.19)', () => {
  it.each<[WsState, boolean, boolean]>([
    ['CONNECTING', false, false],
    ['AUTHENTICATED', true, false],
    ['STALE', true, false],
    ['DISCONNECTED', false, true],
    ['RECONNECTING', false, true],
    ['RESYNCING', false, false],
  ])('%s → orderEntry=%s banner=%s', (state, entry, banner) => {
    expect(orderEntryEnabled(state)).toBe(entry);
    expect(bannerRequired(state)).toBe(banner);
  });
});
