/**
 * Pro/Lite UI mode store (Task 10.3.9).
 *
 * Semantics:
 *   - `'lite'`: simplified order form + balances + positions —
 *     low-friction surface for T0/T1 retail KYC tiers;
 *   - `'pro'`: full cockpit — advanced ticket, depth, chart overlays,
 *     customizable workspace (T2/institutional default per phase plan);
 *   - `null` (never chosen): AUTO — resolves from the session's KYC
 *     tier each render, so a tier upgrade flips the default without
 *     touching persisted state.
 *
 * Persistence: localStorage via zustand/persist (`exc.ui-mode.v1`).
 * The spec surfaces no account-settings endpoint, so account-level
 * persistence is honest localStorage until one lands — documented in
 * frontend/README.md.
 */
import { create } from 'zustand';
import { persist } from 'zustand/middleware';

import { useSessionStore } from '@/lib/auth/session';

export type UiMode = 'lite' | 'pro';

interface UiModeState {
  /** Explicit user choice; null = auto from KYC tier. */
  mode: UiMode | null;
  setMode: (m: UiMode) => void;
  /** Return to KYC-derived auto mode. */
  clearMode: () => void;
}

export const useUiModeStore = create<UiModeState>()(
  persist(
    (set) => ({
      mode: null,
      setMode: (mode) => set({ mode }),
      clearMode: () => set({ mode: null }),
    }),
    { name: 'exc.ui-mode.v1' },
  ),
);

/** KYC-derived default (phase-plan Task 10.3.9 guidance): T0/T1 → lite,
 * T2 and institutional → pro; unknown tier fails safe to lite. */
export function defaultModeForKyc(kycTier: string | null | undefined): UiMode {
  const t = (kycTier ?? '').toUpperCase();
  return t === 'T2' || t === 'INSTITUTIONAL' ? 'pro' : 'lite';
}

/** Resolved mode — explicit choice wins, else the KYC default. */
export function useUiMode(): UiMode {
  const explicit = useUiModeStore((s) => s.mode);
  const kycTier = useSessionStore((s) => s.user?.kycTier);
  return explicit ?? defaultModeForKyc(kycTier);
}
