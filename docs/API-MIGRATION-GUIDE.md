# API Deprecation & Migration Guide

**Owner:** Order Gateway (Go) · **Phase:** Phase-05 Task 5.3.20 (policy + machinery) · Phase-09 Task 9.3.6 (operations) · **Authority:** spec §8.6, §24 #91 · **Live copy:** `GET /developer/migration` renders this policy plus the *live* rule table — it is always the freshest view; this document is the durable published version.

This guide tells API consumers how endpoint deprecation works on the venue, how to detect it, and how to migrate before a sunset date. It covers REST, WebSocket, FIX, and SBE surfaces.

---

## 1. Deprecation policy

| Rule | Contract |
|---|---|
| Notice window | **Minimum 6 months** between announcement (`announced_at`) and removal (`sunset_at`). Enforced twice: a PostgreSQL `CHECK (sunset_at >= announced_at + interval '6 months')` on `api_deprecations` (migration 182) and a service-side floor `deprecation.MinNotice` (183 days) that rejects short notices before insert. No rule can encode a shorter notice. |
| Major versions | When `/api/v2/` is introduced, `/api/v1/` continues serving for **12 months** (double the endpoint window, spec §8.6). Both majors run in parallel for the whole window. |
| Field-level deprecation | Within a major version no fields are removed, no required fields are added, and no enum values are renamed. A field scheduled for removal is marked with the `X-Deprecated-Field` response header (and the OpenAPI schema) and sunsets after 6 months. |
| Sunset behaviour | Once `sunset_at` passes, the endpoint deterministically answers **`410 Gone`** with an RFC 7807 problem body — `error: "ENDPOINT_GONE"`, `status: 410`, and a message naming the sunset instant and the migration-guide URL. The response is emitted by the deprecation middleware before the handler runs, so it is identical everywhere. |
| Lifecycle | Rules move `ANNOUNCED → SUNSET`. A 60-second sweep flips `api_deprecations.status` to `SUNSET` once `sunset_at` has passed (migration 196), sets `sunset_processed_at`, and increments the `deprecation_sunset_total` metric. Enforcement is time-based — a sweep lag never extends a sunset. |
| Scope of a rule | A rule may pin one HTTP `method` or cover **all methods** on a path (`method` NULL). `match_prefix` rules cover a whole subtree (e.g. a retired namespace). When several rules could match, the most specific wins: exact method+path > any-method exact path > longest prefix. |
| Fail-closed cache | The gateway caches rules with a 5-second TTL. If the rules store becomes unreadable, the *last good* rule set keeps applying — a DB blip can never silently un-deprecate or un-sunset an endpoint. |

### Response headers during the notice window

Every response from an announced endpoint carries three signals (RFC 8594 / draft-ietf-httpapi-deprecation-header):

```http
Deprecation: @1759305600        # unix timestamp of the announcement
Sunset: Wed, 01 Apr 2027 00:00:00 GMT   # HTTP-date removal instant
Link: </developer/migration>; rel="deprecation"
```

- `Deprecation` — the announcement instant (unix seconds, `@`-prefixed).
- `Sunset` — the exact removal instant as an HTTP-date. Treat this as a hard deadline: the endpoint is gone at that instant, not "sometime after".
- `Link: rel="deprecation"` — pointer to the live migration guide.

### API versioning headers (all responses, spec §8.6)

| Header | Meaning |
|---|---|
| `X-API-Version` | Semantic build version of the API serving the request (e.g. `1.4.0`). |
| `X-Deprecated-Field` | Names a request/response field scheduled for removal within the same major version. |
| `UNSUPPORTED_PROTOCOL_VERSION` | Error code (400) returned for an unknown path major (`/api/v3/...` before v3 exists) and for an unknown WS `protocol_version`. |

---

## 2. Currently announced deprecations

**None.** The `api_deprecations` rule table contains no announced or sunset endpoints at this revision — no endpoint currently emits `Deprecation`/`Sunset` headers, and none has been removed under this policy. The authoritative live list is always at `GET /developer/migration` (admin view: `GET /api/v1/admin/api-deprecations`, auditor+; usage telemetry: `GET /api/v1/admin/api-deprecations/usage`).

### Known superseded / legacy surfaces (not formal deprecations)

These routes are live or reserved but have a canonical successor. They carry **no `Sunset`** today; if a formal deprecation is announced they will appear in the live table with the standard 6-month window.

