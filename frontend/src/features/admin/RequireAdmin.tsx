/**
 * RequireAdmin — the client-side RBAC gate for admin surfaces
 * (Task 10.3.6 item 1; spec §8.2).
 *
 * Reads the decoded admin-role claim from the session (auth/session.ts)
 * with a fallback to `user.roles` for clusters that deliver identity via
 * the login response instead of claims. This is a UX gate ONLY — the
 * server re-checks the binding on every request; panels additionally
 * surface FORBIDDEN/UNAUTHORIZED_ROLE responses as a denial card rather
 * than crashing.
 */
import type { ReactNode } from 'react';
import { Link, Navigate, useLocation } from 'react-router';

import { useSessionStore, type VenueAdminRole } from '@/lib/auth/session';
import { btnPrimary, cardCls } from '@/lib/ui';

import { useAdminRole } from './adminRole';

export function AccessDeniedCard({ detail }: { detail?: string }) {
  return (
    <div className={cardCls} role="alert" data-testid="access-denied">
      <h2 className="mb-1 text-sm font-semibold text-red-300">403 — Access denied</h2>
      <p className="text-sm text-neutral-400">
        {detail ??
          'The server rejected this request for your admin role. The client-side role claim is a hint only — authorization is enforced server-side.'}
      </p>
    </div>
  );
}

export function RequireAdmin({
  children,
  roles,
}: {
  children: ReactNode;
  /** Narrow to specific §8.2 roles (UX only). */
  roles?: VenueAdminRole[];
}) {
  const token = useSessionStore((s) => s.accessToken);
  const adminRole = useAdminRole();
  const loc = useLocation();

  if (token === null) {
    const redirect = encodeURIComponent(loc.pathname + loc.search);
    return <Navigate to={`/login?redirect=${redirect}`} replace />;
  }
  if (adminRole === null || (roles !== undefined && !roles.includes(adminRole))) {
    return (
      <div className="mx-auto max-w-xl p-6">
        <div className={cardCls} role="alert">
          <h1 className="mb-2 text-lg font-semibold">Admin access required</h1>
          <p className="mb-4 text-sm text-neutral-400">
            This console requires an admin role
            {roles !== undefined ? ` (${roles.join(' or ')})` : ''}. Your session does not carry a
            recognized §8.2 role claim.
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
