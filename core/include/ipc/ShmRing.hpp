#pragma once

// Task 1.3.5 — shared-memory SPSC ring (Aeron fallback transport).
//
// Layout is ABI-fixed and mirrored byte-for-byte by the Go endpoint in
// services/internal/ipc/shm.go. One ring carries one direction:
//
//   /exchange_ipc_{shard}_in   Go gateway -> C++ core   (orders in)
//   /exchange_ipc_{shard}_out  C++ core   -> Go gateway (trades, book updates)
//
// Ring image (all offsets bytes from the mmap base):
//   0    u64 head                    producer write cursor (monotonic)
//   64   u64 tail                    consumer read cursor (monotonic)
//   128  u64 producer_heartbeat_ns   CLOCK_MONOTONIC ns, updated per write batch
//   192  u64 producer_pid            getpid() of the producer (liveness probe)
//   256  u32 magic (kMagic), u32 version (kVersion)
//   264  u64 capacity (power of two)
//   272  u64 slot_payload (bytes of payload per slot)
//   280  u64 drops                   producer increments when ring is full
//   288..319 reserved (zero)
//   320..  slot[0..capacity)         stride = 64B header + slot_payload
//
// Slot header (64B, one cache line): {u32 seq, u32 len, u32 flags, pad}.
// seq echoes the low 32 bits of the write index — diagnostic only; ordering is
// guaranteed by release/acquire on head (producer) / tail (consumer), the
// canonical SPSC discipline.
//
// Zero-copy: the consumer peeks a pointer directly into the slot payload
// (peek/consume); FlatBuffers reads in place — no memcpy into an
// intermediate buffer (spec §2.3 / Task 1.3.5 §4).
//
// Backpressure: when head - tail == capacity the producer declines the write
// (try_write returns false) and bumps `drops`; the caller decides to retry or
// escalate (maps to ENGINE_OVERLOAD in Task 2.3.7). Producer-death detection:
// producer stamps pid + heartbeat; consumers use producer_alive()/heartbeat_ns().

#include <atomic>
#include <cerrno>
#include <cstdint>
#include <cstring>
#include <fcntl.h>
#include <signal.h>
#include <new>
#include <string>
#include <string_view>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <time.h>
#include <unistd.h>

namespace exch {

class ShmRing {
public:
    enum class Role : uint8_t { Producer, Consumer };

    static constexpr uint32_t kMagic = 0x45584348u;  // "EXCH"
    static constexpr uint32_t kVersion = 1u;
    static constexpr uint64_t kHeaderBytes = 320;
    static constexpr uint64_t kSlotHeaderBytes = 64;
    static constexpr uint32_t kDefaultCapacity = 4096;     // slots (power of 2)
    static constexpr uint32_t kDefaultSlotPayload = 1024;  // bytes per slot

    // Byte offsets of the shared header fields (mirrored in Go shm.go).
    static constexpr uint64_t kOffHead = 0;
    static constexpr uint64_t kOffTail = 64;
    static constexpr uint64_t kOffHeartbeat = 128;
    static constexpr uint64_t kOffPid = 192;
    static constexpr uint64_t kOffMagic = 256;
    static constexpr uint64_t kOffVersion = 260;
    static constexpr uint64_t kOffCapacity = 264;
    static constexpr uint64_t kOffSlotPayload = 272;
    static constexpr uint64_t kOffDrops = 280;
    static constexpr uint64_t kOffSlots = kHeaderBytes;

    // slot header fields
    static constexpr uint64_t kOffSlotSeq = 0;
    static constexpr uint64_t kOffSlotLen = 4;
    static constexpr uint64_t kOffSlotFlags = 8;

    ShmRing() = default;
    ShmRing(std::string_view shm_name, Role role, bool create,
            uint32_t capacity = kDefaultCapacity,
            uint32_t slot_payload = kDefaultSlotPayload) {
        open(shm_name, role, create, capacity, slot_payload);
    }

    ShmRing(const ShmRing&) = delete;
    ShmRing& operator=(const ShmRing&) = delete;
    ShmRing(ShmRing&& o) noexcept { move_from(o); }
    ShmRing& operator=(ShmRing&& o) noexcept {
        if (this != &o) { close(); move_from(o); }
        return *this;
    }
    ~ShmRing() { close(); }