| Legacy surface | Canonical successor | State | Migration note |
|---|---|---|---|
| `GET /api/v1/tax/report` | `GET /api/v1/account/tax-report` | Both live | Same producer and parameters (`?year=&method=&format=`). New integrations should call the canonical `/api/v1/account/tax-report` path (route registry, Phase-05 Task 5.3.19/Phase-20 Task 20.3.10). |
| `WS /ws/market` | `WS /ws/v1` | Registered stub | Reserved alias surface; returns an `ENDPOINT_GONE`-style response per the Task 5.3.20 contract (spec §27, remediation #44). Connect to `/ws/v1` and subscribe to market streams. |
| `WS /ws/trade` | `WS /ws/v1` | Registered stub | Same as above — interactive trading traffic belongs on `/ws/v1`. |
| `WS /ws/v1/marketdata`, `WS /ws/v1/orders` | `WS /ws/v1` | Registered stubs | Split-path aliases of the unified `/ws/v1` socket (market data + private feeds + `order.*` request-response multiplex on one connection). |

### Non-REST channels (versioning, not deprecation)

| Channel | Mechanism | Deprecation lifecycle |
|---|---|---|
| WebSocket | `protocol_version` in the auth frame (`{"action":"authenticate","protocol_version":1}`) | A protocol major bump follows the same notice contract; unknown versions are rejected `UNSUPPORTED_PROTOCOL_VERSION`. |
| FIX | `BeginString` tag (FIX.4.4 vs FIXT.1.1/FIX50SP2) — inherent per-session versioning | Dictionary retirements are announced through the same 6-month notice convention out of band. |
| SBE | Schema ID + schema version in the message header | Machine-readable six-month deprecated→retired lifecycle (`internal/sbe` — `deprecated` flag, `sunset` instant, `warning` string on negotiation; spec §24 #284). |

---

## 3. How to migrate

1. **Detect.** Alert on `Deprecation` / `Sunset` response headers and on `X-Deprecated-Field`. Any client that logs response headers can wire a three-line check; the headers appear on *every* response from an announced endpoint, not once per session.
2. **Identify the replacement.** Follow the `Link: rel="deprecation"` header to `/developer/migration` — the live table lists each rule's `replacement` route and free-text `notice`. The current request/response contract is always `GET /api/v1/openapi.json`; the full registered surface is `GET /api/v1/routes` (admin).
3. **Switch before `Sunset`.** Cut over to the replacement while the old endpoint still serves. A field-level migration only requires tolerating the renamed/absent field after its `X-Deprecated-Field` sunset.
4. **Verify.** After cutover, confirm no production traffic still hits the deprecated route — the venue tracks this server-side in `api_deprecation_hits` (visible to operators), but clients should verify against their own egress logs.
5. **Past-sunset behaviour.** Requests to a sunset endpoint fail fast with `410 ENDPOINT_GONE` and a message pointing at the migration guide — there is no grace window, and the response is never routed to a handler.

### Version-major migration

When a new `/api/v{n}/` major ships: both majors serve in parallel for 12 months; migrate by rebasing your integration on the new path prefix and re-running your contract tests against `GET /api/v{n}/openapi.json`. Unknown majors are rejected `400 UNSUPPORTED_PROTOCOL_VERSION` with a `details.supported` list of live majors.

### If no replacement is listed

Every rule should name a `replacement`. If the live table shows none, or the replacement does not cover your use case, open a support ticket (`POST /api/v1/support/tickets`) referencing the deprecated path and the `Sunset` date — do not wait for the sunset to discover a gap.

---

## 4. Operator reference (venue-side)

| Surface | Purpose |
|---|---|
| `POST /api/v1/admin/api-deprecations` | Announce a rule (`Super Admin`). Body: `{method?, path, match_prefix?, announced_at?, sunset_at, replacement?, notice?}` — `sunset_at` must satisfy the 6-month floor or the insert is refused. |
| `GET /api/v1/admin/api-deprecations` | List rules, newest first (`Read-Only Auditor`+). |
| `GET /api/v1/admin/api-deprecations/usage` | Per-rule daily hit telemetry (`hits` = announced-window use, `gone_hits` = post-sunset attempts), compacted from Redis `deprecation:hits:{id}:{yyyymmdd}` into `api_deprecation_hits`. |
| `GET /developer/migration` | Public live guide — this policy plus the current rule table. |
| Enforcement | `internal/deprecation` middleware wrapped around the mux in `cmd/gateway`; emits headers in-window and `410 ENDPOINT_GONE` post-sunset. Rule lookup is per-request over a 5s cached set — a new rule takes effect within one cache TTL. |

*Registry references: migrations 182 (rule store + 6-month CHECK) and 196 (lifecycle status + usage rollup); error code `ENDPOINT_GONE` → 410 is registered in spec §23.*
