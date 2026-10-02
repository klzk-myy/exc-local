// Task 2.3.25 — OptimisticShardCoordinator implementation.
// See the header for the protocol, 500µs budget, compensating-unwind, GL
// posting, and WAL contracts (spec §2.2a / §24 #404 — canonical routing path
// superseding blocking 2PC locks; reservation bookkeeping remains Task
// 2.3.8's CrossShardCoordinator).

#include "matching/OptimisticShardCoordinator.hpp"

#include "utils/safe_math.hpp"

namespace exch {

namespace {

uint32_t opt_body_len(OptCtlType t) noexcept {
    switch (t) {
        case OptCtlType::TryMatch:  return sizeof(OptTryMatchBody);
        case OptCtlType::TryAck:    return sizeof(OptTryAckBody);
        case OptCtlType::TryNack:   return sizeof(OptTryNackBody);
        case OptCtlType::Unwind:    return sizeof(OptUnwindBody);
        case OptCtlType::UnwindAck: return sizeof(OptUnwindAckBody);
    }
    return 0;
}

}  // namespace

const char* opt_ctl_decode_str(OptCtlDecode r) noexcept {
    switch (r) {
        case OptCtlDecode::Ok:          return "Ok";
        case OptCtlDecode::TooShort:    return "TooShort";
        case OptCtlDecode::BadMagic:    return "BadMagic";
        case OptCtlDecode::BadVersion:  return "BadVersion";
        case OptCtlDecode::UnknownType: return "UnknownType";
    }
    return "?";
}

uint32_t opt_ctl_encode(uint8_t* dst, uint32_t cap, OptCtlType type,
                        const void* body, uint32_t src_shard,
                        uint32_t dst_shard) noexcept {
    const uint32_t blen = opt_body_len(type);
    if (dst == nullptr || body == nullptr || blen == 0)
        return 0;
    const uint32_t total = sizeof(CrossShardCtlHeader) + blen;
    if (cap < total)
        return 0;
    const CrossShardCtlHeader h{kOptCtlMagic, static_cast<uint8_t>(type), 0,
                                kOptCtlVersion, src_shard, dst_shard};
    std::memcpy(dst, &h, sizeof(h));
    std::memcpy(dst + sizeof(h), body, blen);
    return total;
}

OptCtlDecode opt_ctl_decode(const void* buf, uint32_t len,
                            OptCtlView* out) noexcept {
    if (buf == nullptr || out == nullptr || len < sizeof(CrossShardCtlHeader))
        return OptCtlDecode::TooShort;
    CrossShardCtlHeader h;
    std::memcpy(&h, buf, sizeof(h));
    if (h.magic != kOptCtlMagic)
        return OptCtlDecode::BadMagic;
    if (h.version != kOptCtlVersion)
        return OptCtlDecode::BadVersion;
    const auto type = static_cast<OptCtlType>(h.type);
    const uint32_t blen = opt_body_len(type);
    if (blen == 0)
        return OptCtlDecode::UnknownType;
    if (len < sizeof(h) + blen)
        return OptCtlDecode::TooShort;
    const uint8_t* p = static_cast<const uint8_t*>(buf) + sizeof(h);
    out->type = type;
    out->src_shard = h.src_shard;
    out->dst_shard = h.dst_shard;
    switch (type) {
        case OptCtlType::TryMatch:
            std::memcpy(&out->try_, p, sizeof(out->try_));
            break;
        case OptCtlType::TryAck:
            std::memcpy(&out->ack, p, sizeof(out->ack));
            break;
        case OptCtlType::TryNack:
            std::memcpy(&out->nack, p, sizeof(out->nack));
            break;
        case OptCtlType::Unwind:
            std::memcpy(&out->unwind, p, sizeof(out->unwind));
            break;
        case OptCtlType::UnwindAck:
            std::memcpy(&out->uack, p, sizeof(out->uack));
            break;
    }
    return OptCtlDecode::Ok;
}

// --- OptimisticShardCoordinator ---------------------------------------------------

OptimisticShardCoordinator::OptimisticShardCoordinator(
    IpcChannel* ctl, Wal* wal, OptimisticCoordinatorOptions opts) noexcept
    : ctl_(ctl), wal_(wal), opts_(opts) {}

uint32_t OptimisticShardCoordinator::hash_id(BasketOpId id) noexcept {
    uint64_t v = id.hi * 0x9E3779B97F4A7C15ULL + id.lo;
    v ^= v >> 30;
    v *= 0xBF58476D1CE4E5B9ULL;
    v ^= v >> 27;
    v *= 0x94D049BB133111EBULL;
    v ^= v >> 31;
    return static_cast<uint32_t>(v >> 32) ^ static_cast<uint32_t>(v);
}

// --- op table ---------------------------------------------------------------------

OptimisticShardCoordinator::OpRec* OptimisticShardCoordinator::find_op(
    BasketOpId id) noexcept {
    uint32_t i = hash_id(id) & (kMaxOps - 1);
    for (uint32_t n = 0; n < kMaxOps; ++n) {
        if (!ops_[i].occupied)
            return nullptr;
        if (ops_[i].id == id)
            return &ops_[i];
        i = (i + 1) & (kMaxOps - 1);
    }
    return nullptr;
}

const OptimisticShardCoordinator::OpRec* OptimisticShardCoordinator::find_op(
    BasketOpId id) const noexcept {
    return const_cast<OptimisticShardCoordinator*>(this)->find_op(id);
}

OptimisticShardCoordinator::OpRec* OptimisticShardCoordinator::insert_op(
    BasketOpId id) noexcept {
    uint32_t i = hash_id(id) & (kMaxOps - 1);
    int32_t tomb = -1;
    for (uint32_t n = 0; n < kMaxOps; ++n) {
        OpRec& s = ops_[i];
        if (s.occupied && s.id == id)
            return &s;
        if (!s.occupied)
            break;
        const auto st = static_cast<OptStatus>(s.state);
        if (tomb < 0 && (st == OptStatus::Committed ||
                         st == OptStatus::Compensated ||
                         st == OptStatus::Failed))
            tomb = static_cast<int32_t>(i);
        i = (i + 1) & (kMaxOps - 1);
    }
    if (tomb >= 0)
        return &ops_[tomb];
    if (!ops_[i].occupied)
        return &ops_[i];
    return nullptr;
}

// --- participant fill table ---------------------------------------------------------

OptimisticShardCoordinator::FillRec* OptimisticShardCoordinator::find_fill(
    BasketOpId id, uint32_t leg_index) noexcept {
    const uint32_t k = hash_id(id) ^ (leg_index * 0x9E3779B9u);
    uint32_t i = k & (kMaxPartFills - 1);
    for (uint32_t n = 0; n < kMaxPartFills; ++n) {
        const FillRec& s = fills_[i];
        if (s.state == static_cast<uint8_t>(FillSt::Empty))
            return nullptr;
        if (s.op_id == id && s.leg_index == leg_index)
            return &fills_[i];
        i = (i + 1) & (kMaxPartFills - 1);
    }
    return nullptr;
}

const OptimisticShardCoordinator::FillRec*
OptimisticShardCoordinator::find_fill(BasketOpId id,
                                      uint32_t leg_index) const noexcept {
    return const_cast<OptimisticShardCoordinator*>(this)->find_fill(id,
                                                                  leg_index);
}

OptimisticShardCoordinator::FillRec* OptimisticShardCoordinator::insert_fill(
    BasketOpId id, uint32_t leg_index) noexcept {
    const uint32_t k = hash_id(id) ^ (leg_index * 0x9E3779B9u);
    uint32_t i = k & (kMaxPartFills - 1);
    int32_t tomb = -1;
    for (uint32_t n = 0; n < kMaxPartFills; ++n) {
        FillRec& s = fills_[i];
        const auto st = static_cast<FillSt>(s.state);
        if (st != FillSt::Empty && s.op_id == id && s.leg_index == leg_index)
            return &s;
        if (st == FillSt::Empty)
            break;
        if (tomb < 0 && (st == FillSt::Unwound || st == FillSt::Denied))
            tomb = static_cast<int32_t>(i);
        i = (i + 1) & (kMaxPartFills - 1);
    }
    if (tomb >= 0)
        return &fills_[tomb];
    if (static_cast<FillSt>(fills_[i].state) == FillSt::Empty)
        return &fills_[i];
    return nullptr;
}

// --- public API -----------------------------------------------------------------------

uint32_t OptimisticShardCoordinator::coordinator_shard(const OptLegSpec* legs,
                                                     uint32_t n) noexcept {
    uint32_t m = 0xFFFFFFFFu;
    if (legs == nullptr)
        return m;
    for (uint32_t i = 0; i < n; ++i)
        if (legs[i].shard_id < m)
            m = legs[i].shard_id;
    return m;
}

OptResult OptimisticShardCoordinator::submit(const OptLegSpec* legs,
                                             uint32_t leg_count,
                                             uint64_t account,
                                             BasketOpId op_id,
                                             uint64_t now_ns) noexcept {
    OptResult rej{};
    rej.op_id = op_id;
    rej.status = static_cast<uint8_t>(OptStatus::Rejected);
    rej.code = static_cast<uint8_t>(OptCode::BadArgs);
    rej.leg_count =
        static_cast<uint8_t>(leg_count > kMaxLegs ? kMaxLegs : leg_count);

    if (legs == nullptr || leg_count == 0 || leg_count > kMaxLegs ||
        account == 0 || basket_op_id_is_nil(op_id)) {
        emit_metrics();
        return rej;
    }
    for (uint32_t i = 0; i < leg_count; ++i) {
        if (legs[i].qty_units <= 0 || legs[i].account_id == 0 ||
            legs[i].side > 1) {
            emit_metrics();
            return rej;
        }
    }
    if (coordinator_shard(legs, leg_count) != opts_.shard_id) {
        rej.code = static_cast<uint8_t>(OptCode::NotCoordinator);
        emit_metrics();
        return rej;
    }
    if (const OpRec* existing = find_op(op_id))
        return snapshot(*existing);  // operation_id dedup — cached result

    OpRec* op = insert_op(op_id);
    if (op == nullptr) {
        rej.code = static_cast<uint8_t>(OptCode::CapacityExceeded);
        emit_metrics();
        return rej;
    }
    *op = OpRec{};
    op->id = op_id;
    op->account = account;
    op->t_submit_ns = now_ns;
    op->match_deadline_ns = now_ns + opts_.match_budget_ns;   // +500µs
    op->unwind_deadline_ns = 0;
    op->state = static_cast<uint8_t>(OptStatus::Matching);
    op->code = static_cast<uint8_t>(OptCode::Ok);
    op->leg_count = static_cast<uint8_t>(leg_count);
    op->occupied = 1;
    ++metrics_.try_total;
    ++live_ops_;

    if (wal_ != nullptr && !wal_begin(*op)) {
        ++metrics_.wal_failures;
        finalize_op(*op, OptStatus::Failed, OptCode::WalFailure, now_ns);
        emit_metrics();
        return snapshot(*op);
    }

    // Parallel non-blocking TRY_MATCH to every participant — all frames are
    // issued inside this single call (parallel dispatch, spec §2.2a).
    bool any_reject = false;
    for (uint32_t i = 0; i < leg_count; ++i) {
        LegRec& l = op->legs[i];
        l.account_id = legs[i].account_id;
        l.order_id = legs[i].order_id;
        l.qty_units = legs[i].qty_units;
        l.limit_price_ticks = legs[i].limit_price_ticks;
        l.shard_id = legs[i].shard_id;
        l.instrument_id = legs[i].instrument_id;
        l.side = legs[i].side;
        l.local = (legs[i].shard_id == opts_.shard_id) ? 1 : 0;
        l.state = static_cast<uint8_t>(LegSt::Pending);
        if (l.local) {
            OptTryMatchBody m{};
            m.op_hi = op_id.hi;
            m.op_lo = op_id.lo;
            m.account_id = l.account_id;
            m.order_id = l.order_id;
            m.qty_units = l.qty_units;
            m.limit_price_ticks = l.limit_price_ticks;
            m.leg_index = i;
            m.instrument_id = l.instrument_id;
            m.side = l.side;
            FillRec* rec =
                local_try_match(m, opts_.shard_id, /*emit_wire_ack=*/false);
            if (rec != nullptr &&
                static_cast<FillSt>(rec->state) == FillSt::Filled &&
                rec->filled_qty >= l.qty_units) {
                l.filled_qty = rec->filled_qty;
                l.fill_vwap = rec->vwap_ticks;
                l.state = static_cast<uint8_t>(LegSt::Filled);
            } else {
                l.filled_qty = (rec != nullptr) ? rec->filled_qty : 0;
                l.state = static_cast<uint8_t>(LegSt::Rejected);
                any_reject = true;
            }
        } else {
            OptTryMatchBody m{};
            m.op_hi = op_id.hi;
            m.op_lo = op_id.lo;
            m.account_id = l.account_id;
            m.order_id = l.order_id;
            m.qty_units = l.qty_units;
            m.limit_price_ticks = l.limit_price_ticks;
            m.leg_index = i;
            m.instrument_id = l.instrument_id;
            m.side = l.side;
            if (!send_frame(OptCtlType::TryMatch, &m, l.shard_id)) {
                l.state = static_cast<uint8_t>(LegSt::Rejected);
                any_reject = true;
            }
        }
    }

    if (any_reject)
        begin_unwind(*op, OptCode::LegRejected, OptUnwindReason::PeerRejected,
                     now_ns);
    else
        maybe_resolve(*op, now_ns);  // all-local basket can finish inline

    emit_metrics();
    return snapshot(*op);
}

OptResult OptimisticShardCoordinator::status(BasketOpId op_id) const noexcept {
    const OpRec* op = find_op(op_id);
    OptResult r{};
    r.op_id = op_id;
    r.status = static_cast<uint8_t>(OptStatus::Unknown);
    r.code = static_cast<uint8_t>(OptCode::Ok);
    return op ? snapshot(*op) : r;
}

bool OptimisticShardCoordinator::cancel(BasketOpId op_id,
                                        uint64_t now_ns) noexcept {
    OpRec* op = find_op(op_id);
    if (op == nullptr)
        return false;
    const auto st = static_cast<OptStatus>(op->state);
    if (st == OptStatus::Matching || st == OptStatus::Unwinding)
        begin_unwind(*op, OptCode::Cancelled, OptUnwindReason::Cancelled,
                     now_ns);
    emit_metrics();
    return true;
}

int64_t OptimisticShardCoordinator::open_fill_qty(BasketOpId op_id,
                                                  uint32_t leg_index) const
    noexcept {
    const FillRec* r = find_fill(op_id, leg_index);
    if (r == nullptr ||
        static_cast<FillSt>(r->state) != FillSt::Filled)
        return 0;
    return r->filled_qty;
}

void OptimisticShardCoordinator::set_match_exec(OptMatchExec fn,
                                                void* ctx) noexcept {
    match_exec_ = fn;
    match_ctx_ = ctx;
}
void OptimisticShardCoordinator::set_unwind_exec(OptUnwindExec fn,
                                                 void* ctx) noexcept {
    unwind_exec_ = fn;
    unwind_ctx_ = ctx;
}
void OptimisticShardCoordinator::set_gl_sink(OptGlSink fn,
                                             void* ctx) noexcept {
    gl_sink_ = fn;
    gl_ctx_ = ctx;
}
void OptimisticShardCoordinator::set_metrics_sink(OptimisticMetricsSink fn,
                                                  void* ctx) noexcept {
    met_sink_ = fn;
    met_ctx_ = ctx;
}

// --- engine pump ----------------------------------------------------------------------

void OptimisticShardCoordinator::on_time_tick(uint64_t now_ns) noexcept {
    if (ctl_ != nullptr && ctl_->is_open()) {
        uint8_t buf[kOptCtlMaxFrame];
        for (uint32_t i = 0; i < 64; ++i) {
            const int32_t n = ctl_->poll(buf, sizeof(buf));
            if (n > 0) {
                on_frame(buf, static_cast<uint32_t>(n), now_ns);
            } else {
                if (n < 0)
                    ++metrics_.bad_frames;
                break;
            }
        }
    }
    // Per-tick enforcement (O(live)): the 500µs match window and the 1s
    // unwind deadline — no reaper interval here, the budget IS the cadence.
    for (uint32_t i = 0; i < kMaxOps; ++i) {
        OpRec& op = ops_[i];
        if (!op.occupied)
            continue;
        const auto st = static_cast<OptStatus>(op.state);
        if (st == OptStatus::Matching && now_ns > op.match_deadline_ns) {
            for (uint32_t li = 0; li < op.leg_count; ++li) {
                if (static_cast<LegSt>(op.legs[li].state) == LegSt::Pending) {
                    op.legs[li].state =
                        static_cast<uint8_t>(LegSt::Rejected);
                    ++metrics_.timeout_total;
                }
            }
            begin_unwind(op, OptCode::LegTimeout,
                         OptUnwindReason::PeerTimeout, now_ns);
        } else if (st == OptStatus::Unwinding &&
                   now_ns > op.unwind_deadline_ns) {
            // Unwind unconfirmed past budget: count every still-open leg as
            // an orphan — loud failure, never silent.
            for (uint32_t li = 0; li < op.leg_count; ++li) {
                LegRec& l = op.legs[li];
                if (static_cast<LegSt>(l.state) == LegSt::UnwindSent) {
                    ++metrics_.orphan_legs_total;
                    l.state = static_cast<uint8_t>(LegSt::Unwound);
                }
            }
            finalize_op(op, OptStatus::Failed,
                        static_cast<OptCode>(op.code), now_ns);
            emit_gl_posting(op, now_ns);
        }
    }
    // Zero-orphan restart sweep: fills restored from WAL carry no commit
    // context — the pessimistic model liquidates them immediately.
    for (uint32_t i = 0; i < kMaxPartFills; ++i) {
        FillRec& r = fills_[i];
        if (r.recovered != 0 &&
            static_cast<FillSt>(r.state) == FillSt::Filled)
            local_unwind(r, OptUnwindReason::RecoveryUnwind,
                         /*send_wire_ack=*/false);
    }
    emit_metrics();
}

// --- coordinator transitions -------------------------------------------------------------

void OptimisticShardCoordinator::maybe_resolve(OpRec& op,
                                               uint64_t now_ns) noexcept {
    const auto st = static_cast<OptStatus>(op.state);
    if (st == OptStatus::Matching) {
        for (uint32_t i = 0; i < op.leg_count; ++i)
            if (static_cast<LegSt>(op.legs[i].state) != LegSt::Filled)
                return;
        // All legs filled inside the 500µs window — fills are already
        // durable on participants; nothing further to commit.
        finalize_op(op, OptStatus::Committed, OptCode::Ok, now_ns);
        return;
    }
    if (st == OptStatus::Unwinding) {
        for (uint32_t i = 0; i < op.leg_count; ++i) {
            const auto ls = static_cast<LegSt>(op.legs[i].state);
            if (ls != LegSt::Rejected && ls != LegSt::Unwound)
                return;
        }
        finalize_op(op, OptStatus::Compensated,
                    static_cast<OptCode>(op.code), now_ns);
        emit_gl_posting(op, now_ns);
    }
}

void OptimisticShardCoordinator::begin_unwind(OpRec& op, OptCode code,
                                              OptUnwindReason reason,
                                              uint64_t now_ns) noexcept {
    const auto st = static_cast<OptStatus>(op.state);
    if (st == OptStatus::Committed || st == OptStatus::Compensated ||
        st == OptStatus::Failed)
        return;
    if (st != OptStatus::Unwinding) {
        op.state = static_cast<uint8_t>(OptStatus::Unwinding);
        op.code = static_cast<uint8_t>(code);
        op.unwind_reason = static_cast<uint8_t>(reason);
        op.unwind_deadline_ns = now_ns + opts_.unwind_deadline_ns;
    }
    for (uint32_t i = 0; i < op.leg_count; ++i) {
        LegRec& l = op.legs[i];
        const auto ls = static_cast<LegSt>(l.state);
        if (ls == LegSt::Unwound || ls == LegSt::UnwindSent)
            continue;  // already resolved / unwind in flight
        if (l.filled_qty <= 0) {
            // No fill on the participant — nothing to liquidate (the leg is
            // normalized to Rejected, whether it arrived Rejected, Pending,
            // or clean). A late TryAck claiming fills is handled by the
            // zero-drift path in on_try_ack.
            l.state = static_cast<uint8_t>(LegSt::Rejected);
            continue;
        }
        // NOTE: a leg Rejected with filled_qty > 0 (partial fill) still
        // holds a live position — the filled_qty check MUST precede the
        // terminal-state skip or the partial orphans (zero-orphan inv.).
        if (l.local) {
            ++metrics_.unwind_orders_total;  // in-process synthetic order
            FillRec* rec = find_fill(op.id, i);
            if (rec != nullptr &&
                static_cast<FillSt>(rec->state) == FillSt::Filled) {
                const int64_t before = rec->filled_qty;
                local_unwind(*rec, reason, /*send_wire_ack=*/false);
                const int64_t slip = leg_slippage(
                    l, before - rec->filled_qty, rec->vwap_ticks);
                int64_t sum;
                if (safe_math::try_add(op.slippage_ticks, slip, sum))
                    op.slippage_ticks = sum;
                else
                    op.slippage_ticks = slip > 0 ? INT64_MAX : INT64_MIN;
                metrics_.unwind_slippage_ticks_sum += slip;
                ++metrics_.unwind_acks_total;  // synchronous ack
                if (rec->filled_qty > 0)
                    ++metrics_.orphan_legs_total;  // partial local unwind
            } else {
                // The leg claims a fill but no participant record exists —
                // cannot liquidate: loud orphan, never silently "clean".
                ++metrics_.orphan_legs_total;
            }
            l.state = static_cast<uint8_t>(LegSt::Unwound);
            continue;
        }
        OptUnwindBody b{};
        b.op_hi = op.id.hi;
        b.op_lo = op.id.lo;
        b.leg_index = i;
        b.reason = static_cast<uint8_t>(reason);
        b.qty_units = l.filled_qty;
        (void)send_frame(OptCtlType::Unwind, &b, l.shard_id);
        ++metrics_.unwind_orders_total;
        l.state = static_cast<uint8_t>(LegSt::UnwindSent);
    }
    maybe_resolve(op, now_ns);
}

void OptimisticShardCoordinator::finalize_op(OpRec& op, OptStatus st,
                                             OptCode code,
                                             uint64_t now_ns) noexcept {
    const auto prev = static_cast<OptStatus>(op.state);
    if ((prev == OptStatus::Matching || prev == OptStatus::Unwinding) &&
        live_ops_ > 0)
        --live_ops_;
    op.state = static_cast<uint8_t>(st);
    op.code = static_cast<uint8_t>(code);
    (void)wal_outcome(op);  // outcome WAL failure is not fatal — tombstone
                            // already correct; audit gap noted.
    if (st == OptStatus::Committed)
        ++metrics_.committed_total;
    else if (st == OptStatus::Compensated)
        ++metrics_.compensated_total;
    else if (st == OptStatus::Failed)
        ++metrics_.failed_total;
    (void)now_ns;
}

OptResult OptimisticShardCoordinator::snapshot(const OpRec& op) const noexcept {
    OptResult r{};
    r.op_id = op.id;
    r.slippage_ticks = op.slippage_ticks;
    r.status = op.state;
    r.code = op.code;
    r.leg_count = op.leg_count;
    for (uint32_t i = 0; i < op.leg_count; ++i) {
        const auto ls = static_cast<LegSt>(op.legs[i].state);
        if (ls == LegSt::Filled)
            ++r.legs_filled;
        else if (ls == LegSt::Unwound)
            ++r.legs_unwound;
    }
    return r;
}

// --- participant transitions --------------------------------------------------------------

OptimisticShardCoordinator::FillRec*
OptimisticShardCoordinator::local_try_match(const OptTryMatchBody& m,
                                            uint64_t coord_shard,
                                            bool emit_wire_ack) noexcept {
    const BasketOpId id{m.op_hi, m.op_lo};
    FillRec* rec = find_fill(id, m.leg_index);
    if (rec != nullptr) {
        const auto st = static_cast<FillSt>(rec->state);
        if (st == FillSt::Filled) {
            if (emit_wire_ack) {
                OptTryAckBody a{};
                a.op_hi = id.hi;
                a.op_lo = id.lo;
                a.leg_index = m.leg_index;
                a.filled_qty = rec->filled_qty;
                a.vwap_ticks = rec->vwap_ticks;
                (void)send_frame(OptCtlType::TryAck, &a, coord_shard);
            }
            return rec;  // idempotent re-ack — never fills twice
        }
        if (emit_wire_ack) {
            OptTryNackBody n{};
            n.op_hi = id.hi;
            n.op_lo = id.lo;
            n.leg_index = m.leg_index;
            n.reason = static_cast<uint32_t>(OptNackReason::AlreadyDone);
            (void)send_frame(OptCtlType::TryNack, &n, coord_shard);
        }
        return rec;
    }
    if (m.account_id == 0 || m.qty_units <= 0 || m.side > 1) {
        ++metrics_.bad_frames;
        return nullptr;
    }
    // Execute immediately — no lock is ever taken. Missing executor =
    // fail-closed reject.
    int64_t vwap = 0;
    const int64_t filled =
        (match_exec_ != nullptr) ? match_exec_(match_ctx_, m, &vwap) : 0;
    if (filled <= 0) {
        FillRec* t = insert_fill(id, m.leg_index);
        if (t != nullptr) {
            *t = FillRec{};
            t->op_id = id;
            t->coordinator_shard = coord_shard;
            t->leg_index = m.leg_index;
            t->state = static_cast<uint8_t>(FillSt::Denied);
        }
        if (emit_wire_ack) {
            OptTryNackBody n{};
            n.op_hi = id.hi;
            n.op_lo = id.lo;
            n.leg_index = m.leg_index;
            n.reason = static_cast<uint32_t>(OptNackReason::Rejected);
            (void)send_frame(OptCtlType::TryNack, &n, coord_shard);
        }
        return nullptr;
    }
    FillRec* s = insert_fill(id, m.leg_index);
    if (s == nullptr) {
        // Cannot even record the fill — fail closed: NACK so the
        // coordinator unwinds nothing and rejects cleanly. The executor
        // already matched; the in-flight fill is covered by the
        // coordinator's zero-drift unwind path (it will not know the qty,
        // so over-report filled=0 here is wrong — report the truth via a
        // Nack for bookkeeping; integrator must size kMaxPartFills >= peak
        // in-flight legs).
        if (emit_wire_ack) {
            OptTryNackBody n{};
            n.op_hi = id.hi;
            n.op_lo = id.lo;
            n.leg_index = m.leg_index;
            n.reason = static_cast<uint32_t>(OptNackReason::Overload);
            (void)send_frame(OptCtlType::TryNack, &n, coord_shard);
        }
        return nullptr;
    }
    *s = FillRec{};
    s->op_id = id;
    s->coordinator_shard = coord_shard;
    s->account_id = m.account_id;
    s->order_id = m.order_id;
    s->filled_qty = filled;
    s->vwap_ticks = vwap;
    s->leg_index = m.leg_index;
    s->instrument_id = m.instrument_id;
    s->side = m.side;
    // WAL the fill BEFORE acking — a fill we cannot persist is a position
    // that could orphan on restart (zero-orphan invariant).
    if (!wal_fill(*s)) {
        ++metrics_.wal_failures;
        s->state = static_cast<uint8_t>(FillSt::Denied);
        if (emit_wire_ack) {
            OptTryNackBody n{};
            n.op_hi = id.hi;
            n.op_lo = id.lo;
            n.leg_index = m.leg_index;
            n.reason = static_cast<uint32_t>(OptNackReason::Rejected);
            (void)send_frame(OptCtlType::TryNack, &n, coord_shard);
        }
        return nullptr;
    }
    s->state = static_cast<uint8_t>(FillSt::Filled);
    if (emit_wire_ack) {
        OptTryAckBody a{};
        a.op_hi = id.hi;
        a.op_lo = id.lo;
        a.leg_index = m.leg_index;
        a.filled_qty = filled;
        a.vwap_ticks = vwap;
        (void)send_frame(OptCtlType::TryAck, &a, coord_shard);
    }
    return s;
}

void OptimisticShardCoordinator::local_unwind(FillRec& r,
                                              OptUnwindReason reason,
                                              bool send_wire_ack) noexcept {
    if (static_cast<FillSt>(r.state) != FillSt::Filled) {
        if (send_wire_ack) {
            OptUnwindAckBody a{};
            a.op_hi = r.op_id.hi;
            a.op_lo = r.op_id.lo;
            a.leg_index = r.leg_index;
            a.unwound_qty = 0;
            a.vwap_ticks = 0;
            (void)send_frame(OptCtlType::UnwindAck, &a,
                             r.coordinator_shard);
        }
        return;  // idempotent — nothing held
    }
    // COMPENSATE_UNWIND: synthetic market order liquidating the fill. The
    // executor reports the unwind VWAP; a short unwind leaves the remainder
    // in the rec (still Filled, open_fill_qty > 0 — observable orphan until
    // a later unwind completes it).
    int64_t uvwap = 0;
    const int64_t unwound =
        (unwind_exec_ != nullptr)
            ? unwind_exec_(unwind_ctx_, r.op_id, r.leg_index, r.filled_qty,
                           &uvwap)
            : 0;
    if (unwound > 0) {
        r.vwap_ticks = uvwap;  // last-unwind price (audit)
        if (unwound >= r.filled_qty) {
            r.filled_qty = 0;
            r.state = static_cast<uint8_t>(FillSt::Unwound);
        } else {
            r.filled_qty -= unwound;  // partial — remainder still open
        }
        (void)wal_unwind(r, reason, unwound, uvwap);
    }
    if (send_wire_ack) {
        OptUnwindAckBody a{};
        a.op_hi = r.op_id.hi;
        a.op_lo = r.op_id.lo;
        a.leg_index = r.leg_index;
        a.unwound_qty = unwound > 0 ? unwound : 0;
        a.vwap_ticks = uvwap;
        (void)send_frame(OptCtlType::UnwindAck, &a, r.coordinator_shard);
    }
}

int64_t OptimisticShardCoordinator::leg_slippage(const LegRec& l,
                                                 int64_t unwind_qty,
                                                 int64_t unwind_vwap) noexcept {
    if (unwind_qty <= 0)
        return 0;
    // Positive = loss charged to 5010: BUY fills unwound by SELLing lower
    // (fill_vwap > unwind_vwap), SELL fills unwound by BUYing higher.
    int64_t diff = (l.side == 0) ? (l.fill_vwap - unwind_vwap)
                                 : (unwind_vwap - l.fill_vwap);
    const safe_math::int128_t prod =
        safe_math::mul_wide_i64(unwind_qty, diff);
    const safe_math::int128_t scaled = prod / 100'000'000;
    int64_t out = 0;
    if (!safe_math::try_narrow_i128(scaled, out))
        return diff > 0 ? INT64_MAX : INT64_MIN;  // fail-loud, never wrap
    return out;
}

void OptimisticShardCoordinator::emit_gl_posting(const OpRec& op,
                                                 uint64_t now_ns) noexcept {
    // GL record of the aggregate unwind slippage — posting proper is the
    // Phase-03 double-entry task (3.3.6); here we emit the signed record.
    if (gl_sink_ == nullptr)
        return;
    CrossShardGlPosting p{};
    p.op_id = op.id;
    std::memcpy(p.account, kGlCrossShardExecDiff,
                sizeof(kGlCrossShardExecDiff));  // includes NUL
    p.amount_ticks = op.slippage_ticks;
    p.ts_ns = now_ns;
    p.reason = op.unwind_reason;
    gl_sink_(gl_ctx_, p);
}

void OptimisticShardCoordinator::emit_metrics() noexcept {
    metrics_.ops_active = live_ops_;
    if (met_sink_ != nullptr)
        met_sink_(met_ctx_, metrics_);
}

// --- wire + inbound ----------------------------------------------------------------

bool OptimisticShardCoordinator::send_frame(OptCtlType t, const void* body,
                                            uint32_t dst) noexcept {
    if (ctl_ == nullptr || !ctl_->is_open())
        return false;
    uint8_t buf[kOptCtlMaxFrame];
    const uint32_t n =
        opt_ctl_encode(buf, sizeof(buf), t, body, opts_.shard_id, dst);
    return n != 0 && ctl_->send(buf, n);
}

void OptimisticShardCoordinator::on_frame(const uint8_t* buf, uint32_t len,
                                          uint64_t now_ns) noexcept {
    OptCtlView v;
    if (opt_ctl_decode(buf, len, &v) != OptCtlDecode::Ok) {
        ++metrics_.bad_frames;
        return;
    }
    // Point-to-point delivery: a frame addressed elsewhere must never be
    // acted on here — without this guard a fan-out/misrouted ctl frame
    // would execute the same leg twice (Phase-3 Task 4 wiring proof).
    if (v.dst_shard != opts_.shard_id) {
        ++metrics_.bad_frames;
        return;
    }
    switch (v.type) {
        case OptCtlType::TryMatch:  on_try_match(v, now_ns);  break;
        case OptCtlType::TryAck:    on_try_ack(v, now_ns);    break;
        case OptCtlType::TryNack:   on_try_nack(v, now_ns);   break;
        case OptCtlType::Unwind:    on_unwind(v, now_ns);     break;
        case OptCtlType::UnwindAck: on_unwind_ack(v, now_ns); break;
    }
}

void OptimisticShardCoordinator::on_try_match(const OptCtlView& v,
                                              uint64_t now_ns) noexcept {
    (void)now_ns;
    (void)local_try_match(v.try_, v.src_shard, /*emit_wire_ack=*/true);
}

void OptimisticShardCoordinator::on_try_ack(const OptCtlView& v,
                                            uint64_t now_ns) noexcept {
    const BasketOpId id{v.ack.op_hi, v.ack.op_lo};
    OpRec* op = find_op(id);
    if (op == nullptr) {
        if (v.ack.filled_qty > 0) {
            // Fill for an op we never issued — zero-drift: unwind it.
            OptUnwindBody b{};
            b.op_hi = id.hi;
            b.op_lo = id.lo;
            b.leg_index = v.ack.leg_index;
            b.reason = static_cast<uint8_t>(OptUnwindReason::PeerRejected);
            b.qty_units = v.ack.filled_qty;
            (void)send_frame(OptCtlType::Unwind, &b, v.src_shard);
        }
        return;
    }
    if (v.ack.leg_index >= op->leg_count) {
        ++metrics_.bad_frames;
        return;
    }
    LegRec& l = op->legs[v.ack.leg_index];
    const auto lst = static_cast<LegSt>(l.state);
    if (lst == LegSt::Pending) {
        if (v.ack.filled_qty >= l.qty_units) {
            l.filled_qty = v.ack.filled_qty;
            l.fill_vwap = v.ack.vwap_ticks;
            l.state = static_cast<uint8_t>(LegSt::Filled);
            maybe_resolve(*op, now_ns);
        } else {
            // Partial fill = failed leg — the partial position is recorded
            // so compensation liquidates it too.
            l.filled_qty = v.ack.filled_qty;
            l.fill_vwap = v.ack.vwap_ticks;
            l.state = static_cast<uint8_t>(LegSt::Rejected);
            begin_unwind(*op, OptCode::LegRejected,
                         OptUnwindReason::PartialFill, now_ns);
        }
        return;
    }
    // Late/duplicate ACK: nothing to do for Filled/UnwindSent/Unwound. For
    // a leg we already gave up on, a fill claim means the participant holds
    // a position we didn't count — add it and unwind (zero drift).
    if (lst == LegSt::Rejected && v.ack.filled_qty > l.filled_qty) {
        l.filled_qty = v.ack.filled_qty;
        l.fill_vwap = v.ack.vwap_ticks;
        const auto ost = static_cast<OptStatus>(op->state);
        if (ost == OptStatus::Unwinding) {
            OptUnwindBody b{};
            b.op_hi = id.hi;
            b.op_lo = id.lo;
            b.leg_index = v.ack.leg_index;
            b.reason = static_cast<uint8_t>(op->unwind_reason);
            b.qty_units = l.filled_qty;
            (void)send_frame(OptCtlType::Unwind, &b, l.shard_id);
            ++metrics_.unwind_orders_total;
            l.state = static_cast<uint8_t>(LegSt::UnwindSent);
        }
    }
}

void OptimisticShardCoordinator::on_try_nack(const OptCtlView& v,
                                             uint64_t now_ns) noexcept {
    ++metrics_.nack_total;
    const BasketOpId id{v.nack.op_hi, v.nack.op_lo};
    OpRec* op = find_op(id);
    if (op == nullptr || v.nack.leg_index >= op->leg_count)
        return;
    LegRec& l = op->legs[v.nack.leg_index];
    if (static_cast<LegSt>(l.state) == LegSt::Pending &&
        static_cast<OptStatus>(op->state) == OptStatus::Matching) {
        l.state = static_cast<uint8_t>(LegSt::Rejected);
        begin_unwind(*op, OptCode::LegRejected,
                     OptUnwindReason::PeerRejected, now_ns);
    }
}

void OptimisticShardCoordinator::on_unwind(const OptCtlView& v,
                                           uint64_t now_ns) noexcept {
    (void)now_ns;
    const BasketOpId id{v.unwind.op_hi, v.unwind.op_lo};
    FillRec* rec = find_fill(id, v.unwind.leg_index);
    if (rec == nullptr || rec->coordinator_shard != v.src_shard) {
        // Nothing held — confirm idempotently so coordinator bookkeeping
        // never wedges.
        OptUnwindAckBody a{};
        a.op_hi = id.hi;
        a.op_lo = id.lo;
        a.leg_index = v.unwind.leg_index;
        a.unwound_qty = 0;
        a.vwap_ticks = 0;
        (void)send_frame(OptCtlType::UnwindAck, &a, v.src_shard);
        return;
    }
    local_unwind(*rec, static_cast<OptUnwindReason>(v.unwind.reason),
                 /*send_wire_ack=*/true);
}

void OptimisticShardCoordinator::on_unwind_ack(const OptCtlView& v,
                                               uint64_t now_ns) noexcept {
    ++metrics_.unwind_acks_total;
    const BasketOpId id{v.uack.op_hi, v.uack.op_lo};
    OpRec* op = find_op(id);
    if (op == nullptr || v.uack.leg_index >= op->leg_count)
        return;  // late/stale — idempotent
    LegRec& l = op->legs[v.uack.leg_index];
    if (static_cast<LegSt>(l.state) != LegSt::UnwindSent)
        return;
    const int64_t slip =
        leg_slippage(l, v.uack.unwound_qty, v.uack.vwap_ticks);
    int64_t sum;
    if (safe_math::try_add(op->slippage_ticks, slip, sum))
        op->slippage_ticks = sum;
    else
        op->slippage_ticks = slip > 0 ? INT64_MAX : INT64_MIN;
    metrics_.unwind_slippage_ticks_sum += slip;
    if (v.uack.unwound_qty < l.filled_qty)
        ++metrics_.orphan_legs_total;  // partial unwind — remainder open
    l.state = static_cast<uint8_t>(LegSt::Unwound);
    maybe_resolve(*op, now_ns);
}

// --- recovery ------------------------------------------------------------------------

size_t OptimisticShardCoordinator::recover(WalReader& reader) noexcept {
    size_t applied = 0;
    WalEntryView v;
    while (reader.next(v) == WalScanStep::Entry) {
        const uint8_t t = static_cast<uint8_t>(v.type);
        if (t == kWalEvtOptBegin &&
            v.payload_len == sizeof(WalOptBeginPayload)) {
            WalOptBeginPayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            const BasketOpId id{p.op_hi, p.op_lo};
            OpRec* op = find_op(id);
            if (op == nullptr)
                op = insert_op(id);
            if (op != nullptr &&
                static_cast<OptStatus>(op->state) == OptStatus::Unknown) {
                *op = OpRec{};
                op->id = id;
                op->account = p.account_id;
                op->match_deadline_ns = p.match_deadline_ns;
                op->leg_count = static_cast<uint8_t>(
                    p.leg_count > kMaxLegs ? kMaxLegs : p.leg_count);
                op->state = static_cast<uint8_t>(OptStatus::Matching);
                op->occupied = 1;
                op->recovered = 1;
            }
            ++applied;
        } else if (t == kWalEvtOptFill &&
                   v.payload_len == sizeof(WalOptFillPayload)) {
            WalOptFillPayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            const BasketOpId id{p.op_hi, p.op_lo};
            FillRec* r = find_fill(id, p.leg_index);
            if (r == nullptr)
                r = insert_fill(id, p.leg_index);
            if (r != nullptr &&
                static_cast<FillSt>(r->state) == FillSt::Empty) {
                *r = FillRec{};
                r->op_id = id;
                r->coordinator_shard = p.coordinator_shard;
                r->account_id = p.account_id;
                r->order_id = p.order_id;
                r->filled_qty = p.filled_qty;
                r->vwap_ticks = p.vwap_ticks;
                r->leg_index = p.leg_index;
                r->instrument_id = p.instrument_id;
                r->side = p.side;
                r->state = static_cast<uint8_t>(FillSt::Filled);
                r->recovered = 1;  // first tick liquidates it
            }
            ++applied;
        } else if (t == kWalEvtOptUnwind &&
                   v.payload_len == sizeof(WalOptUnwindPayload)) {
            WalOptUnwindPayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            const BasketOpId id{p.op_hi, p.op_lo};
            FillRec* r = find_fill(id, p.leg_index);
            if (r != nullptr &&
                static_cast<FillSt>(r->state) == FillSt::Filled) {
                if (p.unwound_qty >= r->filled_qty) {
                    r->filled_qty = 0;
                    r->state = static_cast<uint8_t>(FillSt::Unwound);
                } else {
                    r->filled_qty -= p.unwound_qty;
                }
            }
            ++applied;
        } else if (t == kWalEvtOptOutcome &&
                   v.payload_len == sizeof(WalOptOutcomePayload)) {
            WalOptOutcomePayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            const BasketOpId id{p.op_hi, p.op_lo};
            OpRec* op = find_op(id);
            if (op == nullptr)
                op = insert_op(id);
            if (op != nullptr) {
                const auto st = static_cast<OptStatus>(op->state);
                if (st == OptStatus::Matching || st == OptStatus::Unwinding ||
                    st == OptStatus::Unknown) {
                    if (!op->occupied) {
                        op->id = id;
                        op->account = p.account_id;
                        op->occupied = 1;
                    }
                    op->state = p.status;
                    op->code = p.code;
                    op->slippage_ticks = p.slippage_ticks;
                    op->recovered = 1;
                }
            }
            ++applied;
        }
    }
    // Fail-closed terminal sweep — a non-terminal op never resumes.
    for (uint32_t i = 0; i < kMaxOps; ++i) {
        OpRec& op = ops_[i];
        if (!op.occupied)
            continue;
        const auto st = static_cast<OptStatus>(op.state);
        if (st == OptStatus::Matching || st == OptStatus::Unwinding) {
            op.state = static_cast<uint8_t>(OptStatus::Failed);
            op.code =
                static_cast<uint8_t>(OptCode::RecoveryInterrupted);
        }
    }
    emit_metrics();
    return applied;
}

// --- WAL --------------------------------------------------------------------------------

bool OptimisticShardCoordinator::wal_begin(const OpRec& op) noexcept {
    if (wal_ == nullptr)
        return true;
    WalOptBeginPayload p{};
    p.op_hi = op.id.hi;
    p.op_lo = op.id.lo;
    p.account_id = op.account;
    p.leg_count = op.leg_count;
    p.coordinator_shard = opts_.shard_id;
    p.match_deadline_ns = op.match_deadline_ns;
    return wal_->append(static_cast<WalEventType>(kWalEvtOptBegin), &p,
                        sizeof(p)) == WalStatus::Ok;
}

bool OptimisticShardCoordinator::wal_fill(const FillRec& r) noexcept {
    if (wal_ == nullptr)
        return true;
    WalOptFillPayload p{};
    p.op_hi = r.op_id.hi;
    p.op_lo = r.op_id.lo;
    p.coordinator_shard = r.coordinator_shard;
    p.account_id = r.account_id;
    p.order_id = r.order_id;
    p.filled_qty = r.filled_qty;
    p.vwap_ticks = r.vwap_ticks;
    p.leg_index = r.leg_index;
    p.instrument_id = r.instrument_id;
    p.side = r.side;
    return wal_->append(static_cast<WalEventType>(kWalEvtOptFill), &p,
                        sizeof(p)) == WalStatus::Ok;
}

bool OptimisticShardCoordinator::wal_unwind(const FillRec& r,
                                            OptUnwindReason reason,
                                            int64_t unwound_qty,
                                            int64_t vwap) noexcept {
    if (wal_ == nullptr)
        return true;
    WalOptUnwindPayload p{};
    p.op_hi = r.op_id.hi;
    p.op_lo = r.op_id.lo;
    p.leg_index = r.leg_index;
    p.reason = static_cast<uint8_t>(reason);
    p.unwound_qty = unwound_qty;
    p.vwap_ticks = vwap;
    return wal_->append(static_cast<WalEventType>(kWalEvtOptUnwind), &p,
                        sizeof(p)) == WalStatus::Ok;
}

bool OptimisticShardCoordinator::wal_outcome(const OpRec& op) noexcept {
    if (wal_ == nullptr)
        return true;
    WalOptOutcomePayload p{};
    p.op_hi = op.id.hi;
    p.op_lo = op.id.lo;
    p.account_id = op.account;
    p.slippage_ticks = op.slippage_ticks;
    p.duration_ns = 0;
    p.status = op.state;
    p.code = op.code;
    return wal_->append(static_cast<WalEventType>(kWalEvtOptOutcome), &p,
                        sizeof(p)) == WalStatus::Ok;
}

}  // namespace exch
