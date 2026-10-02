// AccountStateCache + AccountStateRefresher — see
// include/risk/AccountStateCache.hpp for the contract; mirrors
// SuspensionFlags/SuspensionRefresher discipline (control-thread Redis
// poll, immutable shared_ptr snapshot, zero heap / zero clock reads on
// the check path).

#include "risk/AccountStateCache.hpp"

#include <cstdlib>
#include <ctime>
#include <limits>
#include <mutex>
#include <string>

#include "redis/RespClient.hpp"

namespace exch {

namespace {

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

[[nodiscard]] bool parse_i64(std::string_view s, int64_t* out) noexcept {
    if (s.empty()) return false;
    bool neg = false;
    if (s[0] == '-') {
        neg = true;
        s = s.substr(1);
    }
    uint64_t v = 0;
    if (!parse_u64(s, &v)) return false;
    if (neg) {
        if (v > static_cast<uint64_t>(INT64_MAX) + 1) return false;
        *out = v == static_cast<uint64_t>(INT64_MAX) + 1
                   ? INT64_MIN
                   : -static_cast<int64_t>(v);
    } else {
        if (v > static_cast<uint64_t>(INT64_MAX)) return false;
        *out = static_cast<int64_t>(v);
    }
    return true;
}

// 3-byte ISO-4217 code -> packed uint24 ((c0<<16)|(c1<<8)|c2).
[[nodiscard]] bool parse_ccy24(std::string_view ccy, uint32_t* out) noexcept {
    if (ccy.size() != 3) return false;
    uint32_t v = 0;
    for (const char c : ccy) {
        if (c < 'A' || c > 'Z') return false;
        v = (v << 8) | static_cast<uint32_t>(c);
    }
    *out = v;
    return true;
}

[[nodiscard]] bool parse_status(std::string_view s,
                                AccountStatus* out) noexcept {
    if (s == "ACTIVE") {
        *out = AccountStatus::ACTIVE;
    } else if (s == "SUSPENDED") {
        *out = AccountStatus::SUSPENDED;
    } else if (s == "FROZEN") {
        *out = AccountStatus::FROZEN;
    } else if (s == "CLOSED") {
        *out = AccountStatus::CLOSED;
    } else {
        return false;
    }
    return true;
}

[[nodiscard]] bool parse_kyc(std::string_view s, KycTier* out) noexcept {
    if (s == "T0") {
        *out = KycTier::T0;
    } else if (s == "T1") {
        *out = KycTier::T1;
    } else if (s == "T2") {
        *out = KycTier::T2;
    } else {
        return false;
    }
    return true;
}

[[nodiscard]] bool parse_category(std::string_view s,
                                  ClientCategory* out) noexcept {
    if (s == "RETAIL") {
        *out = ClientCategory::RETAIL;
    } else if (s == "PROFESSIONAL") {
        *out = ClientCategory::PROFESSIONAL;
    } else if (s == "ELIGIBLE_COUNTERPARTY") {
        *out = ClientCategory::ELIGIBLE_COUNTERPARTY;
    } else {
        return false;
    }
    return true;
}

[[nodiscard]] bool parse_stp(std::string_view s, uint8_t* out) noexcept {
    if (s.empty() || s == "-") {
        *out = kStpModeUnset;
        return true;
    }
    StpMode m;
    if (s == "CANCEL_NEWEST") {
        m = StpMode::CANCEL_NEWEST;
    } else if (s == "CANCEL_OLDEST") {
        m = StpMode::CANCEL_OLDEST;
    } else if (s == "CANCEL_BOTH") {
        m = StpMode::CANCEL_BOTH;
    } else if (s == "DECREMENT") {
        m = StpMode::DECREMENT;
    } else if (s == "NONE") {
        m = StpMode::NONE;
    } else {
        return false;
    }
    *out = static_cast<uint8_t>(m);
    return true;
}

[[nodiscard]] uint64_t wall_unix_s() noexcept {
    return static_cast<uint64_t>(std::time(nullptr));
}

// Field prefix dispatch:
//   0 = foreign field (caller ignores)
//   1 = "{account_id}"          bare account-state record
//   2 = "a:{account}:{CCY}"     availability
//   3 = "p:{account}:{INSTR}"   net position
//   4 = "i:{instrument}"        instrument currency pair
//   5 = "c:{account}"           credit party index
[[nodiscard]] int classify_field(std::string_view field, uint64_t* id,
                                 uint32_t* aux) noexcept {
    if (field.size() > 2 && field[1] == ':') {
        const char tag = field[0];
        if (tag != 'a' && tag != 'p' && tag != 'i' && tag != 'c') return 0;
        const std::string_view rest = field.substr(2);
        if (tag == 'i') {
            if (!parse_u64(rest, id)) return 0;
            return 4;
        }
        if (tag == 'c') {  // c:{account} -> credit party index
            if (!parse_u64(rest, id)) return 0;
            return 5;
        }
        const std::size_t colon = rest.rfind(':');
        if (colon == std::string_view::npos || colon == 0) return 0;
        if (!parse_u64(rest.substr(0, colon), id)) return 0;
        if (tag == 'a') {
            if (!parse_ccy24(rest.substr(colon + 1), aux)) return 0;
            return 2;
        }
        uint64_t instr = 0;
        if (!parse_u64(rest.substr(colon + 1), &instr) ||
            instr > UINT32_MAX) {
            return 0;
        }
        *aux = static_cast<uint32_t>(instr);
        return 3;
    }
    if (parse_u64(field, id)) return 1;
    return 0;
}

}  // namespace

void AccountStateCache::apply(std::shared_ptr<const Snapshot> snap) noexcept {
    if (snap == nullptr) return;
    {
        std::unique_lock<std::shared_mutex> lk(mu_);
        snap_ = std::move(snap);
    }
    ready_.store(true, std::memory_order_release);
}

void AccountStateCache::mark_unverifiable() noexcept {
    auto snap = std::make_shared<Snapshot>();
    snap->unverifiable = true;
    {
        std::unique_lock<std::shared_mutex> lk(mu_);
        snap_ = std::move(snap);
    }
    ready_.store(true, std::memory_order_release);
}

std::shared_ptr<const AccountStateCache::Snapshot> AccountStateCache::load()
    const noexcept {
    std::shared_lock<std::shared_mutex> lk(mu_);
    return snap_;
}

const AccountStateCache::Snapshot::Rec* AccountStateCache::rec(
    uint64_t account_id) const noexcept {
    const auto snap = load();
    if (snap == nullptr || snap->unverifiable) return nullptr;
    const auto it = snap->accounts.find(account_id);
    return it == snap->accounts.end() ? nullptr : &it->second;
}

AccountStatus AccountStateCache::status(uint64_t account_id) const noexcept {
    const Snapshot::Rec* r = rec(account_id);
    return r != nullptr ? r->status : AccountStatus::UNKNOWN;
}

int64_t AccountStateCache::available_balance(uint64_t account_id,
                                             uint64_t instrument_id,
                                             BalanceUnit unit) const
    noexcept {
    const auto snap = load();
    if (snap == nullptr || snap->unverifiable) return -1;
    const auto ii = snap->instr.find(instrument_id);
    if (ii == snap->instr.end()) return -1;
    const uint64_t ccy =
        unit == BalanceUnit::BASE ? ii->second.first : ii->second.second;
    const auto it = snap->avail.find((account_id << 24) | ccy);
    return it == snap->avail.end() ? -1 : it->second;
}

uint32_t AccountStateCache::open_position_count(
    uint64_t account_id) const noexcept {
    const Snapshot::Rec* r = rec(account_id);
    return r != nullptr ? r->open_positions
                        : std::numeric_limits<uint32_t>::max();
}

StpMode AccountStateCache::default_stp_mode(
    uint64_t account_id) const noexcept {
    const Snapshot::Rec* r = rec(account_id);
    return r != nullptr ? static_cast<StpMode>(r->stp)
                        : static_cast<StpMode>(kStpModeUnset);
}

ClientCategory AccountStateCache::client_category(
    uint64_t account_id) const noexcept {
    const Snapshot::Rec* r = rec(account_id);
    return r != nullptr ? r->category : ClientCategory::RETAIL;
}

KycTier AccountStateCache::kyc_tier(uint64_t account_id) const noexcept {
    const Snapshot::Rec* r = rec(account_id);
    return r != nullptr ? r->kyc : KycTier::T0;
}

uint32_t AccountStateCache::credit_party_id(uint64_t account_id) const
    noexcept {
    const Snapshot::Rec* r = rec(account_id);
    return r != nullptr ? r->credit_party : kCreditMaxParties;
}

int64_t AccountStateCache::net_position_units(
    uint64_t account_id, uint64_t instrument_id) const noexcept {
    const auto snap = load();
    if (snap == nullptr || snap->unverifiable ||
        instrument_id > UINT32_MAX) {
        return 0;
    }
    const auto it = snap->pos.find(
        (account_id << 32) | static_cast<uint32_t>(instrument_id));
    return it == snap->pos.end() ? 0 : it->second;
}

// ---------------------------------------------------------------------------
// Field parser
// ---------------------------------------------------------------------------

bool account_field_into(std::string_view field, std::string_view value,
                        AccountStateCache::Snapshot* out) noexcept {
    if (out == nullptr) return false;
    if (field == "__hb__") {
        return parse_u64(value, &out->heartbeat_unix);
    }
    uint64_t id = 0;
    uint32_t aux = 0;
    switch (classify_field(field, &id, &aux)) {
        case 5: {  // c:{account} -> credit party index (spec §3.3b)
            uint64_t party = 0;
            if (!parse_u64(value, &party) || party > UINT32_MAX) {
                return false;
            }
            auto& r = out->accounts[id];  // value-init when absent
            r.credit_party = static_cast<uint32_t>(party);
            return true;
        }
        case 4: {  // i:{instrument} -> "BASE/QUOTE"
            const std::size_t slash = value.find('/');
            uint32_t b = 0, q = 0;
            if (slash == std::string_view::npos ||
                !parse_ccy24(value.substr(0, slash), &b) ||
                !parse_ccy24(value.substr(slash + 1), &q)) {
                return false;
            }
            out->instr[id] = {b, q};
            return true;
        }
        case 3: {  // p:{account}:{instr} -> net units
            int64_t pos = 0;
            if (!parse_i64(value, &pos)) return false;
            out->pos[(id << 32) | aux] = pos;
            return true;
        }
        case 2: {  // a:{account}:{ccy} -> available units
            int64_t avail = 0;
            if (!parse_i64(value, &avail)) return false;
            out->avail[(id << 24) | aux] = avail;
            return true;
        }
        case 1:
            break;
        default:
            return false;
    }

    // "<STATUS>,<KYC>,<CATEGORY>,<STP>,<OPEN_POS>" — exactly 5 comma-
    // separated columns; a malformed row drops the field, never poisons.
    AccountStateCache::Snapshot::Rec r{};
    std::string_view cols[5];
    std::size_t n = 0, start = 0;
    for (std::size_t i = 0; i <= value.size(); ++i) {
        if (i == value.size() || value[i] == ',') {
            if (n >= 5) return false;
            cols[n++] = value.substr(start, i - start);
            start = i + 1;
        }
    }
    if (n != 5) return false;
    uint64_t pos = 0;
    if (!parse_status(cols[0], &r.status) || !parse_kyc(cols[1], &r.kyc) ||
        !parse_category(cols[2], &r.category) ||
        !parse_stp(cols[3], &r.stp) || !parse_u64(cols[4], &pos) ||
        pos > UINT32_MAX) {
        return false;
    }
    r.open_positions = static_cast<uint32_t>(pos);
    // Preserve a c:{account} party index that may have arrived before the
    // account record — HGETALL field order is unspecified.
    r.credit_party = out->accounts[id].credit_party;
    out->accounts[id] = r;
    return true;
}

// ---------------------------------------------------------------------------
// AccountStateRefresher — control path
// ---------------------------------------------------------------------------

AccountStateRefresher::AccountStateRefresher(
    RespClient* client, uint64_t max_hb_age_s,
    uint64_t (*now_unix_fn)()) noexcept
    : client_(client),
      max_hb_age_s_(max_hb_age_s),
      now_unix_fn_(now_unix_fn != nullptr ? now_unix_fn : wall_unix_s) {}

bool AccountStateRefresher::refresh(AccountStateCache* cache) noexcept {
    if (cache == nullptr) return false;
    if (client_ == nullptr) {
        cache->mark_unverifiable();
        return false;
    }
    RespValue v;
    if (!client_->execute({"HGETALL", "account:state"}, &v) ||
        v.is_error() || v.type != RespValue::Type::Array ||
        v.items.size() % 2 != 0) {
        cache->mark_unverifiable();
        return false;
    }
    auto snap = std::make_shared<AccountStateCache::Snapshot>();
    snap->applied_unix = now_unix_fn_();
    for (std::size_t i = 0; i + 1 < v.items.size(); i += 2) {
        std::string_view f, val;
        if (!v.items[i].as_string(&f) || !v.items[i + 1].as_string(&val)) {
            continue;  // foreign encoding — ignore the field, not the poll
        }
        (void)account_field_into(f, val, snap.get());
    }
    // Liveness gate (control thread, wall clock): a missing or stale
    // publisher heartbeat means the Go projector is down — cached rows
    // cannot be trusted, fail closed.
    const uint64_t now = now_unix_fn_();
    if (snap->heartbeat_unix == 0 ||
        (now > snap->heartbeat_unix &&
         now - snap->heartbeat_unix > max_hb_age_s_)) {
        cache->mark_unverifiable();
        return false;
    }
    cache->apply(std::move(snap));
    return true;
}

}  // namespace exch
