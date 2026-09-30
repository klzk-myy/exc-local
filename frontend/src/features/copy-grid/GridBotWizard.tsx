/**
 * Grid-bot wizard (Task 10.3.26 item 3) — pair / lower-upper bounds /
 * grid count / order type / per-grid qty / total investment / SL / TP.
 *
 * Preview is the deterministic client-side derivation (`deriveGrid`):
 * spacing, per-grid quote/base, fee estimate at the account's taker bps
 * when known. The live 7-day backtest lands with the Phase-16 engine —
 * shown honestly as pending, never fabricated.
 *
 * Enablement is HIGH-severity (typed-phrase) per the task text; max 5
 * concurrent bots per account is enforced client-side against the live
 * list (server enforces authoritatively when Phase-16 lands).
 */
import { useMemo, useState } from 'react';
import { useMutation } from '@tanstack/react-query';

import { apiClient } from '@/app/runtime';
import { ErrorBox, btnPrimary, inputCls, labelCls, selectCls } from '@/lib/ui';
import {
  CONFIRM_PHRASES,
  ConfirmModal,
  SymbolAutocomplete,
  deriveGrid,
  formatFixed,
  isNotImplemented,
  metaFor,
  useInstruments,
  useValidatedField,
} from '@/lib/input-helpers';
import { RULE_PRICE, RULE_SYMBOL, type FieldRule } from '@/lib/input-helpers/validation';

import { createGridBot } from './api';

export const MAX_CONCURRENT_GRID_BOTS = 5;

const GRID_ORDER_TYPES = ['LIMIT', 'MARKET'] as const;
const intRule = (min: number, max: number, name: string): FieldRule => ({
  name,
  kind: 'integer',
  required: true,
  minInt: min,
  maxInt: max,
});
const positiveDecimal = (name: string): FieldRule => ({
  name,
  kind: 'decimal',
  required: true,
  positive: true,
});
const optionalDecimal = (name: string): FieldRule => ({ name, kind: 'decimal', positive: true });