    // `create` is advisory: whichever endpoint finds a fresh (zero-size) image
    // initializes it; a live image (magic stamped) is always attached as-is —
    // never re-initialized — so late-joining peers can never wipe in-flight
    // messages. Stale images from a dead run must be removed via shm_unlink
    // (deploy/scripts/bench_ipc.sh unlinks before benching).
    bool open(std::string_view shm_name, Role role,
              [[maybe_unused]] bool create,
              uint32_t capacity = kDefaultCapacity,
              uint32_t slot_payload = kDefaultSlotPayload) noexcept {
        close();
        if ((capacity & (capacity - 1)) != 0 || capacity == 0)
            return false;
        if (slot_payload == 0 || (slot_payload % 64) != 0)
            return false;  // keep slot stride cache-line aligned
        if (shm_name.empty() || shm_name.size() >= sizeof(name_))
            return false;

        std::memcpy(name_, shm_name.data(), shm_name.size());
        name_[shm_name.size()] = '\0';
        // shm_open wants a leading '/' name.
        std::string shm_path = "/";
        shm_path += shm_name;

        const int fd = ::shm_open(shm_path.c_str(), O_RDWR | O_CREAT, 0600);
        if (fd < 0)
            return false;

        const uint64_t want_size =
            kHeaderBytes + capacity * (kSlotHeaderBytes + slot_payload);

        struct stat st {};
        if (::fstat(fd, &st) != 0) {
            ::close(fd);
            return false;
        }

        const bool fresh = (st.st_size == 0);
        if (!fresh && st.st_size < static_cast<off_t>(kHeaderBytes)) {
            ::close(fd);
            return false;  // truncated image — cannot hold the header
        }
        if (fresh) {
            if (::ftruncate(fd, static_cast<off_t>(want_size)) != 0) {
                ::close(fd);
                return false;
            }
        }

        // Fresh images map the size we just truncated; attaches map exactly
        // the existing file (never past EOF — avoids SIGBUS on a truncated
        // image). The header capacity check below still bounds slot access.
        const uint64_t map_len =
            fresh ? want_size : static_cast<uint64_t>(st.st_size);
        void* base = ::mmap(nullptr, map_len, PROT_READ | PROT_WRITE,
                            MAP_SHARED, fd, 0);
        ::close(fd);
        if (base == MAP_FAILED)
            return false;

        base_ = static_cast<uint8_t*>(base);
        map_size_ = map_len;

        if (fresh) {
            std::memset(base_, 0, static_cast<size_t>(kHeaderBytes));
            write_u32(kOffVersion, kVersion);
            write_u64(kOffCapacity, capacity);
            write_u64(kOffSlotPayload, slot_payload);
            write_u64(kOffDrops, 0);
            head().store(0, std::memory_order_relaxed);
            tail().store(0, std::memory_order_relaxed);
            heartbeat().store(0, std::memory_order_relaxed);
            pid_cell().store(0, std::memory_order_relaxed);
            // Magic stamped last with release: peers treat it as the
            // init-complete barrier (their acquire-read sees all the above).
            magic_cell().store(kMagic, std::memory_order_release);
        } else {
            // Attach: spin until the creator stamps magic (bounded ~5s).
            const int64_t deadline = mono_ns() + 5'000'000'000LL;
            while (const_magic().load(std::memory_order_acquire) != kMagic) {
                if (mono_ns() > deadline) { close(); return false; }
            }
            if (read_u32(kOffVersion) != kVersion) { close(); return false; }
            capacity = static_cast<uint32_t>(read_u64(kOffCapacity));
            slot_payload = static_cast<uint32_t>(read_u64(kOffSlotPayload));
            const uint64_t need =
                kHeaderBytes + capacity * (kSlotHeaderBytes + slot_payload);
            if (need > map_size_) { close(); return false; }
        }

        capacity_ = capacity;
        slot_payload_ = slot_payload;
        slot_stride_ = kSlotHeaderBytes + slot_payload;
        mask_ = capacity - 1;
        role_ = role;

        if (role == Role::Producer) {
            pid_cell().store(static_cast<uint64_t>(::getpid()),
                             std::memory_order_relaxed);
            heartbeat().store(static_cast<uint64_t>(realtime_ns()),
                              std::memory_order_relaxed);
        }
        open_ = true;
        return true;
    }

    void close() noexcept {
        if (base_ != nullptr) {
            ::munmap(base_, map_size_);
            base_ = nullptr;
        }
        open_ = false;
        capacity_ = slot_payload_ = 0;
        slot_stride_ = mask_ = 0;
    }

    [[nodiscard]] bool is_open() const noexcept { return open_; }
    [[nodiscard]] Role role() const noexcept { return role_; }
    [[nodiscard]] uint32_t capacity() const noexcept { return capacity_; }
    [[nodiscard]] uint32_t slot_payload() const noexcept { return slot_payload_; }
    [[nodiscard]] const char* name() const noexcept { return name_; }

