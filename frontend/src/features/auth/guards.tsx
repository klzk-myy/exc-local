/**
 * Route guards — Task 10.3.21 item 6 (spec §21.4).
 *
 *   <RequireAuth>  — unauthenticated users bounce to /login?redirect=<here>
 *   <RequireRole>  — §8.2 role check against the session's role hints
 *
 * Feature pages wrap their content — the manifest auto-discovery contract
 * (no shared-file edits) means the guard lives inside each page rather
 * than in the router. Pages render inside the AppShell either way.
 */
import type { ReactNode } from 'react';
import { Link, Navigate, useLocation } from 'react-router';

import { useSessionStore } from '@/lib/auth/session';
import { cardCls, btnPrimary } from '@/lib/ui';

export function RequireAuth({ children }: { children: ReactNode }) {
  const token = useSessionStore((s) => s.accessToken);
  const loc = useLocation();
  if (token === null) {
    const redirect = encodeURIComponent(loc.pathname + loc.search);
    return <Navigate to={`/login?redirect=${redirect}`} replace />;
  }
  return <>{children}</>;
}

export function RequireRole({ roles, children }: { roles: string[]; children: ReactNode }) {
  const user = useSessionStore((s) => s.user);
  const granted = user?.roles ?? [];
  if (!roles.some((r) => granted.includes(r))) {
    return (
      <div className="mx-auto max-w-xl p-6">
        <div className={cardCls} role="alert">
          <h1 className="mb-2 text-lg font-semibold">Insufficient permissions</h1>
          <p className="mb-4 text-sm text-neutral-400">
            This area requires one of: {roles.join(', ')}.
          </p>
          <Link to="/" className={btnPrimary}>
            Back to dashboard
          </Link>
        </div>
      </div>
    );
  }
  return <>{children}</>;
}
