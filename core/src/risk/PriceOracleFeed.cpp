// Phase-16 Tasks 16.3.17/16.3.22 — PriceOracleFeed: off-thread Redis poll
// of the mark/index oracle keys into the immutable snapshot the matching
// thread reads for MARK_PRICE/INDEX_PRICE conditional triggers
// (spec §6.2a, §19.5 staleness gate). See the header for the key contract.

#include "risk/PriceOracleFeed.hpp"

#include <cerrno>
#include <cstdlib>
#include <limits>
#include <string>
#include <vector>

#include "redis/RespClient.hpp"

namespace exch {

// "{scaled_int64}" — the publisher writes the 1e8-scaled tick value (or the
// unix-ns stamp) as a plain decimal integer. Rejects empty, non-numeric,
// signed-overflowing or trailing-garbage input — anything unclean fails the
// whole poll round (fail closed, never a partial parse).
bool parse_oracle_price(std::string_view value, int64_t* ticks) noexcept {
    if (ticks == nullptr) return false;
    *ticks = 0;
    if (value.empty() || value.size() > 20) return false;
    // Optional leading '-'; digits only otherwise.
    std::size_t i = 0;
    const bool neg = value[0] == '-';
    if (neg) i = 1;
    if (i == value.size()) return false;
    int64_t v = 0;
    for (; i < value.size(); ++i) {
        const char c = value[i];
        if (c < '0' || c > '9') return false;
        const int digit = c - '0';
        if (v > (std::numeric_limits<int64_t>::max() - digit) / 10) {
            return false;  // overflow — fail closed
        }
        v = v * 10 + digit;
    }
    *ticks = neg ? -v : v;
    return true;
}

PriceOracleFeedRefresher::PriceOracleFeedRefresher(
    RespClient* client, std::string_view symbol, std::string* storage) noexcept
    : client_(client) {
    // `storage` is the caller-owned backing for the (possibly transient)
    // symbol view — the refresher builds its key strings from it.
    const std::string sym =
        storage != nullptr ? (*storage = std::string(symbol))
                           : std::string(symbol);
    mark_key_ = "oracle:mark:" + sym;
    mark_ts_key_ = mark_key_ + ":ts";
    index_key_ = "oracle:index:" + sym;
    index_ts_key_ = index_key_ + ":ts";
}

// One poll round:
//   MGET oracle:mark:{sym} oracle:mark:{sym}:ts
//        oracle:index:{sym} oracle:index:{sym}:ts
// Missing keys mark that SOURCE absent (legal); a transport failure or an
// unparseable present value marks the whole feed unverifiable — consumers
// freeze every oracle-sourced conditional until a clean poll lands.
bool PriceOracleFeedRefresher::refresh(PriceOracleFeed* feed) noexcept {
    if (feed == nullptr) return false;
    if (client_ == nullptr || !client_->connected()) {
        feed->mark_unverifiable();
        return false;
    }

    const std::vector<std::string_view> keys = {
        mark_key_, mark_ts_key_, index_key_, index_ts_key_};
    RespValue reply;
    if (!client_->mget(keys, &reply) ||
        reply.type != RespValue::Type::Array ||
        reply.items.size() != 4) {
        feed->mark_unverifiable();
        return false;
    }

    PriceOracleFeed::Snapshot s{};
    // Per-slot decode: nil = source absent (allowed); a present value that
    // fails to parse is a publisher/protocol fault — fail closed.
    auto slot = [](const RespValue& v, int64_t* out) noexcept -> int {
        std::string_view sv;
        if (v.is_nil()) { *out = 0; return 0; }
        if (!v.as_string(&sv)) return -1;
        if (!parse_oracle_price(sv, out)) return -1;
        return 1;
    };
    if (slot(reply.items[0], &s.mark_ticks) < 0 ||
        slot(reply.items[1], &s.mark_ts_ns) < 0 ||
        slot(reply.items[2], &s.index_ticks) < 0 ||
        slot(reply.items[3], &s.index_ts_ns) < 0) {
        feed->mark_unverifiable();
        return false;
    }
    // A price without its stamp (or vice versa) cannot pass the staleness
    // gate — keep the pair coherent by zeroing a half-published source.
    if (s.mark_ticks == 0) s.mark_ts_ns = 0;
    if (s.index_ticks == 0) s.index_ts_ns = 0;

    s.verifiable = true;
    feed->apply(s);
    return true;
}

}  // namespace exch