export function GridBotWizard({
  activeBots,
  feeBps,
  onCreated,
}: {
  /** Live bot count for the max-5 guard — undefined while the list
   * endpoint is stubbed (guard degrades to a server-side check). */
  activeBots: number | undefined;
  /** Taker fee bps for the estimate; null renders "fee n/a". */
  feeBps: string | null;
  onCreated?: () => void;
}) {
  const { instruments } = useInstruments();
  const symbol = useValidatedField(RULE_SYMBOL);
  const lower = useValidatedField(RULE_PRICE);
  const upper = useValidatedField(RULE_PRICE);
  const gridCount = useValidatedField(intRule(2, 200, 'grid_count'));
  const perGridQty = useValidatedField(optionalDecimal('per_grid_qty'));
  const investment = useValidatedField(positiveDecimal('total_investment'));
  const stopLoss = useValidatedField(optionalDecimal('stop_loss'));
  const takeProfit = useValidatedField(optionalDecimal('take_profit'));
  const [orderType, setOrderType] = useState<(typeof GRID_ORDER_TYPES)[number]>('LIMIT');
  const [riskAccepted, setRiskAccepted] = useState(false);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [serverErr, setServerErr] = useState<unknown>(null);

  const meta = metaFor(instruments, symbol.value);
  const count = Number(gridCount.value);
  const derivation = useMemo(() => {
    if (!lower.valid || !upper.valid || !Number.isInteger(count) || !investment.valid) return null;
    return deriveGrid(lower.value, upper.value, count, investment.value, feeBps);
  }, [
    lower.valid,
    lower.value,
    upper.valid,
    upper.value,
    count,
    investment.valid,
    investment.value,
    feeBps,
  ]);

  const boundsInverted = derivation?.gridStep?.startsWith('-') === true;
  const atBotCap = activeBots !== undefined && activeBots >= MAX_CONCURRENT_GRID_BOTS;

  const create = useMutation({
    mutationFn: async () =>
      createGridBot(apiClient, {
        symbol: symbol.value.toUpperCase(),
        lower_price: lower.value,
        upper_price: upper.value,
        grid_count: count,
        order_type: orderType,
        ...(perGridQty.value !== '' ? { per_grid_qty: perGridQty.value } : {}),
        total_investment: investment.value,
        ...(stopLoss.value !== '' ? { stop_loss: stopLoss.value } : {}),
        ...(takeProfit.value !== '' ? { take_profit: takeProfit.value } : {}),
      }),
    onError: setServerErr,
    onSuccess: () => {
      setConfirmOpen(false);
      setServerErr(null);
      onCreated?.();
    },
  });

  const allValid =
    symbol.valid &&
    lower.valid &&
    upper.valid &&
    gridCount.valid &&
    perGridQty.valid &&
    investment.valid &&
    stopLoss.valid &&
    takeProfit.valid &&
    !boundsInverted;

  const fld = (label: string, f: ReturnType<typeof useValidatedField>, hint?: string) => {
    // Stable control id — keeps label association explicit (implicit-only
    // labels are fragile under axe's DOM label resolution).
    const id = `gb-${label.toLowerCase().replace(/[^a-z0-9]+/g, '-')}`;
    return (
      <div>
        <label className={labelCls} htmlFor={id}>
          {label}
        </label>
        <input id={id} className={inputCls} inputMode="decimal" {...f.inputProps} />
        {f.error ? <p className="mt-1 text-xs text-red-400">{f.error}</p> : null}
        {!f.error && hint ? <p className="mt-1 text-xs text-neutral-500">{hint}</p> : null}
      </div>
    );
  };

  return (
    <section
      aria-label="Grid bot wizard"
      className="rounded-lg border border-neutral-800 bg-neutral-900 p-4"
    >
      <h2 className="mb-3 text-base font-semibold text-neutral-100">New grid bot</h2>
      {atBotCap ? (
        <p className="mb-3 rounded border border-amber-800 bg-amber-950/40 p-2 text-xs text-amber-300">
          Maximum of {MAX_CONCURRENT_GRID_BOTS} concurrent grid bots reached — stop one before
          creating another.
        </p>
      ) : null}
      <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
        <SymbolAutocomplete
          value={symbol.value}
          onChange={symbol.setValue}
          onSelect={(sym) => {
            symbol.setValue(sym);
          }}
          label="Pair"
        />
        <div>
          <label className={labelCls} htmlFor="gb-order-type">
            Order type
          </label>
          <select
            id="gb-order-type"
            className={selectCls}
            value={orderType}
            onChange={(e) => {
              setOrderType(e.target.value as (typeof GRID_ORDER_TYPES)[number]);
            }}
          >
            {GRID_ORDER_TYPES.map((t) => (
              <option key={t}>{t}</option>
            ))}
          </select>
        </div>
        {fld('Lower price', lower, meta ? `tick ${meta.tickSize}` : undefined)}
        {fld('Upper price', upper)}
        {fld('Grid count', gridCount, '2–200 levels')}
        {fld('Total investment', investment, meta ? `quote ${meta.quote}` : undefined)}
        {fld('Per-grid quantity (optional)', perGridQty, 'Derived automatically when empty')}
        {fld('Stop loss (optional)', stopLoss)}
        {fld('Take profit (optional)', takeProfit)}
      </div>

      {boundsInverted ? (
        <p className="mt-2 text-xs text-red-400">Lower bound must be below the upper bound.</p>
      ) : null}

      {/* deterministic preview */}
      {derivation ? (
        <dl className="mt-4 grid grid-cols-2 gap-2 rounded border border-neutral-800 bg-neutral-950/60 p-3 text-sm md:grid-cols-4">
          <div>
            <dt className="text-xs text-neutral-500">Grid spacing</dt>
            <dd className="font-mono text-neutral-200">
              {derivation.gridStep !== null ? formatFixed(derivation.gridStep, 8) : '—'}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-neutral-500">Per-grid quote</dt>
            <dd className="font-mono text-neutral-200">
              {derivation.perGridQuote !== null ? formatFixed(derivation.perGridQuote, 8) : '—'}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-neutral-500">Per-grid base (est.)</dt>
            <dd className="font-mono text-neutral-200">
              {derivation.perGridBase !== null ? formatFixed(derivation.perGridBase, 8) : '—'}
            </dd>
          </div>
          <div>
            <dt className="text-xs text-neutral-500">Fee estimate (one side)</dt>
            <dd className="font-mono text-neutral-200">
              {derivation.feeEstimate !== null ? formatFixed(derivation.feeEstimate, 8) : 'fee n/a'}
            </dd>
          </div>
        </dl>
      ) : null}
      <p className="mt-2 text-xs text-neutral-500">
        Backtest: pending — the live 7-day backtest arrives with the Phase-16 grid engine. The
        derivation above is computed locally from your inputs; it is not a performance projection.
      </p>

      <label
        className="mt-3 flex items-start gap-2 text-sm text-neutral-300"
        htmlFor="gb-risk-accept"
      >
        <input
          id="gb-risk-accept"
          type="checkbox"
          checked={riskAccepted}
          onChange={(e) => {
            setRiskAccepted(e.target.checked);
          }}
          className="mt-1"
        />
        <span>I accept the grid-bot risk disclosure.</span>
      </label>

      <div className="mt-3 flex justify-end">
        <button
          type="button"
          className={btnPrimary}
          disabled={!allValid || !riskAccepted || atBotCap}
          onClick={() => {
            setConfirmOpen(true);
          }}
        >
          Create bot
        </button>
      </div>

      <ConfirmModal
        open={confirmOpen}
        severity="HIGH"
        title="Enable grid bot"
        disclosures={['gridBot', 'capitalLoss']}
        requirePhrase={CONFIRM_PHRASES['startGridBot']}
        busy={create.isPending}
        confirmLabel="Start bot"
        onCancel={() => {
          setConfirmOpen(false);
        }}
        onConfirm={() => {
          setServerErr(null);
          create.mutate();
        }}
      >
        <p>
          Deploy <span className="font-mono">{investment.value}</span> across{' '}
          <span className="font-mono">{gridCount.value}</span> grid levels on{' '}
          <span className="font-mono">{symbol.value.toUpperCase()}</span> between{' '}
          <span className="font-mono">{lower.value}</span> and{' '}
          <span className="font-mono">{upper.value}</span>.
        </p>
        {serverErr !== null ? (
          <div className="mt-2">
            {isNotImplemented(serverErr) ? (
              <p className="text-xs text-amber-400">
                Grid-bot creation is registered but not yet live (501 NOT_IMPLEMENTED — Phase-16
                Task 16.3.19). Nothing was created.
              </p>
            ) : (
              <ErrorBox error={serverErr} />
            )}
          </div>
        ) : null}
      </ConfirmModal>
    </section>
  );
}
