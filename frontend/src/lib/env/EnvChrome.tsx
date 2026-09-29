/**
 * Environment chrome (Task 10.3.20 items 1–3) — the shared pieces every
 * env-scoped surface mounts:
 *
 *   <EnvPill />        persistent context pill (dev green / staging amber
 *                      / production red) — always visible while an admin
 *                      or ops page is mounted
 *   <EnvSwitcher />    the switcher control; selecting production stages
 *                      a confirmation that must be completed explicitly
 *   <EnvWatermark />   the production watermark band — spec §19.16.2
 *                      requires prod context to be unmistakable
 *
 * Context travels on the wire as X-Admin-Env via boundAdminApi; the
 * store (useAdminEnvStore) persists to sessionStorage only — a prod
 * context never silently survives a browser restart.
 */
import { useMemo } from 'react';

import { btnGhost, btnPrimary } from '@/lib/ui';

import { boundAdminApi, useAdminEnvStore, type BoundAdminApi } from './index';
import { ADMIN_ENVS, ENV_PILL, type AdminEnv } from './env';
import type { ApiClient } from '@/lib/api/client';

export function EnvPill() {
  const env = useAdminEnvStore((s) => s.env);
  const p = ENV_PILL[env];
  return (
    <span
      className={`rounded px-2 py-0.5 text-xs font-semibold ${p.className}`}
      data-testid="env-pill"
      title={`Admin environment context — sent as X-Admin-Env: ${env}`}
    >
      {p.label}
    </span>
  );
}

export function EnvSwitcher() {
  const env = useAdminEnvStore((s) => s.env);
  const pendingEnv = useAdminEnvStore((s) => s.pendingEnv);
  const requestEnvChange = useAdminEnvStore((s) => s.requestEnvChange);
  const confirmEnvChange = useAdminEnvStore((s) => s.confirmEnvChange);
  const cancelEnvChange = useAdminEnvStore((s) => s.cancelEnvChange);

  return (
    <div className="flex items-center gap-2">
      <label htmlFor="admin-env" className="text-xs text-neutral-400">
        Env
      </label>
      <select
        id="admin-env"
        aria-label="Admin environment"
        className="rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs text-neutral-100"
        value={env}
        onChange={(e) => {
          requestEnvChange(e.target.value as AdminEnv);
        }}
      >
        {ADMIN_ENVS.map((e) => (
          <option key={e} value={e}>
            {e}
          </option>
        ))}
      </select>
      <EnvPill />
      {pendingEnv === 'production' && (
        <div
          role="alertdialog"
          aria-label="Confirm production context"
          className="flex items-center gap-2 rounded border border-red-800/60 bg-red-950/40 px-3 py-1.5 text-xs text-red-200"
        >
          <span>
            Switch to <strong>production</strong>? Every subsequent admin call is production-scoped
            and sensitive actions require dual control.
          </span>
          <button type="button" className={btnPrimary} onClick={confirmEnvChange}>
            Enter production
          </button>
          <button type="button" className={btnGhost} onClick={cancelEnvChange}>
            Cancel
          </button>
        </div>
      )}
    </div>
  );
}

/** Production watermark — fixed banner pinned to the top of the content
 * area while the session targets production (§19.16.2). */
export function EnvWatermark() {
  const env = useAdminEnvStore((s) => s.env);
  if (env !== 'production') return null;
  return (
    <div
      className="sticky top-0 z-10 border-b border-red-800/60 bg-red-950/60 px-4 py-1 text-center text-xs font-semibold uppercase tracking-widest text-red-200"
      data-testid="prod-watermark"
      role="note"
    >
      Production context — actions are dual-controlled and audit-logged
    </div>
  );
}

/** Hook: the admin API bound to the active env context. A page rendered
 * under env=staging physically cannot issue a production-scoped call —
 * the header is stamped by the bound wrapper, not by request params. */
export function useBoundAdminApi(api: ApiClient): BoundAdminApi {
  const env = useAdminEnvStore((s) => s.env);
  return useMemo(() => boundAdminApi(api, env), [api, env]);
}
