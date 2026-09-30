// Phase-22 Task 22.3.12 — ImpliedMatcher implementation. See the header
// for the contract: implied-in (swap/strategy from outright legs) and
// implied-out (outright from swap+near legs) as one two-leg linear
// combination rule. Single-threaded — every registered book is driven
// from the same call site; no wall-clock reads; WAL-before-mutation per
// fill; no phantom resting orders.

#include "matching/ImpliedMatcher.hpp"

#include <cstring>

#include "utils/safe_math.hpp"

namespace exch {

// --- Registration ---------------------------------------------------------

int ImpliedMatcher::book_index(uint32_t instrument_id) const noexcept {
    for (uint32_t i = 0; i < book_count_; ++i) {
        if (books_[i].instrument_id == instrument_id) {
            return static_cast<int>(i);
        }
    }
    return -1;
}

const ImpliedMatcher::LinkRec* ImpliedMatcher::find_link(
    uint64_t link_id) const noexcept {
    for (uint32_t i = 0; i < link_count_; ++i) {
        if (links_[i].link.link_id == link_id) {
            return &links_[i];
        }
    }
    return nullptr;
}

ImpliedMatcher::LinkRec* ImpliedMatcher::find_link_mut(
    uint64_t link_id) noexcept {
    return const_cast<LinkRec*>(find_link(link_id));
}

bool ImpliedMatcher::register_book(uint32_t instrument_id, OrderBook& book,
                                   WalWriter* wal, IpcPublisher* pub,
                                   implied_fill_fn hook,
                                   void* hook_ctx) noexcept {
    if (instrument_id == 0 || book_count_ >= kMaxBooks ||
        book_index(instrument_id) >= 0) {
        return false;
    }
    BookCtx& c = books_[book_count_++];
    c.instrument_id = instrument_id;
    c.book = &book;
    c.wal = wal;
    c.pub = pub;
    c.hook = hook;
    c.hook_ctx = hook_ctx;
    return true;
}

bool ImpliedMatcher::register_link(const ImpliedLink& link) noexcept {
    if (link_count_ >= kMaxLinks || link.link_id == 0 ||
        find_link(link.link_id) != nullptr) {
        return false;
    }
    const int out = book_index(link.out_instrument);
    int leg_idx[2] = {book_index(link.legs[0].instrument_id),
                      book_index(link.legs[1].instrument_id)};
    if (out < 0 || leg_idx[0] < 0 || leg_idx[1] < 0) {
        return false;
    }
    for (int i = 0; i < 2; ++i) {
        const ImpliedLeg& l = link.legs[i];
        if ((l.sign != 1 && l.sign != -1) || l.ratio == 0 ||
            l.instrument_id == link.out_instrument) {
            return false;
        }
    }
    // Keep links sorted by link_id — deterministic evaluation order.
    LinkRec rec{};
    rec.link = link;
    rec.out_idx = static_cast<uint32_t>(out);
    rec.leg_idx[0] = static_cast<uint32_t>(leg_idx[0]);
    rec.leg_idx[1] = static_cast<uint32_t>(leg_idx[1]);
    uint32_t at = link_count_;
    while (at > 0 && links_[at - 1].link.link_id > link.link_id) {
        links_[at] = links_[at - 1];
        --at;
    }
    links_[at] = rec;
    ++link_count_;
    return true;
}

bool ImpliedMatcher::unregister_link(uint64_t link_id) noexcept {
    for (uint32_t i = 0; i < link_count_; ++i) {
        if (links_[i].link.link_id == link_id) {
            std::memmove(&links_[i], &links_[i + 1],
                         (link_count_ - i - 1) * sizeof(LinkRec));
            --link_count_;
            return true;
        }
    }
    return false;
}

// --- Quoting ---------------------------------------------------------------

int64_t ImpliedMatcher::level_capacity(const PriceLevel* lvl,
                                       uint64_t aggressor_account) noexcept {
    // FIFO prefix rule: usable capacity ends at the first same-account
    // maker — skipping it would violate price-time priority and skipping
    // the fill entirely (CANCEL-style STP semantics are engine-internal).
    // Hidden makers bound the prefix the same way: implied quotes derive
    // from VISIBLE liquidity only — a hidden member prints at the
    // visible-BBO midpoint under engine semantics (Task 16.3.13), which
    // the linear leg-price rule cannot express.
    int64_t cap = 0;
    for (const Order* m = lvl->head; m != nullptr; m = m->next) {
        if (m->account_id == aggressor_account ||
            (m->flags & kOrderFlagHidden) != 0) {
            break;
        }
        cap += remaining_qty_units(*m);
    }
    return cap;
}

ImpliedMatcher::Quote ImpliedMatcher::link_quote(
    const LinkRec& lr, Side agg_side,
    uint64_t aggressor_account) const noexcept {
    Quote q;
    q.link = &lr;
    safe_math::int128_t px = 0;
    int64_t max_out = 0;
    for (int i = 0; i < 2; ++i) {
        const ImpliedLeg& leg = lr.link.legs[i];
        const BookCtx& ctx = books_[lr.leg_idx[i]];
        const PriceLevel* lvl =
            ctx.book->level(leg_maker_side(agg_side, leg.sign), 0);
        if (lvl == nullptr) {
            return q;  // no liquidity on a needed leg — no implied quote
        }
        const int64_t cap_units =
            level_capacity(lvl, aggressor_account) /
            static_cast<int64_t>(leg.ratio);
        if (cap_units <= 0) {
            return q;
        }
        if (i == 0 || cap_units < max_out) {
            max_out = cap_units;
        }
        px += safe_math::int128_t{leg.sign} *
              safe_math::int128_t{lvl->price_ticks};
    }
    int64_t px_narrow = 0;
    if (!safe_math::try_narrow_i128(px, px_narrow) || max_out <= 0) {
        return q;
    }
    q.usable = true;
    q.price_ticks = px_narrow;
    q.max_out_units = max_out;
    return q;
}

ImpliedMatcher::Quote ImpliedMatcher::best_quote(
    uint32_t out_instrument, Side agg_side, int64_t limit_ticks,
    bool has_limit, uint64_t aggressor_account) const noexcept {
    Quote best;
    for (uint32_t i = 0; i < link_count_; ++i) {
        const LinkRec& lr = links_[i];
        if (!lr.link.enabled ||
            lr.link.out_instrument != out_instrument) {
            continue;
        }
        Quote q = link_quote(lr, agg_side, aggressor_account);
        if (!q.usable) {
            continue;
        }
        if (has_limit && !crosses(agg_side, limit_ticks, q.price_ticks)) {
            continue;
        }
        if (!best.usable ||
            (agg_side == Side::BUY ? q.price_ticks < best.price_ticks
                                   : q.price_ticks > best.price_ticks)) {
            // First-encountered wins ties — links are link_id sorted.
            best = q;
        }
    }
    return best;
}

// --- Evaluation (read-only) ------------------------------------------------

ImpliedMatcher::EvalResult ImpliedMatcher::evaluate_incoming(
    uint32_t out_instrument, const Order& taker, bool has_limit,
    int64_t limit) const noexcept {
    EvalResult r;
    if (!enabled_) {
        return r;
    }

    // Depth-cursor walk per link — read-only, so consumed level capacity
    // is simulated via (depth, consumed) pairs per leg.
    struct Cursor {
        const LinkRec* lr;
        std::size_t depth[2];
        int64_t     consumed[2];  // units already notionally consumed at depth[i]
    };
    Cursor curs[kMaxLinks];
    uint32_t ncur = 0;
    for (uint32_t i = 0; i < link_count_; ++i) {
        if (links_[i].link.enabled &&
            links_[i].link.out_instrument == out_instrument) {
            curs[ncur].lr = &links_[i];
            curs[ncur].depth[0] = curs[ncur].depth[1] = 0;
            curs[ncur].consumed[0] = curs[ncur].consumed[1] = 0;
            ++ncur;
        }
    }
    if (ncur == 0) {
        return r;
    }
    int64_t remaining = remaining_qty_units(taker);
    for (uint64_t guard = 0; guard < kMaxStepsPerSweep; ++guard) {
        // Best current quote across links at their cursor positions.
        int64_t best_px = 0;
        Cursor* best = nullptr;
        int64_t best_avail = 0;
        for (uint32_t c = 0; c < ncur; ++c) {
            Cursor& cu = curs[c];
            safe_math::int128_t px = 0;
            int64_t avail = 0;
            bool ok = true;
            for (int i = 0; i < 2 && ok; ++i) {
                const ImpliedLeg& leg = cu.lr->link.legs[i];
                const BookCtx& ctx = books_[cu.lr->leg_idx[i]];
                const PriceLevel* lvl = ctx.book->level(
                    leg_maker_side(taker.side, leg.sign), cu.depth[i]);
                if (lvl == nullptr) {
                    ok = false;
                    break;
                }
                int64_t cap = level_capacity(lvl, taker.account_id);
                // Self-makers truncate the prefix; consumed accounting
                // stays consistent because capacity is prefix-bounded.
                cap -= cu.consumed[i];
                if (cap <= 0) {
                    // Level's usable prefix exhausted — advance the cursor.
                    ++cu.depth[i];
                    cu.consumed[i] = 0;
                    --i;  // re-evaluate this leg at the new depth
                    continue;
                }
                cap /= static_cast<int64_t>(leg.ratio);
                if (i == 0 || cap < avail) {
                    avail = cap;
                }
                px += safe_math::int128_t{leg.sign} *
                      safe_math::int128_t{lvl->price_ticks};
            }
            int64_t pxi = 0;
            if (!ok || avail <= 0 || !safe_math::try_narrow_i128(px, pxi)) {
                continue;
            }
            if (has_limit && !crosses(taker.side, limit, pxi)) {
                continue;
            }
            if (best == nullptr ||
                (taker.side == Side::BUY ? pxi < best_px : pxi > best_px)) {
                best = &cu;
                best_px = pxi;
                best_avail = avail;
            }
        }
        if (best == nullptr) {
            break;
        }
        if (r.fillable_qty_units == 0) {
            r.best_price_ticks = best_px;
            r.crossed = true;
        }
        const int64_t step =
            best_avail < remaining ? best_avail : remaining;
        r.fillable_qty_units += step;
        remaining -= step;
        // Notionally consume step units across the winning link's legs.
        for (int i = 0; i < 2; ++i) {
            const ImpliedLeg& leg = best->lr->link.legs[i];
            best->consumed[i] += step * static_cast<int64_t>(leg.ratio);
        }
        if (remaining <= 0) {
            break;
        }
    }
    return r;
}

// --- Execution ---------------------------------------------------------------

bool ImpliedMatcher::exec_step(const LinkRec& lr, Side agg_side,
                               uint64_t agg_account, uint64_t agg_id,
                               Order* taker, Order* resting,
                               int64_t out_units, int64_t out_px,
                               uint64_t now_ns, uint64_t& next_tid,
                               Result& r) noexcept {
    // Leg fills first — each maker journaled at its own leg price before
    // apply_fill (WAL-before-mutation), then the out-book presentation
    // record at the implied price.
    for (int i = 0; i < 2; ++i) {
        const ImpliedLeg& leg = lr.link.legs[i];
        BookCtx& ctx = books_[lr.leg_idx[i]];
        const Side maker_side = leg_maker_side(agg_side, leg.sign);
        const Side t_side = leg_taker_side(agg_side, leg.sign);
        int64_t need = 0;
        if (!safe_math::try_mul_i64(out_units,
                                  static_cast<int64_t>(leg.ratio), need)) {
            last_reject_ = "IMPLIED_QTY_OVERFLOW";
            return false;
        }
        while (need > 0) {
            Order* m = ctx.book->front_order(maker_side);
            if (m == nullptr) {
                // Capacity was prefighted — a vanished level means the
                // book diverged from the quote under our feet.
                last_reject_ = "IMPLIED_LEG_EMPTY";
                wal_fault_ = true;
                return false;
            }
            if (m->account_id == agg_account) {
                // First self-maker reached — prefix capacity ended. This
                // is unreachable if the quote respected level_capacity.
                last_reject_ = "IMPLIED_SELF_ACCOUNT";
                wal_fault_ = true;
                return false;
            }
            if ((m->flags & kOrderFlagHidden) != 0) {
                // Hidden members bound the capacity prefix the same way —
                // reaching one means the book diverged from the quote.
                last_reject_ = "IMPLIED_HIDDEN_LEG";
                wal_fault_ = true;
                return false;
            }
            const int64_t m_rem = remaining_qty_units(*m);
            const int64_t fill = need < m_rem ? need : m_rem;
            if (fill <= 0) {
                last_reject_ = "IMPLIED_ZERO_FILL";
                wal_fault_ = true;
                return false;
            }
            const uint64_t tid = next_tid++;
            // L3-correlation capture precede apply_fill — the node may
            // be freed by it.
            const uint64_t m_id = m->id;
            const uint64_t m_acct = m->account_id;
            const Side m_side = m->side;
            const int64_t m_limit = m->price_ticks;
            const uint64_t buy_id =
                t_side == Side::BUY ? agg_id : m_id;
            const uint64_t sell_id =
                t_side == Side::BUY ? m_id : agg_id;
            const int64_t px = m_limit;
            const uint64_t wseq =
                ctx.wal != nullptr ? ctx.wal->tail_seq() : 0;
            if (ctx.wal != nullptr &&
                ctx.wal->write_trade(tid, buy_id, sell_id,
                                     ctx.instrument_id, px, fill,
                                     now_ns) != WalStatus::Ok) {
                last_reject_ = "WAL_FAULT";
                wal_fault_ = true;
                return false;
            }
            if (ctx.book->apply_fill(m, fill) != BookError::OK) {
                // Book rejected a journaled trade — structural fault,
                // halt like the engine does mid-sweep.
                last_reject_ = "BOOK_FAULT";
                wal_fault_ = true;
                return false;
            }
            if (ctx.pub != nullptr) {
                (void)ctx.pub->publish_trade(tid, buy_id, sell_id, px, fill,
                                             ctx.book->book_seq(), now_ns);
            }
            if (ctx.hook != nullptr) {
                const FillNote n{m_id, m_acct, m_side, ctx.instrument_id,
                                 tid, wseq, px, fill, m_limit,
                                 /*order_remaining_after*/ 0,
                                 /*out_presentation*/ false,
                                 /*taker_leg*/ false};
                ctx.hook(ctx.hook_ctx, n);
            }
            need -= fill;
            r.leg_units += fill;
            ++r.fills;
            ++fills_;
        }
    }

    // Out-book presentation record — the synthetic counterparty marks
    // that the far side was the implied combo, not an order row.
    BookCtx& out = books_[lr.out_idx];
    const uint64_t tid = next_tid++;
    const uint64_t out_wseq =
        out.wal != nullptr ? out.wal->tail_seq() : 0;
    const uint64_t buy_id =
        agg_side == Side::BUY ? agg_id : kImpliedCounterpartyId;
    const uint64_t sell_id =
        agg_side == Side::BUY ? kImpliedCounterpartyId : agg_id;
    if (out.wal != nullptr &&
        out.wal->write_trade(tid, buy_id, sell_id, out.instrument_id,
                             out_px, out_units, now_ns) != WalStatus::Ok) {
        last_reject_ = "WAL_FAULT";
        wal_fault_ = true;
        return false;
    }
    FillNote out_note{};
    out_note.instrument_id = out.instrument_id;
    out_note.trade_id = tid;
    out_note.wal_seq = out_wseq;
    out_note.price_ticks = out_px;
    out_note.qty_units = out_units;
    out_note.out_presentation = true;
    out_note.side = agg_side;
    bool notify_out = false;
    if (resting != nullptr) {
        out_note.order_id = resting->id;
        out_note.account_id = resting->account_id;
        out_note.limit_ticks = resting->price_ticks;
        if (out.book->apply_fill(resting, out_units) != BookError::OK) {
            last_reject_ = "BOOK_FAULT";
            wal_fault_ = true;
            return false;
        }
        out_note.order_remaining_after =
            out.book->find_order(out_note.order_id) != nullptr
                ? -1 /* owner resolves via effective remaining */
                : 0;
        notify_out = true;
    } else if (taker != nullptr) {
        taker->filled_qty_units += out_units;
        out_note.order_id = taker->id;
        out_note.account_id = taker->account_id;
        out_note.limit_ticks = taker->price_ticks;
        out_note.taker_leg = true;
        out_note.order_remaining_after = remaining_qty_units(*taker);
        notify_out = true;
    }
    if (out.pub != nullptr) {
        (void)out.pub->publish_trade(tid, buy_id, sell_id, out_px,
                                     out_units, out.book->book_seq(),
                                     now_ns);
    }
    if (notify_out && out.hook != nullptr) {
        out.hook(out.hook_ctx, out_note);
    }
    r.filled_units += out_units;
    ++r.combos;
    qty_filled_ += out_units;
    return true;
}

ImpliedMatcher::Result ImpliedMatcher::match_incoming(
    uint32_t out_instrument, Order& taker, uint64_t now_ns,
    uint64_t& next_tid, bool has_limit, int64_t limit_ticks) noexcept {
    Result r;
    if (!enabled_ || wal_fault_) {
        r.reject = enabled_ ? "WAL_FAULT" : "IMPLIED_DISABLED";
        return r;
    }
    // POST_ONLY never mutates through the implied path — report whether
    // it would have matched so the host can reject it as marketable.
    if ((taker.flags & kOrderFlagPostOnly) != 0) {
        r.would_match = evaluate_incoming(out_instrument, taker, has_limit,
                                          limit_ticks).crossed;
        return r;
    }
    for (uint64_t guard = 0; guard < kMaxStepsPerSweep; ++guard) {
        const int64_t remaining = remaining_qty_units(taker);
        if (remaining <= 0) {
            break;
        }
        const Quote q = best_quote(out_instrument, taker.side,
                                   limit_ticks, has_limit,
                                   taker.account_id);
        if (!q.usable) {
            break;
        }
        const int64_t step =
            q.max_out_units < remaining ? q.max_out_units : remaining;
        if (!exec_step(*q.link, taker.side, taker.account_id, taker.id,
                       &taker, nullptr, step, q.price_ticks, now_ns,
                       next_tid, r)) {
            r.wal_fault = wal_fault_;
            break;
        }
    }
    r.wal_fault = wal_fault_;
    r.reject = last_reject_;
    return r;
}

ImpliedMatcher::Result ImpliedMatcher::on_book_changed(
    uint32_t changed_instrument, uint64_t now_ns,
    uint64_t& next_tid) noexcept {
    Result r;
    if (!enabled_ || wal_fault_) {
        r.reject = enabled_ ? "WAL_FAULT" : "IMPLIED_DISABLED";
        return r;
    }
    uint64_t steps = 0;
    // Every link whose legs touch the changed instrument re-evaluates
    // its out book — deterministic link_id order.
    for (uint32_t li = 0; li < link_count_; ++li) {
        const LinkRec& lr = links_[li];
        if (!lr.link.enabled) {
            continue;
        }
        // Leg changes shift the implied quote; out-book membership
        // changes (amend into the implied spread, enable-time re-scan)
        // can put a resting order across an unchanged quote.
        const bool touches =
            lr.link.legs[0].instrument_id == changed_instrument ||
            lr.link.legs[1].instrument_id == changed_instrument ||
            lr.link.out_instrument == changed_instrument;
        if (!touches) {
            continue;
        }
        BookCtx& out = books_[lr.out_idx];
        // Resting bids crossing the implied ask (they buy out) — then
        // resting asks crossing the implied bid. Each fill restarts the
        // scan because apply_fill may free nodes.
        for (int pass = 0; pass < 2; ++pass) {
            const Side resting_side = pass == 0 ? Side::BUY : Side::SELL;
            const Side agg_side = resting_side;  // the resting order's
                                                 // direction on `out`
            for (;;) {
                bool progressed = false;
                const uint32_t depth_n = resting_side == Side::BUY
                                             ? out.book->bid_count()
                                             : out.book->ask_count();
                for (uint32_t d = 0; d < depth_n && !progressed; ++d) {
                    const PriceLevel* lvl =
                        out.book->level(resting_side, d);
                    if (lvl == nullptr) {
                        break;
                    }
                    for (Order* o = lvl->head; o != nullptr; o = o->next) {
                        const Quote q = link_quote(lr, agg_side,
                                                   o->account_id);
                        // Resting fills at the implied price (the price
                        // the legs produced), not its own limit — maker
                        // price improvement flows to the resting order.
                        if (!q.usable ||
                            !crosses(agg_side, o->price_ticks,
                                     q.price_ticks)) {
                            continue;
                        }
                        const int64_t need = remaining_qty_units(*o);
                        const int64_t step = q.max_out_units < need
                                                 ? q.max_out_units
                                                 : need;
                        if (step <= 0) {
                            continue;
                        }
                        if (++steps > kMaxStepsPerSweep) {
                            r.reject = "IMPLIED_STEP_BOUND";
                            wal_fault_ = true;
                            r.wal_fault = true;
                            return r;
                        }
                        if (!exec_step(lr, agg_side, o->account_id, o->id,
                                       nullptr, o, step, q.price_ticks,
                                       now_ns, next_tid, r)) {
                            r.wal_fault = wal_fault_;
                            r.reject = last_reject_;
                            return r;
                        }
                        progressed = true;
                        break;  // node may be freed — rescan
                    }
                }
                if (!progressed) {
                    break;
                }
            }
        }
    }
    r.wal_fault = wal_fault_;
    r.reject = last_reject_;
    return r;
}

}  // namespace exch
