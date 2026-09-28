// PHASE-04 TASK-4.3.1 — snapshot-ready notify / PG-persisted ack / WAL trim
// (spec §3.5). See include/recovery/SnapshotManager.hpp for the contract.

#include "recovery/SnapshotManager.hpp"

#include <cerrno>
#include <cstdio>
#include <cstring>

#include <fcntl.h>
#include <unistd.h>

namespace exch {

std::string snap_ready_name(std::string_view base, uint16_t shard) {
    return std::string(base) + "_" + std::to_string(shard) + "_snap";
}

std::string snap_ack_name(std::string_view base, uint16_t shard) {
    return std::string(base) + "_" + std::to_string(shard) + "_snap_ack";
}

SnapshotManager::SnapshotManager(uint16_t shard_id,
                                 const FileSnapshotSink& sink) noexcept
    : shard_id_(shard_id), sink_(sink) {}

void SnapshotManager::open(std::string_view notify_name,
                           std::string_view ack_name, uint32_t capacity,
                           uint32_t slot_payload) noexcept {
    if (slot_payload < sizeof(SnapReadyMsg)) {
        return;  // a ring that can never carry a ready record is useless
    }
    (void)notify_.open(notify_name, ShmRing::Role::Producer,
                       /*create=*/true, capacity, slot_payload);
    (void)ack_.open(ack_name, ShmRing::Role::Consumer,
                    /*create=*/true, capacity, slot_payload);
}

void SnapshotManager::close() noexcept {
    notify_.close();
    ack_.close();
}

bool SnapshotManager::notify_stored(uint32_t instrument_id,
                                    uint64_t snapshot_seq) noexcept {
    // Stat + CRC the file the sink just fsynced. Failure here means the
    // durable signal itself is suspect — do not notify (the dir scan will
    // surface the file; the PG writer's own size/CRC check is the second
    // line of defense).
    const std::string path = sink_.snapshot_path(instrument_id, snapshot_seq);
    const int fd = ::open(path.c_str(), O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        ++notify_drops_;
        return false;
    }
    uint32_t crc = 0;
    uint64_t size = 0;
    bool ok = true;
    {
        uint8_t buf[64 * 1024];
        for (;;) {
            const ssize_t n = ::read(fd, buf, sizeof(buf));
            if (n < 0) {
                if (errno == EINTR) continue;
                ok = false;
                break;
            }
            if (n == 0) break;
            crc = wal_crc32c_continue(crc, buf, static_cast<std::size_t>(n));
            size += static_cast<uint64_t>(n);
        }
    }
    ::close(fd);
    if (!ok) {
        ++notify_drops_;
        return false;
    }

    SnapReadyMsg m{};
    m.magic = kSnapReadyMagic;
    m.version = kSnapMsgVersion;
    m.shard_id = shard_id_;
    m.instrument_id = instrument_id;
    m.snapshot_seq = snapshot_seq;
    m.byte_size = size;
    m.file_crc32c = crc;
    const std::string rel =
        sink_.snapshot_rel_path(instrument_id, snapshot_seq);
    m.rel_path_len = static_cast<uint32_t>(
        rel.size() < sizeof(m.rel_path) ? rel.size() : sizeof(m.rel_path));
    std::memcpy(m.rel_path, rel.data(), m.rel_path_len);

    if (!notify_.is_open() ||
        !notify_.try_write(&m, static_cast<uint32_t>(sizeof(m)))) {
        ++notify_drops_;  // ring full/absent — the Go dir scan catches up
        return false;
    }
    ++notifies_sent_;
    return true;
}

uint64_t SnapshotManager::poll_acks(Wal& wal) noexcept {
    uint64_t confirmed = confirmed_seq_;
    for (;;) {
        uint32_t len = 0;
        const uint8_t* p = ack_.peek(&len);
        if (p == nullptr) break;
        // Consume unconditionally — a malformed slot must not wedge the
        // consumer (poison-slot discipline, same as EnginePump).
        SnapAckMsg m{};
        const bool shape_ok = (len == sizeof(SnapAckMsg));
        if (shape_ok) std::memcpy(&m, p, sizeof(m));
        ack_.consume();
        if (!shape_ok || m.magic != kSnapAckMagic ||
            m.version != kSnapMsgVersion || m.shard_id != shard_id_ ||
            m.status != kSnapAckPersisted) {
            continue;  // foreign/malformed/failed persist — not a trim gate
        }
        ++acks_seen_;
        if (m.snapshot_seq > confirmed) confirmed = m.snapshot_seq;
    }
    if (confirmed <= confirmed_seq_) return 0;
    confirmed_seq_ = confirmed;
    const uint64_t n = wal.trim_sealed(confirmed);
    trimmed_ += n;
    return n;
}

}  // namespace exch
