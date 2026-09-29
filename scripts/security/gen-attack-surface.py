#!/usr/bin/env python3
"""Generate docs/security/attack-surface.md from the live route registry.

Phase-13 Task 13.3.5 (penetration-testing prep). The document is
mechanically derived — routes come from `go run ./services/cmd/route-dump`
(gateway.SeedRoutes() — the same table the gateway mounts and the
/api/v1/routes endpoint serves), WS channels/actions are grepped from
services/internal/marketdata/channels.go and services/internal/ws/server.go,
and the FIX section documents the scaffold state (cmd/fix serves
/metrics+/healthz only; acceptors land in Phase-18).

Run from the repo root:  python3 scripts/security/gen-attack-surface.py
"""
import json
import os
import re
import subprocess
import sys
from datetime import datetime, timezone

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
SVC = os.path.join(ROOT, "services")
OUT = os.path.join(ROOT, "docs", "security", "attack-surface.md")


def dump_routes():
    p = subprocess.run(
        ["go", "run", "./cmd/route-dump"],
        cwd=SVC, capture_output=True, text=True, check=True)
    return json.loads(p.stdout)


def go_map_keys(path, varname):
    """Extract top-level string keys from a `var <varname> = map[string]X{...}` literal."""
    src = open(path, encoding="utf-8").read()
    m = re.search(r"var %s = map\[string\][^{]*\{(.*?)\n\}" % re.escape(varname),
                  src, re.S)
    if not m:
        raise SystemExit(f"could not find map {varname} in {path}")
    return sorted(re.findall(r'"([^"]+)"\s*:', m.group(1)))


def ws_actions():
    """Control actions from internal/ws/server.go dispatch + order.*
    scope map from internal/marketdata/ws_dispatcher.go."""
    src = open(os.path.join(SVC, "internal/ws/server.go"), encoding="utf-8").read()
    acts = re.findall(r'"(authenticate|refresh_token|subscribe|unsubscribe|ping|resume)"', src)
    acts = sorted(set(a for a in acts))
    disp = open(os.path.join(SVC, "internal/marketdata/ws_dispatcher.go"),
                encoding="utf-8").read()
    m = re.search(r"var orderActionScope = map\[string\]string\{(.*?)\n\}",
                  disp, re.S)
    order_actions = dict(re.findall(r'"([^"]+)"\s*:\s*"([^"]+)"',
                                    m.group(1))) if m else {}
    return acts, order_actions


def auth_str(a):
    if not a.get("required"):
        return "public"
    parts = []
    if a.get("role"):
        parts.append(f"role:{a['role']}")
    if a.get("scopes"):
        parts.append("scope:" + "+".join(a["scopes"]))
    if a.get("methods"):
        parts.append("cred:" + "|".join(a["methods"]))
    return "auth" + (" (" + ", ".join(parts) + ")" if parts else "")


