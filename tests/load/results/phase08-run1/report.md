# Phase-08 Task 8.3.2 — Load Test Report (20260928T190605Z)

| knob | value |
|---|---|
| target rate | 50000 orders/s |
| duration requested | 600s |
| duration measured | 600.000259808s |
| shard | 0 (md leg: 1 when --md) |
| instrument | 7 |
| cross-pct | 2 |
| accounts | 1000 |
| ipc base | exc_load_162551 |

## Acceptance criteria verdicts

| AC | target | measured | verdict |
|---|---|---|---|
| 50k orders/s sustained 1h | rate=50000 dur>=3600s | rate=7.253330192544649/s dur=600.000259808s | FAIL / BOUNDED (<1h) |
| p99 <= 50us tick-to-trade | <=50us | p99=110437.485us | FAIL |
| p99 <= 5ms REST | <=5000us | FAIL (p99=6311.999us errs=0) |
| zero order loss | send_drops=0 ring_drops=0 dup=0 decode_err=0 | sd=29988233 rd=1919247120 dup=0 derr=0 | FAIL |
| 100+ WS zero drops | conns>=100 gaps=0 disc=0 | PASS (120 conns) |

## Raw counters

- orders_sent=4352 fills=4
- artifacts: /www/wwwroot/exc.local/tests/load/results/phase08-run1
