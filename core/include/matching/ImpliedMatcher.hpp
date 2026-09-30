#pragma once

// Phase-22 Task 22.3.12 — multi-leg implied matching for the FX
// derivative maturity curve (spec §6.3 swap/outright books, §24 implied
// liquidity; Go-side rollout gate: services/internal/derivatives/
// implied_gate.go, flag "implied_matching").
//
// ImpliedMatcher sits above a registered set of per-instrument
// OrderBooks on the SAME matching thread (single-writer discipline is
// preserved — every book is driven from one call site) and turns
// resting outright liquidity into executable implied liquidity:
//
//   IMPLIED_IN — a synthetic quote in a strategy/swap book composed
//     from outright legs. FX swap points are far − near, so the link
//     legs = {far:+1, near:−1}:  implied swap ask = far_ask − near_bid.
//   IMPLIED_OUT — a synthetic quote in an outright book composed from
//     the swap plus a nearer book: legs = {swap:+1, near:+1} ⇒
//     implied far ask = swap_ask + near_ask.
//
// Both cases reduce to one rule: out_price = s0·p0 + s1·p1 with each
// leg's contributing side chosen by the direction the aggressor takes
// in the out book (a +1 leg contributes its ask when the aggressor
// buys out, its bid when selling; a −1 leg inverts).
//
// Determinism & safety contract (spec §2.7, §3.1):
//   * All book state changes journal BEFORE mutation — per leg fill the
//     sequence is wal->write_trade() → book.apply_fill() → publish; a
//     WAL failure halts the sweep with wal_fault_ set (the journaled
//     prefix is replay-consistent, matching the engine's mid-sweep
//     fault semantics — a partial combo is a durability halt, never a
//     torn in-memory state).
//   * No wall-clock reads: every API takes an explicit now_ns.
//   * No phantom resting orders: the matcher only consumes resting
//     orders via apply_fill — it never inserts into a book.
//   * Same-account makers are never self-matched by the implied path:
//     a FIFO prefix rule caps a level's usable capacity at the first
//     maker sharing the aggressor's account_id (the full STP group
//     resolution lives in MatchingEngine meta tables — a documented
//     production-integration seam).
//   * Links sort by link_id; equal implied prices resolve to the lower
//     link_id — replay-stable.
//
// Production wiring: MatchingEngine::bind_implied() registers the
// engine's book with a matcher shared by the curve's co-located books
// (one matching thread, one process — the curve-shard host topology is
// recorded in spec §27). The engine calls match_incoming between the
// outright sweep and rest_remainder, and on_book_changed at every
// mutation-commit point; each registered book carries an owner hook so
// fills the matcher produces still run the owning engine's post-fill
// bookkeeping (iceberg records, meta slots, OCO winner arms, L3 emits,
// LAST-price trigger reference).

#include <cstdint>

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "matching/IpcPublisher.hpp"
#include "matching/WalWriter.hpp"

namespace exch {

// Synthetic counterparty id used on the out-book presentation TRADE
// record — the far side of an implied fill has no order id (the combo
// is executed against the legs' real makers, each journaled at its own
// leg price). 0 is reserved: order ids are pool-allocated ≥1 upstream.
inline constexpr uint64_t kImpliedCounterpartyId = 0;

// One contributing book in a two-leg linear combination.
struct ImpliedLeg {
    uint32_t instrument_id = 0;
    int8_t   sign          = 0;  // +1 or −1 — price contribution sign
    uint32_t ratio         = 0;  // leg units consumed per out-book unit (≥1)
};

// ImpliedLink: implied price on `out_instrument` =
//     legs[0].sign · legs[0].price + legs[1].sign · legs[1].price
// with each leg's contributing side picked by the aggressor's
// direction (see header comment).
struct ImpliedLink {
    uint64_t   link_id        = 0;
    uint32_t   out_instrument = 0;
    ImpliedLeg legs[2];
    bool       enabled = true;
};

// FillNote — owner-bookkeeping notification for one fill the matcher
// produces inside a registered book. Fired synchronously on the
// matching thread after WAL-commit + apply_fill, once per maker fill
// (leg books) and once per out-book order fill (taker mid-ingress or
// resting). wal_seq is the seq the TRADE row committed with — captured
// pre-append exactly like MatchingEngine::journal_seq(), so L3 fills
// emitted by the owner correlate 1:1 with the journal row.
struct FillNote {
    uint64_t order_id = 0;              // filled order in this book
    uint64_t account_id = 0;
    Side     side = Side::BUY;
    uint32_t instrument_id = 0;         // book the fill printed on
    uint64_t trade_id = 0;
    uint64_t wal_seq = 0;               // 0 in journal-free mode
    int64_t  price_ticks = 0;
    int64_t  qty_units = 0;
    int64_t  limit_ticks = 0;           // filled order's own limit
    int64_t  order_remaining_after = 0; // taker path: post-fill remaining
    bool     out_presentation = false;  // out-book record (vs leg fill)
    bool     taker_leg = false;         // order is mid-ingress, not resting
};
using implied_fill_fn = void (*)(void* ctx, const FillNote&) noexcept;

class ImpliedMatcher {
public:
    static constexpr std::size_t kMaxBooks = 32;
    static constexpr std::size_t kMaxLinks = 64;
    // Combo-step bound: a bounded loop can never run away even on a
    // pathological book (each step consumes ≥1 out unit of liquidity,
    // so the bound is defensive only).
    static constexpr uint64_t kMaxStepsPerSweep = 1u << 20;

