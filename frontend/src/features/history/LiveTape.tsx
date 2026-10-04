/**
 * Live market-data tape (Task 10.3.27 item 6).
 *
 * Consumes the canonical Phase-06 channel grammar
 * (services/internal/marketdata/channels.go):
 *   bbo@{symbol} · aggTrades@{symbol} · liquidations@{symbol|all}
 *   openInterest@{symbol} · referencePrice@{symbol}
 *   depth@{symbol}:{levels}:{cadence_ms} — user-configurable over the
 *   supported cross product (5/10/20 × 100/250/1000ms).
 *
 * Every payload is narrowed from `unknown`; unparseable frames are
 * counted and dropped — nothing is rendered from guessed fields.
 */
import { useRef, useState } from 'react';

import { wsClient } from '@/app/runtime';
import { cardCls, selectCls } from '@/lib/ui';
import { tryDec } from '@/lib/decimal/decimal';
import {
  DEPTH_CADENCES_MS,
  DEPTH_LEVELS,
  aggTradesChannel,
  bboChannel,
  depthChannel,
  type DepthCadenceMs,
  type DepthLevels,
} from '@/lib/market/channels';
import { parseBbo, parseDepthUpdate, type Bbo, type BookSnapshot } from '@/lib/trading/types';
import { useChannel, useWsStatus } from '@/lib/ws';
import { SymbolAutocomplete, useInstruments, metaFor, formatPrice } from '@/lib/input-helpers';
import { useValidatedField } from '@/lib/input-helpers';
import { RULE_SYMBOL } from '@/lib/input-helpers/validation';

// ---------------------------------------------------------------------------
// Wire narrowing — only documented/public fields are surfaced
// ---------------------------------------------------------------------------

interface TapeRow {
  seq: number;
  channel: string;
  tsMs: number;
  price: string | null;
  qty: string | null;
  side: string | null;
  symbol: string | null;
  kind: string;
}

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
function s(v: unknown): string | null {
  return typeof v === 'string' ? v : null;
}
function d(v: unknown): string | null {
  return s(v) ?? tryDec(v)?.toString() ?? null;
}

function tapeRow(channel: string, seq: number, tsMs: number, v: unknown): TapeRow | null {
  if (!isRecord(v)) return null;
  return {
    seq,
    channel,
    tsMs,
    price: d(v['price']) ?? d(v['mark_price']) ?? d(v['reference_price']),
    qty: d(v['qty']) ?? d(v['quantity']) ?? d(v['qty_base']),
    side: s(v['side']) ?? s(v['aggressor']),
    symbol: s(v['symbol']),
    kind: channel.split('@')[0] ?? channel,
  };
}

const TAPE_LIMIT = 50;

// ---------------------------------------------------------------------------

