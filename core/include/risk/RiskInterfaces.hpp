#pragma once

// Pre-trade risk dependency seam (Task 2.3.3, spec §3.3): every input the
// 14-check pipeline consumes arrives through one of these pure-virtual,
// noexcept, allocation-free interfaces. Engine wiring binds production
// providers (Phase-03 balance service views, Phase-14 KYC, Phase-19 margin/
// positions); tests bind mocks. A provider returning its documented
// "unknown" sentinel fails the corresponding check CLOSED (spec §2.7 strict
// fail-closed pessimism) — the checker never treats a missing read as a pass.
//
// Rejection codes are static string literals (never heap-allocated) matching
// the spec §23 registry where a registered code exists; codes marked "§23
// neighbor" are Phase-02-local and land in the Task 5.3.21 registry pass.

#include <cstdint>

#include "book/Instrument.hpp"
#include "book/Order.hpp"
#include "utils/error_severity.hpp"  // kCodeArithmeticOverflowDetected

namespace exch {

// --- Verdict ---------------------------------------------------------------

// Zero-alloc verdict: code/detail are always string literals or nullptr.
struct RiskVerdict {
    bool pass;
    const char* code;    // nullptr iff pass; else a §23-style static code
    const char* detail;  // static diagnostic string; nullptr allowed
};

[[nodiscard]] inline constexpr RiskVerdict verdict_pass() noexcept {
    return {true, nullptr, nullptr};
}
[[nodiscard]] inline constexpr RiskVerdict verdict_reject(
    const char* code, const char* detail) noexcept {
    return {false, code, detail};
}

// --- Rejection codes (check → code mapping; spec §23 status in comments) ---

inline constexpr char kCodeAccountSuspended[] = "ACCOUNT_SUSPENDED";
inline constexpr char kCodeAccountFrozen[] = "ACCOUNT_FROZEN";       // §23: 403
inline constexpr char kCodeAccountInactive[] = "ACCOUNT_INACTIVE";   // CLOSED/unknown
inline constexpr char kCodeInstrumentSuspended[] = "INSTRUMENT_SUSPENDED";  // §23: 409
inline constexpr char kCodeInstrumentHalted[] = "INSTRUMENT_HALTED";        // §23: 409
inline constexpr char kCodeInstrumentDelisted[] = "INSTRUMENT_DELISTED";    // §23: 409
inline constexpr char kCodeInstrumentInactive[] = "INSTRUMENT_INACTIVE";    // DRAFT/etc.
inline constexpr char kCodeInstrumentCancelOnly[] = "INSTRUMENT_CANCEL_ONLY"; // §23: 409
inline constexpr char kCodeInstrumentRestricted[] = "INSTRUMENT_RESTRICTED";
inline constexpr char kCodeInsufficientBalance[] = "INSUFFICIENT_BALANCE";  // §23: 400
inline constexpr char kCodePositionLimitExceeded[] = "POSITION_LIMIT_EXCEEDED";
inline constexpr char kCodeRateLimitExceeded[] = "RATE_LIMIT_EXCEEDED";     // §23: 429
// ↑ §23 pins RATE_LIMIT_EXCEEDED to this exact check (per-account order-rate
// collar, Task 2.3.3 #5) — distinct from RATE_LIMIT_TIER_EXCEEDED (Phase-05
// tiered API quota).
inline constexpr char kCodePriceOutOfBand[] = "PRICE_OUT_OF_BAND";          // §23: 400
inline constexpr char kCodeOrderQtyExceedsMax[] = "ORDER_QTY_EXCEEDS_MAX";
inline constexpr char kCodeOrderQtyBelowMin[] = "ORDER_QTY_BELOW_MIN";
// §23 registers MARGIN_INSUFFICIENT (400) for this check ("Phase-02 Task
// 2.3.3 Pre-Trade Risk — post-fill margin check"); MARGIN_CHECK_FAILED is the
// task-contract name — Task 5.3.21 registers the alias.
inline constexpr char kCodeMarginCheckFailed[] = "MARGIN_CHECK_FAILED";
inline constexpr char kCodeCircuitBreakerOpen[] = "CIRCUIT_BREAKER_OPEN";   // §23: 503
// §23 neighbors: KYC_REQUIRED (403) / KYC_TIER_EXCEEDED (403); the task name
// is emitted, registry alias lands in Task 5.3.21.
inline constexpr char kCodeKycTierInsufficient[] = "KYC_TIER_INSUFFICIENT";
inline constexpr char kCodeTickSizeViolation[] = "TICK_SIZE_VIOLATION";
inline constexpr char kCodeLotSizeViolation[] = "LOT_SIZE_VIOLATION";
inline constexpr char kCodeMinNotionalViolation[] = "MIN_NOTIONAL_VIOLATION"; // §23: 400
inline constexpr char kCodePostOnlyViolation[] = "POST_ONLY_VIOLATION";     // §23: 400
inline constexpr char kCodeReduceOnlyViolation[] = "REDUCE_ONLY_VIOLATION"; // §23: 400
inline constexpr char kCodeStpNoneNotPermitted[] = "STP_NONE_NOT_PERMITTED"; // §23: 409
inline constexpr char kCodeInvalidRequest[] = "INVALID_REQUEST";            // §23: 400
inline constexpr char kCodeBilateralCreditExhausted[] =
    "BILATERAL_CREDIT_EXHAUSTED";  // §23: 409 (spec §3.3b screened liquidity)

// --- Account-side state (one record per account; Phase-03/14 own impls) ----

// spec §5.2 accounts.status ENUM('ACTIVE','SUSPENDED','FROZEN','CLOSED')
// plus UNKNOWN — the provider's "no such account" sentinel (fail-closed).
enum class AccountStatus : uint8_t { ACTIVE, SUSPENDED, FROZEN, CLOSED, UNKNOWN };

// spec §5.2 accounts.kyc_tier ENUM('T0','T1','T2'); ordered — comparison is
// ordinal (T0 < T1 < T2).
enum class KycTier : uint8_t { T0 = 0, T1 = 1, T2 = 2 };

// spec §5.2 accounts.client_category ENUM('RETAIL','PROFESSIONAL',
// 'ELIGIBLE_COUNTERPARTY') — MiFID II categorization (§14.2).
enum class ClientCategory : uint8_t {
    RETAIL,
    PROFESSIONAL,
    ELIGIBLE_COUNTERPARTY,
};

// Which balance bucket an order debits (spec §5.3 spot wallets): a BUY spends
// quote-currency notional; a SELL spends base-currency quantity. Margin/
// derivative debits stay on QUOTE (Phase-19 refines per-product).
enum class BalanceUnit : uint8_t { BASE, QUOTE };

class IAccountState {
public:
    virtual ~IAccountState() = default;

