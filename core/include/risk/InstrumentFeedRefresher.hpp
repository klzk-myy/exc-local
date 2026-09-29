#pragma once

// Phase-15 Tasks 15.3.3/15.3.4/15.3.6 — control-thread Redis poller for the
// InstrumentFeed contract (see InstrumentFeed.hpp for the key layout).
//
// One refresh() round:
//   MGET instrument:status:{sym}  instrument:auction:{sym}
//        instrument:auction:{sym}:result  market:hours
// then, on the same connection:
//   * a new armed auction (deadline differs from the last result the engine
//     published) DELs a stale :result key so the Go ladder can't read a
//     prior CLEARED as this auction's outcome;
//   * a pending engine result (request_auction_result) SETs :result and —
//     for CLEARED — conditional-DELs the armed key via Lua so a re-armed
//     CALL/EXTEND value is never deleted by a stale release.
//
// Every step runs on the control thread only. Any transport/parse failure
// marks the feed unverifiable — consumers fail closed until a clean poll.

#include <string>
#include <string_view>
#include <vector>

#include "redis/RespClient.hpp"
#include "risk/InstrumentFeed.hpp"

namespace exch {

class InstrumentFeedRefresher {
public:
    InstrumentFeedRefresher(RespClient* client, std::string symbol) noexcept
        : client_(client), symbol_(std::move(symbol)) {
        status_key_  = "instrument:status:" + symbol_;
        auction_key_ = "instrument:auction:" + symbol_;
        result_key_  = auction_key_ + ":result";
    }

    // One poll round. Returns true when the snapshot applied cleanly.
    // Requires an already-connected client (matching SuspensionRefresher).
    [[nodiscard]] bool refresh(InstrumentFeed* feed) noexcept;

private:
    // Publishes the engine's pending auction result (SET :result; on
    // CLEARED also conditional-DEL the armed key).
    void flush_auction_result(InstrumentFeed* feed) noexcept;

    RespClient* client_;
    std::string symbol_;
    std::string status_key_;
    std::string auction_key_;
    std::string result_key_;
    int64_t     last_result_deadline_ = 0;  // auction_id of last published result
    bool        result_seen_ = false;       // :result key existed in last poll
};

}  // namespace exch
