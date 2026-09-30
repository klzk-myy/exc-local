# HAProxy edge — Task 5.3.29 (API Gateway / Load Balancer)

`haproxy.cfg` is the L7 edge for the order-gateway (`:8080`). It
terminates TLS, routes REST vs WebSocket, gates traffic on readiness,
and provides blue/green deployment and drain support.

## Layout

| File | Purpose |
|------|---------|
| `haproxy.cfg` | frontends, backends, checks, timeouts, logging |
| `active_color.map` | blue/green selector (runtime-editable) |
| `VALIDATION.md` + `test/` | Task 5.3.29 DoD drill: real haproxy:2.9 run, measured failover/flip evidence |

## Ops cheatsheet (all via the admin socket `/run/haproxy/admin.sock`)

```sh
# Flip traffic to green (no reload)
echo "set map /etc/haproxy/active_color.map gateway green" | socat /run/haproxy/admin.sock -

# Drain a server before deploy (existing WS tunnels keep running until
# timeout tunnel 4h or client disconnect)
echo "set server be_gateway_blue/gw-blue-1 state maint" | socat /run/haproxy/admin.sock -

# Hot certificate rotation without full reload
echo "new ssl cert /etc/haproxy/certs/new.pem" | socat /run/haproxy/admin.sock -
echo "commit ssl cert /etc/haproxy/certs/new.pem" | socat /run/haproxy/admin.sock -
```

## Contract with the Go gateway

- HAProxy emits **no** security headers or CORS — the gateway
  (`services/internal/middleware/security.go`) is the single emitter.
- `X-Forwarded-For` is set here; the gateway and WS server trust it —
  the gateway listener MUST NOT be reachable except via this edge.
- Coarse bounds here: 1 MiB REST body (`413`), 30s client/server
  timeouts, 4h WS tunnel, 10s `http-request` slowloris bound.
  The 64 KiB WS frame cap lives in the gateway (`SetReadLimit`) —
  HAProxy does not re-inspect tunneled frames.
- Readiness (`/health/ready`) gates traffic at 2s intervals;
  `/health/live` is the orchestrator probe only.
- The Go service's own breaker (15% error / 10s window / 5s probe) is
  the finer inner layer; HAProxy marks a node out only on readiness
  failure or TCP refusal.
