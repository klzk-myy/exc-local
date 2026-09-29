# Sanctions List — DEVELOPMENT FIXTURE ONLY

**This directory is a development/test fixture. It is NOT a real
sanctions list and MUST NOT be deployed to production.**

Phase-13.5 Task 13.5.3.3 wires a file-backed sanctions screener
(`services/internal/compliance/sanctions.go`, `compliance.ListScreener`)
into the deposit and withdrawal flows so the screening path is
executable end-to-end in dev and CI. These files exist purely to give
that screener deterministic entries to match — every name below is
fictitious (patterened after classic test fixtures, e.g. "Dr Evil
Testcase").

**Production vendor feeds (OFAC SDN delta pulls, EU consolidated list,
UN consolidated list, commercial providers such as Dow Jones /
World-Check, alias resolution, PEP/adverse-media and ongoing
monitoring) are owned by Phase-21 compliance work.** See
`docs/security/compliance-deferred-phase21.md` for the deferral record
and the post-Phase-21 re-audit checkpoint.

## Files

| File                       | Format                                  | Parser          |
|----------------------------|-----------------------------------------|-----------------|
| `ofac-sdn-dev.csv`         | OFAC SDN CSV (name in field 2)          | `parseOFACSDN`  |
| `eu-consolidated-dev.xml`  | EU consolidated XML (`<WHOLENAME>`)     | `parseConsolidatedXML` |
| `un-consolidated-dev.xml`  | UN consolidated XML (`<INDIVIDUAL>` name parts) | `parseConsolidatedXML` |
| `local-fixture-dev.txt`    | One name per line                       | `parsePlainList`|

## Dev names on the fixture

Normalized, these load as (among others):

- `EVIL TESTCASE` — `Dr Evil Testcase` (UN), fuzzy-matches e.g. "Dr. Evil Test-Case"
- `MAXIMILIAN POWERS` — EU whole-name
- `SANCTIONED SANCTIONOVICH` — OFAC CSV entry
- `BLOCKED BENEFICIARY TRADING` — local txt fixture

Anything matching `>= 0.85` Jaro-Winkler similarity to a listed entry
counts as a hit (threshold is `funding.NameMatchThreshold`, spec-pinned).

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
