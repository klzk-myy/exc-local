// PHASE-02.5 TASK-2.5.3 — wal_audit: WAL integrity scanner + deterministic
// replay fingerprint for the soak harness (spec §3.4 wire format, §3.5
// recovery semantics, §24 zero-loss/parity invariants).
//
// Modes:
//   scan        — read-only segment walk: header validity, seq contiguity
//                 across rotation, CRC/torn-tail detection, per-type counts,
//                 TRADE trade_id duplicate detection.
//   recover     — scan + RecoveryManager replay into a fresh book; emits the
//                 canonical SnapshotStore::serialize_book fingerprint.
//   fingerprint — recover, but stdout carries ONLY the fingerprint hex so
//                 parity scripts can capture it verbatim.
//
// Live-WAL safety: RecoveryManager performs detect+truncate repair on a
// corrupt tail segment (Wal::open ladder step 1) — mutating the file under a
// running writer would corrupt the live engine's journal. `-live` therefore
// stages a private copy of the segment set in a temp dir and recovers on the
// copy. `-mode scan` is always read-only (WalReader mmap) and safe live.
//
// Fail-closed (spec §2.7): any violated invariant sets ok:false and exit 1.
// Arg/IO misuse exits 2. Errors go to stderr; the result line is one JSON
// object on stdout (or bare hex under -mode fingerprint).
//
// Layout (spec §3.4): -wal-dir is the shard segment directory holding
// {seq_base}.wal files. Seq space is continuous across rotation; each
// segment's filename stem equals the seq of its first entry.

#include <algorithm>
#include <cinttypes>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <filesystem>
#include <new>
#include <string>
#include <unordered_map>
#include <unordered_set>
#include <utility>
#include <vector>

#include <unistd.h>

#include "book/Order.hpp"
#include "book/OrderBook.hpp"
#include "recovery/RecoveryManager.hpp"
#include "recovery/SnapshotStore.hpp"
#include "utils/MemoryPool.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

