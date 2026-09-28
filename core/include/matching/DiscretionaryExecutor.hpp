#pragma once

// Task 2.3.26 — Discretionary Offset & Fill-and-Store (FAS) band evaluation
// (spec §6.11, §24 #405; migration 103 orders.discretionary_offset_pips).
//
// Semantics (spec §6.11.1): a LIMIT order may carry a hidden
// `discretionary_offset_pips`. The order rests passively at its nominal
// limit_price — the only price public L2/L3 ever shows — but is willing to
// execute aggressively against contra liquidity inside a hidden band:
//
//     BUY : aggressive_eval_price = limit_price + offset_pips * pip_size
//     SELL: aggressive_eval_price = limit_price - offset_pips * pip_size
//
// All prices/quantities are int64 fixed-point at the 10^8 pipette scale
// (Task 2.3.23, spec §3.3a); pip_size is Instrument::pip_size_ticks, an
// integer tick count — zero floating point anywhere in this component.
//
// This is a STANDALONE policy component: it owns only the band math and the
// §6.11.3 eligibility gates. MatchingEngine wiring is the engine owner's
// task (seam contract below); nothing here mutates book or engine state.
//
// ============================================================================
// ENGINE SEAM (owned by the MatchingEngine integrator — documented, not
// implemented here):
//
//   1. Field transport. `OrderNew.discretionary_offset_pips` already rides
//      the wire (proto/exchange.fbs field 15, int64 whole pips; migration
//      103 stores DECIMAL(10,4) — the gateway truncates fractional pips on
//      encode, deterministic). EnginePump decodes it into
//      `OrderAux::discretionary_offset_pips` (a new aux field alongside
//      stop_price_ticks / gtd_expiry_ns — the Order POD has no slot and
//      must not grow one: the resting node carrying only the nominal
//      price_ticks is what makes the band un-leakable). For
//      replay-correct re-derivation the engine owner should persist the
//      offset in WalOrderNewPayload::_pad[8] (first int64 slot) and restore
//      it into OrderAux on replay — same stream => same band => same fills.
//
//   2. Intake seam. In MatchingEngine::on_order_received(), inside the
//      OrderType::LIMIT branch, AFTER the existing `invalid` validation and
//      risk hook but BEFORE walk_match:
//
//          if (aux.discretionary_offset_pips != 0) {
//              const Instrument* inst = book_.instrument();
//              if (inst == nullptr) {           // fail-closed: no pip size
//                  reject(order, DiscretionaryExecutor::kRejectOffsetInvalid);
//                  orders_.free(order); return;
//              }
//              if (const char* c = DiscretionaryExecutor::validate(
//                      *order, aux.discretionary_offset_pips, *inst)) {
//                  reject(order, c);            // DISCRETIONARY_OFFSET_INVALID
//                  orders_.free(order); return;
//              }
//          }
//          const DiscretionaryEval ev = DiscretionaryExecutor::eval(
//              order->side, order->price_ticks,
//              aux.discretionary_offset_pips, *inst-or-sentinel);
//          TakerResult r = walk_match(*order, aux, /*has_limit=*/true,
//                                     ev.aggressive_price_ticks);
//          finish_taker(*order, aux, r, display);
//
//      FOK + offset is already prohibited by validate() (spec §6.11.3), so
//      the FOK feasibility pre-check stays unchanged. The walk bound is the
//      band edge; fills execute at the resting maker's price (price
//      improvement per §6.11.2 — "uncrosses at the resting contra price").
//
//   3. Remainder-at-limit. finish_taker()/rest_remainder() need NO change:
//      they rest `taker.price_ticks`, which stays the nominal limit — the
//      aggressive eval price only parameterizes the walk bound, it is never
//      written into the order node. The stored slice keeps the arrival
//      (timestamp_ns, ingress_seq) — the FAS "no priority reset on the
//      stored slice" guarantee (spec §6.11.4) falls out of the existing
//      remainder path unchanged.
//
//   4. Resting-side band index (spec §6.11.2). A resting discretionary
//      order is also reachable by a contra taker whose limit falls inside
//      the resting order's band even when it does NOT reach the nominal
//      limit. The engine owner maintains a secondary hidden offset index
//      keyed by eval(...).aggressive_price_ticks of each resting
//      discretionary order (stored alongside OrderMeta); during
//      walk_match, after the nominal-level sweep stalls, the index yields
//      resting orders whose band edge satisfies
//      `crosses(taker.side, taker_limit, resting_band_edge)` and they fill
//      at the resting contra price. The index is engine-private state —
//      never serialized into BookSnapshot or the L2/L3 wire schema.
//
//   5. Public-MD hiding. Guaranteed by construction: (a) the resting book
//      node has no offset field — Order POD is unchanged; (b) OrderBook
//      levels/snapshots carry only price_ticks (the nominal limit);
//      (c) BookSerializer emits book levels only. The band edge exists
//      solely inside engine intake evaluation + the private offset index.
//      test_discretionary.cpp asserts a post-sweep snapshot exposes only
//      the nominal limit price.
// ============================================================================

#include <cstdint>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "utils/safe_math.hpp"

namespace exch {

// Verdict of one discretionary-band evaluation. On rejection
// `aggressive_price_ticks` holds the nominal limit (a safe no-band answer)
// and `reject_code` carries the §23-registered code — callers must check
// reject_code first and never walk with a rejected verdict.
struct DiscretionaryEval {
    int64_t aggressive_price_ticks = 0;  // taker walk bound / band edge
    bool has_band = false;               // band extends past nominal limit
    const char* reject_code = nullptr;   // non-null => fail-closed reject
};

class DiscretionaryExecutor {
public:
    // §23-registered rejection code (HTTP 400). Registered in the Phase-05
    // Task 5.3.21 registry — do not spell it differently anywhere else.
    static constexpr char kRejectOffsetInvalid[] =
        "DISCRETIONARY_OFFSET_INVALID";