export function LiveTape() {
  const symbol = useValidatedField({ ...RULE_SYMBOL, required: false }, 'EUR/USD');
  const { instruments } = useInstruments();
  const meta = metaFor(instruments, symbol.value.toUpperCase());
  const [depthLevels, setDepthLevels] = useState<DepthLevels>(20);
  const [depthCadence, setDepthCadence] = useState<DepthCadenceMs>(100);
  const status = useWsStatus(wsClient);

  const [bbo, setBbo] = useState<Bbo | null>(null);
  const [book, setBook] = useState<BookSnapshot | null>(null);
  const [oi, setOi] = useState<string | null>(null);
  const [refPrice, setRefPrice] = useState<string | null>(null);
  const tape = useRef<TapeRow[]>([]);
  const [, setTapeTick] = useState(0);
  const dropped = useRef(0);

  const sym = symbol.valid && symbol.value !== '' ? symbol.value.toUpperCase() : undefined;
  const push = (row: TapeRow | null) => {
    if (row === null) {
      dropped.current += 1;
      return;
    }
    tape.current = [row, ...tape.current].slice(0, TAPE_LIMIT);
    setTapeTick((t) => t + 1);
  };

  useChannel(wsClient, sym !== undefined ? bboChannel(sym) : null, (f) => {
    setBbo(parseBbo(f.data));
  });
  useChannel(wsClient, sym !== undefined ? aggTradesChannel(sym) : null, (f) => {
    push(tapeRow(f.channel, f.seq, f.ts_ms, f.data));
  });
  useChannel(wsClient, 'liquidations@all', (f) => {
    push(tapeRow(f.channel, f.seq, f.ts_ms, f.data));
  });
  useChannel(wsClient, sym !== undefined ? `openInterest@${sym}` : null, (f) => {
    if (isRecord(f.data)) setOi(d(f.data['open_interest']) ?? d(f.data['oi']));
  });
  useChannel(wsClient, sym !== undefined ? `referencePrice@${sym}` : null, (f) => {
    if (isRecord(f.data)) setRefPrice(d(f.data['price']) ?? d(f.data['reference_price']));
  });
  useChannel(
    wsClient,
    sym !== undefined ? depthChannel(sym, depthLevels, depthCadence) : null,
    (f) => {
      setBook(parseDepthUpdate(f.data));
    },
  );

  const rows = tape.current;
  const live = status.state === 'AUTHENTICATED' || status.state === 'STALE';
  const conn = live ? (status.state === 'STALE' ? 'stale' : 'live') : status.state.toLowerCase();

  return (
    <section aria-label="Live tape" className={cardCls}>
      <div className="flex flex-wrap items-end gap-2">
        <SymbolAutocomplete
          value={symbol.value}
          onChange={symbol.setValue}
          onSelect={(s2) => {
            symbol.setValue(s2);
          }}
          label="Tape symbol"
        />
        <div>
          <label
            className="mb-1 block text-xs font-medium text-neutral-400"
            htmlFor="tape-depth-levels"
          >
            Depth levels
          </label>
          <select
            id="tape-depth-levels"
            className={selectCls + ' w-auto'}
            value={depthLevels}
            onChange={(e) => {
              setDepthLevels(Number(e.target.value) as DepthLevels);
            }}
          >
            {DEPTH_LEVELS.map((l) => (
              <option key={l} value={l}>
                {l}
              </option>
            ))}
          </select>
        </div>
        <div>
          <label className="mb-1 block text-xs font-medium text-neutral-400" htmlFor="tape-cadence">
            Cadence
          </label>
          <select
            id="tape-cadence"
            className={selectCls + ' w-auto'}
            value={depthCadence}
            onChange={(e) => {
              setDepthCadence(Number(e.target.value) as DepthCadenceMs);
            }}
          >
            {DEPTH_CADENCES_MS.map((c) => (
              <option key={c} value={c}>
                {c}ms
              </option>
            ))}
          </select>
        </div>
        <span
          role="status"
          className={`ml-auto rounded px-2 py-1 text-xs ${live ? 'bg-emerald-500/20 text-emerald-400' : 'bg-neutral-800 text-neutral-400'}`}
        >
          {conn}
        </span>
      </div>

      <dl className="mt-3 grid grid-cols-2 gap-2 text-sm md:grid-cols-4">
        <div>
          <dt className="text-xs text-neutral-500">BBO</dt>
          <dd className="font-mono text-neutral-200">
            {bbo
              ? `${bbo.bid ? formatPrice(meta, bbo.bid.toString()) : '—'} / ${bbo.ask ? formatPrice(meta, bbo.ask.toString()) : '—'}`
              : '—'}
          </dd>
        </div>
        <div>
          <dt className="text-xs text-neutral-500">Open interest</dt>
          <dd className="font-mono text-neutral-200">{oi ?? '—'}</dd>
        </div>
        <div>
          <dt className="text-xs text-neutral-500">Reference price</dt>
          <dd className="font-mono text-neutral-200">{refPrice ?? '—'}</dd>
        </div>
        <div>
          <dt className="text-xs text-neutral-500">Book top</dt>
          <dd className="font-mono text-neutral-200">
            {book?.bids[0] && book.asks[0]
              ? `${book.bids[0].price.toString()} / ${book.asks[0].price.toString()}`
              : '—'}
          </dd>
        </div>
      </dl>

      <h3 className="mt-4 text-xs font-semibold uppercase tracking-wide text-neutral-500">
        Trade &amp; liquidation tape
      </h3>
      {rows.length === 0 ? (
        <p className="mt-1 text-sm text-neutral-500">
          Waiting for aggTrades@{sym ?? '…'} / liquidations@all events…
        </p>
      ) : (
        <ul className="mt-1 max-h-64 overflow-y-auto font-mono text-xs" aria-label="Tape" tabIndex={0}>
          {rows.map((r, i) => (
            <li key={`${r.seq}-${i}`} className="flex gap-3 border-b border-neutral-800/40 py-0.5">
              <span className="text-neutral-500">{r.kind}</span>
              <span className="text-neutral-400">{r.symbol ?? ''}</span>
              <span className={r.side === 'SELL' ? 'text-red-400' : 'text-emerald-400'}>
                {r.side ?? '—'}
              </span>
              <span className="text-neutral-200">{r.price ?? '—'}</span>
              <span className="text-neutral-400">{r.qty ?? ''}</span>
              <span className="ml-auto text-neutral-600">
                {r.tsMs > 0 ? new Date(r.tsMs).toLocaleTimeString('en-US', { hour12: false }) : ''}
              </span>
            </li>
          ))}
        </ul>
      )}
      {dropped.current > 0 ? (
        <p className="mt-1 text-xs text-neutral-600">
          {dropped.current} unrecognized frame(s) dropped — payloads are rendered only when they
          parse.
        </p>
      ) : null}
    </section>
  );
}
