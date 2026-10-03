// PHASE-02 TASK-2.3.4 — snapshot cadence + FileSnapshotSink (spec §3.5).

#include "recovery/SnapshotStore.hpp"

#include <algorithm>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <system_error>

#include <fcntl.h>
#include <unistd.h>

namespace exch {
namespace {

// fsync a directory so a rename/create survives a crash (same discipline as
// Wal::fsync_dir — kept local to avoid touching the wal/ translation unit).
void fsync_dir(const std::string& dir) noexcept {
    const int fd = ::open(dir.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC);
    if (fd < 0) return;
    ::fsync(fd);
    ::close(fd);
}

// Write-all loop; returns bytes written or -1.
ssize_t write_all(int fd, const uint8_t* p, uint64_t n) noexcept {
    uint64_t off = 0;
    while (off < n) {
        const ssize_t w =
            ::write(fd, p + off, static_cast<std::size_t>(n - off));
        if (w < 0) {
            if (errno == EINTR) continue;
            return -1;
        }
        off += static_cast<uint64_t>(w);
    }
    return static_cast<ssize_t>(off);
}

// Canonical snapshot filename check: exactly `snap_{seq:020}.bin` — sscanf
// alone would partially match `snap_X.bin.tmp-<pid>` crash remnants, which a
// load path must NEVER select (a torn newest tmp would shadow a valid older
// snapshot). Returns the parsed seq via *seq_out.
bool snap_file_seq(const std::string& name, uint64_t* seq_out) noexcept {
    uint64_t fseq = 0;
    if (std::sscanf(name.c_str(), "snap_%llu.bin",
                    reinterpret_cast<unsigned long long*>(&fseq)) != 1) {
        return false;
    }
    char want[64];
    std::snprintf(want, sizeof(want), "snap_%020llu.bin",
                  static_cast<unsigned long long>(fseq));
    if (name != want) return false;  // non-canonical: tmp, truncation, junk
    *seq_out = fseq;
    return true;
}

}  // namespace

const char* snapshot_outcome_str(SnapshotOutcome o) noexcept {
    switch (o) {
        case SnapshotOutcome::Skipped:         return "Skipped";
        case SnapshotOutcome::Taken:           return "Taken";
        case SnapshotOutcome::SerializeFailed: return "SerializeFailed";
        case SnapshotOutcome::StoreFailed:     return "StoreFailed";
    }
    return "?";
}

// --- FileSnapshotSink ----------------------------------------------------------

FileSnapshotSink::FileSnapshotSink(std::string root, uint16_t shard_id,
                                   uint32_t keep_per_instrument) noexcept
    : root_(std::move(root)),
      shard_id_(shard_id),
      keep_(keep_per_instrument == 0 ? 1 : keep_per_instrument) {}

std::string FileSnapshotSink::dir_for(uint32_t instrument_id) const {
    return root_ + "/i" + std::to_string(instrument_id);
}

std::string FileSnapshotSink::file_for(uint32_t instrument_id,
                                       uint64_t seq) const {
    char name[64];
    std::snprintf(name, sizeof(name), "snap_%020llu.bin",
                  static_cast<unsigned long long>(seq));
    return dir_for(instrument_id) + "/" + name;
}

bool FileSnapshotSink::store(uint32_t instrument_id, uint64_t seq,
                             const uint8_t* blob, uint64_t len) noexcept {
    last_errno_ = 0;
    if (blob == nullptr || len < sizeof(WalBookSnapshotHeader) ||
        len > kSnapMaxPayload) {
        return false;
    }

    std::error_code ec;
    const std::string dir = dir_for(instrument_id);
    std::filesystem::create_directories(dir, ec);
    if (ec) return false;

    SnapFileHeader fh{};
    fh.magic = kSnapFileMagic;
    fh.version = kSnapFileVersion;
    fh.shard_id = shard_id_;
    fh.seq = seq;
    fh.instrument_id = instrument_id;
    fh.payload_len = static_cast<uint32_t>(len);
    fh.payload_crc = wal_crc32c(blob, static_cast<std::size_t>(len));

    const std::string final_path = file_for(instrument_id, seq);
    char tmp_name[80];
    std::snprintf(tmp_name, sizeof(tmp_name), "snap_%020llu.bin.tmp-%d",
                  static_cast<unsigned long long>(seq),
                  static_cast<int>(::getpid()));
    const std::string tmp_path = dir + "/" + tmp_name;

    const int fd =
        ::open(tmp_path.c_str(), O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC,
               0644);
    if (fd < 0) {
        last_errno_ = errno;
        return false;
    }
    bool ok = write_all(fd, reinterpret_cast<const uint8_t*>(&fh),
                        sizeof(fh)) == static_cast<ssize_t>(sizeof(fh)) &&
              write_all(fd, blob, len) == static_cast<ssize_t>(len);
    if (ok && ::fsync(fd) != 0) ok = false;
    if (!ok) last_errno_ = errno;
    ::close(fd);
    if (!ok) {
        std::error_code rec;
        std::filesystem::remove(tmp_path, rec);  // never leave a torn file
        return false;
    }
    if (::rename(tmp_path.c_str(), final_path.c_str()) != 0) {
        last_errno_ = errno;
        std::error_code rec;
        std::filesystem::remove(tmp_path, rec);
        return false;
    }
    fsync_dir(dir);  // the rename itself must be durable

    // Retention: drop all but the newest `keep_` snapshots for this
    // instrument; also sweep stale snap_*.tmp crash remnants.
    std::vector<std::pair<uint64_t, std::string>> found;
    for (const auto& de : std::filesystem::directory_iterator(dir, ec)) {
        if (ec) break;
        std::error_code tec;
        if (!de.is_regular_file(tec) || tec) continue;
        const std::string name = de.path().filename().string();
        uint64_t fseq = 0;
        if (snap_file_seq(name, &fseq)) {
            found.emplace_back(fseq, de.path().string());
        } else if (name.rfind("snap_", 0) == 0) {
            std::error_code rec;
            std::filesystem::remove(de.path(), rec);  // stale tmp remnant
        }
    }
    if (found.size() > keep_) {
        std::sort(found.begin(), found.end(),
                  [](const auto& a, const auto& b) { return a.first > b.first; });
        for (std::size_t i = keep_; i < found.size(); ++i) {
            std::error_code rec;
            std::filesystem::remove(found[i].second, rec);
        }
    }
    return true;
}

// Shared loader: `rank` 0 = newest snapshot, 1 = previous generation
// (Task 4.3.5 rebase fallback — keep_>=2 retains it). Same fail-closed
// integrity discipline as load_latest: header cross-checks, exact size,
// full-payload CRC32C (the payload checksum IS the divergence guard —
// see the design note on load_latest).
bool FileSnapshotSink::load_nth(uint32_t instrument_id, uint32_t rank,
                                uint64_t* seq_out,
                                std::vector<uint8_t>* blob_out,
                                bool* has_snapshot) noexcept {
    last_errno_ = 0;
    *has_snapshot = false;
    *seq_out = 0;
    blob_out->clear();

    const std::string dir = dir_for(instrument_id);
    std::error_code ec;
    if (!std::filesystem::exists(dir, ec)) {
        return !ec;  // no directory = clean cold start
    }

    // Ranked by seq in the snap_*.bin name (the rename target — tmp files are
    // never candidates).
    std::vector<std::pair<uint64_t, std::string>> found;
    for (const auto& de : std::filesystem::directory_iterator(dir, ec)) {
        if (ec) return false;
        std::error_code tec;
        if (!de.is_regular_file(tec) || tec) continue;
        const std::string name = de.path().filename().string();
        uint64_t fseq = 0;
        if (!snap_file_seq(name, &fseq)) continue;
        found.emplace_back(fseq, de.path().string());
    }
    if (found.size() <= rank) return true;  // fewer generations than asked
    std::sort(found.begin(), found.end(),
              [](const auto& a, const auto& b) { return a.first > b.first; });
    const uint64_t best_seq = found[rank].first;
    const std::string best_path = found[rank].second;

    const int fd = ::open(best_path.c_str(), O_RDONLY | O_CLOEXEC);
    if (fd < 0) {
        last_errno_ = errno;
        return false;
    }
    SnapFileHeader fh{};
    ssize_t r = ::read(fd, &fh, sizeof(fh));
    if (r != static_cast<ssize_t>(sizeof(fh))) {
        ::close(fd);
        last_errno_ = (r < 0) ? errno : EIO;
        return false;  // torn header — must never be visible, so this is bad
    }
    if (fh.magic != kSnapFileMagic || fh.version != kSnapFileVersion ||
        fh.instrument_id != instrument_id || fh.seq != best_seq ||
        fh.payload_len > kSnapMaxPayload) {
        ::close(fd);
        return false;  // foreign/corrupt/filename-vs-header mismatch
    }
    // Exact-size cross-check BEFORE the allocation: the file must be
    // precisely header + payload (no trailer is written). A corrupt
    // payload_len is rejected here without touching memory.
    std::error_code szec;
    const auto file_sz = std::filesystem::file_size(best_path, szec);
    if (szec || file_sz != sizeof(fh) + fh.payload_len) {
        ::close(fd);
        return false;
    }
    try {
        blob_out->resize(fh.payload_len);
    } catch (...) {
        ::close(fd);
        return false;  // noexcept contract — bad_alloc must not escape
    }
    uint64_t off = 0;
    while (off < fh.payload_len) {
        const ssize_t n = ::read(fd, blob_out->data() + off,
                                 static_cast<std::size_t>(fh.payload_len - off));
        if (n <= 0) {
            ::close(fd);
            blob_out->clear();
            last_errno_ = (n < 0) ? errno : EIO;
            return false;
        }
        off += static_cast<uint64_t>(n);
    }
    uint8_t trailer = 0;
    const ssize_t extra = ::read(fd, &trailer, 1);
    ::close(fd);
    if (extra != 0) {
        blob_out->clear();
        return false;  // trailing bytes = corrupt file
    }
    if (wal_crc32c(blob_out->data(), blob_out->size()) != fh.payload_crc) {
        blob_out->clear();
        return false;
    }
    *seq_out = fh.seq;
    *has_snapshot = true;
    return true;
}

bool FileSnapshotSink::load_latest(uint32_t instrument_id, uint64_t* seq_out,
                                   std::vector<uint8_t>* blob_out,
                                   bool* has_snapshot) noexcept {
    return load_nth(instrument_id, 0, seq_out, blob_out, has_snapshot);
}

bool FileSnapshotSink::load_prior(uint32_t instrument_id, uint64_t* seq_out,
                                  std::vector<uint8_t>* blob_out,
                                  bool* has_prior) noexcept {
    return load_nth(instrument_id, 1, seq_out, blob_out, has_prior);
}

// --- SnapshotStore: serialize ---------------------------------------------------

namespace {

// Append a POD record to the blob (little-endian host, packed — same wire
// discipline as the WAL contract).
template <typename T>
void blob_put(std::vector<uint8_t>& b, const T& v) {
    const uint8_t* p = reinterpret_cast<const uint8_t*>(&v);
    b.insert(b.end(), p, p + sizeof(T));
}

}  // namespace

void SnapshotStore::order_from_rows(const WalSnapshotOrder& ord,
                                    const WalSnapshotOrderExt& ext,
                                    Order& out) noexcept {
    out = Order{};
    out.id = ord.order_id;
    out.account_id = ord.account_id;
    out.side = static_cast<Side>(ext.side);
    out.type = static_cast<OrderType>(ext.type);
    out.tif = static_cast<TimeInForce>(ord.tif);
    out.stp_mode = static_cast<StpMode>(ord.stp_mode);
    out.flags = ext.flags;
    out.price_ticks = ext.price_ticks;
    out.qty_units = ext.qty_units;
    out.filled_qty_units = ext.filled_qty_units;
    out.display_qty_units = ord.visible_qty_units;
    out.quantity = Decimal::from_mantissa(ext.qty_units);  // compat mirror
    out.timestamp_ns = ext.timestamp_ns;
    out.ingress_seq = ext.ingress_seq;
}

void SnapshotStore::rows_from_order(const Order& o, WalSnapshotOrder& ord,
                                    WalSnapshotOrderExt& ext) noexcept {
    ord = WalSnapshotOrder{};
    ord.order_id = o.id;
    ord.account_id = o.account_id;
    ord.qty_units = remaining_qty_units(o);  // pinned = remaining
    ord.visible_qty_units = o.display_qty_units;
    ord.stp_mode = static_cast<uint32_t>(o.stp_mode);
    ord.tif = static_cast<uint8_t>(o.tif);
    ext = WalSnapshotOrderExt{};
    ext.order_id = o.id;
    ext.qty_units = o.qty_units;             // ext = original total
    ext.filled_qty_units = o.filled_qty_units;
    ext.price_ticks = o.price_ticks;
    ext.timestamp_ns = o.timestamp_ns;
    ext.ingress_seq = o.ingress_seq;
    ext.type = static_cast<uint8_t>(o.type);
    ext.side = static_cast<uint8_t>(o.side);
    ext.flags = o.flags;
}

bool SnapshotStore::serialize_book(const OrderBook& book,
                                   uint32_t instrument_id, uint64_t wal_seq,
                                   std::vector<uint8_t>& out,
                                   WalBookSnapshotHeader& header_out,
                                   uint64_t next_trade_id,
                                   const SnapshotAuxState* aux) noexcept {
    try {
        out.clear();
        // Levels: bids descending then asks ascending (pinned contract order).
        const uint32_t nb = book.bid_count();
        const uint32_t na = book.ask_count();
        const uint32_t level_count = nb + na;
        uint64_t order_count = 0;
        for (uint32_t i = 0; i < nb; ++i) {
            const PriceLevel* l = book.level(Side::BUY, i);
            if (l == nullptr) return false;
            order_count += l->order_count;
        }
        for (uint32_t i = 0; i < na; ++i) {
            const PriceLevel* l = book.level(Side::SELL, i);
            if (l == nullptr) return false;
            order_count += l->order_count;
        }

        header_out.instrument_id = instrument_id;
        header_out.level_count = level_count;
        header_out.order_count = order_count;
        header_out.book_seq = wal_seq;  // WAL cursor covered (see header doc)

        // Aux rows join the blob only when the engine exports them — nullptr
        // keeps the v2 shape for book-only callers (tests, fingerprinting).
        const uint64_t aux_meta = aux != nullptr ? aux->metas.size() : 0;
        const uint64_t aux_pend = aux != nullptr ? aux->pendings.size() : 0;
        const uint64_t aux_ice  = aux != nullptr ? aux->icebergs.size() : 0;
        const uint64_t aux_oco  = aux != nullptr ? aux->ocos.size() : 0;
        const uint64_t aux_peg  = aux != nullptr ? aux->pegs.size() : 0;
        const uint64_t aux_bytes =
            aux != nullptr
                ? sizeof(WalSnapshotAuxHeader) +
                  sizeof(WalSnapshotOrderAux) * aux_meta +
                  sizeof(WalSnapshotPendingOrder) * aux_pend +
                  sizeof(WalSnapshotIceberg) * aux_ice +
                  sizeof(WalSnapshotOco) * aux_oco +
                  sizeof(WalSnapshotPeg) * aux_peg
                : 0;

        const uint64_t total =
            sizeof(WalBookSnapshotHeader) +
            sizeof(WalSnapshotLevel) * level_count +
            sizeof(WalSnapshotOrder) * order_count +
            sizeof(WalSnapshotExtHeader) +
            sizeof(WalSnapshotOrderExt) * order_count +
            aux_bytes +
            sizeof(WalSnapshotCounters);
        if (total > kSnapMaxPayload) return false;
        out.reserve(static_cast<std::size_t>(total));

        blob_put(out, header_out);
        // Pinned contract order: ALL levels first (bids desc, asks asc), then
        // ALL orders FIFO per level in the same level-major order.
        for (int s = 0; s < 2; ++s) {
            const Side side = (s == 0) ? Side::BUY : Side::SELL;
            const uint32_t n = (s == 0) ? nb : na;
            for (uint32_t i = 0; i < n; ++i) {
                const PriceLevel* lvl = book.level(side, i);
                WalSnapshotLevel wl{};
                wl.price_ticks = lvl->price_ticks;
                wl.side = static_cast<uint8_t>(side);
                blob_put(out, wl);
            }
        }
        for (int s = 0; s < 2; ++s) {
            const Side side = (s == 0) ? Side::BUY : Side::SELL;
            const uint32_t n = (s == 0) ? nb : na;
            for (uint32_t i = 0; i < n; ++i) {
                const PriceLevel* lvl = book.level(side, i);
                for (const Order* o = lvl->head; o != nullptr; o = o->next) {
                    WalSnapshotOrder wo;
                    WalSnapshotOrderExt xe;  // unused here — shared mapper
                    rows_from_order(*o, wo, xe);
                    wo.stop_price_ticks = 0;  // stop state lives off-book
                    blob_put(out, wo);
                }
            }
        }

        WalSnapshotExtHeader xh{};
        xh.magic = kSnapExtMagic;
        xh.version = aux != nullptr ? 3 : 2;
        xh.flags = 0;
        xh.order_count = order_count;
        blob_put(out, xh);

        uint32_t level_index = 0;
        for (int s = 0; s < 2; ++s) {
            const Side side = (s == 0) ? Side::BUY : Side::SELL;
            const uint32_t n = (s == 0) ? nb : na;
            for (uint32_t i = 0; i < n; ++i, ++level_index) {
                const PriceLevel* lvl = book.level(side, i);
                for (const Order* o = lvl->head; o != nullptr; o = o->next) {
                    WalSnapshotOrder wo;   // unused here — shared mapper
                    WalSnapshotOrderExt xe;
                    rows_from_order(*o, wo, xe);
                    xe.level_index = level_index;
                    blob_put(out, xe);
                }
            }
        }

        // v3 aux block — engine side-table rows the book cannot carry. The
        // pending records reuse the pinned+ext shapes verbatim so restore
        // walks one record family for both resting and off-book orders.
        if (aux != nullptr) {
            WalSnapshotAuxHeader ah{};
            ah.magic = kSnapAuxMagic;
            ah.version = kSnapAuxVersion;
            ah.flags = 0;
            ah.meta_count = aux_meta;
            ah.pending_count = aux_pend;
            ah.iceberg_count = aux_ice;
            ah.oco_count = aux_oco;
            ah.peg_count = aux_peg;
            blob_put(out, ah);
            for (const auto& m : aux->metas) blob_put(out, m);
            for (const auto& p : aux->pendings) {
                if (p.order == nullptr) return false;
                WalSnapshotPendingOrder w{};
                rows_from_order(*p.order, w.ord, w.ext);
                w.ord.stop_price_ticks = p.stop_price_ticks;  // armed trigger
                w.ext.level_index = kSnapLevelOffBook;
                w.anchor_ticks = p.anchor_ticks;
                w.activation_price_ticks = p.activation_price_ticks;
                w.trail_distance = p.trail_distance;
                w.gslo_notional_units = p.gslo_notional_units;
                w.trigger_source = p.trigger_source;
                w.trail_unit = p.trail_unit;
                w.armed = p.armed;
                blob_put(out, w);
            }
            for (const auto& r : aux->icebergs) {
                WalSnapshotIceberg w{};
                w.order_id = r.tmpl.id;
                w.total_qty_units = r.tmpl.qty_units;
                w.filled_total_units = r.filled_total_units;
                w.display_qty_units = r.display_qty_units;
                blob_put(out, w);
            }
            for (const auto& r : aux->ocos) blob_put(out, r);
            for (const auto& r : aux->pegs) blob_put(out, r);
        }

        // v2 counters trailer — allocator high-water marks. A fingerprinting
        // caller passes next_trade_id=0 (book content only, same convention
        // as wal_seq=0 in wal_audit).
        WalSnapshotCounters ctr{};
        ctr.magic = kSnapCtrMagic;
        ctr.version = kSnapCtrVersion;
        ctr.next_trade_id = next_trade_id;
        blob_put(out, ctr);
        return out.size() == total;
    } catch (...) {
        return false;  // bad_alloc etc. — cold path, fail-closed
    }
}

bool SnapshotStore::parse_book(const uint8_t* blob, uint64_t len,
                               ParsedSnapshot& out) noexcept {
    try {
        out = ParsedSnapshot{};
        if (blob == nullptr || len < sizeof(WalBookSnapshotHeader)) {
            return false;
        }
        const uint8_t* p = blob;
        uint64_t left = len;

        std::memcpy(&out.header, p, sizeof(out.header));
        p += sizeof(out.header);
        left -= sizeof(out.header);

        const uint64_t lc = out.header.level_count;
        const uint64_t oc = out.header.order_count;
        if (lc > OrderBook::kMaxLevels * 2 || oc > OrderBook::kMaxOrders) {
            return false;  // sanity caps
        }
        if (left < sizeof(WalSnapshotLevel) * lc) return false;
        out.levels.resize(static_cast<std::size_t>(lc));
        for (uint64_t i = 0; i < lc; ++i) {
            std::memcpy(&out.levels[static_cast<std::size_t>(i)], p,
                        sizeof(WalSnapshotLevel));
            p += sizeof(WalSnapshotLevel);
            left -= sizeof(WalSnapshotLevel);
            const WalSnapshotLevel& l = out.levels[static_cast<std::size_t>(i)];
            if (l.side > 1 || l.price_ticks <= 0) return false;
        }
        if (oc == 0 && lc != 0) return false;  // a level implies >= 1 order
        // Bids must precede asks (contract order), be internally sorted, and
        // the book must not be crossed.
        for (uint64_t i = 1; i < lc; ++i) {
            const WalSnapshotLevel& a = out.levels[static_cast<std::size_t>(i - 1)];
            const WalSnapshotLevel& b = out.levels[static_cast<std::size_t>(i)];
            if (b.side < a.side) return false;  // asks before bids — contract
            if (a.side == b.side) {
                const bool ok = (a.side == 0) ? (a.price_ticks > b.price_ticks)
                                             : (a.price_ticks < b.price_ticks);
                if (!ok) return false;
            }
        }
        // Never-crossed (spec §3.1 / §24 #1): bids sort descending and asks
        // ascending with all bids first, so the book is crossed iff the FIRST
        // bid (highest) >= the first ask (lowest).
        uint64_t first_ask = UINT64_MAX;
        for (uint64_t i = 0; i < lc; ++i) {
            if (out.levels[static_cast<std::size_t>(i)].side == 1) {
                first_ask = i;
                break;
            }
        }
        if (first_ask != UINT64_MAX && first_ask > 0 &&
            out.levels[0].price_ticks >=
                out.levels[static_cast<std::size_t>(first_ask)].price_ticks) {
            return false;  // crossed snapshot is suspect — fail closed
        }

        if (left < sizeof(WalSnapshotOrder) * oc) return false;
        std::vector<WalSnapshotOrder> base(static_cast<std::size_t>(oc));
        for (uint64_t i = 0; i < oc; ++i) {
            std::memcpy(&base[static_cast<std::size_t>(i)], p,
                        sizeof(WalSnapshotOrder));
            p += sizeof(WalSnapshotOrder);
            left -= sizeof(WalSnapshotOrder);
        }

        // The extension block is REQUIRED for restore: without it orders
        // cannot be mapped to levels and qty/filled are unrecoverable. A
        // pinned-region-only blob is legal only for an empty book (oc == 0).
        if (left == 0) return oc == 0;
        if (left < sizeof(WalSnapshotExtHeader)) return false;
        WalSnapshotExtHeader xh{};
        std::memcpy(&xh, p, sizeof(xh));
        p += sizeof(xh);
        left -= sizeof(xh);
        if (xh.magic != kSnapExtMagic || xh.version < 1 ||
            xh.version > kSnapExtVersion || xh.order_count != oc) {
            return false;
        }
        const uint64_t ext_orders = sizeof(WalSnapshotOrderExt) * oc;
        const uint64_t ctr_bytes =
            (xh.version >= 2) ? sizeof(WalSnapshotCounters) : 0;
        // v3 aux block: the aux header sits directly after the order-ext
        // array — peek at it for the row counts before the strict length
        // check, then validate each record below.
        uint64_t aux_bytes = 0;
        WalSnapshotAuxHeader ah{};
        if (xh.version >= 3) {
            if (left < ext_orders + sizeof(WalSnapshotAuxHeader) + ctr_bytes) {
                return false;
            }
            std::memcpy(&ah, p + ext_orders, sizeof(ah));
            if (ah.magic != kSnapAuxMagic ||
                ah.version != kSnapAuxVersion) {
                return false;
            }
            // Row counts bounded by the same order-cap sanity as the pinned
            // region — the multiplies below can then never overflow.
            if (ah.meta_count > OrderBook::kMaxOrders * 2 ||
                ah.pending_count > OrderBook::kMaxOrders ||
                ah.iceberg_count > OrderBook::kMaxOrders ||
                ah.oco_count > OrderBook::kMaxOrders * 2 ||
                ah.peg_count > OrderBook::kMaxOrders) {
                return false;
            }
            aux_bytes = sizeof(WalSnapshotAuxHeader) +
                        sizeof(WalSnapshotOrderAux) * ah.meta_count +
                        sizeof(WalSnapshotPendingOrder) * ah.pending_count +
                        sizeof(WalSnapshotIceberg) * ah.iceberg_count +
                        sizeof(WalSnapshotOco) * ah.oco_count +
                        sizeof(WalSnapshotPeg) * ah.peg_count;
        }
        if (left != ext_orders + aux_bytes + ctr_bytes) return false;
        if (ctr_bytes != 0) {
            WalSnapshotCounters ctr{};
            std::memcpy(&ctr, p + ext_orders + aux_bytes, sizeof(ctr));
            if (ctr.magic != kSnapCtrMagic ||
                ctr.version != kSnapCtrVersion) {
                return false;
            }
            out.next_trade_id = ctr.next_trade_id;
        }
        if (oc == 0 && xh.version < 3) return true;
        if (oc == 0 && ah.meta_count + ah.pending_count + ah.iceberg_count +
                      ah.oco_count + ah.peg_count == 0) {
            return true;   // v3 empty blob — nothing below to validate
        }

        out.orders.resize(static_cast<std::size_t>(oc));
        for (uint64_t i = 0; i < oc; ++i) {
            WalSnapshotOrderExt xe{};
            std::memcpy(&xe, p, sizeof(xe));
            p += sizeof(xe);
            left -= sizeof(xe);
            const WalSnapshotOrder& w = base[static_cast<std::size_t>(i)];
            if (xe.order_id != w.order_id || xe.level_index >= lc) return false;
            const WalSnapshotLevel& lvl = out.levels[xe.level_index];
            if (xe.price_ticks != lvl.price_ticks ||
                xe.side != lvl.side) {
                return false;
            }
            if (xe.qty_units <= 0 || xe.filled_qty_units < 0 ||
                xe.filled_qty_units >= xe.qty_units) {
                return false;  // resting order must have live remainder
            }
            if (xe.qty_units - xe.filled_qty_units != w.qty_units) {
                return false;  // pinned remaining != ext (qty - filled)
            }
            if (w.qty_units <= 0 || w.visible_qty_units < 0 ||
                w.visible_qty_units > w.qty_units) {
                return false;
            }
            Order& t = out.orders[static_cast<std::size_t>(i)].tmpl;
            order_from_rows(w, xe, t);
            out.orders[static_cast<std::size_t>(i)].remaining = w.qty_units;
            out.orders[static_cast<std::size_t>(i)].level_index = xe.level_index;
        }

        // v3 aux arrays — p now sits on the aux header (already validated
        // above; skip it) followed by the five row families.
        if (xh.version >= 3) {
            p += sizeof(WalSnapshotAuxHeader);
            left -= sizeof(WalSnapshotAuxHeader);
            const uint64_t mc = ah.meta_count, pc = ah.pending_count,
                           ic = ah.iceberg_count, kc = ah.oco_count,
                           gc = ah.peg_count;
            out.metas.resize(static_cast<std::size_t>(mc));
            for (uint64_t i = 0; i < mc; ++i) {
                std::memcpy(&out.metas[static_cast<std::size_t>(i)], p,
                            sizeof(WalSnapshotOrderAux));
                p += sizeof(WalSnapshotOrderAux);
                if (out.metas[static_cast<std::size_t>(i)].order_id == 0 ||
                    out.metas[static_cast<std::size_t>(i)].expiry_ns < 0) {
                    return false;
                }
            }
            out.pendings.resize(static_cast<std::size_t>(pc));
            for (uint64_t i = 0; i < pc; ++i) {
                WalSnapshotPendingOrder& w =
                    out.pendings[static_cast<std::size_t>(i)];
                std::memcpy(&w, p, sizeof(w));
                p += sizeof(w);
                // Same fidelity invariants as a book order, minus the level
                // membership (off-book) and plus an armed trigger threshold.
                if (w.ord.order_id == 0 || w.ext.order_id != w.ord.order_id ||
                    w.ext.level_index != kSnapLevelOffBook ||
                    w.ext.qty_units <= 0 || w.ext.filled_qty_units < 0 ||
                    w.ext.filled_qty_units >= w.ext.qty_units ||
                    w.ord.qty_units !=
                        w.ext.qty_units - w.ext.filled_qty_units ||
                    w.ext.side > 1 ||
                    (w.trail_unit == 0 && w.ord.stop_price_ticks <= 0) ||
                    w.trigger_source > kTriggerSourceIndex) {
                    return false;
                }
            }
            out.icebergs.resize(static_cast<std::size_t>(ic));
            for (uint64_t i = 0; i < ic; ++i) {
                WalSnapshotIceberg& w =
                    out.icebergs[static_cast<std::size_t>(i)];
                std::memcpy(&w, p, sizeof(w));
                p += sizeof(w);
                if (w.order_id == 0 || w.total_qty_units <= 0 ||
                    w.filled_total_units < 0 ||
                    w.filled_total_units >= w.total_qty_units ||
                    w.display_qty_units <= 0) {
                    return false;
                }
            }
            out.ocos.resize(static_cast<std::size_t>(kc));
            for (uint64_t i = 0; i < kc; ++i) {
                WalSnapshotOco& w = out.ocos[static_cast<std::size_t>(i)];
                std::memcpy(&w, p, sizeof(w));
                p += sizeof(w);
                if (w.order_id == 0 || w.link_id == 0 || w.state > 1) {
                    return false;
                }
            }
            out.pegs.resize(static_cast<std::size_t>(gc));
            for (uint64_t i = 0; i < gc; ++i) {
                WalSnapshotPeg& w = out.pegs[static_cast<std::size_t>(i)];
                std::memcpy(&w, p, sizeof(w));
                p += sizeof(w);
                if (w.order_id == 0 || w.mode == 0) return false;
            }
        }
        return true;
    } catch (...) {
        return false;
    }
}

// --- SnapshotStore: cadence -----------------------------------------------------

SnapshotStore::SnapshotStore(ISnapshotSink& sink, SnapshotPolicy policy) noexcept
    : sink_(sink), policy_(policy) {}

SnapshotOutcome SnapshotStore::maybe_snapshot(const OrderBook& book,
                                              uint32_t instrument_id,
                                              uint64_t wal_seq,
                                              uint64_t now_ns,
                                              uint64_t trades_delta,
                                              uint64_t next_trade_id,
                                              const SnapshotAuxState* aux) noexcept {
    trades_acc_ += trades_delta;
    const bool trade_hit = trades_acc_ >= policy_.trade_interval;
    const bool time_hit =
        first_done_ && (now_ns - last_ns_) >= policy_.interval_ns;
    if (!first_done_ || trade_hit || time_hit) {
        // First-ever snapshot is unconditional — a boot's earliest checkpoint
        // bounds worst-case replay depth.
        const SnapshotOutcome o =
            force_snapshot(book, instrument_id, wal_seq, next_trade_id, aux);
        if (o == SnapshotOutcome::Taken) last_ns_ = now_ns;
        return o;
    }
    return SnapshotOutcome::Skipped;
}

SnapshotOutcome SnapshotStore::force_snapshot(const OrderBook& book,
                                              uint32_t instrument_id,
                                              uint64_t wal_seq,
                                              uint64_t next_trade_id,
                                              const SnapshotAuxState* aux) noexcept {
    std::vector<uint8_t> blob;
    WalBookSnapshotHeader hdr{};
    if (!serialize_book(book, instrument_id, wal_seq, blob, hdr,
                        next_trade_id, aux)) {
        return SnapshotOutcome::SerializeFailed;
    }
    if (!sink_.store(instrument_id, wal_seq, blob.data(), blob.size())) {
        return SnapshotOutcome::StoreFailed;
    }
    ++taken_;
    last_seq_ = wal_seq;
    trades_acc_ = 0;
    first_done_ = true;
    return SnapshotOutcome::Taken;
}

}  // namespace exch
