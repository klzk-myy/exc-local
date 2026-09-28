# L3/L4 DDoS response playbook — Task 9.3.13

Runbook for a volumetric attack against `api.exc.local` / `ws.exc.local`.
Owns the edge tier only; application degradation runbooks are in
`docs/runbooks/`. Trigger: `EdgeDDoSSuspected` (P1), provider telemetry,
or ops paged on `haproxy_frontend_denied_connections_total` spike.

## Triage (≤5 min)

1. **Classify the flood** from provider analytics (Cloudflare dashboard /
   Shield console) and edge stats:
   - L3/L4 volumetric (SYN/UDP/ICMP flood, amplification) — bandwidth
     pressure on the provider link, conn churn at edge.
   - L7 request flood — `haproxy_frontend_http_requests_total` spike,
     `sc_http_req_rate` topping on a small IP cohort.
   - Mixed — challenge-mode + provider volumetric mitigation together.
2. **Confirm whitelisted clients unaffected** — `whitelist.map` sources
   must stay green; if the flood ladder is denying them, the map is
   stale (fix map, don't relax thresholds mid-incident).
3. Page SRE on-call + announce in #incident. Severity ladder: sustained
   client impact = P1; provider absorbing without client impact = P2.

## Mitigation ladder

| Step | Action | Command / location |
|---|---|---|
| 1 | Provider volumetric on by default; confirm "Under Attack" mode if L7 pressure | Cloudflare Security → Settings; Shield Advanced automatic |
| 2 | L7 challenge mode on API hostnames | Provider dashboard / `deploy/cloudflare/` ruleset flag |
| 3 | Tighten edge rate ladder (10s window): drop `rate_abuse` 500→200, `err_abuse` 50→20 | edit `haproxy.cfg` + `haproxy -sf` reload |
| 4 | Emergency-source drop list | `add map /etc/haproxy/geo-block.map <CIDR>` on the admin socket — or a dedicated `flood-block.map` for attack sources (distinct from legal geo-block) |
| 5 | Stick-table inspection for attack shape | `echo "show table st_req_per_ip" \| socat /run/haproxy/admin.sock -` |
| 6 | If origin still hot: circuit to `Maintenance` degradation | Task 2.3.6 mode manager (§19.9) — market data keeps flowing, writes halt |

## WS-specific care

WS tunnels are exempt from the request-rate ladder (long-lived
connections look like floods). If the attack is *connection churn* on
`ws.*`, tighten `conn_over` (2000→500) instead; the gateway's per-session
message caps (Task 6.3.7) bound in-tunnel abuse.

## Post-incident

- Record attack profile (peak req/s, source ASN cohort, duration) in the
  incident doc; feed top talkers into `flood-block.map` if they recur.
- Review stick-table thresholds — did legit clients approach limits?
- File follow-ups: provider rule tuning, edge capacity, CRS false-pos.
- Do NOT leave emergency-tightened ladders in place beyond the incident;
  revert via the same haproxy.cfg edit + reload.
