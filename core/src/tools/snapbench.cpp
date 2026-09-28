// snapbench — measure snapshot serialize/parse/restore phases at N orders,
// and WAL prescan/replay cost on synthetic multi-segment journals.
// Phase-02.5 gate: recovery <10s; the 8h soak measured ~19.9s at ~1M orders.
//
// Usage:
//   snapbench book [n_orders] [levels]            serialize/restore timing
//   snapbench walgen <dir> <nseg> <entries/seg>   write synthetic segments
//   snapbench recover <wal_dir> <snap_dir> <n_orders> [tail_entries]
//                                               build a book, snapshot it at
//                                               the journal's covered cursor,
//                                               then run recover() timed
#include <chrono>
#include <cinttypes>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <vector>

#include "book/OrderBook.hpp"
#include "recovery/RecoveryManager.hpp"
#include "recovery/SnapshotStore.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/WalEntry.hpp"

using namespace exch;
using Clock = std::chrono::steady_clock;

static double ms(Clock::time_point a, Clock::time_point b) {
    return std::chrono::duration<double, std::milli>(b - a).count();
}

static constexpr uint32_t kIid = 7;

static int build_book(MemoryPool<Order>& pool, OrderBook& book,
                      std::size_t n, int levels) {
    for (std::size_t i = 0; i < n; ++i) {
        Order o{};
        o.id = i + 1;
        o.account_id = 1 + (i % 1000);
        o.side = (i & 1) ? Side::BUY : Side::SELL;
        o.type = OrderType::LIMIT;
        o.tif = TimeInForce::GTC;
        o.price_ticks = o.side == Side::BUY
                            ? 1'000'000 - (i / 2) % levels - 1
                            : 2'000'000 + (i / 2) % levels + 1;
        o.qty_units = 1000;
        o.timestamp_ns = i;
        o.ingress_seq = i;
        Order* out = nullptr;
        if (book.add_order(o, &out) != BookError::OK) {
            std::fprintf(stderr, "add_order failed at %zu\n", i);
            return 1;
        }
    }
    return 0;
}

static int bench_book(std::size_t n, int levels) {
    MemoryPool<Order> pool(n);
    OrderBook book(pool, n);

    auto t0 = Clock::now();
    if (build_book(pool, book, n, levels) != 0) return 1;
    auto t1 = Clock::now();
    std::printf("build %zu orders / %d levels: %.1f ms (%.0f ord/s)\n",
                n, levels, ms(t0, t1), n / (ms(t0, t1) / 1000.0));

    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    t0 = Clock::now();
    if (!SnapshotStore::serialize_book(book, kIid, n, blob, hdr)) {
        std::fprintf(stderr, "serialize_book failed\n");
        return 1;
    }
    t1 = Clock::now();
    std::printf("serialize: %.1f ms (%zu bytes)\n", ms(t0, t1), blob.size());

    ParsedSnapshot parsed;
    t0 = Clock::now();
    if (!SnapshotStore::parse_book(blob.data(), blob.size(), parsed)) {
        std::fprintf(stderr, "parse_book failed\n");
        return 1;
    }
    t1 = Clock::now();
    std::printf("parse: %.1f ms (%zu orders)\n", ms(t0, t1),
                parsed.orders.size());

    MemoryPool<Order> pool2(n);
    OrderBook book2(pool2, n);
    t0 = Clock::now();
    for (const auto& po : parsed.orders) {
        Order* out = nullptr;
        if (book2.add_order(po.tmpl, &out) != BookError::OK) {
            std::fprintf(stderr, "restore add_order failed\n");
            return 1;
        }
    }
    t1 = Clock::now();
    const double ins_ms = ms(t0, t1);
    std::printf("restore-insert: %.1f ms (%.0f ord/s)\n", ins_ms,
                parsed.orders.size() / (ins_ms / 1000.0));

    t0 = Clock::now();
    const char* violation = nullptr;
    const bool ok = book2.validate(&violation);
    t1 = Clock::now();
    std::printf("validate: %.1f ms (ok=%d %s)\n", ms(t0, t1), ok,
                violation ? violation : "");
    return ok ? 0 : 1;
}

// Write `nseg` segments of ORDER_CANCEL entries, contiguous seqs starting
// at `base_seq`. Segment files are named by first seq ({seq}.wal).
static int walgen(const char* dir, std::size_t nseg, std::size_t per_seg) {
    std::filesystem::create_directories(dir);
    std::vector<uint8_t> buf(1 << 20);
    uint64_t seq = 0;
    WalOrderCancelPayload p{};
    p.account_id = 1;
    p.reason = 0;
    for (std::size_t s = 0; s < nseg; ++s) {
        char name[64];
        // Engine segment naming is unpadded {seq}.wal (Wal::rotate uses
        // std::to_string) — match it so engine boots reopen the same file.
        std::snprintf(name, sizeof(name), "%s/%llu.wal", dir,
                      (unsigned long long)seq);
        FILE* f = std::fopen(name, "wb");
        if (f == nullptr) {
            std::perror("fopen");
            return 1;
        }
        WalFileHeader fh{kWalMagic, kWalVersion, 0};
        std::fwrite(&fh, sizeof(fh), 1, f);
        for (std::size_t i = 0; i < per_seg; ++i) {
            p.order_id = 10'000'000 + seq;
            const uint64_t n = wal_encode_entry(buf.data(), seq, seq,
                                                WalEventType::ORDER_CANCEL,
                                                &p, sizeof(p));
            std::fwrite(buf.data(), 1, n, f);
            ++seq;
        }
        std::fflush(f);
        std::fclose(f);
        if (s % 4 == 0)
            std::fprintf(stderr, "  seg %zu/%zu (seq=%llu)\n", s, nseg,
                         (unsigned long long)seq);
    }
    std::printf("walgen: %zu segs x %zu entries -> seq [0,%llu)\n", nseg,
                per_seg, (unsigned long long)seq);
    return 0;
}