    ImpliedMatcher() noexcept = default;
    ImpliedMatcher(const ImpliedMatcher&) = delete;
    ImpliedMatcher& operator=(const ImpliedMatcher&) = delete;

    // --- Registration (cold path — call before matching starts) ---------

    // Register an instrument's book together with its journal/publisher
    // and owner hook. wal may be nullptr (journal-free unit-test mode);
    // pub may be nullptr (silent). hook may be nullptr — host-owned
    // books with no engine bookkeeping (tests, plain leg books); an
    // engine-owned book MUST carry its engine's hook or matcher fills
    // will bypass the engine's side-table invariants. Fails closed
    // (false) on duplicate id, null book or capacity.
    [[nodiscard]] bool register_book(uint32_t instrument_id,
                                     OrderBook& book,
                                     WalWriter* wal,
                                     IpcPublisher* pub,
                                     implied_fill_fn hook = nullptr,
                                     void* hook_ctx = nullptr) noexcept;

    // Register an implied link. Fails closed on: unregistered out/leg
    // instruments, leg sign ∉ {+1,−1}, ratio 0, out equal to a leg
    // (self-reference), duplicate link_id. Links are kept sorted by
    // link_id for deterministic evaluation order.
    [[nodiscard]] bool register_link(const ImpliedLink& link) noexcept;
    [[nodiscard]] bool unregister_link(uint64_t link_id) noexcept;

    // Feature-flag mirror (IMPLIED_MATCHING gate, services-side). While
    // false, every entry point is a no-op returning empty results.
    void set_enabled(bool on) noexcept { enabled_ = on; }
    [[nodiscard]] bool enabled() const noexcept { return enabled_; }

    // --- Evaluation (read-only) -----------------------------------------

    // Dry-run the implied path for an incoming taker: the maximum
    // out-units the registered links could fill strictly within the
    // taker's limit, and the best implied price it would face. No book
    // or WAL state changes — the host uses this for POST_ONLY and FOK
    // feasibility before committing.
    struct EvalResult {
        int64_t fillable_qty_units = 0;   // full-depth implied capacity
        int64_t best_price_ticks   = 0;   // best implied quote; 0 = none
        bool    crossed            = false; // best quote inside the limit
    };
    [[nodiscard]] EvalResult evaluate_incoming(
        uint32_t out_instrument, const Order& taker) const noexcept {
        return evaluate_incoming(out_instrument, taker,
                                 taker.type != OrderType::MARKET,
                                 taker.price_ticks);
    }
    // Explicit-limit form — the host decides limit semantics (a STOP
    // node swept with market semantics, or a discretionary walk bound
    // substituting the nominal limit).
    [[nodiscard]] EvalResult evaluate_incoming(
        uint32_t out_instrument, const Order& taker, bool has_limit,
        int64_t limit_ticks) const noexcept;

    // --- Matching --------------------------------------------------------

    struct Result {
        int64_t filled_units = 0;   // out-book units executed via implied
        int64_t leg_units    = 0;   // Σ leg units consumed
        uint32_t fills       = 0;   // leg-level maker fills
        uint32_t combos      = 0;   // out-book presentation fills
        bool     wal_fault   = false;
        bool     would_match = false; // post_only probe outcome (no mutation)
        const char* reject = "OK";
    };

    // Implied sweep for an incoming aggressive order in `out_instrument`
    // (implied-in AND implied-out — the link table decides which
    // combinations exist). Fills the taker at implied prices ≤ its
    // limit; partial fills are normal (IOC/FOK semantics belong to the
    // host — FOK callers run evaluate_incoming first). POST_ONLY takers
    // never mutate: would_match reports whether implied liquidity would
    // have filled, letting the host reject.
    //
    // `taker` is mid-ingress (NOT resting) — its filled_qty_units is
    // advanced here; the host owns booking/cancelling the remainder.
    // next_tid is the caller's trade-id stream (engine next_trade_id_ in
    // production; a test-local counter in tests).
    Result match_incoming(uint32_t out_instrument, Order& taker,
                          uint64_t now_ns, uint64_t& next_tid) noexcept {
        return match_incoming(out_instrument, taker, now_ns, next_tid,
                              taker.type != OrderType::MARKET,
                              taker.price_ticks);
    }
    // Explicit-limit form (see evaluate_incoming).
    Result match_incoming(uint32_t out_instrument, Order& taker,
                          uint64_t now_ns, uint64_t& next_tid,
                          bool has_limit, int64_t limit_ticks) noexcept;

