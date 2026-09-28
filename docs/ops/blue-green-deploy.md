# Blue-Green Deploy, Canary Gate & Rollback Runbook

**Tasks:** Phase-09 9.3.3 + 9.3.26 · **Spec:** §19.2 step 4, §19.10.1, §20.4, §24 #309 · **Artifacts:** `deploy/scripts/{bluegreen,canary-check,rollback}.sh`, `deploy/haproxy/{haproxy.cfg,active_color.map}`, `deploy/k8s/services/order-gateway.yaml`.

---

## 1. Topology

```
client → HAProxy :443 (deploy/haproxy/haproxy.cfg)
           │  acl color_green reads /etc/haproxy/active_color.map
           ├── be_gateway_blue / be_ws_blue  → order-gateway-blue svc :8080
           └── be_gateway_green / be_ws_green → order-gateway-green svc :8080
```

`active_color.map` is the single switch. Runtime flip (zero-drop, no reload):

```sh
echo "set map /etc/haproxy/active_color.map gateway green" \
  | socat /run/haproxy/admin.sock -
```

`deploy/scripts/bluegreen.sh` performs exactly this plus persist-back to the
repo copy. Never edit the map file alone on the LB host — HAProxy reads the
runtime map, the file is the boot-time seed.

## 2. Normal deploy (Go services)

```sh
deploy/scripts/bluegreen.sh deploy --image-tag v1.2.3
# = deploy idle color → wait rollouts → per-pod /health/ready smoke →
#   canary-check.sh --window 300 → map flip → hold old color 30min → scale 0
```

Pre-switch gates, all enforced before the map flip (spec §19.10):

| Gate | Check | Failure action |
|---|---|---|
| Rollout | `kubectl rollout status` per deployment | abort, idle stays idle |
| Readiness | every endpoint `GET /health/ready` = 200 (R9 schema) | abort |
| Canary window | `canary-check.sh` 300s: readiness probes + Prometheus 5xx-rate ≤1% + optional synthetic order probe | `DEPLOYMENT_AUTOMATED_ROLLBACK` — failed color scaled to 0 |
| Hold | old color kept warm 30 min (`HOLD_SECONDS`) | `rollback.sh` instant revert |

## 3. Rollback

```sh
# Traffic-level revert (fast path — old color still warm):
deploy/scripts/rollback.sh --color green      # green was bad → flips to blue

# Image-level revert inside a deployment:
deploy/scripts/rollback.sh --image order-gateway-green
```

Manual equivalent if the LB host is unreachable from the deploy shell: run
`map_flip`'s socat line on the HAProxy host, then
`kubectl -n exchange scale deployment/order-gateway-<bad> --replicas=0`.

## 4. Post-switch continuous watch

```sh
deploy/scripts/canary-check.sh --color green --watch --window 900
```

`--watch` invokes `rollback.sh` automatically on any gate trip — this is the
§19.10 "automated traffic roll-back to the prior environment" clause.

## 5. C++ core: NOT blue-green

The matching engine is bare-metal and stateful — it rolls **per shard** with
the 8-step drain/snapshot/swap/replay procedure in
[`shard-binary-swap.md`](shard-binary-swap.md) (`scripts/deploy/shard-swap.sh`),
≥60s between shards (spec §19.6). Never run a color flip and a shard swap in
the same window.

## 6. Failure runbooks referenced by canary gates

| Canary symptom | Runbook |
|---|---|
| `order-gateway-*` pods flapping /health/ready | docs/ops/daemon-supervision.md §"watchdog trip" |
| 5xx burst post-flip | `rollback.sh --color <active>` then investigate `deploy/haproxy` logs |
| Synthetic order timeout | engine health first — `scripts/deploy/verify-shard.sh <shard>` |
| WAL recovery halt on a shard | `docs/runbooks/wal-recovery-halt.md` (per engine FATAL output) |
| Region loss | spec §18.3 DR + Task 9.3.21 quarterly drill |