namespace {

using exch::Order;
using exch::OrderBook;
using exch::RecoveryBookBinding;
using exch::RecoveryManager;
using exch::RecoveryResult;
using exch::SnapshotStore;
using exch::WalBookSnapshotHeader;
using exch::WalEntryView;
using exch::WalEventType;
using exch::WalOrderNewPayload;
using exch::WalReader;
using exch::WalScanStep;
using exch::WalStatus;
using exch::WalTradePayload;
using exch::MemoryPool;

enum class Mode : uint8_t { Scan, Recover, Fingerprint };

struct Args {
    std::string wal_dir;
    uint32_t instrument_id = 0;
    bool instrument_set = false;
    Mode mode = Mode::Scan;
    bool json = false;
    bool live = false;
};

void usage(const char* argv0) {
    std::fprintf(stderr,
        "usage: %s -wal-dir DIR [-instrument-id N] [-mode MODE] [-json] [-live]\n"
        "  -mode scan        segment scan only (default): headers, seq\n"
        "                    contiguity, CRC/torn tail, type counts, dup trades\n"
        "  -mode recover     scan + RecoveryManager replay + book fingerprint\n"
        "  -mode fingerprint recover, stdout = fingerprint hex only\n"
        "  -json             emit single-line JSON result (default: kv lines)\n"
        "  -live             stage a private copy for recover — safe while the\n"
        "                    engine is appending (RecoveryManager truncates torn\n"
        "                    tails; never run recover on a live dir without it)\n",
        argv0);
}

bool parse_u32(const char* s, uint32_t* out) {
    if (s == nullptr || *s == '\0' || *s == '-') return false;
    char* end = nullptr;
    const unsigned long v = std::strtoul(s, &end, 10);
    if (end == s || *end != '\0' || v > 0xFFFFFFFFul) return false;
    *out = static_cast<uint32_t>(v);
    return true;
}

// FNV-1a 64 — stable across runs/hosts, no external deps.
uint64_t fnv1a64(const uint8_t* data, std::size_t len) noexcept {
    uint64_t h = 14695981039346656037ull;
    for (std::size_t i = 0; i < len; ++i) {
        h ^= data[i];
        h *= 1099511628211ull;
    }
    return h;
}

const char* type_name(WalEventType t) noexcept {
    switch (t) {
        case WalEventType::ORDER_NEW:       return "ORDER_NEW";
        case WalEventType::ORDER_CANCEL:    return "ORDER_CANCEL";
        case WalEventType::ORDER_MODIFY:    return "ORDER_MODIFY";
        case WalEventType::TRADE:           return "TRADE";
        case WalEventType::TIME_TICK:       return "TIME_TICK";
        case WalEventType::BOOK_SNAPSHOT:   return "BOOK_SNAPSHOT";
        case WalEventType::MARGIN_RESERVE:  return "MARGIN_RESERVE";
        case WalEventType::MARGIN_RELEASE:  return "MARGIN_RELEASE";
        case WalEventType::PREVENTED_MATCH: return "PREVENTED_MATCH";
    }
    return "?";
}
constexpr std::size_t kTypeCount =
    static_cast<std::size_t>(WalEventType::PREVENTED_MATCH) + 1;

void json_escape(FILE* f, const std::string& s) {
    for (const char c : s) {
        if (c == '"' || c == '\\') std::fputc('\\', f);
        if (c == '\n' || c == '\r') { std::fputc(' ', f); continue; }
        std::fputc(c, f);
    }
}

// --- Segment inventory ------------------------------------------------------

struct Segment {
    std::string path;
    std::string name;
    uint64_t stem = 0;
    bool stem_ok = false;
};

// Numeric-stem ordering (spec §3.4 {seq_base}.wal); unparseable names sort
// last by lexical order and are flagged by the scan.
std::vector<Segment> enumerate_segments(const std::string& dir) {
    std::vector<Segment> segs;
    std::error_code ec;
    for (const auto& de : std::filesystem::directory_iterator(dir, ec)) {
        if (ec) break;
        std::error_code tec;
        if (!de.is_regular_file(tec) || tec) continue;
        if (de.path().extension() != ".wal") continue;
        Segment s;
        s.path = de.path().string();
        s.name = de.path().filename().string();
        const std::string stem = de.path().stem().string();
        char* end = nullptr;
        const unsigned long long v = std::strtoull(stem.c_str(), &end, 10);
        s.stem_ok = (end != stem.c_str() && *end == '\0');
        s.stem = s.stem_ok ? static_cast<uint64_t>(v) : 0;
        segs.push_back(std::move(s));
    }
    std::sort(segs.begin(), segs.end(), [](const Segment& a, const Segment& b) {
        if (a.stem_ok != b.stem_ok) return a.stem_ok;
        if (a.stem_ok && b.stem_ok && a.stem != b.stem) return a.stem < b.stem;
        return a.name < b.name;
    });
    return segs;
}

// --- Scan aggregates --------------------------------------------------------

struct Audit {
    bool ok = true;
    std::string detail;              // first defect description

    uint32_t segments = 0;
    uint64_t entries = 0;
    uint64_t seq_min = 0;
    uint64_t seq_max = 0;
    uint64_t seq_gaps = 0;           // forward discontinuity events
    uint64_t seq_missing = 0;        // total skipped seq numbers
    uint64_t seq_regressions = 0;    // ev.seq < expected
    uint64_t stem_mismatch = 0;      // first entry seq != filename seq_base
    uint64_t bad_payloads = 0;       // pinned-size mismatch on decode
    bool corrupt = false;            // corruption in a non-tail segment
    bool tail_corrupt = false;       // warn: torn tail on the last segment
    uint64_t tail_corrupt_offset = 0;

    uint16_t shard_id = 0;
    bool shard_seen = false;
    bool shard_mismatch = false;

    uint64_t trades = 0;
    uint64_t dup_trade_ids = 0;
    uint64_t max_trade_id = 0;
    uint64_t type_counts[kTypeCount] = {};
    std::unordered_map<uint32_t, uint64_t> instrument_hits;

