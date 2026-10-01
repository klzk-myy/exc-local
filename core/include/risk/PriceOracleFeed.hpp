#pragma once

// Phase-16 Tasks 16.3.17/16.3.22 — mark/index price oracle feed: the
// single immutable snapshot the matching thread reads for
// MARK_PRICE / INDEX_PRICE conditional triggers (spec §6.2a, §19.5).
//
// Redis contract (Go publication side — services/internal/marketdata or the
// oracle bridge owned by Phase-19.5; the keys are additive, single-writer
// per field, republished on every tick):
//
//   oracle:mark:{symbol}     decimal string, price scale 1e8 — the venue's
//                            mark price for the instrument.
//   oracle:mark:{sym}:ts     unix-nanoseconds stamp of the mark value —
//                            the engine's staleness reference.
//   oracle:index:{symbol}    decimal string, price scale 1e8 — the
//                            composite index price.
//   oracle:index:{sym}:ts    unix-nanoseconds stamp of the index value.
//
// Missing keys are legal (an index-less instrument simply publishes no
// index row) — they mark that source unavailable, NOT the feed
// unverifiable. The feed goes unverifiable only on a transport failure or
// an unparseable value (fail-closed: consumers freeze, never guess).
//
// Fail-closed discipline is identical to InstrumentFeed: the matching
// thread NEVER touches Redis; PriceOracleFeedRefresher polls on the
// control thread and atomically replaces the Snapshot. The engine
// additionally applies the 5s staleness gate at evaluation time
// (MatchingEngine::kOracleStaleNs) using the per-source timestamps — a
// fresh-but-absent source and a stale source are indistinguishable to the
// trigger queue: both evaluate to reference 0, which cannot fire.

#include <cstdint>
#include <mutex>
#include <shared_mutex>
#include <string>
#include <string_view>

namespace exch {

class PriceOracleFeed {
public:
    // Immutable per-poll snapshot (POD — copied under a brief shared lock).
    struct Snapshot {
        bool    verifiable = false;   // last poll round parsed end-to-end
        int64_t mark_ticks = 0;       // 0 = source absent
        int64_t mark_ts_ns = 0;       // publisher wall stamp; 0 = absent
        int64_t index_ticks = 0;      // 0 = source absent
        int64_t index_ts_ns = 0;
    };

    PriceOracleFeed() noexcept = default;

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

private:
    mutable std::shared_mutex mu_;
    Snapshot snap_{};
};

// --- control-plane parser (refresher side; pure function) -------------------
// "{scaled_int64}" — the publisher writes the 1e8-scaled tick value as a
// plain decimal integer (same convention as instrument keys). Returns
// false on anything that is not a clean integer in int64 range.
[[nodiscard]] bool parse_oracle_price(std::string_view value,
                                      int64_t* ticks) noexcept;

class RespClient;

// Control-thread poller — one refresh() round:
//   MGET oracle:mark:{sym}  oracle:mark:{sym}:ts
//        oracle:index:{sym} oracle:index:{sym}:ts
// Any transport/parse failure marks the feed unverifiable.
class PriceOracleFeedRefresher {
public:
    PriceOracleFeedRefresher(RespClient* client, std::string_view symbol,
                             std::string* storage) noexcept;
    [[nodiscard]] bool refresh(PriceOracleFeed* feed) noexcept;

private:
    RespClient* client_;
    std::string mark_key_;
    std::string mark_ts_key_;
    std::string index_key_;
    std::string index_ts_key_;
};

}  // namespace exch
