#pragma once

// Production IAccountState + IPositionState provider for the C++ matching
// core (closes the Phase-2 finding that production boots ran with no bound
// account store — check() on an unbound checker returns ACCEPT, leaving
// all account/balance/KYC/STP-default enforcement to the Go gateway).
//
// The Go control plane (services/internal/risk/account_state.go —
// AccountStateProjector) is the authority over the `accounts`,
// `balances`, `positions` and `instruments` tables; it publishes an
// atomic snapshot to the coordination Redis keyspace on a ~1s cadence:
//
//   account:state   HASH — one shot per publish, four field shapes:
//       "{account_id}"              -> "<STATUS>,<KYC>,<CATEGORY>,<STP>,<OPEN_POS>"
//                                      e.g. "ACTIVE,T2,RETAIL,CANCEL_NEWEST,3"
//       "a:{account_id}:{CCY}"      -> available balance, int64 units (1e8 scale)
//       "p:{account_id}:{INSTR}"    -> net position units, signed int64
//       "i:{instrument_id}"         -> "BASE/QUOTE" currency pair
//       "c:{account_id}"            -> credit party index (spec §3.3b;
//                                      absent = unscreened anonymous flow)
//       "__hb__"                    -> publisher unix-seconds heartbeat
//
// A control-path AccountStateRefresher polls that hash via RespClient on
// its own thread (the matching thread NEVER touches Redis — same
// discipline as SuspensionRefresher/SanctionsRefresher) and swaps a
// Snapshot into this object. PreTradeChecker consults it on every order
// admission: shared_ptr load + hash lookups, zero heap on the hot path.
//
// Fail closed (spec §2.7 strict fail-closed pessimism):
//   * no snapshot ever applied                         -> UNKNOWN / -1 / 0
//   * last refresh errored (mark_unverifiable)         -> UNKNOWN / -1 / 0
//   * publisher heartbeat older than the TTL           -> unverifiable
//     (evaluated on the CONTROL thread against wall time — the matching
//     thread never reads a clock; same unverifiable-bit discipline as
//     SuspensionFlags)
//   * account id absent from the snapshot              -> UNKNOWN / -1
//   * instrument absent (no "i:" row)                  -> -1 balance
//   * balance field absent for (account, ccy)          -> -1
//   * position field absent for (account, instr)       -> 0 (flat)
//
// Check-by-check behavior when the bound-but-unverified cache is
// consulted: check 1 sees UNKNOWN -> ACCOUNT_INACTIVE; check 3 sees -1
// -> INSUFFICIENT_BALANCE; check 4 sees UINT32_MAX -> POSITION_LIMIT_
// EXCEEDED; check 8 sees -1 -> MARGIN_CHECK_FAILED; check 10 sees T0;
// check 13 sees pos 0 -> REDUCE_ONLY_VIOLATION; check 14 sees RETAIL
// (STP NONE denied by default). An order can therefore never slip past
// an unreadable account store.

#include <atomic>
#include <cstdint>
#include <memory>
#include <shared_mutex>
#include <string_view>
#include <unordered_map>
#include <utility>

#include "risk/BilateralCreditMatrix.h"  // kCreditMaxParties
#include "risk/PreTradeChecker.hpp"      // kStpModeUnset
#include "risk/RiskInterfaces.hpp"

