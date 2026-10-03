/**
 * Position/margin calculator widget (Task 10.3.8) — notional, margin,
 * pip value (JPY-aware), and a maintenance-model liquidation estimate.
 * Embeddable: CalculatorPage mounts it standalone; the order ticket
 * opens it in a modal beside the form.
 *
 * Marks: live `bbo@{symbol}` mid via the shared market feed; when the
 * feed is stale/disconnected the widget keeps computing but badges every
 * mark-derived output "STALE" (Task 10.3.19 honesty rule — never
 * silently compute on a dead feed). All outputs are QUOTE-currency
 * estimates, labelled as such — no fabricated precision.
 */
import { useMemo, useState } from 'react';

import { wsClient } from '@/app/runtime';
import { useWsStatus, type WsClient } from '@/lib/ws';
import { cardCls, inputCls, labelCls, selectCls } from '@/lib/ui';
import { Dec, dec } from '@/lib/decimal/decimal';
import { formatPrice, isJpyPair, priceDecimals } from '@/lib/trading/fx';
import { useMarketFeed, useMidPrice, useChannelHealth } from '@/lib/trading/marketStore';
import { useInstrument, useInstruments } from '@/lib/trading/queries';

import { computePositionCalc, effectiveLeverage, parseCalcField, type CalcSide } from './calc';
import { PnlSection, SwapSection } from './extra-calcs';

function Output({ label, value, hint }: { label: string; value: string; hint?: string }) {
  return (
    <div className="flex items-baseline justify-between py-1.5">
      <span className="text-sm text-neutral-400">{label}</span>
      <span className="text-right">
        <span className="text-sm font-semibold text-neutral-100">{value}</span>
        {hint !== undefined && <span className="ml-1 text-xs text-neutral-500">{hint}</span>}
      </span>
    </div>
  );
}

