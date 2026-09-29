# Testnet Deployment — Phase-14 Task 14.3.3

`testnet.exchange.com` is a **separate deployment identity**, not a flag
on the production stack. The separation is enforced at three layers:

| Layer | Contract |
|---|---|
| DNS / ingress | `testnet.exchange.com` routes to the `exchange-testnet` namespace only. Never a backend of the production gateway. |
| Kubernetes | Dedicated namespace `exchange-testnet` (`00-namespace.yaml`). Services from `../services/` are applied with `namespace: exchange-testnet` (e.g. `kubectl -n exchange-testnet apply -f ../services/` or a kustomize namespace override). |
| Runtime | `EXC_ENVIRONMENT=testnet` (`01-configmap.yaml`). `config.IsProduction()` returns false; `testenv.Service.Enabled()` returns true; `IsTestnet()` reports the dedicated env for observability. |

## Fail-closed mismatch rule

A workload that lands in `exchange-testnet` but resolves
`EXC_ENVIRONMENT` to `production` (or anything unknown/empty) **fails
closed**: every `/api/v1/test/*` endpoint returns `403 FORBIDDEN`. The
label check is centralized in `testenv.Service.Enabled()` — handlers
cannot bypass it.

## Test surface (non-production only)

| Endpoint | Behaviour |
|---|---|
| `POST /api/v1/test/reset` | Clean slate: orders, positions, fills, ledger trail, zeroed balances. 1 reset / account / 5min (Phase-05 Task 5.3.13). |
| `POST /api/v1/test/seed` | Apply a named balance preset (default `standard`: USD 100k / EUR 50k / GBP 25k / CHF 50k / JPY 10m). Additive — no cooldown consumed. |
| `POST /api/v1/test/reset-seed` | Reset + preset in one cooldown slot — the canonical testnet provisioning call. |
| `POST /api/v1/test/funding/deposit` | Simulated faucet credit `{currency, amount}`. |
| `POST /api/v1/test/funding/withdrawal` | Simulated debit; shortfall → `INSUFFICIENT_BALANCE`. |

## No real banking rails — structural, not conventional

Simulated funding mutates `balances` rows only. `internal/testenv` does
not (and must not) import `internal/funding`: no `funding_transactions`
rows, no rail adapters, no OTP/review-tier flow. The
`banking-rails-worker` deployment SHOULD NOT be scheduled into the
testnet namespace — nothing in testnet can produce a real rail payload,
and running it only adds a dead consumer. If it is ever deployed here,
its ExternalSecret reference resolves against the testnet Vault path —
testnet must not mount production rail credentials.

Ledger note: simulated balances intentionally diverge from the journal
ledger; the `testnet` environment label is out of scope for
reconciliation and ledger audit.

## Apply order

```sh
kubectl apply -f 00-namespace.yaml -f 01-configmap.yaml
kubectl apply -f ../services/ -n exchange-testnet   # minus banking-rails-worker
kubectl apply -f ../cronjobs/ -n exchange-testnet   # optional
```

Testnet needs its own PostgreSQL / Redis / NATS infrastructure (or a
dedicated cluster/schema on shared infra with separate credentials —
never the production DSN). Secrets resolve via the same ExternalSecrets
pattern under the `exchange-testnet` Vault path.
