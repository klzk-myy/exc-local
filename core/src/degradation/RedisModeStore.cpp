// Task 2.3.6 — Redis binding of ModeStore. Write = single MSET (atomic:
// all three keys land or none); read = MGET + strict value validation.

#include "degradation/RedisModeStore.hpp"

#include <cstdlib>
#include <string>

#include "redis/RespClient.hpp"

namespace exch {
namespace {

constexpr std::string_view kKeyMode = "system:degradation:mode";
constexpr std::string_view kKeyEnteredAt = "system:degradation:entered_at";
constexpr std::string_view kKeyReason = "system:degradation:reason";

}  // namespace

RedisModeStore::RedisModeStore(RespClient* client) noexcept : client_(client) {}

bool RedisModeStore::write(const ModeRecord& rec) noexcept {
    if (client_ == nullptr) return false;
    const std::string entered = std::to_string(rec.entered_at_ms);
    return client_->mset({
        {kKeyMode, to_string(rec.mode)},
        {kKeyEnteredAt, entered},
        {kKeyReason, rec.reason},
    });
}

bool RedisModeStore::read(ModeRecord* out) noexcept {
    if (out == nullptr || client_ == nullptr) return false;
    RespValue v;
    if (!client_->mget({kKeyMode, kKeyEnteredAt, kKeyReason}, &v) ||
        v.is_error() || v.type != RespValue::Type::Array ||
        v.items.size() != 3) {
        return false;
    }
    // Absent mode key ⇒ Normal (spec §2.4 default — absence is NOT an error).
    if (v.items[0].is_nil()) {
        *out = ModeRecord{};
        return true;
    }
    std::string_view mode_sv;
    if (!v.items[0].as_string(&mode_sv)) return false;
    ModeRecord rec;
    if (!mode_from_string(mode_sv, &rec.mode)) {
        // A value we cannot classify is foreign/corrupt state — fail the read
        // so callers trip the staleness path instead of trusting it.
        return false;
    }
    if (!v.items[1].is_nil()) {
        std::string_view s;
        if (!v.items[1].as_string(&s)) return false;
        char* end = nullptr;
        const long long ms = std::strtoll(std::string(s).c_str(), &end, 10);
        rec.entered_at_ms = end != nullptr && *end == '\0' ? ms : 0;
    }
    if (!v.items[2].is_nil()) {
        std::string_view s;
        if (!v.items[2].as_string(&s)) return false;
        rec.reason.assign(s);
    }
    *out = std::move(rec);
    return true;
}

}  // namespace exch
