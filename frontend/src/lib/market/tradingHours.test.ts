/**
 * 24/5 FX session boundary tests (spec §6.3, AGENTS.md):
 * Sydney open 21:00 UTC Sunday → New York close 22:00 UTC Friday.
 */
import { describe, expect, it } from 'vitest';

import { isFxMarketOpen, nextFxMarketTransitionMs } from './tradingHours';

const T = (iso: string) => Date.parse(iso);

describe('isFxMarketOpen', () => {
  it.each<[string, boolean]>([
    ['2026-01-12T00:00:00Z', true], // Monday open
    ['2026-01-16T21:59:59Z', true], // Friday just before NY close
    ['2026-01-16T22:00:00Z', false], // Friday NY close
    ['2026-01-17T12:00:00Z', false], // Saturday
    ['2026-01-18T20:59:59Z', false], // Sunday just before Sydney open
    ['2026-01-18T21:00:00Z', true], // Sunday Sydney open
    ['2026-01-18T23:30:00Z', true], // Sunday evening session
  ])('%s → open=%s', (iso, expected) => {
    expect(isFxMarketOpen(T(iso))).toBe(expected);
  });
});

describe('nextFxMarketTransitionMs', () => {
  it('Friday session → counts to the 22:00 close', () => {
    // 2026-01-16 is a Friday; at 21:00 there is 1h to the close.
    expect(nextFxMarketTransitionMs(T('2026-01-16T21:00:00Z'))).toBe(3_600_000);
  });
  it('Saturday → counts to Sunday 21:00 open', () => {
    // Sat 00:00 → Sun 21:00 = 45h.
    expect(nextFxMarketTransitionMs(T('2026-01-17T00:00:00Z'))).toBe(45 * 3_600_000);
  });
  it('Sunday evening session wraps to next Friday close', () => {
    // Sun 21:30 → Fri 22:00 = 4d 24.5h? → Mon00:00..Fri22:00 = 118h; offset
    // from Sun21:30 = 118h - 21.5h... compute via wrap: (week - 165.5h) + 118h
    // = 2.5h + 118h = 120.5h
    expect(nextFxMarketTransitionMs(T('2026-01-18T21:30:00Z'))).toBe(120.5 * 3_600_000);
  });
});
