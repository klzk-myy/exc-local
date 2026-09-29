/**
 * Instrument metadata — the reference-data layer for instrument-aware
 * formatting/validation (Task 10.3.29 item 2).
 *
 * Source: `GET /api/v1/instruments` (live, public — marketapi
 * `instrumentDoc`). Decimals arrive as strings per §5.3.
 *
 * `decimal_places`/`pip_size`/`contract_size` columns land with migration
 * 087 (Phase-15 Task 15.3.11) — until then `InstrumentMeta` derives them
 * from tick_size + the canonical JPY-quote convention, exactly as the
 * settlement pip calculator does
 * (services/internal/settlement/pip_calculator.go).
 */
import { useQuery } from '@tanstack/react-query';
import { useMemo } from 'react';

import { apiClient } from '@/app/runtime';

import { cmpDecimal, decimalPlaces, isDecimal } from './decimal';

/** Wire shape of one row of GET /api/v1/instruments. */
export interface InstrumentWire {
  symbol: string;
  base_currency: string;
  quote_currency: string;
  instrument_type: string; // SPOT|FORWARD|SWAP|NDF|OPTION
  status: string; // ACTIVE|RESTRICTED|CANCEL_ONLY|SUSPENDED|HALTED|DELISTED|DRAFT
  tick_size: string;
  lot_size: string;
  min_order_qty: string;
  max_order_qty: string;
  min_notional: string;
  min_price: string | null;
  max_price: string | null;
  price_band_pct_up: string;
  price_band_pct_down: string;
  max_spread_pips: string | null;
  max_open_orders: number | null;
  max_algo_orders: number | null;
  max_leverage: number;
  settlement_cycle: number;
  settlement?: string;
}

export interface InstrumentsEnvelope {
  data: InstrumentWire[];
  count: number;
  server_time_ms: number;
}

/** Resolved display/validation metadata for one instrument. */
export interface InstrumentMeta {
  readonly symbol: string;
  readonly base: string;
  readonly quote: string;
  readonly type: string;
  readonly status: string;
  /** Decimal places for price rendering (derived from tick_size;
   *  JPY-quote pairs render 3dp, majors 5dp, per §6.3 convention). */
  readonly pricePrecision: number;
  /** Decimal places for quantity rendering (derived from lot_size). */
  readonly qtyPrecision: number;
  readonly tickSize: string;
  readonly lotSize: string;
  readonly minOrderQty: string;
  readonly maxOrderQty: string;
  readonly minNotional: string;
  readonly minPrice: string | null;
  readonly maxPrice: string | null;
  readonly priceBandPctUp: string;
  readonly priceBandPctDown: string;
  readonly maxLeverage: number;
  readonly settlementCycle: number;
  /** One pip in price terms: pip_size override → decimal_places
   *  convention → JPY-quote fallback (0.01 / 0.0001), mirroring
   *  pip_calculator.go InstrumentSpec.PipSizeValue. */
  readonly pipSize: string;
  /** Base units per 1.0 standard lot (100,000 — StandardLotUnits). */
  readonly contractSize: string;
}

/** FX standard lot — mirrors pip_calculator.go `StandardLotUnits`. */
export const STANDARD_LOT_UNITS = '100000';

function derivePipSize(wire: InstrumentWire): string {
  const spec = wire as InstrumentWire & { pip_size?: string };
  if (typeof spec.pip_size === 'string' && isDecimal(spec.pip_size)) return spec.pip_size;
  if (wire.quote_currency === 'JPY') return '0.01';
  return '0.0001';
}

/** Precision fallback when tick_size is absent/zero: JPY → 3dp,
 *  else 5dp (the §6.3 pipette convention — one decimal beyond the pip). */
function derivePricePrecision(wire: InstrumentWire): number {
  if (isDecimal(wire.tick_size) && cmpDecimal(wire.tick_size, '0') === 1) {
    return decimalPlaces(wire.tick_size);
  }
  return wire.quote_currency === 'JPY' ? 3 : 5;
}

function deriveQtyPrecision(wire: InstrumentWire): number {
  if (isDecimal(wire.lot_size) && cmpDecimal(wire.lot_size, '0') === 1) {
    return decimalPlaces(wire.lot_size);
  }
  return 0;
}

export function toInstrumentMeta(wire: InstrumentWire): InstrumentMeta {
  return {
    symbol: wire.symbol,
    base: wire.base_currency,
    quote: wire.quote_currency,
    type: wire.instrument_type,
    status: wire.status,
    pricePrecision: derivePricePrecision(wire),
    qtyPrecision: deriveQtyPrecision(wire),
    tickSize: wire.tick_size,
    lotSize: wire.lot_size,
    minOrderQty: wire.min_order_qty,
    maxOrderQty: wire.max_order_qty,
    minNotional: wire.min_notional,
    minPrice: wire.min_price ?? null,
    maxPrice: wire.max_price ?? null,
    priceBandPctUp: wire.price_band_pct_up,
    priceBandPctDown: wire.price_band_pct_down,
    maxLeverage: wire.max_leverage,
    settlementCycle: wire.settlement_cycle,
    pipSize: derivePipSize(wire),
    contractSize:
      typeof (wire as InstrumentWire & { contract_size?: string }).contract_size === 'string'
        ? (wire as InstrumentWire & { contract_size: string }).contract_size
        : STANDARD_LOT_UNITS,
  };
}

export type InstrumentIndex = ReadonlyMap<string, InstrumentMeta>;

export function indexInstruments(list: readonly InstrumentWire[]): InstrumentIndex {
  const map = new Map<string, InstrumentMeta>();
  for (const w of list) map.set(w.symbol, toInstrumentMeta(w));
  return map;
}

/** Fetch + index the instrument universe (1min server cache — 60s
 * staleTime matches). Components read `metaFor(index, symbol)`. */
export function useInstruments(): {
  instruments: InstrumentIndex;
  list: InstrumentMeta[];
  isLoading: boolean;
  error: unknown;
} {
  const q = useQuery({
    queryKey: ['input-helpers', 'instruments'],
    queryFn: () => apiClient.get<InstrumentsEnvelope>('/instruments'),
    staleTime: 60_000,
  });
  return useMemo(() => {
    const index = indexInstruments(q.data?.data ?? []);
    return {
      instruments: index,
      list: [...index.values()],
      isLoading: q.isLoading,
      error: q.error,
    };
  }, [q.data, q.isLoading, q.error]);
}

export function metaFor(index: InstrumentIndex, symbol: string): InstrumentMeta | undefined {
  return index.get(symbol.toUpperCase());
}
