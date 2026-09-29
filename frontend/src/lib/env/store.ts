/**
 * Environment-context store (Task 10.3.20 item 1).
 *
 * Persisted to sessionStorage — per-tab, deliberately not localStorage:
 * an admin's production context must not silently leak into the next
 * browser session. Entering the production context always requires an
 * explicit confirmation (`requestEnvChange` → `needsConfirm` →
 * `confirmEnvChange`); leaving it is immediate. The store itself is
 * dependency-free so unit tests and the ops feature share the same
 * contract.
 */
import { create } from 'zustand';
import { persist, createJSONStorage } from 'zustand/middleware';

import { normalizeAdminEnv, type AdminEnv } from './env';

interface AdminEnvState {
  /** Active target environment — stamps X-Admin-Env on env-scoped calls. */
  env: AdminEnv;
  /** A pending switch awaiting explicit confirmation (production only). */
  pendingEnv: AdminEnv | null;
  /** Request a context switch. Non-production targets apply immediately;
   * production is staged in `pendingEnv` until `confirmEnvChange`. */
  requestEnvChange: (env: AdminEnv) => void;
  /** Complete the pending switch (no-op when nothing is staged). */
  confirmEnvChange: () => void;
  /** Abandon a staged switch. */
  cancelEnvChange: () => void;
}

export const useAdminEnvStore = create<AdminEnvState>()(
  persist(
    (set) => ({
      env: 'dev',
      pendingEnv: null,
      requestEnvChange: (env) => {
        const target = normalizeAdminEnv(env);
        if (target === 'production') {
          set({ pendingEnv: 'production' });
          return;
        }
        set({ env: target, pendingEnv: null });
      },
      confirmEnvChange: () =>
        set((s) => (s.pendingEnv ? { env: s.pendingEnv, pendingEnv: null } : s)),
      cancelEnvChange: () => set({ pendingEnv: null }),
    }),
    {
      name: 'exchange.admin.env',
      storage: createJSONStorage(() => sessionStorage),
      // Persist only the active env — a staged-but-unconfirmed production
      // switch must never survive a reload as "already confirmed".
      partialize: (s) => ({ env: s.env }),
      // Fail-closed rehydration: a stored-but-unrecognized value resolves
      // to production, matching admin.NormalizeEnv's most-restrictive
      // rule. An ABSENT value is a fresh session, not a corrupt one —
      // keep the `dev` initial so the first production context always
      // goes through explicit confirmation.
      merge: (persisted, current) => {
        if (persisted === undefined) return current;
        return {
          ...current,
          env: normalizeAdminEnv(
            (persisted as Partial<Pick<AdminEnvState, 'env'>> | undefined)?.env,
          ),
        };
      },
    },
  ),
);