    // Post-mutation rescan: after any book change (new resting order,
    // cancel, amend) the coordinator calls this for the changed
    // instrument. Every link whose legs touch that instrument re-evaluates
    // its out book: resting out orders now crossing the implied quote
    // fill at the implied price in price-time order — implied-in when
    // `changed` is an outright leg and the out book is the swap, or
    // implied-out when `changed` is the swap/near leg and the out book
    // is a further outright.
    Result on_book_changed(uint32_t changed_instrument,
                           uint64_t now_ns, uint64_t& next_tid) noexcept;

    // --- Introspection (counters, cold path) ------------------------------

    [[nodiscard]] bool     wal_fault()    const noexcept { return wal_fault_; }
    [[nodiscard]] uint64_t fills()        const noexcept { return fills_; }
    [[nodiscard]] int64_t  qty_filled()   const noexcept { return qty_filled_; }
    [[nodiscard]] const char* last_reject() const noexcept { return last_reject_; }

private:
    struct BookCtx {
        uint32_t         instrument_id = 0;
        OrderBook*       book = nullptr;
        WalWriter*       wal  = nullptr;
        IpcPublisher*    pub  = nullptr;
        implied_fill_fn  hook      = nullptr;
        void*            hook_ctx  = nullptr;
    };
    struct LinkRec {
        ImpliedLink link;
        uint32_t    out_idx;         // index into books_
        uint32_t    leg_idx[2];      // index into books_
    };

    // Index of books_ entry for an instrument, or −1.
    int book_index(uint32_t instrument_id) const noexcept;
    const LinkRec* find_link(uint64_t link_id) const noexcept;
    LinkRec* find_link_mut(uint64_t link_id) noexcept;

    // Side of a leg book the aggressor consumes: buying out buys the +1
    // legs' asks (SELL-side makers) and sells into −1 legs' bids.
    static constexpr Side leg_maker_side(Side agg_side, int8_t sign) noexcept {
        const bool buy_leg = (agg_side == Side::BUY) == (sign > 0);
        return buy_leg ? Side::SELL : Side::BUY;
    }
    // The aggressor's own direction on a leg.
    static constexpr Side leg_taker_side(Side agg_side, int8_t sign) noexcept {
        const bool buy_leg = (agg_side == Side::BUY) == (sign > 0);
        return buy_leg ? Side::BUY : Side::SELL;
    }
    // Implied quote the aggressor faces on a link: BUY ⇒ ask-side combo,
    // SELL ⇒ bid-side combo. out_px = Σ sign_i·level_i.price; usable only
    // when every leg shows a level and capacity > 0.
    struct Quote {
        bool     usable        = false;
        int64_t  price_ticks   = 0;
        int64_t  max_out_units = 0;
        const LinkRec* link    = nullptr;
    };
    Quote link_quote(const LinkRec& lr, Side agg_side,
                     uint64_t aggressor_account) const noexcept;
    // Capacity of a leg level for this aggressor: FIFO prefix of maker
    // remaining quantities until the first same-account maker (never
    // skipped — that would break price-time priority).
    static int64_t level_capacity(const PriceLevel* lvl,
                                  uint64_t aggressor_account) noexcept;
    // Best crossing quote for an aggressor across all enabled links on
    // `out_instrument` — lowest ask for a BUY, highest bid for a SELL;
    // link_id breaks ties.
    Quote best_quote(uint32_t out_instrument, Side agg_side,
                     int64_t limit_ticks, bool has_limit,
                     uint64_t aggressor_account) const noexcept;
    static bool crosses(Side side, int64_t limit, int64_t px) noexcept {
        return side == Side::BUY ? px <= limit : px >= limit;
    }

    // Execute one combo step: fill `out_units` worth of each leg FIFO at
    // its level, journaled+applied per maker, then journal+publish the
    // out-book presentation record at implied px. aggressor may be a
    // mid-ingress taker (resting==nullptr) or a resting out-book order
    // (apply_fill'd itself). Returns false on WAL/book fault.
    bool exec_step(const LinkRec& lr, Side agg_side, uint64_t agg_account,
                   uint64_t agg_id, Order* taker, Order* resting,
                   int64_t out_units, int64_t out_px, uint64_t now_ns,
                   uint64_t& next_tid, Result& r) noexcept;

    BookCtx  books_[kMaxBooks];
    uint32_t book_count_ = 0;
    LinkRec  links_[kMaxLinks];
    uint32_t link_count_ = 0;
    bool     enabled_   = true;
    bool     wal_fault_ = false;
    const char* last_reject_ = "OK";
    uint64_t fills_      = 0;
    int64_t  qty_filled_ = 0;
};

}  // namespace exch
