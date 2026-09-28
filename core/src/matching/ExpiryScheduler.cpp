// Task 2.3.10 — Time-in-Force expiry scheduler (spec §5.4, §6.7).
// Flat min-heap + open-addressed id index; see ExpiryScheduler.hpp for the
// DAY-expiry rule, the deterministic-TIME_TICK contract, and the snapshot
// wire format. No system-clock reads, no heap allocation on any path after
// construction.

#include "matching/ExpiryScheduler.hpp"

#include <cstring>
#include <new>

namespace exch {

namespace {

// Session calendar constants (spec §6.7; all UTC, all in nanoseconds).
constexpr uint64_t kNsPerSec = 1'000'000'000ull;
constexpr uint64_t kNsPerHour = 3'600ull * kNsPerSec;
constexpr uint64_t kNsPerDay = 24ull * kNsPerHour;
constexpr uint64_t kOpenRemNs = 21ull * kNsPerHour;   // Sunday open 21:00 UTC
constexpr uint64_t kCloseRemNs = 22ull * kNsPerHour;  // NY close boundary 22:00

// Weekday index: 1970-01-01 (day 0) was a Thursday.
//   wd = (days_since_epoch + 4) % 7  →  0=Sun, 1=Mon, …, 5=Fri, 6=Sat.
constexpr uint64_t kWdSunday = 0;
constexpr uint64_t kWdFriday = 5;
constexpr uint64_t kWdSaturday = 6;

// Smallest power of two >= n (n >= 1 expected).
[[nodiscard]] std::size_t next_pow2(std::size_t n) noexcept {
    std::size_t p = 1;
    while (p < n) p <<= 1;
    return p;
}

// Snapshot record helpers — unaligned-safe via memcpy (buf alignment is the
// caller's, so field stores go through memcpy like the WAL codec does).
struct SnapRec {
    uint64_t expiry_ns;
    uint64_t order_id;
};

[[nodiscard]] inline SnapRec rec_load(const uint8_t* p) noexcept {
    SnapRec r;
    std::memcpy(&r, p, sizeof(r));
    return r;
}
inline void rec_store(uint8_t* p, const SnapRec& r) noexcept { std::memcpy(p, &r, sizeof(r)); }

// In-place heapsort over the record region of a snapshot buffer —
// descending sift so ascending order results. Zero allocation; the records
// being sorted live in the caller's buffer, not the scheduler's.
void rec_sort(uint8_t* base, uint32_t n) noexcept {
    auto less = [](const SnapRec& a, const SnapRec& b) noexcept {
        return a.expiry_ns < b.expiry_ns || (a.expiry_ns == b.expiry_ns && a.order_id < b.order_id);
    };
    auto swap_rec = [](uint8_t* a, uint8_t* b) noexcept {
        SnapRec t = rec_load(a);
        rec_store(a, rec_load(b));
        rec_store(b, t);
    };
    // Build max-heap.
    for (uint32_t start = n / 2; start-- > 0;) {
        uint32_t i = start;
        for (;;) {
            uint32_t c = 2 * i + 1;
            if (c >= n) break;
            if (c + 1 < n && less(rec_load(base + c * 16u), rec_load(base + (c + 1) * 16u))) ++c;
            if (!less(rec_load(base + i * 16u), rec_load(base + c * 16u))) break;
            swap_rec(base + i * 16u, base + c * 16u);
            i = c;
        }
    }
    // Extract max to the tail — ascending order left behind.
    for (uint32_t end = n; end-- > 1;) {
        swap_rec(base, base + end * 16u);
        uint32_t i = 0;
        for (;;) {
            uint32_t c = 2 * i + 1;
            if (c >= end) break;
            if (c + 1 < end && less(rec_load(base + c * 16u), rec_load(base + (c + 1) * 16u))) ++c;
            if (!less(rec_load(base + i * 16u), rec_load(base + c * 16u))) break;
            swap_rec(base + i * 16u, base + c * 16u);
            i = c;
        }
    }
}

[[nodiscard]] inline uint32_t load32(const uint8_t* p) noexcept {
    uint32_t v;
    std::memcpy(&v, p, sizeof(v));
    return v;
}
[[nodiscard]] inline uint64_t load64(const uint8_t* p) noexcept {
    uint64_t v;
    std::memcpy(&v, p, sizeof(v));
    return v;
}
inline void store32(uint8_t* p, uint32_t v) noexcept { std::memcpy(p, &v, sizeof(v)); }
inline void store64(uint8_t* p, uint64_t v) noexcept { std::memcpy(p, &v, sizeof(v)); }

}  // namespace

// --- lifecycle ---------------------------------------------------------------

ExpiryScheduler::ExpiryScheduler(std::size_t capacity) noexcept : heap_cap_(capacity) {
    // Cold-path allocation — nothrow; failure leaves a fail-closed empty
    // scheduler (nothing registers, register_overflows_ counts refusals).
    // Capacity < 2^32 keeps every heap index strictly below kEmptySlot.
    if (capacity == 0 || capacity >= (std::size_t{1} << 32)) return;
    heap_ = static_cast<Node*>(
        ::operator new[](capacity * sizeof(Node), std::align_val_t{alignof(Node)}, std::nothrow));
    if (heap_ == nullptr) {
        heap_cap_ = 0;
        return;
    }
    // Id index sized to ~2x the heap bound (load <= 50% keeps linear probes
    // short); clamped to the u32 indexable range by the capacity guard.
    const std::size_t buckets = next_pow2(capacity * 2);
    table_ = static_cast<Slot*>(
        ::operator new[](buckets * sizeof(Slot), std::align_val_t{alignof(Slot)}, std::nothrow));
    if (table_ == nullptr) {
        ::operator delete[](heap_, std::align_val_t{alignof(Node)});
        heap_ = nullptr;
        heap_cap_ = 0;
        return;
    }
    table_mask_ = buckets - 1;
    for (std::size_t i = 0; i < buckets; ++i) table_[i].heap_idx = kEmptySlot;
}

ExpiryScheduler::~ExpiryScheduler() {
    ::operator delete[](heap_, std::align_val_t{alignof(Node)});
    ::operator delete[](table_, std::align_val_t{alignof(Slot)});
}

// --- calendar ----------------------------------------------------------------

bool ExpiryScheduler::is_session_open(uint64_t now_ns) noexcept {
    const uint64_t days = now_ns / kNsPerDay;
    const uint64_t rem = now_ns - days * kNsPerDay;
    const uint64_t wd = (days + 4) % 7;  // 0=Sun … 5=Fri, 6=Sat
    switch (wd) {
        case kWdSunday:
            return rem >= kOpenRemNs;  // 21:00 → 24:00 open
        case kWdSaturday:
            return false;  // full close
        case kWdFriday:
            return rem < kCloseRemNs;  // open until 22:00
        default:
            return true;  // Mon–Thu: open all day
    }
}

uint64_t ExpiryScheduler::day_expiry_ns(uint64_t now_ns) noexcept {
    const uint64_t days = now_ns / kNsPerDay;
    const uint64_t rem = now_ns - days * kNsPerDay;
    const uint64_t wd = (days + 4) % 7;

    if (is_session_open(now_ns)) {
        // Next in-session 22:00 UTC boundary strictly after now. When the
        // accept precedes today's boundary it expires today; an accept at or
        // after today's boundary belongs to the next day (Sun→Mon, Mon→Tue,
        // …, Thu→Fri — the following day's 22:00 is always in-session).
        const uint64_t d = (rem < kCloseRemNs) ? days : days + 1;
        return d * kNsPerDay + kCloseRemNs;
    }

    // Weekend gap [Fri 22:00, Sun 21:00): the order joins the upcoming trading
    // week and expires at that week's Friday 22:00 UTC close — the first
    // Friday boundary strictly after now (spec §6.7 "expire at 22:00 UTC
    // Friday").
    const uint64_t delta = (kWdFriday + 7 - wd) % 7;  // days to coming Friday
    uint64_t exp = (days + delta) * kNsPerDay + kCloseRemNs;
    if (exp <= now_ns) exp += 7 * kNsPerDay;  // Fri accept at/after 22:00 → +1 week
    return exp;
}

// --- id index (pow2 open addressing, linear probe, backward-shift delete) ----

uint64_t ExpiryScheduler::mix(uint64_t v) noexcept {
    // splitmix64 finalizer — deterministic, platform-independent mixing.
    v ^= v >> 30;
    v *= 0xBF58476D1CE4E5B9ull;
    v ^= v >> 27;
    v *= 0x94D049BB133111EBull;
    v ^= v >> 31;
    return v;
}

std::size_t ExpiryScheduler::map_find(uint64_t order_id) const noexcept {
    if (table_ == nullptr) return kNoSlot;
    std::size_t i = static_cast<std::size_t>(mix(order_id)) & table_mask_;
    for (;;) {
        if (table_[i].heap_idx == kEmptySlot) return kNoSlot;
        if (table_[i].order_id == order_id) return i;
        i = (i + 1) & table_mask_;
    }
    // Unreachable while load < 100% — guaranteed by ~2x sizing.
}

void ExpiryScheduler::map_put(uint64_t order_id, uint32_t heap_idx) noexcept {
    std::size_t i = static_cast<std::size_t>(mix(order_id)) & table_mask_;
    for (;;) {
        if (table_[i].heap_idx == kEmptySlot) {
            table_[i].order_id = order_id;
            table_[i].heap_idx = heap_idx;
            return;
        }
        if (table_[i].order_id == order_id) {  // unreachable post-erase
            table_[i].heap_idx = heap_idx;
            return;
        }
        i = (i + 1) & table_mask_;
    }
}

void ExpiryScheduler::map_erase_at(std::size_t idx) noexcept {
    // Backward-shift deletion: fill the hole with any entry whose probe
    // chain crosses it — keeps chains intact without tombstones, preserving
    // deterministic probe order.
    table_[idx].heap_idx = kEmptySlot;
    std::size_t i = idx;
    for (;;) {
        std::size_t j = i;
        for (;;) {
            j = (j + 1) & table_mask_;
            if (table_[j].heap_idx == kEmptySlot) return;
            const std::size_t h = static_cast<std::size_t>(mix(table_[j].order_id)) & table_mask_;
            // Entry at j shifts into hole i iff its home h is NOT in the
            // cyclic interval (i, j] — i.e., it probed past the hole.
            const bool in_span = (i < j) ? (h > i && h <= j) : (h > i || h <= j);
            if (!in_span) break;
        }
        table_[i] = table_[j];
        table_[j].heap_idx = kEmptySlot;
        i = j;
    }
}

void ExpiryScheduler::map_repoint(uint64_t order_id, uint32_t heap_idx) noexcept {
    const std::size_t t = map_find(order_id);
    if (t != kNoSlot) table_[t].heap_idx = heap_idx;
}

// --- heap core -----------------------------------------------------------------

void ExpiryScheduler::swap_nodes(std::size_t i, std::size_t j) noexcept {
    const Node tmp = heap_[i];
    heap_[i] = heap_[j];
    heap_[j] = tmp;
    map_repoint(heap_[i].order_id, static_cast<uint32_t>(i));
    map_repoint(heap_[j].order_id, static_cast<uint32_t>(j));
}

void ExpiryScheduler::sift_up(std::size_t i) noexcept {
    while (i > 0) {
        const std::size_t p = (i - 1) / 2;
        if (!node_less(heap_[i], heap_[p])) break;
        swap_nodes(i, p);
        i = p;
    }
}

void ExpiryScheduler::sift_down(std::size_t i) noexcept {
    for (;;) {
        const std::size_t l = 2 * i + 1;
        const std::size_t r = l + 1;
        std::size_t m = i;
        if (l < heap_size_ && node_less(heap_[l], heap_[m])) m = l;
        if (r < heap_size_ && node_less(heap_[r], heap_[m])) m = r;
        if (m == i) break;
        swap_nodes(i, m);
        i = m;
    }
}

void ExpiryScheduler::remove_at(std::size_t i) noexcept {
    const std::size_t t = map_find(heap_[i].order_id);
    if (t != kNoSlot) map_erase_at(t);
    --heap_size_;
    if (i != heap_size_) {
        heap_[i] = heap_[heap_size_];
        map_repoint(heap_[i].order_id, static_cast<uint32_t>(i));
        // The moved node may need to rise (smaller than its new parent) or
        // sink — check the parent first, else restore downward.
        if (i > 0 && node_less(heap_[i], heap_[(i - 1) / 2])) {
            sift_up(i);
        } else {
            sift_down(i);
        }
    }
}

void ExpiryScheduler::clear() noexcept {
    heap_size_ = 0;
    if (table_ != nullptr) {
        for (std::size_t i = 0; i <= table_mask_; ++i) table_[i].heap_idx = kEmptySlot;
    }
}

// --- public hot path -----------------------------------------------------------

void ExpiryScheduler::on_order_accepted(uint64_t order_id, uint8_t tif, int64_t gtd_expiry_ns,
                                        uint64_t now_ns) noexcept {
    uint64_t expiry;
    if (tif == static_cast<uint8_t>(TimeInForce::GTD)) {
        // gtd_expiry_ns <= 0 clamps to 0 → fires on the next tick
        // (fail-closed; INVALID_EXPIRY_TIMESTAMP is the upstream reject).
        expiry = (gtd_expiry_ns > 0) ? static_cast<uint64_t>(gtd_expiry_ns) : 0;
    } else if (tif == static_cast<uint8_t>(TimeInForce::DAY)) {
        expiry = day_expiry_ns(now_ns);
    } else {
        return;  // GTC / IOC / FOK — no scheduler involvement
    }

    // Amend-reset semantics (spec §6.6a): a re-registration replaces the
    // pending expiry instead of stacking a second timer on the same id.
    const std::size_t t = map_find(order_id);
    if (t != kNoSlot) remove_at(table_[t].heap_idx);

    if (heap_size_ >= heap_cap_) {
        ++register_overflows_;  // unreachable at default capacity
        return;
    }
    const std::size_t i = heap_size_++;
    heap_[i] = Node{expiry, order_id};
    map_put(order_id, static_cast<uint32_t>(i));
    sift_up(i);
}

void ExpiryScheduler::on_order_done(uint64_t order_id) noexcept {
    const std::size_t t = map_find(order_id);
    if (t == kNoSlot) return;  // never registered — no-op
    remove_at(table_[t].heap_idx);
}

uint32_t ExpiryScheduler::on_time_tick(uint64_t tick_ns, uint64_t* out_order_ids,
                                       uint32_t cap) noexcept {
    if (out_order_ids == nullptr || cap == 0) return 0;
    uint32_t n = 0;
    // Min-heap root is the earliest pending expiry; each pop preserves the
    // invariant so the drain emits ascending (expiry_ns, order_id). Expiries
    // beyond `cap` stay pending for the next tick.
    while (heap_size_ > 0 && heap_[0].expiry_ns <= tick_ns && n < cap) {
        out_order_ids[n++] = heap_[0].order_id;
        remove_at(0);
    }
    return n;
}

uint64_t ExpiryScheduler::next_expiry_ns() const noexcept {
    return heap_size_ > 0 ? heap_[0].expiry_ns : UINT64_MAX;
}

uint64_t ExpiryScheduler::pending_expiry_of(uint64_t order_id) const noexcept {
    const std::size_t t = map_find(order_id);
    if (t == kNoSlot) return UINT64_MAX;
    return heap_[table_[t].heap_idx].expiry_ns;
}

// --- snapshot / restore (cold path) --------------------------------------------

uint32_t ExpiryScheduler::snapshot_bytes_needed() const noexcept {
    // Saturate: a pending set beyond the u32 buffer-length domain is
    // unsnapshotable — cap reports UINT32_MAX and snapshot_pending writes 0.
    const uint64_t need = static_cast<uint64_t>(kSnapshotHeaderBytes) +
                          static_cast<uint64_t>(heap_size_) * kSnapshotRecordBytes;
    return need > UINT32_MAX ? UINT32_MAX : static_cast<uint32_t>(need);
}

uint32_t ExpiryScheduler::snapshot_pending(uint8_t* buf, uint32_t cap) const noexcept {
    const uint64_t true_need = static_cast<uint64_t>(kSnapshotHeaderBytes) +
                               static_cast<uint64_t>(heap_size_) * kSnapshotRecordBytes;
    if (true_need > UINT32_MAX) return 0;  // pending set exceeds u32 image domain
    const uint32_t need = static_cast<uint32_t>(true_need);
    if (buf == nullptr || cap < need) return 0;
    store32(buf, kSnapshotMagic);
    store32(buf + 4, kSnapshotVersion);
    store32(buf + 8, static_cast<uint32_t>(heap_size_));
    store32(buf + 12, 0);
    store64(buf + 16, 0);  // reserved
    uint8_t* rec = buf + kSnapshotHeaderBytes;
    for (std::size_t i = 0; i < heap_size_; ++i) {
        rec_store(rec + i * kSnapshotRecordBytes, SnapRec{heap_[i].expiry_ns, heap_[i].order_id});
    }
    rec_sort(rec, static_cast<uint32_t>(heap_size_));
    return need;
}

bool ExpiryScheduler::restore_pending(const uint8_t* buf, uint32_t len) noexcept {
    if (buf == nullptr || len < kSnapshotHeaderBytes) {
        clear();
        return false;
    }
    if (load32(buf) != kSnapshotMagic || load32(buf + 4) != kSnapshotVersion) {
        clear();
        return false;
    }
    const uint32_t count = load32(buf + 8);
    const uint64_t want = static_cast<uint64_t>(kSnapshotHeaderBytes) +
                          static_cast<uint64_t>(count) * kSnapshotRecordBytes;
    if (static_cast<uint64_t>(count) > heap_cap_ ||
        static_cast<uint64_t>(len) != want) {
        clear();
        return false;
    }
    clear();
    const uint8_t* rec = buf + kSnapshotHeaderBytes;
    for (uint32_t i = 0; i < count; ++i) {
        const SnapRec r = rec_load(rec + i * kSnapshotRecordBytes);
        if (map_find(r.order_id) != kNoSlot) {
            clear();  // corrupt image (duplicate id) — fail closed
            return false;
        }
        const std::size_t idx = heap_size_++;
        heap_[idx] = Node{r.expiry_ns, r.order_id};
        map_put(r.order_id, static_cast<uint32_t>(idx));
        sift_up(idx);
    }
    return true;
}

}  // namespace exch
