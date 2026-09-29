# Vulnerability Disclosure Policy

**Program:** Exchange Vulnerability Disclosure Program (VDP) & Coordinated Bug Bounty
**Canonical URL:** `GET /api/v1/security/policy` (this document)
**Submission:** `POST /api/v1/security/disclosures` (JSON form; no account required)
**Register:** every accepted report is tracked in the exchange's disclosure
register with system-written SLA milestone timestamps.
**Version:** 1.0 — Phase-13.5 Task 13.5.3.8 (spec §19.11.2, §24 #332)

---

## 1. Scope

### In scope

- **REST API** — all `/api/v1/*` endpoints (public, authenticated, and admin
  surfaces reachable with credentials you legitimately hold or can bypass).
- **WebSocket API** — `/ws/v1` market-data and private streams, including
  session-resume and subscription authorization.
- **FIX gateways** — order entry and drop-copy sessions (authentication,
  entitlement, sequencing).
- **SBE binary gateway** — multicast market data and the TCP
  replay/snapshot recovery service.
- **Admin surfaces** — the admin console API and RBAC enforcement
  (privilege escalation, cross-scope access, dual-control bypass).
- **Core services** — order pipeline, ledger, funding rails adapters,
  reconciliation — where reachable through the surfaces above.

### Out of scope

- **Mobile applications** — none exist in v1 (business ruling R1).
- Third-party banking rails, nostro/vostro bank systems, and CLS — report
  directly to the provider; we will route partner-relevant findings under
  our coordinated-disclosure duty.
- Social engineering, phishing, or physical attacks against staff or
  researchers.
- Denial-of-service / volumetric testing of any kind, including
  rate-limit stress testing beyond the published tiers.
- Spam, content injection without a security impact, missing SPF/DMARC
  records, clickjacking on pages without sensitive actions, and
  self-XSS.
- Vulnerabilities requiring rooting/jailbreaking of the reporter's own
  device, or MITM positions requiring physical access.
- Findings in staging/test environments that cannot affect production
  data or funds (they are still welcome — triaged as informational).

**Testing rules:** use only accounts you own or created. Do not access,
modify, or delete data belonging to other users. Do not move, freeze, or
touch funds — a proof-of-concept that stops short of actual transfer is
sufficient. Automated scanning must stay under 5 requests/second against
any single endpoint.

## 2. Safe Harbor

We will not initiate legal action, suspend your accounts, or refer you
to law enforcement for security research conducted **in good faith**
under this policy: you follow the scope and testing rules above, you do
not exploit a finding beyond what is needed to demonstrate it, you do
not access or retain third-party data, and you give us a reasonable
remediation window before any disclosure. Good-faith research that
accidentally violates this policy is still treated in good faith — tell
us what happened in the report.

Reports submitted through `POST /api/v1/security/disclosures` are
presumed good-faith coordinated disclosures.

## 3. Response SLAs

All clocks start at submission and are tracked by system-written
timestamps in the disclosure register — SLA evidence cannot be edited by
hand. Breaches page Security/DevOps automatically (`VDP_SLA_BREACH`).

| Milestone | SLA |
|---|---|
| Acknowledge | ≤ **72 hours** (wall clock) |
| Triage (severity + CVSS 3.1 assigned) | ≤ **5 business days** |
| Fix ETA | per severity, below |

### Fix ETA by severity (Task 13.5.3.9 contract)

| Severity | CVSS 3.1 band | Fix ETA from triage | Bounty tier |
|---|---|---|---|
| Critical | 9.0 – 10.0 | **7 days** | up to $25,000 |
| High | 7.0 – 8.9 | **30 days** | up to $10,000 |
| Medium | 4.0 – 6.9 | **90 days** | up to $2,500 |
| Low | 0.1 – 3.9 | **180 days** (next scheduled maintenance window) | swag / hall-of-fame |
| Informational | 0.0 | best-effort | thanks |

**Change-freeze rule:** a Critical report triaged inside a deployment
change-freeze window does **not** wait out the freeze — it is flagged on
the expedited emergency-change lane and ships through the incident
change process with its 7-day clock intact.

## 4. Coordinated disclosure & bulletin patching

- Dependent findings are remediated **together** under one security
  bulletin rather than per-report releases — fixes ship through the
  normal canary + blue-green pipeline; the bulletin covers the batch.
- We ask for coordinated disclosure: no public details until the fix is
  deployed or 90 days from acknowledgment, whichever comes first — we
  will agree an earlier date with you when the fix lands faster.
- **Critical reports must come through the private coordinated path**
  (this submission channel, marked `suggested_severity: CRITICAL`, or
  direct contact with the security team). Public pre-disclosure of a
  critical issue forfeits bounty eligibility.
- **Attribution:** researchers are credited in the security bulletin on
  request (`attribution_requested` at intake or ask your triage
  contact). Pseudonymous handles are honored. Decline is equally
  respected.

## 5. Bounty tiers

The payout schedule above is published up front and rated by the
triaged severity — not the reporter's self-assessment. Bonus factors
(+50%): fund-safety impact, exploit demonstrated end-to-end against
production authorization boundaries. Reduced/zero: out-of-scope,
duplicate of a known finding already in remediation, findings requiring
victim interaction we cannot reasonably prevent, or policy violations.

## 6. Pentest findings

Scheduled external penetration tests (annual, plus on material
architecture change) file into this same register — same queue, same
SLA clocks — so contractual pentest findings and researcher reports are
never silently triaged differently.

## 7. What to include in a report

`title`, `affected_components` (e.g. `REST`, `WS`, `FIX`, `SBE`, `ADMIN`,
`CORE`), and `reproduction` steps are required. Add `reporter_handle` and
`contact_email` so we can reach you for triage; `report_id` lets you
retry safely — resubmission with the same key returns the original, never
a duplicate. `suggested_severity` and `attribution_requested` are
optional.

---

*This policy is a controlled document under the same change control as
the exchange specification (§19.11.2). Questions not covered here go
through the same submission channel marked `title: "[policy question]"`.*
