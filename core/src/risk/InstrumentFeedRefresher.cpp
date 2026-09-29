// Phase-15 — InstrumentFeedRefresher: off-thread Redis poll of the
// instrument lifecycle / market-hours / auction control keys into the
// immutable InstrumentFeed snapshot. See the header for the contract.

#include "risk/InstrumentFeedRefresher.hpp"

#include <string>
#include <vector>

namespace exch {

namespace {

// Conditional release of a consumed auction key: delete only when the
// armed value still equals the value the engine consumed (CALL:<id> or
// EXTEND:<id> — the Go ladder rewrites the prefix on extensions).
constexpr char kCondDelScript[] =
    "local v=redis.call('GET',KEYS[1]) "
    "if v==ARGV[1] or v==ARGV[2] then return redis.call('DEL',KEYS[1]) end "
    "return 0";

}  // namespace

bool InstrumentFeedRefresher::refresh(InstrumentFeed* feed) noexcept {
    if (client_ == nullptr || feed == nullptr || !client_->connected()) {
        if (feed != nullptr) feed->mark_unverifiable();
        return false;
    }

    const std::vector<std::string_view> keys = {
        status_key_, auction_key_, result_key_, "market:hours"};
    RespValue reply;
    if (!client_->mget(keys, &reply) ||
        reply.type != RespValue::Type::Array ||
        reply.items.size() != 4) {
        feed->mark_unverifiable();
        return false;
    }

    InstrumentFeed::Snapshot s{};

    // --- instrument:status:{symbol} — plain enum word; absent => DRAFT ---
    std::string_view word;
    if (reply.items[0].as_string(&word)) {
        if (!parse_instrument_status(word, &s.status)) {
            // Unknown status word — fail closed (never defaults to ACTIVE).
            s.status = InstrumentStatus::DRAFT;
        }
    } else if (!reply.items[0].is_nil()) {
        feed->mark_unverifiable();  // malformed reply shape
        return false;
    }

    // --- instrument:auction:{symbol} — "CALL:{ns}" | "EXTEND:{ns}" -------
    std::string_view aval;
    if (reply.items[1].as_string(&aval)) {
        if (!parse_auction_call(aval, &s.auction_deadline_ns)) {
            // Armed key with an unparseable value is a publisher bug — a
            // would-be CALL we can't read must not leave trading open.
            feed->mark_unverifiable();
            return false;
        }
        s.auction_armed = true;
    } else if (!reply.items[1].is_nil()) {
        feed->mark_unverifiable();
        return false;
    }

    // --- instrument:auction:{sym}:result — liveness tracking -------------
    result_seen_ = !reply.items[2].is_nil();

    // --- market:hours — JSON projection; absent => market unknown --------
    std::string_view mh;
    if (reply.items[3].as_string(&mh)) {
        if (!parse_market_hours_json(mh, &s.market)) {
            feed->mark_unverifiable();
            return false;
        }
        s.market_known = true;
    } else if (!reply.items[3].is_nil()) {
        feed->mark_unverifiable();
        return false;
    }

    s.verifiable = true;
    feed->apply(s);

    // --- return path: stale :result eviction + engine result flush -------
    if (s.auction_armed && result_seen_ &&
        s.auction_deadline_ns != last_result_deadline_) {
        // A new armed auction must not inherit the previous auction's
        // CLEARED/FAILED marker — evict it on sight.
        RespValue r;
        if (client_->execute({"DEL", result_key_}, &r) && !r.is_error()) {
            result_seen_ = false;
        }
    }
    flush_auction_result(feed);
    return true;
}

void InstrumentFeedRefresher::flush_auction_result(
    InstrumentFeed* feed) noexcept {
    const int64_t pending = feed->auction_result_pending();
    if (pending == 0) return;
    const bool cleared = feed->auction_result_cleared();

    RespValue r;
    if (!client_->execute({"SET", result_key_,
                           cleared ? "CLEARED" : "FAILED"},
                          &r) ||
        r.is_error()) {
        return;  // keep the request pending — retried on the next poll
    }
    result_seen_ = true;
    last_result_deadline_ = pending;

    if (cleared) {
        // Release only the consumed armed value — a re-armed auction
        // (different deadline) must survive this DEL.
        char call_val[32];
        char ext_val[32];
        std::snprintf(call_val, sizeof(call_val), "CALL:%lld",
                      (long long)pending);
        std::snprintf(ext_val, sizeof(ext_val), "EXTEND:%lld",
                      (long long)pending);
        const std::vector<std::string_view> args = {
            auction_key_, call_val, ext_val};
        RespValue del_r;
        // Best-effort: failure leaves the key for the Go reconciler; the
        // engine dedupes the consumed auction_id so no re-entry occurs.
        (void)client_->eval(kCondDelScript, 1, args, &del_r);
    }
    feed->clear_auction_result_request();
}

}  // namespace exch
