// Task 2.3.24 — BilateralCreditMatrix: shm_open + mmap(MAP_SHARED) backing
// store, CREDIT_UPDATE control-message codec. All hot-path logic
// (can_match/try_debit/consume_or_skip) is inlined in the header; this TU
// owns only the open/close lifecycle and the decode seam.

#include "risk/BilateralCreditMatrix.h"

#include <cstring>
#include <fcntl.h>
#include <string>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

namespace exch {

const char* credit_ctl_decode_str(CreditCtlDecode r) noexcept {
    switch (r) {
        case CreditCtlDecode::Ok: return "Ok";
        case CreditCtlDecode::TooShort: return "TooShort";
        case CreditCtlDecode::BadMagic: return "BadMagic";
        case CreditCtlDecode::BadVersion: return "BadVersion";
        case CreditCtlDecode::UnknownType: return "UnknownType";
        case CreditCtlDecode::PartyOutOfRange: return "PartyOutOfRange";
    }
    return "?";
}

CreditCtlDecode credit_ctl_decode(const void* buf, uint32_t len,
                                  CreditUpdateMsg* out) noexcept {
    if (buf == nullptr || out == nullptr || len < sizeof(CreditUpdateMsg))
        return CreditCtlDecode::TooShort;

    CreditUpdateMsg m;
    std::memcpy(&m, buf, sizeof(m));  // untrusted buffer -> owned copy

    if (m.hdr.magic != kCreditCtlMagic)
        return CreditCtlDecode::BadMagic;
    if (m.hdr.version != kCreditCtlVersion)
        return CreditCtlDecode::BadVersion;
    if (m.hdr.type != static_cast<uint8_t>(CreditCtlType::CreditUpdate))
        return CreditCtlDecode::UnknownType;
    if (m.party_a >= kCreditMaxParties || m.party_b >= kCreditMaxParties)
        return CreditCtlDecode::PartyOutOfRange;

    *out = m;
    return CreditCtlDecode::Ok;
}

bool BilateralCreditMatrix::open(std::string_view shm_name,
                                 [[maybe_unused]] bool create) noexcept {
    close();
    if (shm_name.empty() || shm_name.size() >= 120)
        return false;

    // shm_open wants a leading '/' (same convention as ShmRing).
    std::string path = "/";
    path.append(shm_name.data(), shm_name.size());

    const int fd = ::shm_open(path.c_str(), O_RDWR | O_CREAT, 0600);
    if (fd < 0)
        return false;

    struct stat st {};
    if (::fstat(fd, &st) != 0) {
        ::close(fd);
        return false;
    }

    const bool fresh = (st.st_size == 0);
    if (!fresh && st.st_size < static_cast<off_t>(kCreditMatrixBytes)) {
        ::close(fd);
        return false;  // truncated/foreign image — fail closed
    }

    if (fresh && ::ftruncate(fd, static_cast<off_t>(kCreditMatrixBytes)) != 0) {
        ::close(fd);
        return false;
    }

    const uint64_t map_len =
        fresh ? kCreditMatrixBytes : static_cast<uint64_t>(st.st_size);
    void* base = ::mmap(nullptr, map_len, PROT_READ | PROT_WRITE, MAP_SHARED,
                        fd, 0);
    ::close(fd);
    if (base == MAP_FAILED)
        return false;

    base_ = static_cast<uint8_t*>(base);
    map_size_ = map_len;

    auto* magic_cell =
        reinterpret_cast<std::atomic<uint64_t>*>(base_ + kCreditOffMagic);
    // A second opener can race our ftruncate+init (it too saw st_size==0).
    // Re-reading magic after the mmap narrows the wipe window to the
    // pathological; startup serialization (deploy scripts shm_unlink before
    // core start, then the Go writer) is the operational control — same
    // documented discipline as ShmRing stale images.
    const bool live = magic_cell->load(std::memory_order_acquire) ==
                      kCreditMatrixMagic;
    if (fresh && !live) {
        // ftruncate already zeroed the image (fresh shm objects read as
        // zero); initialize the header explicitly anyway to be independent
        // of that guarantee, then stamp magic LAST with release — peers
        // attach-spin on magic as the init-complete barrier (ShmRing rule).
        std::memset(base_, 0, static_cast<size_t>(kCreditHeaderBytes));
        uint32_t v32;
        v32 = kCreditMatrixVersion;
        std::memcpy(base_ + kCreditOffVersion, &v32, sizeof(v32));
        v32 = kCreditMaxParties;
        std::memcpy(base_ + kCreditOffMaxParties, &v32, sizeof(v32));
        magic_cell->store(kCreditMatrixMagic, std::memory_order_release);
    } else {
        // Attach: spin (bounded ~5s) until the creator's magic lands.
        const auto* magic_cell = reinterpret_cast<const std::atomic<uint64_t>*>(
            base_ + kCreditOffMagic);
        const auto deadline_ns = [] {
            timespec ts {};
            ::clock_gettime(CLOCK_MONOTONIC, &ts);
            return static_cast<int64_t>(ts.tv_sec) * 1'000'000'000LL +
                   ts.tv_nsec + 5'000'000'000LL;
        }();
        while (magic_cell->load(std::memory_order_acquire) !=
               kCreditMatrixMagic) {
            timespec ts {};
            ::clock_gettime(CLOCK_MONOTONIC, &ts);
            if (static_cast<int64_t>(ts.tv_sec) * 1'000'000'000LL +
                    ts.tv_nsec > deadline_ns) {
                close();
                return false;
            }
        }
        uint32_t v32;
        std::memcpy(&v32, base_ + kCreditOffVersion, sizeof(v32));
        if (v32 != kCreditMatrixVersion) {
            close();
            return false;
        }
        std::memcpy(&v32, base_ + kCreditOffMaxParties, sizeof(v32));
        if (v32 != kCreditMaxParties) {
            close();
            return false;
        }
    }
    return true;
}

void BilateralCreditMatrix::close() noexcept {
    if (base_ != nullptr) {
        ::munmap(base_, map_size_);
        base_ = nullptr;
    }
    map_size_ = 0;
}

}  // namespace exch
