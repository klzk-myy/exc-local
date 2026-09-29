/**
 * Sub-account scope store (Task 10.3.7).
 *
 * The selected account re-scopes every TanStack Query cache key via
 * `scopeKey()` — switching sub-accounts refetches balances/positions/
 * orders under a fresh key, so data from the previous scope can never
 * bleed into the new view.
 *
 * The REST contract offers no on-behalf-of header: scoped requests carry
 * `?account_id=<sub>` and the gateway returns FORBIDDEN until the owning
 * backend phase lands the authorization rule — surfaced honestly by the
 * UI rather than fabricating a sub-account view from master data.
 */
import { create } from 'zustand';
import { persist } from 'zustand/middleware';

export const MASTER_ACCOUNT_KEY = 'master';

interface AccountScopeState {
  /** Selected scope — `'master'` or a sub-account id string. */
  scopeKey: string;
  /** Display label resolved by the switcher (e.g. 'Sub #42'). */
  scopeLabel: string;
  setScope: (key: string, label: string) => void;
}

export const useAccountScope = create<AccountScopeState>()(
  persist(
    (set) => ({
      scopeKey: MASTER_ACCOUNT_KEY,
      scopeLabel: 'Master account',
      setScope: (key, label) => set({ scopeKey: key, scopeLabel: label }),
    }),
    { name: 'exc.account-scope.v1' },
  ),
);

/** Numeric account_id for REST scoping; 0 = master (param omitted). */
export function scopeAccountId(scopeKey: string): number {
  if (scopeKey === MASTER_ACCOUNT_KEY) return 0;
  const n = Number(scopeKey);
  return Number.isFinite(n) && n > 0 ? n : 0;
}
