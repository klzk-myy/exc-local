// Embedded spec §23 error-code table (Task 5.3.21).
//
// The rows below transcribe the §23 registry table in
// docs/Specification - Complete Exchange System Suite.md (192 table rows)
// plus the §27.1-matrix-resident codes the owner-resolvability rule
// accepts via the alternate branch — 198 emitted codes total; count
// updated with the Phase-18 Tasks 18.3.7/18.3.10/18.3.13 landings —
// historical remediation counts noted in spec §23's
// registration-history paragraph). specRow extracts the machine-useful
// fields from the
// spec's prose description at load time:
//   - Owner:        first "Phase-NN Task N.N.N" (or bare "Phase-NN")
//     citation in the description, when present.
//   - Reserved:     description carries the "reserved, never emitted" marker.
//   - SupersededBy: description carries "superseded by `CODE`" (DEPRECATED rows).
//   - AliasOf:      description carries "**ALIAS** of `CODE`".
//
// Codes without a Phase citation in their description are owner-resolvable
// via the alternate §23 branch ("the code token appears in a phase plan") —
// CI greps phase docs; UnresolvedOwners lists them for that check.
//
// DO NOT hand-edit rows to fix spec drift: §23 is the contract; edit the
// spec (with a remediation note) and re-transcribe.
package errs

import (
	"regexp"
	"strings"
)

// CodeDef is one row of the spec §23 registry: the canonical mapping from a
// machine-readable error code to its HTTP status, human description and
// owning task.
type CodeDef struct {
	Code         string `json:"code"`
	HTTPStatus   int    `json:"http_status"`
	Description  string `json:"description"`
	Owner        string `json:"owner,omitempty"`         // e.g. "Phase-05 Task 5.3.24"
	AliasOf      string `json:"alias_of,omitempty"`      // spec-pinned alias target (e.g. MARKET_SLIPPAGE_EXCEEDED → SLIPPAGE_EXCEEDED)
	SupersededBy string `json:"superseded_by,omitempty"` // DEPRECATED rows: the replacement code
	Reserved     bool   `json:"reserved,omitempty"`      // "reserved, never emitted"
	Spec         bool   `json:"spec"`                    // true = row transcribed from spec §23
}

// ownerTaskRe matches a full "Phase-05 Task 5.3.24" owner citation;
// ownerPhaseRe matches a bare "Phase-15" mention (fallback when the
// description cites only the phase — resolvable but coarser).
var (
	ownerTaskRe  = regexp.MustCompile(`Phase-\d+(?:\.\d+)? Task [\d.]+`)
	ownerPhaseRe = regexp.MustCompile(`Phase-\d+(?:\.\d+)?`)
)

// supersededRe matches "superseded by `CODE`".
var supersededRe = regexp.MustCompile("superseded by `([A-Z0-9_]+)`")

// aliasRe matches "**ALIAS** of `CODE`".
var aliasRe = regexp.MustCompile("\\*\\*ALIAS\\*\\* of `([A-Z0-9_]+)`")

// specRow builds a CodeDef from a spec §23 table row.
func specRow(code string, httpStatus int, desc string) CodeDef {
	d := CodeDef{Code: code, HTTPStatus: httpStatus, Description: desc, Spec: true}
	if m := ownerTaskRe.FindString(desc); m != "" {
		d.Owner = m
	} else if m := ownerPhaseRe.FindString(desc); m != "" {
		d.Owner = m
	}
	if strings.Contains(desc, "reserved, never emitted") {
		d.Reserved = true
	}
	if m := supersededRe.FindStringSubmatch(desc); m != nil {
		d.SupersededBy = m[1]
	}
	if m := aliasRe.FindStringSubmatch(desc); m != nil {
		d.AliasOf = m[1]
	}
	return d
}

// localRow builds a CodeDef for a code emitted by the gateway but not (yet)
// carrying a spec §23 row. Spec=false flags it for the docs-drift check: the
// long-term fix is a §23 row, not deletion. Currently unused — remediation #44
// registered all emitted codes into §23 — but retained for future emissions.
func localRow(code string, httpStatus int, owner, desc string) CodeDef {
	return CodeDef{Code: code, HTTPStatus: httpStatus, Description: desc,
		Owner: owner, Spec: false}
}

