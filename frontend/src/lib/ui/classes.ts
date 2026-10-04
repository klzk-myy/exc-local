/**
 * Shared Tailwind class tokens for the account cluster (auth / settings /
 * funding / kyc / support) — one place so the surfaces stay visually
 * consistent. Not a design system; extract into components when a pattern
 * repeats a third time.
 */
export const inputCls =
  'w-full min-w-0 rounded border border-neutral-700 bg-neutral-950 px-3 py-2 text-sm text-neutral-100 placeholder:text-neutral-600 focus:border-sky-600 focus:outline-none disabled:opacity-50';

export const selectCls = inputCls;

/** Compact select for dense chrome (toolbars, ticker strips) — no
 * w-full, tight padding. Appending `w-auto`/`py-0.5` to selectCls loses
 * the cascade against its baked-in `w-full`/`py-2`. */
export const selectCompactCls =
  'rounded border border-neutral-700 bg-neutral-950 px-2 py-1 text-xs text-neutral-100 focus:border-sky-600 focus:outline-none disabled:opacity-50';

export const textareaCls = `${inputCls} min-h-28`;

export const cardCls = 'rounded-lg border border-neutral-800 bg-neutral-900 p-4';

export const btnPrimary =
  'rounded bg-sky-700 px-4 py-2 text-sm font-medium text-white hover:bg-sky-600 disabled:cursor-not-allowed disabled:opacity-50';

export const btnDanger =
  'rounded bg-red-700 px-4 py-2 text-sm font-medium text-white hover:bg-red-600 disabled:cursor-not-allowed disabled:opacity-50';

export const btnGhost =
  'rounded border border-neutral-700 px-3 py-1.5 text-sm text-neutral-300 hover:bg-neutral-800 disabled:cursor-not-allowed disabled:opacity-50';

export const labelCls = 'mb-1 block text-sm font-medium text-neutral-300';

export const errorTextCls = 'mt-1 text-xs text-red-400';

export const hintTextCls = 'mt-1 text-xs text-neutral-500';

export const tableCls = 'w-full text-left text-sm';
export const thCls = 'border-b border-neutral-800 px-3 py-2 text-xs font-medium text-neutral-400';
export const tdCls = 'border-b border-neutral-800/60 px-3 py-2 text-neutral-200';
