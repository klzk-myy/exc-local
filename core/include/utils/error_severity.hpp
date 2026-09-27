#pragma once

// L0–L3 error severity hierarchy (Task 1.3.12, spec §2.7.2) — the foundation
// types enforcing Strict Fail-Closed Zero-Loss Pessimism (spec §2.7.1):
//
//   L0  Critical/Fatal fault      → immediate core halt (SIGTERM), crash
//                                   diagnostics + WAL flush, warm-standby
//                                   failover, P0 pager alert.
//   L1  Systemic/Infrastructure   → degradation mode transition
//                                   (Normal→ReadOnly/Throttled/Maintenance),
//                                   circuit-breaker trip, P1.
//   L2  Transaction/State boundary→ synchronous atomic rejection, zero
//                                   side-effects, reservation rollback, P2.
//   L3  Edge/Protocol validation  → fast rejection at the API/FIX gateway
//                                   before IPC/matching, P3.
//
// This header also owns the §23 error-code constants emitted by Task 1.3.12
// primitives plus CoreError — a non-allocating coded exception base whose
// members point exclusively at string literals, so throwing it from a
// resource-exhaustion path never touches the heap (spec §3.6.1).

#include <cstdint>
#include <exception>

namespace exch {

enum class Severity : uint8_t {
    L0 = 0,  // Critical/Fatal — immediate core halt, P0
    L1 = 1,  // Systemic degradation — mode transition / circuit breaker, P1
    L2 = 2,  // Transaction boundary — atomic reject + rollback, P2
    L3 = 3,  // Edge/protocol — gateway fast reject, P3
};

// Named constants matching the spec §2.7.2 tier names (Go parity:
// services/pkg/errors/severity.go SeverityL0..SeverityL3).
inline constexpr Severity SeverityL0 = Severity::L0;
inline constexpr Severity SeverityL1 = Severity::L1;
inline constexpr Severity SeverityL2 = Severity::L2;
inline constexpr Severity SeverityL3 = Severity::L3;

[[nodiscard]] inline const char* severity_name(Severity s) noexcept {
    switch (s) {
        case Severity::L0: return "L0";
        case Severity::L1: return "L1";
        case Severity::L2: return "L2";
        case Severity::L3: return "L3";
    }
    return "L?";
}

// Pager/alert priority per spec §2.7.2 (L0→P0 … L3→P3).
[[nodiscard]] inline const char* severity_priority(Severity s) noexcept {
    switch (s) {
        case Severity::L0: return "P0";
        case Severity::L1: return "P1";
        case Severity::L2: return "P2";
        case Severity::L3: return "P3";
    }
    return "P?";
}

// L0 faults halt the matching core immediately (spec §2.7.2).
[[nodiscard]] inline bool is_fatal(Severity s) noexcept {
    return s == Severity::L0;
}

// --- §23 error codes owned by Task 1.3.12 ---------------------------------
// Canonical registration of every emitted code + HTTP status lands in
// Phase-05 Task 5.3.21; these literals are the code of record until then.
inline constexpr char kCodeArithmeticOverflowDetected[] =
    "ARITHMETIC_OVERFLOW_DETECTED";  // HTTP 400, L2 (spec §3.6.2)
inline constexpr char kCodeOrderBookCapacityExceeded[] =
    "ORDER_BOOK_CAPACITY_EXCEEDED";  // HTTP 503, L2 (spec §3.6.1)
inline constexpr char kCodeTimeSyncLossHalt[] =
    "TIME_SYNC_LOSS_HALT";           // HTTP 503, L0 (spec §2.7.2, RTS 25)

// CoreError is a coded, severity-tagged exception that never allocates:
// code_/message_ are string-literal pointers only, so both construction and
// what() are allocation-free — safe to throw on the pool-exhaustion path.
class CoreError : public std::exception {
public:
    // Not constexpr: std::exception's ctor is non-constexpr (GCC 11), so a
    // literal-type subclass is impossible — constexpr here fails to compile.
    CoreError(const char* code, const char* message,
              Severity severity, int http_status) noexcept
        : code_(code), message_(message),
          severity_(severity), http_status_(http_status) {}

    [[nodiscard]] const char* what() const noexcept override { return message_; }
    [[nodiscard]] const char* code() const noexcept { return code_; }
    [[nodiscard]] Severity severity() const noexcept { return severity_; }
    [[nodiscard]] int http_status() const noexcept { return http_status_; }

private:
    const char* code_;
    const char* message_;
    Severity severity_;
    int http_status_;
};

// Thrown when a pre-allocated arena (MemoryPool, order-book level/node pool)
// is exhausted → ORDER_BOOK_CAPACITY_EXCEEDED (HTTP 503, spec §3.6.1).
// The submission is rejected atomically with zero side-effects (L2); the
// throw itself performs no heap allocation.
class OrderBookCapacityExceeded final : public CoreError {
public:
    OrderBookCapacityExceeded() noexcept
        : CoreError(kCodeOrderBookCapacityExceeded,
                    "order book capacity exceeded", Severity::L2, 503) {}
};

// Thrown when clock drift exceeds the 100µs bound or synchronization is lost
// → TIME_SYNC_LOSS_HALT (HTTP 503, spec §2.7.2 L0 trigger, MiFID II RTS 25).
// L0: the matching core must halt, not degrade.
class TimeSyncLossHalt final : public CoreError {
public:
    TimeSyncLossHalt() noexcept
        : CoreError(kCodeTimeSyncLossHalt, "time synchronization lost",
                    Severity::L0, 503) {}
};

}  // namespace exch
