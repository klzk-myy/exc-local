// Task 1.5.3.5 — WAL fault-injection verifier/recovery tool.
//
// Drives the real Task-1.3.6 WAL implementation (core/src/wal) from the
// ci/fault-injection Go harness: writes deterministic segments, scans them
// read-only, injects torn tails / kills -9 mid-append, runs the detect +
// truncate recovery primitive, and exercises the safe_math overflow
// surface. Every mode prints one JSON line on stdout for the harness to
// assert against; `recover` additionally persists a recovery_report file
// (the spec §3.5/§18.1 recovery-report analogue for Phase-01 primitives).
//
// Build (run.sh does this):
//   g++ -std=c++20 -O2 -Wall -Wextra -I core/include
//       ci/fault-injection/cpp/walverify.cpp
//       core/src/wal/Wal.cpp core/src/wal/WalEntry.cpp
//       core/src/utils/TimeUtils.cpp -o ci/fault-injection/bin/walverify
//
// Exit contract: 0 = mode completed (facts are in the JSON; the harness
// decides pass/fail), 2 = fail-closed halt observed (BadHeader on
// open/recover — the matching-core "must not start" path), 64 = usage.

#include <cerrno>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <string>
#include <vector>

#include <fcntl.h>
#include <signal.h>
#include <sys/stat.h>
#include <unistd.h>

#include "utils/TimeUtils.hpp"
#include "utils/error_severity.hpp"
#include "utils/safe_math.hpp"
#include "wal/Wal.hpp"
#include "wal/WalEntry.hpp"

using namespace exch;

