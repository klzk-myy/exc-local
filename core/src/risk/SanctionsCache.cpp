// Phase-21 Task 21.3.10 — SanctionsCache implementation + control-path
// SanctionsRefresher. See include/risk/SanctionsCache.hpp for the
// contract; mirrors SuspensionFlags/SuspensionRefresher discipline
// (control-thread Redis poll, immutable shared_ptr snapshot, zero
// heap on the check path).

#include "risk/SanctionsCache.hpp"

#include <cstdlib>

#include "redis/RespClient.hpp"
#include "utils/TimeUtils.hpp"

namespace exch {

namespace {

// Numeric-id suffix parse — same shape as suspension_key_into's target
// parse: digits only, no sign/overflow.
[[nodiscard]] bool parse_u64(std::string_view s, uint64_t* out) noexcept {
    if (s.empty() || s.size() > 20) return false;
    uint64_t v = 0;
    for (const char c : s) {
        if (c < '0' || c > '9') return false;
        const uint64_t d = static_cast<uint64_t>(c - '0');
        if (v > (UINT64_MAX - d) / 10) return false;
        v = v * 10 + d;
    }
    *out = v;
    return true;
}

inline constexpr std::string_view kFlagPrefix = "exc:sanctions:flagged:";
inline constexpr std::string_view kClearedPrefix =
    "exc:sanctions:flagged:cleared:";

}  // namespace

bool sanctions_key_into(std::string_view key,
                        SanctionsCache::Snapshot* out) noexcept {
    if (out == nullptr) return false;
    if (key.size() <= kFlagPrefix.size() ||
        key.compare(0, kFlagPrefix.size(), kFlagPrefix) != 0) {
        return false;
    }
    const std::string_view rest = key.substr(kFlagPrefix.size());
    uint64_t id = 0;
    if (rest.size() > 8 && rest.compare(0, 8, "cleared:") == 0) {
        if (!parse_u64(rest.substr(8), &id)) return false;
        out->cleared.insert(id);
        return true;
    }
    if (!parse_u64(rest, &id)) return false;
    out->flagged.insert(id);
    return true;
}

SanctionsCache::Verdict SanctionsCache::verdict_for(
    uint64_t account_id, uint64_t now_ns) const noexcept {
    std::shared_ptr<const Snapshot> snap;
    {
        std::shared_lock<std::shared_mutex> lk(mu_);
        snap = snap_;
    }
    if (snap == nullptr || snap->unverifiable) {
        return Verdict::Unverifiable;
    }
    // Freshness: heartbeat must exist and the snapshot must be young.
    // applied_ns==0 means the refresher never stamped it (hand-built
    // test snapshot may legitimately skip it — treat as fresh when the
    // heartbeat is non-zero so injected snapshots stay usable).
    if (snap->heartbeat_unix == 0) {
        return Verdict::Unverifiable;
    }
    const uint64_t age = (snap->applied_ns != 0 && now_ns > snap->applied_ns)
                             ? now_ns - snap->applied_ns
                             : 0;
    if (age > max_age_ns_.load(std::memory_order_acquire)) {
        return Verdict::Unverifiable;
    }
    if (snap->flagged.count(account_id) != 0 &&
        snap->cleared.count(account_id) == 0) {
        return Verdict::Hit;
    }
    if (require_screened() && snap->cleared.count(account_id) == 0) {
        const std::string& bm = snap->screened_bitmap;
        const uint64_t byte = account_id >> 3;
        const uint8_t mask = static_cast<uint8_t>(1u << (account_id & 7));
        const bool screened =
            byte < bm.size() &&
            (static_cast<uint8_t>(bm[byte]) & mask) != 0;
        if (!screened) {
            return Verdict::Unscreened;
        }
    }
    return Verdict::Clear;
}

// ---------------------------------------------------------------------------
// SanctionsRefresher — control path
// ---------------------------------------------------------------------------

SanctionsRefresher::SanctionsRefresher(RespClient* client) noexcept
    : client_(client) {}

bool SanctionsRefresher::refresh(SanctionsCache* cache) noexcept {
    if (cache == nullptr) return false;
    auto snap = std::make_shared<SanctionsCache::Snapshot>();
    snap->applied_ns = steady_ns();
    if (client_ == nullptr) {
        cache->mark_unverifiable();
        return false;
    }
    // Feed heartbeat — the liveness signal. A missing/parse-failed
    // heartbeat means the Go-side publisher is not running: flag data
    // cannot be trusted -> unverifiable (fail closed).
    {
        RespValue hb;
        if (!client_->execute({"GET", "exc:sanctions:feed:heartbeat"},
                              &hb) ||
            hb.is_error()) {
            cache->mark_unverifiable();
            return false;
        }
        std::string_view sv;
        if (hb.type == RespValue::Type::Integer && hb.integer > 0) {
            snap->heartbeat_unix = static_cast<uint64_t>(hb.integer);
        } else if (hb.as_string(&sv)) {
            uint64_t v = 0;
            if (parse_u64(sv, &v)) snap->heartbeat_unix = v;
        }
        // heartbeat_unix == 0 stays unverifiable at check time — the
        // snapshot still applies so the flag sets stay current for the
        // moment the publisher returns.
    }
    // Flag inventory — flagged:{id} + flagged:cleared:{id}.
    {
        RespValue v;
        if (!client_->execute({"KEYS", "exc:sanctions:flagged:*"}, &v) ||
            v.is_error() || v.type != RespValue::Type::Array) {
            cache->mark_unverifiable();
            return false;
        }
        for (const RespValue& item : v.items) {
            std::string_view key;
            if (!item.as_string(&key)) continue;
            (void)sanctions_key_into(key, snap.get());
        }
    }
    // Screened bitmap — whole-blob GET (bit-test on the check path).
    {
        RespValue v;
        if (!client_->execute({"GET", "exc:sanctions:screened:bloom"},
                              &v) ||
            v.is_error()) {
            cache->mark_unverifiable();
            return false;
        }
        std::string_view blob;
        if (v.as_string(&blob)) {
            snap->screened_bitmap.assign(blob);
        }
        // nil blob (never published) is legal — require_screened mode
        // then treats every account as unscreened.
    }
    cache->apply(std::move(snap));
    return true;
}

}  // namespace exch
