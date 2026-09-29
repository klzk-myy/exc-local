# Phase-13.5 Task 13.5.3.3 — Compliance Capabilities Deferred to Phase 21

Per the Phase-13.5 task contract, regulatory reporting features are
implemented in **Phase 21** (Tasks 21.3.1–21.3.7 and the remediation-set
tasks 21.3.11–21.3.21) and can only be validated in a **post-Phase-21
audit checkpoint**. This document is the deferral record — none of the
below is claimed complete at Phase 13.5.

## Deferred capabilities

| Capability | Phase-21 home | Phase-13.5 status |
|---|---|---|
| Production sanctions-provider feed (OFAC/EU/UN official list sync, provenance, delta updates) | Task 21.3.x sanctions provider integration | File-backed dev fixtures only (`deploy/security/sanctions-dev`); `EXC_SANCTIONS_LIST_DIR` seam live |
| Sanctions feed scoped-degradation policy (Phase-21 AC #43) | Phase-21 | Not implemented — current behavior is fail-closed for wired tiers |
| PEP / adverse-media / ongoing monitoring | Task 21.3.11 | Not implemented |
| MiFID II best-execution + transaction reporting (RTS 27/28) | Tasks 21.3.19 + 21.3.x | Not implemented |
| MiFID II APA/ARM post-trade transparency adapters | Task 21.3.16 | Not implemented |
| MiFID II communications recording (taping) | Task 21.3.20 | Not implemented (phone desk out of scope per ruling — taping prerequisite) |
| EMIR / EMIR REFIT + CFTC Parts 43/45 lifecycle reporting | Tasks 21.3.14–15 | Not implemented |
| Dodd-Frank / FinCEN MSB + AML program, CTR | Tasks 21.3.x | Not implemented |
| FATF Travel Rule | Task 21.3.x | Not implemented |
| SAR generation | Task 21.3.x | Not implemented |
| Market-abuse alert enforcement (Phase-17 emits signals; Phase-21 enforces) | Task 21.3.x | Signals only at this stage |
| Surveillance case management | Task 21.3.21 | Not implemented |
| RTS 6 / DEA + order-retention regime | Task 21.3.12 | Not implemented |
| Basel III regulatory reporting | Task 21.3.13 | Not implemented |
| GDPR erasure production workflow / geo-blocking routes | Phase-21 | Design docs exist (`gdpr-erasure-runbook.md`); enforcement routes Phase-21 |
| Data-residency enforcer | Task 21.3.18 | Not implemented |
| FX Global Code 55-principle assessment | Task 21.3.17 | Not implemented |
| Compliance-hold workflow (account holds) | Task 14.3.10 (Phase-14) | Not implemented |
| Production C++ engine-side sanctions hook | Phase-21 integration | Go-service-level screening only |
| Compliance case management (deposit/withdrawal `PENDING_REVIEW` queue UI + SLA tooling) | Phase-21 | Records land in `PENDING_REVIEW` with flags; case-management UI Phase-21 |

## What IS in place at Phase 13.5 (for the post-Phase-21 audit baseline)

- `SANCTIONS_HIT` / `SANCTIONS_UNAVAILABLE` /
  `SANCTIONS_SERVICE_UNAVAILABLE` flags and `PENDING_REVIEW` routing —
  the review-queue seam Phase-21 case management will consume.
- `funding.NormalizeLegalName` + `JaroWinkler` @ `0.85` — the matching
  primitive the production feed integration reuses.
- Audit hash chain + `verify-audit` — the tamper-evidence control that
  Phase-21 regulatory evidence packs will rely on.
- Dual-control queue + synchronous four-eyes paths — the approval
  mechanics Phase-21 compliance workflows reuse.

## Post-Phase-21 audit checkpoint (per task contract)

Re-run this Phase-13.5 validation plus: official-feed provenance checks,
provider-outage scoped-degradation drill (AC #43), jurisdiction report
generation (MiFID II/EMIR/FinCEN), travel-rule message exchange, SAR
workflow, PEP/adverse-media hits, surveillance alert→case→SAR chain.
