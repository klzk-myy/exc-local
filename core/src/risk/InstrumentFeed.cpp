// Phase-15 — InstrumentFeed control-plane parsers (see header for the
// Redis contract). All parsing is hand-rolled, allocation-free on the
// matched paths and total: ANY malformed structure returns false so the
// refresher can mark the feed unverifiable (fail closed).

#include "risk/InstrumentFeed.hpp"

#include <cctype>
#include <cstdlib>

namespace exch {

namespace {

[[nodiscard]] bool parse_i64(std::string_view s, int64_t* out) noexcept {
    if (s.empty() || s.size() > 20 || out == nullptr) return false;
    int64_t v = 0;
    std::size_t i = 0;
    bool neg = false;
    if (s[0] == '-') { neg = true; i = 1; }
    if (i == s.size()) return false;
    for (; i < s.size(); ++i) {
        const char c = s[i];
        if (c < '0' || c > '9') return false;
        v = v * 10 + (c - '0');   // bounded: <= 19 digits
    }
    *out = neg ? -v : v;
    return true;
}

// "DOW HH:MM" -> seconds-of-week. DOW ∈ SUN..SAT (publisher's casing).
[[nodiscard]] bool parse_sow(std::string_view s, int64_t* out) noexcept {
    if (s.size() < 8) return false;
    static constexpr std::string_view kDays[7] = {
        "SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"};
    int dow = -1;
    for (int d = 0; d < 7; ++d) {
        if (s.substr(0, 3) == kDays[d]) { dow = d; break; }
    }
    if (dow < 0 || s[3] != ' ' || s.size() < 9) return false;
    const std::string_view hh = s.substr(4, 2);
    if (s[6] != ':') return false;
    const std::string_view mm = s.substr(7, 2);
    if (!std::isdigit(static_cast<unsigned char>(hh[0])) ||
        !std::isdigit(static_cast<unsigned char>(hh[1])) ||
        !std::isdigit(static_cast<unsigned char>(mm[0])) ||
        !std::isdigit(static_cast<unsigned char>(mm[1]))) {
        return false;
    }
    const int h = (hh[0] - '0') * 10 + (hh[1] - '0');
    const int m = (mm[0] - '0') * 10 + (mm[1] - '0');
    if (h > 23 || m > 59) return false;
    *out = static_cast<int64_t>(dow) * 86400 + h * 3600 + m * 60;
    return true;
}

// "HH:MM" -> seconds-of-day.
[[nodiscard]] bool parse_sod(std::string_view s, int64_t* out) noexcept {
    if (s.size() != 5 || s[2] != ':') return false;
    if (!std::isdigit(static_cast<unsigned char>(s[0])) ||
        !std::isdigit(static_cast<unsigned char>(s[1])) ||
        !std::isdigit(static_cast<unsigned char>(s[3])) ||
        !std::isdigit(static_cast<unsigned char>(s[4]))) {
        return false;
    }
    const int h = (s[0] - '0') * 10 + (s[1] - '0');
    const int m = (s[3] - '0') * 10 + (s[4] - '0');
    if (h > 23 || m > 59) return false;
    *out = h * 3600 + m * 60;
    return true;
}

// "YYYY-MM-DD" -> days since 1970-01-01 (Howard Hinnant's civil algorithm,
// integer-only).
[[nodiscard]] bool parse_epoch_day(std::string_view s, int64_t* out) noexcept {
    if (s.size() != 10 || s[4] != '-' || s[7] != '-') return false;
    for (std::size_t i : {0u, 1u, 2u, 3u, 5u, 6u, 8u, 9u}) {
        if (!std::isdigit(static_cast<unsigned char>(s[i]))) return false;
    }
    int64_t y = (s[0] - '0') * 1000 + (s[1] - '0') * 100 +
                (s[2] - '0') * 10 + (s[3] - '0');
    const int64_t m = (s[5] - '0') * 10 + (s[6] - '0');
    const int64_t d = (s[8] - '0') * 10 + (s[9] - '0');
    if (m < 1 || m > 12 || d < 1 || d > 31) return false;
    y -= m <= 2;
    const int64_t era = (y >= 0 ? y : y - 399) / 400;
    const unsigned yoe = static_cast<unsigned>(y - era * 400);
    const unsigned doy = (153 * (m + (m > 2 ? -3 : 9)) + 2) / 5 +
                         static_cast<unsigned>(d) - 1;
    const unsigned doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    *out = era * 146097 + static_cast<int64_t>(doe) - 719468;
    return true;
}

// Extracts the string value of "key":"value" — minimal JSON object scan
// (control-plane cadence; the contract emits flat keys only).
[[nodiscard]] bool json_string(std::string_view j, std::string_view key,
                               std::string_view* out) noexcept {
    std::string pat;
    pat.reserve(key.size() + 3);
    pat.push_back('"');
    pat.append(key);
    pat.push_back('"');
    const std::size_t k = j.find(pat);
    if (k == std::string_view::npos) return false;
    std::size_t p = j.find(':', k + pat.size());
    if (p == std::string_view::npos) return false;
    ++p;
    while (p < j.size() && (j[p] == ' ' || j[p] == '\t')) ++p;
    if (p >= j.size() || j[p] != '"') return false;
    const std::size_t e = j.find('"', p + 1);
    if (e == std::string_view::npos) return false;
    *out = j.substr(p + 1, e - p - 1);
    return true;
}

[[nodiscard]] bool json_bool(std::string_view j, std::string_view key,
                             bool* out, bool* present) noexcept {
    std::string pat;
    pat.reserve(key.size() + 3);
    pat.push_back('"');
    pat.append(key);
    pat.push_back('"');
    const std::size_t k = j.find(pat);
    if (k == std::string_view::npos) {
        if (present != nullptr) *present = false;
        return true;
    }
    if (present != nullptr) *present = true;
    const std::size_t p = j.find(':', k + pat.size());
    if (p == std::string_view::npos) return false;
    if (j.substr(p + 1, 4) == "true") { *out = true; return true; }
    if (j.substr(p + 1, 5) == "false") { *out = false; return true; }
    return false;
}

// Iterate the top-level objects inside the "overrides":[{..},{..}] array —
// the contract emits flat objects so brace matching suffices.
[[nodiscard]] bool parse_overrides(std::string_view j,
                                   MarketHours* out) noexcept {
    const std::size_t k = j.find("\"overrides\"");
    if (k == std::string_view::npos) return true;  // absent -> no overrides
    const std::size_t open = j.find('[', k);
    if (open == std::string_view::npos) return false;
    std::size_t p = open + 1;
    while (p < j.size() && j[p] != ']') {
        if (j[p] != '{') { ++p; continue; }
        const std::size_t close = j.find('}', p);
        if (close == std::string_view::npos) return false;
        if (out->override_count >= MarketHours::kMaxOverrides) return false;
        const std::string_view obj = j.substr(p, close - p + 1);
        MarketHoursOverride ov{};
        std::string_view date;
        if (!json_string(obj, "date", &date) ||
            !parse_epoch_day(date, &ov.epoch_day)) {
            return false;
        }
        bool closed = false;
        bool have_closed = false;
        if (!json_bool(obj, "closed", &closed, &have_closed)) return false;
        ov.closed = have_closed && closed;
        if (!ov.closed) {
            std::string_view o, c;
            if (!json_string(obj, "open", &o) ||
                !json_string(obj, "close", &c) ||
                !parse_sod(o, &ov.open_sod) ||
                !parse_sod(c, &ov.close_sod)) {
                return false;
            }
        }
        out->overrides[out->override_count++] = ov;
        p = close + 1;
    }
    if (p >= j.size()) return false;  // unterminated array
    return true;
}

}  // namespace

bool parse_instrument_status(std::string_view word,
                             InstrumentStatus* out) noexcept {
    if (out == nullptr) return false;
    if (word == "ACTIVE")      { *out = InstrumentStatus::ACTIVE;      return true; }
    if (word == "CANCEL_ONLY") { *out = InstrumentStatus::CANCEL_ONLY; return true; }
    if (word == "SUSPENDED")   { *out = InstrumentStatus::SUSPENDED;   return true; }
    if (word == "HALTED")      { *out = InstrumentStatus::HALTED;      return true; }
    if (word == "RESTRICTED")  { *out = InstrumentStatus::RESTRICTED;  return true; }
    if (word == "DELISTED")    { *out = InstrumentStatus::DELISTED;    return true; }
    if (word == "DRAFT")       { *out = InstrumentStatus::DRAFT;       return true; }
    return false;
}

bool parse_auction_call(std::string_view value,
                        int64_t* deadline_ns) noexcept {
    if (deadline_ns == nullptr) return false;
    std::string_view num;
    if (value.substr(0, 5) == "CALL:")        num = value.substr(5);
    else if (value.substr(0, 7) == "EXTEND:") num = value.substr(7);
    else return false;
    return parse_i64(num, deadline_ns) && *deadline_ns > 0;
}

bool parse_market_hours_json(std::string_view j, MarketHours* out) noexcept {
    if (out == nullptr || j.empty()) return false;
    if (j.front() != '{') return false;
    MarketHours h{};  // canonical defaults; keys below may replace them
    std::string_view v;
    if (json_string(j, "open_utc", &v) && !parse_sow(v, &h.open_sow)) {
        return false;
    }
    if (json_string(j, "close_utc", &v) && !parse_sow(v, &h.close_sow)) {
        return false;
    }
    if (json_string(j, "pre_open_utc", &v) && !parse_sow(v, &h.pre_open_sow)) {
        return false;
    }
    if (!parse_overrides(j, &h)) return false;
    *out = h;
    return true;
}

}  // namespace exch
