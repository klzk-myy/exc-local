/**
 * FX trading hours — 24/5 (spec §6.3, AGENTS.md canonical):
 * Sydney open 21:00 UTC Sunday → New York close 22:00 UTC Friday.
 *
 * Used by the WS stale monitor (STALE only applies during active trading
 * sessions, Task 10.3.19) and the market-open badge in the shell.
 */

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
/** Unix epoch (1970-01-01) was a Thursday. */
const WEEK_MS = 7 * DAY;

/** ms offset within the ISO week (Mon 00:00 UTC = 0). */
export function weekOffsetMs(epochMs: number): number {
  const dayMs = ((epochMs % DAY) + DAY) % DAY;
  const dow = new Date(epochMs).getUTCDay(); // 0=Sun … 6=Sat
  const isoDow = (dow + 6) % 7; // 0=Mon … 6=Sun
  return isoDow * DAY + dayMs;
}

// Week anchored at Monday 00:00 UTC:
//   open  = Sunday 21:00 UTC  = end of week − 3h
//   close = Friday 22:00 UTC  = 4 days + 22h
const OPEN_OFFSET_MS = WEEK_MS - 3 * HOUR; // Sun 21:00
const CLOSE_OFFSET_MS = 4 * DAY + 22 * HOUR; // Fri 22:00

export function isFxMarketOpen(epochMs: number): boolean {
  const off = weekOffsetMs(epochMs);
  // Wrap-around: open is near the *end* of the ISO-week offset range.
  return off >= OPEN_OFFSET_MS || off < CLOSE_OFFSET_MS;
}

/** ms until the next market open/close boundary (for timers/badges). */
export function nextFxMarketTransitionMs(epochMs: number): number {
  const off = weekOffsetMs(epochMs);
  if (isFxMarketOpen(epochMs)) {
    return CLOSE_OFFSET_MS - off + (off >= OPEN_OFFSET_MS ? WEEK_MS : 0);
  }
  return OPEN_OFFSET_MS - off;
}
