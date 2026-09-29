// Task 2.3.2 — WalWriter: packed-payload encoders (spec §3.4).
// One append per state change; WAL-before-mutate ordering is owned by the
// MatchingEngine call sites.

#include "matching/WalWriter.hpp"

#include <cstring>

namespace exch {

WalStatus WalWriter::append(WalEventType type, const void* payload,
                            uint32_t payload_len, uint64_t ts_ns) noexcept {
    ++appends_;
    if (wal_ == nullptr) return WalStatus::NotOpen;
    // Explicit-seq form: stamps the engine's deterministic logical clock into
    // the entry header instead of wall-clock now_ns(); seq = tail_seq() is
    // identical to what the auto-seq overload would assign.
    const WalStatus s =
        wal_->append(wal_->tail_seq(), ts_ns, type, payload, payload_len);
    if (s != WalStatus::Ok) ++failures_;
    return s;
}

WalStatus WalWriter::write_order_new(const Order& o, const OrderAux& aux,
                                     uint32_t instrument_id,
                                     int64_t visible_qty_units,
                                     uint64_t ts_ns) noexcept {
    WalOrderNewPayload p{};
    p.order_id = o.id;
    p.account_id = o.account_id;
    p.instrument_id = instrument_id;
    p.side = wal_side_wire(o.side);
    p.type = wal_order_type_wire(o.type);
    p.tif = static_cast<uint8_t>(o.tif);
    p.flags = o.flags;
    p.price_ticks = o.price_ticks;
    p.qty_units = o.qty_units;
    p.visible_qty_units = visible_qty_units;
    p.stop_price_ticks = aux.stop_price_ticks;
    p.stp_mode = static_cast<uint32_t>(o.stp_mode);
    p.trade_group_id = aux.trade_group_id;
    p.gtd_expiry_ns = aux.gtd_expiry_ns;
    return append(WalEventType::ORDER_NEW, &p, sizeof(p), ts_ns);
}

WalStatus WalWriter::write_order_cancel(uint64_t order_id, uint64_t account_id,
                                        uint8_t reason,
                                        uint64_t ts_ns) noexcept {
    WalOrderCancelPayload p{};
    p.order_id = order_id;
    p.account_id = account_id;
    p.reason = reason;
    return append(WalEventType::ORDER_CANCEL, &p, sizeof(p), ts_ns);
}

WalStatus WalWriter::write_order_modify(uint64_t order_id,
                                        int64_t new_price_ticks,
                                        int64_t new_qty_units,
                                        int64_t new_stop_price_ticks,
                                        uint64_t ts_ns) noexcept {
    WalOrderModifyPayload p{};
    p.order_id = order_id;
    p.new_price_ticks = new_price_ticks;
    p.new_qty_units = new_qty_units;
    p.new_stop_price_ticks = new_stop_price_ticks;
    return append(WalEventType::ORDER_MODIFY, &p, sizeof(p), ts_ns);
}

WalStatus WalWriter::write_trade(uint64_t trade_id, uint64_t buy_order_id,
                                 uint64_t sell_order_id, uint32_t instrument_id,
                                 int64_t price_ticks, int64_t qty_units,
                                 uint64_t ts_ns) noexcept {
    WalTradePayload p{};
    p.trade_id = trade_id;
    p.buy_order_id = buy_order_id;
    p.sell_order_id = sell_order_id;
    p.instrument_id = instrument_id;
    p.price_ticks = price_ticks;
    p.qty_units = qty_units;
    return append(WalEventType::TRADE, &p, sizeof(p), ts_ns);
}

WalStatus WalWriter::write_time_tick(uint64_t tick_ns) noexcept {
    WalTimeTickPayload p{};
    p.tick_ns = tick_ns;
    return append(WalEventType::TIME_TICK, &p, sizeof(p), tick_ns);
}

WalStatus WalWriter::write_prevented_match(
    const WalPreventedMatchPayload& p, uint64_t ts_ns) noexcept {
    return append(WalEventType::PREVENTED_MATCH, &p, sizeof(p), ts_ns);
}

WalStatus WalWriter::write_oco_link(
    const WalOcoLinkPayload& p, uint64_t ts_ns) noexcept {
    return append(WalEventType::OCO_LINK, &p, sizeof(p), ts_ns);
}

WalStatus WalWriter::write_auction_phase(
    const WalAuctionPhasePayload& p, uint64_t ts_ns) noexcept {
    return append(WalEventType::AUCTION_PHASE, &p, sizeof(p), ts_ns);
}

WalStatus WalWriter::flush() noexcept {
    if (wal_ == nullptr) return WalStatus::NotOpen;
    return wal_->flush();
}

}  // namespace exch
