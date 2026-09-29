/**
 * Idempotency-key helper for money-moving POSTs (spec §8.8).
 *
 * The `Idempotency-Key` header is REQUIRED on transfer/withdrawal/order
 * mutations (services/internal/middleware/idempotency.go). Keys are
 * account-scoped, deduped for 60s server-side; reuse only for a true
 * retry of the same payload (`IDEMPOTENCY_KEY_MISMATCH` otherwise).
 */
export const IDEMPOTENCY_KEY_HEADER = 'Idempotency-Key';

/** RFC 4122 v4 key. `crypto.randomUUID` covers secure contexts; the
 * getRandomValues fallback keeps plain-http dev origins working. */
export function newIdempotencyKey(): string {
  // DOM types declare `crypto` unconditionally present; insecure contexts
  // can still lack it at runtime — read through an optional-typed view.
  const c: Crypto | undefined = (globalThis as { crypto?: Crypto }).crypto;
  if (c === undefined) {
    // Insecure-context fallback should never fire (the SPA is https-only),
    // but fail loudly rather than mint a weak key silently.
    throw new Error('crypto API unavailable — insecure context');
  }
  if (typeof c.randomUUID === 'function') {
    return c.randomUUID();
  }
  const b = new Uint8Array(16);
  c.getRandomValues(b);
  b[6] = ((b[6] ?? 0) & 0x0f) | 0x40;
  b[8] = ((b[8] ?? 0) & 0x3f) | 0x80;
  const hex = [...b].map((x) => x.toString(16).padStart(2, '0'));
  return `${hex.slice(0, 4).join('')}-${hex.slice(4, 6).join('')}-${hex
    .slice(6, 8)
    .join('')}-${hex.slice(8, 10).join('')}-${hex.slice(10).join('')}`;
}

/** Merge an idempotency key into a header bag. */
export function withIdempotencyKey(
  headers: Record<string, string> = {},
  key: string = newIdempotencyKey(),
): Record<string, string> {
  return { ...headers, [IDEMPOTENCY_KEY_HEADER]: key };
}
