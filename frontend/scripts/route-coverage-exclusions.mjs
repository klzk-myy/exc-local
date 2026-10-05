/**
 * route-coverage-exclusions.mjs — Task 10.5.3.27 registries.
 *
 * EXCLUSIONS — mounted routes deliberately without a UI surface.
 *   Keys are `METHOD path`; `*` wildcards the method; a trailing `/*`
 *   globs the whole sub-tree, an interior `*` one segment. Every entry
 *   needs a reason comment. Stale entries (route gone or now
 *   referenced) fail the gate — keep the list honest.
 *
 * DISPATCHES — source URL templates whose `${…}` segment is a
 *   typed-union verb or resource selector emitted from one call site.
 *   The checker asserts each template exists verbatim in src and marks
 *   every mounted route of the same shape (each `${…}` covering exactly
 *   one segment) as covered.
 */
export const EXCLUSIONS = {
  // --- OPS: gateway probes + docs portal -----------------------------
  'GET /health': 'OPS — liveness alias (admin-monitoring convention)',
  'GET /health/live': 'OPS — liveness probe',
  'GET /health/ready': 'OPS — readiness probe (PG+Redis+NATS)',
  'GET /ready': 'OPS — readiness alias',

  'GET /developer/migration': 'DOCS — API deprecation migration guide page',
  'GET /api/v1/openapi.json': 'M2M — machine-readable contract document',
  'GET /api/v1/routes': 'ADMIN-API — super-admin route-registry introspection JSON',
  'GET /api/v1/errors': 'ADMIN-API — super-admin error-code registry introspection JSON',

  // --- TEST: e2e/seeding scaffolding ----------------------------------
  '* /api/v1/test/*': 'TEST — test-only seed/reset/funding scaffolding',

  // --- WS: streaming handshakes ---------------------------------------
  'GET /ws/market': 'WS-ALIAS — legacy market socket; console multiplexes /ws/v1',
  'GET /ws/trade': 'WS-ALIAS — legacy trading socket (registry marks it alias of /ws/v1)',
  'GET /ws/stream': 'WS-ALIAS — combined-stream socket for machine consumers',
  'GET /ws/v1/marketdata': 'WS-ALIAS — registry-declared legacy alias of /ws/v1',
  'GET /ws/v1/orders': 'WS-ALIAS — registry-declared legacy alias of /ws/v1',
  'GET /ws/v1/l3/{symbol}':
    'M2M — professional-tier L3 stream; console uses REST /market-data/l3-snapshot',

  // --- M2M REST read seams (console streams via /ws/v1) ---------------
  'GET /api/v1/ticker/{symbol}': 'M2M — REST poll seam for SDK clients; console uses WS channels',
  'GET /api/v1/trades/{symbol}': 'M2M — public tape REST seam; console uses aggTrades@ WS',
  'GET /api/v1/market-data/snapshot':
    'SEAM — WS-gap recovery fallback; console seeds/resyncs via /book/{symbol} (§10.3)',
  'GET /api/v1/market/open-interest':
    'ALIAS — query-param spelling of /analytics/open-interest/{symbol} (same handler)',

  // --- ALIAS: legacy spellings of covered surfaces --------------------
  'GET /api/v1/account/api-keys':
    'ALIAS — shares developer key-store; console spells /developer/api-keys',
  'POST /api/v1/account/api-keys': 'ALIAS — same as above',
  'DELETE /api/v1/account/api-keys/{id}': 'ALIAS — same as above',
  'GET /api/v1/venue/info': 'ALIAS — registry-declared alias of /api/v1/exchange-info',
};

/**
 * Typed-union dispatch templates verified to exist verbatim in src.
 * Each `${…}` position covers exactly one contract path segment.
 */
export const DISPATCHES = [
  // admin-surveillance/api.ts — action ∈ review|approve|file|reject
  '/admin/sar/${id}/${action}',
  // admin-venue/api.ts — member lifecycle + rulebook + CCO report verbs
  '/admin/venue/members/${id}/${action}',
  '/admin/venue/rulebooks/${id}/${action}',
  '/admin/venue/cco-reports/${id}/${action}',
  // admin-settlement/api.ts — CLS verb union + allocation leg verbs
  '/admin/settlement/cls/instructions/${ref}/${verb}',
  '/admin/allocations/${allocId}/${verb}',
  // admin-conduct/api.ts — FXGC assessment lifecycle
  '/admin/fx-global-code/assessments/${id}/${action}',
  // admin/api.ts — instrument lifecycle ops
  '/admin/instruments/${id}/${op}',
  // admin-finance/api.ts — tax run workflow verbs
  '/admin/tax-reporting/runs/${id}/${verb}',
  // admin-content/api.ts — promotion lifecycle + template decision ternary
  '/admin/promotions/${id}/${verb}',
  '/admin/strategy-templates/${id}/${approve}',
  // admin-marketmaking/api.ts — MM program verbs
  '/admin/mm-programs/${id}/${verb}',
  // lib/admin/api.ts — fleet host lifecycle + dual-control decision
  '/admin/fleet/hosts/${hostId}/${action}',
  '/admin/dual-control/${id}/${approve}',
  // admin-funding/api.ts — approve|reject ternary
  '/admin/withdrawals/${id}/${approve}',
  // admin-crm/api.ts — freeze|unfreeze ternary
  '/admin/accounts/${accountId}/${freeze}',
  // admin-regreport/api.ts — RTS27/28 publish selector
  '/admin/bestexec/${kind}/${id}/publish',
  // history/api.ts — composite order type union (twap|vwap|…)
  '/orders/${type}',
  // transparency/api.ts — rts27|rts28 kind selector
  '/venue/best-execution/${kind}',
  '/venue/best-execution/${kind}/${id}',
  '/venue/best-execution/${kind}/${id}/csv',
  // admin-compliance/api.ts — approve|reject|revoke decision union
  '/admin/swap-free/${id}/${decision}',
  // funding/api.ts — whitelist enable|disable ternary
  '/funding/withdrawal-whitelist/${enable}',
];
