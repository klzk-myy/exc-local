#pragma once

// Phase-15 Tasks 15.3.3/15.3.4/15.3.6/15.3.10 — instrument control feed:
// the single immutable snapshot the matching thread reads for lifecycle
// status, market-hours and auction-arming state.
//
// Redis contract (Go publication side — services/internal/admin
// instrument_lifecycle.go / market_schedule.go and services/internal/
// instruments redis.go):
//
//   instrument:status:{symbol}        plain enum word for every non-DRAFT
//                                     instrument ("ACTIVE", "SUSPENDED", …).
//                                     Republished by a 1s reconciler; a
//                                     MISSING key must never read as
//                                     ACTIVE (DRAFT instruments carry no
//                                     key by contract).
//   instrument:auction:{symbol}       "CALL:{deadline_unix_ns}" while a
//                                     reopening call auction is armed,
//                                     rewritten "EXTEND:{deadline_unix_ns}"
//                                     on each 30s extension (ladder owned
//                                     by the Go scheduler — max 3 then
//                                     SUSPENDED). Deleted on direct
//                                     (skip_auction) resumes.
//   instrument:auction:{symbol}:result  "CLEARED" | "FAILED" — written by
//                                     THIS engine (via the control-thread
//                                     refresher — never the matching
//                                     thread) at each deadline strike. An
//                                     absent result counts FAILED on the
//                                     Go side (strict pessimism §2.7).
//   market:hours                      JSON projection:
//                                     {"open_utc":"SUN 21:00",
//                                      "close_utc":"FRI 22:00",
//                                      "pre_open_utc":"SUN 20:45",
//                                      "overrides":[{date:"YYYY-MM-DD",
//                                      "closed":true}|{date,open,close}],
//                                      "published_at":...,"version":N}
//
// Fail-closed discipline (identical to SuspensionFlags): the matching
// thread NEVER touches Redis. InstrumentFeedRefresher (control thread)
// polls, builds a fully-parsed Snapshot, and publishes it atomically; any
// transport/parse failure marks the snapshot unverifiable, and every
// consumer (status gate, market-hours gate) fails closed until a clean
// poll lands. A bound feed that has never verified is therefore closed —
// exactly like SuspensionRefresher's unverified startup state.
//
// Engine→control return path: when the consumed CALL reaches a terminal
// deadline strike the engine calls request_auction_result(); the refresher
// then SETs {key}:result (CLEARED/FAILED) and — on CLEARED only — DELs the
// armed key guarded by a Lua compare on the consumed value so a re-armed
// auction is never killed by a stale release.

#include <atomic>
#include <cstdint>
#include <mutex>
#include <shared_mutex>
#include <string_view>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "risk/RiskInterfaces.hpp"

