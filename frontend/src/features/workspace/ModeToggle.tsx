/**
 * PRO | LITE segmented toggle (Task 10.3.9) — persists an explicit
 * choice to `exc.ui-mode.v1`; "(auto)" state announces the KYC-derived
 * default. Rendered in the Workspace header — the shared AppShell is
 * not editable per feature-wiring rules, so the page header is the
 * toggle's accessible home.
 */
import { useUiModeStore, defaultModeForKyc, type UiMode } from './liteMode';
import { useSessionStore } from '@/lib/auth/session';

export function ModeToggle() {
  const explicit = useUiModeStore((s) => s.mode);
  const setMode = useUiModeStore((s) => s.setMode);
  const kycTier = useSessionStore((s) => s.user?.kycTier);
  const resolved = explicit ?? defaultModeForKyc(kycTier);

  return (
    <div
      role="group"
      aria-label={`Interface mode — currently ${resolved}${explicit === null ? ' (auto from KYC tier)' : ''}`}
      className="flex rounded border border-neutral-700 bg-neutral-900 p-0.5"
    >
      {(['lite', 'pro'] as const satisfies readonly UiMode[]).map((m) => (
        <button
          key={m}
          type="button"
          aria-pressed={resolved === m}
          onClick={() => setMode(m)}
          className={`rounded px-3 py-1 text-xs font-semibold uppercase tracking-wide focus-visible:ring-2 focus-visible:ring-sky-500 ${
            resolved === m ? 'bg-sky-600 text-white' : 'text-neutral-400 hover:text-neutral-200'
          }`}
        >
          {m}
          {explicit === null && resolved === m && (
            <span className="ml-1 font-normal normal-case opacity-70">(auto)</span>
          )}
        </button>
      ))}
    </div>
  );
}
