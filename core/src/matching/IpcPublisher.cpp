// Task 2.3.2 — IpcPublisher: FlatBuffers Event envelopes over IpcChannel.
// Without generated flatbuffers headers the component compiles to a
// drop-everything stub (published/drops counters still move) — the build
// always provides them when flatc + runtime headers are present.

#include "matching/IpcPublisher.hpp"

#include "ipc/IpcChannel.hpp"

namespace exch {

#ifdef EXCH_IPC_FLATBUFFERS

bool IpcPublisher::emit(flatbuffers::Offset<exc::wire::Event> ev) noexcept {
    builder_.Finish(ev);
    const bool ok = out_ != nullptr &&
                    out_->send(builder_.GetBufferPointer(),
                               static_cast<uint32_t>(builder_.GetSize()));
    builder_.Reset();  // keeps capacity — no reallocation
    if (ok) {
        ++published_;
        ++pub_seq_;
    } else {
        ++drops_;
    }
    return ok;
}

bool IpcPublisher::publish_trade(uint64_t trade_id, uint64_t buy_order_id,
                                 uint64_t sell_order_id, int64_t price_ticks,
                                 int64_t qty_units, uint64_t engine_seq,
                                 uint64_t ts_ns) noexcept {
    namespace w = exc::wire;
    const auto fill =
        w::CreateTradeFill(builder_, trade_id, buy_order_id, sell_order_id,
                           price_ticks, qty_units, engine_seq);
    const auto ev =
        w::CreateEvent(builder_, pub_seq_, ts_ns, w::EventType_TradeFill,
                       fill.Union());
    return emit(ev);
}

bool IpcPublisher::publish_order_cancel(uint64_t order_id,
                                        uint64_t account_id,
                                        uint64_t ts_ns) noexcept {
    namespace w = exc::wire;
    const auto cx = w::CreateOrderCancel(builder_, order_id, account_id);
    const auto ev = w::CreateEvent(builder_, pub_seq_, ts_ns,
                                   w::EventType_OrderCancel, cx.Union());
    return emit(ev);
}

bool IpcPublisher::publish_book_snapshot(const OrderBook& book,
                                         uint32_t instrument_id,
                                         uint64_t ts_ns) noexcept {
    namespace w = exc::wire;
    const uint32_t nb = book.bid_count();
    const uint32_t na = book.ask_count();
    for (uint32_t i = 0; i < nb; ++i) {
        const PriceLevel* l = book.level(Side::BUY, i);
        level_off_[i] = w::CreatePriceLevel(builder_, l->price_ticks,
                                            l->total_qty_units,
                                            l->order_count);
    }
    for (uint32_t i = 0; i < na; ++i) {
        const PriceLevel* l = book.level(Side::SELL, i);
        level_off_[OrderBook::kMaxLevels + i] =
            w::CreatePriceLevel(builder_, l->price_ticks, l->total_qty_units,
                                l->order_count);
    }
    const auto bids =
        builder_.CreateVector(level_off_, static_cast<size_t>(nb));
    const auto asks = builder_.CreateVector(level_off_ + OrderBook::kMaxLevels,
                                            static_cast<size_t>(na));
    const auto snap = w::CreateBookSnapshot(builder_, instrument_id,
                                            book.book_seq(), bids, asks);
    const auto ev = w::CreateEvent(builder_, pub_seq_, ts_ns,
                                   w::EventType_BookSnapshot, snap.Union());
    return emit(ev);
}

#else  // !EXCH_IPC_FLATBUFFERS — degraded stub

bool IpcPublisher::publish_trade(uint64_t, uint64_t, uint64_t, int64_t,
                                 int64_t, uint64_t, uint64_t) noexcept {
    ++drops_;
    return false;
}
bool IpcPublisher::publish_order_cancel(uint64_t, uint64_t,
                                        uint64_t) noexcept {
    ++drops_;
    return false;
}
bool IpcPublisher::publish_book_snapshot(const OrderBook&, uint32_t,
                                         uint64_t) noexcept {
    ++drops_;
    return false;
}

#endif

}  // namespace exch