    // recover/fingerprint mode only:
    uint64_t wal_tail = 0;
    uint64_t recovered_orders = 0;
    uint64_t entries_replayed = 0;
    uint64_t mutations_applied = 0;
    uint64_t dedup_skips = 0;
    uint64_t trades_derived = 0;
    bool tail_truncated = false;
    std::string fingerprint;
    uint32_t bound_instrument = 0;
};

void defect(Audit& a, const char* msg) {
    if (a.detail.empty()) a.detail = msg;
}

// Scan every segment in order; fills `a`. `is_last` distinguishes the live
// tail (torn = warn, spec: a crash mid-append is the expected boundary) from
// sealed segments (torn = hard corruption).
void scan_segments(const std::vector<Segment>& segs, Audit& a) {
    std::unordered_set<uint64_t> trade_ids;
    bool expected_valid = false;
    uint64_t expected = 0;

    for (std::size_t i = 0; i < segs.size(); ++i) {
        const Segment& seg = segs[i];
        const bool is_last = (i + 1 == segs.size());
        ++a.segments;

        if (!seg.stem_ok) {
            ++a.stem_mismatch;
            defect(a, "non-numeric segment filename (expected {seq_base}.wal)");
        }

        WalReader r;
        const WalStatus os = r.open(seg.path);
        if (os != WalStatus::Ok) {
            // A just-rotated segment can momentarily look unreadable on a
            // live dir; on a sealed segment an unreadable/bad header is a
            // hard integrity failure.
            if (is_last) {
                a.tail_corrupt = true;
                defect(a, "tail segment unreadable (torn creation?)");
            } else {
                a.corrupt = true;
                defect(a, "sealed segment unreadable/bad header");
            }
            continue;
        }

        if (!a.shard_seen) {
            a.shard_seen = true;
            a.shard_id = r.shard_id();
        } else if (r.shard_id() != a.shard_id) {
            a.shard_mismatch = true;
            defect(a, "mixed shard_id across segments");
        }

        bool first_entry = true;
        WalEntryView ev{};
        for (;;) {
            const WalScanStep s = r.next(ev);
            if (s == WalScanStep::Pad) continue;
            if (s == WalScanStep::End) break;
            if (s == WalScanStep::Corrupt) {
                if (is_last) {
                    a.tail_corrupt = true;
                    a.tail_corrupt_offset = r.offset();  // truncate target
                } else {
                    a.corrupt = true;
                    defect(a, "corrupt record inside a sealed segment");
                }
                break;
            }
            // Entry.
            ++a.entries;
            const std::size_t ti = static_cast<std::size_t>(ev.type);
            if (ti < kTypeCount) ++a.type_counts[ti];

            if (first_entry) {
                first_entry = false;
                if (seg.stem_ok && ev.seq != seg.stem) {
                    ++a.stem_mismatch;
                    defect(a, "segment first seq != filename seq_base");
                }
            }
            if (!expected_valid) {
                expected_valid = true;
                expected = ev.seq;
                a.seq_min = ev.seq;
            }
            if (ev.seq > expected) {
                ++a.seq_gaps;
                a.seq_missing += ev.seq - expected;
                defect(a, "WAL seq gap — entries lost mid-stream");
            } else if (ev.seq < expected) {
                ++a.seq_regressions;
                defect(a, "WAL seq regression — overlapping streams");
            }
            expected = ev.seq + 1;
            a.seq_max = ev.seq;

            // Payload-level checks (pinned sizes from wal/WalEntry.hpp).
            if (ev.type == WalEventType::TRADE) {
                if (ev.payload_len == sizeof(WalTradePayload)) {
                    WalTradePayload p{};
                    std::memcpy(&p, ev.payload, sizeof(p));
                    ++a.trades;
                    if (p.trade_id > a.max_trade_id) a.max_trade_id = p.trade_id;
                    if (!trade_ids.insert(p.trade_id).second) {
                        ++a.dup_trade_ids;
                        defect(a, "duplicate TRADE trade_id in journal");
                    }
                } else {
                    ++a.bad_payloads;
                    defect(a, "TRADE payload size mismatch");
                }
            } else if (ev.type == WalEventType::ORDER_NEW) {
                if (ev.payload_len == sizeof(WalOrderNewPayload)) {
                    WalOrderNewPayload p{};
                    std::memcpy(&p, ev.payload, sizeof(p));
                    ++a.instrument_hits[p.instrument_id];
                } else {
                    ++a.bad_payloads;
                    defect(a, "ORDER_NEW payload size mismatch");
                }
            }
        }
    }

    a.wal_tail = expected_valid ? expected : 0;
    if (a.seq_gaps > 0 || a.seq_regressions > 0 || a.stem_mismatch > 0 ||
        a.corrupt || a.shard_mismatch || a.dup_trade_ids > 0 ||
        a.bad_payloads > 0) {
        a.ok = false;
        if (a.detail.empty()) a.detail = "wal integrity violation";
    }
}

uint32_t pick_instrument(const Args& args, const Audit& a) {
    if (args.instrument_set) return args.instrument_id;
    // Auto-bind the dominant journaled instrument (one book per shard in
    // Phase-02, but the WAL may carry foreign rows counted as such).
    uint32_t best = 0;
    uint64_t best_n = 0;
    for (const auto& [iid, n] : a.instrument_hits) {
        if (n > best_n || (n == best_n && iid < best)) {
            best = iid;
            best_n = n;
        }
    }
    return best;
}

// Copy *.wal into a private temp dir so RecoveryManager's torn-tail repair
// can never touch the live journal. Returns "" on failure.
std::string stage_live_copy(const std::string& dir) {
    char tmpl[128];
    std::snprintf(tmpl, sizeof(tmpl), "wal_audit_%ld_stage", static_cast<long>(::getpid()));
    const std::filesystem::path tmp =
        std::filesystem::temp_directory_path() / tmpl;
    std::error_code ec;
    std::filesystem::remove_all(tmp, ec);
    ec.clear();
    std::filesystem::create_directories(tmp, ec);
    if (ec) return {};
    bool copy_failed = false;
    for (const auto& de : std::filesystem::directory_iterator(dir, ec)) {
        if (ec) { copy_failed = true; break; }
        std::error_code tec;
        if (!de.is_regular_file(tec) || tec) continue;
        if (de.path().extension() != ".wal") continue;
        std::filesystem::copy_file(de.path(), tmp / de.path().filename(), tec);
        if (tec) { copy_failed = true; break; }
    }
    if (copy_failed) {
        // A partial segment set would fingerprint a false-subset book —
        // fail closed rather than emit a plausible-looking hash.
        std::filesystem::remove_all(tmp, ec);
        return {};
    }
    return tmp.string();
}

// Replay the segment set via RecoveryManager into a fresh book and compute
// the canonical fingerprint (serialize_book bytes -> FNV-1a 64).
bool run_recovery(const Args& args, Audit& a, const std::string& dir,
                  uint32_t shard_id) {
    a.bound_instrument = pick_instrument(args, a);

    std::string target = dir;
    if (args.live) {
        target = stage_live_copy(dir);
        if (target.empty()) {
            a.ok = false;
            defect(a, "live staging copy failed");
            return false;
        }
    }

    bool replay_ok = false;
    try {
        // Same pool domain the production binding uses (main.cpp): the book
        // frees recovered nodes into it, so it must outlive the book.
        MemoryPool<Order> pool(exch::kOrderPoolCapacity);
        OrderBook book(pool);
        RecoveryManager mgr(shard_id);
        const RecoveryResult res =
            mgr.recover(target, {RecoveryBookBinding{a.bound_instrument,
                                                     &book, &pool}});
        a.entries_replayed = res.entries_replayed;
        a.mutations_applied = res.mutations_applied;
        a.dedup_skips = res.dedup_skips;
        a.trades_derived = res.trades_derived;
        a.tail_truncated = res.tail_truncated;
        a.wal_tail = res.wal_tail;
        if (res.max_trade_id > a.max_trade_id) a.max_trade_id = res.max_trade_id;
        if (!res.ok()) {
            a.ok = false;
            char buf[160];
            std::snprintf(buf, sizeof(buf), "recovery failed status=%s: %s",
                          exch::recovery_status_str(res.status), res.detail);
            defect(a, buf);
        } else {
            a.recovered_orders = book.live_orders();
            std::vector<uint8_t> blob;
            WalBookSnapshotHeader hdr{};
            // wal_seq = 0: the fingerprint must hash book CONTENT only. The
            // live engine keeps appending TIME_TICK entries after restart,
            // so wal_tail advances while the recovered book is unchanged —
            // embedding the cursor would break failover-parity comparisons.
            if (SnapshotStore::serialize_book(book, a.bound_instrument,
                                              /*wal_seq*/ 0, blob, hdr)) {
                char hex[17];
                std::snprintf(hex, sizeof(hex), "%016" PRIx64,
                              fnv1a64(blob.data(), blob.size()));
                a.fingerprint = hex;
                replay_ok = true;
            } else {
                a.ok = false;
                defect(a, "serialize_book failed on recovered state");
            }
        }
    } catch (const std::bad_alloc&) {
        a.ok = false;
        defect(a, "order pool allocation failed (bad_alloc)");
    }

    if (args.live) {
        std::error_code ec;
        std::filesystem::remove_all(target, ec);
    }
    return replay_ok;
}

void emit_json(const Args& args, const Audit& a) {
    std::printf("{\"ok\":%s,\"wal_dir\":\"", a.ok ? "true" : "false");
    json_escape(stdout, args.wal_dir);
    std::printf(
        "\",\"mode\":\"%s\",\"shard_id\":%u,\"segments\":%u,\"entries\":%llu,"
        "\"seq_min\":%llu,\"seq_max\":%llu,\"seq_gaps\":%llu,"
        "\"seq_missing\":%llu,\"seq_regressions\":%llu,"
        "\"stem_mismatch\":%llu,\"bad_payloads\":%llu,"
        "\"corrupt\":%s,\"tail_corrupt\":%s,\"truncation_offset\":%llu,"
        "\"shard_mismatch\":%s,\"trades\":%llu,\"dup_trade_ids\":%llu,"
        "\"max_trade_id\":%llu,\"wal_tail\":%llu,\"instrument_id\":%u,"
        "\"recovered_orders\":%llu,\"entries_replayed\":%llu,"
        "\"mutations_applied\":%llu,\"dedup_skips\":%llu,"
        "\"trades_derived\":%llu,\"tail_truncated\":%s,"
        "\"fingerprint\":\"%s\",\"detail\":\"",
        args.mode == Mode::Scan ? "scan"
            : args.mode == Mode::Recover ? "recover" : "fingerprint",
        static_cast<unsigned>(a.shard_id), a.segments,
        static_cast<unsigned long long>(a.entries),
        static_cast<unsigned long long>(a.seq_min),
        static_cast<unsigned long long>(a.seq_max),
        static_cast<unsigned long long>(a.seq_gaps),
        static_cast<unsigned long long>(a.seq_missing),
        static_cast<unsigned long long>(a.seq_regressions),
        static_cast<unsigned long long>(a.stem_mismatch),
        static_cast<unsigned long long>(a.bad_payloads),
        a.corrupt ? "true" : "false",
        a.tail_corrupt ? "true" : "false",
        static_cast<unsigned long long>(a.tail_corrupt_offset),
        a.shard_mismatch ? "true" : "false",
        static_cast<unsigned long long>(a.trades),
        static_cast<unsigned long long>(a.dup_trade_ids),
        static_cast<unsigned long long>(a.max_trade_id),
        static_cast<unsigned long long>(a.wal_tail), a.bound_instrument,
        static_cast<unsigned long long>(a.recovered_orders),
        static_cast<unsigned long long>(a.entries_replayed),
        static_cast<unsigned long long>(a.mutations_applied),
        static_cast<unsigned long long>(a.dedup_skips),
        static_cast<unsigned long long>(a.trades_derived),
        a.tail_truncated ? "true" : "false",
        a.fingerprint.c_str());
    json_escape(stdout, a.detail);
    std::printf("\",\"types\":{");
    bool first = true;
    for (std::size_t i = 0; i < kTypeCount; ++i) {
        if (a.type_counts[i] == 0) continue;
        if (!first) std::fputc(',', stdout);
        first = false;
        std::printf("\"%s\":%llu", type_name(static_cast<WalEventType>(i)),
                    static_cast<unsigned long long>(a.type_counts[i]));
    }
    std::printf("}}\n");
}

void emit_human(const Audit& a) {
    std::printf("ok=%s segments=%u entries=%llu seq_min=%llu seq_max=%llu "
                "seq_gaps=%llu seq_regressions=%llu stem_mismatch=%llu "
                "corrupt=%s tail_corrupt=%s trades=%llu dup_trade_ids=%llu "
                "max_trade_id=%llu wal_tail=%llu recovered_orders=%llu "
                "fingerprint=%s detail=%s\n",
                a.ok ? "true" : "false", a.segments,
                static_cast<unsigned long long>(a.entries),
                static_cast<unsigned long long>(a.seq_min),
                static_cast<unsigned long long>(a.seq_max),
                static_cast<unsigned long long>(a.seq_gaps),
                static_cast<unsigned long long>(a.seq_regressions),
                static_cast<unsigned long long>(a.stem_mismatch),
                a.corrupt ? "true" : "false",
                a.tail_corrupt ? "true" : "false",
                static_cast<unsigned long long>(a.trades),
                static_cast<unsigned long long>(a.dup_trade_ids),
                static_cast<unsigned long long>(a.max_trade_id),
                static_cast<unsigned long long>(a.wal_tail),
                static_cast<unsigned long long>(a.recovered_orders),
                a.fingerprint.c_str(), a.detail.c_str());
}

}  // namespace

