#pragma once

// In-memory instrument reference data (Task 2.3.23, spec §5.1/§3.3a/§24 #402).
//
// ALL monetary/price/quantity fields are int64 fixed-point at the canonical
// 10^8 pipette scale — identical to Decimal::SCALE and the FlatBuffers wire
// convention (proto/exchange.fbs). No floating-point representation exists in
// the matching hot path: SBE and FlatBuffers encoders emit these raw integers.
//
// Placement choice: a standalone header (rather than folding the struct into
// Order.hpp) because Instrument is per-book configuration consumed by
// OrderBook, PreTradeChecker (§3.3 checks 6/7/11/12) and later by
// MatchingEngine slippage/spread gates (Tasks 2.3.13/2.3.15) — none of which
// should drag the order node definition in.

#include <cstdint>
#include <type_traits>

#include "utils/Decimal.hpp"
#include "utils/safe_math.hpp"

namespace exch {

// Canonical fixed-point scale: 1 unit of currency/price == 10^8 ticks.
inline constexpr int64_t kFixedScale = Decimal::SCALE;  // 100'000'000

// spec §5.1 instrument_type enum (verbatim order).
enum class InstrumentType : uint8_t { SPOT, FORWARD, SWAP, NDF, OPTION };

// spec §5.1 status enum (verbatim order; CANCEL_ONLY per remediation #14).
enum class InstrumentStatus : uint8_t {
    DRAFT, ACTIVE, CANCEL_ONLY, SUSPENDED, HALTED, RESTRICTED, DELISTED
};

struct Instrument {
    uint64_t instrument_id = 0;
    char symbol[16] = {};            // e.g. "EUR/USD" — fixed buffer, POD only

    InstrumentType type = InstrumentType::SPOT;
    InstrumentStatus status = InstrumentStatus::ACTIVE;
    uint8_t decimal_places = 5;      // 5 majors / 3 JPY pairs (spec §5.1)
    uint8_t settlement_cycle = 1;    // T+0=0, T+1=1, T+2=2 (spec §6.3)

