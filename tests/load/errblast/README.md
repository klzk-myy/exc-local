# errblast — high-stress error-injection blaster (Task 8.5.3.3)

Spec refs: §2.7 (fail-closed error dispatch), §24 #308 — "Pre-production
load tests verify platform stability during high-stress error injection
and circuit-breaker tripping".

`errblast` fires a paced stream of **invalid** order submissions at the
gateway's `POST /api/v1/orders` while a low-rate control stream probes
`/health/live` for liveness. The assertion: under rejection pressure the
error-dispatch path stays healthy — every request resolves to a clean
4xx/429 rejection, 5xx stays under threshold, and the gateway never
stops answering the canary.

## Defect classes (round-robin, equal weight)

| class | wire shape | expected rejection |
|---|---|---|
| `negative_quantity` | valid order JSON, `quantity:"-N"` | 4xx |
| `negative_price` | valid order JSON, `price:"-P"` | 4xx |
| `malformed_json` | truncated JSON mid-token | 400 |
| `invalid_signature` | well-formed order + wrong-key Bearer JWT | 401 |
| `invalid_hmac_headers` | well-formed order + garbage `X-API-KEY`/`X-SIGNATURE` | 401 |
| `unknown_instrument` | valid order, `symbol:"ZZZnnn"` | 4xx |
| `enum_violation` | `side:"SIDEWAYS"` | 4xx |
| `missing_fields` | `{"symbol":"EUR/USD"}` | 4xx |
| `oversized_payload` | ~1 MiB JSON body | 413/400 |
| `wrong_content_type` | valid order as `text/plain` | 415/400 |

All authed classes carry a well-formed `Idempotency-Key` (UUIDv7) so the
defect reaches the validation layer it targets instead of being masked
by an idempotency rejection. Canonical symbols use the `EUR/USD` slash
form (the `instruments` table universe).

## Usage

```sh
go build -o errblast .          # in tests/load/errblast

# staging-gate shape (plan: 10k invalid orders/sec under 50k TPS
# background load — run tests/soak or tests/load/bookpump alongside):
./errblast -addr http://gw:8080 -rate 10000 -duration 60s \
    -jwt-secret-b64 <base64-HS256-key> -report errblast.json

# dev check — unauthenticated classes still exercise auth+dispatch:
./errblast -addr http://127.0.0.1:8080 -rate 500 -duration 10s
```

Key flags: `-token` (static Bearer JWT) or `-jwt-secret-b64` +
`-jwt-accounts N` (mint a rotating pool of signed tokens so per-account
rate buckets don't collapse the burst); `-workers`, `-control-rate`,
`-req-timeout`, `-startup-grace`, `-max-5xx-pct` (default 1.0%),
`-xff-pool N` (rotate `X-Forwarded-For` — requires the gateway run with
`EXC_TRUST_PROXY=1`), `-oversize-kb`, `-seed`, `-report`.

## Exit codes

| code | meaning |
|---|---|
| 0 | PASS — rejections clean, control stream held |
| 2 | target presumed crashed (sustained liveness loss / all transport failures) |
| 3 | 5xx share exceeded `-max-5xx-pct` |
| 4 | no usable traffic produced |
| 64 | usage error |

## Tests

```sh
go test ./...                       # harness unit tests (stub gateway)
EXC_ERRBLAST_LIVE=1 go test -v -run TestLiveGatewayErrorBlast
```

Environment knobs for the live leg:

| Var | Default | Purpose |
|-----|---------|---------|
| `EXC_ERRBLAST_LIVE` | unset (SKIP) | enable the live-gateway leg |
| `EXC_REPO_ROOT` | repo root by walk-up | source tree the gateway/oracle binaries build from — point at a clean worktree when the working tree has unrelated in-progress breakage |
| `EXC_PG_DSN` | `postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable` | dev PostgreSQL |
| `EXC_REDIS_TEST_ADDR` | `127.0.0.1:16379` | dev Redis primary |
| `EXC_NATS_URLS` | `nats://127.0.0.1:4222` | dev NATS |

The live leg (`EXC_ERRBLAST_LIVE=1`, needs the docker-compose dev
topology — PG :5433, Redis :16379, NATS :4222) builds and boots the REAL
`cmd/gateway` + `cmd/oracle` binaries against scratch Redis DB 14, then:

1. **Leg A — degradation gate over the wire:** writes
   `system:degradation:mode=ReadOnly` into the coordination Redis and
   asserts `POST /api/v1/orders` → `503 DEGRADED_MODE` with
   `X-Degradation-Mode: ReadOnly` while `GET /api/v1/time` and
   `GET /api/v1/instruments` keep answering (plan step 2). Clearing the
   record restores write-path evaluation.
2. **Leg B — real burst:** a paced 1.5k/s invalid-order storm for 4s
   with rotating minted JWTs and rotating XFF identities; asserts
   verdict 0, the process stays alive, and the degradation header kept
   reporting `Normal` through the storm.

Boot-order caveat the test already handles: the gateway's Phase-13
reconciliation engine runs its first sweep *at boot* and the dirty dev
fixture data legitimately trips `RECONCILIATION_MISMATCH` findings that
raise `halt:*` flags (PgHalter → `halt:global`/scoped flags) — a real
fail-closed halt that would collapse every admission onto
`503 TRADING_HALTED` and mask the defect classes. The test waits for
the sweep's `reconciliation: run` log line, sweeps `halt:*` on the
scratch DB, and asserts the flags stay clear (next tick is ~1h out).
The `trading_suspensions` PG rows the sweep inserts are left in place —
they are the gateway's own correct artifacts; `ReconcileFlags` at the
next boot re-raises them, which is intended fail-closed behavior.

Companion hysteresis coverage lives with the mechanism owners:
`core/tests/test_health.cpp`
(`InfraLagSignalEscalatesToReadOnlyImmediately`,
`ReadOnlyRecoveryDwellRequires30ConsecutiveClearSeconds` — run
`ctest -R health` in `core/build-debug`) and
`services/internal/middleware/degradation_test.go` (`TestDegradationGate_*`).
