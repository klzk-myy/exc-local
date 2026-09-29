# Phase-13.5 Task 13.5.3.3 — Compliance Validation Report

**Scope:** pre-Phase-21 controls — sanctions screening (deposit + withdrawal),
dual control on all §8.2 sensitive operations, audit hash-chain integrity.
Regulatory-reporting criteria are deferred to the post-Phase-21 audit
([compliance-deferred-phase21.md](./compliance-deferred-phase21.md)).

**Environment:** dev PostgreSQL `127.0.0.1:55433`, dev Redis `127.0.0.1:6379`
(integration runs use `EXC_PG_TEST=1`, `EXC_TABLETOP=1`).

---

## 1. Sanctions screening — DONE

### Implementation

| Piece | Location |
|---|---|
| File-backed list screener | `services/internal/compliance/sanctions.go` — `NewListScreener` (:73), `ScreenDeposit` (:220), `ScreenWithdrawal` (:238) |
| Parsers | `parseOFACSDN` (:259, OFAC-style CSV), EU consolidated XML, UN consolidated XML, `parsePlainList` (:394) |
| Matching | reuses `funding.NormalizeLegalName` + `funding.JaroWinkler` (`internal/funding/jaro_winkler.go`), threshold `NameMatchThreshold = 0.85` — exact, normalized, fuzzy ≥0.85 |
| Deposit seam | `funding.DepositService.WithSanctions` (`internal/funding/deposit.go`) — pre-existing interface, now implemented by a real screener |
| Withdrawal seam | `internal/funding/withdrawals.go` — `WithdrawalScreener` (:68), `WithSanctions` (:131), `WithBeneficiaryResolver` (:139), screen at :497/:567 |
| Beneficiary resolution | `funding.BeneficiaryRow.BeneficiaryName` via `BeneficiaryByDestination` (`store_flows.go`) — outbound screen runs on the registered legal name, not the raw destination |
| Gateway wiring | `cmd/gateway/main.go` :300–324 — `EXC_SANCTIONS_LIST_DIR`; same screener instance wired to `depositSvc.WithSanctions` and `withdrawalSvc.WithSanctions` + `WithBeneficiaryResolver(fundStore)` |
| Dev fixtures | `deploy/security/sanctions-dev/` — `ofac-sdn-dev.csv`, `eu-consolidated-dev.xml`, `un-consolidated-dev.xml`, `local-fixture-dev.txt` (development-only, clearly labeled — NOT official production feeds) |

### Fail-closed contract (validated)

| Condition | Behavior |
|---|---|
| Configured list dir missing/unloadable | gateway startup fails (`sanctions screener: …`) |
| Directory with no usable entries / empty list file | `NewListScreener` error — fail closed |
| Malformed list file | load error — fail closed |
| Deposit: screener absent (env unset) | `SANCTIONS_UNAVAILABLE` — deposit held, not auto-completed |
| Deposit: positive hit | `SANCTIONS_HIT` → `PENDING_REVIEW` |
| Withdrawal: screener absent, STANDARD tier | `PENDING_REVIEW` + `SANCTIONS_UNAVAILABLE` |
| Withdrawal: screener absent, AUTO tier | confirms (documented pre-production residual — auto-tier has no review obligation pre-Phase-21) |
| Withdrawal: screener outage | `SANCTIONS_SERVICE_UNAVAILABLE`, withdrawal stays `PENDING` — funds do not move |
| Withdrawal: positive hit | `SANCTIONS_HIT` in `WithdrawalResult.Flags`, routed `PENDING_REVIEW` (4h window >$50K tier) |

### Test evidence

```
=== RUN   TestITSanctionsDepositBlocks           --- PASS
=== RUN   TestITSanctionsWithdrawalBlocks        --- PASS
=== RUN   TestITSanctionsCleanCounterpartyPasses --- PASS
```
(`internal/compliance/sanctions_integration_test.go` — real PG scratch
schema + real migrations + real Redis; dev fixture counterparty blocked
in BOTH directions.)

Unit tests (`sanctions_test.go`, `internal/funding/withdrawal_sanctions_test.go`):
exact / normalized / fuzzy-≥0.85 / non-match; each list format; malformed +
unusable dir; hit/outage/nil-screener tiers; beneficiary-name screening;
`flags` JSON shape. All `go test ./internal/compliance/ ./internal/funding/`
green.

## 2. Dual control — DONE (matrix)

`admin.SensitiveOperation` registry (`internal/admin/dualcontrol.go:63`)
vs enforcement seam per operation:

