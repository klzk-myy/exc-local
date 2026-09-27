// Task 2.3.12 — CrossShardMarginCoordinator implementation.
// See the header for the protocol, layered-timeout, and WAL contracts
// (spec §5.35 / §13.1 / §24 #176).

#include "risk/CrossShardMarginCoordinator.h"

#include "utils/TimeUtils.hpp"
#include "utils/safe_math.hpp"

namespace exch {

namespace {

// req_flags bit 7 (internal, never on wire): reservation was restored from
// WAL replay rather than granted live — sweep reports its expiry release
// with RecoveryOrphan instead of Expired for auditability.
constexpr uint8_t kFlagRecovered = 0x80;

// Fixed body size per wire type; 0 terminates dispatch (unknown type).
uint32_t margin_body_len(MarginCtlType t) noexcept {
    switch (t) {
        case MarginCtlType::ReserveReq:  return sizeof(MarginReserveReqBody);
        case MarginCtlType::ReserveAck:  return sizeof(MarginReserveAckBody);
        case MarginCtlType::ReserveNack: return sizeof(MarginReserveNackBody);
        case MarginCtlType::Release:     return sizeof(MarginReleaseBody);
    }
    return 0;
}

}  // namespace

const char* margin_ctl_decode_str(MarginCtlDecode r) noexcept {
    switch (r) {
        case MarginCtlDecode::Ok: return "Ok";
        case MarginCtlDecode::TooShort: return "TooShort";
        case MarginCtlDecode::BadMagic: return "BadMagic";
        case MarginCtlDecode::BadVersion: return "BadVersion";
        case MarginCtlDecode::UnknownType: return "UnknownType";
    }
    return "?";
}

uint32_t margin_ctl_encode(uint8_t* dst, uint32_t cap, MarginCtlType type,
                           const void* body) noexcept {
    const uint32_t blen = margin_body_len(type);
    if (dst == nullptr || body == nullptr || blen == 0)
        return 0;
    const uint32_t total = sizeof(MarginCtlHeader) + blen;
    if (cap < total)
        return 0;
    const MarginCtlHeader h{kMarginCtlMagic, static_cast<uint8_t>(type), 0,
                            kMarginCtlVersion};
    std::memcpy(dst, &h, sizeof(h));
    std::memcpy(dst + sizeof(h), body, blen);
    return total;
}

MarginCtlDecode margin_ctl_decode(const void* buf, uint32_t len,
                                  MarginCtlView* out) noexcept {
    if (buf == nullptr || out == nullptr || len < sizeof(MarginCtlHeader))
        return MarginCtlDecode::TooShort;
    MarginCtlHeader h;
    std::memcpy(&h, buf, sizeof(h));
    if (h.magic != kMarginCtlMagic)
        return MarginCtlDecode::BadMagic;
    if (h.version != kMarginCtlVersion)
        return MarginCtlDecode::BadVersion;
    const auto type = static_cast<MarginCtlType>(h.type);
    const uint32_t blen = margin_body_len(type);
    if (blen == 0)
        return MarginCtlDecode::UnknownType;
    if (len < sizeof(h) + blen)  // trailing pad tolerated (forward-compat)
        return MarginCtlDecode::TooShort;
    const uint8_t* p = static_cast<const uint8_t*>(buf) + sizeof(h);
    out->type = type;
    switch (type) {
        case MarginCtlType::ReserveReq:
            std::memcpy(&out->req, p, sizeof(out->req));
            break;
        case MarginCtlType::ReserveAck:
            std::memcpy(&out->ack, p, sizeof(out->ack));
            break;
        case MarginCtlType::ReserveNack:
            std::memcpy(&out->nack, p, sizeof(out->nack));
            break;
        case MarginCtlType::Release:
            std::memcpy(&out->rel, p, sizeof(out->rel));
            break;
    }
    return MarginCtlDecode::Ok;
}

// --- CrossShardMarginCoordinator ------------------------------------------------

CrossShardMarginCoordinator::CrossShardMarginCoordinator(
    IpcChannel* ctl, Wal* wal, MarginCoordinatorOptions opts) noexcept
    : ctl_(ctl), wal_(wal), opts_(opts) {
    // Placement-zero the fixed tables (Empty / account_id=0 sentinels).
    for (auto& s : slots_) s.r.state = static_cast<uint8_t>(ReservationState::Empty);
}

int64_t CrossShardMarginCoordinator::steady() const noexcept {
    return opts_.steady_ns_fn ? opts_.steady_ns_fn(opts_.steady_ctx)
                              : static_cast<int64_t>(steady_ns());
}
uint64_t CrossShardMarginCoordinator::wall_now() const noexcept {
    return opts_.now_ns_fn ? opts_.now_ns_fn(opts_.now_ctx) : now_ns();
}

// --- table primitives ----------------------------------------------------------

MarginReservation*
CrossShardMarginCoordinator::find_res(uint64_t id) noexcept {
    uint32_t i = hash64(id) & (kMaxReservations - 1);
    for (uint32_t n = 0; n < kMaxReservations; ++n) {
        const auto st = static_cast<ReservationState>(slots_[i].r.state);
        if (st == ReservationState::Empty)
            return nullptr;  // probe terminator — absent
        if (slots_[i].r.id == id)
            return &slots_[i].r;
        i = (i + 1) & (kMaxReservations - 1);
    }
    return nullptr;
}

const MarginReservation*
CrossShardMarginCoordinator::find_res(uint64_t id) const noexcept {
    return const_cast<CrossShardMarginCoordinator*>(this)->find_res(id);
}

MarginReservation*
CrossShardMarginCoordinator::insert_res(uint64_t id) noexcept {
    uint32_t i = hash64(id) & (kMaxReservations - 1);
    int32_t tomb = -1;
    for (uint32_t n = 0; n < kMaxReservations; ++n) {
        const auto st = static_cast<ReservationState>(slots_[i].r.state);
        if (slots_[i].r.id == id && st != ReservationState::Empty)
            return &slots_[i].r;  // already present
        if (st == ReservationState::Empty)
            break;  // key absent proven; insert below
        if (tomb < 0 && (st == ReservationState::Denied ||
                         st == ReservationState::Released))
            tomb = static_cast<int32_t>(i);  // remember first reusable tombstone
        i = (i + 1) & (kMaxReservations - 1);
    }
    if (tomb >= 0) {
        // Key absent (probe reached Empty) — safe to reuse the tombstone.
        return &slots_[tomb].r;
    }
    if (static_cast<ReservationState>(slots_[i].r.state) ==
        ReservationState::Empty)
        return &slots_[i].r;
    return nullptr;  // table full of live records — fail closed
}

CrossShardMarginCoordinator::AccountAgg*
CrossShardMarginCoordinator::find_agg(uint64_t account_id) noexcept {
    uint32_t i = hash64(account_id) & (kMaxAccounts - 1);
    for (uint32_t n = 0; n < kMaxAccounts; ++n) {
        if (aggs_[i].account_id == 0)
            return nullptr;
        if (aggs_[i].account_id == account_id)
            return &aggs_[i];
        i = (i + 1) & (kMaxAccounts - 1);
    }
    return nullptr;
}

const CrossShardMarginCoordinator::AccountAgg*
CrossShardMarginCoordinator::find_agg(uint64_t account_id) const noexcept {
    return const_cast<CrossShardMarginCoordinator*>(this)->find_agg(account_id);
}

CrossShardMarginCoordinator::AccountAgg*
CrossShardMarginCoordinator::ensure_agg(uint64_t account_id) noexcept {
    uint32_t i = hash64(account_id) & (kMaxAccounts - 1);
    for (uint32_t n = 0; n < kMaxAccounts; ++n) {
        if (aggs_[i].account_id == account_id)
            return &aggs_[i];
        if (aggs_[i].account_id == 0) {
            aggs_[i].account_id = account_id;  // fresh slot (sums already 0)
            return &aggs_[i];
        }
        i = (i + 1) & (kMaxAccounts - 1);
    }
    return nullptr;
}

int64_t CrossShardMarginCoordinator::local_available(
    uint64_t account_id) const noexcept {
    if (local_fn_ != nullptr)
        return local_fn_(local_ctx_, account_id);
    const AccountAgg* a = find_agg(account_id);
    return a ? a->local_headroom : 0;  // unknown account -> 0 (fail closed)
}

// --- evaluation ---------------------------------------------------------------

MarginCheck CrossShardMarginCoordinator::evaluate(
    uint64_t account_id, int64_t required_margin) noexcept {
    MarginCheck out{false, 0};
    if (required_margin <= 0) {  // nothing required — trivially covered
        out.covered = true;
        return out;
    }
    const AccountAgg* a = find_agg(account_id);
    const int64_t local = local_available(account_id);
    const int64_t locked = a ? a->locked_out : 0;
    const int64_t granted = a ? a->granted_in : 0;

    // usable = local - locked_out + granted_in, all in ticks. Saturating
    // failure on overflow -> covered=false (fail closed, spec §2.7.1).
    int64_t usable;
    if (!safe_math::try_sub(local, locked, usable) ||
        !safe_math::try_add(usable, granted, usable)) {
        return out;  // covered=false
    }
    out.covered = usable >= required_margin;
    out.shortfall = out.covered ? 0 : required_margin - usable;
    return out;
}

// --- requester path -------------------------------------------------------------

uint64_t CrossShardMarginCoordinator::begin_reserve(
    uint64_t account_id, uint32_t instrument_id, int64_t amount,
    uint64_t expires_at_ns, bool needs_correlation_offset, uint32_t dst_shard,
    uint64_t order_id) noexcept {
    // Channel down / bad args -> 0: caller applies the pessimistic floor.
    if (ctl_ == nullptr || !ctl_->is_open() || amount <= 0 || account_id == 0)
        return 0;

    // reservation_id: high16 = issuing shard, low48 = per-shard sequence
    // (monotonic; reseeded past the WAL max on recover(), so a fresh id can
    // never collide with a live or tombstoned record). A retried reservation
    // always gets a fresh id.
    const uint64_t id =
        (static_cast<uint64_t>(opts_.shard_id) << 48) |
        (++local_seq_ & 0x0000FFFFFFFFFFFFULL);
    MarginReservation* s = insert_res(id);
    if (s == nullptr)
        return 0;  // table full of live reservations — fail closed
    if (s->id == id &&
        static_cast<ReservationState>(s->state) != ReservationState::Empty &&
        static_cast<ReservationState>(s->state) != ReservationState::Denied &&
        static_cast<ReservationState>(s->state) != ReservationState::Released)
        return 0;  // pathological live collision — fail closed, seq is burnt

    const int64_t now = steady();
    *s = MarginReservation{};
    s->id = id;
    s->account_id = account_id;
    s->order_id = order_id;
    s->consumer_shard = opts_.shard_id;   // our shard consumes this slice
    s->host_shard = dst_shard;            // kMarginCoordinatorPicks allowed
    s->instrument_id = instrument_id;
    s->amount = amount;
    s->expires_at_ns = expires_at_ns;
    s->t_soft_ns = now + opts_.rpc_budget_ns;
    s->t_hard_ns = now + opts_.hard_deadline_ns;
    s->req_flags =
        needs_correlation_offset ? kMarginReqFlagCorrelationOffset : 0;
    s->origin = 0;  // Local
    s->state = static_cast<uint8_t>(ReservationState::Pending);

    // WAL the intent BEFORE announcing the id (origin=2): on replay the id
    // sequence is reseeded past it, so a coordinator ACK arriving after a
    // restart can never collide with a recycled id. Fail-closed: an
    // unpersistable intent is an id we must not announce.
    if (wal_ != nullptr && !wal_intent(*s)) {
        ++n_wal_fail_;
        s->state = static_cast<uint8_t>(ReservationState::Released);
        return 0;
    }
    if (!send_req(*s)) {
        // Wire failure: nothing was committed — tombstone the slot (NEVER
        // Empty: clearing a mid-chain slot would orphan keys whose probe
        // path passes through it).
        s->state = static_cast<uint8_t>(ReservationState::Released);
        return 0;
    }
    if (live_n_ < kMaxReservations)
        live_ids_[live_n_++] = id;
    ++n_reqs_;
    return id;
}

MarginResolve CrossShardMarginCoordinator::resolve_wait(
    uint64_t reservation_id) noexcept {
    MarginReservation* r = find_res(reservation_id);
    if (r == nullptr || r->origin != 0)
        return MarginResolve::Unknown;

    for (;;) {
        poll();  // drain inbound + sweep deadlines (drives the state machine)
        const auto st = static_cast<ReservationState>(r->state);
        switch (st) {
            case ReservationState::Granted:
                return MarginResolve::Granted;
            case ReservationState::Denied:
                return MarginResolve::Denied;
            case ReservationState::Released:
                // A release landed before we ever resolved: only possible via
                // inbound cancel or hard-deadline compensation.
                return MarginResolve::HardTimeout;
            case ReservationState::TimedOut:
                return MarginResolve::Fallback;
            case ReservationState::Pending:
            case ReservationState::GrantedHosted:
            case ReservationState::Empty:
                break;
        }
        if (steady() >= r->t_soft_ns) {
            // 500µs RPC budget spent — pessimistic fallback applies NOW while
            // the REQ continues in the background toward the hard deadline
            // (spec §13.1 layered budget). Caller rejects relying orders with
            // CROSS_SHARD_MARGIN_UNAVAILABLE.
            r->state = static_cast<uint8_t>(ReservationState::TimedOut);
            ++n_soft_;
            return MarginResolve::Fallback;
        }
        // Bounded spin: each iteration drains the channel; the loop is
        // time-bounded by t_soft_ns (≤500µs) so the matching loop never
        // blocks beyond the coordinator RPC budget.
    }
}

// --- public lifecycle -------------------------------------------------------------

bool CrossShardMarginCoordinator::release(uint64_t reservation_id,
                                        ReleaseReason reason) noexcept {
    MarginReservation* r = find_res(reservation_id);
    if (r == nullptr)
        return true;  // unknown id — release is idempotent by contract
    const auto st = static_cast<ReservationState>(r->state);
    switch (st) {
        case ReservationState::Denied:
        case ReservationState::Released:
        case ReservationState::Empty:
            return true;  // already terminal — idempotent no-op
        case ReservationState::Granted:
        case ReservationState::GrantedHosted:
        case ReservationState::Pending:
        case ReservationState::TimedOut:
            finalize_release(*r, reason, /*send_wire=*/true,
                             /*write_wal=*/true);
            return true;
    }
    return true;
}

void CrossShardMarginCoordinator::poll() noexcept {
    if (ctl_ != nullptr && ctl_->is_open()) {
        uint8_t buf[kMarginCtlMaxFrame];
        // Bounded drain per poll — control traffic is tiny; 64 frames/turn
        // leaves matching-loop budget intact under a flood.
        for (uint32_t i = 0; i < 64; ++i) {
            const int32_t n = ctl_->poll(buf, sizeof(buf));
            if (n > 0) {
                on_frame(buf, static_cast<uint32_t>(n));
            } else {
                if (n < 0)
                    ++n_bad_;  // channel-level fault
                break;
            }
        }
    }
    sweep();
}

// --- local margin plumbing ---------------------------------------------------------

void CrossShardMarginCoordinator::set_local_headroom(
    uint64_t account_id, int64_t headroom) noexcept {
    AccountAgg* a = ensure_agg(account_id);
    if (a != nullptr)
        a->local_headroom = headroom;
}

void CrossShardMarginCoordinator::set_local_margin_provider(
    int64_t (*fn)(void*, uint64_t) noexcept, void* ctx) noexcept {
    local_fn_ = fn;
    local_ctx_ = ctx;
}

// --- recovery -------------------------------------------------------------------------

size_t CrossShardMarginCoordinator::recover(WalReader& reader) noexcept {
    size_t applied = 0;
    WalEntryView v;
    while (reader.next(v) == WalScanStep::Entry) {
        if (v.type == WalEventType::MARGIN_RESERVE &&
            v.payload_len == sizeof(WalMarginReservePayload)) {
            WalMarginReservePayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            if (p.origin == 2) {
                // Uncommitted intent — restores NO slice state; its only
                // effect is reseeding the local id sequence past every
                // announced id (a late ACK post-restart for such an id then
                // misses the table and is compensated via the unknown-ACK
                // RELEASE path). A grant for the same id may still follow —
                // no tombstone is created here.
                if ((p.reservation_id >> 48) ==
                    static_cast<uint64_t>(opts_.shard_id)) {
                    const uint64_t low =
                        p.reservation_id & 0x0000FFFFFFFFFFFFULL;
                    if (low > local_seq_)
                        local_seq_ = low;
                }
                ++applied;
                continue;
            }
            MarginReservation* s = find_res(p.reservation_id);
            if (s == nullptr)
                s = insert_res(p.reservation_id);
            if (s == nullptr)
                continue;  // table full — cannot restore; fail-closed: skip
            const auto st = static_cast<ReservationState>(s->state);
            if (st == ReservationState::Released ||
                st == ReservationState::Denied)
                continue;  // already resolved by a later RELEASE entry
            *s = MarginReservation{};
            s->id = p.reservation_id;
            s->account_id = p.account_id;
            s->order_id = p.order_id;
            s->consumer_shard = p.consumer_shard;
            s->host_shard = p.host_shard;
            s->instrument_id = p.instrument_id;
            s->amount = p.amount;
            s->expires_at_ns = p.expires_at_ns;
            s->origin = p.origin;
            s->req_flags = kFlagRecovered;  // sweep reason select
            s->state = static_cast<uint8_t>(
                p.origin == 0 ? ReservationState::Granted
                              : ReservationState::GrantedHosted);
            AccountAgg* a = ensure_agg(p.account_id);
            if (a != nullptr) {
                if (p.origin == 0)
                    a->granted_in += p.amount;
                else
                    a->locked_out += p.amount;
            }
            if (live_n_ < kMaxReservations)
                live_ids_[live_n_++] = p.reservation_id;
            // Reseed the local id sequence from OUR reservations only.
            if ((p.reservation_id >> 48) ==
                static_cast<uint64_t>(opts_.shard_id)) {
                const uint64_t low = p.reservation_id & 0x0000FFFFFFFFFFFFULL;
                if (low > local_seq_)
                    local_seq_ = low;
            }
            ++applied;
        } else if (v.type == WalEventType::MARGIN_RELEASE &&
                   v.payload_len == sizeof(WalMarginReleasePayload)) {
            WalMarginReleasePayload p;
            std::memcpy(&p, v.payload, sizeof(p));
            MarginReservation* s = find_res(p.reservation_id);
            if (s == nullptr) {
                // Release without a preceding reserve (defensive — in-order
                // WAL never produces this): leave a Released tombstone so a
                // replayed ACK/REQ cannot resurrect it.
                s = insert_res(p.reservation_id);
                if (s != nullptr) {
                    *s = MarginReservation{};
                    s->id = p.reservation_id;
                    s->account_id = p.account_id;
                    s->origin = p.origin;
                    s->state =
                        static_cast<uint8_t>(ReservationState::Released);
                }
            } else {
                finalize_release(*s,
                                 static_cast<ReleaseReason>(p.reason),
                                 /*send_wire=*/false, /*write_wal=*/false);
            }
            ++applied;
        }
        // other event types: not ours — skip
    }
    return applied;
}

// --- introspection ---------------------------------------------------------------------

ReservationState CrossShardMarginCoordinator::state_of(
    uint64_t reservation_id) const noexcept {
    const MarginReservation* r = find_res(reservation_id);
    return r ? static_cast<ReservationState>(r->state)
             : ReservationState::Empty;
}

int64_t CrossShardMarginCoordinator::granted_slices(
    uint64_t account_id) const noexcept {
    const AccountAgg* a = find_agg(account_id);
    return a ? a->granted_in : 0;
}

int64_t CrossShardMarginCoordinator::hosted_locks(
    uint64_t account_id) const noexcept {
    const AccountAgg* a = find_agg(account_id);
    return a ? a->locked_out : 0;
}

// --- wire send helpers -----------------------------------------------------------------

bool CrossShardMarginCoordinator::send_req(
    const MarginReservation& r) noexcept {
    MarginReserveReqBody b{};
    b.reservation_id = r.id;
    b.account_id = r.account_id;
    b.order_id = r.order_id;
    b.src_shard = r.consumer_shard;
    b.dst_shard = r.host_shard;
    b.instrument_id = r.instrument_id;
    b.req_flags = r.req_flags;
    b.amount = r.amount;
    b.expires_at_ns = r.expires_at_ns;
    uint8_t buf[kMarginCtlMaxFrame];
    const uint32_t n =
        margin_ctl_encode(buf, sizeof(buf), MarginCtlType::ReserveReq, &b);
    return n != 0 && ctl_->send(buf, n);
}

bool CrossShardMarginCoordinator::send_ack(
    const MarginReservation& r) noexcept {
    MarginReserveAckBody b{};
    b.reservation_id = r.id;
    b.account_id = r.account_id;
    b.shard_id = opts_.shard_id;  // we host
    b.granted_amount = r.amount;
    b.expires_at_ns = r.expires_at_ns;
    uint8_t buf[kMarginCtlMaxFrame];
    const uint32_t n =
        margin_ctl_encode(buf, sizeof(buf), MarginCtlType::ReserveAck, &b);
    return n != 0 && ctl_->send(buf, n);
}

bool CrossShardMarginCoordinator::send_nack(uint64_t id, uint64_t account,
                                            uint32_t shard,
                                            NackReason reason) noexcept {
    MarginReserveNackBody b{};
    b.reservation_id = id;
    b.account_id = account;
    b.shard_id = shard;
    b.reason = static_cast<uint32_t>(reason);
    uint8_t buf[kMarginCtlMaxFrame];
    const uint32_t n =
        margin_ctl_encode(buf, sizeof(buf), MarginCtlType::ReserveNack, &b);
    return n != 0 && ctl_->send(buf, n);
}

bool CrossShardMarginCoordinator::send_release(
    const MarginReservation& r, ReleaseReason reason) noexcept {
    MarginReleaseBody b{};
    b.reservation_id = r.id;
    b.account_id = r.account_id;
    // shard_id = shard hosting the released slice; for records that never
    // reached a known host fall back to our own shard (cancel semantics).
    b.shard_id = (r.host_shard != 0 && r.host_shard != kMarginCoordinatorPicks)
                     ? r.host_shard
                     : opts_.shard_id;
    b.reason = static_cast<uint8_t>(reason);
    uint8_t buf[kMarginCtlMaxFrame];
    const uint32_t n =
        margin_ctl_encode(buf, sizeof(buf), MarginCtlType::Release, &b);
    return n != 0 && ctl_->send(buf, n);
}

// --- inbound dispatch -------------------------------------------------------------------

void CrossShardMarginCoordinator::on_frame(const uint8_t* buf,
                                         uint32_t len) noexcept {
    MarginCtlView v;
    if (margin_ctl_decode(buf, len, &v) != MarginCtlDecode::Ok) {
        ++n_bad_;  // malformed control frame — drop, fail closed
        return;
    }
    switch (v.type) {
        case MarginCtlType::ReserveReq:  on_req(v.req);   break;
        case MarginCtlType::ReserveAck:  on_ack(v.ack);   break;
        case MarginCtlType::ReserveNack: on_nack(v.nack); break;
        case MarginCtlType::Release:     on_release(v.rel); break;
    }
}

// Participant role: the coordinator asks THIS shard to host a lock for a
// remote reservation. Idempotent on redelivery: a duplicate REQ for an
// already-granted id re-ACKs the original decision (no double-lock).
void CrossShardMarginCoordinator::on_req(
    const MarginReserveReqBody& m) noexcept {
    if (m.account_id == 0 || m.amount <= 0) {
        ++n_bad_;
        return;
    }
    MarginReservation* r = find_res(m.reservation_id);
    if (r != nullptr) {
        const auto st = static_cast<ReservationState>(r->state);
        if (st == ReservationState::GrantedHosted)
            (void)send_ack(*r);          // idempotent re-ack
        else if (st != ReservationState::Empty)
            (void)send_nack(m.reservation_id, m.account_id, opts_.shard_id,
                            NackReason::InsufficientHeadroom);
        return;
    }
    // Admission for the hosted slice: account headroom net of already-locked
    // slices must cover the request — pessimistic, zero-breach floor.
    const AccountAgg* a = find_agg(m.account_id);
    const int64_t locked = a ? a->locked_out : 0;
    int64_t usable;
    if (!safe_math::try_sub(local_available(m.account_id), locked, usable) ||
        usable < m.amount) {
        (void)send_nack(m.reservation_id, m.account_id, opts_.shard_id,
                        NackReason::InsufficientHeadroom);
        // Tombstone so a replayed REQ deterministically re-NACKs.
        MarginReservation* t = insert_res(m.reservation_id);
        if (t != nullptr) {
            *t = MarginReservation{};
            t->id = m.reservation_id;
            t->account_id = m.account_id;
            t->consumer_shard = m.src_shard;
            t->host_shard = opts_.shard_id;
            t->amount = m.amount;
            t->origin = 1;
            t->state = static_cast<uint8_t>(ReservationState::Denied);
        }
        return;
    }
    MarginReservation* s = insert_res(m.reservation_id);
    if (s == nullptr) {
        (void)send_nack(m.reservation_id, m.account_id, opts_.shard_id,
                        NackReason::InsufficientHeadroom);
        return;
    }
    *s = MarginReservation{};
    s->id = m.reservation_id;
    s->account_id = m.account_id;
    s->order_id = m.order_id;
    s->consumer_shard = m.src_shard;   // remote shard consumes
    s->host_shard = opts_.shard_id;    // we host the collateral slice
    s->instrument_id = m.instrument_id;
    s->amount = m.amount;
    s->expires_at_ns = m.expires_at_ns;
    s->req_flags = m.req_flags;
    s->origin = 1;
    s->state = static_cast<uint8_t>(ReservationState::GrantedHosted);

    // Commit ordering: WAL first — a slice we cannot persist must not be
    // ACK'd (fail closed; the coordinator retries or NACKs elsewhere).
    if (wal_reserve(*s)) {
        AccountAgg* ag = ensure_agg(m.account_id);
        if (ag != nullptr)
            ag->locked_out += m.amount;
        if (live_n_ < kMaxReservations)
            live_ids_[live_n_++] = m.reservation_id;
        (void)send_ack(*s);
    } else {
        // Cannot persist the lock -> refuse it (fail closed). Denied
        // tombstone — NOT Empty — keeps probe chains intact and dedupes a
        // replayed REQ into the same deterministic NACK.
        ++n_wal_fail_;
        s->state = static_cast<uint8_t>(ReservationState::Denied);
        (void)send_nack(m.reservation_id, m.account_id, opts_.shard_id,
                        NackReason::InsufficientHeadroom);
    }
}

// Requester role: grant for our own REQ.
void CrossShardMarginCoordinator::on_ack(
    const MarginReserveAckBody& m) noexcept {
    ++n_acks_;
    MarginReservation* r = find_res(m.reservation_id);
    if (r == nullptr || r->origin != 0) {
        // ACK for an id we never issued (or already forgot): the coordinator
        // believes a slice is held for us — compensate with RELEASE so its
        // books cannot drift (spec §2.7 zero-loss pessimism).
        MarginReservation tmp{};
        tmp.id = m.reservation_id;
        tmp.account_id = m.account_id;
        tmp.host_shard = m.shard_id;
        (void)send_release(tmp, ReleaseReason::TimeoutCompensate);
        return;
    }
    const auto st = static_cast<ReservationState>(r->state);
    if (st == ReservationState::Granted)
        return;  // duplicate ACK — idempotent
    if (st == ReservationState::Denied || st == ReservationState::Released ||
        st == ReservationState::Empty) {
        // ACK for a dead reservation (protocol violation or tombstone):
        // the coordinator believes a slice is held — compensate so its
        // books cannot drift (fail-closed symmetric bookkeeping).
        (void)send_release(*r, ReleaseReason::TimeoutCompensate);
        ++n_comp_;
        return;
    }
    // Pending or TimedOut: commit the granted slice...
    r->host_shard = m.shard_id;  // coordinator tells us who hosts it
    grant_local(*r, m.granted_amount > 0 ? m.granted_amount : r->amount,
                m.expires_at_ns);
    const auto now_st = static_cast<ReservationState>(r->state);
    if (now_st == ReservationState::Granted && steady() >= r->t_soft_ns) {
        // ...then, if the soft budget already expired, the triggering order
        // was rejected on the pessimistic floor — the late slice is unwound
        // immediately (cancel + compensate, spec §13.1 layered budget).
        finalize_release(*r, ReleaseReason::TimeoutCompensate,
                         /*send_wire=*/true, /*write_wal=*/true);
        ++n_comp_;
    } else if (now_st == ReservationState::Denied) {
        // WAL-failed grant: coordinator still believes it committed a slice
        // for us — release it on the wire so its books cannot drift.
        (void)send_release(*r, ReleaseReason::TimeoutCompensate);
        ++n_comp_;
    }
}

void CrossShardMarginCoordinator::on_nack(
    const MarginReserveNackBody& m) noexcept {
    ++n_nacks_;
    MarginReservation* r = find_res(m.reservation_id);
    if (r == nullptr || r->origin != 0)
        return;
    const auto st = static_cast<ReservationState>(r->state);
    if (st == ReservationState::Pending ||
        st == ReservationState::TimedOut) {
        r->state = static_cast<uint8_t>(ReservationState::Denied);
        live_drop(m.reservation_id);  // dead — nothing committed to release
    }
}

// Inbound RELEASE: coordinator-driven cancel/unlock. Idempotent — unknown or
// already-terminal ids are a no-op (out-of-order delivery safe).
void CrossShardMarginCoordinator::on_release(
    const MarginReleaseBody& m) noexcept {
    MarginReservation* r = find_res(m.reservation_id);
    if (r == nullptr)
        return;
    const auto st = static_cast<ReservationState>(r->state);
    switch (st) {
        case ReservationState::Granted:
        case ReservationState::GrantedHosted:
            // Committed slice — unwind aggs + WAL the transition.
            finalize_release(*r, ReleaseReason::CoordinatorInitiated,
                             /*send_wire=*/false, /*write_wal=*/true);
            break;
        case ReservationState::Pending:
        case ReservationState::TimedOut:
            // In-flight REQ cancelled by the coordinator — nothing was ever
            // committed, so nothing is WAL'd; tombstone only.
            r->state = static_cast<uint8_t>(ReservationState::Released);
            live_drop(m.reservation_id);
            break;
        case ReservationState::Denied:
        case ReservationState::Released:
        case ReservationState::Empty:
            break;  // idempotent
    }
}

// --- deadline / expiry sweep ---------------------------------------------------------

void CrossShardMarginCoordinator::sweep() noexcept {
    const int64_t now = steady();
    const uint64_t wall = wall_now();
    // Iterate over the live-id list only — O(live) not O(capacity).
    // finalize_release() compacts the list itself (live_drop swaps the last
    // live id into slot i), so after any finalize the current index must be
    // re-examined WITHOUT advancing or decrementing — doing both is a
    // double-compaction and underflows live_n_.
    for (uint32_t i = 0; i < live_n_;) {
        MarginReservation* r = find_res(live_ids_[i]);
        if (r == nullptr) {  // stale id — compact defensively
            live_ids_[i] = live_ids_[--live_n_];
            continue;
        }
        const auto st = static_cast<ReservationState>(r->state);
        bool finalized = false;  // finalize_release already dropped slot i
        switch (st) {
            case ReservationState::Pending:
                if (now >= r->t_soft_ns) {
                    r->state =
                        static_cast<uint8_t>(ReservationState::TimedOut);
                    ++n_soft_;
                }
                if (now >= r->t_hard_ns) {
                    // >10ms hard deadline: cancel + compensate (spec §13.1).
                    finalize_release(*r, ReleaseReason::TimeoutCompensate,
                                     /*send_wire=*/true, /*write_wal=*/true);
                    ++n_hard_;
                    ++n_comp_;
                    finalized = true;
                }
                break;
            case ReservationState::TimedOut:
                if (now >= r->t_hard_ns) {
                    finalize_release(*r, ReleaseReason::TimeoutCompensate,
                                     /*send_wire=*/true, /*write_wal=*/true);
                    ++n_hard_;
                    ++n_comp_;
                    finalized = true;
                }
                break;
            case ReservationState::Granted:
            case ReservationState::GrantedHosted:
                if (r->expires_at_ns != 0 && wall >= r->expires_at_ns) {
                    // Recovered slices distinguish as RecoveryOrphan for
                    // audit; live ones are plain Expired.
                    const bool rec = (r->req_flags & kFlagRecovered) != 0;
                    finalize_release(*r,
                                     rec ? ReleaseReason::RecoveryOrphan
                                         : ReleaseReason::Expired,
                                     /*send_wire=*/true, /*write_wal=*/true);
                    finalized = true;
                }
                break;
            default:
                // Terminal/empty record on the live list (defensive — all
                // real transitions drop it): compact manually.
                live_ids_[i] = live_ids_[--live_n_];
                finalized = true;  // slot i now holds the swapped element
                break;
        }
        if (!finalized)
            ++i;
        // else: live_ids_[i] now holds a different (swapped) id — re-examine.
    }
}

// --- state transitions ----------------------------------------------------------------

void CrossShardMarginCoordinator::grant_local(MarginReservation& r,
                                              int64_t amount,
                                              uint64_t expires_at_ns) noexcept {
    const bool was_pending =
        static_cast<ReservationState>(r.state) == ReservationState::Pending ||
        static_cast<ReservationState>(r.state) == ReservationState::TimedOut;
    r.amount = amount;
    r.expires_at_ns = expires_at_ns;
    if (!wal_reserve(r)) {
        // Cannot persist the commitment — refuse the grant (fail closed).
        ++n_wal_fail_;
        r.state = static_cast<uint8_t>(ReservationState::Denied);
        live_drop(r.id);
        return;
    }
    r.state = static_cast<uint8_t>(ReservationState::Granted);
    AccountAgg* a = ensure_agg(r.account_id);
    if (a != nullptr)
        a->granted_in += r.amount;
    if (!was_pending && live_n_ < kMaxReservations)
        live_ids_[live_n_++] = r.id;
}

void CrossShardMarginCoordinator::finalize_release(
    MarginReservation& r, ReleaseReason reason, bool send_wire,
    bool write_wal) noexcept {
    const auto st = static_cast<ReservationState>(r.state);
    // Unwind committed aggregates BEFORE tombstoning.
    AccountAgg* a = find_agg(r.account_id);
    if (a != nullptr) {
        if (st == ReservationState::Granted && r.origin == 0)
            a->granted_in -= r.amount;
        else if (st == ReservationState::GrantedHosted && r.origin == 1)
            a->locked_out -= r.amount;
    }
    if (send_wire)
        (void)send_release(r, reason);
    if (write_wal)
        (void)wal_release(r, reason);  // release WAL failure is not fatal:
                                       // the in-memory state is already
                                       // correct; the gap is a Phase-04
                                       // recovery-ladder audit item.
    r.state = static_cast<uint8_t>(ReservationState::Released);
    live_drop(r.id);
    ++n_rel_;
}

void CrossShardMarginCoordinator::live_drop(uint64_t id) noexcept {
    for (uint32_t i = 0; i < live_n_; ++i) {
        if (live_ids_[i] == id) {
            live_ids_[i] = live_ids_[--live_n_];
            return;
        }
    }
}

// --- WAL ------------------------------------------------------------------------------

bool CrossShardMarginCoordinator::wal_reserve(
    const MarginReservation& r) noexcept {
    if (wal_ == nullptr)
        return true;  // persistence disabled (tests/bench) — treat as committed
    WalMarginReservePayload p{};
    p.reservation_id = r.id;
    p.account_id = r.account_id;
    p.order_id = r.order_id;
    p.consumer_shard = r.consumer_shard;
    p.host_shard = r.host_shard;
    p.instrument_id = r.instrument_id;
    p.origin = r.origin;
    p.amount = r.amount;
    p.expires_at_ns = r.expires_at_ns;
    return wal_->append(WalEventType::MARGIN_RESERVE, &p, sizeof(p)) ==
           WalStatus::Ok;
}

bool CrossShardMarginCoordinator::wal_intent(
    const MarginReservation& r) noexcept {
    if (wal_ == nullptr)
        return true;
    WalMarginReservePayload p{};
    p.reservation_id = r.id;
    p.account_id = r.account_id;
    p.order_id = r.order_id;
    p.consumer_shard = r.consumer_shard;
    p.host_shard = r.host_shard;
    p.instrument_id = r.instrument_id;
    p.origin = 2;  // Intent — uncommitted; replay reseeds the id sequence only
    p.amount = r.amount;
    p.expires_at_ns = r.expires_at_ns;
    return wal_->append(WalEventType::MARGIN_RESERVE, &p, sizeof(p)) ==
           WalStatus::Ok;
}

bool CrossShardMarginCoordinator::wal_release(
    const MarginReservation& r, ReleaseReason reason) noexcept {
    if (wal_ == nullptr)
        return true;
    WalMarginReleasePayload p{};
    p.reservation_id = r.id;
    p.account_id = r.account_id;
    p.origin = r.origin;
    p.reason = static_cast<uint8_t>(reason);
    return wal_->append(WalEventType::MARGIN_RELEASE, &p, sizeof(p)) ==
           WalStatus::Ok;
}

}  // namespace exch
