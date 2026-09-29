# Trader UI — `frontend/`

Phase-10 deliverable: React 18 + TypeScript (`strict: true`) + Vite SPA for the
fiat FOREX exchange (spec §21). **en-US only — no i18n framework, no locale
tables** (spec §27 R5). FX market hours are 24/5 (Sun 21:00 UTC → Fri 22:00
UTC, spec §6.3).

## Quick start

```bash
npm install
npm run dev          # Vite dev server on :5173 (proxies /api + /ws to :8080)
npm run build        # tsc --noEmit → vite build → bundle-budget gate
npm run test         # Vitest + React Testing Library (jsdom)
npm run typecheck    # tsc --noEmit
npm run lint         # ESLint (strictTypeChecked)
npm run format:check # Prettier
npm run test:e2e     # Playwright (scaffold — Wave-2 adds the smoke path)
npm run size:check   # standalone bundle-budget check against dist/
```

## Feature auto-discovery (manifest contract)

**Parallel agents add features without touching shared files.** Routes and
nav are discovered at build time via `import.meta.glob` — drop a directory
under `src/features/<name>/` exporting the conventions below and it is live:

### `src/features/<name>/routes.ts` (required to route)

```ts
import { lazy } from 'react';
import type { FeatureRoute } from '@/app/manifest';

const TradePage = lazy(() => import('./TradePage'));

export const routes: FeatureRoute[] = [
  { path: 'trade/:symbol', element: TradePage, title: 'Trade' },
];
```

| Field      | Type              | Notes                                                                                                          |
| ---------- | ----------------- | -------------------------------------------------------------------------------------------------------------- |
| `path`     | `string?`         | React Router path (relative to the shell route).                                                               |
| `index`    | `boolean?`        | Index route for `/` — mutually exclusive with `path`.                                                          |
| `element`  | `ElementType`     | **MUST be `React.lazy(() => import('./Page'))`** — per-route code splitting keeps initial JS ≤ 300 kB gzipped. |
| `title`    | `string`          | Sets `document.title` (`"<title> — Exchange"`).                                                                |
| `children` | `FeatureRoute[]?` | Nested routes render inside this route's `<Outlet/>`.                                                          |

### `src/features/<name>/nav.ts` (optional)

```ts
import type { FeatureNavItem } from '@/app/manifest';

export const nav: FeatureNavItem[] = [
  { label: 'Trade', to: '/trade/EUR/USD', section: 'Trade', order: 10 },
];
```

| Field     | Type      | Notes                                                          |
| --------- | --------- | -------------------------------------------------------------- |
| `label`   | `string`  | Nav label (en-US).                                             |
| `to`      | `string`  | Absolute route path the link navigates to.                     |
| `icon`    | `string?` | Icon token — resolved by the shell.                            |
| `section` | `string`  | Nav grouping in the header (e.g. `Trade`, `Account`, `Admin`). |
| `order`   | `number?` | Sort key within the section (ascending).                       |

Discovery is implemented in `src/app/manifest.ts` and consumed by
`src/app/router.tsx` + `src/components/AppShell.tsx`. Both manifest files may
alternatively `export default <array>`. Working example: `src/features/home/`.

**Rules for feature authors (Wave-2+):**

- Never edit `src/app/*`, `src/components/AppShell.tsx`, or another feature's
  files to wire a feature — the glob does it.
- Keep heavy deps (TradingView Lightweight Charts, backtester) behind
  `React.lazy` — they must not enter the entry chunk's static-import closure
  (bundle gate fails the build at >300 kB gzipped).
- Server state goes through TanStack Query + `apiClient` (`@/app/runtime`);
  local UI state uses Zustand; stream state uses `wsClient` (`useWsStatus`,
  `useChannel`).

## WS client (`src/lib/ws/`)

