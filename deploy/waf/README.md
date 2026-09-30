# deploy/waf — OWASP CRS payload-inspection layer (Task 9.3.13, spec §19.1, §24 #160)

The "full WAF" tier of `deploy/edge/README.md`: HAProxy's ACLs cover
framing/rate/volume; **ModSecurity v3 + OWASP CRS behind nginx**
(`owasp/modsecurity-crs:nginx`, official image) inspects payloads — SQLi,
XSS, path traversal, RCE — that HAProxy does not parse.

## Topology

```
client ──TLS──▶ CRS/nginx :8080 ──BACKEND──▶ HAProxy edge ──▶ gateway
                 (this layer)                 (deploy/haproxy)
```

CRS runs **in front of** the edge, never behind it — this is the standard
pattern since HAProxy does not natively run ModSecurity CRS. In the drill
topology the backend is the Task 5.3.29 Go stub instead of HAProxy.

## Contents

| File | Purpose |
|---|---|
| `docker-compose.waf.yml` | Self-contained CRS + stub stack (`docker compose -f … up`). `BACKEND` env points CRS at the stub; in staging/prod it points at the HAProxy edge. The stub runs from bind-mounted canonical source (`deploy/haproxy/test/stub_server.go` via `go run` in `golang:1.23`) — REST 200 + honest WS 101. |
| `rules/after-crs/99-exc-exchange.conf` | Exchange-specific CRS tuning hook, loaded after the ruleset. Comments-only today — PL1 needs no exemptions. |
| `../../scripts/waf_drill.sh` | Automated proof: real CRS container, real attack payloads → 403 + rule ids, legit → 200, WS upgrade → 101. |

## Verified (waf_drill.sh, this host)

- Image `owasp/modsecurity-crs:nginx` @ `sha256:ac057618…fe5be`, **OWASP CRS 4.29.0**, `SecRuleEngine On`, PL1, anomaly inbound threshold 5.
- `GET /?id=1' OR '1'='1` → **403** (rule 942xxx SQLi family)
- `GET /?q=<script>alert(1)</script>` → **403** (rule 941xxx XSS family)
- `GET /?f=../../etc/passwd` → **403** (rule 930xxx path-traversal family)
- `GET /api/v1/instruments` → **200**, body = backend stub color (proves clean traffic is proxied through)
- WS upgrade `GET /ws/stream` + `Upgrade: websocket` → **101** through the proxy

## Honest limits

- **WS frames are not inspected.** ModSecurity sees the HTTP upgrade
  request only; post-upgrade WebSocket frames are tunnel bytes at every
  paranoia level. Frame-level abuse protection stays in the gateway
  (Task 6.3.7 WS message rate limiting) and the HAProxy conn-cap/whitelist
  ladder. The CRS layer still inspects the upgrade request itself.
- **PL1 now; PL2 is the `api.*` target** once false-positive exclusions
  are tuned in `rules/after-crs/` against real order payloads.
- This is the **origin-side WAF tier**. Managed L3/L4 DDoS remains the
  provider layer (`deploy/cloudflare/`) — this composes under it.
- FIX (Phase-18) is on private cross-connects and never routes here.

## Ruleset maintenance

Pin by digest (recorded in `docker-compose.waf.yml`); a bump is a ruleset
change → re-run `waf_drill.sh`, and regression-test with `go-ftw` when the
CI lane exists. Audit log is `MODSEC_AUDIT_LOG=/dev/stdout` JSON
(`RelevantOnly`) — blocked events carry rule ids for the SIEM/Prometheus
`waf_block` counter described in `deploy/edge/README.md`.