// specCodes is the verbatim spec §23 table (207 rows).
var specCodes = []CodeDef{
	specRow("INVALID_REQUEST", 400, "Malformed request body (Phase-05 Task 5.3.29 API Gateway & Load Balancer — centralized OpenAPI-schema request validation; remediation #19)"),
	specRow("UNAUTHORIZED", 401, "Missing or invalid auth token (Phase-05 Task 5.3.26 WebSocket Authentication Upgrade & In-Flight Token Renewal; REST JWT/OAuth per spec §8.4; remediation #19)"),
	specRow("FORBIDDEN", 403, "Insufficient permissions"),
	specRow("NOT_FOUND", 404, "Resource not found (Phase-05 Task 5.3.7 Route Registration System — emitted by every registered route; remediation #19)"),
	specRow("RATE_LIMIT_TIER_EXCEEDED", 429, "Rate limit hit"),
	specRow("INSUFFICIENT_BALANCE", 400, "Not enough available balance (Phase-02 Task 2.3.3 pre-trade check 2; Phase-05 Task 5.3.24 batch rejection; remediation #35 — owner citation added)"),
	specRow("MARGIN_INSUFFICIENT", 400, "Post-fill margin exceeds threshold (Phase-02 Task 2.3.3 Pre-Trade Risk — post-fill margin check; Phase-19 Task 19.3.1 Margin Modes; remediation #19)"),
	specRow("MARGIN_MODE_SWITCH_BLOCKED", 409, "Margin-mode switch rejected while open positions exist (Phase-19 Task 19.3.1 Margin Modes / Task 19.3.23 runtime change)"),
	specRow("MARGIN_CALL_EXCEEDED", 409, "Margin call active — position-increasing orders blocked for the episode (spec §13.6d; Phase-19 Task 19.3.3 margin-call order block)"),
	specRow("PRICE_OUT_OF_BAND", 400, "Price outside allowed band"),
	specRow("INSTRUMENT_SUSPENDED", 409, "Instrument not active"),
	specRow("INSTRUMENT_HALTED", 409, "Instrument halted"),
	specRow("INSTRUMENT_DELISTED", 409, "Instrument delisted"),
	specRow("MARKET_CLOSED", 409, "Order outside trading window (24/5) — added 2026-09-16 for Phase-15 Task 15.3.4"),
	specRow("ORDER_NOT_FOUND", 404, "Order does not exist (Phase-05 Task 5.3.7; Phase-15 Task 15.3.9 CANCEL_ONLY amend/cancel path; remediation #19)"),
	specRow("ORDER_REJECTED", 400, "Order rejected by risk check"),
	specRow("DEGRADED_MODE", 503, "System in degraded mode (Phase-02 Task 2.3.6 Degradation Mode Manager; remediation #19)"),
	specRow("MAINTENANCE_MODE", 503, "System in maintenance (Phase-02 Task 2.3.6 Degradation Mode Manager — Maintenance tier; remediation #19)"),
	specRow("CAPACITY_EXCEEDED", 503, "System at capacity"),
	specRow("CIRCUIT_BREAKER_OPEN", 503, "Circuit breaker tripped (Phase-13 Task 13.3.1 Five-Tier Circuit Breaker; Phase-13 Task 13.3.9 automated reset; remediation #19)"),
	specRow("WITHDRAWAL_COOLDOWN_ACTIVE", 422, "Withdrawal cooldown active — 30-minute same-bank-account cooldown after a completed withdrawal (Phase-11 Task 11.3.2 Withdrawal Flow step 5; the 15-minute figure is the separate confirmation window, spec §5.7; remediation #35 — supersedes the 15-minute mislabel in the prior description)"),
	specRow("ADDRESS_NOT_ALLOWLISTED", 422, "**DEPRECATED** — superseded by `BANK_ACCOUNT_NOT_VERIFIED` (Phase-11 Task 11.3.7 Beneficiary Registry; remediation #35). Retained for backward compatibility; new code must use `BANK_ACCOUNT_NOT_VERIFIED`."),
	specRow("GEO_BLOCKED", 403, "IP in restricted jurisdiction (Phase-21 Task 21.3.7 GDPR & Geo-Block; remediation #19)"),
	specRow("MARKET_ABUSE_DETECTED", 403, "Market abuse signal triggered; emitted on the enforcement surface (order rejection or account action) when a Phase-17 signal is confirmed (Phase-21 Task 21.3.8 Market-Abuse Enforcement; remediation #19, emission point pinned remediation #35)"),
	specRow("WASH_TRADE_DETECTED", 403, "Wash trade detected; same emission surface as `MARKET_ABUSE_DETECTED` (Phase-21 Task 21.3.8; remediation #19, emission point pinned remediation #35)"),
	specRow("LAYERING_DETECTED", 403, "Layering detected; same emission surface as `MARKET_ABUSE_DETECTED` (Phase-21 Task 21.3.8; remediation #19, emission point pinned remediation #35)"),
	specRow("SPREAD_ORDER_REJECTED", 400, "Spread order validation failed (Phase-16 Task 16.3.14 bracket/spread validation; remediation #35 — owner citation added)"),
	specRow("ENFORCEMENT_ACTION_EXISTS", 409, "Duplicate enforcement action on a surveillance signal — the UNIQUE(signal_id, action) ledger dedup refuses the second POST (Phase-21 Task 21.3.8; registered with the task landing)"),
	specRow("VARIATION_MARGIN_INSUFFICIENT", 400, "Variation margin shortfall (Phase-22 Task 22.3.7 Variation Margin; remediation #19)"),
	specRow("MAX_EXPOSURE_EXCEEDED", 400, "Exposure limit exceeded (Phase-19 Task 19.3.5 Exposure Limits; remediation #19)"),
	specRow("OPTION_ASSIGNMENT_FAILED", 409, "Option assignment failed (Phase-22 Task 22.3.10 Option Lifecycle — American intra-day assignment; remediation #19)"),
	specRow("FUTURE_SETTLEMENT_PENDING", 409, "Future settlement pending (Phase-03 Task 3.3.3 T+1/T+2 Settlement Instructions; remediation #19)"),
	specRow("CORPORATE_ACTION_SCHEDULED", 409, "Corporate action pending — **reserved, never emitted:** no corporate actions exist in fiat spot FX (spec §1 fiat-only scope); retained only to reserve the code; remediation #19"),
	specRow("FUNDING_RATE_ERROR", 503, "Funding rate computation error (Phase-11 Task 11.3.9 Funding Fee Schedule; the FX analog is the Phase-03 Task 3.3.11 overnight swap-rate engine; remediation #19)"),
	specRow("FUNDING_FEE_EXCEEDS_AMOUNT", 422, "Scheduled funding fee meets or exceeds the transaction amount; the movement is refused rather than posting a non-positive net credit/debit (Phase-11 Task 11.3.9 Funding Fee Schedule; fail-closed §2.7 — registered with the Task 11.3.9 landing)"),
	specRow("TOKEN_IP_FORBIDDEN", 403, "API token IP mismatch"),
	specRow("SANCTIONS_HIT", 403, "Sanctions screening positive (Phase-21 Task 21.3.1 Sanctions Screening / Task 21.3.10 C++ SanctionsHook; remediation #19)"),
	specRow("KYC_REQUIRED", 403, "KYC verification required; enforced at two points — Phase-05 Task 5.3.3 order submission (fast gateway pre-check, also on the WS `order.place` path) with fail-closed C++ core rejection (Phase-14 Task 14.3.4 KYC Lifecycle Management owns the lifecycle; remediation #19, enforcement points pinned remediation #35)"),
	specRow("TRADING_HALTED", 503, "Global trading halt active"),
	specRow("STALE_MODIFY", 409, "Stale order sequence on modify"),
	specRow("ACCOUNT_BUSY", 423, "Account mutex held by another operation (Phase-03 Task 3.3.1 Post-Trade Balance Service account mutex; HTTP 423 Locked; remediation #19)"),
	specRow("POST_ONLY_VIOLATION", 400, "Post-only order would execute immediately — added 2026-09-15"),
	specRow("REDUCE_ONLY_VIOLATION", 400, "Reduce-only order would increase exposure or no position exists — added 2026-09-15"),
	specRow("MIN_NOTIONAL_VIOLATION", 400, "Order notional below instrument minimum — added 2026-09-15"),
	specRow("OTR_LIMIT_EXCEEDED", 429, "Order-to-trade ratio limit breached (MiFID II RTS 9) — added 2026-09-15"),
	specRow("BANK_ACCOUNT_NOT_VERIFIED", 422, "Withdrawal beneficiary not registered/verified — added 2026-09-15 (Phase-11 Task 11.3.7 Beneficiary Bank-Account Registry; remediation #19)"),
	specRow("THIRD_PARTY_DEPOSIT_REJECTED", 422, "Inbound wire originator name fails the Jaro-Winkler ≥0.85 match vs verified KYC legal name; funds quarantined to 2150_SUSPENSE_DEPOSITS and returned to source (Phase-11 Task 11.3.11 Third-Party Deposit Fraud; §17.12.2 — description amended 2026-11-09, supersedes 'Deposit sender name does not match account holder')"),
	specRow("SESSION_NOT_ENTITLED", 403, "FIX session not entitled for account/instrument — added 2026-09-15"),
	specRow("NEGATIVE_BALANCE_PROTECTED", 422, "Operation would push a retail balance negative — added 2026-09-15 (Phase-19 Task 19.3.9 Retail Negative-Balance Protection; remediation #19)"),
	specRow("TRADE_BUST_PENDING", 409, "Trade under obvious-error review; settlement held — added 2026-09-15 (Phase-15 Task 15.3.5 Trade Bust & Price-Adjust Workflow; remediation #19)"),
	specRow("MM_OBLIGATION_BREACH", 429, "Market-maker quoting obligation breach — added 2026-09-15"),
	specRow("QUOTE_REQUEST_REJECTED", 400, "FIX MassQuote (35=i) rejected — non-firm semantics (non-zero hold time / last-look request) or malformed quote set/entry; venue is 100% firm liquidity per §6.4 / FX Global Code P17 (Phase-18 Task 18.3.7 FIX Mass Quoting; spec §9.4, §24 #128)"),
	specRow("MMP_TRIGGERED", 429, "Market-Maker-Protection sliding window tripped (mmp_max_fills fills inside mmp_window_ms) — remaining quotes for the session/instrument mass-cancelled (Phase-18 Task 18.3.10; spec §9.6, §24 #139)"),
	specRow("MMP_LOCKED_OUT", 403, "MM quotes rejected while an MMP-trigger lockout stands — requires explicit reset (FIX 35=a QuoteStatusRequest or REST admin) before quoting resumes (Phase-18 Task 18.3.10; spec §9.6, §24 #139)"),
	specRow("INVALID_SIGNATURE", 401, "HMAC request signature mismatch (§8.1) — added 2026-09-15"),
	specRow("TIMESTAMP_OUT_OF_WINDOW", 401, "HMAC timestamp outside 30s replay window (§8.1) — added 2026-09-15"),
	specRow("SESSION_THROTTLED", 429, "FIX session exceeded `max_msgs_per_sec` (§9.3) — added 2026-09-15"),
	specRow("PRODUCT_NOT_PERMITTED", 403, "Client category/appropriateness test does not permit product (§14.2) — added 2026-09-15"),
	specRow("LEGAL_DOC_REQUIRED", 403, "Executed ISDA/CSA/FMSB agreement required for instrument (§15.5) — added 2026-09-15"),
	specRow("EXERCISE_CUTOFF_PASSED", 409, "Manual exercise after 15:00 UTC expiry cutoff (§15.4) — added 2026-09-15"),
	specRow("TRADE_ALREADY_SETTLED", 409, "Bust/adjust rejected — settlement already dispatched (§5.29; Phase-15 Task 15.3.5; remediation #35 — owner citation added) — added 2026-09-15"),
	specRow("ALGO_NOT_CERTIFIED", 403, "Algo strategy lacks RTS 6 certification (§14.1) — added 2026-09-15"),
	specRow("RATE_LIMIT_EXCEEDED", 429, "Per-account order-rate collar breached (Phase-02 Task 2.3.3 check 5); distinct from `RATE_LIMIT_TIER_EXCEEDED` (tiered API quota) — registered 2026-09-15"),
	specRow("PB_NOP_LIMIT_EXCEEDED", 400, "Prime-broker NOP credit limit breached (§13.7, Phase-19 Task 19.3.7) — registered 2026-09-15"),
	specRow("PB_DSL_LIMIT_EXCEEDED", 400, "Prime-broker DSL daily-settlement limit breached (§13.7, Phase-19 Task 19.3.7) — registered 2026-09-15"),
	specRow("FIXING_CUTOFF_EXCEEDED", 409, "Fixing order submitted after T-15m pre-benchmark cutoff (Phase-16 Task 16.3.9) — registered 2026-09-15"),
	specRow("BILATERAL_CREDIT_EXCEEDED", 422, "No sufficient mutual counterparty credit for the product pool/value date (Phase-19 Task 19.3.10) — registered 2026-09-15"),
	specRow("CLIENT_MONEY_SHORTFALL", 503, "Client-money resource is below requirement; withdrawals/funding movements blocked pending remediation (Phase-24 Task 24.3.11) — registered 2026-09-15"),
	specRow("AUTH_EXPIRED", 401, "WS auth token expired without renewal; private channels torn down; WS close code 4019 (registered RFC 6455 private-use code, Phase-05 Task 5.3.26) — registered 2026-09-19"),
	specRow("WS_RATE_EXCEEDED", 429, "WS control-message rate limit exceeded; warning frame includes `retry_after_ms` (Phase-06 Task 6.3.7, §24 #215) — registered 2026-09-19"),
	specRow("WS_ABUSE_DETECTED", 403, "WS subscription churn abuse; session terminated (Phase-06 Task 6.3.7) — registered 2026-09-19"),
	specRow("UNSUPPORTED_PROTOCOL_VERSION", 400, "WS auth frame carried an unknown `protocol_version` (§8.6, Phase-05 Task 5.3.28) — registered 2026-09-19"),
	specRow("EXECUTION_RULE_PRICE_RANGE_EXCEEDED", 409, "Taker remainder expired before an execution outside its snapshotted reference-price collar (Phase-02 Task 2.3.17 Reference-Price Execution Collars; remediation #35 — owner citation added; the code was emitted by the task but never cited in §23)"),
	specRow("INSTRUMENT_CANCEL_ONLY", 409, "Instrument accepts cancellations only; new/replace/amend blocked"),
	specRow("CANCEL_REPLACE_PARTIAL_FAILURE", 409, "Atomic cancel-replace produced different cancel/new outcomes (Phase-05 Task 5.3.37 Atomic Cancel-Replace; remediation #19)"),
	specRow("ORDER_AMEND_REJECTED", 409, "Keep-priority amendment invalid, stale, or would increase quantity (Phase-05 Task 5.3.37 keep-priority amendment rules — price-change loses priority, qty-down preserves it; remediation #19)"),
	specRow("ASYMMETRIC_KEY_INVALID", 401, "Ed25519/RSA public key or signature failed validation (Phase-05 Task 5.3.38 Ed25519 and RSA API Keys; remediation #19)"),
	specRow("SBE_SCHEMA_RETIRED", 400, "Requested SBE schema is retired and no longer accepted (Phase-06 Task 6.3.18 Negotiated SBE lifecycle; Phase-18 Task 18.3.17; remediation #19)"),
	specRow("MULTI_VALIDATOR_REQUIRED", 409, "Client operation awaits the configured M-of-N approvals (Phase-12 Task 12.3.11 Client Multi-Validator / M-of-N Controls; remediation #19)"),
	specRow("QUOTE_QUANTITY_INVALID", 400, "Quote-denominated market-order amount or quantity combination is invalid (Phase-05 Task 5.3.39 Quote-Denominated Market Orders and Dry-Run Preview; remediation #19)"),
	specRow("UNSUPPORTED_ASSET_CLASS", 400, "Non-fiat currency or unsupported asset class requested (remediation #15) (Phase-19 Task 19.3.1 Margin Modes — non-fiat asset classes refused at pre-trade; remediation #19)"),
	specRow("TIME_SYNC_LOSS_HALT", 503, "PTP/NTP clock drift > 100µs; matching halted (MiFID II RTS 25, remediation #15)"),
	specRow("CRITICAL_BACKPRESSURE", 503, "Aeron IPC ring buffer > 95% watermark; ingress paused (remediation #15)"),
	specRow("ORDER_BOOK_CAPACITY_EXCEEDED", 503, "Static pre-allocated order book level/node pool exhausted (remediation #15; OrdRejReason corrected 16→99 per functional cluster review F12 — 16 is BrokerCredit in FIX 4.4)"),
	specRow("ARITHMETIC_OVERFLOW_DETECTED", 400, "Checked fixed-point price/volume calculation overflow/underflow (remediation #15)"),
	specRow("TRANSACTION_CONFLICT_RETRY_EXHAUSTED", 503, "PostgreSQL SERIALIZABLE 40001/40P01 retry budget exhausted (remediation #15)"),
	specRow("LEDGER_IMBALANCE_ABORT", 500, "Journal entry debits do not equal credits (GL zero-sum breach, remediation #15)"),
	specRow("AUDIT_HASH_CORRUPTION", 500, "Audit hash chain SHA-256 cryptographic link mismatch (remediation #15) (Phase-01 Task 1.3.8 Audit Hash Chain Infrastructure; verified on load by Phase-04 Task 4.3.5 Recovery Manager; remediation #19)"),
	specRow("FOK_NOT_FILLABLE", 400, "Fill-Or-Kill order cannot execute in full immediately at limit price (remediation #15) (Phase-02 Task 2.3.2 Matching Engine — FOK fills fully or cancels; remediation #19)"),
	specRow("PEGGED_PRICING_UNAVAILABLE", 409, "Pegged order pricing unavailable due to missing or crossed BBO (remediation #15)"),
	specRow("MARKET_SLIPPAGE_EXCEEDED", 400, "Market order slippage exceeds max_slippage_bps or price band; **ALIAS** of `SLIPPAGE_EXCEEDED` for the same event on the order-submission path — clients must treat the two codes interchangeably (remediation #15) (Phase-02 Task 2.3.15 Market Order Slippage Protection & Price Banding; remediation #19; alias pinning remediation #35). CI check enforces both codes always appear together in documentation (internal consistency audit F11)."),
	specRow("OCO_SIBLING_CANCEL_RACE", 409, "OCO sibling cancel race condition resolved in favor of first match (remediation #15)"),
	specRow("CONDITIONAL_TRIGGER_ORACLE_STALE", 409, "Mark/Index price trigger source staleness > 5s; trigger suspended (remediation #15)"),
	specRow("INVALID_LIFECYCLE_TRANSITION", 409, "Out-of-order instrument lifecycle state machine transition (remediation #15) (Phase-15 Task 15.3.1 Instrument Lifecycle State Machine; remediation #19)"),
	specRow("AUCTION_CLEARING_FAILED", 409, "Reopening auction failed to cross liquidity after maximum extensions (remediation #15)"),
	specRow("L3_SEQUENCE_GAP_DETECTED", 409, "Monotonic L3 stream sequence gap detected; snapshot recovery required (remediation #15)"),
	specRow("L3_CONSUMER_OVERRUN", 429, "L3 subscriber dropped due to outbound buffer saturation (remediation #15)"),
	specRow("OPTION_PRICING_CONVERGENCE_ERROR", 422, "Black-Scholes implied volatility numerical root-finding failed (remediation #15)"),
	specRow("VOLATILITY_SURFACE_ARBITRAGE", 422, "Option volatility surface violates calendar or butterfly spread bounds (remediation #15)"),
	specRow("OPTION_EXERCISE_MARGIN_SHORTFALL", 400, "Counterparty margin insufficient for ITM option exercise delivery (remediation #15) (Phase-22 Task 22.3.14 Vol Surface Arbitrage Rejection & Option Exercise Margin Failures; remediation #19)"),
	specRow("YIELD_CURVE_UNAVAILABLE", 503, "Benchmark yield curve missing or stale for forward/swap pricing (remediation #15) (Phase-19.5 Task 19.5.3.5 Interest-Rate / Yield-Curve Feeds; remediation #19)"),
	specRow("REPLAY_ATTACK_DETECTED", 401, "HMAC signature replay detected in replay cache window (remediation #15) (Phase-18 Task 18.3.18 FIX Sequence Gap Resolution, Session Reject & CoD Recovery; Phase-05 Task 5.3.24 idempotent submission; remediation #19)"),
	specRow("INSUFFICIENT_SCOPE", 403, "API key lacks required permission scope for endpoint (remediation #15) (Phase-05 Task 5.3.26 scope-claim verification; Phase-12 Task 12.3.11 delegated RBAC; remediation #19)"),
	specRow("IDEMPOTENCY_KEY_COLLISION", 409, "Duplicate client_order_id submitted with mismatched payload (remediation #15) (Phase-05 Task 5.3.24 HMAC Request Signing + Idempotent Order Submission; remediation #19)"),
	specRow("GATEWAY_TIMEOUT_MATCHING_ENGINE", 504, "Upstream Aeron IPC / matching engine response timeout (>500ms, remediation #15)"),
	specRow("WS_MAX_SUBSCRIPTIONS_EXCEEDED", 400, "WebSocket connection exceeded maximum 200 subscribed channels (remediation #15)"),
	specRow("ACCOUNT_LOCKED_AUTH_FAILURES", 423, "Account locked for 15 minutes due to 5 consecutive auth failures (remediation #15)"),
	specRow("INVALID_CREDENTIALS", 401, "Email/password pair failed authentication (Phase-12 Task 12.3.1 User Registration & Authentication; also current-password verification on change-password/2FA-disable paths; remediation #45)"),
	specRow("WEBAUTHN_VERIFICATION_FAILED", 401, "WebAuthn/FIDO2 signature verification or sign_count check failed (remediation #15)"),
	specRow("CROSS_SHARD_MARGIN_TIMEOUT", 504, "Two-phase commit margin reservation timeout (>10ms hard deadline, remediation #15)"),
	specRow("SANCTIONS_SERVICE_UNAVAILABLE", 503, "External sanctions screening provider unreachable; scoped degradation (remediation #15)"),
	specRow("REGULATORY_REPORT_RESUBMISSION", 409, "ARM/APA regulatory submission rejected by trade repository (remediation #15) (Phase-21 Task 21.3.23 ARM/APA Resubmission; remediation #19)"),
	specRow("CLS_SETTLEMENT_MISMATCH", 409, "Internal settlement batch conflicts with CLS match report (remediation #15)"),
	specRow("WAL_RECOVERY_HALT", 500, "Graduated WAL recovery ladder failed; manual operator recovery required (remediation #15)"),
	specRow("ACCOUNT_FROZEN", 403, "Order entry or withdrawal rejected while the account is compliance-frozen (Phase-14 Task 14.3.10; remediation #18)"),
	specRow("ACCOUNT_CLOSE_BLOCKED", 409, "Account close refused: open positions, open orders, pending settlement or pending funding transactions remain (Phase-14; remediation #18)"),
	specRow("BATCH_SIZE_EXCEEDED", 400, "Batch order payload exceeds the 10-order submit / 20-order cancel ceiling (Phase-05 Task 5.3.32; remediation #18)"),
	specRow("COOLING_OFF_ACTIVE", 409, "Margin or leveraged order entry rejected while a cooling-off / self-exclusion period is active (Phase-14 Task 14.3.11; remediation #18)"),
	specRow("CROSS_SHARD_LIMIT_EXCEEDED", 429, "Concurrent cross-shard operations exceed the 10-per-account cap (Phase-02 Task 2.3.14; remediation #18)"),
	specRow("CROSS_SHARD_MARGIN_UNAVAILABLE", 503, "Cross margin evaluation missed the 500µs coordinator RPC budget; order rejected on the pessimistic floor (Phase-02 Task 2.3.12; remediation #18)"),
	specRow("EMPLOYEE_DEALING_PRECLEARANCE_REQUIRED", 422, "`SENSITIVE_ROLES` employee account attempted order entry without pre-clearance (Phase-21 Task 21.3.24; remediation #18; remediation #35 — HTTP status aligned 403 → 422 per spec §14.10.1, the canonical citation)"),
	specRow("HISTORICAL_QUERY_TIMEOUT", 504, "Historical market-data query exceeded the 10-second ClickHouse timeout (Phase-23 Task 23.3.8; remediation #18)"),
	specRow("MARKET_ORDER_REJECTED_WIDE_SPREAD", 400, "Aggressive market order rejected because spread exceeds `max_spread_pips` (Phase-02 Task 2.3.15; remediation #18)"),
	specRow("PRICE_ORACLE_UNAVAILABLE", 503, "Fewer than 2 independent oracle feeds fresh (<5s); margin order submissions halted (Phase-19.5 Task 19.5.3.1; remediation #18)"),
	specRow("PROMOTION_NOT_APPROVED", 410, "Financial promotion is not approved or has passed its ≤12-month approval expiry (Phase-21 Task 21.3.26; remediation #18; remediation #35 — HTTP status aligned 409 → 410 per spec §14.10.3, the canonical citation)"),
	specRow("SERVICE_DEGRADED", 503, "Gateway circuit breaker open: upstream error rate >15% over 10s (Phase-05 Task 5.3.29; remediation #18)"),
	specRow("SLIPPAGE_EXCEEDED", 400, "Market order remainder unfilled after sweeping the book to the slippage protection price (Phase-02 Task 2.3.15; remediation #18)"),
	specRow("TREASURY_LIQUIDITY_BREACH", 503, "Stressed 5-business-day liquidity buffer breached; discretionary house outflows frozen and new LP capacity blocked (Phase-24 Task 24.3.17; remediation #18)"),
	specRow("RETENTION_POLICY_VIOLATION", 409, "Data type exceeds its retention period without archival; nightly retention enforcer drift (spec §19.12, Phase-09 Task 9.3.22; remediation #18)"),
	specRow("ORDER_REJECTED_NO_LIQUIDITY", 400, "MARKET / IOC / FOK rejected when the order's *opposite* book side is empty (side-aware check — a BUY is rejected only when `ask_count == 0`); fail-closed, no partial fills (spec §6.6, Phase-02 Task 2.3.13; remediation #18; remediation #35 — side-awareness pinned)"),
	specRow("SOR_TIMEOUT", 504, "External venue non-response timeout (500ms) after SOR auto-cancel and re-route failed; rejection returned to client (Phase-18 Task 18.3.14; remediation #18)"),
	specRow("ROUTING_REJECTED", 409, "SOR route rejected: the parent order already holds a non-terminal external shadow order — no concurrent local+external order (spec §24 #242, Phase-18 Task 18.3.14; internal/sor Router guard)"),
	specRow("L3_SNAPSHOT_TOO_LARGE", 413, "L3 order-level snapshot request exceeds the reconstruction ceiling (>100k orders on the book); use paginated streaming instead (Phase-17 Task 17.3.2; remediation #18)"),
	specRow("VALUE_DATE_ON_HOLIDAY", 422, "Order value date falls on a base/quote currency holiday; shifted date required (spec §7.4, Phase-15 Task 15.3.11; remediation #24)"),
	specRow("AMEND_IN_AUCTION_REJECTED", 409, "Amend/replace rejected while the instrument is in CALL, CANCEL_ONLY, SUSPENDED or HALTED; cancels remain available (spec §6.9, Phase-02 Task 2.3.20; remediation #24)"),
	specRow("REQUEST_WEIGHT_EXCEEDED", 429, "Per-route request-weight quota exceeded under the tabulated multi-interval counters (spec §8.8, Phase-05 Task 5.3.42; remediation #24)"),
	specRow("ENTITLEMENT_REQUIRED", 403, "Symbol-level market-data entitlement missing on WS/SBE/private stream (spec §10.7, Phase-06 Task 6.3.22; remediation #24)"),
	specRow("MARGIN_MODEL_UNVALIDATED", 503, "Margin-parameter change blocked without a passing independent validation run (spec §13.12, Phase-19 Task 19.3.21; remediation #24)"),
	specRow("SECRET_ROTATION_OVERDUE", 503, "Secret past its rotation SLA with no completed emergency rotation (spec §19.14, Phase-09 Task 9.3.29; remediation #24)"),
	specRow("IDEMPOTENCY_KEY_MISMATCH", 422, "Same `Idempotency-Key` resubmitted with a different payload; stored ack is not replayed (spec §8.8, Phase-05 Task 5.3.42; remediation #24 — registered late 2026-09-27, remediation #26 repair: the Task 5.3.42 text cited this code but §23 never carried the row)"),
	specRow("STP_NONE_NOT_PERMITTED", 409, "Account or instrument not eligible for STP mode NONE (PRO/ECP only); order rejected at pre-trade (Phase-02 Task 2.3.16; remediation #35 — registered: emitted by the task, row was missing)"),
	specRow("FIXING_CANCELLATION_RESTRICTED", 409, "Benchmark-fixing order cannot be cancelled once the fixing window has opened (Phase-16 Task 16.3.9; remediation #35 — registered)"),
	specRow("ENGINE_OVERLOAD", 503, "Inbound queue saturation: order dropped after 95% ingress watermark with `ENGINE_OVERLOAD`; 80% watermark sheds load per spec §2.7.3 (Phase-02 Task 2.3.7 Backpressure & Engine Overload; remediation #35 — registered)"),
	specRow("IOC_PARTIALLY_FILLED_REMAINDER_CANCELED", 409, "IOC filled the available liquidity; remainder cancelled immediately per spec §6.8 (Phase-02 Task 2.3.2; remediation #35 — registered)"),
	specRow("IP_BANNED", 418, "Progressive IP ban escalation: request from a banned IP after repeated abuse (4th strike = ban) (Phase-05 Task 5.3.34; remediation #35 — registered)"),
	specRow("COUNTDOWN_INVALID_DURATION", 400, "Dead-man countdown duration outside the accepted 1s–300s range (Phase-05 Task 5.3.33; remediation #35 — registered)"),
	specRow("COUNTDOWN_ALREADY_ACTIVE", 409, "Dead-man countdown already active for this account; duplicate start rejected (Phase-05 Task 5.3.33; remediation #35 — registered)"),
	specRow("CLOSE_ALL_PARTIAL_FAILURE", 409, "Close-all-positions convenience call partially failed; per-position results returned with this code on the failed entries (Phase-05 Task 5.3.36; remediation #35 — registered)"),
	specRow("CORE_TIMEOUT", 504, "WS-surface sibling of `GATEWAY_TIMEOUT_MATCHING_ENGINE`: 500ms Aeron no-response on the interactive WS path, NACK to the `order.place` frame (Phase-06 Task 6.3.10; remediation #35 — registered; surface-split documented, not a duplicate)"),
	specRow("SETTLEMENT_ACCOUNT_CLOSED", 409, "Rail transfer rejected: beneficiary settlement account is closed at the bank (Phase-11 Task 11.3.11 Rail Return-Code Mapping; remediation #35 — registered)"),
	specRow("SETTLEMENT_RAIL_REJECTED", 409, "Rail rejected the instruction (e.g., RR04 regulatory-rule rejection); distinct from provider outage `SANCTIONS_SERVICE_UNAVAILABLE` (Phase-11 Task 11.3.11; remediation #35 — registered)"),
	specRow("CROSSED_BOOK_DETECTED", 409, "Book crossed beyond the uncross threshold; instrument quarantined to HALTED and the uncross-override path opened (Phase-15 Task 15.3.10; remediation #35 — registered)"),
	specRow("PREMIUM_INSUFFICIENT", 400, "Option premium debit failed: buyer lacks `premium_currency` balance at T+2 settlement; premium settlement queued into the margin-call workflow (Phase-22 Task 22.3.10; remediation #35 — registered)"),
	specRow("GSLO_EXPOSURE_EXCEEDED", 400, "GSLO submission rejected: aggregate guaranteed-stop gap liability on the instrument plus the new order exceeds the per-instrument cap (`instruments.param_overrides->'gslo'.max_exposure_quote`, Risk Manager configurable) (Phase-16 Task 16.3.16; cited by §24 Order-Types matrix since remediation #11 — registered 2026-09-29)"),
	specRow("TRADE_THROUGH_DETECTED", 409, "Aggressive order would match at a price worse than the protected quote; rejected per §6.6b trade-through prevention (Phase-02 Task 2.3.22; feature completeness audit #36 — registered)"),
	specRow("RAIL_CUTOFF_EXCEEDED", 422, "Outbound rail settlement or withdrawal submitted past daily banking rail cut-off schedule without auto-roll (Phase-24 Task 24.3.20; remediation #37 — registered)"),
	specRow("BILATERAL_CREDIT_EXHAUSTED", 409, "Matching loop found no executable contra liquidity within bilateral credit screening limits (Phase-02 Task 2.3.24; remediation #37 — registered)"),
	specRow("DISCRETIONARY_OFFSET_INVALID", 400, "Discretionary price offset negative, exceeds maximum spread band, or attached to non-LIMIT/TIF order (Phase-02 Task 2.3.26; remediation #37 — registered)"),
	specRow("ISOLATED_MARGIN_DEFICIT", 409, "Position-level isolated margin deficit cannot be satisfied and auto-replenish is disabled (Phase-19 Task 19.3.27; remediation #37 — registered)"),
	specRow("ACCOUNT_NOT_FOUND", 404, "Account identifier does not resolve (Phase-05 Task 5.3.5 Internal Transfers; emitted by gateway funding paths — registered 2026-10-05, remediation #44)"),
	specRow("API_KEY_NOT_FOUND", 401, "API key identifier does not resolve during mass-cancel/session validation (Phase-05 Task 5.3.24 Scoped Mass Cancel; remediation #44)"),
	specRow("AUTH_INTERNAL", 500, "Authentication subsystem internal failure (Phase-05 Task 5.3.24; remediation #44)"),
	specRow("BALANCE_EVENT_DISPATCH_FAILED", 500, "Balance-change NATS dispatch failed after commit (Phase-03 Task 3.3.6 GL posting seam; remediation #44)"),
	specRow("DUAL_CONTROL_REQUIRED", 400, "Operation requires a second-authorizer approval (Phase-05 Task 5.3.12 FROZEN legal-hold; remediation #44)"),
	specRow("DUAL_CONTROL_VIOLATION", 400, "Dual-control approval attempt by the initiating principal (Phase-05 Task 5.3.12; remediation #44)"),
	specRow("ENDPOINT_GONE", 410, "Endpoint removed per deprecation schedule (Phase-05 Task 5.3.20 API Deprecation Policy; remediation #44)"),
	specRow("FEE_INVALID_INPUT", 400, "Fee-schedule input failed validation (Phase-03 Task 3.3.4 Fee Engine; remediation #44)"),
	specRow("FEE_TIER_NOT_FOUND", 404, "Referenced fee tier does not exist (Phase-05 Task 5.3.15 Fee Schedule Surface; remediation #44)"),
	specRow("INSTRUMENT_RESTRICTED", 409, "Instrument in RESTRICTED lifecycle grace state; new orders blocked, cancels allowed (Phase-05 Task 5.3.3 order ingress; remediation #44)"),
	specRow("INTERNAL_ERROR", 500, "Unmapped internal failure; RFC-7807 envelope generic surface (Phase-05 Task 5.3.41 Error Envelope; remediation #44)"),
	specRow("LEDGER_INVALID_JOURNAL", 500, "Journal entry failed double-entry validation at posting (Phase-03 Task 3.3.6; remediation #44)"),
	specRow("LEDGER_LOCK_UNAVAILABLE", 503, "Account ledger mutex unavailable within deadline (Phase-03 Task 3.3.6; remediation #44)"),
	specRow("LEDGER_UNKNOWN_ACCOUNT", 500, "Journal references a GL account absent from the chart (Phase-03 Task 3.3.6; remediation #44)"),
	specRow("NOT_IMPLEMENTED", 501, "Route registered but handler not yet implemented (Phase-05 Task 5.3.7 Route Registration; remediation #44)"),
	specRow("OPS_ALERT_DISPATCH_FAILED", 500, "Operational alert dispatch to the alert taxonomy failed (Phase-03 Task 3.3.6; remediation #44)"),
	specRow("RISK_LIMITS_INTERNAL", 500, "Risk-limit store internal failure (Phase-05 Task 5.3.4 Risk-Limit Administration; remediation #44)"),
	specRow("TWO_FACTOR_REQUIRED", 403, "TOTP step-up required for the privileged operation (Phase-05 Task 5.3.36 Close-All Positions; remediation #44)"),
	specRow("UNAUTHORIZED_ROLE", 403, "Caller role lacks the required authorization for the operation (Phase-05 Task 5.3.12; remediation #44)"),
	specRow("WEBHOOK_DELIVERY_FAILED", 500, "Webhook delivery permanently failed after retry budget (Phase-05 Task 5.3.17 Webhooks; remediation #44)"),
	specRow("WITHDRAWAL_CONFIRM_EXPIRED", 409, "Withdrawal confirmation window elapsed; request must be re-initiated (Phase-05 Task 5.3.6 Withdrawal Flow; remediation #44)"),
	specRow("INVALID_DEPTH_LIMIT", 400, "Requested book depth levels/cadence outside the supported {5,10,20}×{100,250,1000}ms table (Phase-06 Task 6.3.15 Configurable Depth; §24 #265 — cited by §27.1 L2 Book row but never registered; remediation #44 follow-on)"),
	specRow("INVALID_INTERVAL", 400, "Kline/aggregate interval outside the canonical 13-timeframe set (Phase-06 Task 6.3.14; Phase-20 Task 20.3.1 REST surface — cited by §27.1 Klines row but never registered; remediation #44 follow-on)"),
	specRow("TICKET_NOT_FOUND", 404, "Support ticket identifier does not resolve, or resolves to a ticket outside the caller's account scope (Phase-07 Task 7.3.7 Support Tickets & Complaints; cited by the §27.1 Complaints & Dispute Resolution matrix row but never tabled — registered 2026-09-28, remediation #44 follow-on)"),
	specRow("DISCLOSURE_NOT_FOUND", 404, "Vulnerability disclosure identifier does not resolve (Phase-13.5 Task 13.5.3.8 Vulnerability Disclosure Program; §19.11.2 intake register — VDP_SLA_BREACH stays an internal ops alert per §23 internal-only list)"),
	specRow("BANKING_RAIL_UNAVAILABLE", 503, "No banking rail can carry the instruction — every candidate failed currency, amount-cap, availability (scoped kill-switch) or account-eligibility screening; fail-closed (Phase-11 Task 11.3.1 Banking Rails Integration; §24 Banking Rails matrix row)"),
	specRow("BENEFICIARY_HOLD_ACTIVE", 422, "Withdrawal target is a VERIFIED beneficiary still inside its 24-hour new-account hold — retry after `unlocked_at` (Phase-11 Task 11.3.7 Beneficiary Bank-Account Registry; §24 #391 hold, verified_at + 24h — distinct from `BANK_ACCOUNT_NOT_VERIFIED` which covers unregistered/unverified destinations)"),
	specRow("WITHDRAWAL_WHITELIST_ONLY", 422, "Whitelist-only mode is enabled for the account and the destination resolves to no VERIFIED bank_accounts beneficiary (Phase-11 Task 11.3.10 Withdrawal Whitelist Mode; §5.23/§24 #391 — registered for the flows cluster)"),
	specRow("WITHDRAWAL_WHITELIST_LOCKED", 423, "Account-scoped withdrawal egress lock — whitelist-only mode was disabled less than 24 hours ago (Phase-11 Task 11.3.10 deactivation safety lock; account-scoped, never platform-wide — remediation #35 semantics)"),
	specRow("WHITELIST_CHANGE_LOCKED", 429, "Whitelist-mode re-enable is rate-limited until timelock_until lapses (Phase-11 Task 11.3.10; the 24h deactivation latch — documented unlock path is to wait out the latch)"),
	specRow("NOSTRO_INSUFFICIENT_FUNDS", 500, "Aggregate ACTIVE nostro balance cannot cover the withdrawal — the withdrawal is QUEUED for dispatch, never rejected (Phase-11 Task 11.3.6 Nostro-Aware Withdrawals; §27.1 Nostro/Vostro matrix code — emitted on the durable funding_ops_alerts trail)"),
	specRow("PAMM_MIN_INVESTMENT_NOT_MET", 400, "PAMM investment below the pool's min_investment floor (Phase-14 Task 14.3.8 PAMM/MAM; §27.1 PAMM/MAM Investment Modules matrix code — registered with the Task 14.3.8 landing)"),
	specRow("PAMM_INVESTOR_LOCKED", 403, "PAMM invest/redeem rejected: the investor account fails the FROZEN/SUSPENDED mutability gate (Phase-14 Task 14.3.8; §27.1 PAMM matrix code)"),
	specRow("PAMM_ALLOCATION_MISMATCH", 500, "Pro-rata fill allocation failed conservation — residuals could not be distributed or the persisted child set diverges from the master fill; fail-closed, never silently misallocates (Phase-14 Task 14.3.8; §27.1 PAMM matrix code, L1/L2)"),
	// Phase-16 Tasks 16.3.19/16.3.21 — grid bots + recurring/rebalancing
	// strategies (registered with the task landing; §24 #275/#296).
	specRow("MAX_GRID_BOTS_EXCEEDED", 429, "Concurrent grid-bot limit reached — at most five RUNNING bots per account (Phase-16 Task 16.3.19; §24 Order-Types matrix code, ruling R13)"),
	specRow("GRID_PARAMETERS_INVALID", 400, "Grid bot configuration invalid — bounds ordering, grid_count outside 5–200, unknown mode, tick-misaligned or colliding levels, leverage beyond the instrument tier, or per-level quantity below min_order_qty/min_notional (Phase-16 Task 16.3.19)"),
	specRow("GRID_MARGIN_INSUFFICIENT", 400, "Grid bot committed capital exceeds available balance for the investment currency at placement (Phase-16 Task 16.3.19; §24 Order-Types matrix code)"),
	specRow("STRATEGY_NOT_FOUND", 404, "Strategy or strategy-template identifier does not resolve within the caller's scope (Phase-16 Task 16.3.21)"),
	specRow("STRATEGY_CONFIG_INVALID", 400, "Strategy or template configuration failed validation — bad kind/schedule/currency pair, non-positive amount, targets not summing to 1, drift band out of range, or config keys outside the allowlist (Phase-16 Task 16.3.21)"),
	specRow("STRATEGY_TEMPLATE_NOT_APPROVED", 409, "Strategy template is not APPROVED — marketplace instantiation requires the approval gate (Phase-16 Task 16.3.21)"),
	// Phase-18 Task 18.3.13 — FIX 35=J/35=AK allocation workflow (§27.1
	// Allocation + Post-Trade Allocation matrix codes; §24 #200/#237).
	specRow("ALLOCATION_SUM_MISMATCH", 400, "FIX AllocationInstruction (35=J) rejected: the sum of NoAllocs(78) AllocQty(80) legs does not exactly equal the referenced executed quantity — over/under-allocation is never booked; wire surface is 35=P with AllocRejCode(88)=4 per the §27.1 Allocation matrix (Phase-18 Task 18.3.13)"),
	specRow("ALLOCATION_INVALID", 400, "FIX AllocationInstruction rejected for a structural fault — unresolvable ExecID/OrderID reference, AllocAccount(79) outside the master-account hierarchy, unknown AllocType(626) method, non-positive leg quantity/weight, or amend against a non-ACCEPTED/settlement-locked instruction (Phase-18 Task 18.3.13)"),
	// Phase-19.5 — Price Oracle & Mark Price (spec §27.1 Mark Price /
	// Index Oracle / Staleness Gates / Yield Curves matrix codes;
	// §24 #45/#120/#134/#196/#321).
	specRow("ORACLE_FEED_STALE", 503, "Oracle feed's newest quote exceeds the 5-second staleness gate; source excluded from the mark cohort (Phase-19.5 Task 19.5.3.3 Staleness Gates; §27.1 Index Oracle/Staleness Gates matrix rows — registered with the Task 19.5.3 landing)"),
	specRow("ORACLE_DIVERGENCE_EXCEEDED", 503, "Oracle source diverges from the cohort median by >25bps; outlier discarded, mark computed from remaining coherent sources (Phase-19.5 Task 19.5.3.7; §27.1 Index Oracle matrix row)"),
	specRow("MARK_PRICE_STALE", 503, "Published mark price exceeded the consumer's staleness window; margin/liquidation evaluation deferred to the stale-price ladder (Phase-19.5 Task 19.5.3.3; §27.1 Mark Price matrix row)"),
	specRow("MARK_PRICE_OUT_OF_BOUNDS", 400, "Mark/reference price outside the instrument's sanity band; rejected before valuation (Phase-19.5 Task 19.5.3.2; §27.1 Mark Price matrix row)"),
	specRow("STALE_FORWARD_POINTS", 503, "Tom-Next / forward swap points past their staleness window; forward/rollover pricing halted (Phase-19.5 Task 19.5.3.5; §27.1 Yield Curves matrix row)"),
	specRow("INSUFFICIENT_COHORT", 422, "Marketing-consent cohort aggregate below the 100-record anonymity floor; the cohort row is suppressed rather than emitted (spec §16.11, Phase-20 Task 20.3.16; §24 #381)"),
}

