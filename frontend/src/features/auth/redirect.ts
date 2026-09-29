/**
 * Redirect-target sanitization for the auth flow — kept out of guards.tsx
 * so that module stays component-only for react-refresh.
 */

/** Sanitize a redirect target to a same-origin path (no `//evil.com`,
 * no protocol-relative, no javascript: scheme tricks). */
export function safeRedirectTarget(raw: string | null): string {
  if (raw === null || !raw.startsWith('/') || raw.startsWith('//')) return '/';
  return raw;
}
