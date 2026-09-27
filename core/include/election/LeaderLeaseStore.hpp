#pragma once

// Task 2.3.5 — pluggable backend seam for the per-shard epoch lease.
//
// Canonical mechanics are spec §18.6.2 (remediation #35 — the epoch-lease
// model supersedes the pre-epoch SETNX contract still printed in spec §2.5 /
// §4.2: TTL 10s / heartbeat 3s must NOT be implemented):
//
//   * lease key  `engine:leader:{shardId}` — STRING, PX 2,000ms
//   * value      `{"epoch":N,"leader":"<node-id>"}` — 64-bit monotonic
//     fencing epoch plus holder token
//   * refresh    every 500ms, token-checked (leader+epoch must match)
//   * release    Lua compare-and-del on {epoch, leader} — token-checked
//     revocation is mandatory for any lease release (spec §2.5 note)
//   * epoch      `engine:leader:epoch:{shardId}` — persistent INCR counter,
//     gives globally monotonically increasing fencing tokens even across
//     lease expiry (a bare "old+1 from current value" cannot: an expired
//     key carries no epoch). Fencing tokens must never regress.
//
// The interface is what LeaderElection depends on; RedisLeaseStore is the
// production binding over RespClient+Lua, and tests substitute a
// deterministic in-memory fake (test_election.cpp) — every method is
// noexcept and transport failures surface as `false`/`LeaseCheck::Error`,
// never swallowed (fail-closed, spec §2.7).

#include <cstdint>
#include <string>
#include <string_view>

namespace exch {

class RespClient;

// Canonical timing constants — spec §18.6.2 / remediation #35.
inline constexpr int64_t kLeaderLeaseTtlMs = 2'000;   // PX on the lease key
inline constexpr int64_t kLeaderRefreshMs = 500;      // heartbeat cadence
inline constexpr int64_t kLeaderSettleMs = 5'000;     // follower startup gate
inline constexpr int64_t kLeaderHbKeyTtlMs = 15'000;  // leader:heartbeat:{shard} §4.2

enum class LeaderRole : uint8_t { FOLLOWER, LEADER };

// Result of a token-checked heartbeat refresh.
enum class LeaseCheck : uint8_t {
    Held,        // still ours: TTL re-armed, epoch unchanged
    Superseded,  // another holder or a newer epoch — we were fenced
    Absent,      // lease key gone (expired/revoked): our claim is invalid
    Error,       // transport/store failure — lease state unknown
};

struct LeaseRecord {
    bool present = false;
    uint64_t epoch = 0;
    std::string leader;
    int64_t ttl_ms = -2;  // Redis PTTL semantics: -2 absent, -1 no TTL
};

// Serialize/parse the canonical lease value `{"epoch":N,"leader":"<id>"}`.
// parse returns false on any malformed input — callers treat unparseable
// state as "foreign" (fail-closed).
[[nodiscard]] std::string format_lease_value(uint64_t epoch,
                                             std::string_view leader);
[[nodiscard]] bool parse_lease_value(std::string_view value, uint64_t* epoch,
                                     std::string* leader) noexcept;
// Node-id charset: [A-Za-z0-9._-] only — keeps the value JSON-safe without
// escaping and keeps the Lua `string.match` patterns exact.
[[nodiscard]] bool valid_node_id(std::string_view id) noexcept;

class LeaderLeaseStore {
public:
    virtual ~LeaderLeaseStore() = default;

    // Atomic acquire. On success returns true with *granted=true and
    // *epoch_out = the fencing epoch we now hold. On success with
    // *granted=false the lease is held by another node and *epoch_out is
    // that holder's epoch (0 if unreadable). false = transport error.
    virtual bool try_acquire(std::string_view leader, int64_t ttl_ms,
                             bool* granted, uint64_t* epoch_out) noexcept = 0;

    // Token-checked heartbeat: refresh TTL iff stored {leader,epoch} equals
    // ours. *cur_epoch_out receives the stored epoch on Held/Superseded so
    // the caller detects supersession (spec §18.6.2 split-brain fencing).
    virtual LeaseCheck heartbeat(std::string_view leader, uint64_t epoch,
                                 int64_t ttl_ms,
                                 uint64_t* cur_epoch_out) noexcept = 0;

    // Token-checked release. Returns true only when our {leader,epoch} lease
    // was actually deleted — never true on transport error or foreign hold.
    virtual bool release(std::string_view leader, uint64_t epoch) noexcept = 0;

    // Read the current record. false = transport error (state unknown).
    virtual bool read(LeaseRecord* out) noexcept = 0;
};

// Production binding: single shard's lease keys over one RespClient.
// Non-owning — the client must outlive the store.
class RedisLeaseStore final : public LeaderLeaseStore {
public:
    RedisLeaseStore(RespClient* client, uint32_t shard_id) noexcept;

    bool try_acquire(std::string_view leader, int64_t ttl_ms, bool* granted,
                     uint64_t* epoch_out) noexcept override;
    LeaseCheck heartbeat(std::string_view leader, uint64_t epoch,
                         int64_t ttl_ms,
                         uint64_t* cur_epoch_out) noexcept override;
    bool release(std::string_view leader, uint64_t epoch) noexcept override;
    bool read(LeaseRecord* out) noexcept override;

    [[nodiscard]] const std::string& lease_key() const noexcept {
        return lease_key_;
    }

private:
    RespClient* client_;
    std::string lease_key_;   // engine:leader:{shardId}
    std::string epoch_key_;   // engine:leader:epoch:{shardId} (INCR counter)
    std::string hb_key_;      // leader:heartbeat:{shardId} (§4.2 observability)
};

}  // namespace exch
