/**
 * Position/margin calculator math (Task 10.3.8) — pure Dec functions so
 * every output is unit-testable and no money math ever touches JS floats.
 *
 * Conventions (spec §6.3 / Phase-03 Task 3.3.12):
 *   - pair grammar `BASE/QUOTE`; notional and margin are QUOTE-currency
 *     amounts — conversion to account currency needs a conversion mark
 *     the UI discloses as a basis caveat;
 *   - pip size: 0.01 for JPY-quoted pairs, else 0.0001;
 *   - pip value = qty × pipSize (quote ccy per pip);
 *   - margin required = notional ÷ effective leverage;
 *   - liquidation estimate = entry ∓ (margin − maintRate·notional)/qty —
 *     an ESTIMATE under the venue's maintenance-margin model, always
 *     labelled as such by callers.
 */
import { Dec, tryDec } from '@/lib/decimal/decimal';
import { liquidationEstimate, pipSize, splitPair, type Pair } from '@/lib/trading/fx';
import type { Instrument } from '@/lib/trading/types';

export type CalcSide = 'LONG' | 'SHORT';

export interface CalcInputs {
  symbol: string;
  side: CalcSide;
  /** Position size in base units. */
  quantity: Dec | undefined;
  /** Effective account leverage (≤ instrument max). */
  leverage: Dec | undefined;
  /** Entry price — live mark or user override. */
  entryPrice: Dec | undefined;
  /** Maintenance-margin assumption in basis points (default 50). */
  maintenanceBps: Dec;
}

export interface CalcResult {
  pair: Pair;
  /** qty × entry — quote currency. */
  notional: Dec;
  /** notional ÷ leverage — quote currency. */
  marginRequired: Dec;
  /** Canonical pip size for the pair (0.01 JPY-quoted, else 0.0001). */
  pipSize: Dec;
  /** Quote-currency value per pip for the position size. */
  pipValueQuote: Dec;
  /** Maintenance-model liquidation estimate, or undefined when it
   * can't be computed (never fabricated). */
  liquidation: Dec | undefined;
  /** Per-pip label for output, e.g. "USD per pip". */
  quoteCcy: string;
}

/** Returns undefined until every required input parses positive —
 * the page renders an "awaiting inputs" state rather than zeros. */
export function computePositionCalc(inputs: CalcInputs): CalcResult | undefined {
  const pair = splitPair(inputs.symbol);
  if (!pair) return undefined;
  const { quantity, leverage, entryPrice } = inputs;
  if (!quantity?.isPositive()) return undefined;
  if (!leverage?.isPositive()) return undefined;
  if (!entryPrice?.isPositive()) return undefined;

  const notional = quantity.mul(entryPrice);
  const marginRequired = notional.div(leverage);
  const ps = pipSize(inputs.symbol);
  const liquidation = liquidationEstimate(
    inputs.side,
    entryPrice,
    quantity,
    marginRequired,
    inputs.maintenanceBps,
  );
  return {
    pair,
    notional,
    marginRequired,
    pipSize: ps,
    pipValueQuote: quantity.mul(ps),
    liquidation,
    quoteCcy: pair.quote,
  };
}

/** Effective leverage for the calculator: the account-tier input clamped
 * to the instrument cap (tiered-leverage bands land in Phase-19 Task
 * 19.3.17 — the cap is the honest bound until then). */
export function effectiveLeverage(
  requested: Dec | undefined,
  instrument: Instrument | undefined,
): Dec | undefined {
  if (!requested?.isPositive()) return undefined;
  const cap = Dec.of(instrument?.maxLeverage ?? 0);
  if (cap.isPositive() && requested.gt(cap)) return cap;
  return requested;
}

/** Lenient numeric-field parser shared with the page. */
export function parseCalcField(v: string): Dec | undefined {
  return v.trim() === '' ? undefined : tryDec(v);
}
