// Task 2.3.8 + Task 2.3.14 — CrossShardCoordinator implementation.
// See the header for the protocol, layered-deadline, dedup, reaper-cadence,
// metrics, and WAL contracts (spec §2.2a / §13.1 / §24 #214; supersession by
// Task 2.3.25 noted there).

#include "matching/CrossShardCoordinator.hpp"

#include "utils/safe_math.hpp"

namespace exch {

namespace {

// Fixed body size per wire type; 0 terminates dispatch (unknown type).
uint32_t basket_body_len(BasketCtlType t) noexcept {
    switch (t) {
        case BasketCtlType::ReserveReq:  return sizeof(BasketReserveReqBody);
        case BasketCtlType::ReserveAck:
        case BasketCtlType::ReserveNack: return sizeof(BasketVoteBody);
        case BasketCtlType::Commit:      return sizeof(BasketCommitBody);
        case BasketCtlType::CommitAck:   return sizeof(BasketCommitAckBody);
        case BasketCtlType::Release:     return sizeof(BasketReleaseBody);
        case BasketCtlType::ReleaseAck:  return sizeof(BasketReleaseAckBody);
    }
    return 0;
}

}  // namespace

const char* basket_ctl_decode_str(BasketCtlDecode r) noexcept {
    switch (r) {
        case BasketCtlDecode::Ok:          return "Ok";
        case BasketCtlDecode::TooShort:    return "TooShort";
        case BasketCtlDecode::BadMagic:    return "BadMagic";
        case BasketCtlDecode::BadVersion:  return "BadVersion";
        case BasketCtlDecode::UnknownType: return "UnknownType";
    }
    return "?";
}

uint32_t basket_ctl_encode(uint8_t* dst, uint32_t cap, BasketCtlType type,
                           const void* body, uint32_t src_shard,
                           uint32_t dst_shard) noexcept {
    const uint32_t blen = basket_body_len(type);
    if (dst == nullptr || body == nullptr || blen == 0)
        return 0;
    const uint32_t total = sizeof(CrossShardCtlHeader) + blen;
    if (cap < total)
        return 0;
    const CrossShardCtlHeader h{kBasketCtlMagic, static_cast<uint8_t>(type),
                                0, kBasketCtlVersion, src_shard, dst_shard};
    std::memcpy(dst, &h, sizeof(h));
    std::memcpy(dst + sizeof(h), body, blen);
    return total;
}

BasketCtlDecode basket_ctl_decode(const void* buf, uint32_t len,
                                  BasketCtlView* out) noexcept {
    if (buf == nullptr || out == nullptr || len < sizeof(CrossShardCtlHeader))
        return BasketCtlDecode::TooShort;
    CrossShardCtlHeader h;
    std::memcpy(&h, buf, sizeof(h));
    if (h.magic != kBasketCtlMagic)
        return BasketCtlDecode::BadMagic;
    if (h.version != kBasketCtlVersion)
        return BasketCtlDecode::BadVersion;
    const auto type = static_cast<BasketCtlType>(h.type);
    const uint32_t blen = basket_body_len(type);
    if (blen == 0)
        return BasketCtlDecode::UnknownType;
    if (len < sizeof(h) + blen)  // trailing pad tolerated (forward-compat)
        return BasketCtlDecode::TooShort;
    const uint8_t* p = static_cast<const uint8_t*>(buf) + sizeof(h);
    out->type = type;
    out->src_shard = h.src_shard;
    out->dst_shard = h.dst_shard;
    switch (type) {
        case BasketCtlType::ReserveReq:
            std::memcpy(&out->req, p, sizeof(out->req));
            break;
        case BasketCtlType::ReserveAck:
        case BasketCtlType::ReserveNack:
            std::memcpy(&out->vote, p, sizeof(out->vote));
            break;
        case BasketCtlType::Commit:
            std::memcpy(&out->commit, p, sizeof(out->commit));
            break;
        case BasketCtlType::CommitAck:
            std::memcpy(&out->cack, p, sizeof(out->cack));
            break;
        case BasketCtlType::Release:
            std::memcpy(&out->rel, p, sizeof(out->rel));
            break;
        case BasketCtlType::ReleaseAck:
            std::memcpy(&out->rack, p, sizeof(out->rack));
            break;
    }
    return BasketCtlDecode::Ok;
}

// --- CrossShardCoordinator ---------------------------------------------------

CrossShardCoordinator::CrossShardCoordinator(IpcChannel* ctl, Wal* wal,
                                             BasketCoordinatorOptions opts) noexcept
    : ctl_(ctl), wal_(wal), opts_(opts) {
    // Tables are trivially-constructed PODs; occupied=0 / account_id=0 /
    // state=Empty are the probe terminators — zero-init suffices.
}

uint32_t CrossShardCoordinator::hash_id(BasketOpId id) noexcept {
    // splitmix64 finalizer over hi+lo — op ids may be structured (shard
    // prefix, timestamps) so mix thoroughly before masking.
    uint64_t v = id.hi * 0x9E3779B97F4A7C15ULL + id.lo;
    v ^= v >> 30;
    v *= 0xBF58476D1CE4E5B9ULL;
    v ^= v >> 27;
    v *= 0x94D049BB133111EBULL;
    v ^= v >> 31;
    return static_cast<uint32_t>(v >> 32) ^ static_cast<uint32_t>(v);
}

// --- op table (coordinator role) ------------------------------------------------

CrossShardCoordinator::OpRec* CrossShardCoordinator::find_op(
    BasketOpId id) noexcept {
    uint32_t i = hash_id(id) & (kMaxOps - 1);
    for (uint32_t n = 0; n < kMaxOps; ++n) {
        if (!ops_[i].occupied)
            return nullptr;  // probe terminator
        if (ops_[i].id == id)
            return &ops_[i];
        i = (i + 1) & (kMaxOps - 1);
    }
    return nullptr;
}

const CrossShardCoordinator::OpRec* CrossShardCoordinator::find_op(
    BasketOpId id) const noexcept {
    return const_cast<CrossShardCoordinator*>(this)->find_op(id);
}

CrossShardCoordinator::OpRec* CrossShardCoordinator::insert_op(
    BasketOpId id) noexcept {
    uint32_t i = hash_id(id) & (kMaxOps - 1);
    int32_t tomb = -1;
    for (uint32_t n = 0; n < kMaxOps; ++n) {
        OpRec& s = ops_[i];
        if (s.occupied && s.id == id)
            return &s;
        if (!s.occupied)
            break;  // key absent proven; insert below
        const auto st = static_cast<BasketStatus>(s.state);
        if (tomb < 0 && (st == BasketStatus::Committed ||
                         st == BasketStatus::Compensated ||
                         st == BasketStatus::Failed))
            tomb = static_cast<int32_t>(i);  // first reusable tombstone
        i = (i + 1) & (kMaxOps - 1);
    }
    if (tomb >= 0)
        return &ops_[tomb];
    if (!ops_[i].occupied)
        return &ops_[i];
    return nullptr;  // full of live records — fail closed
}

// --- participant lock table -----------------------------------------------------

CrossShardCoordinator::PartRec* CrossShardCoordinator::find_part(
    BasketOpId id, uint32_t leg_index) noexcept {
    const uint32_t k = hash_id(id) ^ (leg_index * 0x9E3779B9u);
    uint32_t i = k & (kMaxPartRes - 1);
    for (uint32_t n = 0; n < kMaxPartRes; ++n) {
        const PartRec& s = parts_[i];
        if (s.state == static_cast<uint8_t>(PartSt::Empty))
            return nullptr;
        if (s.op_id == id && s.leg_index == leg_index)
            return &parts_[i];
        i = (i + 1) & (kMaxPartRes - 1);
    }
    return nullptr;
}

const CrossShardCoordinator::PartRec* CrossShardCoordinator::find_part(
    BasketOpId id, uint32_t leg_index) const noexcept {
    return const_cast<CrossShardCoordinator*>(this)->find_part(id, leg_index);
}

CrossShardCoordinator::PartRec* CrossShardCoordinator::insert_part(
    BasketOpId id, uint32_t leg_index) noexcept {
    const uint32_t k = hash_id(id) ^ (leg_index * 0x9E3779B9u);
    uint32_t i = k & (kMaxPartRes - 1);
    int32_t tomb = -1;
    for (uint32_t n = 0; n < kMaxPartRes; ++n) {
        PartRec& s = parts_[i];
        const auto st = static_cast<PartSt>(s.state);
        if (st != PartSt::Empty && s.op_id == id && s.leg_index == leg_index)
            return &s;
        if (st == PartSt::Empty)
            break;
        if (tomb < 0 && (st == PartSt::Denied || st == PartSt::Released ||
                         st == PartSt::Committed))
            tomb = static_cast<int32_t>(i);
        i = (i + 1) & (kMaxPartRes - 1);
    }
    if (tomb >= 0)
        return &parts_[tomb];
    if (static_cast<PartSt>(parts_[i].state) == PartSt::Empty)
        return &parts_[i];
    return nullptr;
}

// --- balance store -----------------------------------------------------------------

CrossShardCoordinator::AccountBal* CrossShardCoordinator::find_bal(
    uint64_t account_id) noexcept {
    uint32_t i = static_cast<uint32_t>(
                   (account_id * 0x9E3779B97F4A7C15ULL) >> 32) &
               (kMaxAccounts - 1);
    for (uint32_t n = 0; n < kMaxAccounts; ++n) {
        if (bals_[i].account_id == 0)
            return nullptr;
        if (bals_[i].account_id == account_id)
            return &bals_[i];
        i = (i + 1) & (kMaxAccounts - 1);
    }
    return nullptr;
}

const CrossShardCoordinator::AccountBal* CrossShardCoordinator::find_bal(
    uint64_t account_id) const noexcept {
    return const_cast<CrossShardCoordinator*>(this)->find_bal(account_id);
}

CrossShardCoordinator::AccountBal* CrossShardCoordinator::ensure_bal(
    uint64_t account_id) noexcept {
    uint32_t i = static_cast<uint32_t>(
                   (account_id * 0x9E3779B97F4A7C15ULL) >> 32) &
               (kMaxAccounts - 1);
    for (uint32_t n = 0; n < kMaxAccounts; ++n) {
        if (bals_[i].account_id == account_id)
            return &bals_[i];
        if (bals_[i].account_id == 0) {
            bals_[i].account_id = account_id;
            return &bals_[i];
        }
        i = (i + 1) & (kMaxAccounts - 1);
    }
    return nullptr;
}

void CrossShardCoordinator::set_balance(uint64_t account_id,
                                        int64_t available) noexcept {
    AccountBal* b = ensure_bal(account_id);
    if (b != nullptr)
        b->available = available;
}

void CrossShardCoordinator::set_balance_provider(
    int64_t (*fn)(void*, uint64_t) noexcept, void* ctx) noexcept {
    bal_fn_ = fn;
    bal_ctx_ = ctx;
}

int64_t CrossShardCoordinator::available(uint64_t account_id) const noexcept {
    if (bal_fn_ != nullptr)
        return bal_fn_(bal_ctx_, account_id);
    const AccountBal* b = find_bal(account_id);
    return b ? b->available : 0;  // unknown account -> 0 (fail closed)
}

int64_t CrossShardCoordinator::locked(uint64_t account_id) const noexcept {
    const AccountBal* b = find_bal(account_id);
    return b ? b->locked : 0;
}

void CrossShardCoordinator::set_leg_sink(BasketLegSink fn,
                                       void* ctx) noexcept {
    leg_sink_ = fn;
    leg_ctx_ = ctx;
}

void CrossShardCoordinator::set_metrics_sink(CrossShardMetricsSink fn,
                                             void* ctx) noexcept {
    met_sink_ = fn;
    met_ctx_ = ctx;
}

// --- public coordinator API ---------------------------------------------------

uint32_t CrossShardCoordinator::coordinator_shard(const BasketLegSpec* legs,
                                                  uint32_t n) noexcept {
    uint32_t m = 0xFFFFFFFFu;
    if (legs == nullptr)
        return m;
    for (uint32_t i = 0; i < n; ++i)
        if (legs[i].shard_id < m)
            m = legs[i].shard_id;
    return m;  // lowest participant shard coordinates (Task 2.3.8 rule)
}

BasketOpId CrossShardCoordinator::generate_op_id(uint64_t now_ns) noexcept {
    // Deterministic uniqueness per shard+process; callers with real UUID v4
    // supply their own bytes to submit() instead.
    const uint64_t hi_seed =
        (static_cast<uint64_t>(opts_.shard_id) << 48) ^ now_ns;
    BasketOpId id{0, 0};
    id.hi = hash_id(BasketOpId{hi_seed, ++id_seq_});
    id.hi = (static_cast<uint64_t>(id.hi) << 32) |
            (hi_seed & 0xFFFFFFFFull);
    id.lo = ++id_seq_;
    return id;
}

BasketResult CrossShardCoordinator::snapshot(const OpRec& op) const noexcept {
    BasketResult r{};
    r.op_id = op.id;
    r.status = op.state;
    r.code = op.code;
    r.leg_count = op.leg_count;
    for (uint32_t i = 0; i < op.leg_count; ++i) {
        const auto st = static_cast<LegSt>(op.legs[i].state);
        if (st == LegSt::Committed)
            ++r.legs_committed;
        else if (st == LegSt::Released)
            ++r.legs_released;
        else if (st == LegSt::Nacked)
            ++r.legs_nacked;
    }
    return r;
}

uint32_t CrossShardCoordinator::live_ops_of(uint64_t account_id) const noexcept {
    uint32_t n = 0;
    for (const OpRec& op : ops_) {
        if (!op.occupied || op.initiator_account != account_id)
            continue;
        const auto st = static_cast<BasketStatus>(op.state);
        if (st == BasketStatus::Reserving || st == BasketStatus::Committing ||
            st == BasketStatus::Compensating)
            ++n;
    }
    return n;
}

BasketResult CrossShardCoordinator::submit(const BasketLegSpec* legs,
                                           uint32_t leg_count,
                                           uint64_t initiator_account,
                                           BasketOpId op_id,
                                           uint64_t now_ns) noexcept {
    BasketResult rej{};
    rej.op_id = op_id;
    rej.status = static_cast<uint8_t>(BasketStatus::Rejected);
    rej.code = static_cast<uint8_t>(BasketCode::BadArgs);
    rej.leg_count = static_cast<uint8_t>(
        leg_count > kMaxLegs ? kMaxLegs : leg_count);

    // --- gate validation (never tombstoned — a retry re-evaluates) ---
    if (legs == nullptr || leg_count == 0 || leg_count > kMaxLegs ||
        initiator_account == 0 || basket_op_id_is_nil(op_id)) {
        emit_metrics();
        return rej;
    }
    for (uint32_t i = 0; i < leg_count; ++i) {
        if (legs[i].amount <= 0 || legs[i].account_id == 0) {
            emit_metrics();
            return rej;  // BadArgs
        }
    }
    if (coordinator_shard(legs, leg_count) != opts_.shard_id) {
        rej.code = static_cast<uint8_t>(BasketCode::NotCoordinator);
        emit_metrics();
        return rej;
    }

    // --- operation_id dedup: cached result, zero side-effects ---
    if (const OpRec* existing = find_op(op_id))
        return snapshot(*existing);

    // --- Task 2.3.14: max 10 concurrent cross-shard ops per account ---
    if (live_ops_of(initiator_account) >= opts_.max_ops_per_account) {
        rej.code = static_cast<uint8_t>(BasketCode::LimitExceeded);
        ++metrics_.limit_exceeded_total;
        emit_metrics();
        return rej;  // -> CROSS_SHARD_LIMIT_EXCEEDED (HTTP 429)
    }

    OpRec* op = insert_op(op_id);
    if (op == nullptr) {
        rej.code = static_cast<uint8_t>(BasketCode::CapacityExceeded);
        emit_metrics();
        return rej;  // table full — fail closed
    }
    *op = OpRec{};
    op->id = op_id;
    op->initiator_account = initiator_account;
    op->t_submit_ns = now_ns;
    op->reserve_deadline_ns = now_ns + opts_.reserve_ttl_ns;
    op->total_deadline_ns = now_ns + opts_.total_deadline_ns;
    op->state = static_cast<uint8_t>(BasketStatus::Reserving);
    op->code = static_cast<uint8_t>(BasketCode::Ok);
    op->leg_count = static_cast<uint8_t>(leg_count);
    op->occupied = 1;
    ++metrics_.transactions_total;
    ++live_ops_;

    // WAL the intent BEFORE announcing the op on the wire — replay creates a
    // tombstone so redelivered op_ids dedup fail-closed.
    if (wal_ != nullptr && !wal_begin(*op)) {
        ++metrics_.wal_failures;
        finalize_op(*op, BasketStatus::Failed, BasketCode::WalFailure, now_ns);
        emit_metrics();
        return snapshot(*op);
    }

    // --- Phase 1: Reserve every leg -------------------------------------
    bool any_nack = false;
    for (uint32_t i = 0; i < leg_count; ++i) {
        LegRec& l = op->legs[i];
        l.account_id = legs[i].account_id;
        l.order_id = legs[i].order_id;
        l.amount = legs[i].amount;
        l.shard_id = legs[i].shard_id;
        l.instrument_id = legs[i].instrument_id;
        l.local = (legs[i].shard_id == opts_.shard_id) ? 1 : 0;
        l.state = static_cast<uint8_t>(LegSt::Pending);
        l.was_committed = 0;
        if (l.local) {
            // Leg homed on this shard: in-process participant path — same
            // lock/TTL semantics, no wire frame.
            BasketReserveReqBody m{};
            m.op_hi = op_id.hi;
            m.op_lo = op_id.lo;
            m.leg_index = i;
            m.instrument_id = l.instrument_id;
            m.account_id = l.account_id;
            m.order_id = l.order_id;
            m.amount = l.amount;
            m.expires_at_ns = op->reserve_deadline_ns;
            PartRec* rec = local_reserve(m, opts_.shard_id,
                                         /*emit_wire_ack=*/false);
            l.state = (rec != nullptr &&
                       static_cast<PartSt>(rec->state) == PartSt::Reserved)
                          ? static_cast<uint8_t>(LegSt::Reserved)
                          : static_cast<uint8_t>(LegSt::Nacked);
        } else if (!send_reserve_req(*op, l, i)) {
            // Wire failure — nothing committed on that shard yet (or the req
            // is in flight); compensation below sends a Release that the
            // participant answers idempotently.
            l.state = static_cast<uint8_t>(LegSt::Nacked);
        }
        if (static_cast<LegSt>(l.state) == LegSt::Nacked)
            any_nack = true;
    }

    if (any_nack)
        begin_compensation(*op, BasketCode::ParticipantNack, now_ns);
    else
        maybe_commit(*op, now_ns);  // all-local baskets can finish inline

    emit_metrics();
    return snapshot(*op);
}

BasketResult CrossShardCoordinator::status(BasketOpId op_id) const noexcept {
    const OpRec* op = find_op(op_id);
    BasketResult r{};
    r.op_id = op_id;
    r.status = static_cast<uint8_t>(BasketStatus::Unknown);
    r.code = static_cast<uint8_t>(BasketCode::Ok);
    return op ? snapshot(*op) : r;
}

bool CrossShardCoordinator::cancel(BasketOpId op_id,
                                   uint64_t now_ns) noexcept {
    OpRec* op = find_op(op_id);
    if (op == nullptr)
        return false;  // never existed here
    const auto st = static_cast<BasketStatus>(op->state);
    if (st == BasketStatus::Reserving || st == BasketStatus::Committing ||
        st == BasketStatus::Compensating)
        begin_compensation(*op, BasketCode::Cancelled, now_ns);
    emit_metrics();
    return true;  // terminal ops: idempotent no-op
}

uint8_t CrossShardCoordinator::part_state(BasketOpId op_id,
                                          uint32_t leg_index) const noexcept {
    const PartRec* r = find_part(op_id, leg_index);
    return r ? r->state : 0;
}

// --- engine pump ----------------------------------------------------------------

void CrossShardCoordinator::on_time_tick(uint64_t now_ns) noexcept {
    if (ctl_ != nullptr && ctl_->is_open()) {
        uint8_t buf[kBasketCtlMaxFrame];
        // Bounded drain — control traffic is tiny; 64 frames/turn keeps the
        // matching-loop budget intact under a flood (margin-ctl precedent).
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
    if (now_ns >= next_reap_ns_) {
        // Task 2.3.14 CompensationReaper — tick-driven scan at 2s cadence of
        // ENGINE-LOGICAL time (spec's Go-side goroutine is a later gateway
        // phase; here the reaper is deterministic).
        reap(now_ns);
        next_reap_ns_ = now_ns + opts_.reaper_interval_ns;
    }
    emit_metrics();
}

// --- coordinator transitions ------------------------------------------------------

void CrossShardCoordinator::maybe_commit(OpRec& op, uint64_t now_ns) noexcept {
    if (static_cast<BasketStatus>(op.state) != BasketStatus::Reserving)
        return;
    for (uint32_t i = 0; i < op.leg_count; ++i)
        if (static_cast<LegSt>(op.legs[i].state) != LegSt::Reserved)
            return;

    // Phase 2: all legs Reserved inside the 5s window -> Commit all.
    op.state = static_cast<uint8_t>(BasketStatus::Committing);
    for (uint32_t i = 0; i < op.leg_count; ++i) {
        LegRec& l = op.legs[i];
        if (l.local) {
            PartRec* rec = find_part(op.id, i);
            if (rec != nullptr &&
                static_cast<PartSt>(rec->state) == PartSt::Reserved) {
                part_commit(*rec);
                l.state = static_cast<uint8_t>(LegSt::Committed);
                l.was_committed = 1;
            } else {
                // Local lock already expired/denied — commit refused.
                l.state = static_cast<uint8_t>(LegSt::Released);
                begin_compensation(op, BasketCode::CommitRefused, now_ns);
                return;
            }
        } else {
            l.state = static_cast<uint8_t>(LegSt::CommitSent);
            l.was_committed = 1;  // dispatch means the participant MAY have
                                  // applied — an unconfirmed unwind of this
                                  // leg escalates the op to Failed, never
                                  // a falsely-clean Compensated
            if (!send_commit(op, i)) {
                // Channel down mid-commit: the participant's TTL backstops
                // (it expires the lock unanswered); compensate everything.
                begin_compensation(op, BasketCode::ChannelDown, now_ns);
                return;
            }
            ++metrics_.commits_sent;
        }
    }
    maybe_terminal(op, now_ns);
}

void CrossShardCoordinator::begin_compensation(OpRec& op, BasketCode code,
                                               uint64_t now_ns) noexcept {
    const auto st = static_cast<BasketStatus>(op.state);
    if (st == BasketStatus::Committed || st == BasketStatus::Compensated ||
        st == BasketStatus::Failed)
        return;
    if (st != BasketStatus::Compensating) {
        op.state = static_cast<uint8_t>(BasketStatus::Compensating);
        op.code = static_cast<uint8_t>(code);
        ++metrics_.compensations_total;
    }
    for (uint32_t i = 0; i < op.leg_count; ++i) {
        LegRec& l = op.legs[i];
        const auto ls = static_cast<LegSt>(l.state);
        if (ls == LegSt::Nacked || ls == LegSt::Released)
            continue;  // already terminal
        if (l.local) {
            PartRec* rec = find_part(op.id, i);
            if (rec != nullptr)
                part_release(*rec, BasketReleaseReason::Compensate,
                             /*send_wire_ack=*/false);
            l.state = static_cast<uint8_t>(LegSt::Released);
            continue;
        }
        if (ls == LegSt::Pending) {
            // The REQ may be in flight or its ACK lost — a best-effort
            // Release accelerates cleanup, and a granted reservation
            // self-expires at its TTL either way. Bookkeeping resolves to
            // Released immediately: phase 2 requires EVERY leg Reserved, so
            // this leg can never commit — no uncertainty to carry.
            (void)send_release(op, i, code == BasketCode::Cancelled
                                          ? BasketReleaseReason::CoordinatorCancel
                                          : BasketReleaseReason::Compensate);
            ++metrics_.releases_sent;
            l.state = static_cast<uint8_t>(LegSt::Released);
            continue;
        }
        // Reserved/CommitSent/Committed remote legs DO carry uncertainty —
        // the participant provably holds (or may hold) a lock/activation,
        // so we wait for the ReleaseAck inside the compensation window.
        if (ls != LegSt::ReleaseSent) {
            (void)send_release(op, i, code == BasketCode::Cancelled
                                          ? BasketReleaseReason::CoordinatorCancel
                                          : BasketReleaseReason::Compensate);
            ++metrics_.releases_sent;
            if (ls != LegSt::Committed)
                l.state = static_cast<uint8_t>(LegSt::ReleaseSent);
            // Committed legs keep LegSt::Committed until the ReleaseAck —
            // an unconfirmed committed-leg unwind lands the op in Failed,
            // never Compensated (uncertain states must not report clean).
        }
    }
    maybe_terminal(op, now_ns);
}

void CrossShardCoordinator::maybe_terminal(OpRec& op,
                                           uint64_t now_ns) noexcept {
    const auto st = static_cast<BasketStatus>(op.state);
    if (st == BasketStatus::Committing) {
        for (uint32_t i = 0; i < op.leg_count; ++i)
            if (static_cast<LegSt>(op.legs[i].state) != LegSt::Committed)
                return;
        finalize_op(op, BasketStatus::Committed, BasketCode::Ok, now_ns);
        return;
    }
    if (st == BasketStatus::Compensating) {
        for (uint32_t i = 0; i < op.leg_count; ++i) {
            const auto ls = static_cast<LegSt>(op.legs[i].state);
            if (ls != LegSt::Nacked && ls != LegSt::Released)
                return;
        }
        finalize_op(op, BasketStatus::Compensated,
                    static_cast<BasketCode>(op.code), now_ns);
    }
}

void CrossShardCoordinator::finalize_op(OpRec& op, BasketStatus st,
                                        BasketCode code,
                                        uint64_t now_ns) noexcept {
    const auto prev = static_cast<BasketStatus>(op.state);
    if (prev == BasketStatus::Reserving || prev == BasketStatus::Committing ||
        prev == BasketStatus::Compensating) {
        if (live_ops_ > 0)
            --live_ops_;
    }
    op.state = static_cast<uint8_t>(st);
    op.code = static_cast<uint8_t>(code);
    (void)wal_outcome(op);  // outcome WAL failure is not fatal — in-memory
                            // tombstone already correct; audit gap noted.
    if (st == BasketStatus::Committed)
        ++metrics_.transactions_success;
    else
        ++metrics_.transactions_failure;
    metrics_.transaction_duration_ns_sum += now_ns - op.t_submit_ns;
}

// --- participant transitions ------------------------------------------------------

CrossShardCoordinator::PartRec* CrossShardCoordinator::local_reserve(
    const BasketReserveReqBody& m, uint64_t coord_shard,
    bool emit_wire_ack) noexcept {
    const BasketOpId id{m.op_hi, m.op_lo};
    PartRec* rec = find_part(id, m.leg_index);
    if (rec != nullptr) {
        const auto st = static_cast<PartSt>(rec->state);
        if (st == PartSt::Reserved || st == PartSt::Committed) {
            // Idempotent redelivery: re-ACK the original decision — the
            // lock is NOT taken twice.
            if (emit_wire_ack) {
                BasketVoteBody a{};
                a.op_hi = m.op_hi;
                a.op_lo = m.op_lo;
                a.leg_index = m.leg_index;
                a.reason = 0;
                a.amount = rec->amount;
                a.expires_at_ns = rec->expires_at_ns;
                (void)send_frame(BasketCtlType::ReserveAck, &a,
                                 rec->coordinator_shard);
            }
            return rec;
        }
        // Denied/Released tombstone: deterministic re-NACK.
        if (emit_wire_ack) {
            BasketVoteBody n{};
            n.op_hi = m.op_hi;
            n.op_lo = m.op_lo;
            n.leg_index = m.leg_index;
            n.reason = static_cast<uint32_t>(BasketNackReason::TtlExpired);
            (void)send_frame(BasketCtlType::ReserveNack, &n, coord_shard);
        }
        return rec;
    }
    if (m.account_id == 0 || m.amount <= 0) {
        ++metrics_.bad_frames;
        return nullptr;
    }
    // Balance admission: available must cover the lock — pessimistic,
    // zero-breach floor (spec §2.7).
    const int64_t avail = available(m.account_id);
    if (avail < m.amount) {
        PartRec* t = insert_part(id, m.leg_index);
        if (t != nullptr) {
            *t = PartRec{};
            t->op_id = id;
            t->coordinator_shard = coord_shard;
            t->account_id = m.account_id;
            t->order_id = m.order_id;
            t->amount = m.amount;
            t->expires_at_ns = m.expires_at_ns;
            t->leg_index = m.leg_index;
            t->instrument_id = m.instrument_id;
            t->state = static_cast<uint8_t>(PartSt::Denied);
        }
        if (emit_wire_ack) {
            BasketVoteBody n{};
            n.op_hi = m.op_hi;
            n.op_lo = m.op_lo;
            n.leg_index = m.leg_index;
            n.reason =
                static_cast<uint32_t>(BasketNackReason::InsufficientBalance);
            (void)send_frame(BasketCtlType::ReserveNack, &n, coord_shard);
        }
        return nullptr;
    }
    PartRec* s = insert_part(id, m.leg_index);
    if (s == nullptr) {
        if (emit_wire_ack) {
            BasketVoteBody n{};
            n.op_hi = m.op_hi;
            n.op_lo = m.op_lo;
            n.leg_index = m.leg_index;
            n.reason = static_cast<uint32_t>(BasketNackReason::Overload);
            (void)send_frame(BasketCtlType::ReserveNack, &n, coord_shard);
        }
        return nullptr;
    }
    *s = PartRec{};
    s->op_id = id;
    s->coordinator_shard = coord_shard;
    s->account_id = m.account_id;
    s->order_id = m.order_id;
    s->amount = m.amount;
    s->expires_at_ns = m.expires_at_ns;
    s->leg_index = m.leg_index;
    s->instrument_id = m.instrument_id;
    s->state = static_cast<uint8_t>(PartSt::Reserved);

    // Commit ordering: WAL first — a lock we cannot persist must not be
    // ACK'd (fail closed; the coordinator compensates or retries).
    if (!wal_reserve(*s)) {
        ++metrics_.wal_failures;
        s->state = static_cast<uint8_t>(PartSt::Denied);
        if (emit_wire_ack) {
            BasketVoteBody n{};
            n.op_hi = m.op_hi;
            n.op_lo = m.op_lo;
            n.leg_index = m.leg_index;
            n.reason =
                static_cast<uint32_t>(BasketNackReason::InsufficientBalance);
            (void)send_frame(BasketCtlType::ReserveNack, &n, coord_shard);
        }
        return nullptr;
    }
    // Lock the balance: provider mode tracks only `locked` (the real ledger
    // owns availability); local mode debits `available` too.
    AccountBal* b = ensure_bal(m.account_id);
    if (b != nullptr) {
        if (bal_fn_ == nullptr)
            b->available -= m.amount;
        b->locked += m.amount;
    }
    ++live_locks_;
    emit_leg(id, m.leg_index, opts_.shard_id, m.account_id, m.order_id,
             m.amount, BasketLegEvent::Reserved,
             static_cast<BasketReleaseReason>(0));
    if (emit_wire_ack) {
        BasketVoteBody a{};
        a.op_hi = m.op_hi;
        a.op_lo = m.op_lo;
        a.leg_index = m.leg_index;
        a.reason = 0;
        a.amount = m.amount;
        a.expires_at_ns = m.expires_at_ns;
        (void)send_frame(BasketCtlType::ReserveAck, &a, coord_shard);
    }
    return s;
}

void CrossShardCoordinator::part_commit(PartRec& r) noexcept {
    // Consume the lock (funds now bound to the activated order — the ledger
    // debit proper is Phase-03; the local store mirrors it).
    AccountBal* b = find_bal(r.account_id);
    if (b != nullptr)
        b->locked -= r.amount;
    if (live_locks_ > 0)
        --live_locks_;
    (void)wal_leg_done(r, /*kind=*/0, static_cast<BasketReleaseReason>(0));
    r.state = static_cast<uint8_t>(PartSt::Committed);
    emit_leg(r.op_id, r.leg_index, opts_.shard_id, r.account_id, r.order_id,
             r.amount, BasketLegEvent::Activated,
             static_cast<BasketReleaseReason>(0));
}

void CrossShardCoordinator::part_release(PartRec& r,
                                         BasketReleaseReason reason,
                                         bool send_wire_ack) noexcept {
    const auto st = static_cast<PartSt>(r.state);
    if (st == PartSt::Released || st == PartSt::Denied ||
        st == PartSt::Empty) {
        if (send_wire_ack) {
            BasketReleaseAckBody a{};
            a.op_hi = r.op_id.hi;
            a.op_lo = r.op_id.lo;
            a.leg_index = r.leg_index;
            (void)send_frame(BasketCtlType::ReleaseAck, &a,
                             r.coordinator_shard);
        }
        return;  // idempotent
    }
    AccountBal* b = find_bal(r.account_id);
    if (st == PartSt::Reserved) {
        // Unlock: locked -> available (local mode; provider mode drops the
        // lock only — the ledger was never debited).
        if (b != nullptr) {
            b->locked -= r.amount;
            if (bal_fn_ == nullptr)
                b->available += r.amount;
        }
        emit_leg(r.op_id, r.leg_index, opts_.shard_id, r.account_id,
                 r.order_id, r.amount, BasketLegEvent::Released, reason);
    } else if (st == PartSt::Committed) {
        // Restitution: the lock was consumed at commit; cancelling the
        // activated order credits `available` back (GL settle = Phase-03).
        if (b != nullptr && bal_fn_ == nullptr)
            b->available += r.amount;
        emit_leg(r.op_id, r.leg_index, opts_.shard_id, r.account_id,
                 r.order_id, r.amount, BasketLegEvent::Restituted, reason);
    }
    if (live_locks_ > 0)
        --live_locks_;
    (void)wal_leg_done(r, /*kind=*/1, reason);
    r.state = static_cast<uint8_t>(PartSt::Released);
    if (send_wire_ack) {
        BasketReleaseAckBody a{};
        a.op_hi = r.op_id.hi;
        a.op_lo = r.op_id.lo;
        a.leg_index = r.leg_index;
        (void)send_frame(BasketCtlType::ReleaseAck, &a, r.coordinator_shard);
    }
}

// --- wire send helpers -------------------------------------------------------------

bool CrossShardCoordinator::send_frame(BasketCtlType t, const void* body,
                                       uint32_t dst) noexcept {
    if (ctl_ == nullptr || !ctl_->is_open())
        return false;
    uint8_t buf[kBasketCtlMaxFrame];
    const uint32_t n = basket_ctl_encode(buf, sizeof(buf), t, body,
                                         opts_.shard_id, dst);
    return n != 0 && ctl_->send(buf, n);
}

bool CrossShardCoordinator::send_reserve_req(const OpRec& op,
                                             const LegRec& leg,
                                             uint32_t leg_index) noexcept {
    BasketReserveReqBody b{};
    b.op_hi = op.id.hi;
    b.op_lo = op.id.lo;
    b.leg_index = leg_index;
    b.instrument_id = leg.instrument_id;
    b.account_id = leg.account_id;
    b.order_id = leg.order_id;
    b.amount = leg.amount;
    b.expires_at_ns = op.reserve_deadline_ns;  // authoritative 5s TTL
    return send_frame(BasketCtlType::ReserveReq, &b, leg.shard_id);
}

bool CrossShardCoordinator::send_commit(const OpRec& op,
                                        uint32_t leg_index) noexcept {
    BasketCommitBody b{};
    b.op_hi = op.id.hi;
    b.op_lo = op.id.lo;
    b.leg_index = leg_index;
    return send_frame(BasketCtlType::Commit, &b, op.legs[leg_index].shard_id);
}

bool CrossShardCoordinator::send_release(const OpRec& op, uint32_t leg_index,
                                         BasketReleaseReason reason) noexcept {
    BasketReleaseBody b{};
    b.op_hi = op.id.hi;
    b.op_lo = op.id.lo;
    b.leg_index = leg_index;
    b.reason = static_cast<uint32_t>(reason);
    return send_frame(BasketCtlType::Release, &b,
                      op.legs[leg_index].shard_id);
}

bool CrossShardCoordinator::send_expired_notice(const PartRec& r) noexcept {
    BasketReleaseBody b{};
    b.op_hi = r.op_id.hi;
    b.op_lo = r.op_id.lo;
    b.leg_index = r.leg_index;
    b.reason = static_cast<uint32_t>(BasketReleaseReason::Expired);
    return send_frame(BasketCtlType::Release, &b, r.coordinator_shard);
}

// --- inbound dispatch -----------------------------------------------------------------

void CrossShardCoordinator::on_frame(const uint8_t* buf, uint32_t len,
                                     uint64_t now_ns) noexcept {
    BasketCtlView v;
    if (basket_ctl_decode(buf, len, &v) != BasketCtlDecode::Ok) {
        ++metrics_.bad_frames;  // malformed control frame — drop, fail closed
        return;
    }
    switch (v.type) {
        case BasketCtlType::ReserveReq:  on_reserve_req(v, now_ns);   break;
        case BasketCtlType::ReserveAck:  on_reserve_vote(v, true, now_ns);  break;
        case BasketCtlType::ReserveNack: on_reserve_vote(v, false, now_ns); break;
        case BasketCtlType::Commit:      on_commit(v, now_ns);        break;
        case BasketCtlType::CommitAck:   on_commit_ack(v, now_ns);    break;
        case BasketCtlType::Release:     on_release(v, now_ns);       break;
        case BasketCtlType::ReleaseAck:  on_release_ack(v, now_ns);   break;
    }
}

void CrossShardCoordinator::on_reserve_req(const BasketCtlView& v,
                                           uint64_t now_ns) noexcept {
    // A REQ whose TTL already passed NACKs outright — the coordinator's
    // expiry is authoritative and engine clocks share the WAL tick stream.
    if (v.req.expires_at_ns <= now_ns) {
        BasketVoteBody n{};
        n.op_hi = v.req.op_hi;
        n.op_lo = v.req.op_lo;
        n.leg_index = v.req.leg_index;
        n.reason = static_cast<uint32_t>(BasketNackReason::TtlExpired);
        (void)send_frame(BasketCtlType::ReserveNack, &n, v.src_shard);
        return;
    }
    (void)local_reserve(v.req, v.src_shard, /*emit_wire_ack=*/true);
}

void CrossShardCoordinator::on_reserve_vote(const BasketCtlView& v, bool ack,
                                            uint64_t now_ns) noexcept {
    const BasketOpId id{v.vote.op_hi, v.vote.op_lo};
    OpRec* op = find_op(id);
    if (op == nullptr) {
        if (ack) {
            // ACK for an op we never issued: the participant believes it
            // holds a lock for us — compensate so its books cannot drift
            // (symmetric zero-loss pessimism, margin-ctl precedent).
            BasketReleaseBody r{};
            r.op_hi = id.hi;
            r.op_lo = id.lo;
            r.leg_index = v.vote.leg_index;
            r.reason =
                static_cast<uint32_t>(BasketReleaseReason::Compensate);
            (void)send_frame(BasketCtlType::Release, &r, v.src_shard);
        }
        return;
    }
    if (v.vote.leg_index >= op->leg_count) {
        ++metrics_.bad_frames;
        return;
    }
    LegRec& l = op->legs[v.vote.leg_index];
    const auto lst = static_cast<LegSt>(l.state);
    const auto ost = static_cast<BasketStatus>(op->state);
    if (!ack) {
        // NACK resolves only a Pending leg; anything else is stale noise.
        if (ost == BasketStatus::Reserving && lst == LegSt::Pending) {
            l.state = static_cast<uint8_t>(LegSt::Nacked);
            begin_compensation(*op, BasketCode::ParticipantNack, now_ns);
        }
        return;
    }
    if (ost == BasketStatus::Reserving && lst == LegSt::Pending) {
        l.state = static_cast<uint8_t>(LegSt::Reserved);
        maybe_commit(*op, now_ns);
        return;
    }
    if (lst == LegSt::Reserved || lst == LegSt::ReleaseSent)
        return;  // duplicate ACK, or a release is already in flight
    if (lst == LegSt::CommitSent || lst == LegSt::Committed)
        return;  // late ACK overtaken by Phase 2 — Commit consumes the lock
    // ACK for a leg we consider dead (Nacked/Released) or for a terminal op:
    // the participant believes it holds a lock — compensate for zero drift.
    BasketReleaseBody r{};
    r.op_hi = id.hi;
    r.op_lo = id.lo;
    r.leg_index = v.vote.leg_index;
    r.reason = static_cast<uint32_t>(BasketReleaseReason::Compensate);
    (void)send_frame(BasketCtlType::Release, &r, l.shard_id);
    ++metrics_.releases_sent;
}

void CrossShardCoordinator::on_commit(const BasketCtlView& v,
                                      uint64_t now_ns) noexcept {
    const BasketOpId id{v.commit.op_hi, v.commit.op_lo};
    PartRec* rec = find_part(id, v.commit.leg_index);
    uint8_t applied = 0;
    if (rec != nullptr) {
        const auto st = static_cast<PartSt>(rec->state);
        if (st == PartSt::Reserved) {
            if (now_ns < rec->expires_at_ns) {
                part_commit(*rec);
                applied = 1;
            } else {
                // Commit lost the TTL race — the lock dies unanswered; the
                // expiry notice tells the coordinator to compensate.
                part_release(*rec, BasketReleaseReason::Expired,
                             /*send_wire_ack=*/false);
                (void)send_expired_notice(*rec);
                applied = 0;
            }
        } else if (st == PartSt::Committed) {
            applied = 1;  // duplicate COMMIT — idempotent
        } else {
            applied = 0;  // Denied/Released tombstone — commit refused
        }
    } else {
        // Unknown reservation: leave a Released tombstone so a redelivered
        // REQ for this (op,leg) deterministically re-NACKs.
        PartRec* t = insert_part(id, v.commit.leg_index);
        if (t != nullptr) {
            *t = PartRec{};
            t->op_id = id;
            t->coordinator_shard = v.src_shard;
            t->leg_index = v.commit.leg_index;
            t->state = static_cast<uint8_t>(PartSt::Released);
        }
    }
    BasketCommitAckBody a{};
    a.op_hi = id.hi;
    a.op_lo = id.lo;
    a.leg_index = v.commit.leg_index;
    a.applied = applied;
    (void)send_frame(BasketCtlType::CommitAck, &a, v.src_shard);
}

void CrossShardCoordinator::on_commit_ack(const BasketCtlView& v,
                                          uint64_t now_ns) noexcept {
    const BasketOpId id{v.cack.op_hi, v.cack.op_lo};
    OpRec* op = find_op(id);
    if (op == nullptr) {
        if (v.cack.applied != 0) {
            // Participant activated an order for an unknown op — release it.
            BasketReleaseBody r{};
            r.op_hi = id.hi;
            r.op_lo = id.lo;
            r.leg_index = v.cack.leg_index;
            r.reason =
                static_cast<uint32_t>(BasketReleaseReason::Compensate);
            (void)send_frame(BasketCtlType::Release, &r, v.src_shard);
        }
        return;
    }
    if (v.cack.leg_index >= op->leg_count)
        return;
    LegRec& l = op->legs[v.cack.leg_index];
    const auto lst = static_cast<LegSt>(l.state);
    if (lst == LegSt::CommitSent) {
        if (v.cack.applied != 0) {
            l.state = static_cast<uint8_t>(LegSt::Committed);
            l.was_committed = 1;
            maybe_terminal(*op, now_ns);
        } else {
            // Commit refused (TTL race): the participant holds nothing —
            // mark the leg released and compensate the survivors.
            l.state = static_cast<uint8_t>(LegSt::Released);
            begin_compensation(*op, BasketCode::CommitRefused, now_ns);
        }
        return;
    }
    if (lst == LegSt::Released && v.cack.applied != 0) {
        // applied=1 on a leg we already released: participant holds an
        // activated order — release again for zero drift.
        (void)send_release(*op, v.cack.leg_index,
                           BasketReleaseReason::Compensate);
        l.state = static_cast<uint8_t>(LegSt::ReleaseSent);
        ++metrics_.releases_sent;
    }
    // duplicate CommitAck on Committed / ReleaseSent legs: idempotent
}

void CrossShardCoordinator::on_release(const BasketCtlView& v,
                                       uint64_t now_ns) noexcept {
    const BasketOpId id{v.rel.op_hi, v.rel.op_lo};

    // Direction disambiguation: a RELEASE from the shard hosting leg i of an
    // op WE coordinate is an expiry notice (participant -> coordinator);
    // anything else targets a lock WE host (coordinator -> participant).
    OpRec* op = find_op(id);
    const bool coord_hit =
        op != nullptr && v.rel.leg_index < op->leg_count &&
        op->legs[v.rel.leg_index].shard_id == v.src_shard &&
        op->legs[v.rel.leg_index].local == 0;
    PartRec* rec = find_part(id, v.rel.leg_index);
    const bool part_hit = rec != nullptr &&
                          rec->coordinator_shard == v.src_shard;

    if (coord_hit) {
        LegRec& l = op->legs[v.rel.leg_index];
        const auto lst = static_cast<LegSt>(l.state);
        const auto ost = static_cast<BasketStatus>(op->state);
        switch (lst) {
            case LegSt::Pending:
            case LegSt::Reserved:
                // The participant's TTL lapsed un-committed — this leg can
                // never commit; compensate the survivors (all-or-nothing).
                l.state = static_cast<uint8_t>(LegSt::Released);
                if (ost == BasketStatus::Reserving ||
                    ost == BasketStatus::Committing)
                    begin_compensation(*op, BasketCode::ParticipantNack,
                                       now_ns);
                break;
            case LegSt::CommitSent:
                // Participant expired instead of applying our Commit.
                l.state = static_cast<uint8_t>(LegSt::Released);
                if (ost == BasketStatus::Committing ||
                    ost == BasketStatus::Reserving)
                    begin_compensation(*op, BasketCode::CommitRefused,
                                       now_ns);
                break;
            case LegSt::ReleaseSent:
                l.state = static_cast<uint8_t>(LegSt::Released);
                maybe_terminal(*op, now_ns);
                break;
            case LegSt::Committed:
            case LegSt::Nacked:
            case LegSt::Released:
                break;  // durable or already resolved — idempotent
        }
        return;
    }

    // Participant path: coordinator-initiated compensate/cancel.
    if (rec != nullptr) {
        const auto st = static_cast<PartSt>(rec->state);
        if (st == PartSt::Reserved || st == PartSt::Committed) {
            part_release(*rec,
                         static_cast<BasketReleaseReason>(v.rel.reason),
                         /*send_wire_ack=*/true);
            return;
        }
        // Denied/Released tombstone — confirm idempotently.
        BasketReleaseAckBody a{};
        a.op_hi = id.hi;
        a.op_lo = id.lo;
        a.leg_index = v.rel.leg_index;
        (void)send_frame(BasketCtlType::ReleaseAck, &a, v.src_shard);
        return;
    }
    (void)part_hit;  // rec resolved above; kept for readability
    // Nothing held for this (op,leg): confirm anyway — the coordinator's
    // compensation bookkeeping must never wedge on a lost REQ.
    BasketReleaseAckBody a{};
    a.op_hi = id.hi;
    a.op_lo = id.lo;
    a.leg_index = v.rel.leg_index;
    (void)send_frame(BasketCtlType::ReleaseAck, &a, v.src_shard);
}

void CrossShardCoordinator::on_release_ack(const BasketCtlView& v,
                                           uint64_t now_ns) noexcept {
    const BasketOpId id{v.rack.op_hi, v.rack.op_lo};
    OpRec* op = find_op(id);
    if (op == nullptr || v.rack.leg_index >= op->leg_count)
        return;  // unknown / stale ack — idempotent
    LegRec& l = op->legs[v.rack.leg_index];
    const auto lst = static_cast<LegSt>(l.state);
    if (lst == LegSt::ReleaseSent || lst == LegSt::Committed ||
        lst == LegSt::Pending || lst == LegSt::Reserved) {
        // Participant confirms nothing is held for this leg.
        l.state = static_cast<uint8_t>(LegSt::Released);
        const auto ost = static_cast<BasketStatus>(op->state);
        if (ost == BasketStatus::Reserving || ost == BasketStatus::Committing)
            begin_compensation(*op, BasketCode::ParticipantNack, now_ns);
        else
            maybe_terminal(*op, now_ns);
    }
}

// --- CompensationReaper ---------------------------------------------------------------

void CrossShardCoordinator::reap(uint64_t now_ns) noexcept {
    ++metrics_.reaper_scans;

    // Coordinator ops: layered deadlines (5s reserve / 8s total).
    for (uint32_t i = 0; i < kMaxOps; ++i) {
        OpRec& op = ops_[i];
        if (!op.occupied)
            continue;
        const auto st = static_cast<BasketStatus>(op.state);
        if (st == BasketStatus::Reserving &&
            now_ns > op.reserve_deadline_ns) {
            // Task 2.3.14: shard non-response within 5s -> cancel the
            // reservation and compensate ALL participants.
            ++metrics_.reservation_timeout_total;
            begin_compensation(op, BasketCode::ReserveTimeout, now_ns);
        } else if (st == BasketStatus::Committing &&
                   now_ns > op.total_deadline_ns) {
            ++metrics_.reservation_timeout_total;
            begin_compensation(op, BasketCode::CommitTimeout, now_ns);
        } else if (st == BasketStatus::Compensating &&
                   now_ns > op.total_deadline_ns) {
            // Rollback out of budget: force-resolve every leg that cannot
            // hold uncertain state. Pending/Reserved/CommitSent/ReleaseSent
            // legs are safe to mark Released — the participant-side TTL
            // backstops zero-orphan regardless of ack delivery. A leg that
            // reached Committed (or was_commit-flagged CommitSent) without
            // a confirmed unwind is genuinely uncertain -> op Failed.
            bool uncertain = false;
            for (uint32_t li = 0; li < op.leg_count; ++li) {
                LegRec& l = op.legs[li];
                const auto ls = static_cast<LegSt>(l.state);
                if (ls == LegSt::Nacked || ls == LegSt::Released)
                    continue;
                if (l.local) {
                    PartRec* rec = find_part(op.id, li);
                    if (rec != nullptr)
                        part_release(*rec, BasketReleaseReason::Compensate,
                                     false);
                    l.state = static_cast<uint8_t>(LegSt::Released);
                    continue;
                }
                (void)send_release(op, li, BasketReleaseReason::Compensate);
                ++metrics_.releases_sent;
                if (ls == LegSt::Committed || l.was_committed != 0)
                    uncertain = true;  // committed unwind unconfirmed
                else
                    l.state = static_cast<uint8_t>(LegSt::Released);
            }
            if (uncertain) {
                finalize_op(op, BasketStatus::Failed,
                            static_cast<BasketCode>(op.code), now_ns);
            } else {
                maybe_terminal(op, now_ns);
            }
        }
    }

    // Participant locks: TTL expiry (the zero-orphan backstop — independent
    // of any coordinator message ever arriving).
    for (uint32_t i = 0; i < kMaxPartRes; ++i) {
        PartRec& r = parts_[i];
        if (static_cast<PartSt>(r.state) != PartSt::Reserved)
            continue;
        if (r.expires_at_ns != 0 && now_ns >= r.expires_at_ns) {
            const BasketReleaseReason why =
                r.recovered != 0 ? BasketReleaseReason::RecoveryOrphan
                                 : BasketReleaseReason::Expired;
            const uint64_t coord = r.coordinator_shard;
            part_release(r, why, /*send_wire_ack=*/false);
            if (coord != opts_.shard_id)
                (void)send_expired_notice(r);
            ++metrics_.reservation_timeout_total;
        }
    }
}

// --- recovery ---------------------------------------------------------------------------

size_t CrossShardCoordinator::recover(WalReader& reader) noexcept {
    size_t applied = 0;
    WalEntryView v;
    while (reader.next(v) == WalScanStep::Entry) {
        const uint8_t t = static_cast<uint8_t>(v.type);
        if (t == kWalEvtBasketBegin &&
            v.payload_len == sizeof(WalBasketBeginPayload)) {
            WalBasketBeginPayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            const BasketOpId id{p.op_hi, p.op_lo};
            OpRec* op = find_op(id);
            if (op == nullptr)
                op = insert_op(id);
            if (op != nullptr &&
                static_cast<BasketStatus>(op->state) ==
                    BasketStatus::Unknown) {
                // Uncommitted intent: restore an in-flight marker only —
                // the terminal sweep below decides fail-closed.
                *op = OpRec{};
                op->id = id;
                op->initiator_account = p.initiator_account;
                op->reserve_deadline_ns = p.reserve_deadline_ns;
                op->total_deadline_ns = p.total_deadline_ns;
                op->leg_count = static_cast<uint8_t>(
                    p.leg_count > kMaxLegs ? kMaxLegs : p.leg_count);
                op->state = static_cast<uint8_t>(BasketStatus::Reserving);
                op->occupied = 1;
                op->recovered = 1;
            }
            ++applied;
        } else if (t == kWalEvtBasketReserve &&
                   v.payload_len == sizeof(WalBasketReservePayload)) {
            WalBasketReservePayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            const BasketOpId id{p.op_hi, p.op_lo};
            PartRec* r = find_part(id, p.leg_index);
            if (r == nullptr)
                r = insert_part(id, p.leg_index);
            if (r != nullptr &&
                (static_cast<PartSt>(r->state) == PartSt::Empty ||
                 static_cast<PartSt>(r->state) == PartSt::Reserved)) {
                *r = PartRec{};
                r->op_id = id;
                r->coordinator_shard = p.coordinator_shard;
                r->account_id = p.account_id;
                r->order_id = p.order_id;
                r->amount = p.amount;
                r->expires_at_ns = p.expires_at_ns;
                r->leg_index = p.leg_index;
                r->instrument_id = p.instrument_id;
                r->state = static_cast<uint8_t>(PartSt::Reserved);
                r->recovered = 1;
                AccountBal* b = ensure_bal(p.account_id);
                if (b != nullptr)
                    b->locked += p.amount;  // restore the lock increment
                ++live_locks_;
            }
            ++applied;
        } else if (t == kWalEvtBasketLegDone &&
                   v.payload_len == sizeof(WalBasketLegDonePayload)) {
            WalBasketLegDonePayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            const BasketOpId id{p.op_hi, p.op_lo};
            PartRec* r = find_part(id, p.leg_index);
            if (r != nullptr &&
                static_cast<PartSt>(r->state) == PartSt::Reserved) {
                AccountBal* b = find_bal(r->account_id);
                if (p.kind == 0) {
                    if (b != nullptr)
                        b->locked -= r->amount;
                    r->state = static_cast<uint8_t>(PartSt::Committed);
                } else {
                    if (b != nullptr) {
                        b->locked -= r->amount;
                        if (bal_fn_ == nullptr)
                            b->available += r->amount;
                    }
                    r->state = static_cast<uint8_t>(PartSt::Released);
                }
                if (live_locks_ > 0)
                    --live_locks_;
            }
            ++applied;
        } else if (t == kWalEvtBasketOutcome &&
                   v.payload_len == sizeof(WalBasketOutcomePayload)) {
            WalBasketOutcomePayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            const BasketOpId id{p.op_hi, p.op_lo};
            OpRec* op = find_op(id);
            if (op == nullptr)
                op = insert_op(id);
            if (op != nullptr) {
                const auto st = static_cast<BasketStatus>(op->state);
                if (st == BasketStatus::Reserving ||
                    st == BasketStatus::Committing ||
                    st == BasketStatus::Compensating ||
                    st == BasketStatus::Unknown) {
                    if (!op->occupied) {
                        op->id = id;
                        op->initiator_account = p.initiator_account;
                        op->occupied = 1;
                    }
                    op->state = p.status;
                    op->code = p.code;
                    op->recovered = 1;
                }
            }
            ++applied;
        }
        // other event types: not ours — skip
    }
    // Fail-closed terminal sweep: a non-terminal op after replay can never
    // legitimately resume the protocol — tombstone it so redelivered op_ids
    // dedup to a definitive failure instead of a half-known resurrection.
    for (uint32_t i = 0; i < kMaxOps; ++i) {
        OpRec& op = ops_[i];
        if (!op.occupied)
            continue;
        const auto st = static_cast<BasketStatus>(op.state);
        if (st == BasketStatus::Reserving || st == BasketStatus::Committing ||
            st == BasketStatus::Compensating) {
            op.state = static_cast<uint8_t>(BasketStatus::Failed);
            op.code = static_cast<uint8_t>(BasketCode::RecoveryInterrupted);
        }
    }
    emit_metrics();
    return applied;
}

// --- sinks -------------------------------------------------------------------------

void CrossShardCoordinator::emit_leg(BasketOpId op, uint32_t leg_index,
                                     uint32_t shard, uint64_t account,
                                     uint64_t order, int64_t amount,
                                     BasketLegEvent ev,
                                     BasketReleaseReason reason) noexcept {
    if (leg_sink_ == nullptr)
        return;
    BasketLegNotice n{};
    n.op_id = op;
    n.account_id = account;
    n.order_id = order;
    n.amount = amount;
    n.leg_index = leg_index;
    n.shard_id = shard;
    n.event = static_cast<uint8_t>(ev);
    n.reason = static_cast<uint8_t>(reason);
    leg_sink_(leg_ctx_, n);
}

void CrossShardCoordinator::emit_metrics() noexcept {
    metrics_.reservation_active =
        static_cast<uint64_t>(live_ops_) + live_locks_;
    if (met_sink_ != nullptr)
        met_sink_(met_ctx_, metrics_);
}

// --- WAL -------------------------------------------------------------------------------

bool CrossShardCoordinator::wal_begin(const OpRec& op) noexcept {
    if (wal_ == nullptr)
        return true;
    WalBasketBeginPayload p{};
    p.op_hi = op.id.hi;
    p.op_lo = op.id.lo;
    p.initiator_account = op.initiator_account;
    p.leg_count = op.leg_count;
    p.coordinator_shard = opts_.shard_id;
    p.reserve_deadline_ns = op.reserve_deadline_ns;
    p.total_deadline_ns = op.total_deadline_ns;
    return wal_->append(static_cast<WalEventType>(kWalEvtBasketBegin), &p,
                        sizeof(p)) == WalStatus::Ok;
}

bool CrossShardCoordinator::wal_reserve(const PartRec& r) noexcept {
    if (wal_ == nullptr)
        return true;
    WalBasketReservePayload p{};
    p.op_hi = r.op_id.hi;
    p.op_lo = r.op_id.lo;
    p.coordinator_shard = r.coordinator_shard;
    p.account_id = r.account_id;
    p.order_id = r.order_id;
    p.amount = r.amount;
    p.expires_at_ns = r.expires_at_ns;
    p.leg_index = r.leg_index;
    p.instrument_id = r.instrument_id;
    return wal_->append(static_cast<WalEventType>(kWalEvtBasketReserve), &p,
                        sizeof(p)) == WalStatus::Ok;
}

bool CrossShardCoordinator::wal_leg_done(const PartRec& r, uint8_t kind,
                                         BasketReleaseReason reason) noexcept {
    if (wal_ == nullptr)
        return true;
    WalBasketLegDonePayload p{};
    p.op_hi = r.op_id.hi;
    p.op_lo = r.op_id.lo;
    p.leg_index = r.leg_index;
    p.kind = kind;
    p.reason = static_cast<uint8_t>(reason);
    return wal_->append(static_cast<WalEventType>(kWalEvtBasketLegDone), &p,
                        sizeof(p)) == WalStatus::Ok;
}

bool CrossShardCoordinator::wal_outcome(const OpRec& op) noexcept {
    if (wal_ == nullptr)
        return true;
    WalBasketOutcomePayload p{};
    p.op_hi = op.id.hi;
    p.op_lo = op.id.lo;
    p.initiator_account = op.initiator_account;
    p.duration_ns = 0;  // filled by caller-side accounting when needed
    p.status = op.state;
    p.code = op.code;
    return wal_->append(static_cast<WalEventType>(kWalEvtBasketOutcome), &p,
                        sizeof(p)) == WalStatus::Ok;
}

}  // namespace exch