Implements the **normative state machine of Phase-10 Task 10.3.19**
(spec §2.7, §10.6, §21.3, §24 #310):

```
CONNECTING → AUTHENTICATED ↔ STALE → DISCONNECTED → RECONNECTING → CONNECTING
                ↑________________ RESYNCING ________________|
```

| State           | Order entry           | Surface                                                                |
| --------------- | --------------------- | ---------------------------------------------------------------------- |
| `CONNECTING`    | locked                | dial + auth handshake                                                  |
| `AUTHENTICATED` | enabled               | live, per-channel `last_seq` tracking                                  |
| `STALE`         | enabled + amber badge | no md tick >3.0s in trading hours (24/5)                               |
| `DISCONNECTED`  | locked + banner       | socket closed                                                          |
| `RECONNECTING`  | locked + banner       | waiting on backoff                                                     |
| `RESYNCING`     | locked                | ring-buffer replay / snapshot until every subscribed channel converges |

- **Reconnect schedule** (canonical, spec §21.3 — shared with Phase-06;
  change it in both places): `RECONNECT_SCHEDULE_MS = [100, 250, 500, 1000,
2000, 10000]` ms + ±20% jitter. Resets only on a successful handshake —
  never on stream resume, never on `server.shutdown` advisories.
- **`last_seq` resume**: per-channel cursors are retained across the whole
  cycle; reconnect sends `{"action":"resume","channel":…,"last_seq":N}` and
  converges on `resumed` / `snapshot` / `subscribed` frames (spec §10.7,
  `services/internal/marketdata/ws_session_resume.go`).
- **Subscription grammar**: `{"action":"subscribe","params":["bbo@EUR/USD"]}`
  (`unsubscribe` same shape; `params` array per `internal/ws` `channelList`).
- **Gap/resync**: an envelope `seq` discontinuity drops the frame
  (fail-closed, §10.9), sends `{"action":"resync",…}`, and fires `onGap`;
  a `resync` directive without a following `snapshot` fires
  `onResyncRequired` (consumer refetches via REST, e.g.
  `GET /api/v1/book/{symbol}?depth=20`).
- **Close codes**: `4019 AUTH_EXPIRED` → token refresh → `CONNECTING`
  (refresh failure flushes all optimistic order state → `onAuthFailure` →
  `/login`); `4003` abuse / `4008` slow-consumer drop reconnect through the
  normal backoff.
- **`server.shutdown` advisory**: transitions to `RECONNECTING` honoring the
  advisory's `endpoint`/`retry_after_ms` **without** touching the attempt
  counter — scheduled maintenance can't bypass backoff.
- **Stale data**: `getStatus().health[channel]` exposes `lastTickAt`,
  `lastSeq`, `stale`, `resyncing` per subscription; `STALE` only during FX
  trading hours (`src/lib/market/tradingHours.ts`).
- **Invariant**: optimistic order state never survives a full
  re-authentication — `onOptimisticFlush` fires before every authenticate
  after the first.

`WsClient` is framework-agnostic and fully port-injected (`socketFactory`,
`clock`, `rng`, `tokenProvider`, `tokenRefresher`); `src/lib/ws/testkit.ts`
ships `FakeClock` + `MockSocket` + `SocketFactory` for feature tests.

## API client (`src/lib/api/`)

- `ApiClient` — fetch wrapper: `Authorization: Bearer` via `getAuthToken`,
  query serialization, JSON bodies, `credentials: 'same-origin'`.
- RFC 7807 error envelope (spec §8.7): `{"type":"error","error":"<CODE>",…}`
  → `ApiError { code, message, status, requestId, retryAfterMs }`; transport
  failures → `NetworkError`.
- `codes.ts` — hand-maintained union of the spec §23 registry (**type-only**,
  no generated coupling; unknown codes degrade to `string & {}`).
- `Idempotency-Key` — required on money-moving POSTs (spec §8.8):
  `api.post(path, body, { idempotent: true })` or an explicit
  `idempotencyKey` for true retries (same payload only —
  `IDEMPOTENCY_KEY_MISMATCH` otherwise).

## Trading library (`src/lib/trading/`)

Wave-2 trading-ux shared layer (Tasks 10.3.7–10.3.15). All wire payloads are
parsed from `unknown` and money/price/qty fields are `Dec`
(`src/lib/decimal/decimal.ts` — bigint-backed, no binary float).

- `types.ts` / `api.ts` / `queries.ts` — wire parsers + REST calls +
  TanStack Query hooks. Query keys carry the account scope, so a
  sub-account switch re-scopes cached data atomically.
- `accountScope.ts` — `useAccountScope` (persisted zustand): `master` or a
  numeric sub-account id (`scopeAccountId`, `MASTER_ACCOUNT_KEY`); sub
  requests append `account_id`.
- `marketStore.ts` — `useMarketStore` BBO/depth cache fed by
  `useMarketFeed` (`bbo@{symbol}` / `depth@{symbol}`); `useMidPrice` is
  WeakMap-memoized per Bbo (zustand selectors must be referentially
  stable); `useChannelHealth` surfaces per-channel staleness.
- `orderDraft.ts` — `useOrderDraft` shared draft so depth-chart clicks and
  chart repricing prefill the order panel.
- `positionFeed.ts` — `usePrivatePositionsFeed` merges `private:positions`
  overlays per field; absent fields never erase a known ADL rank or mark.
- `projections.ts` — book-walk fill projection (VWAP, partial fill,
  slippage vs mid, cumulative depth).
- `fx.ts` — pair/pip conventions (`EUR/USD`, bare `USDJPY` tolerated),
  JPY pip = `0.01`, instrument tick-size overrides, `formatPrice`.
- `testkit.ts` — `makeWsHarness` / `connectWs` / `pushEvent` / `ackSub`:
  a real `WsClient` over `FakeClock`+`MockSocket`, act()-wrapped for
  component tests.

## Workspace stores (`src/features/workspace/`)

- `liteMode.ts` — `useUiModeStore` (persisted): `lite` | `pro` | auto;
  `defaultModeForKyc` resolves T0/T1→lite, T2/institutional→pro (Task
  10.3.9).
- `theme.ts` — `useTheme` (persisted): `dark` | `light`, applied via
  `theme.css` tokens.
- `layouts.ts` — named workspace layouts under a per-scope storage key
  (`workspaceStorageKey(scopeKey)`); save/load/list/delete/reset, panel
  grid placements clamped to `GRID_COLS`×`ROW_H`; corrupt JSON degrades to
  mode defaults rather than throwing (Task 10.3.14).

## SPA shell security (Task 10.3.1 item 8)

- `index.html` emits a meta CSP: `default-src 'self'`, **no
  `'unsafe-inline'`**, `frame-ancestors 'none'`, `upgrade-insecure-requests`.
  In `vite dev` only, `devCspRelaxation` (vite.config.ts) relaxes
  `style-src`/`connect-src` for HMR + injected styles — the production
  `dist/index.html` is untouched.
- `public/_headers` (Netlify/CF-Pages convention) emits `X-Frame-Options:
DENY`, `frame-ancestors 'none'`, `Referrer-Policy: no-referrer`, `nosniff`,
  COOP/CORP + Permissions-Policy on every response. Nginx equivalent:

  ```nginx
  add_header X-Frame-Options DENY always;
  add_header Referrer-Policy no-referrer always;
  add_header X-Content-Type-Options nosniff always;
  add_header Content-Security-Policy "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self' wss:; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; upgrade-insecure-requests" always;
  ```

- **SRI**: no vendored third-party chunks exist in Wave-1. If Wave-2 adds
  one, the `<script>`/`<link>` tag MUST carry `integrity="sha384-…"` +
  `crossorigin` — CSP `require-trusted-types-for 'script'`/SRI enforcement
  is checked at review.

## Bundle budget

`npm run build` runs `scripts/check-bundle-size.mjs`, which resolves the
entry chunk + static-import closure from `dist/.vite/manifest.json` and fails
the build when gzipped initial JS exceeds **300 kB** (Task 10.3.1 item 7).

## Test stack

Vitest + React Testing Library (jsdom). `src/lib/ws/testkit.ts` provides the
`FakeClock`/`MockSocket` ports the client tests use — reuse them in feature
tests. Playwright is scaffolded under `e2e/` with `npm run test:e2e`; the CI
e2e job lands in Wave-2.
