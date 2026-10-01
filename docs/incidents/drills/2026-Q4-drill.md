# DR Drill Report — 2026-Q4

- generated: 2026-10-01 19:42:23Z
- evidence root: `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/`
- drills requested: D1,D2,D3,D4,D5,D6,D7

| component | RPO target | RPO actual | RTO target | RTO actual | verdict |
|---|---|---|---|---|---|
| order-book-wal | 0ms | 0ms | 10000ms | 1849ms | **PASS** |
| wal-archive | 0ms | 0ms | 30000ms | - | **PASS** |
| postgresql | 15000ms | 0ms | 300000ms | 416ms | **PASS** |
| redis | 5000ms | 0ms | 30000ms | 1198ms | **PASS** |
| clickhouse | 60000ms | - | 1800000ms | 192ms | **PASS** |
| secrets | - | - | - | - | **PASS** |
| region-failover | 0ms | - | 300000ms | - | **SKIP** |
| wal-catchup | 0 | 0 | 30000 | null | **PASS** |
| book-restore | 0 | 0 | 10000 | 2664 | **PASS** |
| edge-reroute | null | null | 30000 | null | **SKIP** |
| secrets-decrypt-secondary | null | null | null | null | **SKIP** |
| resumption-ladder | null | null | null | null | **SKIP** |

## Timeline

- [dr-drill 19:35:13.649] DR drill 2026-Q4 starting — drills=D1,D2,D3,D4,D5,D6,D7 out=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence strict=0
- [dr-drill 19:36:04.159]   D1/wal-catchup: PASS — sealed segments archived to s3://exchange-wal and re-fetched intact
- [dr-drill 19:36:06.869]   D1/book-restore: PASS — secondary reconstruction 2664ms, fingerprint parity e1228ac6fbb82498
- [dr-drill 19:36:06.888]   D1/edge-reroute: SKIP — Anycast/LB health-check flip needs a second network domain
- [dr-drill 19:36:06.901]   D1/secrets-decrypt-secondary: SKIP — DR-critical secret copies need Vault/KMS in the secondary region (Task 9.3.29)
- [dr-drill 19:36:06.919]   D1/resumption-ladder: SKIP — CANCEL_ONLY→auction→Normal ladder against live ingress needs a staged secondary engine
- [dr-drill 19:36:06.933]   D1/region-failover: SKIP — end-to-end cutover pending-infra (single failure domain); composite legs above carry the locally-verifiable evidence
- [dr-drill 19:36:07.284] === D2 postgres failover ===
- [dr-drill 19:36:27.386]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d2.log
- [dr-drill 19:36:27.405]   D2/postgresql: PASS — pg_failover_drill exit=0 (docker semi-sync pair, RTO=416ms gap=0s)
- [dr-drill 19:37:09.504] === D3 archive-status ===
- [dr-drill 19:37:10.718]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d3.log
- [dr-drill 19:37:10.720] === D3 replay-from-archive ===
- [dr-drill 19:38:02.752]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d3.log
- [dr-drill 19:39:48.088]   D3/wal-archive: PASS — archive-status=PASS replay exit=0 segs=5 trades=11469791 archived-bytes entries=55355058 gaps=0 corrupt=false
- [dr-drill 19:39:48.983] === D4 clickhouse restore ===
- [dr-drill 19:39:51.872]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d4.log
- [dr-drill 19:39:51.886]   D4/clickhouse: PASS — local_drill exit=0 (scratch-db restore+verify, RTO=192ms)
- [dr-drill 19:39:51.891] === D5 redis sentinel failover ===
- [dr-drill 19:39:57.277]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d5.log
- [dr-drill 19:39:57.328]   D5/redis: PASS — detect=1191ms write_rto=1198ms rpo_proxy=0ms
- [dr-drill 19:39:57.331] === D6 chaos suite ===
- [dr-drill 19:42:23.294]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d6.log
- [dr-drill 19:42:23.376]   D6/order-book-wal: PASS — chaos suite 18/18 runs; max recovery 1849ms; dups=0 missing=0
- [dr-drill 19:42:23.378] === D7 secrets rotation lifecycle ===
- [dr-drill 19:42:23.606]   exit=0 log=/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d7.log
- [dr-drill 19:42:23.621]   D7/secrets: PASS — secretdrill exit=0 (rotation lifecycle + audit trail); NB: decrypt-in-secondary leg still env-bound (no DR Vault)

## Incidents during drill

- none recorded

## Evidence

- `D1/wal-catchup` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d1/d1-archive.log` — sealed segments archived to s3://exchange-wal and re-fetched intact
- `D1/book-restore` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d1/d1-restore.json` — secondary reconstruction 2664ms, fingerprint parity e1228ac6fbb82498
- `D1/edge-reroute` → `` — Anycast/LB health-check flip needs a second network domain
- `D1/secrets-decrypt-secondary` → `` — DR-critical secret copies need Vault/KMS in the secondary region (Task 9.3.29)
- `D1/resumption-ladder` → `` — CANCEL_ONLY→auction→Normal ladder against live ingress needs a staged secondary engine
- `D1/region-failover` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d1` — end-to-end cutover pending-infra (single failure domain); composite legs above carry the locally-verifiable evidence
- `D2/postgresql` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d2.log` — pg_failover_drill exit=0 (docker semi-sync pair, RTO=416ms gap=0s)
- `D3/wal-archive` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d3` — archive-status=PASS replay exit=0 segs=5 trades=11469791 archived-bytes entries=55355058 gaps=0 corrupt=false
- `D4/clickhouse` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d4.log` — local_drill exit=0 (scratch-db restore+verify, RTO=192ms)
- `D5/redis` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d5.json` — detect=1191ms write_rto=1198ms rpo_proxy=0ms
- `D6/order-book-wal` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/chaos.json` — chaos suite 18/18 runs; max recovery 1849ms; dups=0 missing=0
- `D7/secrets` → `/www/wwwroot/exc.local/docs/incidents/drills/2026-Q4/evidence/d7.log` — secretdrill exit=0 (rotation lifecycle + audit trail); NB: decrypt-in-secondary leg still env-bound (no DR Vault)

## Remediation items

- `D1`: Anycast/LB health-check flip needs a second network domain — owner: TBD, due: next drill
- `D1`: DR-critical secret copies need Vault/KMS in the secondary region (Task 9.3.29) — owner: TBD, due: next drill
- `D1`: CANCEL_ONLY→auction→Normal ladder against live ingress needs a staged secondary engine — owner: TBD, due: next drill
- `D1`: end-to-end cutover pending-infra (single failure domain); composite legs above carry the locally-verifiable evidence — owner: TBD, due: next drill
