/**
 * Autocomplete & suggestion system (Task 10.3.29 item 5).
 *
 *   - `fuzzyRank` — subsequence/substring scorer for type-ahead lists
 *   - `<Autocomplete>` — generic WAI-ARIA combobox (role=combobox,
 *     aria-expanded, aria-activedescendant, full arrow-key nav)
 *   - `<SymbolAutocomplete>` — instrument search over
 *     GET /api/v1/instruments (pair category + spread display)
 *   - `<AmountPresets>` — 25/50/75/100% quick-select for monetary inputs
 *     (extends Task 10.3.11 sliders to every money form)
 */
import { useId, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';

import { divDecimal, mulDecimal } from './decimal';
import { formatFixed } from './format';
import { useInstruments, type InstrumentMeta } from './instruments';

// ---------------------------------------------------------------------------
// Fuzzy ranking — pure, testable
// ---------------------------------------------------------------------------

export interface Scored<T> {
  item: T;
  score: number;
}

/**
 * Score `query` against `text`: substring match beats subsequence.
 * Returns -1 for no match. Case-insensitive; empty query scores 0
 * (matches everything, caller limits the list).
 */
export function fuzzyScore(query: string, text: string): number {
  const q = query.trim().toLowerCase();
  if (q === '') return 0;
  const t = text.toLowerCase();
  const sub = t.indexOf(q);
  if (sub >= 0) return 1000 - sub - (t.length - q.length) * 0.01;
  // subsequence
  let ti = 0;
  let score = 0;
  for (const ch of q) {
    const found = t.indexOf(ch, ti);
    if (found < 0) return -1;
    score += 10 - Math.min(found - ti, 9);
    ti = found + 1;
  }
  return score;
}

/** Rank items by best fuzzy score across their searchable fields. */
export function fuzzyRank<T>(
  query: string,
  items: readonly T[],
  fields: (item: T) => readonly string[],
  limit = 20,
): T[] {
  const scored: Scored<T>[] = [];
  for (const item of items) {
    let best = -1;
    for (const f of fields(item)) {
      const s = fuzzyScore(query, f);
      if (s > best) best = s;
    }
    if (best >= 0) scored.push({ item, score: best });
  }
  return scored
    .sort((a, b) => b.score - a.score)
    .slice(0, limit)
    .map((s) => s.item);
}

// ---------------------------------------------------------------------------
// Generic ARIA combobox
// ---------------------------------------------------------------------------

export interface AutocompleteProps<T> {
  items: readonly T[];
  value: string;
  onChange: (v: string) => void;
  onSelect: (item: T) => void;
  itemKey: (item: T) => string;
  itemLabel: (item: T) => string;
  renderItem?: (item: T, active: boolean) => ReactNode;
  placeholder?: string;
  label?: string;
  disabled?: boolean;
}

export function Autocomplete<T>({
  items,
  value,
  onChange,
  onSelect,
  itemKey,
  itemLabel,
  renderItem,
  placeholder,
  label,
  disabled,
}: AutocompleteProps<T>) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const listRef = useRef<HTMLUListElement>(null);

  const onKey = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setOpen(true);
      setActive((a) => Math.min(a + 1, items.length - 1));
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      setActive((a) => Math.max(a - 1, 0));
    } else if (e.key === 'Enter') {
      if (open && items.length > 0) {
        e.preventDefault();
        const item = items[Math.min(active, items.length - 1)];
        if (item !== undefined) {
          onSelect(item);
          setOpen(false);
        }
      }
    } else if (e.key === 'Escape') {
      setOpen(false);
    }
  };

  return (
    <div className="relative">
      {label ? (
        <label htmlFor={id} className="mb-1 block text-xs font-medium text-neutral-400">
          {label}
        </label>
      ) : null}
      <input
        id={id}
        role="combobox"
        aria-expanded={open}
        aria-controls={`${id}-listbox`}
        aria-activedescendant={open ? `${id}-opt-${active}` : undefined}
        aria-autocomplete="list"
        autoComplete="off"
        disabled={disabled}
        value={value}
        placeholder={placeholder}
        onChange={(e) => {
          onChange(e.target.value);
          setOpen(true);
          setActive(0);
        }}
        onFocus={() => {
          setOpen(true);
        }}
        onBlur={() => {
          // Delay so mousedown on an option lands before close.
          setTimeout(() => {
            setOpen(false);
          }, 120);
        }}
        onKeyDown={onKey}
        className="w-full rounded border border-neutral-700 bg-neutral-900 px-3 py-1.5 text-sm text-neutral-100 outline-none focus:border-sky-600 disabled:opacity-50"
      />
      {open && items.length > 0 && (
        <ul
          ref={listRef}
          id={`${id}-listbox`}
          role="listbox"
          className="absolute z-40 mt-1 max-h-56 w-full overflow-auto rounded border border-neutral-700 bg-neutral-900 shadow-lg"
        >
          {items.map((item, i) => (
            <li
              key={itemKey(item)}
              id={`${id}-opt-${i}`}
              role="option"
              aria-selected={i === active}
              className={`cursor-pointer px-3 py-1.5 text-sm ${
                i === active ? 'bg-neutral-800 text-white' : 'text-neutral-300'
              }`}
              onMouseDown={(e) => {
                e.preventDefault();
                onSelect(item);
                setOpen(false);
              }}
              onMouseEnter={() => {
                setActive(i);
              }}
            >
              {renderItem ? renderItem(item, i === active) : itemLabel(item)}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// ---------------------------------------------------------------------------
// Symbol search (instruments)
// ---------------------------------------------------------------------------

const PAIR_CATEGORY: Record<string, 'major' | 'minor' | 'exotic'> = {
  'EUR/USD': 'major',
  'USD/JPY': 'major',
  'GBP/USD': 'major',
  'USD/CHF': 'major',
  'USD/CAD': 'major',
  'AUD/USD': 'major',
  'NZD/USD': 'major',
};
const MINOR_QUOTES = new Set(['EUR', 'GBP', 'JPY']);

export function pairCategory(meta: InstrumentMeta): 'major' | 'minor' | 'exotic' {
  const known = PAIR_CATEGORY[meta.symbol];
  if (known !== undefined) return known;
  if (MINOR_QUOTES.has(meta.base) || MINOR_QUOTES.has(meta.quote)) return 'minor';
  return 'exotic';
}

export function SymbolAutocomplete({
  value,
  onChange,
  onSelect,
  label = 'Symbol',
  placeholder = 'Search pairs (e.g. EUR/USD)',
}: {
  value: string;
  /** Raw text updates while the user types (drives the ranking). */
  onChange: (v: string) => void;
  onSelect: (symbol: string, meta: InstrumentMeta) => void;
  label?: string;
  placeholder?: string;
}) {
  const { list } = useInstruments();
  const items = useMemo(
    () => fuzzyRank(value, list, (m) => [m.symbol, m.symbol.replace('/', ''), m.base, m.quote], 10),
    [value, list],
  );
  return (
    <Autocomplete
      items={items}
      value={value}
      onChange={onChange}
      onSelect={(m) => {
        onSelect(m.symbol, m);
      }}
      itemKey={(m) => m.symbol}
      itemLabel={(m) => m.symbol}
      label={label}
      placeholder={placeholder}
      renderItem={(m) => (
        <span className="flex items-center justify-between">
          <span className="font-mono">{m.symbol}</span>
          <span className="ml-2 text-xs text-neutral-500">{pairCategory(m)}</span>
        </span>
      )}
    />
  );
}

// ---------------------------------------------------------------------------
// Amount presets — 25/50/75/100% of a balance (fixed-point)
// ---------------------------------------------------------------------------

export const AMOUNT_PRESETS = ['25', '50', '75', '100'] as const;

export function presetAmount(available: string, pct: string): string | null {
  const share = divDecimal(pct, '100', 8);
  if (share === null) return null;
  return mulDecimal(available, share);
}

export function AmountPresets({
  available,
  onPick,
  decimals = 8,
}: {
  available: string;
  onPick: (amount: string) => void;
  decimals?: number;
}) {
  return (
    <div className="flex gap-1" role="group" aria-label="Amount presets">
      {AMOUNT_PRESETS.map((pct) => {
        const amt = presetAmount(available, pct);
        return (
          <button
            key={pct}
            type="button"
            disabled={amt === null}
            onClick={() => {
              if (amt !== null) onPick(amt);
            }}
            className="rounded border border-neutral-700 px-2 py-0.5 text-xs text-neutral-300 hover:bg-neutral-800 disabled:opacity-40"
          >
            {pct}%
          </button>
        );
      })}
      <span className="sr-only">of available {formatFixed(available, decimals)}</span>
    </div>
  );
}