// localCodes are emitted by the gateway but carry no spec §23 row yet.
// Spec=false marks them for the docs-drift check — each needs a §23 row.
// As of remediation #44 the slice was empty; Phase-19 Task 19.3.12 adds
// three codes that ARE spec-cited but were never transcribed into §23:
// INSUFFICIENT_MARGIN is named verbatim in §13.9 item 2(c) (HTTP 409) and
// the §27.1 Internal Position Transfers matrix row carries
// POSITION_TRANSFER_FAILED / TRANSFER_INSUFFICIENT (400, L2). They land
// here (append-only) rather than as specRows because the §23 transcription
// count is pinned by tests; the spec-side fix is a §23 row add.
var localCodes = []CodeDef{
	localRow("INSUFFICIENT_MARGIN", 409, "Phase-19 Task 19.3.12",
		"Position transfer aborted atomically: destination account lacks free balance for required initial margin (spec §13.9 item 2(c), remediation #38)"),
	localRow("POSITION_TRANSFER_FAILED", 400, "Phase-19 Task 19.3.12",
		"Internal position transfer rejected: entity/hierarchy mismatch, non-ACTIVE account, structural validation failure, or audit-row conflict (spec §13.9, §27.1 matrix row)"),
	localRow("TRANSFER_INSUFFICIENT", 400, "Phase-19 Task 19.3.12",
		"Internal position transfer rejected: source account has no matching open position or the open quantity is below the requested transfer quantity (spec §13.9, §27.1 matrix row)"),
	// Phase-21 Tasks 21.3.2/21.3.3/21.3.6 — §27.1 Travel Rule / SAR-CTR /
	// FinCEN MSB matrix codes (named verbatim there; the §23 fix is a
	// spec-side transcription row). CTR_TRIGGERED is an Audit Event, not
	// an HTTP code — emitted to the audit trail, never to the wire.
	localRow("TRAVEL_RULE_MISSING_INFO", 400, "Phase-21 Task 21.3.2",
		"FATF R.16 travel-rule data incomplete for a >= $1,000 transfer — required originator/beneficiary fields absent; the wire is held until supplied (spec §14.3, §27.1 Travel Rule matrix)"),
	localRow("TRAVEL_RULE_REJECTED", 403, "Phase-21 Task 21.3.2",
		"Transfer rejected under the FATF travel rule — the record was dispositioned REJECTED by compliance (spec §14.3, §27.1 Travel Rule matrix)"),
	localRow("SAR_DUAL_CONTROL_REQUIRED", 400, "Phase-21 Task 21.3.3",
		"SAR filing approval requires a distinct second Compliance Officer — the approver may not be the drafting or reviewing officer (spec §24 #108, §27.1 SAR/CTR matrix)"),
	localRow("MSB_COMPLIANCE_BREACH", 500, "Phase-21 Task 21.3.6",
		"FinCEN MSB program artifact missing or expired — registration (Form 107), designated officer, policy version or annual review is not current (spec §14.1, §27.1 FinCEN MSB matrix)"),
	// Phase-21 Task 21.3.13 — §27.1 Basel III matrix codes (named
	// verbatim there with 500): they ride the basel-report payload's
	// `code` field and the breach alerts; the §23 fix is a spec-side
	// transcription row.
	localRow("CAPITAL_ADEQUACY_BREACH", 500, "Phase-21 Task 21.3.13",
		"Basel III capital adequacy ratio below the 8% floor (or report inputs incomplete — fail-closed flag) for the stored snapshot period (spec §14.1, §27.1 Basel III matrix)"),
	localRow("LEVERAGE_RATIO_BREACH", 500, "Phase-21 Task 21.3.13",
		"Basel III leverage ratio below the 3% floor for the stored snapshot period (spec §14.1, §27.1 Basel III matrix)"),
	// Phase-21 wave-2 — §23 transcription pending; §27.1/spec §14.x
	// citations inline.
	localRow("CONSENT_NOT_GRANTED", 403, "Phase-21 Task 21.3.7",
		"Outbound processing requires an affirmative account consent that is absent or withdrawn — GDPR opt-in semantics for marketing/analytics/data-sharing (spec §14.13)"),
	localRow("CROSS_BORDER_JUSTIFICATION_REQUIRED", 403, "Phase-21 Task 21.3.18",
		"Administrative access crosses the resident-data jurisdiction boundary without a recorded justification (spec §14.13 cross-border control)"),
	localRow("RESIDENCY_VIOLATION", 403, "Phase-21 Task 21.3.18",
		"Data placement or cross-border transfer conflicts with the jurisdiction's residency policy — unsupported jurisdiction, wrong region/bucket/KMS key, or no legal transfer instrument (spec §14.13)"),
	localRow("COMMS_INTEGRITY_FAILURE", 500, "Phase-21 Task 21.3.20",
		"Communications-recording integrity check failed — stored object hash or per-day chain mismatch, tamper-suspect (MiFID II Art. 16(7); fail-closed)"),
	localRow("COMMS_RETENTION_ACTIVE", 409, "Phase-21 Task 21.3.20",
		"Recording deletion refused — the five-year retention_until floor has not elapsed (MiFID II Art. 16(7))"),
	localRow("TAX_REPORT_INVALID_TRANSITION", 409, "Phase-21 Task 21.3.22",
		"CRS/FATCA report-run status transition not permitted by the DRAFT→UNDER_REVIEW→APPROVED→SUBMITTED lifecycle (spec §14.11)"),
	localRow("TAX_REPORT_DATA_INCOMPLETE", 422, "Phase-21 Task 21.3.22",
		"Reportable account is missing required CRS/FATCA data (residency, TIN, balances) — the run fails closed rather than emitting a partial file (spec §14.11)"),
	// Phase-21 Task 21.3.15 — §27.1 Regulated Venue License Governance
	// matrix codes (named verbatim there; the §23 fix is a spec-side
	// transcription row). JURISDICTION_UNLICENSED is emitted by the
	// member-trading gate (order admission, L2 403) when the
	// jurisdiction-scoped LICENSE/REGULATOR_AUTH prerequisite is absent
	// or expired; ANNUAL_ATTESTATION_OVERDUE rides the P1 ops-alert
	// payload `code` field (venue.SweepOverdue), not HTTP responses.
	localRow("JURISDICTION_UNLICENSED", 403, "Phase-21 Task 21.3.15",
		"Venue member trading rejected — the jurisdiction's venue authorization/license prerequisite is absent or expired (spec §14.1b, §27.1 Regulated Venue matrix)"),
	localRow("ANNUAL_ATTESTATION_OVERDUE", 500, "Phase-21 Task 21.3.15",
		"Member annual review or required attestation past due — P1 ops alert until the review lands; member trading admission blocks concurrently (spec §14.1b, §27.1 Regulated Venue matrix)"),
	localRow("VENUE_RULEBOOK_NOT_APPROVED", 409, "Phase-21 Task 21.3.15",
		"Rulebook/product-terms activation refused — venue approval, required regulator approval, or participant notice outstanding (spec §14.1b no-activation-before-approvals)"),
	// Phase-22 Task 22.3.14 — spec §15.6 derivatives pricing fallbacks.
	// The IV-surface feed fallback hierarchy (§15.7 item 2) exhausts to a
	// 503 like YIELD_CURVE_UNAVAILABLE; emitted by
	// internal/options (ivsurface.go query paths). The §23 fix is a
	// spec-side transcription row.
	localRow("VOLATILITY_SURFACE_UNAVAILABLE", 503, "Phase-22 Task 22.3.14",
		"Implied-volatility surface unavailable — empty/unbuilt surface queried or no usable surface snapshot remains after the feed-fallback hierarchy (spec §15.6, §15.7 item 2)"),
	// Phase-22 Task 22.3.15 — emitted by internal/options
	// (types.go invalidInput contract: american.go/ivsurface.go/
	// determinism.go/barrier.go input guards).
	localRow("OPTION_PRICING_INPUT_INVALID", 400, "Phase-22 Task 22.3.15",
		"Option pricing input failed validation — non-positive or non-finite spot/strike/tenor/vol, out-of-bounds target price, malformed smile quote, or unknown enum (spec §15.6 fail-closed, §2.7)"),
	// Phase-22 Tasks 22.3.1/22.3.3 — derivatives booking/settlement codes
	// emitted by internal/derivatives. BENCHMARK_UNAVAILABLE is named
	// verbatim in the §27.1 MTF/Fair-Value matrix row (503, L1); emitted
	// when an NDF has no published fixing at settle time (spec §15.6/§15.7
	// hierarchy exhausted). The §23 fix is a spec-side transcription row.
	localRow("BENCHMARK_UNAVAILABLE", 503, "Phase-22 Task 22.3.3",
		"NDF fixing benchmark unavailable — no published rate for the contract's fixing date; cash settlement halted (spec §15.7, §27.1 MTF/Fair-Value matrix)"),
	localRow("DERIVATIVE_STATE_CONFLICT", 409, "Phase-22 Task 22.3.1",
		"Derivative contract lost the expected lifecycle state mid-operation (e.g. settle raced with a status flip), or its persisted settlement legs diverge from the contract — fail-closed, nothing booked (spec §15.1, §2.7)"),
	// Phase-22 Task 22.3.8 — Roll Management codes emitted by
	// internal/derivatives (roll.go). The §23 fix is a spec-side
	// transcription row.
	localRow("ROLL_NOT_PERMITTED", 400, "Phase-22 Task 22.3.8",
		"Position is not rollable — wrong account, flat quantity, or a non-derivative instrument class (spec §15 roll mechanics; FORWARD|SWAP|NDF only)"),
	localRow("ROLL_TARGET_INVALID", 400, "Phase-22 Task 22.3.8",
		"Roll target instrument missing, inactive, identical to the source, or mismatched in class/base/quote — the roll is rejected rather than re-priced (fail-closed, spec §2.7)"),
	localRow("ROLL_SPREAD_TOLERANCE_EXCEEDED", 422, "Phase-22 Task 22.3.8",
		"Roll price (open−close spread) exceeded the caller's max_roll_price_bps tolerance — atomic rejection, nothing booked (Phase-22 Task 22.3.15 roll spread-tolerance hook)"),
	localRow("ROLL_CONFIG_INVALID", 400, "Phase-22 Task 22.3.8",
		"Automatic-roll configuration rejected — missing account, lead_days outside 0..10, or a target_instrument_id that is not a rollable derivative"),
	// Phase-22 Task 22.3.10 — Option Lifecycle codes emitted by
	// internal/derivatives (lifecycle.go); spec §15.4, §24 #158/#393.
	localRow("OPTION_NOT_EXERCISABLE", 409, "Phase-22 Task 22.3.10",
		"Option lifecycle state forbids the instruction — already EXERCISED/ASSIGNED/EXPIRED, wrong side/account, a EUROPEAN before its expiry day, or a do-not-exercise flag racing auto-exercise (spec §15.4)"),
	localRow("OPTION_CONTRACT_INVALID", 400, "Phase-22 Task 22.3.10",
		"Option contract terms are malformed or unsettleable — non-positive strike/quantity, unknown option_type/exercise_style/settlement mode, missing premium_currency, past expiry, or no ACTIVE spot instrument for PHYSICAL delivery (spec §15.4; fail-closed §2.7)"),
	localRow("MARGIN_CALL_QUEUE_FAILED", 500, "Phase-22 Task 22.3.10",
		"Premium debit failed (PREMIUM_INSUFFICIENT, §24 #393) and the Phase-19 Task 19.3.3 margin-call queue write also failed — the settlement row stays FAILED and ops is paged; nothing partial was booked"),
	localRow("OPTION_EXERCISE_AUCTION_BLOCKED", 409, "Phase-22 Task 22.3.10",
		"Writer assignment blocked while a §13.4 liquidation auction on the option instrument or its underlying is live — delivering mid-auction would worsen a liquidating account's position (§24 #247; retry once the auction resolves)"),
	localRow("EXERCISE_AUCTION_EVAL_FAILED", 503, "Phase-22 Task 22.3.10",
		"Liquidation-auction guard unreadable during exercise — auction state could not be verified, so assignment fails closed (§2.7, §24 #247)"),
	// Phase-22 Task 22.3.9 — derivative order-parameter admission codes
	// emitted by internal/orders (types.go validateDerivativeParams +
	// service.go linkage/persistence seams); migration 039, spec §5.4.
	localRow("DERIVATIVE_PARAMS_INVALID", 400, "Phase-22 Task 22.3.9",
		"Derivative order parameters missing, malformed or mismatched to the instrument class — required per-class fields absent, unknown enum, non-positive strike/barrier/premium, inconsistent expiry/value-date ordering, or unresolvable instrument settlement_mode linkage (spec §5.4, §15.7; fail-closed §2.7)"),
	// Phase-22 Task 22.3.12 — multi-leg implied-matching gate code
	// emitted by internal/derivatives (implied_gate.go); spec §6.3/§24
	// implied liquidity. The §23 fix is a spec-side transcription row.
	localRow("IMPLIED_MATCHING_UNAVAILABLE", 503, "Phase-22 Task 22.3.12",
		"Multi-leg implied matching unavailable — the implied_matching feature flag is disabled/unresolvable for this account or tier, so implied-liquidity admission fails closed (spec §24; §2.7 fail-closed)"),
	// Phase-22 Task 22.3.7 — variation-margin sweep internals emitted by
	// internal/risk/variation_margin.go (subject scan, claim, GL posting).
	localRow("VARIATION_MARGIN_INTERNAL", 500, "Phase-22 Task 22.3.7",
		"Variation-margin sweep internal failure — subject scan, settlement claim, or GL posting error mid-sweep; the subject retries on the next pass (spec §15.7, §5.25)"),
	// Phase-22 Task 22.3.14 — fail-closed exercise margin-eval /
	// liquidation-adapter construction failures.
	localRow("EXERCISE_MARGIN_EVAL_UNAVAILABLE", 503, "Phase-22 Task 22.3.14",
		"Option exercise margin evaluation path unavailable — admission fails closed; on the auto-exercise path the liquidation seam is dispatched rather than leaving delivery unverified (spec §15.6, §24 #324)"),
	// Phase-22 Task 22.3.11 — UMR/IM assessment or gate degradation
	// (sensitivity feed, assessment store, posted-collateral read).
	localRow("UMR_IM_EVAL_FAILED", 503, "Phase-22 Task 22.3.11",
		"Uncleared-margin IM assessment/gating failed — sensitivity feed, assessment store, scope or collateral read degraded; admission and assessment fail closed (spec §15.5, §24 #146)"),
	// Phase-22 Task 22.3.13 — spread-offset param lifecycle violations
	// and internal persistence degradation on the offsets path.
	localRow("SPREAD_OFFSET_PARAM_INVALID", 422, "Phase-22 Task 22.3.13",
		"Option spread offset parameter rejected — bps out of range, unknown spread type, non-pending row, or maker-checker violation (approver must differ from proposer) (spec §15.7)"),
	localRow("SPREAD_OFFSET_INTERNAL", 500, "Phase-22 Task 22.3.13",
		"Option spread offset internal failure — margin-mode, leg, params or persistence read/write degraded; detection fails closed (spec §15.7, §13.11)"),
	// Phase-19 Task 19.3.3 — liquidation engine failure code emitted by
	// internal/risk (liquidation.go, auction.go, variation_margin.go
	// exercise-shortfall adapter). Registered here because the §23
	// canonical table has no row for it; spec §13.4/§13.11.
	localRow("LIQUIDATION_FAILED", 500, "Phase-19 Task 19.3.3",
		"Liquidation dispatch or processing failed — queue enqueue, level re-read, mass cancel or position close degraded; ops is paged and the account remains flagged (spec §13.4, §13.11; §2.7 fail-closed)"),
	// Phase-23 Task 23.3.2 — data export (CSV/JSON/Parquet + async job)
	// admission codes emitted by internal/api/handlers_export.go over
	// internal/marketdata. The §23 fix is a spec-side transcription row.
	localRow("EXPORT_LIMIT_EXCEEDED", 400, "Phase-23 Task 23.3.2",
		"Export request exceeds the 1,000,000-row per-export cap — narrow the time range or request a smaller bound (spec §10, §16.6)"),
	localRow("EXPORT_JOB_NOT_FOUND", 404, "Phase-23 Task 23.3.2",
		"Export job does not resolve, resolves outside the caller's account scope, or its 24-hour download link has expired — expiry is a miss, never a stale ref (spec §10, §16.6; §2.7 fail-closed)"),
	localRow("EXPORT_FORMAT_INVALID", 400, "Phase-23 Task 23.3.2",
		"Export format is not one of csv|json|parquet — the value is rejected, never defaulted (spec §10, §16.6)"),
	// Phase-24 Tasks 24.3.1–24.3.4 — nostro/vostro backoffice codes
	// emitted by internal/backoffice. NOSTRO_OVERDRAWN,
	// NOSTRO_RECON_MISMATCH and SETTLEMENT_CONFIRMATION_OVERDUE are
	// durable ops-alert codes riding the funding_ops_alerts trail (and
	// the ops.alerts page), never HTTP rejections — the 500 follows the
	// NOSTRO_INSUFFICIENT_FUNDS convention. The SETTLEMENT_* rows
	// register the Phase-03 Task 3.3.3 scaffold codes the Phase-24
	// confirmation path relays to the wire. The §23 fix is a spec-side
	// transcription row.
	localRow("NOSTRO_ACCOUNT_EXISTS", 409, "Phase-24 Task 24.3.1",
		"Correspondent nostro/vostro registry row already exists for (currency, bank_code, account_number) — dedup enforced, never double-counted (spec §17.1)"),
	localRow("NOSTRO_ACCOUNT_NOT_FOUND", 404, "Phase-24 Task 24.3.1",
		"Nostro/vostro account identifier does not resolve (spec §17.1)"),
	localRow("NOSTRO_OVERDRAWN", 500, "Phase-24 Task 24.3.1",
		"Posting a settlement DEBIT pushed a nostro_accounts balance below zero — the payment already happened at the correspondent; durable P1 alert, never a silent mask (spec §17.1, §24 #3)"),
	localRow("NOSTRO_RECON_MISMATCH", 500, "Phase-24 Task 24.3.2",
		"Daily nostro reconciliation found our records diverging from the bank statement — durable P1 alert feeding the break investigation workflow; §24 #21 discrepancy threshold (> $1,000 or > 0.01%) flagged on the run (spec §17.1)"),
	localRow("SETTLEMENT_CONFIRMATION_OVERDUE", 500, "Phase-24 Task 24.3.3",
		"Dispatched settlement leg unconfirmed past the 2-business-day window — durable P2 alert, dedup-keyed per instruction (spec §17.1, §24 #12)"),
	localRow("SETTLEMENT_NOT_FOUND", 404, "Phase-24 Task 24.3.3",
		"Settlement instruction id/reference does not resolve — emitted when a correspondent MT900/910 confirmation arrives for an unknown dispatch reference (Phase-03 Task 3.3.3 scaffold code, registered with the Phase-24 landing)"),
	localRow("SETTLEMENT_STATE_CONFLICT", 409, "Phase-24 Task 24.3.3",
		"Confirmation rejected — the settlement leg is no longer PENDING (FAILED/RECONCILED); replayed SETTLED legs stay idempotent (Phase-03 Task 3.3.3 scaffold code)"),
	localRow("SETTLEMENT_NOSTRO_MISSING", 503, "Phase-24 Task 24.3.3",
		"No ACTIVE nostro account resolves for the settlement currency — legs for it cannot settle (Phase-03 Task 3.3.3 scaffold code; spec §17.1)"),
	localRow("SETTLEMENT_INVALID_MESSAGE", 400, "Phase-24 Task 24.3.3",
		"Settlement payment payload cannot be rendered — e.g. the nostro row lacks a valid BIC (Phase-03 Task 3.3.3 scaffold code)"),
	localRow("SETTLEMENT_INVALID_FILL", 400, "Phase-24 Task 24.3.3",
		"Settlement fill is missing ids, carries non-positive price/quantity or an unset trade date (Phase-03 Task 3.3.3 scaffold code)"),
	// Phase-24 Task 24.3.8 — CLS PvP lifecycle codes emitted by
	// internal/settlement/cls_pvp.go. CLS_SETTLEMENT_MISMATCH carries a
	// §23 spec row already; these lifecycle/routing codes have no spec
	// row (§23 fix is a spec-side transcription row).
	localRow("CLS_MEMBER_UNAVAILABLE", 503, "Phase-24 Task 24.3.8",
		"CLS settlement-member ISO 20022 adapter not wired or degraded — the instruction stays pre-dispatch and is never claimed sent (spec §17.6, §2.7 fail-closed)"),
	localRow("CLS_WINDOW_CLOSED", 409, "Phase-24 Task 24.3.8",
		"Versioned CLS cut-off window (initial pay-in / rescind deadline) for the instruction's value date has closed — the operation is refused, never silently slipped (spec §17.6)"),
	localRow("CLS_MATCH_FAILED", 409, "Phase-24 Task 24.3.8",
		"CLS member match report conflicts with the persisted paired instruction or the member reported the pair unmatched — routed to the exception queue before cut-off (spec §17.6 step 3)"),
	localRow("CLS_INSTRUCTION_STATE_CONFLICT", 409, "Phase-24 Task 24.3.8",
		"Requested transition violates the CLS instruction lifecycle (RECEIVED→VALIDATED→MATCHED/UNMATCHED→ELIGIBLE/INELIGIBLE→PAY_IN→SETTLED|RESCINDED|EXPIRED|REJECTED) — replayed SETTLED stays idempotent (spec §17.6)"),
	localRow("CLS_REFERENCE_DATA_MISSING", 503, "Phase-24 Task 24.3.8",
		"No ACTIVE cls_reference_versions row — currency/product/member eligibility and cut-off evaluation fail closed without versioned reference data (spec §17.6, §2.7)"),
	localRow("CLS_NOT_ELIGIBLE", 422, "Phase-24 Task 24.3.8",
		"Pair/product/member fails the versioned CLS eligibility checks — the caller must route through the settlement-risk waterfall (alternative PvP → bilateral netting → controlled gross) (spec §17.6 step 5)"),
	// Phase-24 Task 24.3.12 — bank statement ingestion codes emitted by
	// internal/settlement/statement_parser.go + the backoffice parsers.
	localRow("STATEMENT_MALFORMED", 400, "Phase-24 Task 24.3.12",
		"Bank statement file failed structural validation — missing mandatory tags/fields, undecodable XML, checksum mismatch or out-of-order sequence; nothing is persisted (spec §17.10, §2.7 fail-closed)"),
	localRow("STATEMENT_PARSER_MISSING", 503, "Phase-24 Task 24.3.12",
		"Statement parser seam not wired — ingestion refuses rather than misparsing a bank file (spec §17.10, §2.7 fail-closed)"),
	localRow("STATEMENT_ACCOUNT_MISMATCH", 422, "Phase-24 Task 24.3.12",
		"Statement IBAN/BIC/currency does not agree with the nostro account it claims to describe — rejected, never ingested (spec §17.10)"),
	localRow("STATEMENT_NOT_FOUND", 404, "Phase-24 Task 24.3.12",
		"bank_statements row does not resolve (spec §17.10)"),
	// Phase-24 Task 24.3.21 — suspense routing over the Phase-11
	// DepositGuard quarantine pipeline.
	localRow("SUSPENSE_ROUTER_MISSING", 503, "Phase-24 Task 24.3.21",
		"Suspense router (funding.DepositGuard) not wired — an unidentified credit cannot be quarantined; the caller must open a manual break instead of dropping the funds (spec §17.16b, §2.7 fail-closed)"),
	// Phase-24 Task 24.3.9 — SSI + bilateral netting codes emitted by
	// internal/settlement/ssi.go and netting.go.
	localRow("SSI_NOT_VERIFIED", 422, "Phase-24 Task 24.3.9",
		"Standing settlement instruction's beneficiary does not resolve to a VERIFIED bank_accounts registry row for the account, or the claimed ref/BIC drifts from the registered record (spec §17.7)"),
	localRow("NETTING_BATCH_STATE_CONFLICT", 409, "Phase-24 Task 24.3.9",
		"Requested operation violates the payment_netting_batches lifecycle (OPEN→NETTED→DISPATCHED→SETTLED|FAILED) — e.g. reopen on a SETTLED batch or dispatch on a non-NETTED row (spec §17.7)"),
	localRow("NETTING_AGREEMENT_MISSING", 422, "Phase-24 Task 24.3.9",
		"Counterparty has no EXECUTED, unexpired ISDA/netting agreement — bilateral netting is not legally enforceable and the run refuses (spec §17.7, §5.26)"),
	// Phase-09 Task 9.3.15 — DORA material-incident closure gate emitted
	// by internal/operations/dora. No spec §23 row yet; the §23-side fix
	// is a transcription row.
	localRow("INCIDENT_CLOSURE_BLOCKED", 409, "Phase-09 Task 9.3.15",
		"Material incident CLOSED transition refused: overdue/pending DORA regulator reports, incomplete P0/P1 post-mortem artifacts, or unresolved/unaccepted remediation items (spec §19.5, §19.8)"),
	// §27.1 MTF/Fair-Value matrix row cites both codes (400, L2 / 503,
	// L1); no §23 row — same transcription gap as the rows above.
	// Emitted by internal/algo fixing admission and
	// internal/derivatives forward/NDF booking.
	localRow("FIXING_WINDOW_CLOSED", 400, "Phase-16 Task 16.3.9",
		"Fixing-order operation targets a benchmark with no enabled upcoming window (disabled/delisted fixing on the instrument's auction calendar) — spec §27.1 MTF/Fair-Value row"),
	localRow("FAIR_VALUE_DIVERGENCE", 503, "Phase-22 Task 22.3.1",
		"Explicit agreed forward/NDF rate diverges from the CIP fair value beyond the 25 bps band (spec §15.3 formula, §27.1 MTF/Fair-Value row, oracle-divergence convention)"),
	// Phase-03 Task 3.3.1 — post-trade balance/fill-settlement codes
	// emitted by internal/settlement (balance_service.go, balance_batch.go)
	// on the engine fill pipeline. Neither is an HTTP response — they
	// surface on the consumer error/alert trail; the §23 fix is a
	// spec-side transcription row. TRADE_FILL_UNRESOLVABLE backfilled here
	// (emitted since the task landed, unregistered); TRADE_ID_COLLISION
	// registered with its implementation.
	localRow("TRADE_FILL_UNRESOLVABLE", 500, "Phase-03 Task 3.3.1",
		"Fill cannot resolve its instrument, account legs, or rate context — the settlement commit fails closed rather than booking against a guessed leg (spec §2.7)"),
	localRow("TRADE_ID_COLLISION", 500, "Phase-03 Task 3.3.1",
		"Incoming fill reuses a trade_id already committed in processed_trades but its stored raw frame differs (or is absent/corrupt) — the signature of an engine trade-id counter regression across restart; dedup fails closed instead of silently stranding the fill's settlement legs (spec §2.7 L0 zero-loss)"),
}
