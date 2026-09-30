# Sanctions List — DEVELOPMENT FIXTURE ONLY

**This directory is a development/test fixture. It is NOT a real
sanctions list and MUST NOT be deployed to production.**

Phase-13.5 Task 13.5.3.3 wires a file-backed sanctions screener
(`services/internal/compliance/sanctions.go`, `compliance.ListScreener`)
into the deposit and withdrawal flows so the screening path is
executable end-to-end in dev and CI. Phase-21 Tasks 21.3.1/21.3.11
extend it with alternate identities (OFAC `alt.csv`), EU/UN XML
aliases, the UK HMT/OFSI consolidated CSV, PEP-kind entries, per-list
provenance (source/version/SHA-256) and reload deltas. These files
exist purely to give that screener deterministic entries to match —
every name below is fictitious (patterned after classic test fixtures,
e.g. "Dr Evil Testcase").

Production vendor pulls are wired through `VendorRefresher`
(`sanctions_refresh.go`): declare feeds in `EXC_SANCTIONS_FEEDS` (JSON
array of `ListFeed` — `{"name","url","filename","required"}`), staged
swap + SHA-256 dedupe + daily cadence, and feed outcomes report into
the `ProviderGate` quarantine (Task 21.3.23). See
`docs/security/compliance-deferred-phase21.md` for the deferral record
and the post-Phase-21 re-audit checkpoint.

## Files

| File                       | Format                                            | Parser                 | Kind/Source            |
|----------------------------|---------------------------------------------------|------------------------|------------------------|
| `ofac-sdn-dev.csv`         | OFAC SDN CSV (name in field 2, ent_num field 1)   | `parseCSVNames`        | SANCTIONS/OFAC_SDN     |
| `ofac-alt-dev.csv`         | OFAC alt.csv (alt name field 3, parent ref field 2)| `parseOFACAlt`        | SANCTIONS/OFAC_SDN_ALT |
| `uk-hmt-dev.csv`           | UK OFSI consolidated CSV (header-driven Name1..6 + Alias* cols) | `parseUKHMTRecords` | SANCTIONS/UK_HMT |
| `eu-consolidated-dev.xml`  | EU consolidated XML (`WHOLENAME`, `NAME_ALIAS`, `ALIAS` attr names) | `parseConsolidatedXML` | SANCTIONS/EU |
| `un-consolidated-dev.xml`  | UN consolidated XML (`INDIVIDUAL` parts + `INDIVIDUAL_ALIAS`) | `parseConsolidatedXML` | SANCTIONS/UN |
| `pep-dev.txt`              | One name per line (`pep*` prefix → PEP kind)      | `parsePlainList`       | PEP/PEP_VENDOR         |
| `local-fixture-dev.txt`    | One name per line                                 | `parsePlainList`       | SANCTIONS/LOCAL        |

## Dev names on the fixture

Normalized, these load as (among others):

- `EVIL TESTCASE` — `Dr Evil Testcase` (UN), fuzzy-matches e.g. "Dr. Evil Test-Case"
- `DOCTOR EVIL`, `EVIL DR` — UN `INDIVIDUAL_ALIAS` rows resolving to DATAID 9990001
- `MAXIMILIAN POWERS` — EU whole-name; aliases `MAX POWERS JUNIOR`, `POWERS MAXIMILIAN`
- `SANCTIONED SANCTIONOVICH` — OFAC CSV entry; aliases `SANCTIONOVICH S`, `SANCTIONED SANCTIONOVICH` resolve to ent_num 9001
- `BLOCKED HOLDINGS` — OFAC entity; aliases `BLOCKED HOLDINGS GROUP`, `BHG` resolve to ent_num 9002
- `EMBARGO MARKETS`, `SANCTIONED SHIPPING` — UK HMT rows (aliases `EM TRADING`, `SS LINES`, `SANSHIP`)
- `BLOCKED BENEFICIARY TRADING` — local txt fixture
- PEP-kind entries (screened via `EntryKindPEP`, never the funding block): `SENATOR FIXTURE EXAMPLE`, `AMBASSADOR DEV PERSONA`, `CONSUL TEST OFFICIAL`

Anything matching `>= 0.85` Jaro-Winkler similarity to a listed entry
counts as a hit (threshold is `funding.NameMatchThreshold`, spec-pinned).
Alias entries are flagged `alias=true` and carry the parent party's
list-native reference so a hit resolves back to the primary listing.

## Wiring

Set `EXC_SANCTIONS_LIST_DIR` to this directory for dev:

```bash
export EXC_SANCTIONS_LIST_DIR=/www/wwwroot/exc.local/deploy/security/sanctions-dev
```

The gateway constructs `compliance.NewListScreener(dir)` at boot;
an unreadable or empty directory is a boot failure (fail closed).
When the variable is unset no screener is wired — documented residual:
STANDARD-tier deposits/withdrawals escalate to PENDING_REVIEW instead
of screening (same fail-closed posture as pre-wire).

`EXC_SANCTIONS_FEEDS` (optional, JSON) enables the daily vendor
refresh loop — e.g. a mounted drop:

```bash
export EXC_SANCTIONS_FEEDS='[
  {"name":"ofac-sdn","url":"file:///var/lib/exchange/vendor/sdn.csv","filename":"ofac-sdn.csv","required":true},
  {"name":"pep","url":"file:///var/lib/exchange/vendor/pep.csv","filename":"pep.csv"}]'
```
