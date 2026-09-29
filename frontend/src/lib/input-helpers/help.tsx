/**
 * Contextual help & tooltip system (Task 10.3.29 item 7).
 *
 * Static, compliance-curated registry — never advisory, never a chatbot
 * (per R6 no-phone-desk ruling extension). Fields carry `helpText`,
 * `example`, and an optional glossary term linking to `/help#glossary`.
 */
import { useId, useRef, useState, type ReactNode } from 'react';

// ---------------------------------------------------------------------------
// FX glossary — en-US only (spec §27 R5)
// ---------------------------------------------------------------------------

export interface GlossaryTerm {
  term: string;
  definition: string;
}

export const GLOSSARY: readonly GlossaryTerm[] = [
  {
    term: 'pip',
    definition:
      'The smallest standard price move in an FX pair — 0.0001 for most pairs, 0.01 for JPY-quoted pairs.',
  },
  {
    term: 'pipette',
    definition: 'One tenth of a pip — the fifth decimal on most pairs (third on JPY pairs).',
  },
  { term: 'spread', definition: 'The difference between the best ask and best bid prices.' },
  {
    term: 'swap',
    definition:
      'Overnight financing (Tom-Next rollover) credited or debited for holding a position past the daily cutoff.',
  },
  {
    term: 'NDF',
    definition:
      'Non-deliverable forward — a forward contract settled in a reference currency rather than by delivering the underlying.',
  },
  {
    term: 'value date',
    definition:
      'The date on which an FX trade settles; T+1 for most spot pairs, same-day for USD/CAD and USD/MXN.',
  },
  {
    term: 'lot',
    definition:
      'A standardized trade size — one standard lot is 100,000 units of the base currency.',
  },
  { term: 'tick size', definition: 'The smallest allowed price increment for an instrument.' },
  { term: 'lot size', definition: 'The quantity step an order must be a multiple of.' },
  {
    term: 'time in force',
    definition:
      'How long an order stays active: GTC (until cancelled), IOC, FOK, GTD (until a date), or DAY.',
  },
  {
    term: 'iceberg',
    definition: 'An order that displays only part of its total quantity on the book.',
  },
  {
    term: 'mark price',
    definition:
      'The reference price used for margin and liquidation checks, sourced from external oracles.',
  },
  {
    term: 'open interest',
    definition: 'The total notional of outstanding positions on an instrument.',
  },
  { term: 'leverage', definition: 'The ratio of position notional to required margin.' },
  { term: 'margin', definition: 'Collateral locked to support an open leveraged position.' },
  {
    term: 'liquidation',
    definition: 'Forced closure of a position when margin falls below the maintenance requirement.',
  },
] as const;

// ---------------------------------------------------------------------------
// Field help registry — keyed by canonical wire field name
// ---------------------------------------------------------------------------

export interface FieldHelp {
  helpText: string;
  example?: string;
  glossaryTerm?: string;
}

export const HELP_REGISTRY: Readonly<Record<string, FieldHelp>> = {
  symbol: {
    helpText: 'Currency pair in BASE/QUOTE form.',
    example: 'EUR/USD',
    glossaryTerm: 'tick size',
  },
  side: { helpText: 'BUY goes long the base currency; SELL goes short.', example: 'BUY' },
  type: {
    helpText:
      'Order type — LIMIT rests on the book, MARKET fills immediately at the best available price.',
    example: 'LIMIT',
  },
  quantity: {
    helpText: 'Base-currency amount; must be a multiple of the lot size.',
    glossaryTerm: 'lot',
  },
  quote_quantity: {
    helpText: 'Quote-currency spend for a market order (alternative to quantity).',
    example: '1000',
  },
  price: {
    helpText: 'Limit price — must be a multiple of the tick size.',
    glossaryTerm: 'tick size',
  },
  stop_price: { helpText: 'Trigger price that activates a stop or stop-limit order.' },
  time_in_force: {
    helpText: 'Order lifetime policy.',
    glossaryTerm: 'time in force',
    example: 'GTC',
  },
  gtd_expiry: {
    helpText: 'Expiry timestamp for GTD orders (RFC3339, UTC).',
    example: '2026-12-31T22:00:00Z',
  },
  iceberg_visible_qty: {
    helpText: 'Quantity displayed on the book for an iceberg order.',
    glossaryTerm: 'iceberg',
  },
  countdown_ms: {
    helpText:
      'Dead-man timer: all open orders cancel unless refreshed before expiry (1,000–300,000 ms).',
    example: '60000',
  },
  grid_count: {
    helpText: 'Number of grid levels between lower and upper bounds (5–200).',
    example: '20',
  },
  total_investment: {
    helpText: 'Total quote-currency amount the bot deploys across the grid.',
    example: '10000',
  },
  allocation: { helpText: 'Notional allocated to copying this strategy.', example: '5000' },
  safety_mode: {
    helpText: 'FULL copies at provider size; HALF_RISK scales each copied trade to 50%.',
    example: 'HALF_RISK',
  },
  stop_loss_cap: {
    helpText: 'Follow-level stop: unfollow triggers if provider drawdown breaches this cap.',
  },
};

export function fieldHelp(name: string): FieldHelp | undefined {
  return HELP_REGISTRY[name];
}

// ---------------------------------------------------------------------------
// <HelpTooltip> — focusable, Esc-dismissible, aria-describedby
// ---------------------------------------------------------------------------

export function HelpTooltip({ help, id }: { help: FieldHelp; id?: string }) {
  const autoId = useId();
  const tipId = id ?? `${autoId}-tip`;
  const [open, setOpen] = useState(false);
  const btnRef = useRef<HTMLButtonElement>(null);

  return (
    <span className="relative inline-flex">
      <button
        ref={btnRef}
        type="button"
        aria-label="Field help"
        aria-expanded={open}
        aria-describedby={open ? tipId : undefined}
        onClick={() => {
          setOpen((o) => !o);
        }}
        onKeyDown={(e) => {
          if (e.key === 'Escape') {
            setOpen(false);
            btnRef.current?.focus();
          }
        }}
        className="inline-flex h-4 w-4 items-center justify-center rounded-full border border-neutral-600 text-[10px] text-neutral-400 hover:border-neutral-400 hover:text-neutral-200"
      >
        ?
      </button>
      {open && (
        <span
          id={tipId}
          role="tooltip"
          className="absolute left-5 top-0 z-40 w-64 rounded border border-neutral-700 bg-neutral-900 p-2 text-xs leading-relaxed text-neutral-300 shadow-lg"
        >
          {help.helpText}
          {help.example ? (
            <span className="mt-1 block text-neutral-500">
              Example: <code className="font-mono">{help.example}</code>
            </span>
          ) : null}
          {help.glossaryTerm ? (
            <a
              href={`/help#glossary-${help.glossaryTerm.replace(/\s+/g, '-')}`}
              className="mt-1 block text-sky-400 hover:underline"
            >
              Glossary: {help.glossaryTerm}
            </a>
          ) : null}
        </span>
      )}
    </span>
  );
}

/** Glossary section for the /help page (compliance-curated, static). */
export function GlossaryList(): ReactNode {
  return (
    <dl className="space-y-3">
      {GLOSSARY.map((g) => (
        <div key={g.term} id={`glossary-${g.term.replace(/\s+/g, '-')}`}>
          <dt className="text-sm font-semibold text-neutral-200">{g.term}</dt>
          <dd className="text-sm text-neutral-400">{g.definition}</dd>
        </div>
      ))}
    </dl>
  );
}