    // Order-entry gate (check 1). UNKNOWN for any unrecognized id — never
    // claim ACTIVE for an account you cannot see.
    [[nodiscard]] virtual AccountStatus status(
        uint64_t account_id) const noexcept = 0;

    // Available (unreserved) balance in the bucket `unit` resolves to for
    // (account_id, instrument_id), at the 10^8 fixed-point scale. Return < 0
    // for "unknown/unavailable" → the caller rejects fail-closed.
    [[nodiscard]] virtual int64_t available_balance(
        uint64_t account_id, uint64_t instrument_id,
        BalanceUnit unit) const noexcept = 0;

    // Currently open position count on the account (check 4).
    [[nodiscard]] virtual uint32_t open_position_count(
        uint64_t account_id) const noexcept = 0;

    // accounts.default_stp_mode (migration 094, Task 2.3.21). Return the
    // kStpModeUnset sentinel (cast) when unset → resolution falls to
    // CANCEL_NEWEST.
    [[nodiscard]] virtual StpMode default_stp_mode(
        uint64_t account_id) const noexcept = 0;

    // MiFID II client category (check 14 STP-NONE gate, §24 #274).
    [[nodiscard]] virtual ClientCategory client_category(
        uint64_t account_id) const noexcept = 0;

    // KYC tier (check 10; Phase-14 Task 14.3.4 owns the real lifecycle).
    [[nodiscard]] virtual KycTier kyc_tier(
        uint64_t account_id) const noexcept = 0;
};

// --- Position state (check 13 reduce_only; Phase-19 owns the real book) ----

class IPositionState {
public:
    virtual ~IPositionState() = default;
    // Signed net position in 10^8 units: >0 long, <0 short, 0 flat.
    [[nodiscard]] virtual int64_t net_position_units(
        uint64_t account_id, uint64_t instrument_id) const noexcept = 0;
};

// --- Market state (checks 6/9/13) -------------------------------------------
//
// Read-only market view. Prices are int64 10^8 ticks; 0 means "no data" and
// the consuming check skips rather than rejects (documented per check: a
// band/breaker without a reference cannot be violated; ORDERBOOK-level
// fail-closed behavior for empty books is owned by Task 2.3.13, not here).
class IMarketState {
public:
    virtual ~IMarketState() = default;