    // -- Producer API -------------------------------------------------------

    // Non-blocking write. false => ring full (backpressure; see drops()).
    bool try_write(const void* data, uint32_t len) noexcept {
        if (!open_ || role_ != Role::Producer || len > slot_payload_)
            return false;
        const uint64_t h = head().load(std::memory_order_relaxed);
        const uint64_t t = tail().load(std::memory_order_acquire);
        if (h - t >= capacity_) {
            drops_cell().fetch_add(1, std::memory_order_relaxed);
            return false;
        }
        uint8_t* slot = slot_ptr(h);
        write_slot_u32(slot, kOffSlotLen, len);
        write_slot_u32(slot, kOffSlotFlags, 0);
        write_slot_u32(slot, kOffSlotSeq, static_cast<uint32_t>(h));
        if (len > 0)
            std::memcpy(slot + kSlotHeaderBytes, data, len);
        heartbeat().store(static_cast<uint64_t>(realtime_ns()),
                          std::memory_order_relaxed);
        // Release: payload + slot header visible before head advances.
        head().store(h + 1, std::memory_order_release);
        return true;
    }

    // Bounded-spin write; returns false only after `spin_ns` of saturation.
    bool write_wait(const void* data, uint32_t len, int64_t spin_ns) noexcept {
        const int64_t deadline = mono_ns() + spin_ns;
        while (!try_write(data, len)) {
            if (mono_ns() >= deadline)
                return false;
        }
        return true;
    }

    void beat() noexcept {
        if (open_ && role_ == Role::Producer)
            heartbeat().store(static_cast<uint64_t>(realtime_ns()),
                              std::memory_order_relaxed);
    }

    // -- Consumer API -------------------------------------------------------

    // Zero-copy peek at the next inbound slot. Returns payload pointer,
    // *len_out its size; nullptr when drained. Valid until consume().
    const uint8_t* peek(uint32_t* len_out) noexcept {
        if (!open_ || role_ != Role::Consumer)
            return nullptr;
        const uint64_t t = tail().load(std::memory_order_relaxed);
        const uint64_t h = head().load(std::memory_order_acquire);
        if (t == h)
            return nullptr;
        const uint8_t* slot = slot_ptr(t);
        const uint32_t len = read_slot_u32(slot, kOffSlotLen);
        if (len > slot_payload_)  // torn/corrupt slot — fail closed
            return nullptr;
        if (len_out) *len_out = len;
        return slot + kSlotHeaderBytes;
    }

    // Commit the peeked slot; call only after a non-null peek().
    void consume() noexcept {
        const uint64_t t = tail().load(std::memory_order_relaxed);
        tail().store(t + 1, std::memory_order_release);
    }

    // Copying read for IpcChannel-compat call sites (no intermediate buffer —
    // copies straight from the slot into the caller's destination).
    int32_t read(void* buf, uint32_t buf_cap) noexcept {
        uint32_t len = 0;
        const uint8_t* p = peek(&len);
        if (p == nullptr)
            return 0;
        if (len > buf_cap)
            return -1;  // caller buffer too small; slot left pending
        std::memcpy(buf, p, len);
        consume();
        return static_cast<int32_t>(len);
    }

    // -- Introspection ------------------------------------------------------

