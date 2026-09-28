// Package ws implements the unified interactive WebSocket surface
// (GET /ws/v1, plus the legacy aliases /ws/v1/marketdata and
// /ws/v1/orders) for Phase-05 Wave-2 cluster E:
//
//   - Task 5.3.26 — in-band authentication upgrade ("authenticate" frame
//     accepting a JWT or an "ak_"-prefixed API key + signature), in-flight
//     JWT renewal via "refresh_token", private-channel authorization, and
//     AUTH_EXPIRED teardown with registered close code 4019 (spec §10.5,
//     §24 #187).
//   - Task 5.3.31 — interactive trading request/response protocol:
//     order.* actions with client-supplied request_id correlation,
//     ACK/NACK response frames, canonical error envelopes, and a 60s
//     request_id dedup window (spec §10.5 item 6, §24 #253).
//   - Task 5.3.42 item 4 — per-IP connection caps (published via
//     GET /api/v1/meta/rate-limits) and the WS-side dedup contract.
//
// Canonical contracts consumed here:
//
//   - Discriminator is "action" (remediation #9; the "op" form is dead).
//   - Error frames: {"type":"error","request_id":..,"action":..,
//     "error":"<§23 code>","message":"..","ts_ms":..,"retry_after_ms":..}
//     — WS durations are milliseconds with the _ms suffix (§10.5 item 5).
//   - Response frames: {"type":"response","request_id":..,"action":..,
//     "status":"ACK"|"NACK","data":{...},"ts_ms":..}.
//   - Private channels: private:orders | private:executions |
//     private:positions | private:balances — require authentication.
//   - protocol_version is mandatory on authenticate; unknown versions are
//     rejected UNSUPPORTED_PROTOCOL_VERSION (§8.6, Task 5.3.28).
//   - Subscription cap 200 (WS_MAX_SUBSCRIPTIONS_EXCEEDED); control-rate
//     breach emits WS_RATE_EXCEEDED + retry_after_ms and a rate_info
//     feedback frame, escalating to a forced 4029 disconnect after 3
//     warnings in 60s; churn abuse terminates with close code 4003 +
//     WS_ABUSE_DETECTED; slow consumers are dropped with 4008 when the
//     1024-message outbound buffer fails to drain within 2s (producer
//     block) or stays saturated for ~2s of queue-monitor ticks.
//   - Disconnect classification (Task 6.3.7 item 6): close reasons carry
//     the disconnect_reason discriminator CLIENT_DISCONNECT /
//     NETWORK_TIMEOUT / ABUSE_DISCONNECT (server-initiated teardowns —
//     drain, AUTH_EXPIRED — classify SERVER_SHUTDOWN internally); the
//     OnDisconnect hook feeds the dead-man/CoD machinery which must NOT
//     mass-cancel on ABUSE_DISCONNECT.
//   - Drain (Task 6.3.19): Server.Drain emits a server.shutdown advisory
//     (reason, retry_after_ms, endpoint(s), deadline_ms), refuses new
//     upgrades (503 MAINTENANCE_MODE) and new in-band work while keeping
//     cancels/session upkeep available, then closes survivors with 1001
//     CONNECTION_DRAINING inside DrainDeadline.
//   - Feed failover seam (Task 6.3.21): Config.Failover endpoints +
//     Server.SetFeedState broadcast feed.failover advisories; the
//     multicast A/B arbitration itself is marketdata's.
//   - Reconnect flood (Task 6.3.21): ≤ReconnectRate upgrades per
//     ReconnectWindow per IP on the accept path → HTTP 429
//     RATE_LIMIT_TIER_EXCEEDED.
//
// Boundaries (owners elsewhere, consumed through narrow seams):
//
//   - Channel payload schemas, per-symbol sequence ring buffers and
//     replay/snapshot resync are Phase-06 (Tasks 6.3.x). This package
//     numbers each (conn, channel) event stream monotonically ("seq") so
//     Phase-06's resume protocol can bind to it; "resume" currently
//     answers with a resync directive — honest fail-closed until the ring
//     buffer lands.
//   - Per-channel-type subscription splits (§24 #84: 20 L2 / 5 L3) and
//     §10.7 symbol entitlements are marketdata's — admitted via the
//     SubAdmit hook; this package owns only the generic 200-conn cap.
//   - Order execution itself is the order pipeline's (Tasks 5.3.24/5.3.25,
//     Phase-02 core). ws.Dispatcher is the narrow seam; an unwired
//     dispatcher fails closed with NOT_IMPLEMENTED.
//   - order.countdown_cancel_all is routed to the Task 5.3.33 dead-man
//     service through ws.CountdownController so the timer stays shared
//     across REST/WS/FIX per spec §24 #257.
//   - Cancel-on-disconnect (Phase-05 Task 5.3.25) consumes OnDisconnect;
//     the ≤1 mass-cancel per account per 5s storm throttle is enforced
//     there, not here.
package ws
