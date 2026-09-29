/**
 * Admin environment context (Phase-10 Task 10.3.20; spec §19.16).
 *
 * The admin session targets exactly one environment at a time. On the
 * wire the context travels as the `X-Admin-Env` header (canonical name:
 * `internal/admin/middleware.go EnvHeader`), resolved by the RBAC
 * middleware into the request's environment axis — a binding valid in
 * dev grants nothing in production, and the server enforces it.
 *
 * Vocabulary is the §19.16.1 set: dev | staging | production
 * (admin.NormalizeEnv maps anything unrecognized onto production,
 * fail-closed; we mirror that here — an unknown stored value resolves
 * to production, the most restrictive context).
 */

export const ADMIN_ENVS = ['dev', 'staging', 'production'] as const;
export type AdminEnv = (typeof ADMIN_ENVS)[number];

/** Header carrying the session's target environment (§19.16). */
export const ADMIN_ENV_HEADER = 'X-Admin-Env';

/** Fail-closed normalization: unknown/absent input → 'production'. */
export function normalizeAdminEnv(v: unknown): AdminEnv {
  if (v === 'dev' || v === 'staging' || v === 'production') return v;
  return 'production';
}

/** Promotion direction lattice (§19.16.1): dev → staging → production.
 * Production is a sink; dev is a source stage, never a target. */
export const NEXT_ENV: Partial<Record<AdminEnv, AdminEnv>> = {
  dev: 'staging',
  staging: 'production',
};

/** Pill colors per task text: dev green / staging amber / prod red. */
export const ENV_PILL: Record<AdminEnv, { label: string; className: string }> = {
  dev: { label: 'DEV', className: 'bg-emerald-500/20 text-emerald-400' },
  staging: { label: 'STAGING', className: 'bg-amber-500/20 text-amber-400' },
  production: { label: 'PRODUCTION', className: 'bg-red-500/20 text-red-400' },
};