export function PositionCalculator({
  client = wsClient,
  initialSymbol = 'EUR/USD',
}: {
  client?: WsClient;
  initialSymbol?: string;
}) {
  const instruments = useInstruments();

  const [symbol, setSymbol] = useState(initialSymbol);
  const [side, setSide] = useState<CalcSide>('LONG');
  const [qtyText, setQtyText] = useState('100000');
  const [levText, setLevText] = useState('');
  const [entryText, setEntryText] = useState('');
  const [maintText, setMaintText] = useState('50');

  const instrument = useInstrument(symbol || undefined);
  useMarketFeed(symbol || undefined, client);
  const mid = useMidPrice(symbol || undefined);
  const ws = useWsStatus(client);
  const health = useChannelHealth(ws, `bbo@${symbol}`);
  const markStale = health.stale || ws.state === 'STALE';
  const markLive = mid !== undefined && !markStale;

  const entry = parseCalcField(entryText) ?? mid;
  const requestedLev = parseCalcField(levText) ?? Dec.of(instrument?.maxLeverage ?? 1);
  const leverage = effectiveLeverage(requestedLev, instrument);
  const decimals = priceDecimals(symbol, instrument?.tickSize);

  const result = useMemo(
    () =>
      computePositionCalc({
        symbol,
        side,
        quantity: parseCalcField(qtyText),
        leverage,
        entryPrice: entry,
        maintenanceBps: parseCalcField(maintText) ?? dec('50'),
      }),
    [symbol, side, qtyText, leverage, entry, maintText],
  );

  const markSource = entryText.trim() === '' ? 'live mid' : 'manual';

  return (
    <div>
      <div className="grid gap-6 md:grid-cols-2">
        <div className={cardCls}>
          <div className="mb-4">
            <label htmlFor="calc-symbol" className={labelCls}>
              Pair
            </label>
            <select
              id="calc-symbol"
              value={symbol}
              onChange={(e) => setSymbol(e.target.value)}
              className={selectCls}
            >
              {(instruments.data ?? []).map((i) => (
                <option key={i.symbol} value={i.symbol}>
                  {i.symbol}
                </option>
              ))}
              {instruments.data !== undefined &&
                !instruments.data.some((i) => i.symbol === symbol) && (
                  <option value={symbol}>{symbol}</option>
                )}
            </select>
          </div>

          <div className="mb-4 grid grid-cols-2 gap-2" role="group" aria-label="Position side">
            {(['LONG', 'SHORT'] as const).map((s) => (
              <button
                key={s}
                type="button"
                aria-pressed={side === s}
                onClick={() => setSide(s)}
                className={`rounded py-1.5 text-sm font-semibold focus-visible:ring-2 focus-visible:ring-sky-500 ${
                  side === s
                    ? s === 'LONG'
                      ? 'bg-emerald-600 text-white'
                      : 'bg-red-600 text-white'
                    : 'bg-neutral-800 text-neutral-400 hover:bg-neutral-700'
                }`}
              >
                {s}
              </button>
            ))}
          </div>

          <div className="mb-4">
            <label htmlFor="calc-qty" className={labelCls}>
              Quantity (base units)
            </label>
            <input
              id="calc-qty"
              inputMode="decimal"
              value={qtyText}
              onChange={(e) => setQtyText(e.target.value)}
              className={inputCls}
            />
          </div>

          <div className="mb-4">
            <label htmlFor="calc-lev" className={labelCls}>
              Leverage
            </label>
            <input
              id="calc-lev"
              inputMode="decimal"
              value={levText}
              onChange={(e) => setLevText(e.target.value)}
              placeholder={
                instrument !== undefined
                  ? `account tier — max ${String(instrument.maxLeverage)}:1`
                  : '30'
              }
              aria-describedby="calc-lev-hint"
              className={inputCls}
            />
            <p id="calc-lev-hint" className="mt-1 text-xs text-neutral-500">
              Empty uses the instrument/account cap
              {instrument !== undefined ? ` (${String(instrument.maxLeverage)}:1)` : ''}; the cap
              binds.
            </p>
          </div>

          <div className="mb-4">
            <label htmlFor="calc-entry" className={labelCls}>
              Entry price
            </label>
            <input
              id="calc-entry"
              inputMode="decimal"
              value={entryText}
              onChange={(e) => setEntryText(e.target.value)}
              placeholder={mid ? mid.toFixed(decimals) : 'live mid when connected'}
              aria-describedby="calc-entry-hint"
              className={inputCls}
            />
            <p id="calc-entry-hint" className="mt-1 text-xs text-neutral-500" aria-live="polite">
              {entryText.trim() === ''
                ? mid !== undefined
                  ? markStale
                    ? `using last received mid ${mid.toFixed(decimals)} — feed stale`
                    : `using live mid ${mid.toFixed(decimals)}`
                  : 'waiting for a live mark — enter a price to compute offline'
                : 'manual price override'}
            </p>
          </div>

          <div>
            <label htmlFor="calc-maint" className={labelCls}>
              Maintenance margin (bps, assumed)
            </label>
            <input
              id="calc-maint"
              inputMode="decimal"
              value={maintText}
              onChange={(e) => setMaintText(e.target.value)}
              className={inputCls}
            />
          </div>
        </div>

        <div className={cardCls} aria-live="polite" aria-label="Calculation results">
          <div className="mb-2 flex items-center justify-between">
            <h2 className="text-sm font-semibold text-neutral-200">Results</h2>
            <span
              className={`rounded px-2 py-0.5 text-xs ${
                markLive
                  ? 'bg-emerald-500/15 text-emerald-400'
                  : markStale
                    ? 'bg-amber-500/15 text-amber-400'
                    : 'bg-neutral-700/40 text-neutral-400'
              }`}
              role="status"
            >
              {markLive ? 'live mark' : markStale ? 'STALE mark' : 'mark unavailable'}
            </span>
          </div>

          {result === undefined ? (
            <p className="py-6 text-center text-sm text-neutral-500" role="status">
              Enter a pair, quantity, leverage, and entry price to compute.
            </p>
          ) : (
            <>
              <Output
                label="Notional"
                value={`${result.notional.toDisplay(2)} ${result.quoteCcy}`}
              />
              <Output
                label="Margin required"
                value={`${result.marginRequired.toDisplay(2)} ${result.quoteCcy}`}
                hint={leverage ? `${leverage.toString()}:1` : undefined}
              />
              <Output
                label={`Pip size${isJpyPair(symbol) ? ' (JPY pair)' : ''}`}
                value={result.pipSize.toString()}
              />
              <Output
                label="Pip value"
                value={`${result.pipValueQuote.toDisplay(2)} ${result.quoteCcy} / pip`}
              />
              <Output
                label="Liquidation estimate"
                value={
                  result.liquidation !== undefined
                    ? formatPrice(symbol, result.liquidation, instrument?.tickSize)
                    : 'unavailable'
                }
                hint="est."
              />
              <p className="mt-3 text-xs text-neutral-500">
                Basis:{' '}
                {markSource === 'live mid'
                  ? `mark ${markStale ? 'STALE ' : ''}${entry?.toFixed(decimals) ?? '—'}`
                  : `manual entry ${entry?.toFixed(decimals) ?? '—'}`}{' '}
                · maintenance {maintText}bps · quote-currency amounts — conversion to account
                currency needs the conversion mark. Liquidation is an estimate under the venue
                maintenance-margin model.
              </p>
            </>
          )}
        </div>
      </div>

      <div className="mt-4 grid gap-4 lg:grid-cols-2">
        <PnlSection symbol={symbol} />
        <SwapSection symbol={symbol} instrument={instrument} />
      </div>
    </div>
  );
}