| Operation | Registry | Enforcement | Mechanism | Status |
|---|---|---|---|---|
| kill-switch | ✓ | `killswitch_service.go` `requireApprover` (:183) + `trading_suspensions.approved_by` | synchronous approver_id; GLOBAL/COUNTERPARTY require a distinct authorizer | ENFORCED |
| balance-adjustment | ✓ | admin balance-adjust handler approver gate | synchronous four-eyes | ENFORCED |
| manual-liquidation | ✓ | `handlers_admin.go` `Execute` — `approver_id` + role check, `DUAL_CONTROL_REQUIRED` | synchronous approver_id | ENFORCED |
| withdrawal-override | ✓ | funding override flow approver gate | synchronous four-eyes | ENFORCED |
| admin-role-change | ✓ | `handlers_rbac.go` :159/:200 `dual.Submit` + executor :224 | `admin_dual_control_requests` queue, 15-min window | ENFORCED |
| fee-tier-change | ✓ | `fees.go` promo approve — approver ≠ creator, 15-min window (:190–194) | synchronous window | ENFORCED |
| release-suspended-account | ✓ | admin lifecycle release approver gate | synchronous four-eyes | ENFORCED |
| deploy-to-production | ✓ | `handlers_fleet.go` `approver_id` (:136/:158) | synchronous four-eyes | ENFORCED |
| break-glass | ✓ | `lifecycle.go` `second_approver_id` (:617–681) + unreachable-approver P0 escape | synchronous + escape alarm | ENFORCED |
| instrument-maintenance | ✓ | Phase-15 maker-checker surface | registry consistent (see fix below) | ENFORCED |
| circuit-breaker-reset | ✓ | `handlers_circuit_breaker.go` :156 `dual.Submit` + executor :181 | queue, 15-min window | ENFORCED |
| api-key-expiry-extend | ✓ | `solvency.go` :166 `dual.Submit` + executor :196; route flag corrected | queue, 15-min window | ENFORCED |

**Fixes applied during validation:**
- `OpInstrumentMaintenance` was declared but missing from the
  `SensitiveOperation` switch — added (`dualcontrol.go:66–72`).
- `PUT /api/v1/admin/api-keys/{id}/extend-expiry` route lacked the
  `DualControl: true` metadata flag despite queue-enforced handling —
  corrected (`internal/gateway/routes_v1.go`, +`x-dual-control` in
  `docs/openapi/openapi.json`). `POST /admin/settlement-exceptions/{id}/resolve`
  likewise flagged (money-moving Phase-24 stub).

Queue contract verified by `internal/admin/dualcontrol_test.go` +
solvency/handlers tests: maker ≠ approver, role-eligibility, `PENDING →
APPROVED → EXECUTED/REJECTED/EXPIRED`, 15-minute `ApprovalWindow`,
in-transaction executor.

## 3. Audit hash chain — DONE

`cmd/verify-audit` run against a scratch PG schema populated via the
canonical payload format (migration 009 + 021 chain):

```
=== CLEAN VERIFY ===
verify-audit: date=2026-09-29 rows_checked=3
merkle: date=2026-09-29 root=12ca338f…9cee (no stored root for date)
verify-audit: OK                     EXIT=0

=== TAMPERED payload ===
payload_hash mismatch → verify-audit: FAILED   exit status 2

=== TAMPERED prev_hash link ===
chain link mismatch: predecessor tampered, missing, or this row rewritten
verify-audit: FAILED                 exit status 2
```

Clean chain exits 0; any mutated payload or predecessor link exits
non-zero — the §8.x tamper-evidence control is operative.

## 4. AC summary

- [x] Sanctions screening invoked on deposit/withdrawal — OFAC, EU, UN
  (dev-format fixtures) loaded and enforced both directions
- [x] Dual control verified on all §8.2 sensitive ops (matrix above)
- [x] Audit hash chain integrity verified — `verify-audit` non-zero on
  tampering (payload + chain-link)
- [x] Deferred Phase-21 criteria documented —
  [compliance-deferred-phase21.md](./compliance-deferred-phase21.md)

**Residual risks / honesty notes:**
1. Dev fixtures are NOT official sanctions feeds — production requires the
   Phase-21 vendor integration (deferral doc §1).
2. AUTO-tier withdrawals (<$10K) without a configured screener still
   confirm — acceptable pre-production residual; production profile must
   set `EXC_SANCTIONS_LIST_DIR` (fail-closed startup otherwise).
3. `balance-adjustment`/`withdrawal-override`/`release-suspended-account`
   use the synchronous approver-field pattern rather than the queue —
   equivalent four-eyes guarantee, different mechanics (noted for the
   Phase-21 case-management consolidation).