namespace {

constexpr uint32_t kPayloadLen = 32;

// Deterministic payload shared with the Go harness: byte i of the record
// with sequence `seq` is (seq*31 + i) & 0xFF (same generator as
// core/tests/test_wal.cpp, so corruption checks are self-verifying).
std::vector<uint8_t> payload_for(uint64_t seq, uint32_t len = kPayloadLen) {
    std::vector<uint8_t> v(len);
    for (uint32_t i = 0; i < len; ++i)
        v[i] = static_cast<uint8_t>((seq * 31 + i) & 0xFF);
    return v;
}

uint64_t file_size(const std::string& p) {
    struct stat st {};
    return ::stat(p.c_str(), &st) == 0 ? static_cast<uint64_t>(st.st_size) : 0;
}

[[noreturn]] void usage() {
    std::fprintf(stderr,
        "walverify modes:\n"
        "  write   <path> <shard> <count>           create WAL, append count entries, fsync\n"
        "  append  <path> <shard> <seq>             explicit-seq append; prints WalStatus\n"
        "  scan    <path>                           read-only drain; entries/corrupt/offsets\n"
        "  tamper  <path> <offset> <len> [hexfill]  pwrite raw bytes (corruption injector)\n"
        "  crash   <path> <shard> <count>           commit count entries, fsync, pwrite a\n"
        "                                         torn partial record, fsync, SIGKILL self\n"
        "  recover <path> <shard> <report.json>     Wal::open() recovery; writes report\n"
        "  overflow                                 safe_math overflow/underflow checks\n");
    std::exit(64);
}

const char* scan_step_name(WalScanStep s) {
    switch (s) {
        case WalScanStep::Entry:   return "Entry";
        case WalScanStep::Pad:     return "Pad";
        case WalScanStep::End:     return "End";
        case WalScanStep::Corrupt: return "Corrupt";
    }
    return "?";
}

// --- write -------------------------------------------------------------------

int mode_write(const char* path, uint16_t shard, uint64_t count) {
    Wal w(path, shard);
    WalStatus s = w.open();
    if (s != WalStatus::Ok) {
        std::printf("{\"mode\":\"write\",\"status\":\"%s\"}\n", wal_status_str(s));
        return 0;
    }
    std::vector<uint64_t> offsets;
    offsets.reserve(count);
    for (uint64_t i = 0; i < count; ++i) {
        const auto payload = payload_for(i);
        offsets.push_back(w.logical_offset());
        s = w.append(WalEventType::ORDER_NEW, payload.data(),
                     static_cast<uint32_t>(payload.size()));
        if (s != WalStatus::Ok) {
            std::printf("{\"mode\":\"write\",\"status\":\"%s\",\"entries\":%llu}\n",
                        wal_status_str(s),
                        static_cast<unsigned long long>(i));
            return 0;
        }
    }
    s = w.flush();
    const uint64_t tail = w.logical_offset();
    w.close();
    std::printf("{\"mode\":\"write\",\"status\":\"%s\",\"entries\":%llu,"
                "\"last_seq\":%llu,\"tail\":%llu,\"file_size\":%llu,\"offsets\":[",
                wal_status_str(s),
                static_cast<unsigned long long>(count),
                static_cast<unsigned long long>(count ? count - 1 : 0),
                static_cast<unsigned long long>(tail),
                static_cast<unsigned long long>(file_size(path)));
    for (size_t i = 0; i < offsets.size(); ++i)
        std::printf("%s%llu", i ? "," : "",
                    static_cast<unsigned long long>(offsets[i]));
    std::printf("]}\n");
    return 0;
}

// --- append ------------------------------------------------------------------

int mode_append(const char* path, uint16_t shard, uint64_t seq) {
    Wal w(path, shard);
    WalStatus s = w.open();
    if (s != WalStatus::Ok) {
        std::printf("{\"mode\":\"append\",\"status\":\"%s\"}\n", wal_status_str(s));
        return (s == WalStatus::BadHeader) ? 2 : 0;
    }
    const auto payload = payload_for(seq);
    const uint64_t tail_before = w.logical_offset();
    s = w.append(seq, 1'700'000'000'000'000'000ull + seq, WalEventType::ORDER_NEW,
                 payload.data(), static_cast<uint32_t>(payload.size()));
    if (s == WalStatus::Ok) s = w.flush();
    const uint64_t tail_after = w.logical_offset();
    w.close();
    std::printf("{\"mode\":\"append\",\"status\":\"%s\",\"seq\":%llu,"
                "\"tail_before\":%llu,\"tail_after\":%llu}\n",
                wal_status_str(s),
                static_cast<unsigned long long>(seq),
                static_cast<unsigned long long>(tail_before),
                static_cast<unsigned long long>(tail_after));
    return 0;
}

// --- scan --------------------------------------------------------------------

int mode_scan(const char* path) {
    WalReader r;
    const WalStatus s = r.open(path);
    if (s != WalStatus::Ok) {
        std::printf("{\"mode\":\"scan\",\"path\":\"%s\",\"open\":\"%s\","
                    "\"file_size\":%llu}\n",
                    path, wal_status_str(s),
                    static_cast<unsigned long long>(file_size(path)));
        return 0;
    }
    uint64_t corrupt_offset = 0;
    WalScanStep last = WalScanStep::End;
    bool payload_ok = true;
    bool seq_contiguous = true;
    uint64_t expect_seq = 0;
    WalEntryView v;
    for (;;) {
        const WalScanStep st = r.next(v);
        if (st == WalScanStep::Entry) {
            if (v.seq != expect_seq) seq_contiguous = false;
            expect_seq = v.seq + 1;
            if (v.payload_len != kPayloadLen ||
                std::memcmp(v.payload, payload_for(v.seq).data(),
                            kPayloadLen) != 0)
                payload_ok = false;
            continue;
        }
        last = st;
        if (st == WalScanStep::Corrupt) corrupt_offset = r.offset();
        break;
    }
    std::printf("{\"mode\":\"scan\",\"path\":\"%s\",\"open\":\"%s\","
                "\"magic\":%u,\"version\":%u,\"shard\":%u,"
                "\"file_size\":%llu,\"entries\":%llu,\"last_seq\":%llu,"
                "\"payload_ok\":%s,\"seq_contiguous\":%s,"
                "\"end_step\":\"%s\",\"corrupt\":%s,\"corrupt_offset\":%llu}\n",
                path, wal_status_str(s), r.magic(), r.version(), r.shard_id(),
                static_cast<unsigned long long>(r.file_size()),
                static_cast<unsigned long long>(r.entries_seen()),
                static_cast<unsigned long long>(r.last_seq()),
                payload_ok ? "true" : "false",
                seq_contiguous ? "true" : "false",
                scan_step_name(last),
                r.corrupt_seen() ? "true" : "false",
                static_cast<unsigned long long>(corrupt_offset));
    return 0;
}

// --- tamper ------------------------------------------------------------------

int mode_tamper(const char* path, uint64_t offset, uint64_t len,
                uint8_t fill) {
    const int fd = ::open(path, O_WRONLY | O_CLOEXEC);
    if (fd < 0) {
        std::printf("{\"mode\":\"tamper\",\"status\":\"open:%s\"}\n",
                    std::strerror(errno));
        return 0;
    }
    std::vector<uint8_t> junk(len, fill);
    // Vary the bytes so a repeated tamper still differs from neighbours.
    for (uint64_t i = 0; i < len; ++i) junk[i] = static_cast<uint8_t>(fill ^ i);
    const ssize_t wr = ::pwrite(fd, junk.data(), len, static_cast<off_t>(offset));
    ::fsync(fd);
    ::close(fd);
    std::printf("{\"mode\":\"tamper\",\"status\":\"%s\",\"offset\":%llu,"
                "\"len\":%llu}\n",
                wr == static_cast<ssize_t>(len) ? "Ok" : "short-write",
                static_cast<unsigned long long>(offset),
                static_cast<unsigned long long>(len));
    return 0;
}

// --- crash -------------------------------------------------------------------
// Simulates kill -9 mid-append: count entries are committed and fsynced,
// then a torn partial record (a half-written entry header: valid seq +
// timestamp, truncated before payload_len/CRC) is pwritten at the logical
// tail, fsynced, and the process raises SIGKILL. On restart the recovery
// path must observe exactly `count` valid entries and truncate the tail.

int mode_crash(const char* path, uint16_t shard, uint64_t count) {
    {
        Wal w(path, shard);
        WalStatus s = w.open();
        if (s != WalStatus::Ok) {
            std::printf("{\"mode\":\"crash\",\"status\":\"%s\"}\n",
                        wal_status_str(s));
            std::fflush(stdout);
            return 0;
        }
        for (uint64_t i = 0; i < count; ++i) {
            const auto payload = payload_for(i);
            s = w.append(WalEventType::ORDER_NEW, payload.data(),
                         static_cast<uint32_t>(payload.size()));
            if (s != WalStatus::Ok) break;
        }
        (void)w.flush();
        const uint64_t tail = w.logical_offset();
        // Half an entry header (seq+ts, no payload_len/crc) = torn write.
        uint8_t torn[16];
        const uint64_t bad_seq = count;  // the in-flight next seq
        const uint64_t bad_ts = now_ns();
        std::memcpy(torn, &bad_seq, 8);
        std::memcpy(torn + 8, &bad_ts, 8);
        const int fd = ::open(path, O_WRONLY | O_CLOEXEC);
        if (fd >= 0) {
            const ssize_t wr =
                ::pwrite(fd, torn, sizeof(torn), static_cast<off_t>(tail));
            if (wr != static_cast<ssize_t>(sizeof(torn)))
                std::fprintf(stderr, "walverify: torn pwrite short: %zd\n", wr);
            ::fsync(fd);
            ::close(fd);
        }
        std::printf("{\"mode\":\"crash\",\"status\":\"committed_then_killed\","
                    "\"committed\":%llu,\"tail\":%llu,\"torn_bytes\":%zu}\n",
                    static_cast<unsigned long long>(count),
                    static_cast<unsigned long long>(tail), sizeof(torn));
        std::fflush(stdout);
        // Wal destructor intentionally skipped: process dies here.
    }
    ::raise(SIGKILL);
    _exit(128 + SIGKILL);
}

// --- recover -----------------------------------------------------------------

int mode_recover(const char* path, uint16_t shard, const char* report_path) {
    const uint64_t size_before = file_size(path);
    Wal w(path, shard);
    const WalStatus s = w.open();
    const uint64_t size_after = file_size(path);

    const WalScanResult& sc = w.last_scan();
    const char* outcome;
    int rc = 0;
    if (s == WalStatus::BadHeader) {
        outcome = "fatal_bad_header";   // fail-closed halt: file left untouched
        rc = 2;
    } else if (s != WalStatus::Ok) {
        outcome = "fatal_io";
        rc = 2;
    } else {
        outcome = sc.corrupt ? "truncated_torn_tail" : "clean";
    }
    // Torn-header reinit (all-zero start) grows the file; truncation only
    // ever shrinks it, so report the delta signed-free.
    const uint64_t truncated =
        size_after < size_before ? size_before - size_after : 0;

    std::string rep;
    char buf[1024];
    std::snprintf(buf, sizeof(buf),
        "{\"mode\":\"recover\",\"open\":\"%s\",\"outcome\":\"%s\","
        "\"header_ok\":%s,\"entries\":%llu,\"last_valid_seq\":%llu,"
        "\"corrupt_detected\":%s,\"valid_end\":%llu,"
        "\"file_size_before\":%llu,\"file_size_after\":%llu,"
        "\"truncated_bytes\":%llu,\"recovered_tail_seq\":%llu}\n",
        wal_status_str(s), outcome,
        sc.header_ok ? "true" : "false",
        static_cast<unsigned long long>(sc.entries),
        static_cast<unsigned long long>(sc.last_seq),
        sc.corrupt ? "true" : "false",
        static_cast<unsigned long long>(sc.valid_end),
        static_cast<unsigned long long>(size_before),
        static_cast<unsigned long long>(size_after),
        static_cast<unsigned long long>(truncated),
        static_cast<unsigned long long>(w.tail_seq()));
    rep = buf;
    std::fputs(rep.c_str(), stdout);

    if (const int fd = ::open(report_path, O_WRONLY | O_CREAT | O_TRUNC | O_CLOEXEC,
                              0644); fd >= 0) {
        char hdr[256];
        const int hn = std::snprintf(hdr, sizeof(hdr),
            "{\"kind\":\"wal_recovery_report\",\"spec\":\"3.5/18.1\","
            "\"shard\":%u,\"recovered_at_ns\":%llu,",
            shard, static_cast<unsigned long long>(now_ns()));
        rep = std::string(hdr, hn) + rep.substr(1);  // splice into one object
        const ssize_t wr = ::write(fd, rep.data(), rep.size());
        if (wr != static_cast<ssize_t>(rep.size()))
            std::fprintf(stderr, "walverify: report write short: %zd\n", wr);
        ::fsync(fd);
        ::close(fd);
    }
    w.close();
    return rc;
}

// --- overflow ----------------------------------------------------------------

int mode_overflow() {
    bool ok = true;
    // try_* must return false AND leave the destination unmodified.
    int64_t sentinel = -777;
    int64_t out = sentinel;
    const bool add_detected = !safe_math::try_add<int64_t>(INT64_MAX, 1, out);
    ok &= add_detected && out == sentinel;
    const bool sub_detected = !safe_math::try_sub<int64_t>(INT64_MIN, 1, out);
    ok &= sub_detected && out == sentinel;
    const bool mul_detected = !safe_math::try_mul<int64_t>(INT64_MAX, 2, out);
    ok &= mul_detected && out == sentinel;
    const bool neg_detected = !safe_math::try_neg<int64_t>(INT64_MIN, out);
    ok &= neg_detected && out == sentinel;

    // Throwing surface must carry the §23 code + L2 severity.
    bool throw_ok = false;
    const char* code = "";
    const char* sev = "";
    int http = 0;
    try {
        (void)safe_math::add<int64_t>(INT64_MAX, 1);
    } catch (const safe_math::ArithmeticOverflowError& e) {
        throw_ok = true;
        code = e.code();
        sev = severity_name(e.severity());
        http = e.http_status();
    }
    ok &= throw_ok;

    std::printf("{\"mode\":\"overflow\",\"add_detected\":%s,\"sub_detected\":%s,"
                "\"mul_detected\":%s,\"neg_detected\":%s,"
                "\"out_unmodified\":%s,\"throw_ok\":%s,\"throw_code\":\"%s\","
                "\"throw_severity\":\"%s\",\"throw_http\":%d}\n",
                add_detected ? "true" : "false",
                sub_detected ? "true" : "false",
                mul_detected ? "true" : "false",
                neg_detected ? "true" : "false",
                (out == sentinel) ? "true" : "false",
                throw_ok ? "true" : "false", code, sev, http);
    return ok ? 0 : 1;
}

}  // namespace