    // Pipette scaling (Task 2.3.23): pip_factor is 10^3 for 3-decimal JPY
    // pairs and 10^1 for 5-decimal pairs, i.e. 10^(8 - pip_decimals - 3).
    // ticks_per_pip() == pip_factor * 1000 exactly — spec formula.
    int64_t pip_factor = 10;
    int64_t pip_size_ticks = 10'000; // == pip_factor * 1000; asserted by is_valid()
    int64_t tick_size_ticks = 1'000; // min price increment in 10^8 ticks (0.00001)
    int64_t lot_size_units = 0;      // min quantity step in 10^8 units
    int64_t min_order_qty_units = 0; // spec §5.1 min_order_qty
    int64_t max_order_qty_units = 0; // spec §5.1 max_order_qty
    int64_t min_notional_units = 0;  // quote-ccy notional floor (§3.3 #12)
    // Price-band percentages stored at the DECIMAL(5,2) scale: percent*100,
    // so 200 == 2.00%. (spec §5.1 price_band_pct_up/down, §3.3 check #6)
    int64_t price_band_pct_up = 200;   // default 2.00%
    int64_t price_band_pct_down = 500; // default 5.00%
    int64_t max_spread_pips = 0;       // §6.6 wide-spread protection band
    int64_t max_slippage_bps = 0;      // §6.6a market-order protection (bps)
    // ICEBERG visible-slice ratio, percent*100 (1000 == 10.00%, spec §3.2 #6).
    int64_t display_ratio = 0;
    int64_t contract_size_units = 0;   // spec §5.1 contract_size (per lot)
    int64_t last_price_ticks = 0;      // band reference price (§3.3 check #6)
};

static_assert(std::is_aggregate_v<Instrument>);
static_assert(std::is_trivially_copyable_v<Instrument>);
static_assert(std::is_standard_layout_v<Instrument>);

// --- Pipette conversions (pure integer; spec Task 2.3.23 formula) -----------

// Ticks per one pip — spec literal: instrument.pip_factor * 1000.
[[nodiscard]] constexpr int64_t ticks_per_pip(const Instrument& i) noexcept {
    return i.pip_factor * 1000;
}
// Ticks per 0.1 pip (one pipette): 5-dec pair → 1'000 ticks (0.00001),
// 3-dec JPY pair → 100'000 ticks (0.001).
[[nodiscard]] constexpr int64_t ticks_per_pipette(const Instrument& i) noexcept {
    return i.pip_factor * 100;
}

// Spread in whole pips — spec formula verbatim:
//   spread_pips = (best_ask_ticks - best_bid_ticks) / (pip_factor * 1000).
// Integer division truncates the pipette remainder (spec-literal behavior).
// Fail-closed: an unconfigured pip_factor (<= 0) reports INT64_MAX — a
// degenerate instrument reads as infinitely wide rather than infinitely tight.
[[nodiscard]] constexpr int64_t spread_pips(int64_t best_ask_ticks,
                                            int64_t best_bid_ticks,
                                            const Instrument& i) noexcept {
    if (i.pip_factor <= 0) return INT64_MAX;
    return (best_ask_ticks - best_bid_ticks) / (i.pip_factor * 1000);
}
// Spread in pipettes (0.1 pip granularity) — exact, no rounding.
[[nodiscard]] constexpr int64_t spread_pipettes(int64_t best_ask_ticks,
                                                int64_t best_bid_ticks,
                                                const Instrument& i) noexcept {
    if (i.pip_factor <= 0) return INT64_MAX;
    return (best_ask_ticks - best_bid_ticks) / (i.pip_factor * 100);
}

[[nodiscard]] constexpr int64_t pips_to_ticks(int64_t pips,
                                              const Instrument& i) noexcept {
    return pips * ticks_per_pip(i);
}
[[nodiscard]] constexpr int64_t pipettes_to_ticks(int64_t pipettes,
                                                  const Instrument& i) noexcept {
    return pipettes * ticks_per_pipette(i);
}

// §3.3 check #11 — tick/lot quantization. A zero tick/lot is "unconfigured":
// fail-closed → not a multiple.
[[nodiscard]] constexpr bool is_tick_multiple(int64_t price_ticks,
                                              const Instrument& i) noexcept {
    return i.tick_size_ticks > 0 && price_ticks % i.tick_size_ticks == 0;
}
[[nodiscard]] constexpr bool is_lot_multiple(int64_t qty_units,
                                             const Instrument& i) noexcept {
    return i.lot_size_units > 0 && qty_units % i.lot_size_units == 0;
}

// Reference-data sanity gate: internally consistent pipette chain.
[[nodiscard]] constexpr bool is_valid(const Instrument& i) noexcept {
    return i.pip_factor > 0 &&
           i.pip_size_ticks == ticks_per_pip(i) &&
           i.tick_size_ticks > 0 &&
           i.pip_size_ticks % i.tick_size_ticks == 0 &&
           i.lot_size_units > 0 &&
           i.min_order_qty_units >= 0 &&
           i.price_band_pct_up >= 0 && i.price_band_pct_down >= 0 &&
           i.price_band_pct_down <= 10'000;   // band can't exceed 100.00%
}

// --- Checked helpers (widening 128-bit intermediates, spec §3.6.2) ----------
// Cold/boundary math for the risk checks that own the rejection codes; all
// return false on overflow so callers abort the mutation fail-closed.

// Order notional in 10^8 quote-currency units: qty_units * price_ticks / 1e8.
[[nodiscard]] inline bool notional_units(int64_t qty_units, int64_t price_ticks,
                                         int64_t& out) noexcept {
    const safe_math::int128_t w =
        safe_math::mul_wide_i64(qty_units, price_ticks) / kFixedScale;
    return safe_math::try_narrow_i128(w, out);
}

// §3.3 check #6 price band around a reference price: lo = ref*(1-down%),
// hi = ref*(1+up%) with pct fields at percent*100 scale over a 10'000 base.
[[nodiscard]] inline bool price_band_bounds(const Instrument& i,
                                            int64_t ref_ticks,
                                            int64_t& lo_ticks,
                                            int64_t& hi_ticks) noexcept {
    const safe_math::int128_t up_w =
        safe_math::mul_wide_i64(ref_ticks, 10'000 + i.price_band_pct_up) / 10'000;
    const safe_math::int128_t dn_w =
        safe_math::mul_wide_i64(ref_ticks, 10'000 - i.price_band_pct_down) / 10'000;
    return safe_math::try_narrow_i128(dn_w, lo_ticks) &&
           safe_math::try_narrow_i128(up_w, hi_ticks);
}

// ICEBERG visible slice: qty_units * display_ratio / 10'000 (ratio at
// percent*100 scale). Callers clamp to lot granularity.
[[nodiscard]] inline bool iceberg_visible_units(const Instrument& i,
                                                int64_t qty_units,
                                                int64_t& out) noexcept {
    const safe_math::int128_t w =
        safe_math::mul_wide_i64(qty_units, i.display_ratio) / 10'000;
    return safe_math::try_narrow_i128(w, out);
}

}  // namespace exch
