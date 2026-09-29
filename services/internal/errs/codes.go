// Embedded spec §23 error-code table (Task 5.3.21).
//
// The rows below are a verbatim transcription of the §23 registry table in
// docs/Specification - Complete Exchange System Suite.md (180 codes;
// count updated with the Phase-11 Tasks 11.3.2/11.3.3/11.3.6/11.3.9
// landings — historical remediation counts noted in spec §23's
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

// specCodes is the verbatim spec §23 table (180 rows).
var specCodes = []CodeDef{
	specRow("INVALID_REQUEST", 400, "Malformed request body (Phase-05 Task 5.3.29 API Gateway & Load Balancer — centralized OpenAPI-schema request validation; remediation #19)"),
	specRow("UNAUTHORIZED", 401, "Missing or invalid auth token (Phase-05 Task 5.3.26 WebSocket Authentication Upgrade & In-Flight Token Renewal; REST JWT/OAuth per spec §8.4; remediation #19)"),
	specRow("FORBIDDEN", 403, "Insufficient permissions"),
	specRow("NOT_FOUND", 404, "Resource not found (Phase-05 Task 5.3.7 Route Registration System — emitted by every registered route; remediation #19)"),
	specRow("RATE_LIMIT_TIER_EXCEEDED", 429, "Rate limit hit"),
	specRow("INSUFFICIENT_BALANCE", 400, "Not enough available balance (Phase-02 Task 2.3.3 pre-trade check 2; Phase-05 Task 5.3.24 batch rejection; remediation #35 — owner citation added)"),
	specRow("MARGIN_INSUFFICIENT", 400, "Post-fill margin exceeds threshold (Phase-02 Task 2.3.3 Pre-Trade Risk — post-fill margin check; Phase-19 Task 19.3.1 Margin Modes; remediation #19)"),
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
	specRow("BANKING_RAIL_UNAVAILABLE", 503, "No banking rail can carry the instruction — every candidate failed currency, amount-cap, availability (scoped kill-switch) or account-eligibility screening; fail-closed (Phase-11 Task 11.3.1 Banking Rails Integration; §24 Banking Rails matrix row)"),
	specRow("BENEFICIARY_HOLD_ACTIVE", 422, "Withdrawal target is a VERIFIED beneficiary still inside its 24-hour new-account hold — retry after `unlocked_at` (Phase-11 Task 11.3.7 Beneficiary Bank-Account Registry; §24 #391 hold, verified_at + 24h — distinct from `BANK_ACCOUNT_NOT_VERIFIED` which covers unregistered/unverified destinations)"),
	specRow("WITHDRAWAL_WHITELIST_ONLY", 422, "Whitelist-only mode is enabled for the account and the destination resolves to no VERIFIED bank_accounts beneficiary (Phase-11 Task 11.3.10 Withdrawal Whitelist Mode; §5.23/§24 #391 — registered for the flows cluster)"),
	specRow("WITHDRAWAL_WHITELIST_LOCKED", 423, "Account-scoped withdrawal egress lock — whitelist-only mode was disabled less than 24 hours ago (Phase-11 Task 11.3.10 deactivation safety lock; account-scoped, never platform-wide — remediation #35 semantics)"),
	specRow("WHITELIST_CHANGE_LOCKED", 429, "Whitelist-mode re-enable is rate-limited until timelock_until lapses (Phase-11 Task 11.3.10; the 24h deactivation latch — documented unlock path is to wait out the latch)"),
	specRow("NOSTRO_INSUFFICIENT_FUNDS", 500, "Aggregate ACTIVE nostro balance cannot cover the withdrawal — the withdrawal is QUEUED for dispatch, never rejected (Phase-11 Task 11.3.6 Nostro-Aware Withdrawals; §27.1 Nostro/Vostro matrix code — emitted on the durable funding_ops_alerts trail)"),
}

// localCodes are emitted by the gateway but carry no spec §23 row yet.
// Spec=false marks them for the docs-drift check — each needs a §23 row.
// As of remediation #44 the slice is empty: all emitted codes are §23 rows.
var localCodes = []CodeDef{}
