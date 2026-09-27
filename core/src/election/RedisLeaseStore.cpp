// Task 2.3.5 — Redis binding of the epoch-lease contract (spec §18.6.2,
// remediation #35: canonical; supersedes the §2.5/§4.2 SETNX 10s/3s legacy
// values). All mutating operations are single Lua scripts so {inspect,
// compare, write} executes atomically on the Redis thread — no check-then-
// act window exists between the fencing read and the lease write.

#include <cstdio>

#include "election/LeaderLeaseStore.hpp"
#include "redis/RespClient.hpp"

namespace exch {
namespace {

// Acquire: claim the lease iff absent (an expired key reads absent in Redis,
// so "lease expired" needs no special case). The fencing epoch comes from a
// persistent INCR counter — strictly monotonic across expiry and failover,
// which a value-embedded "old+1" cannot guarantee (an expired key carries
// no epoch at all). Idempotent for the current holder: re-acquiring our own
// lease just re-arms the TTL.
const char* kAcquireScript = R"LUA(
local cur = redis.call('GET', KEYS[1])
if cur then
  local l = string.match(cur, '"leader":"([^"]-)"')
  local e = tonumber(string.match(cur, '"epoch":(%d+)')) or 0
  if l == ARGV[1] then
    redis.call('PEXPIRE', KEYS[1], ARGV[2])
    return {1, e}
  end
  return {0, e}
end
local epoch = redis.call('INCR', KEYS[2])
redis.call('SET', KEYS[1],
  '{"epoch":' .. epoch .. ',"leader":"' .. ARGV[1] .. '"}', 'PX', ARGV[2])
return {1, epoch}
)LUA";

// Heartbeat: refresh TTL iff stored {leader,epoch} still equals ours, and
// refresh the §4.2 observability key leader:heartbeat:{shard} in the same
// atomic step. Returns {0,epoch}=Held, {1,epoch}=Superseded, {2,-1}=Absent —
// the superseded branch hands back the winning epoch so the caller can run
// the spec §18.6.2 fencing rule `epoch_local == epoch_current`.
const char* kHeartbeatScript = R"LUA(
local cur = redis.call('GET', KEYS[1])
if not cur then return {2, -1} end
local e = tonumber(string.match(cur, '"epoch":(%d+)')) or -1
local l = string.match(cur, '"leader":"([^"]-)"')
if l == ARGV[1] and e == tonumber(ARGV[2]) then
  redis.call('PEXPIRE', KEYS[1], ARGV[3])
  redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[4])
  return {0, e}
end
return {1, e}
)LUA";

// Release: mandatory token-checked compare-and-del (spec §2.5 remediation
// #35) — an expired-and-reacquired lease must never be deleted out from
// under its new holder.
const char* kReleaseScript = R"LUA(
local cur = redis.call('GET', KEYS[1])
if not cur then return 0 end
local e = tonumber(string.match(cur, '"epoch":(%d+)')) or -1
local l = string.match(cur, '"leader":"([^"]-)"')
if l == ARGV[1] and e == tonumber(ARGV[2]) then
  return redis.call('DEL', KEYS[1])
end
return 0
)LUA";

// Atomic read: value + PTTL in one step so the record can never be observed
// between expiry and read.
const char* kReadScript = R"LUA(
local cur = redis.call('GET', KEYS[1])
if not cur then return {0, '', -2} end
return {1, cur, redis.call('PTTL', KEYS[1])}
)LUA";

}  // namespace

std::string format_lease_value(uint64_t epoch, std::string_view leader) {
    std::string v;
    v.reserve(24 + leader.size());
    v += "{\"epoch\":";
    v += std::to_string(epoch);
    v += ",\"leader\":\"";
    v += leader;
    v += "\"}";
    return v;
}

bool parse_lease_value(std::string_view value, uint64_t* epoch,
                       std::string* leader) noexcept {
    // Exact shape {"epoch":<digits>,"leader":"<id>"} — strict parse, no
    // whitespace tolerance: anything we did not write is foreign state.
    static constexpr std::string_view kPre = "{\"epoch\":";
    static constexpr std::string_view kMid = ",\"leader\":\"";
    static constexpr std::string_view kEnd = "\"}";
    if (value.size() < kPre.size() + kMid.size() + kEnd.size() + 2) return false;
    if (value.substr(0, kPre.size()) != kPre) return false;
    size_t pos = kPre.size();
    uint64_t e = 0;
    const size_t e_begin = pos;
    while (pos < value.size() && value[pos] >= '0' && value[pos] <= '9') {
        const uint64_t d = static_cast<uint64_t>(value[pos] - '0');
        if (e > (UINT64_MAX - d) / 10) return false;  // overflow → foreign
        e = e * 10 + d;
        ++pos;
    }
    if (pos == e_begin) return false;
    if (value.substr(pos, kMid.size()) != kMid) return false;
    pos += kMid.size();
    const size_t l_begin = pos;
    while (pos < value.size() && value[pos] != '"') ++pos;
    const size_t l_len = pos - l_begin;
    // pos sits on the leader's closing quote; the value must end exactly `"}`
    if (pos >= value.size() || value.substr(pos) != kEnd) return false;
    if (l_len == 0) return false;
    if (epoch != nullptr) *epoch = e;
    if (leader != nullptr) leader->assign(value.substr(l_begin, l_len));
    return true;
}

