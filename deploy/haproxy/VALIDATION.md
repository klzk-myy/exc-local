# HAProxy validation drill — Task 5.3.29 DoD evidence

Executed 2026-09-30 against real HAProxy in Docker. Validates the two DoD
rows: **"HAProxy routes traffic to Go services with health-check
failover"** and **"Blue-green deploy switches traffic with zero dropped
requests"** — both **PASS**.

## Environment

| Component | Version / note |
|---|---|
| HAProxy | `haproxy:2.9` → 2.9.15-e872a3f, image created 2025-03-21 (pinned tag ≥7 days old) |
| Docker | 29.8.1 |
| Stub backends | `test/stub_server.py` (python 3.10.12, host, ports 18081–18084) |
| Container→host | `--add-host=host.docker.internal:host-gateway` → 172.17.0.1 |
| Stats socket | `./run/admin.sock` on host via bind-mounted `/run/haproxy` |

## Config defects found by `haproxy -c` (real defects — fixed in haproxy.cfg)

The committed cfg **did not parse at all** on HAProxy 2.9 (4 fatal ALERTs +
ordering WARNINGs). Fixes applied to `haproxy.cfg` (each with a supersedes
comment in place):

1. `acl rate_abuse sc1_http_req_rate(10s,st_req_per_ip)` →
   `sc1_http_req_rate(st_req_per_ip)`. On 2.9 the `sc<N>_*` fetchers take
   only an optional **table** arg; the 10s window is bound by the table's
   `store http_req_rate(10s)`. Same for `sc1_http_err_rate`.
   (Verified the `(period,table)` form also fails on the generic
   `sc_http_req_rate(1,10s,tbl)` spelling on 2.9.)
2. `acl color_green str(green) -m found,map_str(...)` — a converter cannot
   hang off `-m`. Replaced with
   `str(gateway),map_str(/etc/haproxy/active_color.map,blue) -m str green`
   (fetch literal key "gateway", map-convert, match "green", default blue).
3. `use_backend ... if var(txn.is_ws)` — bare fetch is not a valid
   switching-rule condition. Added
   `acl ws_upgrade var(txn.is_ws) -m bool true` and switched the rules to
   `if ws_upgrade`.
4. `tcp-request connection` rules re-ordered above the first
   `http-request` rule with `acl whitelisted`/`acl conn_over` declared
   before them (named ACLs must precede first reference; silences the
   "will still be processed before" WARNINGs).

`haproxy -c -f test/haproxy.test.cfg` on `haproxy:2.9` after fixes:
**clean — zero WARNING, zero ALERT.** The fixed file also parses clean on
`haproxy:lts` (3.x) — the explicit-table form is forward-compatible.

## Test cfg delta (deploy/haproxy/test/haproxy.test.cfg)

`sed`-generated from `haproxy.cfg`; **identical except the 8 `server`
lines** (4 backends × primary+backup): `10.0.x.x:8080` →
`host.docker.internal:1808x`. Maps (`active_color.map`, `geo-block.map`,
`whitelist.map`) and `certs/` are bind-mounted at `/etc/haproxy/` so all
in-cfg paths are unchanged. One runtime concession: the admin socket is
`chmod 666` via `docker exec` after start so the unprivileged host user
can drive it — cfg keeps `mode 660` (prod value).

## TEST A — routing + health-check failover: PASS

`be_gateway_blue`: `option httpchk GET /health/ready`, `inter 2s fall 3
rise 2`, `gw-blue-2 backup`.

```
baseline:        gw-blue-1 UP (L7OK), gw-blue-2 UP (L7OK); GET / -> BLUE-1
kill BLUE-1 @    18:34:52.970
first check fail  +2.22s   (status "UP 2/3", check=L4CON)
fully DOWN        +6.66s   (= inter 2s x fall 3, as configured)
fall window:      4 probes: 2 ok / 2 x HTTP 503 (see caveat)
post-DOWN:        10/10 -> 200, body BLUE-2 (backup serving)
restart BLUE-1    18:35:00.18
marked UP         +3.51s   (= rise 2 x inter 2s)
post-recovery:    GET / -> BLUE-1 (leastconn returns to primary)
```

**Caveat (honest):** during the `fall` window a hard-killed primary still
attracts dispatches (it is nominally UP) while the `backup` peer stays
ineligible; `option redispatch`/retries then have no live non-backup to
land on → bounded 503s (~6s worst case). This is expected HAProxy
semantics, and why the runbook drain step exists (`set server ... state
maint` before deploy kills). Graceful drain = zero 503s; hard kill =
bounded 503 window.

## TEST B — blue/green zero-drop flip: PASS

500 sequential `GET /` through `https_in` (~55ms pace, under the 500/10s
flood ladder). Map flipped live via the documented mechanism:
`set map /etc/haproxy/active_color.map gateway green` at req 250,
`gateway blue` at req 400 (via `socat run/admin.sock`).

```
HTTP 200: 500/500   TOTAL FAILED: 0
req 1–249   -> BLUE-1     req 250–399 -> GREEN-1     req 400–500 -> BLUE-1
```

Both flips take effect on the **very next request** — no reload, no
connection teardown, zero drops.

## Edge probes

| Probe | Result |
|---|---|
| `GET http://:8080/` | 301 → https (redirect confirmed in access log) |
| WS upgrade `/ws/v1/stream` | `101 Switching Protocols`, valid `Sec-WebSocket-Accept`, log `be=be_ws_blue` |
| WS upgrade `/api/other` (no `/ws/` prefix) | 101 via `be_gateway_blue` — WS routing correctly gated on `path_beg /ws/` |
| both blue servers `state maint` | 503 "No server is available to handle this request"; `state ready` → instant 200 |
| `TRACE` | 405 (method allowlist) |
| `GET` + `Content-Length` | 400 (smuggling rule) |
| duplicate `Content-Length` | 400 |
| 2 MiB POST body | 413 (1 MiB cap) |
| `GET /metrics` on `listen stats` (127.0.0.1:8404) | 200 Prometheus exposition |
| TLS | TLS 1.3-only bind; self-signed PEM in `test/certs/` — cert provisioning path itself is config-only (`/etc/haproxy/certs/` dir, Phase-13.5 pipeline) |

## Reproduce

```sh
cd deploy/haproxy/test
./run_drill.sh            # stubs + haproxy -c + container (socket at run/admin.sock)
docker exec -u root haproxy-5329-test chmod 666 /run/haproxy/admin.sock
./test_a_failover.sh      # health-check failover timings
./test_b_bluegreen.sh     # 500-req zero-drop flip
./run_drill.sh stop       # teardown
```
