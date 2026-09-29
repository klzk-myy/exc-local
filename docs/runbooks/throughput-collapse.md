# Runbook: throughput collapse / pipeline stalls (`GatewayTrafficCollapse`, `MarketDataFramesStalled`, `MarketDataDeltasDropped`, `BridgePublishStalled`, `BridgeEventsDropped`)

**Severity:** P1 (event loss / publish stall) / P2 (traffic collapse, marketdata stall) · **Owner:** SRE

## Symptom

Traffic or event volume has collapsed or stalled: gateway request rate dropped >5× vs 1h ago, the bridge receives but does not publish, or the marketdata conflator consumes deltas but emits no frames.

## Diagnosis

### gateway-traffic

1. Confirm the drop is upstream, not internal: `sum(rate(exchange_errors_total{service="gateway"}[10m]))` flat while `http_requests_total` collapses = requests die before reaching the gateway (edge/WAF/HAProxy). Check `haproxy_frontend_denied_connections_total` and the WAF counters (`waf_block_total`) — see [../../deploy/edge/ddos-playbook.md](../../deploy/edge/ddos-playbook.md).
2. Auth outage can masquerade as collapse: check `exchange_errors_total{tier="L3"}` for a 401/403 spike edge-side.
3. Confirm the venue is in-session: FX trades 24/5 — a drop during the weekend close (Fri 22:00 → Sun 21:00 UTC) is expected; the baseline gate (>5 req/s one hour ago) limits this rule to real collapses.

### marketdata-pipeline

1. `marketdata_deltas_received_total` rising while `marketdata_frames_published_total` is flat → conflator wedged between intake and fan-out.
2. `marketdata_deltas_dropped_total` >0 → input-queue saturation; `marketdata_resync_directives_total` will climb as gaps are announced — clients are already repairing.
3. Check `ws_connections_active` + `ws_subscriptions_active` for a connection storm preceding the stall.

## Mitigation

1. `BridgePublishStalled`: check `nats_connected`/`bridge_health_nats_connected` — a NATS outage fills the bounded buffer (see [bridge-buffer-depth-high.md](./bridge-buffer-depth-high.md)); restart the bridge only after the buffer/spool posture is known.
2. `BridgeEventsDropped`: treat as potential event loss (zero-loss pessimism). Determine the dropped window from `bridge_events_received_total` - `bridge_events_published_total` deltas and plan a consumer re-baseline — affected downstreams rebuild from the next snapshot, not from the gap.
3. Marketdata stall: restart the marketdata process only after checking whether the ws server or the conflator is wedged (`/healthz` reports conn/sub counts); a wedged conflator with live connections means clients hold stale subscriptions silently.

## Escalation

- `BridgeEventsDropped`/`BridgePublishStalled` are P1 (event-path loss risk). Escalate to P0 if the stall coincides with engine-side errors (`aeron_poll_errors_total` rising) — the shard may be down, follow [engine-halt-failover.md](./engine-halt-failover.md).
- `GatewayTrafficCollapse` escalates to P1 if the cause is confirmed internal (gateway wedged while edge sees traffic).
