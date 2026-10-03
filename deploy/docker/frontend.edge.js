// frontend.edge.js — njs module for the docker frontend image.
//
// This image is dev-stack-only (docker-compose.app.yml stage 5 — no
// prod manifest references it), and the dev contract is that LAN
// clients are not edge-rate-limited. The gateway keys §8.8 buckets on
// client IP (EXC_TRUST_PROXY=1), so a single proxy upstream IP would
// land every SPA call in one bucket and 429 ordinary page-mount bursts.
// The shared dev-proxy policy (same model the Vite dev server uses in
// vite.config.ts) is therefore: every request gets its OWN synthetic
// 10.90.x bucket, seeded from connection metadata nginx already has —
// no RNG and no hash needed. 10.90.x is also what `seeddev
// -allowlist-ips` exempts from ban escalation.

// /api — (source port, request-number-on-connection) is unique per
// request on each TCP connection; browsers keep ~6 sockets so parallel
// widget bursts on the same endpoint land in distinct buckets.
function xff(r) {
  const port = Number(r.variables.remote_port) % 256;
  const req = Number(r.variables.connection_requests) % 256;
  return `10.90.${port}.${req}`;
}

// /ws — the gateway bounds upgrades at ReconnectRate=10 per 60s per IP;
// each WebSocket opens on its own TCP connection, so the source port
// alone gives every upgrade its own budget.
function wsxff(r) {
  return `10.90.255.${Number(r.variables.remote_port) % 256}`;
}

export default { xff, wsxff };