namespace exch {

// --- market-hours schedule ---------------------------------------------------
// The canonical 24/5 grid is expressed in seconds-of-week (SUN 00:00 UTC
// == 0; Sunday is day-of-week 0). Defaults match spec §7.3: pre-open SUN
// 20:45, open SUN 21:00, close FRI 22:00. Entry is admitted over
// [pre_open_sow, close_sow) — pre-open orders accumulate for the weekly
// reopening auction; the weekend gap (close..pre_open) rejects
// MARKET_CLOSED.
struct MarketHoursOverride {
    int64_t epoch_day = 0;  // days since 1970-01-01 UTC
    bool    closed = false; // whole-day closure
    int64_t open_sod = 0;   // seconds-of-day open  (when !closed)
    int64_t close_sod = 0;  // seconds-of-day close (when !closed)
};

struct MarketHours {
    static constexpr std::size_t kMaxOverrides = 64;
    int64_t pre_open_sow = 20 * 3600 + 45 * 60;        // SUN 20:45 UTC
    int64_t open_sow     = 21 * 3600;                  // SUN 21:00 UTC
    int64_t close_sow    = 5 * 86400 + 22 * 3600;      // FRI 22:00 UTC
    MarketHoursOverride overrides[kMaxOverrides] = {};
    uint32_t  override_count = 0;
};

// Seconds-of-week helper (Sun=0 … Sat=6), pure for any ns timestamp.
[[nodiscard]] inline int64_t seconds_of_week(uint64_t now_ns) noexcept {
    constexpr uint64_t kDayNs = 86'400ull * 1'000'000'000ull;
    const uint64_t days = now_ns / kDayNs;              // 1970-01-01 = Thursday
    const uint64_t sow = (days + 4) % 7 * 86400 +
                         (now_ns % kDayNs) / 1'000'000'000ull;
    return static_cast<int64_t>(sow);
}

// Admission decision — true when new-order entry is allowed at now_ns
// (pre-open admitted; matching itself is gated by the auction key).
// Deterministic: pure function of the schedule + logical timestamp.
[[nodiscard]] inline bool market_entry_allowed(const MarketHours& h,
                                               uint64_t now_ns) noexcept {
    const int64_t epoch_day =
        static_cast<int64_t>(now_ns / 86'400'000'000'000ull);
    for (uint32_t i = 0; i < h.override_count; ++i) {
        const MarketHoursOverride& ov = h.overrides[i];
        if (ov.epoch_day != epoch_day) continue;
        if (ov.closed) return false;
        const int64_t sod = static_cast<int64_t>(
            (now_ns % 86'400'000'000'000ull) / 1'000'000'000ull);
        return sod >= ov.open_sod && sod < ov.close_sod;
    }
    const int64_t sow = seconds_of_week(now_ns);
    return sow >= h.pre_open_sow && sow < h.close_sow;
}

// --- lifecycle gates ----------------------------------------------------------
// Shared status-matrix verdicts so the engine admission gate and
// PreTradeChecker agree bit-for-bit (spec §7.1 table). Codes are the
// registered §23 strings from RiskInterfaces.hpp.
//   nullptr  -> entry/amend permitted
//   otherwise -> rejection code (detail chosen by the caller)
// DRAFT/unknown fail closed as INSTRUMENT_SUSPENDED — the Go order gate
// maps DRAFT to the same code (missing status key ⇒ DRAFT ⇒ fail closed).
[[nodiscard]] inline const char* instrument_entry_gate(
    InstrumentStatus s, OrderType type, uint8_t flags) noexcept {
    switch (s) {
        case InstrumentStatus::ACTIVE:
            return nullptr;
        case InstrumentStatus::CANCEL_ONLY:
            return kCodeInstrumentCancelOnly;
        case InstrumentStatus::SUSPENDED:
        case InstrumentStatus::DRAFT:
            return kCodeInstrumentSuspended;
        case InstrumentStatus::HALTED:
            return kCodeInstrumentHalted;
        case InstrumentStatus::DELISTED:
            // §7.1 remediation #35: reduce_only drains the 30d close window.
            return (flags & kOrderFlagReduceOnly) != 0
                       ? nullptr
                       : kCodeInstrumentDelisted;
        case InstrumentStatus::RESTRICTED:
            return type == OrderType::LIMIT ? nullptr
                                            : kCodeInstrumentRestricted;
        default:
            return kCodeInstrumentSuspended;  // unknown word — fail closed
    }
}

[[nodiscard]] inline const char* instrument_amend_gate(
    InstrumentStatus s) noexcept {
    switch (s) {
        case InstrumentStatus::CANCEL_ONLY:
            return kCodeInstrumentCancelOnly;
        case InstrumentStatus::SUSPENDED:
        case InstrumentStatus::DRAFT:
            return kCodeInstrumentSuspended;
        case InstrumentStatus::HALTED:
            return kCodeInstrumentHalted;
        case InstrumentStatus::DELISTED:
            return kCodeInstrumentDelisted;
        default:
            return nullptr;  // ACTIVE/RESTRICTED: amends permitted
    }
}

// --- control-plane parsers (refresher side; pure functions) -------------------
// All return false on malformed input — the caller decides how to fail.

// "ACTIVE" / "CANCEL_ONLY" / … (exact enum word, case-sensitive per the
// publisher's canonical spelling).
[[nodiscard]] bool parse_instrument_status(std::string_view word,
                                           InstrumentStatus* out) noexcept;

// "CALL:{unix_ns}" or "EXTEND:{unix_ns}" — both arm the auction at the
// given deadline (EXTEND is the Go ladder's +30s rewrite). Returns true on
// a parseable armed value; armed=false is expressed by an absent key.
[[nodiscard]] bool parse_auction_call(std::string_view value,
                                      int64_t* deadline_ns) noexcept;

// market:hours JSON projection → MarketHours. Unknown/missing fields keep
// canonical defaults; malformed structure returns false (fail closed).
[[nodiscard]] bool parse_market_hours_json(std::string_view json,
                                           MarketHours* out) noexcept;

// --- the feed ----------------------------------------------------------------

class InstrumentFeed {
public:
    // Immutable per-poll snapshot (POD — copied under a brief shared lock;
    // ~1.6KB, tens of ns, never blocks a writer for longer than the copy).
    struct Snapshot {
        bool verifiable = false;  // last poll round parsed end-to-end
        InstrumentStatus status = InstrumentStatus::DRAFT;
        bool auction_armed = false;
        int64_t auction_deadline_ns = 0;
        bool market_known = false;   // market:hours key existed + parsed
        MarketHours market{};
    };

    InstrumentFeed() noexcept = default;

    // Hot-path read (matching thread): copy under shared lock.
    [[nodiscard]] Snapshot snapshot() const noexcept {
        std::shared_lock lk(mu_);
        return snap_;
    }

    // Control-thread publish: replace the snapshot atomically.
    void apply(const Snapshot& s) noexcept {
        std::unique_lock lk(mu_);
        snap_ = s;
    }

    // Transport/parse failure — fail closed until a clean poll lands.
    void mark_unverifiable() noexcept {
        std::unique_lock lk(mu_);
        snap_.verifiable = false;
    }

    // --- engine → refresher return path (auction result + key release) -------
    // Called on the matching thread at each terminal deadline strike.
    // `cleared` => write result CLEARED + conditional-DEL the armed key;
    // `!cleared` => write result FAILED (the armed key stays — the Go
    // ladder decides EXTEND vs SUSPENDED).
    void request_auction_result(int64_t auction_id, bool cleared) noexcept {
        // Two atomics instead of a packed struct — the refresher reads
        // cleared only after observing the id store (release ordering).
        auction_result_cleared_.store(cleared ? 1 : 0,
                                      std::memory_order_relaxed);
        auction_result_pending_.store(auction_id,
                                      std::memory_order_release);
    }
    // Returns the pending result auction_id once (0 = none pending); the
    // refresher clears it after the writes land (or fail — the engine's
    // next strike re-arms it).
    [[nodiscard]] int64_t auction_result_pending() const noexcept {
        return auction_result_pending_.load(std::memory_order_acquire);
    }
    [[nodiscard]] bool auction_result_cleared() const noexcept {
        return auction_result_cleared_.load(std::memory_order_relaxed) != 0;
    }
    void clear_auction_result_request() noexcept {
        auction_result_pending_.store(0, std::memory_order_release);
    }

private:
    mutable std::shared_mutex mu_;
    Snapshot snap_{};
    std::atomic<int64_t> auction_result_pending_{0};
    std::atomic<int32_t> auction_result_cleared_{0};
};

}  // namespace exch