namespace exch {

class AccountStateCache final : public IAccountState, public IPositionState,
                                public IPartyMap {
public:
    struct Snapshot {
        bool unverifiable = false;      // last refresh failed -> fail closed
        uint64_t heartbeat_unix = 0;    // publisher __hb__ field (seconds)
        uint64_t applied_unix = 0;      // wall seconds at apply (ops only)

        struct Rec {
            AccountStatus status = AccountStatus::UNKNOWN;
            KycTier kyc = KycTier::T0;
            ClientCategory category = ClientCategory::RETAIL;
            uint8_t stp = kStpModeUnset;
            uint32_t open_positions = 0;
            // Bilateral-credit matrix party index (spec §3.3b).
            // >= kCreditMaxParties means the account is not credit-
            // screened — the consume_or_skip gate skips the pair.
            uint32_t credit_party = kCreditMaxParties;
        };
        std::unordered_map<uint64_t, Rec> accounts;
        // Packed availability: key = (account_id << 24) | ccy24 where ccy24
        // is the 3-byte ISO code (e.g. 'U','S','D'). Value = 1e8 units.
        std::unordered_map<uint64_t, int64_t> avail;
        // Packed net position: key = (account_id << 32) | instrument_id.
        std::unordered_map<uint64_t, int64_t> pos;
        // instrument_id -> (base_ccy24, quote_ccy24).
        std::unordered_map<uint64_t, std::pair<uint32_t, uint32_t>> instr;
    };

    // Publisher heartbeat TTL — the refresher refuses snapshots whose
    // __hb__ is older than this (wall seconds, control-thread check).
    // Default 30s against a ~1s publish cadence.
    static constexpr uint64_t kDefaultMaxHeartbeatAgeS = 30;

    AccountStateCache() = default;

    // Control path — publish a freshly polled snapshot / poison the cache.
    void apply(std::shared_ptr<const Snapshot> snap) noexcept;
    void mark_unverifiable() noexcept;

    [[nodiscard]] bool ready() const noexcept {
        return ready_.load(std::memory_order_acquire);
    }

    // --- IAccountState (hot path, zero heap, zero clock reads) -----------
    [[nodiscard]] AccountStatus status(uint64_t account_id) const
        noexcept override;
    [[nodiscard]] int64_t available_balance(uint64_t account_id,
                                            uint64_t instrument_id,
                                            BalanceUnit unit) const
        noexcept override;
    [[nodiscard]] uint32_t open_position_count(uint64_t account_id) const
        noexcept override;
    [[nodiscard]] StpMode default_stp_mode(uint64_t account_id) const
        noexcept override;
    [[nodiscard]] ClientCategory client_category(uint64_t account_id) const
        noexcept override;
    [[nodiscard]] KycTier kyc_tier(uint64_t account_id) const
        noexcept override;

    // --- IPositionState --------------------------------------------------
    // Flat when the snapshot is unverifiable or the (account, instrument)
    // pair is absent — reduce-only fails closed on flat (check 13).
    [[nodiscard]] int64_t net_position_units(
        uint64_t account_id, uint64_t instrument_id) const noexcept override;

    // --- IPartyMap (spec §3.3b credit screen) ------------------------------
    // account_id -> bilateral-credit party index, or kCreditMaxParties
    // when the account is unscreened (absent record or no c: field) — the
    // walk's consume_or_skip gate then leaves the pair unscreened.
    [[nodiscard]] uint32_t credit_party_id(uint64_t account_id) const
        noexcept override;

private:
    [[nodiscard]] std::shared_ptr<const Snapshot> load() const noexcept;
    // Verified account record or nullptr (unverifiable/absent). Callers
    // fold the nullptr into their fail-closed sentinel.
    [[nodiscard]] const Snapshot::Rec* rec(uint64_t account_id) const
        noexcept;

    mutable std::shared_mutex mu_;
    std::shared_ptr<const Snapshot> snap_{};
    std::atomic<bool> ready_{false};
};

// Parse one `account:state` hash field into the Snapshot. Field grammar:
//   "{digits}"            -> account record "STATUS,KYC,CAT,STP,OPENPOS"
//   "a:{digits}:{CCC}"    -> availability int64 units
//   "p:{digits}:{digits}" -> net position int64 units
//   "i:{digits}"          -> "BASE/QUOTE"
//   "c:{digits}"          -> credit party index (uint32, <1024 screened)
//   "__hb__"              -> publisher heartbeat unix seconds
// Returns false for foreign/malformed fields (ignored, never poison the
// snapshot); a transport failure is reported by the refresher, not here.
[[nodiscard]] bool account_field_into(std::string_view field,
                                      std::string_view value,
                                      AccountStateCache::Snapshot* out) noexcept;

class RespClient;

// AccountStateRefresher is the control-path poller — one thread polls
// `HGETALL account:state` via RespClient on its own cadence (main.cpp
// wires ~1s, `-accounts-poll-ms`) and swaps the Snapshot into the cache.
// Any transport failure, a missing `__hb__`, or a heartbeat older than
// max_hb_age_s marks the cache unverifiable — admission fails closed
// until a clean poll lands.
class AccountStateRefresher {
public:
    // Non-owning: client may be nullptr (refresh then always marks
    // unverifiable — bound-but-unwired still fails closed).
    // now_unix_fn is injectable for tests; default reads wall time.
    explicit AccountStateRefresher(
        RespClient* client,
        uint64_t max_hb_age_s = AccountStateCache::kDefaultMaxHeartbeatAgeS,
        uint64_t (*now_unix_fn)() = nullptr) noexcept;
    bool refresh(AccountStateCache* cache) noexcept;

private:
    RespClient* client_;
    uint64_t max_hb_age_s_;
    uint64_t (*now_unix_fn_)();
};

}  // namespace exch