int main(int argc, char** argv) {
    if (argc < 2) usage();
    const std::string mode = argv[1];
    if (mode == "write" && argc == 5)
        return mode_write(argv[2],
                          static_cast<uint16_t>(std::stoul(argv[3])),
                          std::stoull(argv[4]));
    if (mode == "append" && argc == 5)
        return mode_append(argv[2],
                           static_cast<uint16_t>(std::stoul(argv[3])),
                           std::stoull(argv[4]));
    if (mode == "scan" && argc == 3) return mode_scan(argv[2]);
    if (mode == "tamper" && argc >= 5) {
        const uint8_t fill = argc >= 6
            ? static_cast<uint8_t>(std::stoul(argv[5], nullptr, 16))
            : 0xA5;
        return mode_tamper(argv[2], std::stoull(argv[3]),
                           std::stoull(argv[4]), fill);
    }
    if (mode == "crash" && argc == 5)
        return mode_crash(argv[2],
                          static_cast<uint16_t>(std::stoul(argv[3])),
                          std::stoull(argv[4]));
    if (mode == "recover" && argc == 5)
        return mode_recover(argv[2],
                            static_cast<uint16_t>(std::stoul(argv[3])),
                            argv[4]);
    if (mode == "overflow" && argc == 2) return mode_overflow();
    usage();
}