    // --- Order-shape eligibility gate (spec §6.11.3) -------------------------
    // Returns nullptr when the order may carry `offset_pips`, else
    // kRejectOffsetInvalid. offset_pips == 0 means "no discretionary band"
    // and is always eligible regardless of type/TIF/flags (the gateway may
    // send the field unconditionally; migration 103 DEFAULT 0.0).
    //
    // §6.11.3 rules: LIMIT only, TIF in {GTC, GTD, DAY} — IOC/FOK die at
    // intake anyway so a band on them is meaningless; POST_ONLY explicitly
    // prohibited because a hidden aggression band contradicts the
    // never-take-liquidity contract. Numeric sanity (negative, cap,
    // overflow, degenerate instrument) is delegated to eval() so the
    // arithmetic has exactly one implementation site.
    [[nodiscard]] static const char* validate(
        const Order& o, int64_t offset_pips, const Instrument& i) noexcept {
        if (offset_pips == 0) return nullptr;
        if (offset_pips < 0) return kRejectOffsetInvalid;
        if (o.type != OrderType::LIMIT) return kRejectOffsetInvalid;
        if (o.tif == TimeInForce::IOC || o.tif == TimeInForce::FOK) {
            return kRejectOffsetInvalid;
        }
        if ((o.flags & kOrderFlagPostOnly) != 0) {
            return kRejectOffsetInvalid;
        }
        return eval(o.side, o.price_ticks, offset_pips, i).reject_code;
    }

    // --- Band evaluation (spec §6.11.1–3) ------------------------------------
    // Computes the aggressive walk bound for `side` at `limit_ticks` with
    // a hidden offset of `offset_pips` whole pips.
    //
    // Fail-closed verdicts (reject_code set, aggressive := limit):
    //   * offset_pips < 0                    — negative offsets are the
    //     "NaN-like sentinel" family of an int64 wire field (INT64_MIN
    //     included); rejected before any arithmetic can touch them.
    //   * offset_pips > max_spread_pips * 2  — spec §6.11.3 literal bound.
    //     Read strictly fail-closed: an instrument with max_spread_pips
    //     <= 0 has bound 0, so EVERY positive offset rejects. The feature
    //     is unusable until the wide-spread band is configured — a hidden
    //     aggression range with no configured sanity ceiling is exactly
    //     what §2.7 pessimism forbids. (Instrument owners: set
    //     max_spread_pips to enable discretionary orders.)
    //   * pip_size_ticks <= 0                — unpriced pip: cannot express
    //     the band; never silently treat it as zero.
    //   * checked-mul/add overflow           — would wrap the walk bound;
    //     reject rather than match at a wrapped price.
    //
    // offset_pips == 0 is the passthrough: has_band = false, aggressive =
    // limit — the engine takes the plain LIMIT path and may skip the call.
    //
    // SELL floor: when limit - delta <= 0 the band would reach non-positive
    // prices, which cannot exist on the book (OrderBook rejects
    // price_ticks <= 0). The bound clamps at the instrument tick floor
    // (tick_size_ticks, else 1) — semantically "take any positive bid",
    // the closest legal expression of the trader's intent; a rejection
    // here would punish an otherwise-valid order for book geometry.
    [[nodiscard]] static DiscretionaryEval eval(
        Side side, int64_t limit_ticks, int64_t offset_pips,
        const Instrument& i) noexcept {
        DiscretionaryEval ev{};
        ev.aggressive_price_ticks = limit_ticks;

        // Sentinel/negative family — checked before any multiplication.
        if (offset_pips < 0) {
            ev.reject_code = kRejectOffsetInvalid;
            return ev;
        }
        if (offset_pips == 0) return ev;  // no band — plain limit semantics
        if (i.pip_size_ticks <= 0) {
            ev.reject_code = kRejectOffsetInvalid;
            return ev;
        }

        // §6.11.3 cap: offset <= max_spread_pips * 2. On cap-mul overflow
        // the bound is unreachable (offset is int64) — continue; a
        // negative/zero max_spread_pips yields cap <= 0 -> all positive
        // offsets reject (fail-closed, see docstring).
        int64_t cap_pips = 0;
        if (safe_math::try_mul(i.max_spread_pips, int64_t{2}, cap_pips) &&
            offset_pips > cap_pips) {
            ev.reject_code = kRejectOffsetInvalid;
            return ev;
        }

        // delta = offset_pips * pip_size_ticks — widening-checked, then a
        // checked add/sub against the limit (spec §3.6.2 arithmetic rule).
        int64_t delta_ticks = 0;
        if (!safe_math::try_mul(offset_pips, i.pip_size_ticks,
                                delta_ticks)) {
            ev.reject_code = kRejectOffsetInvalid;
            return ev;
        }

        int64_t edge = limit_ticks;
        if (side == Side::BUY) {
            if (!safe_math::try_add(limit_ticks, delta_ticks, edge)) {
                ev.reject_code = kRejectOffsetInvalid;
                return ev;
            }
        } else {
            if (!safe_math::try_sub(limit_ticks, delta_ticks, edge)) {
                ev.reject_code = kRejectOffsetInvalid;
                return ev;
            }
            const int64_t floor =
                i.tick_size_ticks > 0 ? i.tick_size_ticks : 1;
            if (edge < floor) edge = floor;
        }

        ev.aggressive_price_ticks = edge;
        ev.has_band = edge != limit_ticks;
        return ev;
    }
};

}  // namespace exch
