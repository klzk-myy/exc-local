# deploy/edge — WAF + DDoS edge protection (Task 9.3.13, spec §19.1, §24 #160)

Defense-in-depth stack for the public `api.*` and `ws.*` hostnames. FIX
connectivity (Phase-18) lives on private cross-connects and is **never**
routed through this edge.

## Layer responsibilities

| Layer | Where | Enforces |
|---|---|---|
| **L3/L4 DDoS** | Provider (`deploy/cloudflare/` — Cloudflare Spectrum/Magic Transit or AWS Shield Advanced equivalent) | volumetric floods, SYN/UDP amplification, network-level drop at provider PoP before traffic reaches origin |
| **L7 managed rules** | Provider WAF + challenge mode | OWASP-parity managed ruleset, JS/managed challenge for API paths under attack, bot management |
| **Origin edge ACLs** | `deploy/haproxy/haproxy.cfg` (this repo) | TLS 1.3 only, method allowlist, body cap, request-smuggling denies, per-IP conn/rate stick tables, geo-block map, trading-client whitelist |
| **App rate limits** | `internal/ratelimit` (Phase-05) | fine-grained tiered limits + progressive IP bans — additive, never replaced by edge limits |
| **Full WAF** | ModSecurity + OWASP CRS (recommended, see below) | payload inspection: SQLi/XSS/path traversal/rce attempts on request bodies HAProxy does not parse |

## What HAProxy itself enforces (haproxy.cfg)

- **TLS 1.3 only**, `no-tls-tickets`, restricted cipher suites (existing).
- **Method allowlist** `GET POST PUT DELETE OPTIONS HEAD` — TRACE/TRACK/CONNECT denied 405.
- **Body cap** 1 MiB (`req.hdr(content-length) > 1048576` → 413); WS upgrades carry no body; the 64 KiB WS frame cap stays in the gateway.
- **Smuggling denies**: duplicate `Content-Length`, `Content-Length`+`Transfer-Encoding` coexistence, non-chunked TE, body headers on GET/HEAD/OPTIONS → 400.
- **Per-IP connection cap** `conn_cur > 2000` → TCP reject (stick table `st_conn_per_ip`).
- **Per-IP request rate** `http_req_rate(10s) > 500` or `http_err_rate(10s) > 50` → 429 (`st_req_per_ip`); whitelisted + WS exempt.
- **Geo-block** `src -f /etc/haproxy/geo-block.map` → 451. The Phase-21 jurisdiction feed renders the map; edge enforces it.
- **Whitelist** `/etc/haproxy/whitelist.map` — market makers / PB cross-connect exits skip the flood ladder.

Both tables sync via HAProxy peers in production (two-edge L4 anycast);
add a `peers` section when the second edge lands (pending-infra).

## ModSecurity + OWASP CRS (full-WAF companion)

HAProxy's ACLs cover framing/rate/volume, not payload inspection.
Recommended complement: **Coraza** (Go-native WAF embedding OWASP CRS)
as a sidecar to the gateway, or ModSecurity v3 + CRS fronting HAProxy via
SPOE. CRS paranoia level 2 for `api.*`; WS upgrade requests bypass body
rules (SecRule on `REQUEST_HEADERS:Upgrade` exceptions). Ruleset
maintenance: pin CRS version, regression-test with `go-ftw` in CI, tune
paranoia per-endpoint to avoid false positives on order payloads.

## Geo-blocking contract (Phase-21 feed)

`deploy/edge/geo-block.map.example` is the map shape — one CIDR per line.
The Phase-21 geo-block list is the source of truth; a deploy pipeline
renders `geo-block.map` and live-updates it through the admin socket
(`add map`/`del map`) — no reload needed, empty map = no denies.

## Edge metrics → Prometheus / PagerDuty

HAProxy exposes deny counters via `haproxy_frontend_denied_connections_total`
and per-backend `http_requests`; stick-table contents are inspectable on
the admin socket (`show table st_req_per_ip`). Provider-side WAF events
stream via Logpush → the SIEM; a `waf_block`/`waf_challenge` counter
exporter feeds `deploy/prometheus/rules/exchange-alerts.yml`
(`edge.*` group) and pages **P2** on sustained abuse events.

## Negative test (staging, Task 9.3.13 DoD)

```bash
# Flood ladder trip: 600 requests from one test IP in 10s → 429s;
# whitelisted client unaffected (same loop from whitelist.map IP).
for i in $(seq 600); do curl -s -o /dev/null https://api.staging.exc.local/health & done; wait
# Expect: unlisted source → 429s after ~500; whitelisted source → all 200.
# Conn cap: hold 2100 parallel sockets open → TCP rejects beyond 2000.
# Smuggling: curl --http1.1 with both Content-Length and Transfer-Encoding
# headers → 400 before reaching the gateway.
```

Guardrails: run against staging only, whitelist the trading-sim clients
first, and confirm `haproxy_frontend_denied_connections_total` rises for
the blocked sources while whitelisted traffic stays green.
