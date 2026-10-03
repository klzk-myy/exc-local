// frontend.edge.js — njs module for the docker frontend image.
//
// This image is dev-stack-only (docker-compose.app.yml stage 5 — no
// prod manifest references it), and the dev requirement is that LAN
// clients are not edge-rate-limited. The gateway keys §8.8 buckets on
// client IP (EXC_TRUST_PROXY=1), so a single proxy upstream IP would
// land every SPA call in one bucket and the public tier would 429
// ordinary page-mount bursts. Path-hash bucketing (the Vite devEdgeIp
// model) still collapses concurrent widgets hitting the same endpoint
// onto one bucket, so this image goes one step further: each REQUEST
// gets its own 10.90.x bucket, seeded from nginx's per-request
// $request_id — the closest faithful model of "no edge rate limit for
// LAN dev traffic" while keeping the synthetic-IP machinery (10.90.x is
// what `seeddev -allowlist-ips` exempts from ban escalation).

function xff(r) {
  const id = r.variables.request_id || r.uri;
  let h = 0x811c9dc5; // FNV-1a 32-bit
  for (let i = 0; i < id.length; i++) {
    h = Math.imul(h ^ id.charCodeAt(i), 0x01000193) >>> 0;
  }
  return `10.90.${(h >>> 8) & 0xff}.${h & 0xff}`;
}

// WS upgrades get a per-connection bucket instead of one fixed IP:
// the gateway bounds upgrades at ReconnectRate=10 per 60s per IP, and a
// shared proxy IP would 429 a SPA that opens a socket per page mount.
// Bucketing on the client's ephemeral source port gives each TCP
// connection its own budget while keeping the synthetic-IP model.
function wsxff(r) {
  return `10.90.255.${Number(r.variables.remote_port) % 256}`;
}

export default { xff, wsxff };