def main():
    routes = dump_routes()
    pub_ch = go_map_keys(os.path.join(SVC, "internal/marketdata/channels.go"),
                         "channelTypes")
    priv_ch = go_map_keys(os.path.join(SVC, "internal/ws/server.go"),
                          "PrivateChannels")
    actions, order_actions = ws_actions()

    live = [r for r in routes if r["status"] == "live"]
    stub = [r for r in routes if r["status"] == "stub"]
    ws_routes = [r for r in routes if r["method"] == "WS"]
    dual = [r for r in routes if r.get("dual_control")]
    env_gated = [r for r in routes if r.get("env")]

    ts = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M UTC")

    def row(r):
        return ("| `{m}` | `{p}` | {a} | {t} | {s} | {d} | {e} |".format(
            m=r["method"], p=r["path"], a=auth_str(r.get("auth", {})),
            t=r["rate_tier"], s=r["status"],
            d="yes" if r.get("dual_control") else "",
            e=r.get("env", "")))

    out = []
    out.append("# Attack Surface Map — Penetration Test Preparation")
    out.append("")
    out.append(f"**Generated:** {ts} by `scripts/security/gen-attack-surface.py` "
               "(Task 13.3.5). Regenerate before each test window — the table is "
               "derived from `gateway.SeedRoutes()` (`services/internal/gateway/"
               "routes_v1.go`), the same table the gateway mounts and "
               "`GET /api/v1/routes` serves. Do not hand-edit; update the "
               "registry or this generator.")
    out.append("")
    out.append("Scope/ROE live in [pentest-scope.md](./pentest-scope.md); "
               "fixtures in `tests/pentest/`.")
    out.append("")
    out.append("## 1. Summary")
    out.append("")
    out.append(f"- **{len(routes)} registered routes**: {len(live)} live, "
               f"{len(stub)} stub (501-until-implemented — still routable "
               "surface: they consume auth/rate-limit middleware and are "
               "part of the attack surface)")
    out.append(f"- **{len(ws_routes)} WebSocket endpoints**, "
               f"{len(pub_ch)} public channel types, {len(priv_ch)} private "
               f"channels, {len(actions) + len(order_actions)} control/order actions")
    out.append(f"- **{len(dual)} dual-control routes** (four-eyes operations), "
               f"**{len(env_gated)} env-gated routes** (nonprod/env-scoped only)")
    out.append("- FIX surface: scaffold only — see §4")
    out.append("")
    out.append("## 2. HTTP/REST routes (full registry)")
    out.append("")
    out.append("Columns: auth = `public` (no credentials) or required "
               "credential/scope/role; tier = §8.8 rate-limit bucket; "
               "status = live|stub; dual = dual-control required; env = "
               "env-gated (`nonprod` = never mounted in prod).")
    out.append("")
    out.append("| Method | Path | Auth | Rate tier | Status | Dual | Env |")
    out.append("|---|---|---|---|---|---|---|")
    for r in routes:
        out.append(row(r))
    out.append("")
    out.append("## 3. WebSocket surface")
    out.append("")
    out.append("### 3.1 Upgrade endpoints (Method=WS in the registry)")
    out.append("")
    out.append("| Path | Auth | Notes |")
    out.append("|---|---|---|")
    for r in ws_routes:
        note = r.get("description", "")
        out.append(f"| `{r['path']}` | {auth_str(r.get('auth', {}))} | {note} |")
    out.append("")
    out.append("The marketdata binary (`cmd/marketdata`, :8081) mounts "
               "`/ws/v1/marketdata` and `/ws/v1/orders` directly; gateway-side "
               "WS routes proxy the same machinery.")
    out.append("")
    out.append("### 3.2 Control actions (client→server frames)")
    out.append("")
    out.append("Session/control actions (`internal/ws/server.go` dispatch): " +
               ", ".join(f"`{a}`" for a in actions) + ".")
    out.append("")
    out.append("Interactive order actions (`internal/marketdata/"
               "ws_dispatcher.go` `orderActionScope` — the §8.8 scope each "
               "action requires):")
    out.append("")
    out.append("| Action | Required scope |")
    out.append("|---|---|")
    for a, s in sorted(order_actions.items()):
        out.append(f"| `{a}` | {s} |")
    out.append("")
    out.append("- `authenticate` accepts JWT or `ak_*` API key (+optional "
               "HMAC signature); `protocol_version` checked "
               "(UNSUPPORTED_PROTOCOL_VERSION).")
    out.append("- `order.*` actions are private (authenticated) order-entry "
               "over WS — `order.test` is the conformance probe.")
    out.append("- Abuse guards: subscription-churn guard "
               "(`WS_ABUSE_DETECTED` → close), per-conn subscription cap "
               "(`WS_MAX_SUBSCRIPTIONS_EXCEEDED`), channel-name charset "
               "allowlist, `resume` sequence-window replay.")
    out.append("")
    out.append("### 3.3 Public channels (`internal/marketdata/channels.go` "
               "`channelTypes`)")
    out.append("")
    out.append(", ".join(f"`{c}`" for c in pub_ch) + ".")
    out.append("")
    out.append("### 3.4 Private channels (`internal/ws/server.go` "
               "`PrivateChannels` — authenticated, account-scoped)")
    out.append("")
    out.append(", ".join(f"`{c}`" for c in priv_ch) + ".")
    out.append("")
    out.append("## 4. FIX surface (Phase-18 — scaffold today)")
    out.append("")
    out.append("`cmd/fix` currently binds only :8082 for `/metrics` + "
               "`/healthz` — **no FIX session acceptor is live**. The Phase-18 "
               "acceptor surface, when it lands, will be: FIX 4.4 + FIX 5.0 "
               "SP2 sessions (initiator + acceptor, `fix_sessions` table "
               "migration 030), order entry (NewOrderSingle 35=D, "
               "OrderCancelRequest 35=F, OrderCancelReplaceRequest 35=G), "
               "execution reports (35=8), market data (35=W/X), Mass Quoting "
               "(35=i), PB drop copy / Traiana affirmation, allocations "
               "(35=J/35=AK), mTLS client certification (Task 18.3.11), and an "
               "SBE binary gateway (Task 18.3.8). Retest this section when "
               "Phase-18 lands — none of it is reachable today.")
    out.append("")
    out.append("## 5. Non-route surface")
    out.append("")
    out.append("| Surface | Bind | Notes |")
    out.append("|---|---|---|")
    out.append("| `/metrics`, `/healthz` | per-service (8080-8085, bridge 9100+N) | Prometheus exposition + health; not client-facing but reachable on pod network |")
    out.append("| `GET /api/v1/routes`, `/api/v1/errors`, `/api/v1/openapi.json` | gateway :8080 | registry dumps — `openapi.json` is public, routes/errors are Super-Admin |")
    out.append("| Admin DLQ (`/api/v1/admin/dlq*`) | admin :8085 | dead-letter inspect/replay/discard |")
    out.append("| Aeron IPC media driver | host-local (`/dev/shm/aeron-*`, EXC_AERON_DIR) | CnC file + IPC rings — not network-exposed; on-host integrity is an OS problem |")
    out.append("| NATS JetStream | nats cluster (4222/8222) | event backbone + `ops.*` alert/DLQ subjects; mTLS per deploy/nats |")
    out.append("| Postgres / Redis / ClickHouse | 5433/16379+/8123 dev binds | infra-tier; pen-test in scope only via lateral movement |")
    out.append("")
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    open(OUT, "w").write("\n".join(out))
    print(f"wrote {OUT}: {len(routes)} routes "
          f"({len(live)} live/{len(stub)} stub), "
          f"{len(ws_routes)} WS endpoints, {len(actions)} WS actions")


if __name__ == "__main__":
    sys.exit(main())
