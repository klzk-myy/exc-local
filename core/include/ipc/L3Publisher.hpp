#pragma once

// PHASE-17 TASK-17.3.1 — L3 order-level market-data publisher (spec §11).
//
// Emits one L3OrderEvent wire event per order-level lifecycle mutation:
//   * Add     — resting admission (incl. hidden / PEG / pending conditional)
//   * Modify  — qty amend, price amend (atomic cancel-replace legs are one
//               atomic modify in-core, spec AC #57), iceberg DECREMENT/TRANSFER
//               suppression, PEG_REPRICE (flags bit1 set), iceberg slice
//               collapse where the order survives
//   * Cancel  — user cancel, expiry, IOC/FOK remainder, STP kill, OCO sibling,
//               conditional reject, admission terminal notice, auction drain
//   * Fill    — per leg: fill_role 1=taker, 2=maker, 3=auction uncross; the
//               GSLO synthetic venue leg carries flags bit2
//
// Journal-first correlation (spec §24 #318 engine half): every emitted event
// carries wal_seq = the exact sequence of the WAL row that caused it,
// captured BEFORE the append (WalWriter assigns seq == tail_seq() at call
// time). In journal-free replay the engine feeds the cursor via
// set_replay_wal_seq() so the emitted wal_seq values are bit-identical
// across runs.
//
// Ordering contract: MatchingEngine calls publish() only after the WAL row
// is committed and the book mutation is applied. A send() failure is
// backpressure — the event's per-instrument seq is still consumed so
// consumers observe a gap and resync (spec §11.1); the matching loop
// NEVER blocks.
//
// Account pseudonymity: account_id is never serialized. hash_account()
// applies salted FNV-1a-64 — stable across processes/replay (no external
// deps, same hash family as wal_audit fingerprints).
//
// Hot-path budget (spec §3.6): no heap traffic after construction.
//   - flatbuffers::FlatBufferBuilder reserves `builder_capacity` once.
//   - per-instrument seq counters live in a preallocated open-addressed
//     table (`symbol_cap` slots) — no std::map/std::unordered_map.
// Degrade mode (channel==nullptr): everything counts as dropped, no heap.

#include <cstddef>
#include <cstdint>
#include <new>
#include <utility>

#include "book/Order.hpp"
#include "ipc/IpcChannel.hpp"

#if __has_include("exchange_generated.h") && \
    __has_include(<flatbuffers/flatbuffers.h>)
#define EXCH_L3_FLATBUFFERS 1
#include "exchange_generated.h"
#else
#define EXCH_L3_FLATBUFFERS 0
#endif

namespace exch {

// Wire constants (mirror core/proto/exchange.fbs L3OrderEvent docs).
enum class L3Kind : uint8_t {
    Add = 0,
    Modify = 1,
    Cancel = 2,
    Fill = 3,
};

// L3OrderEvent.fill_role values.
inline constexpr uint8_t kL3RoleNone = 0;
inline constexpr uint8_t kL3RoleTaker = 1;
inline constexpr uint8_t kL3RoleMaker = 2;
inline constexpr uint8_t kL3RoleAuction = 3;

// L3OrderEvent.flags bits.
inline constexpr uint8_t kL3FlagHidden = 1u << 0;    // not public-L2 visible
inline constexpr uint8_t kL3FlagPegged = 1u << 1;    // OrderType::PEG
inline constexpr uint8_t kL3FlagSynthetic = 1u << 2; // GSLO venue leg
inline constexpr uint8_t kL3FlagIceberg = 1u << 3;   // OrderType::ICEBERG
inline constexpr uint8_t kL3FlagDetail = 1u << 4;    // side/price/qty populated

// Fully-specified emission record — the engine fills this at each journal
// site; the publisher adds only the per-instrument seq + the envelope.
struct L3Event {
    uint32_t instrument_id = 0;
    uint8_t kind = 0;              // L3Kind
    uint64_t order_id = 0;
    uint64_t account_id = 0;       // raw id — hashed on the wire
    Side side = Side::BUY;
    int64_t price_ticks = 0;
    int64_t ref_price_ticks = 0;
    int64_t qty_units = 0;         // remaining AFTER the event
    int64_t qty_delta = 0;         // signed remaining-qty change
    uint64_t trade_id = 0;
    uint8_t fill_role = 0;
    uint8_t flags = 0;             // kL3Flag* bits
    uint8_t cancel_reason = 0;     // kWalCancelReason* on Cancel
    uint64_t wal_seq = 0;          // triggering WAL row seq
    uint64_t ts_ns = 0;            // engine logical clock (TIME_TICK)
};

class L3Publisher {
public:
    // Emitted before the account bytes so the hash differs from an
    // unsalted FNV-1a of the same id — the literal is part of the wire
    // contract (schema comment) so old/new consumers agree.
    static constexpr char kAccountHashSalt[] = "exc.l3.account.v1";

    explicit L3Publisher(IpcChannel* out,
                         std::size_t builder_capacity = 16u << 10,
                         std::size_t symbol_cap = 4096) noexcept;
    ~L3Publisher();

    L3Publisher(const L3Publisher&) = delete;
    L3Publisher& operator=(const L3Publisher&) = delete;

    // Salted FNV-1a-64 pseudonym — deterministic across processes and
    // replay. Raw account ids never reach the wire.
    static uint64_t hash_account(uint64_t account_id) noexcept;

    // Serializes + sends one event; consumes the next per-instrument seq
    // even when the send fails (gap semantics, spec §11.1). Returns false
    // on send failure / seq-table saturation (counted in drops()).
    [[nodiscard]] bool publish(const L3Event& e) noexcept;

    // Next per-instrument seq that would be assigned. Seqs are 1-BASED —
    // the Go decode gate rejects l3_seq==0 as an absent field.
    [[nodiscard]] uint64_t symbol_seq(uint32_t instrument_id) const noexcept;

    [[nodiscard]] uint64_t published() const noexcept { return published_; }
    [[nodiscard]] uint64_t drops() const noexcept { return drops_; }
    // seq table saturated — events for new instruments were dropped
    // unsequenced (fail-closed: better a gap than an unsequenced row).
    [[nodiscard]] uint64_t seq_overflow() const noexcept { return seq_overflow_; }

private:
    struct SymSlot {          // open-addressed, linear-probe, no rehash
        uint32_t instrument_id = kEmptyIid;
        uint32_t pad = 0;
        uint64_t seq = 0;
    };
    static constexpr uint32_t kEmptyIid = 0xFFFFFFFFu;

    // Assigns the next seq for `iid`; UINT64_MAX when the table is full.
    [[nodiscard]] uint64_t next_seq(uint32_t iid) noexcept;

    IpcChannel* out_;
    SymSlot* sym_ = nullptr;
    std::size_t sym_cap_ = 0;
    std::size_t sym_live_ = 0;

    uint64_t pub_seq_ = 0;
    uint64_t published_ = 0;
    uint64_t drops_ = 0;
    uint64_t seq_overflow_ = 0;

#if EXCH_L3_FLATBUFFERS
    flatbuffers::FlatBufferBuilder builder_;
#endif
};

}  // namespace exch