    [[nodiscard]] uint64_t occupancy() const noexcept {
        const uint64_t h = const_head().load(std::memory_order_acquire);
        const uint64_t t = const_tail().load(std::memory_order_acquire);
        return h - t;
    }
    [[nodiscard]] uint64_t drops() const noexcept {
        return const_drops().load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t producer_pid() const noexcept {
        return const_pid().load(std::memory_order_relaxed);
    }
    [[nodiscard]] uint64_t producer_heartbeat_ns() const noexcept {
        return const_heartbeat().load(std::memory_order_relaxed);
    }
    // Producer-death detection: kill(pid,0) — ESRCH means the writer is gone.
    [[nodiscard]] bool producer_alive() const noexcept {
        const uint64_t p = producer_pid();
        if (p == 0)
            return false;
        if (::kill(static_cast<pid_t>(p), 0) == 0)
            return true;
        return errno == EPERM;
    }

    static int64_t mono_ns() noexcept {
        timespec ts {};
        ::clock_gettime(CLOCK_MONOTONIC, &ts);
        return static_cast<int64_t>(ts.tv_sec) * 1'000'000'000LL + ts.tv_nsec;
    }

    // Wall-clock ns — heartbeat domain shared with the Go endpoint
    // (time.Now().UnixNano()); liveness itself is decided by kill(pid,0).
    static int64_t realtime_ns() noexcept {
        timespec ts {};
        ::clock_gettime(CLOCK_REALTIME, &ts);
        return static_cast<int64_t>(ts.tv_sec) * 1'000'000'000LL + ts.tv_nsec;
    }

private:
    void move_from(ShmRing& o) noexcept {
        base_ = o.base_; map_size_ = o.map_size_;
        capacity_ = o.capacity_; slot_payload_ = o.slot_payload_;
        slot_stride_ = o.slot_stride_; mask_ = o.mask_;
        role_ = o.role_; open_ = o.open_;
        std::memcpy(name_, o.name_, sizeof(name_));
        o.base_ = nullptr; o.open_ = false;
    }

    std::atomic<uint64_t>& head() noexcept {
        return *reinterpret_cast<std::atomic<uint64_t>*>(base_ + kOffHead);
    }
    std::atomic<uint64_t>& tail() noexcept {
        return *reinterpret_cast<std::atomic<uint64_t>*>(base_ + kOffTail);
    }
    std::atomic<uint64_t>& heartbeat() noexcept {
        return *reinterpret_cast<std::atomic<uint64_t>*>(base_ + kOffHeartbeat);
    }
    std::atomic<uint64_t>& pid_cell() noexcept {
        return *reinterpret_cast<std::atomic<uint64_t>*>(base_ + kOffPid);
    }
    std::atomic<uint64_t>& drops_cell() noexcept {
        return *reinterpret_cast<std::atomic<uint64_t>*>(base_ + kOffDrops);
    }
    std::atomic<uint32_t>& magic_cell() noexcept {
        return *reinterpret_cast<std::atomic<uint32_t>*>(base_ + kOffMagic);
    }
    const std::atomic<uint32_t>& const_magic() const noexcept {
        return *reinterpret_cast<const std::atomic<uint32_t>*>(base_ + kOffMagic);
    }
    const std::atomic<uint64_t>& const_head() const noexcept {
        return *reinterpret_cast<const std::atomic<uint64_t>*>(base_ + kOffHead);
    }
    const std::atomic<uint64_t>& const_tail() const noexcept {
        return *reinterpret_cast<const std::atomic<uint64_t>*>(base_ + kOffTail);
    }
    const std::atomic<uint64_t>& const_heartbeat() const noexcept {
        return *reinterpret_cast<const std::atomic<uint64_t>*>(base_ + kOffHeartbeat);
    }
    const std::atomic<uint64_t>& const_pid() const noexcept {
        return *reinterpret_cast<const std::atomic<uint64_t>*>(base_ + kOffPid);
    }
    const std::atomic<uint64_t>& const_drops() const noexcept {
        return *reinterpret_cast<const std::atomic<uint64_t>*>(base_ + kOffDrops);
    }

    uint8_t* slot_ptr(uint64_t index) noexcept {
        return base_ + kOffSlots + (index & mask_) * slot_stride_;
    }
    const uint8_t* slot_ptr(uint64_t index) const noexcept {
        return base_ + kOffSlots + (index & mask_) * slot_stride_;
    }

    void write_u32(uint64_t off, uint32_t v) noexcept {
        std::memcpy(base_ + off, &v, sizeof(v));
    }
    void write_u64(uint64_t off, uint64_t v) noexcept {
        std::memcpy(base_ + off, &v, sizeof(v));
    }
    uint32_t read_u32(uint64_t off) const noexcept {
        uint32_t v;
        std::memcpy(&v, base_ + off, sizeof(v));
        return v;
    }
    uint64_t read_u64(uint64_t off) const noexcept {
        uint64_t v;
        std::memcpy(&v, base_ + off, sizeof(v));
        return v;
    }
    static void write_slot_u32(uint8_t* slot, uint64_t off, uint32_t v) noexcept {
        std::memcpy(slot + off, &v, sizeof(v));
    }
    static uint32_t read_slot_u32(const uint8_t* slot, uint64_t off) noexcept {
        uint32_t v;
        std::memcpy(&v, slot + off, sizeof(v));
        return v;
    }

    uint8_t* base_ = nullptr;
    uint64_t map_size_ = 0;
    uint32_t capacity_ = 0;
    uint32_t slot_payload_ = 0;
    uint64_t slot_stride_ = 0;
    uint64_t mask_ = 0;
    Role role_ = Role::Consumer;
    bool open_ = false;
    char name_[128] = {};
};

}  // namespace exch
