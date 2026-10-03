# Phase 10 — Trader UI (React + TypeScript)

**Duration:** 31–39 days (35 days nominal; supersedes prior 20–26 / 23.5 nominal — header drift repair 2026-09-27, aligns header to §10.6; prior supersedes 19–24 (Task 10.3.20 added 2026-09-27, remediation #26) and "7–9 days" (AGENTS.md phase index) and "5–7 days" (this header and §10.6)). Recomputed 2026-09-25 against the complete 19-task list — §10.6 had been itemizing only Tasks 10.3.1–10.3.6 + 10.3.19 and silently omitted the 12 tasks added by remediations #11–#14 (10.3.7–10.3.18, ≈15 days).
**Dependencies:** Phases 3, 5, 6 (hard) + soft forward references per §10.5
**Spec Reference:** §21 (Trader UI)

---

## 10.1 Objectives

Implement the React 18 + TypeScript trading UI: order entry, order book (virtualized), positions, account balances, charts (TradingView Lightweight Charts), and admin dashboards.

---

## 10.2 Prerequisites

- Phase 3 (settlement), Phase 5 (gateway API), Phase 6 (market data WS)

---

## 10.3 Tasks

### Task 10.3.1: Project Scaffold

**Objective:** Create React 18 + TypeScript project with Vite.

**File Locations:** `frontend/`

**Implementation:**
1. Vite + React 18 + TypeScript with `"strict": true` (no `any` anywhere in `src/` except declared adapter boundaries).
2. State: Zustand (local), TanStack Query (server).
3. WS client: custom hook with reconnect + `last_seq` resume — the state machine is normative per Task 10.3.19.
4. Routing: React Router, code-split per route.
5. Styling: Tailwind CSS.
6. **(amended 2026-09-25 — frontend baseline):** Test stack = Vitest + React Testing Library (unit/component) and Playwright (e2e smoke: login → subscribe → place order → close position). CI gate: `tsc --noEmit` + ESLint + Prettier check + unit + e2e smoke.
7. **(amended 2026-09-25 — frontend baseline):** Bundle budget — initial JS payload ≤ 300 kB gzipped, with TradingView Lightweight Charts and the backtester loaded as lazy chunks; a failing budget fails the build.
8. **(amended 2026-09-25 — frontend baseline; ownership re-pointed by governance remediation #17):** SPA shell security — build emits a meta `Content-Security-Policy` (`default-src 'self'`, no `'unsafe-inline'`, Trusted Types for any dynamic HTML), subresource integrity on vendored chunks, and `X-Frame-Options: DENY` / `frame-ancestors 'none'` plus `Referrer-Policy: no-referrer` on the static-asset server. **Division of ownership:** this task owns the headers on SPA/static-asset responses; Phase-05 Task 5.3.29 (amended 2026-09-25) owns the same hardening-header set on every API-gateway response. Both are enforced in CI, neither depends on the other.
9. **(amended 2026-09-25 — frontend baseline):** No i18n — en-US only, no i18n framework, per spec §27 R5. Do not add locale tables or CLDR/RTL support in Phase 10.

**Definition of Done (Acceptance Criteria):**
* [x] Vite + React 18 + TypeScript project builds
* [x] Zustand + TanStack Query configured
* [x] WS client hook with reconnect
* [x] Tailwind CSS configured

**SDD Checklist:**
- [x] Spec checkpoint: React 18 + TypeScript + Vite — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.2: Order Book Component

**Objective:** Virtualized order book with real-time WS updates.

**File Locations:** `frontend/src/components/OrderBook/`

**Implementation:**
1. Virtualized list (react-window) for 20 levels per side.
2. Real-time WS updates with 100ms conflation.
3. Price level highlighting on change.
4. Spread display.
5. Depth bar visualization.

**Definition of Done (Acceptance Criteria):**
* [x] Virtualized order book renders 20 levels per side
* [x] Real-time WS updates with no jitter
* [x] Spread displayed
* [x] Depth bars rendered

**SDD Checklist:**
- [x] Spec checkpoint: virtualized order book — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.3: Order Entry Form

**Objective:** Order entry form with validation.

**File Locations:** `frontend/src/components/OrderEntry/`

**Implementation:**
1. Fields: symbol, side (buy/sell), type (limit/market/stop), qty, price, TIF.
2. Validation: qty > 0, price > 0 for limit, balance check (client-side estimate).
3. Submit: POST /api/v1/orders.
4. Quick order: click on book level pre-fills price.

**Definition of Done (Acceptance Criteria):**
* [x] Order entry form validates all fields
* [x] Submit reaches gateway API
* [x] Click on book level pre-fills price

**SDD Checklist:**
- [x] Spec checkpoint: order entry with validation — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

**(amended 2026-10-03 — IA consolidation, spec §27):** the standalone `/trade/:symbol` page and its basic `OrderEntry` form are superseded — the workspace order ticket is `AdvancedOrderPanel` (Task 10.3.7); `/trade/:symbol` redirects into `/workspace` preserving the instrument, and book-level click prefill survives via the workspace order-book panel + `useOrderDraft`. Server-side order preview→confirm remains on `history/OrderTestPanel` (`POST /api/v1/orders/test`, Task 10.3.27).

---

### Task 10.3.4: Charts (TradingView Lightweight Charts)

**Objective:** Candlestick charts with real-time updates.

**File Locations:** `frontend/src/components/Charts/`

**Implementation:**
1. TradingView Lightweight Charts library.
2. Candlestick + volume.
3. Real-time updates from WS trades.
4. Timeframes: 1m, 5m, 15m, 1h, 4h, 1d.
5. Crosshair + tooltips.

**Definition of Done (Acceptance Criteria):**
* [x] Candlestick chart renders with volume
* [x] Real-time updates from WS
* [x] Multiple timeframes switchable
* [x] Crosshair + tooltips work

**SDD Checklist:**
- [x] Spec checkpoint: TradingView Lightweight Charts — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

**(amended 2026-10-03 — IA consolidation, spec §27):** `features/charts/TradingChart` is the single chart implementation — it now also hosts the Task-10.3.15 overlay layer (`ChartOverlays`); the interim hand-rolled SVG renderer under `advanced-orders/` is deleted.

---

### Task 10.3.5: Positions & Balances

**Objective:** Real-time positions and balances display.

**File Locations:** `frontend/src/components/Account/`

**Implementation:**
1. Balances: all currencies with available/locked/total.
2. Positions: instrument, side, qty, entry, mark, unrealized P&L.
3. Real-time via WS private stream.
4. Close position button.

**Definition of Done (Acceptance Criteria):**
* [x] Balances display all currencies
* [x] Positions display with unrealized P&L
* [x] Real-time updates via WS
* [x] Close position works

**SDD Checklist:**
- [x] Spec checkpoint: real-time positions + balances — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.6: Admin Dashboard

**Objective:** RBAC-gated admin dashboard.

**File Locations:** `frontend/src/components/Admin/`

**Implementation:**
1. Role-based view: Super Admin sees all; Read-Only Auditor sees dashboards only.
2. Instrument management: create, suspend, halt, resume, delist.
3. User management: freeze, unfreeze, KYC review.
4. Audit log viewer.
5. System health: degradation mode, circuit breakers, shard health.

**Forward-reference notes:** The admin dashboard UI is built against route definitions from Phase 5 (gateway API). Backend integration for some features is deferred to later phases: instrument management API is Phase 15 (Task 15.3.2), KYC review workflow is Phase 14 (Task 14.3.4), five-tier circuit breaker is Phase 13 (Task 13.3.1). During Phase 10, the UI components are built with mock data / stub API responses; later phases wire the real backend endpoints. Phase 7 (Admin & Monitoring) provides the RBAC middleware and admin audit log that the dashboard consumes.

**Definition of Done (Acceptance Criteria):**
* [x] Admin dashboard RBAC-gated
* [x] Instrument management UI *(closed 2026-09-30: `InstrumentsPanel` consumes the live Phase-15 surface — list table (symbol/type/status+grace/tick/lot/qty bounds/settlement/leverage), lifecycle actions filtered by state edge-set AND caller role (mirror of route registry + `lifecycleOps`), create modal (SA+dual-control), edit modal (RM, DRAFT/ACTIVE), confirm+reason capture, resume auction opts, 202→"submitted for approval" ack; 12/12 vitest green)*
* [x] User management UI
* [x] Audit log viewer
* [x] System health dashboard

**SDD Checklist:**
- [x] Spec checkpoint: RBAC-gated admin dashboard — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.7: Advanced Order Types Panel & Sub-Account Switcher

**Objective:** Extend the order entry UI to expose all backend-supported order types and provide institutional sub-account switching. Added 2026-09-20 (feature-completeness audit remediation #11).

**File Locations:** `frontend/src/components/OrderEntry/AdvancedPanel.tsx`, `frontend/src/components/AccountSwitcher/SubAccountDropdown.tsx`

**Implementation:**
1. "Advanced" toggle on order form expands panel with: all TIF modes (GTC, IOC, FOK, DAY, GTD), iceberg parameters (visible_qty), trailing stop distance (PIPS/PERCENTAGE/ABSOLUTE), bracket order (entry + SL + TP).
2. Sub-account dropdown in account header: lists all sub-accounts under the master, shows per-sub-account balances, and switches trading context.
3. Admin fee tier assignment screen: allows admin to assign bespoke fee tiers to institutional accounts.

**Definition of Done (Acceptance Criteria):**
* [x] Advanced order panel exposes all TIF modes, iceberg, trailing stop, bracket
* [x] Sub-account switcher displays all sub-accounts with balances
* [x] Admin fee tier assignment UI functional

**SDD Checklist:**
- [x] Spec checkpoint: advanced order UI — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

**(amended 2026-10-03 — IA consolidation, spec §27):** `AdvancedOrderPanel` is the single Pro order ticket (the Task-10.3.3 basic form is superseded); `SubAccountSwitcher` renders in the workspace header rather than a dedicated page; `/advanced/:symbol` redirects into `/workspace`. The panel also opens the Task-10.3.8 calculator as a modal.

---

### Task 10.3.8: Position / margin calculator widget

Position / margin calculator widget — React component panel accessible from trading interface providing: PnL Calculator (entry/exit/lots → profit in account currency), Pip Value Calculator (leverages Phase-03 pip engine), Margin Calculator (pair/lots/leverage → required margin), Liquidation Price Calculator (entry/balance/leverage → liquidation price), Swap Calculator (pair/lots/direction → daily swap charge). All calculations use live mark prices and account leverage tier.

**SDD Checklist:**
- [x] Spec checkpoint: calculator outputs use live marks and account leverage (§24 #267) — defined first, validated against spec

**(amended 2026-10-03 — IA consolidation, spec §27):** delivered as the reusable `PositionCalculator` widget — opened as a modal from the order ticket; `/calculator/:symbol?` is a thin deep-link host, no longer a primary nav destination.

---

### Task 10.3.9: Pro/Lite UI mode toggle

Pro/Lite UI mode toggle — user preference `ui_mode: PRO | LITE` stored in user settings. LITE mode renders simplified interface: pair selector, market buy/sell buttons, balance display, basic chart, and open positions list. PRO mode renders full trading cockpit (charts, depth, order book, advanced order forms, multiple panels). Default: LITE for T0/T1 KYC tiers, PRO for T2/institutional. Toggle accessible via header icon.

**SDD Checklist:**
- [x] Spec checkpoint: persistent Lite/Pro modes expose appropriate complexity (§24 #268) — defined first, validated against spec

---

### Task 10.3.10: One-click quick actions

One-click quick actions — UI buttons for: (a) "Close All Positions" — calls `POST /api/v1/positions/close-all` with confirmation modal showing projected execution summary; (b) "Reverse Position" per-position button — closes current position and opens equal-size opposite, implemented as reduce_only close + new order; (c) "Flatten" per-position button — market-close single position. All actions require confirmation modal.

**SDD Checklist:**
- [x] Spec checkpoint: quick actions require projected-execution confirmation (§24 #268) — defined first, validated against spec

---

### Task 10.3.11: Percentage order quantity sliders

Percentage order quantity sliders — order entry form percentage selector (10%, 25%, 50%, 75%, 100%) that auto-calculates and populates quantity field based on available free margin, current leverage tier, and selected instrument's lot size constraints. Updates dynamically as mark price changes.

**SDD Checklist:**
- [x] Spec checkpoint: percentage sizing respects free margin and lot filters (§24 #268) — defined first, validated against spec

---

### Task 10.3.12: Interactive depth chart visualization

Interactive depth chart visualization — D3.js or TradingView Lightweight Charts depth area chart rendering cumulative bid (green) and ask (red) volume curves vs price. Clicking a price level populates the order form price field. Updates in real-time from L2 feed. Displayed as collapsible panel below the order book.

**(amended 2026-10-03 — chart-stack consolidation, spec §27):** the renderer is TradingView Lightweight Charts (the "or" resolved in favor of unification) — `features/depth-chart/DepthChart` draws the bid/ask cumulative curves as two `AreaSeries` on the shared `useLwcChart` lifecycle seam (`features/charts/useLwcChart`), with prices index-encoded on the time axis and decoded by `tickMarkFormatter`. The interim hand-rolled SVG renderer is deleted; the same migration put `features/performance/EquityCurveChart` (Task 10.3.18 surface) on a `LineSeries`. `ChartOverlays` remains an SVG annotation layer over the canonical chart — it is not a competing chart renderer.

**SDD Checklist:**
- [x] Spec checkpoint: interactive depth chart consumes canonical L2 data (§24 #268) — defined first, validated against spec

---

### Task 10.3.13: ADL priority indicator display

ADL priority indicator display — per-position 5-segment indicator bar showing the user's current Auto-Deleveraging priority rank (1=lowest risk, 5=highest risk). Sourced from `adl_indicator` field in `private:positions` WS updates (computed by Phase-19 Task 19.3.19). Tooltip explains ADL mechanics and risk reduction strategies.

**SDD Checklist:**
- [x] Spec checkpoint: ADL rank is visible with clear risk explanation (§24 #269) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.14: Customizable Trading Workspace

Implement movable/resizable panels, saved named layouts, light/dark themes, per-device preferences, and one-click restore to the canonical Lite/Pro defaults (migration 076 workspace preferences). Validate keyboard navigation and ensure safety-critical order/risk panels cannot be hidden without an explicit warning.

**(amended 2026-09-25 — accessibility baseline):** Target WCAG 2.1 AA across every Phase-10 surface: 4.5:1 text and 3:1 large-text/UI contrast in both themes, full keyboard operability for order entry, order book, chart overlays and quick actions, visible focus rings, `aria-live` announcements for order confirmations/rejections, and screen-reader labels on calculators, the ADL indicator and the depth chart. Colour is never the sole signal for buy/sell, stale pricing or ADL rank. Enforced by an axe-core audit in CI (`playwright/test/a11y.spec.ts`) over the trade, positions, admin and calculator routes.

**SDD Checklist:**
- [x] Spec checkpoint: users can save, restore, and reset accessible trading layouts (§24 #292) — defined first, validated against spec

**(amended 2026-10-03 — IA consolidation, spec §27):** the Pro grid gained a sixth panel (order book); stored layouts merge panels added after the save with the mode default rather than invalidating the layout.

---

### Task 10.3.15: Chart Trading and Order Overlays

Display open orders as draggable price lines, historical fills as buy/sell markers, positions and stop/target levels, and current-candle close countdown. Dragging an order opens a confirmation preview and uses keep-priority amendment only for quantity-down; price drags use cancel-replace and show priority loss.

**SDD Checklist:**
- [x] Spec checkpoint: chart overlays support safe inspect/modify/cancel with explicit priority semantics (§24 #292) — defined first, validated against spec

**(amended 2026-10-03 — IA consolidation, spec §27):** overlays render on the canonical Lightweight Charts surface (`features/charts/ChartOverlays`) via `priceToCoordinate`/`coordinateToPrice`/`timeToCoordinate`; drag, keyboard-nudge, inspect, and cancel-replace/priority-loss semantics are unchanged — the interim SVG renderer is deleted.

---

### Task 10.3.16: Technical Indicators and Strategy Backtesting

Provide built-in MA/EMA/WMA/Bollinger/VWAP/RSI indicators, saved presets, and a sandboxed strategy editor/backtester over Phase-23 historical data. Backtests disclose spread, commission, swap, slippage, survivorship, and data-latency assumptions and cannot place live orders.

**SDD Checklist:**
- [x] Spec checkpoint: indicator presets and cost-aware sandboxed backtests are reproducible (§24 #293) — defined first, validated against spec

---

### Task 10.3.17: FX Market Discovery, Watchlists, and Rate Alerts

Add symbol search, persistent watchlists, top movers, volume/volatility heatmaps, session-mover views, and user-defined price/percentage alerts delivered through Phase-12 preferences. Market-cap concepts are excluded; all ranking is FX-relevant.

**SDD Checklist:**
- [x] Spec checkpoint: FX discovery views, watchlists, and rate alerts use canonical market data (§24 #294) — defined first, validated against spec

---

### Task 10.3.18: Client Performance Dashboard

Add equity/account-value curve, realized/unrealized P&L, drawdown, pair/strategy attribution, and fee/commission/swap breakdown using Phase-20 reporting APIs. Values support account base/reporting currency and reconcile to statements.

**SDD Checklist:**
- [x] Spec checkpoint: client performance dashboard reconciles to ledger-backed reports (§24 #295) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.19: Network Reconnection, Stale Data Indicators & Optimistic Rollback

**Objective:** Implement client-side fault tolerance, disconnect state banners, stale pricing warnings, and optimistic state rollbacks in Trader UI per spec §2.7, §10.6, §21.3, and §24 #310.

**Implementation:**
1. **Network Disconnect Banner & Lockout:** On WebSocket disconnect, display prominent top banner `DISCONNECTED — RECONNECTING...` and disable order placement buttons to prevent blind submission during network failure.
2. **Reconnect Schedule (canonical, conforms to spec §21.3):** exponential backoff with jitter of 100ms, 250ms, 500ms, 1s, 2s, then a 10s cap with ±20% jitter. This supersedes the prior plan-prose "500ms to 30s" (remediation #16 reconciliation): the 30s cap would have stalled the client outside the Phase-06 Task 6.3.9 60s ring-buffer replay window, forcing needless full-snapshot re-syncs, while the 10s cap guarantees ≤6 attempts/min — comfortably under the Phase-06 Task 6.3.21 throttle of 10 reconnects/min/IP (citation corrected 2026-09-27, remediation #35 — the throttle lives in Task 6.3.21; 6.3.7 is the WS message-rate task), so the client is never self-throttled with `RATE_LIMIT_TIER_EXCEEDED` during a real outage. The attempt counter resets to 100ms only on a successful handshake, not on a resumed stream.
3. **Stale Pricing Warning:** If market data stream remains silent for >3.0s during active trading sessions, trigger amber `STALE_PRICING` visual badge across order book and chart. Order entry stays enabled in `STALE` — server-side reference-price execution collars (spec §22.2) are the safety net, not the client.
4. **Optimistic State Rollback:** When submitting orders, optimistically reflect pending state in UI; upon receipt of server error envelope (RFC 7807), immediately roll back pending UI state and present actionable error toast with `request_id`.

**WS Client State Machine (normative, amended 2026-09-25 — remediation #16):**

| State | Meaning | Order entry |
|---|---|---|
| `CONNECTING` | Dialing + auth handshake | locked |
| `AUTHENTICATED` | Subscribed, live, per-channel `last_seq` tracking active | enabled |
| `STALE` | Connected, but no md tick for >3.0s in trading hours | enabled + amber |
| `DISCONNECTED` | Socket closed | **locked + banner** |
| `RECONNECTING` | Waiting on backoff attempt | **locked + banner** |
| `RESYNCING` | Handshake OK, replaying from ring buffer or applying snapshot | **locked** until every subscribed channel converges |

Transitions: `CONNECTING → AUTHENTICATED` (auth ack) · `CONNECTING → DISCONNECTED` (timeout/failure) · `AUTHENTICATED ↔ STALE` (tick received / 3.0s silence) · `AUTHENTICATED|STALE → DISCONNECTED` (close frame, heartbeat miss, or close code 4019 `AUTH_EXPIRED`) · `DISCONNECTED → RECONNECTING → CONNECTING` · `CONNECTING → RESYNCING → AUTHENTICATED` · `AUTH_EXPIRED` forces token refresh then `CONNECTING`; if refresh fails, flush all optimistic order state and redirect to login. On a `server.shutdown` advisory (Phase-06 Task 6.3.19) transition to `RECONNECTING` using the advisory's reconnect target and **do not** reset the attempt counter, so scheduled maintenance does not bypass backoff. `last_seq` is retained per channel across the whole cycle so the Phase-06 60s ring buffer is always used instead of snapshot fallback.

Invariant: the client may never hold optimistic order state across a full re-authentication.

**Definition of Done (Acceptance Criteria):**
* [x] Disconnected socket disables trading inputs and renders reconnection banner
* [x] Stale market data (>3s quiet) surfaces amber warning indicator
* [x] Rejected orders roll back optimistic UI updates and render RFC 7807 error toast

**SDD Checklist:**
- [x] Spec checkpoint: UI network reconnection, stale pricing indicators, and optimistic rollback (§24 #310) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.20: Environment Switcher, Fleet Pages & Ops Board UI

**Objective:** Build the admin console surfaces for the Phase-09 fleet backend and the Phase-15 operations workflows, per spec §19.16/§7.5 and §24 #351. Added 2026-09-27 (environment & operations remediation #26).

**Implementation:**
1. **Environment switcher:** persistent context pill (dev green / staging amber / prod red) with explicit confirm on entering production context; all fleet/promotion/ops pages render through the active context. Prod context shows a standing banner and disables bulk actions outside deploy windows.
2. **Fleet pages (extends Task 10.3.6 dashboard):** host inventory grid (role, shard, AZ, health, state), cluster topology views (shard→host, Sentinel roles, NATS/CH/PG health), release pipeline (artifact hash, stage, approvers, gate evidence) with promote buttons gated by the Task 9.3.30 interlocks; server-action confirmations route through dual-control signing.
3. **Ops board (extends Task 10.3.6 instrument UI):** list-pair wizard (proposal form with §7.4 auto-check display), delist workflow with impact preview, non-ACTIVE instrument board with grace timers and one-click resume/halt. Every action surfaces its required role and dual-control state before submission.
4. **Guards:** staging pages never call prod endpoints (context-bound API client); RBAC-gating reuses the Task 10.3.6 gate plus `env` scope; axe-core audit extended to the three new routes (WCAG 2.1 AA per Task 10.3.14).

**Definition of Done (Acceptance Criteria):**
* [x] Context pill, prod confirm, and context-bound API client verified (no cross-env calls)
* [x] Fleet, release and ops-board pages render backend state with dual-control flows
* [x] New routes pass the axe-core audit; RBAC-gated including `env` scope — context pill/prod-confirm/context-bound client verified + FleetPage/ReleasesPage/OpsBoardPage pass wcag21aa via a11y.audit.test.tsx (jest-axe 11)

**SDD Checklist:**
- [x] Spec checkpoint: environment switcher with context-bound client, fleet/release pages, and ops-board UI with dual-control surfacing (§24 #351) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.21: Authentication, Registration & Session Screens

**Objective:** Build the login, registration, 2FA, and session-management screens that gate the entire SPA. Added 2026-09-27 (UI-surface-coverage remediation #32). The WS state machine (Task 10.3.19) redirects to `/login` on auth-expired close code 4019, but the login page itself had no owner.

**File Locations:** `frontend/src/pages/auth/LoginPage.tsx`, `frontend/src/pages/auth/RegisterPage.tsx`, `frontend/src/pages/auth/TwoFactorPage.tsx`, `frontend/src/components/Account/SessionList.tsx`

**Implementation:**
1. **Login screen** (`/login`): email/username + password, "remember me" toggle, redirect target param, rate-limit aware (surfaces `RATE_LIMIT_TIER_EXCEEDED` with `Retry-After`), anti-phishing-code display slot.
2. **Registration screen** (`/register`): email, password (strength meter), confirm, accepts terms checkbox (links to execution-policy and consent texts), calls `POST /api/v1/auth/register`; on success routes to KYC submission (Task 10.3.24).
3. **2FA screens**: setup (`POST /api/v1/auth/2fa/setup` — shows QR + backup codes), verify (`POST /api/v1/auth/2fa/verify`), disable (`POST /api/v1/auth/2fa/disable` — requires password re-entry). Backup-code download with one-time display.
4. **Session management**: `GET /api/v1/account/sessions` renders active sessions table (device, IP, location, last-active, current-marker); `DELETE /api/v1/account/sessions/{id}` revokes with confirmation modal. Calls `POST /api/v1/auth/logout` for self-revoke.
5. **Token refresh**: silent `POST /api/v1/auth/refresh` interceptor on 401; on refresh failure, flush optimistic state and redirect to `/login`.
6. **Route guards**: `<RequireAuth>` wrapper redirects unauthenticated users to `/login` with redirect target; `<RequireRole>` for admin routes (extends Task 10.3.6 RBAC gating).

**Definition of Done (Acceptance Criteria):**
* [x] Login, register, 2FA setup/verify/disable, and session-list screens functional
* [x] Route guards redirect unauthenticated users; token refresh is silent until refresh fails
* [x] Session revoke works with confirmation; logout clears all local state
* [x] All auth screens pass axe-core audit (WCAG 2.1 AA per Task 10.3.14) — LoginPage + TOTP step + RegisterPage + ForgotPasswordPage + SessionList audited green (a11y.audit.test.tsx, jest-axe)

**SDD Checklist:**
- [x] Spec checkpoint: auth screens gate the SPA, 2FA is mandatory before trading, sessions are revocable (§24 #382) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.22: Account Settings & Security Center

**Objective:** Build the account-security hub covering profile, WebAuthn/passkeys, anti-phishing code, device management, emergency freeze, cooling-off, GDPR export/erase, consent, and notification preferences. Added 2026-09-27 (remediation #32). Phase-12 and Phase-14 backends existed but had no UI.

**File Locations:** `frontend/src/pages/account/SettingsPage.tsx`, `frontend/src/components/Account/SecurityPanel.tsx`, `frontend/src/components/Account/ProfileForm.tsx`, `frontend/src/components/Account/WebAuthnManager.tsx`, `frontend/src/components/Account/GdprPanel.tsx`

**Implementation:**
1. **Profile**: `GET/PUT /api/v1/account/profile` — editable display name, email, phone, tax residency, MiFID categorization display (read-only).
2. **WebAuthn/Passkeys**: `POST /api/v1/account/webauthn/register` (browser PublicKeyCredential API), `POST /api/v1/account/webauthn/authenticate`; list registered passkeys with friendly names; delete with password re-entry.
3. **Anti-phishing code**: `PUT /api/v1/account/settings/anti-phishing-code` — set 4–32 char code (range aligned with spec §5.16 and Phase-12 Task 12.3.8, remediation #35) displayed in every platform email; shown masked in settings.
4. **Device management & login history**: `GET /api/v1/account/login-history` — paginated table (timestamp, device, IP, geo, success/fail); active-device list with revoke.
5. **Emergency freeze**: `POST /api/v1/account/emergency-freeze` — one-click self-freeze with prominent confirmation modal explaining scope (blocks logins, cancels open orders, disables withdrawals); routes to unfreeze-request flow (`POST /api/v1/account/unfreeze-request`).
6. **Cooling-off / self-exclusion**: `POST /api/v1/account/cooling-off` — select duration (24h/7d/30d/permanent), confirmation with countdown; shows active cooling-off status and remaining time.
7. **Account closure**: `POST /api/v1/account/close` — multi-step wizard (zero-balance check, open-position check, withdrawal of residual, final confirmation); blocks if preconditions unmet.
8. **GDPR**: `POST /api/v1/account/gdpr/export` (async job → download link), `POST /api/v1/account/gdpr/erase` (right-to-erasure with legal-hold carve-out notice).
9. **Consent**: `PUT /api/v1/account/consent` — toggle consents (marketing, research, third-party); `GET` shows current state.
10. **Notification preferences**: `GET/PUT /api/v1/account/notifications/preferences` — per-channel (email/push/SMS/WS) × per-event toggles (WS channel delivered by Phase-12 Task 12.3.5, remediation #35).

**Definition of Done (Acceptance Criteria):**
* [x] Profile edit, WebAuthn register/authenticate, anti-phishing code set functional
* [x] Login history, device management, emergency freeze, cooling-off, account closure, GDPR export/erase, consent, notification preferences all functional
* [x] Emergency freeze and account closure require explicit confirmation modals
* [x] All settings screens pass axe-core audit (WCAG 2.1 AA per Task 10.3.14) — profile/security/api-keys/notifications/safety tabs audited green

**SDD Checklist:**
- [x] Spec checkpoint: account-security center covers profile, WebAuthn, anti-phishing, devices, emergency freeze, cooling-off, closure, GDPR, consent, notifications (§24 #383) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.23: Funding & Transfers Screens

**Objective:** Build deposit-instructions, withdrawal, internal-transfer, and fee-estimator screens. Added 2026-09-27 (remediation #32). Phase-11 banking-rails backend existed but had no UI.

**File Locations:** `frontend/src/pages/funding/FundingPage.tsx`, `frontend/src/components/Funding/DepositPanel.tsx`, `frontend/src/components/Funding/WithdrawalForm.tsx`, `frontend/src/components/Funding/TransferForm.tsx`, `frontend/src/components/Funding/FeeEstimator.tsx`

**Implementation:**
1. **Deposit**: `GET /api/v1/deposits/{currency}` — shows per-currency bank instructions (bank name, SWIFT/IBAN, beneficiary, reference code), QR for SEPA/FedNow instant, copy-to-clipboard; flags pending deposits from `GET /api/v1/funding`.
2. **Withdrawal**: `POST /api/v1/withdrawals` — currency selector, amount, beneficiary bank-account selector (from Phase-11 beneficiary registry), 2FA confirmation, shows 15-min confirmation window; `POST /api/v1/withdrawals/{id}/confirm` — pending-withdrawal list with countdown timer and confirm/cancel buttons.
3. **Internal transfers**: `POST /api/v1/transfers` — master↔sub-account and account↔account dropdowns (same user), amount, currency, live balance display, GL-posting confirmation; `GET /api/v1/transfers` — transfer history table.
4. **Fee estimator**: `POST /api/v1/funding/fee-estimate {amount, currency, rail, direction}` — inline preview of rail fee, estimated arrival time, and cut-off time before which the rail processes same business day; shown before withdrawal submission.
5. **Funding history**: `GET /api/v1/funding` — unified table (deposits, withdrawals, transfers, fees) with filters (currency, type, date range, status).

**Definition of Done (Acceptance Criteria):**
* [x] Deposit instructions render per-currency bank details with copy/QR
* [x] Withdrawal form with beneficiary selector, 2FA, and 15-min confirm window functional
* [x] Internal transfer between owned accounts with live balance check
* [x] Fee estimator shows rail fee, arrival estimate, and cut-off
* [x] Funding history table filters by type, currency, date, status
* [x] All funding screens pass axe-core audit — deposit/withdraw/transfer/history tabs audited green

**SDD Checklist:**
- [x] Spec checkpoint: funding screens cover deposits, withdrawals with 15-min confirm, internal transfers, fee estimate, and unified history (§24 #384) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.24: KYC Submission & Status Tracker

**Objective:** Build the document-upload wizard and KYC-status tracker. Added 2026-09-27 (remediation #32). Phase-14 KYC backend existed but had no UI. Registration (Task 10.3.21) routes here on success.

**File Locations:** `frontend/src/pages/kyc/KycPage.tsx`, `frontend/src/components/Kyc/DocumentUpload.tsx`, `frontend/src/components/Kyc/StatusTracker.tsx`

**Implementation:**
1. **Status tracker**: `GET /api/v1/kyc/status` — displays current tier (T0/T1/T2), verification state (PENDING/APPROVED/REJECTED/EXPIRED — aligned with the §5.16 enum extended by remediation #35: `APPROVED` is canonical, `EXPIRED` drives re-verification), per-document status, re-verification due date, and trading-limit impact summary (leverage, max notional) per spec §12.7 matrix.
2. **Upload wizard**: `POST /api/v1/kyc/submit` — multi-step: (a) personal info (name, DOB, nationality, address), (b) ID document (front/back upload or camera capture), (c) proof of address (utility bill/bank statement upload), (d) selfie/liveness check if required by tier. File validation (type, size ≤10 MB, DPI ≥ 200). Resumable uploads with progress bar.
3. **Re-verification flow**: when `status == EXPIRED`, shows re-verification prompt with due date and degraded-access warnings; routes to upload wizard pre-filled.
4. **Institutional manual review**: if T2 institutional, shows manual-review submission state and contact channel.

**Definition of Done (Acceptance Criteria):**
* [x] KYC status tracker displays tier, state, per-document status, and trading-limit impact
* [x] Upload wizard handles multi-step document submission with file validation and progress
* [x] Re-verification flow triggers on expiry with degraded-access warnings
* [x] KYC screens pass axe-core audit — status tracker + upload wizard audited green

**SDD Checklist:**
- [x] Spec checkpoint: KYC UI shows tier/status, trading-limit impact, multi-step upload, and re-verification prompts (§24 #385) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.25: Support Tickets & Help Center

**Objective:** Build the support-ticket submission and tracking screen. Added 2026-09-27 (remediation #32). Phase-07 Task 7.3.7 backend existed but had no UI.

**File Locations:** `frontend/src/pages/support/SupportPage.tsx`, `frontend/src/components/Support/TicketForm.tsx`, `frontend/src/components/Support/TicketList.tsx`

**Implementation:**
1. **Ticket list**: `GET /api/v1/support/tickets` — paginated table (subject, category, status, priority, created, last-update); filter by status/category; click to view conversation thread.
2. **Ticket form**: `POST /api/v1/support/tickets` — category selector (trading, funding, account, technical, compliance), subject, rich-text body, attachment upload (screenshots/logs, ≤5 MB), priority (normal/urgent — urgent requires T2+ or financial-impact justification).
3. **Conversation view**: per-ticket thread with user/staff messages, attachments, status transitions (OPEN → IN_PROGRESS → RESOLVED → CLOSED), and user-side reopen button.
4. **Support-view** (per Phase-07 Task 7.3.7): if user has Support Agent role, renders the staff-side queue with assignment, SLA timer, and canned-response templates.

**Definition of Done (Acceptance Criteria):**
* [x] Ticket list renders with filters and conversation thread view
* [x] Ticket submission with category, attachments, and priority gating functional
* [x] Staff support-view queue renders for Support Agent role
* [x] Support screens pass axe-core audit — ticket list + new-ticket form + ticket thread audited green

**SDD Checklist:**
- [x] Spec checkpoint: support UI covers ticket submission with attachments, conversation thread, status lifecycle, and staff queue (§24 #386) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.26: Copy Trading & Grid Bot Management UI

**Objective:** Build the copy-trading strategy browser/follow UI and the grid-bot creation/management panel. Added 2026-09-27 (remediation #32). Phase-14 Task 14.3.14 copy-trading and Phase-16 Task 16.3.19 grid-bot backends existed but had no UI.

**File Locations:** `frontend/src/pages/strategies/StrategiesPage.tsx`, `frontend/src/components/CopyTrading/StrategyBrowser.tsx`, `frontend/src/components/CopyTrading/FollowModal.tsx`, `frontend/src/components/GridBot/GridBotWizard.tsx`, `frontend/src/components/GridBot/ActiveBotsPanel.tsx`

**Implementation:**
1. **Strategy browser**: `GET /api/v1/copy/strategies` — sortable/filterable leaderboard of strategy providers (provider alias, 30/90-day return, max drawdown, Sharpe, AUM, followers, risk class); no PAMM/MAM fee-structure display (deferred per R10). Card or table view toggle.
2. **Follow flow**: `POST /api/v1/copy/follows` — allocation amount, max per-trade copy size, stop-loss on provider drawdown, confirmation modal; unfollow button with position-close-or-keep choice.
3. **Grid bot wizard**: `POST /api/v1/bots/grid` — symbol, upper/lower price bounds, grid count (auto-calculates grid spacing), order type (limit/market), per-grid quantity, total investment, stop-loss, take-profit; live backtest preview over last 7 days; enforces max 5 concurrent bots per account (R13).
4. **Active bots panel**: `GET /api/v1/bots/grid` and `GET /api/v1/bots/grid/{id}` — active bot cards with live P&L, filled grid levels, pause/resume, `DELETE /api/v1/bots/grid/{id}` with close-all-positions confirmation.
5. **Risk warnings**: copy-trading and grid-bot screens display standardized risk disclosure (capital-loss risk, past-performance disclaimer, liquidation risk on leveraged grid).

**Definition of Done (Acceptance Criteria):**
* [x] Strategy browser renders with sort/filter and follow/unfollow flow
* [x] Grid bot wizard with live preview and max-5-concurrent enforcement functional
* [x] Active bot panel shows live P&L and pause/resume/cancel *(closed 2026-09-30: backend live — GET /api/v1/bots/grid serves live PnL + filled levels; pause/resume landed end-to-end (migration 275 PAUSED state, engine Pause/Resume/rearm, POST /bots/grid/{id}/pause|resume routes, panel Pause/Resume mutations wired); cancel via DELETE already live; 9/9 panel tests + 4/4 PG-gated engine ITs green)*
* [x] Risk disclosures displayed on both surfaces
* [x] All strategy/bot screens pass axe-core audit — strategy browser + grid-bots tab + follow modal audited green

**SDD Checklist:**
- [x] Spec checkpoint: copy-trading browser/follow and grid-bot wizard/management UI enforce limits and display risk disclosures (§24 #387) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.27: Order History, Algo Management & OPO Lists

**Objective:** Build the open-orders/history table, algo-order management panel, OPO list viewer, dead-man-switch toggle, and order test/preview surfaces. Added 2026-09-27 (remediation #32). The order-entry panel (Task 10.3.3/10.3.7) covers submit/cancel/modify but no task rendered the history table, algo controls, or OPO lists.

**File Locations:** `frontend/src/pages/trading/OrderHistoryPage.tsx`, `frontend/src/components/Orders/OrderTable.tsx`, `frontend/src/components/Orders/AlgoManagementPanel.tsx`, `frontend/src/components/Orders/OpoListView.tsx`, `frontend/src/components/Orders/DeadManSwitch.tsx`

**Implementation:**
1. **Open orders & history**: `GET /api/v1/orders` — tabbed table (open / all-history) with columns (time, symbol, type, side, qty, price, filled, status, TIF); per-row cancel (`DELETE /api/v1/orders/{id}`), amend (routes to Task 10.3.15 chart-trading or inline edit), and audit-trail link (`GET /api/v1/admin/orders/{id}/audit` for Compliance Officer+). Filters: symbol, side, type, status, date range. Pagination with cursor.
2. **Amendment history**: `GET /api/v1/orders/{id}/amendments` — expandable row showing every cancel-replace and keep-priority amend with diff (old/new price, qty, TIF, timestamp).
3. **Algo management**: `GET /api/v1/algo-orders` — active algo orders (TWAP/VWAP/VP/grid) with progress bars (quantity sliced / total, elapsed time, next-slice ETA); per-algo `POST /api/v1/orders/algo/{id}/pause` and `POST /api/v1/orders/algo/{id}/resume`; `DELETE /api/v1/algo-orders` — mass-cancel algos.
4. **OPO / OCO list viewer**: `GET /api/v1/order-lists` and `GET /api/v1/order-lists/{id}` — list-of-orders view showing parent-child relationships (OPO/OPOCO), working/filled/cancelled status per leg, and list-level cancel. `GET /api/v1/order-lists/history` — completed lists.
5. **Dead-man switch**: `POST /api/v1/orders/countdown-cancel-all` — toggle with configurable timeout (30s–300s), live countdown badge in header, and manual-deactivate button; visual indicator when active.
6. **Order test / preview**: `POST /api/v1/orders/test` — dry-run panel accessible from order entry showing estimated cost, margin impact, execution-rule check, and filter validation without submitting.
7. **Additional order types** in the order panel (extends Task 10.3.7): scaled (`POST /api/v1/orders/scaled`), spread (`POST /api/v1/orders/spread`), roll (`POST /api/v1/orders/roll`) — exposed as advanced-tab entries with their own parameter forms.
8. **WS channel consumption (amends Tasks 10.3.2 and 10.3.4)**: BBO stream `bbo@{symbol}` consumed by order-book component for zero-latency top-of-book highlight; `aggTrades@{symbol}` consumed by charts for trade markers; `liquidations@{symbol}` rendered as a side-feed widget in the trading view; `openInterest@{symbol}` and `referencePrice@{symbol}` rendered in the discovery panel (Task 10.3.17); configurable depth `depth@{symbol}:{levels}:{update_ms}` selectable in order-book settings.

**Definition of Done (Acceptance Criteria):**
* [x] Open-orders and history table with filters, pagination, per-row cancel/amend/audit-link functional
* [x] Amendment-history expandable row shows diff per amend
* [x] Algo management panel shows progress and pause/resume
* [x] OPO/OCO list viewer renders parent-child relationships and list-level cancel
* [x] Dead-man switch toggle with live countdown and deactivate
* [x] Order test/preview panel validates without submitting
* [x] BBO, aggTrades, liquidations, OI, referencePrice, and configurable-depth WS channels consumed by the appropriate components
* [x] All order-history screens pass axe-core audit — history page tabs audited green

**SDD Checklist:**
- [x] Spec checkpoint: order history, algo management, OPO lists, dead-man switch, and test/preview are functional; BBO/aggTrades/liquidations/OI/referencePrice/configurable-depth WS channels are consumed (§24 #388) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.28: Reports, Statements & Public Transparency Downloads

**Objective:** Build the report-download, statement-view, solvency-proof, fee-schedule, and system-info surfaces. Added 2026-09-27 (remediation #32). Phase-20/23 reporting backends existed but the client-facing download screens had no UI owner.

**File Locations:** `frontend/src/pages/reports/ReportsPage.tsx`, `frontend/src/components/Reports/TaxReportDownload.tsx`, `frontend/src/components/Reports/StatementViewer.tsx`, `frontend/src/components/Reports/TcaReport.tsx`, `frontend/src/components/Reports/SolvencyProof.tsx`, `frontend/src/components/Reports/FeeSchedule.tsx`, `frontend/src/components/Reports/SystemInfoPanel.tsx`

**Implementation:**
1. **Tax report**: `GET /api/v1/tax/report?year={YYYY}&method={FIFO|LIFO|HIFO|AVG_COST}` — year selector, method selector, downloadable CSV/PDF, realized-gain/loss summary, per-lot detail table.
2. **Account statements**: `GET /api/v1/account/statements?period=` — period selector (monthly/quarterly/annual), statement viewer (opening balance, trades, fees, funding, transfers, closing balance), downloadable PDF (MT515 per spec §16.6).
3. **Trade confirmations**: `GET /api/v1/account/confirmations/{trade_id}` — per-trade confirmation view (execution details, settlement date, counterparty-protected).
4. **TCA report**: `GET /api/v1/reports/tca/{account_id}?period=monthly&instrument_class=FX_MAJOR` — execution-quality analysis (spread, slippage, fill-rate, venue comparison), period and instrument-class filters, downloadable.
5. **Solvency proof**: `GET /api/v1/solvency/latest` — displays latest Merkle-tree solvency summary (total liabilities, total assets, proof height); `GET /api/v1/solvency/proof?account_id={id}&currency={curr}` — per-account proof viewer with Merkle path verification steps and self-verification instructions.
6. **Fee schedule**: `GET /api/v1/fees` — current fee tiers (maker/taker, spread markup vs raw commission), active promos highlighted; `GET /api/v1/account/commission/{symbol}` — per-symbol commission for the logged-in account's tier.
7. **Account snapshots**: `GET /api/v1/account/snapshots?date=` — daily balance snapshots table, date picker, downloadable.
8. **Income history**: `GET /api/v1/account/income?type=&symbol=&from=&to=` — income breakdown (rebate, funding, commission cashback, liquidation gain/loss) with filters.
9. **System info panel** (public, unauthenticated): `GET /api/v1/exchange-info` (trading rules, filters per symbol), `GET /api/v1/time` (server clock for time-sync), `GET /api/v1/system/status` (degradation mode, shard health, circuit-breaker state), `GET /api/v1/announcements` (banner feed), `GET /api/v1/maintenance/schedule` (upcoming windows), `GET /api/v1/execution-policy` (published policy link). Announcement banners render globally in the SPA shell.
10. **Market performance stats** (extends Task 10.3.17): `GET /api/v1/market/performance` — public aggregate spreads, latency, fill-rate, uptime reconciled to TCA/SLO.

**Definition of Done (Acceptance Criteria):**
* [x] Tax report with year/method selectors and CSV/PDF download functional
* [x] Account statements, trade confirmations, TCA report downloadable/viewable
* [x] Solvency proof viewer renders Merkle summary and per-account verification path *(closed 2026-09-30: SolvencyPanel consumes live `/public/proof-of-reserves/daily-root` (root/totals/tree-height) + `AccountProofSection` consumes `/account/solvency-proof` — per-currency leaf hash, salt, and the leaf→root sibling path with offline-verification instructions; backend live via `reconciliation.SolvencyPgStore` + api.SolvencyLatest/Proof/AccountSolvencyProof; vitest+axe 47/47 green)*
* [x] Fee schedule, account snapshots, income history tables functional
* [x] System-info panel shows exchange-info, time, status, announcements, maintenance, execution-policy
* [x] Announcement banners render globally in SPA shell
* [x] All report screens pass axe-core audit — ReportsPage tabs audited green

**SDD Checklist:**
- [x] Spec checkpoint: report-download, statement, solvency-proof, fee-schedule, and system-info surfaces are functional; announcement banners render globally (§24 #389) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

### Task 10.3.29: Centralized Input Helper Framework

**Objective:** Build the shared input-assistance layer that spans every UI surface in Phase-10 — order entry, transfers, withdrawals, KYC, support, grid bots, reports, and admin forms. Added 2026-09-27 (remediation #33). Prior to this task, input handling was distributed across 28 tasks with no shared module: validation was inline per-form, formatting was ad-hoc, preview/estimation was reimplemented per surface, and keyboard shortcuts, autocomplete, bulk import, and unit conversion were absent entirely.

**File Locations:** `frontend/src/lib/input/` (shared module), `frontend/src/lib/input/validation.ts`, `frontend/src/lib/input/format.ts`, `frontend/src/lib/input/preview.ts`, `frontend/src/lib/input/defaults.ts`, `frontend/src/lib/input/autocomplete.ts`, `frontend/src/lib/input/shortcuts.ts`, `frontend/src/lib/input/help.ts`, `frontend/src/lib/input/confirm.ts`, `frontend/src/lib/input/bulk.ts`, `frontend/src/lib/input/convert.ts`, `frontend/src/hooks/useInputHelper.ts`

**Implementation:**

1. **OpenAPI-synced validation schemas** (`validation.ts`):
   - Fetch the OpenAPI 3.1 spec from `GET /api/v1/routes` (Phase-05 Task 5.3.7/5.3.8) at build time and generate TypeScript Zod schemas per route. Client-side validation mirrors server-side structural validation (required fields, types, ranges, enums, regex patterns) exactly — a field that passes client validation will not fail with `INVALID_REQUEST` on the server (barring business-rule rejections from the C++ core / domain services).
   - Per-field error messages are human-readable, localized to en-US (no i18n per R5), and rendered inline below the field with `aria-invalid` and `aria-describedby` for screen readers.
   - `useInputHelper(route: string)` hook returns `{ schema, validate, errors, isValid }` for any form bound to a registered route; forms in Tasks 10.3.3, 10.3.7, 10.3.21–10.3.28 consume this hook instead of inline rules.

2. **Instrument-aware input formatting** (`format.ts`):
   - Consumes `GET /api/v1/instruments` and `GET /api/v1/exchange-info` to load per-instrument metadata: `tick_size`, `pip_size`, `lot_size`, `min_order_qty`, `min_notional`, `price_precision`, `qty_precision`, `value_date`, `settlement_type`.
   - **Price formatting**: prices rendered and input-constrained to `tick_size` increments; JPY pairs show 3 decimals, standard majors 5 decimals; input rejects keystrokes that would produce sub-tick prices.
   - **Quantity formatting**: lot-rounded to `lot_size` increments; shows units (lots, units, or base currency) per instrument convention; `min_notional` check before submit.
   - **Currency formatting**: all monetary values formatted with proper decimal places and currency code (ISO 4217); account-currency conversion for display.
   - `formatPrice(symbol, rawPrice)`, `formatQty(symbol, rawQty)`, `formatCurrency(code, amount)` exported for all components.

3. **Real-time preview engine** (`preview.ts`):
   - Unified abstraction for any operation that involves money: orders, transfers, withdrawals, grid-bot investment, copy-trading allocation.
   - **Order preview**: wraps `POST /api/v1/orders/test` (Phase-05 Task 5.3.39) — shows estimated cost, margin impact, commission, spread, swap, execution-rule check, and filter validation in a live panel beside the order entry form, updating on every field change (debounced 150ms).
   - **Transfer/withdrawal preview**: wraps `POST /api/v1/funding/fee-estimate` — shows rail fee, arrival estimate, cut-off, and resulting balance.
   - **Grid-bot preview**: wraps the live 7-day backtest (Task 10.3.26) — shows projected P&L range and max drawdown.
   - All previews render in a shared `<PreviewPanel>` component; the preview result includes a `risk_level` field (LOW/MEDIUM/HIGH) that drives the confirmation-modal severity colour.

4. **Smart defaults service** (`defaults.ts`):
   - Context-aware defaults computed from live account and market state:
     - **Limit order**: default price = current BBO bid (for SELL) / ask (for BUY), tick-rounded.
     - **Market order**: default quantity = max affordable from free margin at current mark × leverage (lot-rounded down).
     - **Stop-loss**: default distance = instrument's typical daily range (from 20-day ATR, pre-computed by Phase-23 analytics).
     - **Beneficiary**: last-used beneficiary per currency for withdrawals.
     - **Transfer**: default `from_account_id` = currently selected sub-account; `to_account_id` = master if on sub-account.
   - Defaults are non-binding pre-fills — the user always sees and can override them. They are recomputed when the market context changes (symbol switch, mark-price move >0.1%).

5. **Autocomplete & suggestion system** (`autocomplete.ts`):
   - **Symbol search**: type-ahead dropdown over `GET /api/v1/instruments` with fuzzy match on symbol, base/quote currency, and display name; shows pair category (major/minor/exotic), pip value, and current spread.
   - **Beneficiary lookup**: type-ahead over the beneficiary registry (Phase-11 Task 11.3.7) by bank name or IBAN fragment.
   - **Amount presets**: quick-select buttons (25%/50%/75%/100% of available balance) for withdrawals and transfers — extends Task 10.3.11 sliders to all monetary-input forms.
   - All autocomplete widgets use `role="combobox"` with `aria-expanded`, `aria-activedescendant`, and full keyboard arrow-key navigation per WAI-ARIA Authoring Practices.

6. **Keyboard shortcut manager** (`shortcuts.ts`):
   - Global keyboard shortcut registry with scope-aware dispatch (trading scope, admin scope, funding scope):
     - `Ctrl+Enter` — submit current form (order entry, transfer, withdrawal, support ticket)
     - `Esc` — cancel/close current modal or clear order entry
     - `Ctrl+D` — dead-man switch toggle
     - `Ctrl+B` — buy (focus order entry, pre-set side BUY)
     - `Ctrl+S` — sell (focus order entry, pre-set side SELL)
     - `Ctrl+W` — close all positions (with confirmation)
     - `1–9` — switch workspace panel focus (extends Task 10.3.14 workspace)
     - `/` — focus symbol search
     - `?` — show keyboard-shortcut help overlay
   - Shortcuts are discoverable via the `?` overlay and are configurable in workspace preferences (migration 076). LITE mode exposes only `Ctrl+Enter`, `Esc`, and `/`.
   - All shortcuts respect `aria-keyshortcuts` for screen readers.

7. **Contextual help & tooltip system** (`help.ts`):
   - Field-level help text registered per form schema — each field carries `helpText`, `example`, and optional `glossaryLink` to a `/help#glossary` page with FX-specific term definitions (pip, spread, swap, NDF, value date, etc.).
   - Tooltips render via a shared `<HelpTooltip>` component (`aria-describedby`, focusable, dismissible with Esc).
   - Contextual help is **not** a chatbot or AI — it is a static registry curated by the compliance team, so it never gives trading advice (per R6 no-phone-desk ruling extension: no advisory surfaces).

8. **Unified confirmation & risk-warning framework** (`confirm.ts`):
   - Shared `<ConfirmModal>` component with severity levels:
     - **LOW** (green): standard confirmation (e.g., session revoke, support-ticket close).
     - **MEDIUM** (amber): financial-impact confirmation (e.g., limit order, internal transfer, grid-bot start).
     - **HIGH** (red): irreversible / high-risk confirmation (e.g., withdrawal to new beneficiary, emergency freeze, account closure, close-all positions, `POST /api/v1/orders/test` → real submit when preview shows HIGH risk).
   - HIGH-severity modals require typing a confirmation phrase (e.g., "CLOSE ALL" for close-all) or 2FA, not just a click — the text is defined per action in a registry.
   - Risk disclosures (capital-loss, leverage, liquidation) are injected into the modal from the same registry, not hardcoded per component.
   - Dual-control-aware: when the action requires 4-eyes (per Phase-07 Task 7.3.2), the modal shows the pending-approval state and the second-approver flow.

9. **Bulk / paste input helpers** (`bulk.ts`):
   - **CSV order import**: paste or upload a CSV (`symbol,side,type,qty,price,tif`) to batch-submit via `POST /api/v1/orders/batch` (Phase-05 Task 5.3.32, max 10 submit / 20 cancel). Shows a pre-submit table with per-row validation (green/amber/red) and estimated total cost; rows that fail validation are excluded and highlighted.
   - **Beneficiary list paste**: paste multi-line text to bulk-add beneficiaries (Phase-11 Task 11.3.7) with per-row IBAN/bank-code validation.
   - **Scaled order paste**: paste a price/quantity ladder for scaled orders (`POST /api/v1/orders/scaled`).
   - All bulk imports show a summary count, per-row status, and total notional before the user confirms.

10. **Unit converter service** (`convert.ts`):
    - **Pip ↔ price**: `pipToPrice(symbol, pips)` and `priceToPip(symbol, price)` using instrument `pip_size`.
    - **Lots ↔ units**: `lotsToUnits(symbol, lots)` and `unitsToLots(symbol, units)` using instrument `lot_size`.
    - **Base ↔ quote**: `baseToQuote(symbol, baseAmount, rate)` and `quoteToBase(symbol, quoteAmount, rate)` using live mark price.
    - **Account-currency conversion**: `convertToAccountCurrency(amount, fromCurrency)` using the daily P&L-conversion rate (Phase-03 Task 3.3.9).
    - **Percentage ↔ absolute**: `pctToQty(symbol, pct, freeMargin, leverage, price)` and `qtyToPct(symbol, qty, freeMargin, leverage, price)` — extends Task 10.3.11 sliders to any monetary input.
    - All converters are pure functions, testable in isolation, and consumed by the calculator widget (Task 10.3.8), percentage sliders (Task 10.3.11), and order preview (item 3 above).

**Integration with existing tasks (amendments):**
- Tasks 10.3.3, 10.3.7, 10.3.21–10.3.28 replace their inline per-form validation with `useInputHelper(route)` (item 1).
- Task 10.3.3 order entry and Task 10.3.7 advanced panel consume `formatPrice`/`formatQty` (item 2) and the preview engine (item 3).
- Task 10.3.11 percentage sliders delegate to `pctToQty` (item 10) instead of inline math.
- Task 10.3.10 quick actions and all confirmation modals migrate to `<ConfirmModal>` (item 8).
- Task 10.3.14 workspace keyboard nav extends to the shortcut manager (item 6).
- Task 10.3.17 symbol search uses the autocomplete system (item 5).
- Task 10.3.23 withdrawal/transfer fee estimate uses the preview engine (item 3) and amount presets (item 5).
- Task 10.3.26 grid-bot wizard uses the preview engine (item 3) and `ConfirmModal` HIGH-severity (item 8).
- Task 10.3.27 order test/preview delegates to the preview engine (item 3); batch orders use bulk CSV import (item 9).

**Definition of Done (Acceptance Criteria):**
* [x] `useInputHelper` hook generates Zod schemas from OpenAPI spec for all registered routes; client-side validation failures never reach the server as `INVALID_REQUEST`
* [x] `formatPrice`/`formatQty`/`formatCurrency` render instrument-aware values across all trading forms
* [x] Preview panel shows live cost/margin/fee estimate for orders, transfers, withdrawals, and grid bots
* [x] Smart defaults pre-fill order entry, beneficiaries, and transfer accounts
* [x] Symbol-search and beneficiary autocomplete with WAI-ARIA combobox pattern
* [x] Keyboard shortcuts registered with `?` overlay, configurable in workspace prefs, LITE-mode limited
* [x] Contextual help tooltips with glossary links on all form fields
* [x] `<ConfirmModal>` with LOW/MEDIUM/HIGH severity used across all destructive actions; HIGH requires typed phrase or 2FA
* [x] CSV order import and beneficiary paste with per-row validation and pre-submit summary
* [x] Unit converters (pip↔price, lots↔units, base↔quote, pct↔absolute) consumed by calculators, sliders, and preview
* [x] All existing inline validation in Tasks 10.3.3/10.3.7/10.3.21–10.3.28 refactored to use the shared framework *(verified: order-entry `validateDraft`, auth (Login/Register/ForgotPassword/SessionList), funding (FeeEstimator/Transfer/Withdrawal), settings (ApiKeys/Security/TotpEnrollment) all delegate to ROUTE_FIELD_BINDINGS + generated contracts; 173/173 scoped vitest pass, tsc+eslint clean)*
* [x] Input-helper framework passes axe-core audit (WCAG 2.1 AA per Task 10.3.14) — field/autocomplete/preset/help/preview/unavailable fixtures + ConfirmModal HIGH + ShortcutHelpOverlay + GlossaryList audited green

**SDD Checklist:**
- [x] Spec checkpoint: centralized input-helper framework with OpenAPI-synced validation, instrument-aware formatting, preview engine, smart defaults, autocomplete, keyboard shortcuts, contextual help, unified confirmation modals, bulk import, and unit converters shared across all UI surfaces (§24 #390) — defined first, validated against spec
- [x] All spec checkpoints pass after implementation

---

## 10.4 Deliverables

- React 18 + TypeScript trading UI
- Virtualized order book
- Order entry form
- TradingView charts
- Positions & balances
- Admin dashboard
- Advanced order types panel + sub-account switcher (Task 10.3.7)
- Calculators, Lite/Pro, quick actions, percentage sizing, depth chart, and ADL display
- Frontend engineering baseline (Task 10.3.1): strict TypeScript, ESLint + Prettier, Vitest + React Testing Library + Playwright smoke, ≤300 kB gzipped initial bundle budget, SPA shell CSP / subresource integrity / frame-busting
- WCAG 2.1 AA compliance with CI axe-core audit (Task 10.3.14)
- Custom layouts, chart trading, indicators/backtesting, FX discovery, watchlists/alerts, and performance dashboard
- Network disconnect overlay, stale pricing indicator & optimistic rollback UI, and the normative WS client state machine (Task 10.3.19)
- Environment switcher, fleet/release pages & ops-board UI with dual-control surfacing (Task 10.3.20)
- Auth, registration, 2FA & session screens (Task 10.3.21, added 2026-09-27 remediation #32)
- Account settings & security center (Task 10.3.22, remediation #32)
- Funding & transfers screens (Task 10.3.23, remediation #32)
- KYC submission & status tracker (Task 10.3.24, remediation #32)
- Support tickets & help center (Task 10.3.25, remediation #32)
- Copy trading & grid bot management UI (Task 10.3.26, remediation #32)
- Order history, algo management, OPO lists & dead-man switch (Task 10.3.27, remediation #32)
- Reports, statements & public transparency downloads (Task 10.3.28, remediation #32)
- Centralized input-helper framework: OpenAPI-synced validation, instrument-aware formatting, preview engine, smart defaults, autocomplete, keyboard shortcuts, contextual help, unified confirmation modals, bulk import, unit converters (Task 10.3.29, remediation #33)

---

## 10.5 Dependencies

**Hard:** Phases 3, 5, 6

**Soft forward references** (amended 2026-09-25 — remediation #16). These were consumed by Phase-10 tasks without being declared here. All are built against mocks/stubs in Phase 10 and wired to the real backend in their owning phase, exactly as Task 10.3.6 already does:

| Consuming task | Owning phase | Delivered by |
|---|---|---|
| 10.3.8 calculators | Phase-03 Task 3.3.11/3.3.12 (swap rates, pip value) | Phase 3 |
| 10.3.10 close-all | Phase-05 Task 5.3.36 (`POST /api/v1/positions/close-all`) | Phase 5 |
| 10.3.17 rate alerts | Phase-12 Task 12.3.6 (notification preferences) | Phase 12 |
| 10.3.13 `adl_indicator` | Phase-19 Task 19.3.19 | Phase 19 |
| 10.3.18 performance dashboard | Phase-20 Task 20.3.8/20.3.6 (reporting portal, statements) | Phase 20 |
| 10.3.16 backtesting | Phase-23 Task 23.3.4/23.3.7 (historical tick + block tape) | Phase 23 |
| 10.3.6 instruments / KYC / breakers | Phase 15 / 14 / 13 (see forward-reference note on 10.3.6) | Phase 13–15 |
| 10.3.20 fleet/promotion/ops-board | Phase-09 Task 9.3.30 (fleet backend) / Phase-15 Task 15.3.12 (ops console) | Phase 9, Phase 15 |
| 10.3.21 auth/2FA/sessions | Phase-05 Task 5.3.1 (auth backend) / Phase-12 Task 12.3.2 (2FA) / Task 5.3.10 (sessions) | Phase 5, Phase 12 |
| 10.3.22 account security/WebAuthn/GDPR | Phase-12 Tasks 12.3.7–12.3.10 (WebAuthn, anti-phishing, devices, freeze) / Phase-14 Task 14.3.11 (cooling-off) / Phase-21 (GDPR) | Phase 12, Phase 14, Phase 21 |
| 10.3.23 funding/transfers | Phase-11 Tasks 11.3.1–11.3.9 (banking rails, fees, beneficiary registry) / Phase-05 Task 5.3.23 (transfers) | Phase 11, Phase 5 |
| 10.3.24 KYC | Phase-14 Task 14.3.4 (KYC review) / Phase-12 Task 12.3.13 (KYC matrix) | Phase 14, Phase 12 |
| 10.3.25 support | Phase-07 Task 7.3.7 (support tickets) | Phase 7 |
| 10.3.26 copy-trading/grid bots | Phase-14 Task 14.3.14 (copy layer) / Phase-16 Task 16.3.19 (grid engine) | Phase 14, Phase 16 |
| 10.3.27 order history/algo/OPO | Phase-05 Tasks 5.3.3/5.3.33/5.3.37 (orders, dead-man, cancel-replace) / Phase-16 (algo orders) / Phase-06 (BBO/aggTrades/liquidations/OI/depth WS) | Phase 5, Phase 6, Phase 16 |
| 10.3.28 reports/statements/solvency | Phase-20 Tasks 20.3.6–20.3.16 (statements, TCA, tax, costs) / Phase-13 Task 13.3.7 (solvency) / Phase-23 Task 23.3.11 (public stats) | Phase 13, Phase 20, Phase 23 |
| 10.3.29 input-helper framework | Phase-05 Tasks 5.3.7/5.3.8 (OpenAPI route registry + spec) / Phase-05 Task 5.3.39 (order preview) / Phase-11 Task 11.3.7 (beneficiary registry) / Phase-05 Task 5.3.32 (batch orders) | Phase 5, Phase 11 |

---

## 10.6 Duration Estimate

**31–39 days (35 days nominal).** Supersedes prior 30–38 (Task 10.3.29 input-helper framework added 2026-09-27, remediation #33); prior supersedes 20–26 (Tasks 10.3.21–10.3.28 added 2026-09-27, remediation #32); prior supersedes 19–24 (Task 10.3.20 env/fleet/ops-board UI added 2026-09-27, remediation #26); prior supersedes "5–7 days" (this section) and "7–9 days" (AGENTS.md). The prior breakdown covered only 7 of the phase's 19 tasks and omitted Tasks 10.3.7–10.3.18 entirely (≈15 days of work added by remediations #11–#14), which understated the phase by roughly 12 days.

| Task | Scope | Days |
|---|---|---|
| 10.3.1 | Scaffold + tooling, bundle-budget and SPA-security baseline | 0.75 |
| 10.3.2 | Virtualized order book | 1.0 |
| 10.3.3 | Order entry form | 1.0 |
| 10.3.4 | Charts (TradingView Lightweight Charts) | 1.0 |
| 10.3.5 | Positions & balances | 1.0 |
| 10.3.6 | Admin dashboard (RBAC, instruments, users, audit, health) | 2.0 |
| 10.3.7 | Advanced order panel + sub-account switcher + fee-tier screen | 1.0 |
| 10.3.8 | Calculators (PnL, pip, margin, liquidation, swap) | 1.0 |
| 10.3.9 | Lite/Pro UI mode toggle | 0.5 |
| 10.3.10 | One-click quick actions (close-all, reverse, flatten) | 0.5 |
| 10.3.11 | Percentage order quantity sliders | 0.5 |
| 10.3.12 | Interactive depth chart | 0.75 |
| 10.3.13 | ADL priority indicator | 0.5 |
| 10.3.14 | Customizable workspace + WCAG 2.1 AA baseline | 1.5 |
| 10.3.15 | Chart trading & order overlays | 1.5 |
| 10.3.16 | Indicators + sandboxed cost-aware backtesting | 2.0 |
| 10.3.17 | FX discovery, watchlists, rate alerts | 1.5 |
| 10.3.18 | Client performance dashboard | 1.5 |
| 10.3.19 | Network resilience, WS state machine, optimistic rollback | 1.0 |
| 10.3.20 | Environment switcher, fleet/release pages & ops-board UI | 1.5 |
| 10.3.21 | Auth, registration, 2FA & session screens | 1.5 |
| 10.3.22 | Account settings & security center (profile, WebAuthn, anti-phishing, GDPR, freeze, cooling-off, closure) | 2.0 |
| 10.3.23 | Funding & transfers screens (deposits, withdrawals, transfers, fee estimator) | 1.5 |
| 10.3.24 | KYC submission & status tracker | 1.0 |
| 10.3.25 | Support tickets & help center | 0.5 |
| 10.3.26 | Copy trading & grid bot management UI | 1.5 |
| 10.3.27 | Order history, algo management, OPO lists, dead-man switch, test/preview + WS channel consumption | 2.0 |
| 10.3.28 | Reports, statements, solvency proof, fee schedule, system info | 1.5 |
| 10.3.29 | Centralized input-helper framework (validation, formatting, preview, defaults, autocomplete, shortcuts, help, confirm modals, bulk import, converters) | 1.0 |
| — | Cross-cutting QA (unit + e2e smoke + axe audit) | 2.5 |
| | **Total** | **35.0** |

Critical path is 10.3.1 → 10.3.29 (input framework must land early so all forms consume it) → 10.3.2/10.3.3/10.3.4 → 10.3.5 → 10.3.19 → 10.3.21 → 10.3.28 → QA. With 2 frontend engineers the wall-clock compresses to ~18–19 days, since 10.3.6/10.3.14/10.3.15/10.3.16 run in parallel with 10.3.7–10.3.13 and 10.3.22–10.3.28 run in parallel with 10.3.17–10.3.20. The input-helper framework (10.3.29) is 1.0 day of net-new code but saves ~0.5 day per consuming task by replacing inline validation, so it is net-neutral on the critical path. The earlier 7–9 day figure is only achievable if Tasks 10.3.6, 10.3.14–10.3.18, 10.3.21–10.3.28, and 10.3.29 are deferred to a Phase 10b; that deferral is **not** approved in this plan.

---

## 10.7 Acceptance Criteria

| # | Criterion |
|---|-----------|
| 1 | Vite + React 18 + TypeScript project builds with strict TS, ESLint/Prettier, Vitest + Testing Library, Playwright smoke, and an enforced ≤300 kB gzipped initial bundle budget |
| 2 | Zustand + TanStack Query configured |
| 3 | WS client hook with reconnect + last_seq |
| 4 | Virtualized order book renders 20 levels per side |
| 5 | Real-time WS updates with no jitter |
| 6 | Spread + depth bars displayed |
| 7 | Order entry form validates all fields |
| 8 | Order submit reaches gateway API |
| 9 | Click on book level pre-fills price |
| 10 | Candlestick chart with volume renders |
| 11 | Real-time chart updates from WS |
| 12 | Multiple timeframes (1m–1d) switchable |
| 13 | Balances display all currencies with available/locked/total |
| 14 | Positions display with unrealized P&L |
| 15 | Real-time position/balance updates via WS |
| 16 | Close position button works |
| 17 | Admin dashboard RBAC-gated |
| 18 | Instrument management UI (create, suspend, halt, resume, delist) |
| 19 | User management UI (freeze, unfreeze, KYC review) |
| 20 | Audit log viewer |
| 21 | System health dashboard (degradation, breakers, shards) |
| 22 | Advanced order panel: all TIF modes, iceberg, trailing stop, bracket; sub-account switcher with balances; admin fee tier assignment (§24 #251) |
| 23 | Position/margin/pip/liquidation/swap calculators use live mark and account leverage (§24 #267) |
| 24 | Lite/Pro modes, confirmed quick actions, percentage sizing, and interactive depth chart are functional (§24 #268) |
| 25 | ADL rank is visible per position with explanatory risk tooltip (§24 #269) |
| 26 | Users can save/reset accessible customizable workspace layouts and themes, meeting WCAG 2.1 AA with a CI axe-core audit over trade, positions, admin and calculator routes (§24 #292) |
| 27 | Chart shows draggable open orders, fill markers, position/stop/target overlays, and candle countdown with safe amendment semantics (§24 #292) |
| 28 | Built-in indicators and cost-aware sandboxed backtests are reproducible and cannot place live orders (§24 #293) |
| 29 | FX top movers, heatmaps, watchlists, and rate alerts consume canonical market data (§24 #294) |
| 30 | Equity/P&L/drawdown/cost dashboard reconciles to Phase-20 ledger-backed reporting (§24 #295) |
| 31 | UI renders DISCONNECTED banner and disables order entry on socket drop; rolls back optimistic state on rejection; warns STALE_PRICING on >3s silence; reconnects on the spec §21.3 schedule (100ms→2s then 10s cap, ±20% jitter) with the normative state machine staying under the Phase-06 10/min/IP throttle and inside the 60s replay window (§24 #310) |
| 32 | Environment pill with prod confirm and context-bound API client; fleet/release/ops-board pages with dual-control surfacing; new routes axe-audited and RBAC-gated (§24 #351) |
| 33 | Login, register, 2FA setup/verify/disable, and session-list screens functional; route guards redirect unauthenticated users; silent token refresh until refresh fails (§24 #382) |
| 34 | Account-security center covers profile, WebAuthn/passkeys, anti-phishing code, device management, emergency freeze, cooling-off, account closure, GDPR export/erase, consent, and notification preferences with confirmation modals on destructive actions (§24 #383) |
| 35 | Deposit instructions render per-currency bank details; withdrawal form with beneficiary selector, 2FA, and 15-min confirm window; internal transfers between owned accounts; fee estimator shows rail fee, arrival estimate, and cut-off (§24 #384) |
| 36 | KYC status tracker displays tier/state/per-document status and trading-limit impact; upload wizard handles multi-step document submission with file validation; re-verification prompts on expiry (§24 #385) |
| 37 | Support-ticket submission with attachments and priority gating; conversation thread with status lifecycle; staff-side queue renders for Support Agent role (§24 #386) |
| 38 | Copy-trading strategy browser/follow and grid-bot wizard/management UI enforce max-5-concurrent bots and display risk disclosures (§24 #387) |
| 39 | Order history table with filters and per-row cancel/amend/audit-link; algo management with pause/resume; OPO/OCO list viewer; dead-man switch with live countdown; test/preview validates without submitting; BBO/aggTrades/liquidations/OI/referencePrice/configurable-depth WS channels consumed (§24 #388) |
| 40 | Tax report, account statements, trade confirmations, TCA report, solvency proof viewer, fee schedule, account snapshots, income history, and system-info panel functional; announcement banners render globally in SPA shell (§24 #389) |
| 41 | Centralized input-helper framework: OpenAPI-synced validation schemas, instrument-aware formatting, real-time preview engine, smart defaults, autocomplete, keyboard shortcuts, contextual help, unified confirmation/risk-warning modals, bulk/paste import, and unit converters are shared across all UI surfaces (§24 #390) |
