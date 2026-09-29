/**
 * Admin-role helpers for RequireAdmin + admin panels — kept out of the
 * component module so react-refresh sees component-only exports there.
 */
import { useSessionStore, ADMIN_ROLES, type VenueAdminRole } from '@/lib/auth/session';
import { ApiError } from '@/lib/api';

const ADMIN_ROLE_SET: ReadonlySet<string> = new Set(ADMIN_ROLES);

/** Effective admin role for the session: the dedicated `admin_role`
 * claim wins; otherwise the first §8.2 role found on the user snapshot.
 * Returns null for traders — unknown/missing roles hide admin surfaces. */
export function useAdminRole(): VenueAdminRole | null {
  const claimRole = useSessionStore((s) => s.adminRole);
  const userRoles = useSessionStore((s) => s.user?.roles);
  if (claimRole !== null) return claimRole;
  const found = (userRoles ?? []).find((r) => ADMIN_ROLE_SET.has(r));
  return (found as VenueAdminRole | undefined) ?? null;
}

/** True when an error is the server's authorization denial — the admin
 * surface renders the denial card instead of the panel. */
export function isAccessDenied(err: unknown): boolean {
  return (
    err instanceof ApiError &&
    (err.status === 403 || err.code === 'UNAUTHORIZED_ROLE' || err.code === 'FORBIDDEN')
  );
}
