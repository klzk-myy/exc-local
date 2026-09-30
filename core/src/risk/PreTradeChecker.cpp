// Task 2.3.3 + 2.3.9 — 14 in-process pre-trade checks (spec §3.3).
// Straight-line integer pipeline, cheapest-first, short-circuits on the first
// failure; see include/risk/PreTradeChecker.hpp for the check contract.

#include "risk/PreTradeChecker.hpp"

#include "utils/TimeUtils.hpp"
#include "utils/safe_math.hpp"

namespace exch {

namespace {

// Fibonacci hashing (same constant as OrderBook's id index).
inline constexpr uint64_t kCollarHashMul = 0x9E3779B97F4A7C15ull;
inline constexpr uint64_t kNsPerSec = 1'000'000'000ULL;

[[nodiscard]] inline RiskVerdict reject(const char* code,
                                        const char* detail) noexcept {
    return verdict_reject(code, detail);
}

}  // namespace

PreTradeChecker::PreTradeChecker(RiskConfig cfg) noexcept : cfg_(cfg) {
    // burst==0 with rate>0 would reject every order — almost certainly a
    // misconfig; clamp to the rate (rate==0 remains the deliberate kill).
    if (cfg_.order_rate_burst == 0 && cfg_.order_rate_per_sec > 0) {
        cfg_.order_rate_burst = cfg_.order_rate_per_sec;
    }
    // One cold-path allocation for the whole object lifetime (nothrow — a
    // failed allocation leaves collar_ == nullptr and check 5 rejects
    // fail-closed; see header).
    collar_ = new (std::nothrow) CollarSlot[kCollarTableSlots]();
}

PreTradeChecker::~PreTradeChecker() { delete[] collar_; }

RiskDecision PreTradeChecker::check(const Order& order) noexcept {
    // The kill-switch gate applies even on the detached stub path — a
    // bound suspension lattice must never be bypassed by an unwired
    // account provider.
    if (suspensions_ != nullptr) {
        const char* kscope = suspensions_->scope_for(
            order.account_id,
            instrument_ != nullptr ? instrument_->symbol : nullptr,
            instrument_ != nullptr ? instrument_type_name(instrument_->type)
                                   : nullptr);
        if (kscope != nullptr) return RiskDecision::REJECT;
        // Task 13.3.6 — the OTR breach gate applies on the detached
        // path too; a bound lattice is never bypassed.
        if (suspensions_->otr_breached(order.account_id)) {
            return RiskDecision::REJECT;
        }
    }
    // Task 21.3.10 — a bound sanctions cache gates the detached path
    // too: anything but a clear verdict rejects (fail closed).
    if (sanctions_ != nullptr) {
        const uint64_t now = clock_fn_ != nullptr
                                 ? clock_fn_(clock_ctx_)
                                 : steady_ns();
        if (sanctions_->verdict_for(order.account_id, now) !=
            SanctionsCache::Verdict::Clear) {
            return RiskDecision::REJECT;
        }
    }
    if (accounts_ == nullptr) return RiskDecision::ACCEPT;  // detached stub
    const uint64_t now = clock_fn_ != nullptr
                             ? clock_fn_(clock_ctx_)
                             : steady_ns();
    const CheckContext ctx{instrument_, now};
    return run(order, ctx, nullptr).pass ? RiskDecision::ACCEPT
                                         : RiskDecision::REJECT;
}

StpMode PreTradeChecker::resolved_stp(const Order& order) const noexcept {
    const StpMode def = accounts_ != nullptr
                            ? accounts_->default_stp_mode(order.account_id)
                            : static_cast<StpMode>(kStpModeUnset);
    return resolve_stp_mode(order.stp_mode, def);
}

void PreTradeChecker::reset_account_collar(uint64_t account_id) noexcept {
    if (collar_ == nullptr || account_id == 0) return;
    const std::size_t mask = kCollarTableSlots - 1;
    const auto home_of = [](uint64_t k) noexcept {
        return static_cast<std::size_t>((k * kCollarHashMul) >> 48) &
               (kCollarTableSlots - 1);
    };
    std::size_t hole = home_of(account_id);
    for (uint32_t probe = 0; probe < kCollarMaxProbe; ++probe) {
        if (collar_[hole].key == 0) return;  // absent — nothing to evict
        if (collar_[hole].key == account_id) break;
        hole = (hole + 1) & mask;
        if (probe + 1 == kCollarMaxProbe) return;  // bound hit — not found
    }

    // Backward-shift deletion (Knuth 6.4R): clearing the slot outright would
    // orphan keys that probed past it, so the following cluster is reseated.
    for (;;) {
        collar_[hole].key = 0;
        std::size_t j = (hole + 1) & mask;
        for (;;) {
            if (collar_[j].key == 0) {
                if (collar_used_ > 0) --collar_used_;
                return;
            }
            const std::size_t h = home_of(collar_[j].key);
            // Move j -> hole iff h is NOT cyclically in (hole, j]: the key's
            // probe path crossed the hole, so filling the hole keeps it
            // reachable. h == hole (forward distance 0) is movable.
            const std::size_t d_hole_j = (j - hole) & mask;
            const std::size_t d_hole_h = (h - hole) & mask;
            if (d_hole_h == 0 || d_hole_h > d_hole_j) {
                collar_[hole] = collar_[j];
                collar_[j].key = 0;
                hole = j;
                break;
            }
            j = (j + 1) & mask;
        }
    }
}

int64_t PreTradeChecker::effective_price_ticks(
    const Order& order, uint64_t instrument_id) const noexcept {
    if (order.price_ticks > 0) return order.price_ticks;
    if (market_ == nullptr) return 0;
    const int64_t last = market_->last_price_ticks(instrument_id);
    if (last > 0) return last;
    // No trade yet — price the debit off the side the order would pay at.
    return order.side == Side::BUY
               ? market_->best_ask_ticks(instrument_id)
               : market_->best_bid_ticks(instrument_id);
}

// Check 5 — per-account order-rate collar (Task 2.3.9; in-process mandate).
// Token bucket, milli-token precision, integer-only refill. A submission that
// is rejected LATER still consumed its token here — the collar rates order
// submissions, not accepted orders.
RiskVerdict PreTradeChecker::check_collar(uint64_t account_id,
                                          uint64_t now_ns) noexcept {
    if (collar_ == nullptr) {
        return reject(kCodeRateLimitExceeded, "collar table unavailable");
    }
    if (cfg_.order_rate_per_sec == 0) {
        // rate 0 = deliberate reject-all collar (admin kill semantic).
        return reject(kCodeRateLimitExceeded, "account collar is zero");
    }

    const int64_t cap_x1000 =
        static_cast<int64_t>(cfg_.order_rate_burst) * kTokenScale;
    const int64_t rate_x1000 =
        static_cast<int64_t>(cfg_.order_rate_per_sec) * kTokenScale;

    std::size_t idx = static_cast<std::size_t>(
        (account_id * kCollarHashMul) >> 48) & (kCollarTableSlots - 1);
    CollarSlot* slot = nullptr;
    for (uint32_t probe = 0; probe < kCollarMaxProbe; ++probe) {
        CollarSlot& s = collar_[idx];
        if (s.key == account_id) {
            slot = &s;
            break;
        }
        if (s.key == 0) {  // empty — claim it for this account
            s.key = account_id;
            s.tokens_x1000 = cap_x1000;  // fresh account starts with a
            s.last_ns = now_ns;          // full burst allowance
            ++collar_used_;
            slot = &s;
            break;
        }
        idx = (idx + 1) & (kCollarTableSlots - 1);
    }
    if (slot == nullptr) {
        // Probe bound hit — table is nearly full; fail closed rather than
        // degrade to an unbounded scan or silently pass.
        return reject(kCodeRateLimitExceeded, "collar table saturated");
    }

    // Refill: elapsed * rate / 1e9, int128 intermediate (spec §3.6.2). A
    // clock regression yields zero refill — pessimistic, never free tokens.
    const uint64_t elapsed =
        now_ns > slot->last_ns ? now_ns - slot->last_ns : 0;
    slot->last_ns = now_ns;
    if (elapsed > 0) {
        const safe_math::int128_t add = safe_math::mul_wide_i64(
            elapsed > static_cast<uint64_t>(INT64_MAX)
                ? INT64_MAX
                : static_cast<int64_t>(elapsed),
            rate_x1000) / static_cast<safe_math::int128_t>(kNsPerSec);
        if (add >= static_cast<safe_math::int128_t>(cap_x1000)) {
            slot->tokens_x1000 = cap_x1000;
        } else {
            const int64_t t = slot->tokens_x1000 + static_cast<int64_t>(add);
            slot->tokens_x1000 = t > cap_x1000 ? cap_x1000 : t;
        }
    }

    if (slot->tokens_x1000 < kTokenScale) {
        return reject(kCodeRateLimitExceeded, "per-account order rate");
    }
    slot->tokens_x1000 -= kTokenScale;
    return verdict_pass();
}

RiskVerdict PreTradeChecker::run(const Order& order,
                                 const CheckContext& ctx,
                                 Order* stamp) noexcept {
    const Instrument* inst = ctx.instrument;
    const uint64_t instrument_id =
        inst != nullptr ? inst->instrument_id : 0;

    // ---- 0. Kill-switch suspension lattice (Task 11.3.4/11.3.8/11.3.12) --
    // Redis `halt:*` flags, refreshed in-process by the control-path
    // SuspensionRefresher. GLOBAL → ACCOUNT → COUNTERPARTY → INSTRUMENT →
    // INSTRUMENT_CLASS, first match wins; unverifiable state rejects
    // (fail closed). Bound checker only — unbound keeps the legacy
    // gateway-only enforcement.
    if (suspensions_ != nullptr) {
        const char* kscope = suspensions_->scope_for(
            order.account_id,
            inst != nullptr ? inst->symbol : nullptr,
            inst != nullptr ? instrument_type_name(inst->type) : nullptr);
        if (kscope != nullptr) {
            return reject(kCodeTradingHalted, kscope);
        }
        // ---- 0b. MiFID II RTS 9 OTR breach (Phase-13 Task 13.3.6) -------
        // Same refreshed snapshot: `otr:breach:{account}` raised by the
        // Go OtrMonitor. New orders reject OTR_LIMIT_EXCEEDED (429);
        // cancels never reach this pipeline — cancel-only during breach.
        if (suspensions_->otr_breached(order.account_id)) {
            return reject(kCodeOtrLimitExceeded,
                          "order-to-trade ratio limit breached — cancels only");
        }
    }

    // ---- 0c. Sanctions account flag (Phase-21 Task 21.3.10) --------------
    // Bound cache reads the last `exc:sanctions:*` snapshot — flagged
    // accounts reject SANCTIONS_HIT before any balance/margin work; a
    // snapshot that cannot attest state (unverifiable, missing heartbeat,
    // or stale past the bound window) rejects fail closed.
    if (sanctions_ != nullptr) {
        const SanctionsCache::Verdict sv =
            sanctions_->verdict_for(order.account_id, ctx.now_ns);
        if (sv == SanctionsCache::Verdict::Hit) {
            return reject(kCodeSanctionsHit,
                          "account sanctions-flagged — order entry blocked");
        }
        if (sv != SanctionsCache::Verdict::Clear) {
            return reject(kCodeSanctionsUnavailable,
                          sv == SanctionsCache::Verdict::Unscreened
                              ? "account screening unverified — fail closed"
                              : "sanctions flag state unverifiable — fail closed");
        }
    }

    // ---- 1. Account status (ACTIVE only) ---------------------------------
    // Cheapest first; account_id 0 is never a real account (BIGSERIAL).
    const AccountStatus as =
        (accounts_ != nullptr && order.account_id != 0)
            ? accounts_->status(order.account_id)
            : AccountStatus::UNKNOWN;
    switch (as) {
        case AccountStatus::ACTIVE:
            break;
        case AccountStatus::SUSPENDED:
            return reject(kCodeAccountSuspended, "account suspended");
        case AccountStatus::FROZEN:
            return reject(kCodeAccountFrozen, "account frozen");
        default:
            return reject(kCodeAccountInactive, "account not active");
    }

    // ---- 2. Instrument status (ACTIVE only; §7.1 state table) -------------
    if (inst == nullptr) {
        return reject(kCodeInstrumentInactive, "no instrument bound");
    }
    switch (inst->status) {
        case InstrumentStatus::ACTIVE:
            break;
        case InstrumentStatus::SUSPENDED:
            return reject(kCodeInstrumentSuspended, "instrument suspended");
        case InstrumentStatus::HALTED:
            return reject(kCodeInstrumentHalted, "instrument halted");
        case InstrumentStatus::CANCEL_ONLY:
            return reject(kCodeInstrumentCancelOnly,
                          "instrument accepts cancels only");
        case InstrumentStatus::DELISTED:
            // §7.1 remediation #35: the 30d close-only window accepts
            // reduce_only orders so positions can close; all other entry is
            // rejected.
            if ((order.flags & kOrderFlagReduceOnly) == 0) {
                return reject(kCodeInstrumentDelisted, "instrument delisted");
            }
            break;
        case InstrumentStatus::RESTRICTED:
            // §7.1: limit orders only — no market/stop-style types.
            if (order.type != OrderType::LIMIT) {
                return reject(kCodeInstrumentRestricted,
                              "restricted: limit orders only");
            }
            break;
        case InstrumentStatus::DRAFT:
        default:
            return reject(kCodeInstrumentInactive, "instrument not active");
    }

    // Notional in 10^8 quote units — computed once (128-bit intermediate),
    // reused by checks 3/8/12 and the optional credit screen. -1 == not
    // computable (price-less order with no reference) or overflow.
    const int64_t ref_price = effective_price_ticks(order, instrument_id);
    int64_t notional = -1;
    if (order.qty_units > 0 && ref_price > 0 &&
        !notional_units(order.qty_units, ref_price, notional)) {
        return reject(kCodeArithmeticOverflowDetected,
                      "notional computation overflow");
    }

    // ---- 3. Balance (available >= required debit) --------------------------
    // BUY debits quote notional; SELL debits base quantity. A price-less
    // market order with no reference at all (notional < 0) cannot be priced —
    // it passes here; the engine's no-liquidity gate (Task 2.3.13) owns the
    // empty-book case.
    {
        const bool buy = order.side == Side::BUY;
        const int64_t required = buy ? notional : order.qty_units;
        if (required >= 0) {
            const int64_t avail = accounts_->available_balance(
                order.account_id, instrument_id,
                buy ? BalanceUnit::QUOTE : BalanceUnit::BASE);
            if (avail < required) {
                return reject(kCodeInsufficientBalance,
                              "available balance below order requirement");
            }
        }
    }

    // ---- 4. Position limit (max open positions) ----------------------------
    // Skipped for reduce_only — it only ever shrinks exposure. A config of 0
    // means "no limit" (admin opt-out), matching §5.10 risk_limits semantics.
    if (cfg_.max_open_positions > 0 &&
        (order.flags & kOrderFlagReduceOnly) == 0 &&
        accounts_->open_position_count(order.account_id) >=
            cfg_.max_open_positions) {
        return reject(kCodePositionLimitExceeded,
                      "open position limit reached");
    }

    // ---- 5. Order-rate collar (in-process token bucket, Task 2.3.9) --------
    {
        const RiskVerdict v = check_collar(order.account_id, ctx.now_ns);
        if (!v.pass) return v;
    }

    // ---- 6. Price band vs last price (Task 2.3.9) --------------------------
    // Band exists only around a reference: first trade / no last_price ->
    // pass (documented contract choice — a bandless check cannot fire; the
    // alternative rejects every order on a fresh instrument forever).
    // Price-less order types (MARKET etc.) skip: their banding is the
    // §6.6a synthetic-limit slippage guard (Task 2.3.15), not this check.
    if (order.price_ticks > 0) {
        int64_t last = market_ != nullptr
                           ? market_->last_price_ticks(instrument_id)
                           : 0;
        if (last <= 0) last = inst->last_price_ticks;
        // A configured 0%/0% band is a zero-tolerance pin (pegged
        // instruments), not "unconfigured": it is still enforced.
        if (last > 0) {
            int64_t lo = 0, hi = 0;
            if (!price_band_bounds(*inst, last, lo, hi)) {
                return reject(kCodeArithmeticOverflowDetected,
                              "price band bounds overflow");
            }
            if (order.price_ticks > hi || order.price_ticks < lo) {
                return reject(kCodePriceOutOfBand,
                              "price outside instrument band");
            }
        }
    }

    // ---- 7. Max/min order qty ----------------------------------------------
    if (order.qty_units <= 0 ||
        order.qty_units < inst->min_order_qty_units) {
        return reject(kCodeOrderQtyBelowMin, "qty below instrument minimum");
    }
    // max_order_qty_units <= 0 = uncapped (documented; §5.1 configures it).
    if (inst->max_order_qty_units > 0 &&
        order.qty_units > inst->max_order_qty_units) {
        return reject(kCodeOrderQtyExceedsMax, "qty above instrument maximum");
    }

    // ---- 8. Margin (Phase-2 stub; Phase-19 owns the real engine) ------------
    // Seam: bound IMarginCheck replaces the placeholder entirely.
    if (margin_ != nullptr) {
        if (!margin_->passes(order, *inst, *accounts_, notional)) {
            return reject(kCodeMarginCheckFailed, "margin check failed");
        }
    } else if (notional > 0) {
        // Placeholder: quote balance >= notional * ratio (percent*100 over
        // 10'000). Unknown balance (<0) fails closed.
        const int64_t avail = accounts_->available_balance(
            order.account_id, instrument_id, BalanceUnit::QUOTE);
        const safe_math::int128_t need = safe_math::mul_wide_i64(
            notional, cfg_.min_margin_ratio_pct_x100);
        if (avail < 0 ||
            safe_math::mul_wide_i64(avail, 10'000) < need) {
            return reject(kCodeMarginCheckFailed,
                          "balance below stub margin requirement");
        }
    }

    // ---- 9. Circuit breaker (Phase-2 stub; Phase-13 owns five-tier) ---------
    // Placeholder per plan: reject if last price moved >5% in the 60s window.
    if (market_ != nullptr && cfg_.circuit_move_pct_x100 > 0) {
        const int64_t then = market_->price_ticks_ago(
            instrument_id, cfg_.circuit_window_ns, ctx.now_ns);
        const int64_t last = market_->last_price_ticks(instrument_id);
        if (then > 0 && last > 0) {
            const safe_math::int128_t diff =
                static_cast<safe_math::int128_t>(last) -
                static_cast<safe_math::int128_t>(then);
            const safe_math::int128_t adiff = diff < 0 ? -diff : diff;
            if (adiff * 10'000 >
                static_cast<safe_math::int128_t>(then) *
                    cfg_.circuit_move_pct_x100) {
                return reject(kCodeCircuitBreakerOpen,
                              "price move beyond 60s breaker window");
            }
        }
    }

    // ---- 10. KYC tier (Phase-2 stub; Phase-14 owns lifecycle) ----------------
    if (accounts_->kyc_tier(order.account_id) < cfg_.required_kyc_tier) {
        return reject(kCodeKycTierInsufficient, "kyc tier below requirement");
    }

    // ---- 11. Tick/lot quantization ------------------------------------------
    // price_ticks < 0 is malformed outright; == 0 is a price-less type and
    // skips the tick grid (nothing to quantize).
    if (order.price_ticks < 0 ||
        (order.price_ticks > 0 &&
         !is_tick_multiple(order.price_ticks, *inst))) {
        return reject(order.price_ticks < 0 ? kCodeInvalidRequest
                                            : kCodeTickSizeViolation,
                      order.price_ticks < 0 ? "negative price"
                                            : "price not a tick multiple");
    }
    if (!is_lot_multiple(order.qty_units, *inst)) {
        return reject(kCodeLotSizeViolation, "qty not a lot multiple");
    }

    // ---- 12. Min notional ----------------------------------------------------
    // Uncomputable (price-less order, no reference) passes — the check
    // prices nothing; engine-side guards own that case.
    if (notional >= 0 && inst->min_notional_units > 0 &&
        notional < inst->min_notional_units) {
        return reject(kCodeMinNotionalViolation,
                      "order notional below instrument minimum");
    }

    // ---- 13. Execution flags (§6.5) ------------------------------------------
    if ((order.flags & kOrderFlagPostOnly) != 0) {
        // MARKET can never rest -> marketable by definition. Limit-priced
        // orders are marketable iff they meet/cross the opposite best. An
        // unbound market view reports "not marketable" — the book's own
        // CROSSED-insert rejection (OrderBook, spec §24 #1) is the backstop.
        bool marketable = order.type == OrderType::MARKET;
        if (!marketable && market_ != nullptr && order.price_ticks > 0) {
            if (order.side == Side::BUY) {
                const int64_t ask = market_->best_ask_ticks(instrument_id);
                marketable = ask > 0 && order.price_ticks >= ask;
            } else {
                const int64_t bid = market_->best_bid_ticks(instrument_id);
                marketable = bid > 0 && order.price_ticks <= bid;
            }
        }
        if (marketable) {
            return reject(kCodePostOnlyViolation,
                          "post-only order would execute immediately");
        }
    }
    if ((order.flags & kOrderFlagReduceOnly) != 0) {
        // Requires an existing opposite-direction position (Phase-19 owns the
        // real position store; unbound provider fails closed = flat).
        const int64_t pos =
            positions_ != nullptr
                ? positions_->net_position_units(order.account_id,
                                                 instrument_id)
                : 0;
        if (pos == 0 ||
            (order.side == Side::BUY && pos > 0) ||
            (order.side == Side::SELL && pos < 0)) {
            return reject(kCodeReduceOnlyViolation,
                          "reduce-only would not reduce an open position");
        }
    }

    // ---- 14. STP — validate + resolve (Task 2.3.11/2.3.21/2.3.16) ------------
    // The same-account match prevention itself runs engine-side
    // (SelfTradeGuard); admission validates the field, resolves the effective
    // mode, and gates NONE to Professional/ECP (§24 #274).
    {
        const uint8_t raw = static_cast<uint8_t>(order.stp_mode);
        if (raw != kStpModeUnset && !is_known_stp_mode(order.stp_mode)) {
            return reject(kCodeInvalidRequest, "unknown stp_mode value");
        }
        const StpMode resolved = resolved_stp(order);
        if (resolved == StpMode::NONE) {
            // §24 #274 whitelist gate: NONE is legal ONLY for
            // PROFESSIONAL/ELIGIBLE_COUNTERPARTY — retail AND any future/
            // unrecognized category reject fail-closed.
            const ClientCategory cat =
                accounts_->client_category(order.account_id);
            if (cat != ClientCategory::PROFESSIONAL &&
                cat != ClientCategory::ELIGIBLE_COUNTERPARTY) {
                return reject(kCodeStpNoneNotPermitted,
                              "stp NONE requires Professional/ECP");
            }
        }
        if (stamp != nullptr) stamp->stp_mode = resolved;
    }

    // ---- Optional §3.3b admission screen (bound deployments only) -----------
    // Not one of the 14: the authoritative bilateral filter is match-time
    // consume_or_skip in the engine. This coarse pre-screen fails a
    // credit-screened taker early when NO counterparty row has mutual
    // headroom >= this order's notional. Unmapped accounts (anonymous flow)
    // skip — spec §3.3b.
    if (credit_ != nullptr && party_map_ != nullptr && notional > 0) {
        const uint32_t taker = party_map_->credit_party_id(order.account_id);
        if (taker < kCreditMaxParties) {
            bool any = false;
            for (uint32_t m = 0; m < kCreditMaxParties; ++m) {
                if (m == taker) continue;
                if (credit_->can_match(m, taker, notional)) {
                    any = true;
                    break;
                }
            }
            if (!any) {
                return reject(kCodeBilateralCreditExhausted,
                              "no bilateral credit headroom");
            }
        }
    }

    return verdict_pass();
}

}  // namespace exch