static int recover_bench(const char* wal_dir, const char* snap_dir,
                         std::size_t n_orders, uint64_t tail_entries) {
    // Book + snapshot at the covered cursor = wal entries minus tail.
    // walgen wrote contiguous seqs [0, total); snapshot covers all but the
    // last `tail_entries`.
    // walgen writes fixed-size ORDER_CANCEL records named by first seq —
    // derive the journal tail from name + file size without scanning.
    uint64_t total = 0;
    for (const auto& de : std::filesystem::directory_iterator(wal_dir)) {
        if (de.path().extension() != ".wal") continue;
        const auto sz = de.file_size();
        const std::string stem = de.path().stem().string();
        const uint64_t base = std::strtoull(stem.c_str(), nullptr, 10);
        const uint64_t rec = sizeof(WalEntryHeader) +
                             sizeof(WalOrderCancelPayload) + sizeof(uint32_t);
        const uint64_t cnt = (sz - sizeof(WalFileHeader)) / rec;
        if (base + cnt > total) total = base + cnt;
    }
    const uint64_t snap_seq = total > tail_entries ? total - tail_entries : 0;

    FileSnapshotSink sink(snap_dir, 0, 2);
    MemoryPool<Order> pool(n_orders + tail_entries + 64);
    OrderBook book(pool, n_orders + tail_entries + 64);
    {
        auto t0 = Clock::now();
        if (build_book(pool, book, n_orders, 4000) != 0) return 1;
        std::vector<uint8_t> blob;
        WalBookSnapshotHeader hdr{};
        if (!SnapshotStore::serialize_book(book, kIid, snap_seq, blob, hdr) ||
            !sink.store(kIid, snap_seq, blob.data(), blob.size())) {
            std::fprintf(stderr, "snapshot store failed\n");
            return 1;
        }
        auto t1 = Clock::now();
        std::printf("snapshot store: %.1f ms (seq=%llu, %zu bytes)\n",
                    ms(t0, t1), (unsigned long long)snap_seq, blob.size());
    }
    // Fresh destination book for recovery (recovered in place).
    MemoryPool<Order> pool2(n_orders + tail_entries + 64);
    OrderBook dst(pool2, n_orders + tail_entries + 64);
    RecoveryManager rm(0, sink);

    auto t0 = Clock::now();
    const RecoveryResult res =
        rm.recover(wal_dir, {{kIid, &dst, &pool2}});
    auto t1 = Clock::now();
    std::printf("recover(): %.1f ms  status=%s wal_tail=%llu "
                "entries=%llu covered_skips=%llu skipped_segs=%llu "
                "replayed=%llu dedup=%llu detail=\"%s\"\n",
                ms(t0, t1), recovery_status_str(res.status),
                (unsigned long long)res.wal_tail,
                (unsigned long long)res.wal_entries,
                (unsigned long long)res.covered_skips,
                (unsigned long long)res.covered_segments_skipped,
                (unsigned long long)res.entries_replayed,
                (unsigned long long)res.dedup_skips, res.detail);
    if (!res.books.empty()) {
        const auto& b = res.books[0];
        std::printf("  book: restored=%llu consumed=%llu cursor_ok=%d\n",
                    (unsigned long long)b.orders_restored,
                    (unsigned long long)b.entries_consumed,
                    b.book_seq_verified);
    }
    return res.ok() ? 0 : 1;
}

int main(int argc, char** argv) {
    const std::string mode = argc > 1 ? argv[1] : "book";
    if (mode == "book") {
        const std::size_t n =
            argc > 2 ? std::strtoull(argv[2], nullptr, 10) : 1'000'000;
        const int levels = argc > 3 ? std::atoi(argv[3]) : 4000;
        return bench_book(n, levels);
    }
    if (mode == "walgen") {
        if (argc < 5) {
            std::fprintf(stderr, "usage: snapbench walgen <dir> <nseg> "
                                 "<entries_per_seg>\n");
            return 2;
        }
        return walgen(argv[2], std::strtoull(argv[3], nullptr, 10),
                      std::strtoull(argv[4], nullptr, 10));
    }
    if (mode == "recover") {
        if (argc < 5) {
            std::fprintf(stderr, "usage: snapbench recover <wal_dir> "
                                 "<snap_dir> <n_orders> [tail_entries]\n");
            return 2;
        }
        return recover_bench(argv[2], argv[3],
                             std::strtoull(argv[4], nullptr, 10),
                             argc > 5 ? std::strtoull(argv[5], nullptr, 10)
                                      : 1000);
    }
    std::fprintf(stderr, "unknown mode %s\n", mode.c_str());
    return 2;
}