bool valid_node_id(std::string_view id) noexcept {
    if (id.empty() || id.size() > 64) return false;
    for (const char c : id) {
        const bool ok = (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
                        (c >= '0' && c <= '9') || c == '.' || c == '_' ||
                        c == '-';
        if (!ok) return false;
    }
    return true;
}

RedisLeaseStore::RedisLeaseStore(RespClient* client, uint32_t shard_id) noexcept
    : client_(client) {
    char buf[96];
    std::snprintf(buf, sizeof(buf), "engine:leader:%u", shard_id);
    lease_key_ = buf;
    std::snprintf(buf, sizeof(buf), "engine:leader:epoch:%u", shard_id);
    epoch_key_ = buf;
    std::snprintf(buf, sizeof(buf), "leader:heartbeat:%u", shard_id);
    hb_key_ = buf;
}

bool RedisLeaseStore::try_acquire(std::string_view leader, int64_t ttl_ms,
                                  bool* granted, uint64_t* epoch_out) noexcept {
    if (granted == nullptr || epoch_out == nullptr || client_ == nullptr ||
        !valid_node_id(leader)) {
        return false;
    }
    *granted = false;
    *epoch_out = 0;
    const std::string ttl = std::to_string(ttl_ms);
    RespValue out;
    if (!client_->eval(kAcquireScript, 2,
                       {lease_key_, epoch_key_, leader, ttl}, &out) ||
        out.is_error() || out.type != RespValue::Type::Array ||
        out.items.size() != 2) {
        return false;
    }
    int64_t g = 0, e = 0;
    if (!out.items[0].as_int(&g) || !out.items[1].as_int(&e) || e < 0) {
        return false;
    }
    *epoch_out = static_cast<uint64_t>(e);
    *granted = (g == 1);
    return true;
}

LeaseCheck RedisLeaseStore::heartbeat(std::string_view leader, uint64_t epoch,
                                      int64_t ttl_ms,
                                      uint64_t* cur_epoch_out) noexcept {
    if (cur_epoch_out != nullptr) *cur_epoch_out = 0;
    if (client_ == nullptr || !valid_node_id(leader)) return LeaseCheck::Error;
    const std::string e_str = std::to_string(epoch);
    const std::string ttl = std::to_string(ttl_ms);
    const std::string hb_ttl = std::to_string(kLeaderHbKeyTtlMs);
    RespValue out;
    if (!client_->eval(kHeartbeatScript, 2,
                       {lease_key_, hb_key_, leader, e_str, ttl, hb_ttl},
                       &out) ||
        out.is_error() || out.type != RespValue::Type::Array ||
        out.items.size() != 2) {
        return LeaseCheck::Error;
    }
    int64_t code = -1, e = -1;
    if (!out.items[0].as_int(&code) || !out.items[1].as_int(&e)) {
        return LeaseCheck::Error;
    }
    if (cur_epoch_out != nullptr && e > 0) {
        *cur_epoch_out = static_cast<uint64_t>(e);
    }
    switch (code) {
        case 0: return LeaseCheck::Held;
        case 1: return LeaseCheck::Superseded;
        case 2: return LeaseCheck::Absent;
        default: return LeaseCheck::Error;
    }
}

bool RedisLeaseStore::release(std::string_view leader,
                              uint64_t epoch) noexcept {
    if (client_ == nullptr || !valid_node_id(leader)) return false;
    const std::string e_str = std::to_string(epoch);
    RespValue out;
    int64_t n = 0;
    return client_->eval(kReleaseScript, 1, {lease_key_, leader, e_str},
                         &out) &&
           !out.is_error() && out.as_int(&n) && n == 1;
}

bool RedisLeaseStore::read(LeaseRecord* out) noexcept {
    if (out == nullptr || client_ == nullptr) return false;
    *out = LeaseRecord{};
    RespValue v;
    if (!client_->eval(kReadScript, 1, {lease_key_}, &v) || v.is_error() ||
        v.type != RespValue::Type::Array || v.items.size() != 3) {
        return false;
    }
    int64_t present = 0, ttl = -2;
    std::string_view raw;
    if (!v.items[0].as_int(&present) || !v.items[1].as_string(&raw) ||
        !v.items[2].as_int(&ttl)) {
        return false;
    }
    out->ttl_ms = ttl;
    if (present != 1) return true;  // absent record is a successful read
    uint64_t e = 0;
    std::string leader;
    // Unparseable lease state is reported absent — callers must never act on
    // a value they cannot verify (fail-closed).
    if (!parse_lease_value(raw, &e, &leader)) {
        out->ttl_ms = -2;
        return true;
    }
    out->present = true;
    out->epoch = e;
    out->leader = std::move(leader);
    return true;
}

}  // namespace exch