    // Last trade/reference price (check 6 band center).
    [[nodiscard]] virtual int64_t last_price_ticks(
        uint64_t instrument_id) const noexcept = 0;

    // Reference price at-or-before `now_ns - window_ns` (check 9 Phase-2
    // circuit-breaker placeholder; Phase-13 replaces with the five-tier
    // system). 0 = history unavailable → check passes.
    [[nodiscard]] virtual int64_t price_ticks_ago(
        uint64_t instrument_id, uint64_t window_ns,
        uint64_t now_ns) const noexcept = 0;

    // Opposite-side quotes for marketability (check 13 post_only). 0 = that
    // side is empty → a limit order cannot cross it.
    [[nodiscard]] virtual int64_t best_bid_ticks(
        uint64_t instrument_id) const noexcept = 0;
    [[nodiscard]] virtual int64_t best_ask_ticks(
        uint64_t instrument_id) const noexcept = 0;
};

// --- Margin seam (check 8; Phase-19 Task 19.3.1 swaps the real engine in) ---
//
// Bind one of these and the check-8 placeholder math is bypassed entirely —
// call sites (engine admission) are untouched. `notional_units` is the
// order's quote notional already computed by the checker (<= 0 when a market
// order has no reference price to price it).
class IMarginCheck {
public:
    virtual ~IMarginCheck() = default;
    [[nodiscard]] virtual bool passes(
        const Order& order, const Instrument& instrument,
        const IAccountState& accounts,
        int64_t notional_units) const noexcept = 0;
};

// --- Bilateral-credit admission screen (optional; spec §3.3b) ---------------
//
// The BilateralCreditMatrix is a match-time consume-or-skip filter owned by
// the matching engine (B1/SelfTradeGuard territory). When a deployment wants
// an *admission-time* pre-screen it binds the matrix plus this account→party
// map; the checker then fails fast when the taker has no mutual headroom with
// ANY counterparty. Unmapped accounts (anonymous retail flow) skip the
// screen — spec §3.3b routes only credit-screened institutional flow here.
class IPartyMap {
public:
    virtual ~IPartyMap() = default;
    // Matrix party id for the account, or >= kCreditMaxParties when the
    // account is not credit-screened (screen skipped).
    [[nodiscard]] virtual uint32_t credit_party_id(
        uint64_t account_id) const noexcept = 0;
};

// --- Tunables (Task 2.3.9 defaults; all integer) -----------------------------

struct RiskConfig {
    // Check 5 — per-account order-rate collar, token bucket, IN-PROCESS
    // (remediation #35: a Redis round-trip violates the §3.3 no-IPC mandate
    // and the <10µs budget). 0 = reject-everything collar (admin kill
    // semantic — deterministic, documented).
    uint32_t order_rate_per_sec = 50;
    uint32_t order_rate_burst = 50;  // bucket capacity in orders (== rate)
    // Check 4 — max concurrent open positions per account. 0 = unlimited.
    uint32_t max_open_positions = 500;
    // Check 8 stub — required = notional * min_margin_ratio_pct_x100 / 10'000.
    // 334 == 3.34% ≈ ESMA retail 30:1 major-pair floor. Phase-19 replaces via
    // IMarginCheck without touching call sites.
    int64_t min_margin_ratio_pct_x100 = 334;
    // Check 10 stub — minimum tier to trade at all. T0 (default) gates
    // nothing: the Phase-2 contract treats all accounts as fully tiered;
    // Phase-14 raises this per instrument/category.
    KycTier required_kyc_tier = KycTier::T0;
    // Check 9 stub — reject when |last − ref(window)| / ref > move pct.
    uint64_t circuit_window_ns = 60'000'000'000ULL;  // 60s
    int64_t circuit_move_pct_x100 = 500;             // 5.00%
};

// --- Per-call context ---------------------------------------------------------
//
// now_ns is injected (TIME_TICK/ingress stamp discipline, Task 2.3.10): the
// checker NEVER reads a system clock — replay determinism is preserved and
// tests drive time explicitly. instrument is bound per call (a shard serves
// many books); nullptr fails check 2 closed.
struct CheckContext {
    const Instrument* instrument = nullptr;
    uint64_t now_ns = 0;
};

}  // namespace exch
