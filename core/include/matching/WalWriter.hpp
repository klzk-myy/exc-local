#pragma once

// Task 2.3.2 — matching-engine WAL encoder (spec §3.4, §3.7).
//
// Thin wrapper over Wal (core/include/wal/Wal.hpp): every engine state
// change is serialized into one of the PINNED packed payloads in
// core/include/wal/WalEntry.hpp and appended. Encoding field order is the
// encoder<->RecoveryManager contract — this is the only place the mapping
// from engine types to wire/payload values lives:
//
//   WalOrderNewPayload.side         wire::Side          (BUY=0, SELL=1 — identical)
//   WalOrderNewPayload.type         wire::OrderType     (Market=0, Limit=1,
//                                   StopMarket=2, StopLimit=3) with the
//                                   engine-only extension kWalOrderTypeIceberg=4
//                                   (internal ICEBERG — outside the fbs enum
//                                   range, unambiguous on replay).
//   WalOrderNewPayload.tif          wire::TimeInForce   (identical ordering)
//   WalOrderNewPayload.stp_mode     internal StpMode    (0=CANCEL_NEWEST ...)
//   WalOrderCancelPayload.reason    kWalCancelReason*   (0 user / 1 expired /
//                                   2 STP / 3 FOK_unfilled / 4 IOC_remainder)
//
// The optional timestamp argument lets the engine stamp its deterministic
// logical clock (TIME_TICK-driven) into the entry header; the WAL keeps
// assigning the entry seq itself (single-writer, tail_seq()).
//
// All methods are noexcept and allocation-free; a null Wal* degrades every
// call to WalStatus::NotOpen so unit tests can run the engine journal-free.

#include <cstdint>

#include "book/Order.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

namespace exch {

// Engine-side aux fields that have no slot on the POD Order node but must
// reach the WAL record (stop trigger price, GTD/DAY expiry, STP group).
// Populated by ingress (EngineLoop/gateway decode) next to the Order.
struct OrderAux {
    int64_t  stop_price_ticks = 0;  // >0 for STOP / STOP_LIMIT orders
    int64_t  gtd_expiry_ns    = 0;  // TIF=GTD/DAY absolute expiry (gateway
                                    // computes DAY session close; 0 = none)
    uint32_t trade_group_id   = 0;  // migration 072 STP group; 0 = none
    uint32_t instrument_id    = 0;  // WAL stamping when book has no instrument
    int64_t  discretionary_offset_pips = 0;  // Task 2.3.26 (§6.11, migration
                                           // 103); whole pips, 0 = plain limit
};

// Cancel/terminal reasons — values of WalOrderCancelPayload.reason.
inline constexpr uint8_t kWalCancelReasonUser        = 0;
inline constexpr uint8_t kWalCancelReasonExpired     = 1;  // GTD/DAY expiry
inline constexpr uint8_t kWalCancelReasonStp         = 2;
inline constexpr uint8_t kWalCancelReasonFokUnfilled = 3;
inline constexpr uint8_t kWalCancelReasonIocRemainder = 4;  // also MARKET rest

// Internal OrderType::ICEBERG marker for WalOrderNewPayload.type — the
// wire::OrderType enum tops out at StopLimit=3.
inline constexpr uint8_t kWalOrderTypeIceberg = 4;

// internal Side -> wire::Side (identity today; one mapping site).
[[nodiscard]] constexpr uint8_t wal_side_wire(Side s) noexcept {
    return s == Side::BUY ? 0u : 1u;
}
// internal OrderType -> payload value (wire::OrderType + iceberg ext).
[[nodiscard]] constexpr uint8_t wal_order_type_wire(OrderType t) noexcept {
    switch (t) {
        case OrderType::MARKET:     return 0;  // wire::OrderType_Market
        case OrderType::LIMIT:      return 1;  // wire::OrderType_Limit
        case OrderType::STOP:       return 2;  // wire::OrderType_StopMarket
        case OrderType::STOP_LIMIT: return 3;  // wire::OrderType_StopLimit
        case OrderType::ICEBERG:    return kWalOrderTypeIceberg;
        default:                    return 0xFF;  // not encodable (rejected upstream)
    }
}

class WalWriter {
public:
    // wal may be nullptr — every write then returns WalStatus::NotOpen and
    // the caller (MatchingEngine) runs journal-free (unit-test mode).
    explicit WalWriter(Wal* wal) noexcept : wal_(wal) {}

    WalWriter(const WalWriter&) = delete;
    WalWriter& operator=(const WalWriter&) = delete;

    // One append per state change. `ts_ns` is the engine logical clock
    // (0/stale in tests is fine — deterministic either way).
    [[nodiscard]] WalStatus write_order_new(const Order& o,
                                            const OrderAux& aux,
                                            uint32_t instrument_id,
                                            int64_t visible_qty_units,
                                            uint64_t ts_ns) noexcept;
    [[nodiscard]] WalStatus write_order_cancel(uint64_t order_id,
                                               uint64_t account_id,
                                               uint8_t reason,
                                               uint64_t ts_ns) noexcept;
    [[nodiscard]] WalStatus write_order_modify(uint64_t order_id,
                                               int64_t new_price_ticks,
                                               int64_t new_qty_units,
                                               int64_t new_stop_price_ticks,
                                               uint64_t ts_ns) noexcept;
    [[nodiscard]] WalStatus write_trade(uint64_t trade_id,
                                        uint64_t buy_order_id,
                                        uint64_t sell_order_id,
                                        uint32_t instrument_id,
                                        int64_t price_ticks,
                                        int64_t qty_units,
                                        uint64_t ts_ns) noexcept;
    [[nodiscard]] WalStatus write_time_tick(uint64_t tick_ns) noexcept;
    // Task 2.3.18 — immutable STP prevented-match audit record (spec §6.5,
    // §24 #279-280). Appended by the matching engine for mutually-requested
    // TRANSFER prevention across accounts inside one trade_group_id; the
    // Phase-03 GL service consumes it for the balanced ledger posting.
    [[nodiscard]] WalStatus write_prevented_match(
        const WalPreventedMatchPayload& p, uint64_t ts_ns) noexcept;

    // Durability barrier passthrough (batch fsync).
    [[nodiscard]] WalStatus flush() noexcept;

    [[nodiscard]] bool bound() const noexcept { return wal_ != nullptr; }
    [[nodiscard]] uint64_t tail_seq() const noexcept {
        return wal_ != nullptr ? wal_->tail_seq() : 0;
    }
    // Attempted/failed append counters (test + metrics surface).
    [[nodiscard]] uint64_t appends() const noexcept { return appends_; }
    [[nodiscard]] uint64_t failures() const noexcept { return failures_; }

private:
    [[nodiscard]] WalStatus append(WalEventType type, const void* payload,
                                   uint32_t payload_len,
                                   uint64_t ts_ns) noexcept;

    Wal* wal_;
    uint64_t appends_ = 0;
    uint64_t failures_ = 0;
};

}  // namespace exch