int main(int argc, char** argv) {
    Args args;
    for (int i = 1; i < argc; ++i) {
        const char* s = argv[i];
        if (std::strcmp(s, "-wal-dir") == 0) {
            if (++i >= argc) { usage(argv[0]); return 2; }
            args.wal_dir = argv[i];
        } else if (std::strcmp(s, "-instrument-id") == 0) {
            if (++i >= argc || !parse_u32(argv[i], &args.instrument_id)) {
                usage(argv[0]);
                return 2;
            }
            args.instrument_set = true;
        } else if (std::strcmp(s, "-mode") == 0) {
            if (++i >= argc) { usage(argv[0]); return 2; }
            if (std::strcmp(argv[i], "scan") == 0) args.mode = Mode::Scan;
            else if (std::strcmp(argv[i], "recover") == 0) args.mode = Mode::Recover;
            else if (std::strcmp(argv[i], "fingerprint") == 0)
                args.mode = Mode::Fingerprint;
            else { usage(argv[0]); return 2; }
        } else if (std::strcmp(s, "-json") == 0) {
            args.json = true;
        } else if (std::strcmp(s, "-live") == 0) {
            args.live = true;
        } else if (std::strcmp(s, "-h") == 0 || std::strcmp(s, "--help") == 0) {
            usage(argv[0]);
            return 0;
        } else {
            std::fprintf(stderr, "unknown argument: %s\n", s);
            usage(argv[0]);
            return 2;
        }
    }
    if (args.wal_dir.empty()) {
        std::fprintf(stderr, "missing required -wal-dir\n");
        usage(argv[0]);
        return 2;
    }
    std::error_code ec;
    if (!std::filesystem::is_directory(args.wal_dir, ec) || ec) {
        std::fprintf(stderr, "wal-dir not a directory: %s\n", args.wal_dir.c_str());
        return 2;
    }

    Audit a;
    const std::vector<Segment> segs = enumerate_segments(args.wal_dir);
    if (segs.empty()) {
        a.ok = false;
        defect(a, "no .wal segments found");
    } else {
        scan_segments(segs, a);
    }

    if (a.ok && args.mode != Mode::Scan) {
        run_recovery(args, a, args.wal_dir, a.shard_id);
    } else if (args.mode != Mode::Scan && !a.ok) {
        defect(a, "recovery skipped — scan-phase violation");
    }

    if (args.mode == Mode::Fingerprint) {
        // Machine contract: stdout carries only the fingerprint hex.
        if (a.ok && !a.fingerprint.empty()) {
            std::printf("%s\n", a.fingerprint.c_str());
            return 0;
        }
        std::fprintf(stderr, "wal_audit fingerprint failed: %s\n",
                     a.detail.c_str());
        return 1;
    }
    if (args.json) emit_json(args, a); else emit_human(a);
    return a.ok ? 0 : 1;
}
