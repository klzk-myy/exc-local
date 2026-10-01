# DR Drill Report — 2026-Q4

- generated: 2026-10-01 18:35:06Z
- evidence root: `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/`
- drills requested: D1,D2,D3,D4,D5,D6,D7

| component | RPO target | RPO actual | RTO target | RTO actual | verdict |
|---|---|---|---|---|---|
| order-book-wal | 0ms | 0ms | 10000ms | 1775ms | **PASS** |
| wal-archive | 0ms | - | 30000ms | - | **SKIP** |
| postgresql | 15000ms | 0ms | 300000ms | 454ms | **PASS** |
| redis | 5000ms | 0ms | 30000ms | 1084ms | **PASS** |
| clickhouse | 60000ms | - | 1800000ms | - | **SKIP** |
| secrets | - | - | - | - | **PASS** |
| region-failover | - | - | - | - | **SKIP** |

## Timeline

- [dr-drill 18:32:21.197] DR drill 2026-Q4 starting — drills=D1,D2,D3,D4,D5,D6,D7 out=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence strict=0
- [dr-drill 18:32:21.215]   D1/region-failover: SKIP — no secondary provisioned (--secondary-pg/--secondary-redis); pending-infra per docs/ops/dr.md
- [dr-drill 18:32:21.249] === D2 postgres failover ===
- [dr-drill 18:32:34.591]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d2.log
- [dr-drill 18:32:34.610]   D2/postgresql: PASS — pg_failover_drill exit=0 (docker semi-sync pair, RTO=454ms gap=0s)
- [dr-drill 18:32:34.625]   D3/wal-archive: SKIP — EXC_S3_WAL_BUCKET unset — no archive endpoint
- [dr-drill 18:32:34.682]   D4/clickhouse: SKIP — clickhouse-backup binary missing (CHB_BIN=/tmp/clickhouse-backup)
- [dr-drill 18:32:34.686] === D5 redis sentinel failover ===
- [dr-drill 18:32:39.915]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d5.log
- [dr-drill 18:32:39.972]   D5/redis: PASS — detect=1078ms write_rto=1084ms rpo_proxy=0ms
- [dr-drill 18:32:39.975] === D6 chaos suite ===
- [dr-drill 18:35:06.179]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d6.log
- [dr-drill 18:35:06.259]   D6/order-book-wal: PASS — chaos suite 18/18 runs; max recovery 1775ms; dups=0 missing=0
- [dr-drill 18:35:06.261] === D7 secrets rotation lifecycle ===
- [dr-drill 18:35:06.427]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d7.log
- [dr-drill 18:35:06.443]   D7/secrets: PASS — secretdrill exit=0 (rotation lifecycle + audit trail); NB: decrypt-in-secondary leg still env-bound (no DR Vault)

## Incidents during drill

- none recorded

## Evidence

- `D1/region-failover` → `deploy/dr/failover-runbook.sh` — no secondary provisioned (--secondary-pg/--secondary-redis); pending-infra per docs/ops/dr.md
- `D2/postgresql` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d2.log` — pg_failover_drill exit=0 (docker semi-sync pair, RTO=454ms gap=0s)
- `D3/wal-archive` → `` — EXC_S3_WAL_BUCKET unset — no archive endpoint
- `D4/clickhouse` → `` — clickhouse-backup binary missing (CHB_BIN=/tmp/clickhouse-backup)
- `D5/redis` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d5.json` — detect=1078ms write_rto=1084ms rpo_proxy=0ms
- `D6/order-book-wal` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/chaos.json` — chaos suite 18/18 runs; max recovery 1775ms; dups=0 missing=0
- `D7/secrets` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d7.log` — secretdrill exit=0 (rotation lifecycle + audit trail); NB: decrypt-in-secondary leg still env-bound (no DR Vault)

## Remediation items

- `D1`: no secondary provisioned (--secondary-pg/--secondary-redis); pending-infra per docs/ops/dr.md — owner: TBD, due: next drill
- `D3`: EXC_S3_WAL_BUCKET unset — no archive endpoint — owner: TBD, due: next drill
- `D4`: clickhouse-backup binary missing (CHB_BIN=/tmp/clickhouse-backup) — owner: TBD, due: next drill
