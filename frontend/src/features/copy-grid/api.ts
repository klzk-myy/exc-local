/**
 * Copy-trading & grid-bot adapters (Task 10.3.26).
 *
 * Routes are live in the gateway (Phase-14 Task 14.3.8/14.3.14 and
 * Phase-16 Task 16.3.19). The adapters narrow `unknown` wire payloads
 * honestly; surfaces still render `UnavailablePanel` on 5xx degradation
 * and never synthesize strategies/bots/fills.
 */
import type { ApiClient } from '@/lib/api';

import { tryDec } from '@/lib/decimal/decimal';

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}
function str(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}
function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

// ---------------------------------------------------------------------------
// Copy trading — GET /copy/strategies, POST /copy/follows
// ---------------------------------------------------------------------------

export interface CopyStrategy {
  id: string;
  alias: string;
  return30d: string | null;
  return90d: string | null;
  maxDrawdown: string | null;
  sharpe: string | null;
  aum: string | null;
  followers: number | null;
  riskClass: string | null;
}

export function parseStrategy(v: unknown): CopyStrategy | null {
  if (!isRecord(v)) return null;
  const id =
    str(v['strategy_id']) ?? str(v['id']) ?? (num(v['id']) !== undefined ? String(v['id']) : null);
  const alias = str(v['alias']) ?? str(v['provider_alias']);
  if (!id || !alias) return null;
  const pct = (k: string) => {
    const s = str(v[k]);
    return s ?? tryDec(v[k])?.toString() ?? null;
  };
  return {
    id,
    alias,
    return30d: pct('return_30d_pct') ?? pct('return30d'),
    return90d: pct('return_90d_pct') ?? pct('return90d'),
    maxDrawdown: pct('max_drawdown_pct') ?? pct('max_drawdown'),
    sharpe: pct('sharpe'),
    aum: pct('aum') ?? pct('aum_quote'),
    followers: num(v['followers']) ?? num(v['follower_count']) ?? null,
    riskClass: str(v['risk_class']) ?? null,
  };
}

/** GET /api/v1/copy/strategies — tolerant envelope unwrap ({data:[]}|[]). */
export async function listStrategies(api: ApiClient): Promise<CopyStrategy[]> {
  const res = await api.get<unknown>('/copy/strategies');
  const rows = isRecord(res) ? res['data'] : res;
  if (!Array.isArray(rows)) return [];
  return rows.map(parseStrategy).filter((s): s is CopyStrategy => s !== null);
}

export interface FollowRequest {
  strategy_id: string;
  allocation: string;
  max_copy_size?: string;
  stop_loss_drawdown_pct?: string;
  safety_mode?: string;
}

export function followStrategy(api: ApiClient, req: FollowRequest): Promise<unknown> {
  return api.post('/copy/follows', req, { idempotent: true });
}

/** One row of GET /api/v1/copy/follows — the caller's follow with the
 * strategy display fields joined (Task-8 read surface). */
export interface MyFollow {
  follow_id: number;
  strategy_id: number;
  strategy_name: string;
  strategy_status: string;
  allocation_notional: string;
  currency: string;
  safety_mode: string;
  stop_loss_cap?: string;
  status: string;
  unfollowed_at?: string;
  created_at: string;
}

export async function listMyFollows(api: ApiClient): Promise<MyFollow[]> {
  const res = await api.get<{ follows?: MyFollow[] }>('/copy/follows');
  return Array.isArray(res.follows) ? res.follows : [];
}

/** Unfollow — DELETE /api/v1/copy/follows/{id} (live, Phase-14 Task
 * 14.3.14). Pending copied orders are cancelled; open positions stay. */
export function unfollow(api: ApiClient, followId: number): Promise<unknown> {
  return api.delete(`/copy/follows/${followId}`);
}

// ---------------------------------------------------------------------------
// Grid bots — GET/POST /bots/grid, GET/DELETE /bots/grid/{id}
// ---------------------------------------------------------------------------

export interface GridBot {
  id: string;
  symbol: string;
  status: string;
  lowerPrice: string | null;
  upperPrice: string | null;
  gridCount: number | null;
  totalInvestment: string | null;
  pnl: string | null;
  filledLevels: number | null;
  createdAt: string | null;
}

export function parseGridBot(v: unknown): GridBot | null {
  if (!isRecord(v)) return null;
  const id =
    str(v['bot_id']) ?? str(v['id']) ?? (num(v['id']) !== undefined ? String(v['id']) : null);
  const symbol = str(v['symbol']);
  if (!id || !symbol) return null;
  const d = (k: string) => str(v[k]) ?? tryDec(v[k])?.toString() ?? null;
  return {
    id,
    symbol,
    status: str(v['status']) ?? 'UNKNOWN',
    lowerPrice: d('lower_price'),
    upperPrice: d('upper_price'),
    gridCount: num(v['grid_count']) ?? null,
    totalInvestment: d('total_investment'),
    pnl: d('pnl') ?? d('realized_pnl'),
    filledLevels: num(v['filled_levels']) ?? null,
    createdAt: str(v['created_at']) ?? null,
  };
}

export async function listGridBots(api: ApiClient): Promise<GridBot[]> {
  const res = await api.get<unknown>('/bots/grid');
  const rows = isRecord(res) ? res['data'] : res;
  if (!Array.isArray(rows)) return [];
  return rows.map(parseGridBot).filter((b): b is GridBot => b !== null);
}

export async function getGridBot(api: ApiClient, id: string): Promise<GridBot | null> {
  return parseGridBot(await api.get(`/bots/grid/${encodeURIComponent(id)}`));
}

export interface GridBotCreateRequest {
  symbol: string;
  lower_price: string;
  upper_price: string;
  grid_count: number;
  order_type: string;
  per_grid_qty?: string;
  total_investment: string;
  stop_loss?: string;
  take_profit?: string;
}

export function createGridBot(api: ApiClient, req: GridBotCreateRequest): Promise<unknown> {
  return api.post('/bots/grid', req, { idempotent: true });
}

/** Stop/delete — registered route; `close_positions` is the
 * close-or-keep choice the task mandates on the confirm path. */
export function deleteGridBot(
  api: ApiClient,
  id: string,
  closePositions: boolean,
): Promise<unknown> {
  return api.delete(`/bots/grid/${encodeURIComponent(id)}`, {
    body: { close_positions: closePositions },
  });
}

/** Pause/resume routes are registered and live (Phase-10 Task 10.3.26
 * over the Phase-16 Task 16.3.19 engine): pause freezes the bot while
 * its working children stay on the book; resume re-arms suspended legs. */
export const GRID_BOT_CONTROL_ROUTES = {
  pause: 'POST /api/v1/bots/grid/{id}/pause',
  resume: 'POST /api/v1/bots/grid/{id}/resume',
} as const;
export function gridBotControlRegistered(): boolean {
  return true;
}

/** POST /api/v1/bots/grid/{id}/pause — freeze a RUNNING bot. */
export function pauseGridBot(api: ApiClient, id: string): Promise<unknown> {
  return api.post(`/bots/grid/${encodeURIComponent(id)}/pause`, {});
}

/** POST /api/v1/bots/grid/{id}/resume — re-arm a PAUSED bot. */
export function resumeGridBot(api: ApiClient, id: string): Promise<unknown> {
  return api.post(`/bots/grid/${encodeURIComponent(id)}/resume`, {});
}

// ---------------------------------------------------------------------------
// Strategy engine — GET/POST /strategies[/{id}], pause/resume/DELETE
// (Phase-16 Task 16.3.21)
// ---------------------------------------------------------------------------

export interface Strategy {
  strategy_id: number;
  kind: string; // DCA | REBALANCE | …
  label?: string;
  from_currency?: string;
  to_currency?: string;
  amount?: string;
  schedule?: string; // DAILY | WEEKLY | MONTHLY
  targets?: Record<string, string>;
  drift_band_pct?: string;
  template_id?: number;
  status: string;
  next_run_at?: string;
  last_run_at?: string;
  realized_pnl?: string;
  total_fees?: string;
  run_count?: number;
  created_at?: string;
}

export interface StrategyRun {
  run_id: number;
  scheduled_for?: string;
  kind?: string;
  status: string;
  skip_reason?: string;
  order_ids?: number[];
  legs?: {
    currency?: string;
    action?: string;
    symbol?: string;
    amount?: string;
    status?: string;
    note?: string;
  }[];
  notional?: string;
  currency?: string;
  fees?: string;
}

export interface StrategyDetail {
  strategy: Strategy;
  runs?: StrategyRun[];
}

export const STRATEGY_KINDS = ['DCA', 'REBALANCE'] as const;
export const STRATEGY_SCHEDULES = ['DAILY', 'WEEKLY', 'MONTHLY'] as const;

export async function listMyStrategies(api: ApiClient): Promise<Strategy[]> {
  const res = await api.get<{ strategies?: Strategy[] }>('/strategies');
  return res.strategies ?? [];
}

export async function strategyDetail(api: ApiClient, id: number): Promise<StrategyDetail | null> {
  const res = await api.get<StrategyDetail | Strategy>(`/strategies/${id}`);
  if ('strategy' in res) return res;
  return { strategy: res };
}

export interface StrategyCreateInput {
  kind: string;
  label?: string;
  from_currency?: string;
  to_currency?: string;
  amount?: string;
  schedule?: string;
  targets?: Record<string, string>;
  drift_band_pct?: string;
}

export function createStrategy(api: ApiClient, input: StrategyCreateInput): Promise<Strategy> {
  return api.post<Strategy>('/strategies', input);
}

export function pauseStrategy(api: ApiClient, id: number): Promise<Strategy> {
  return api.post<Strategy>(`/strategies/${id}/pause`, {});
}

export function resumeStrategy(api: ApiClient, id: number): Promise<Strategy> {
  return api.post<Strategy>(`/strategies/${id}/resume`, {});
}

/** DELETE /strategies/{id} — cancels the strategy and unwinds any open run. */
export function cancelStrategy(api: ApiClient, id: number): Promise<Strategy> {
  return api.delete<Strategy>(`/strategies/${id}`);
}

// ---------------------------------------------------------------------------
// Strategy templates — approved marketplace list, publish, instantiate
// ---------------------------------------------------------------------------

export interface StrategyTemplate {
  template_id: number;
  name: string;
  description: string;
  kind: string;
  config?: unknown;
  status: string;
  publisher_account_id?: number;
  reject_reason?: string;
  created_at?: string;
}

export async function listTemplates(api: ApiClient): Promise<StrategyTemplate[]> {
  const res = await api.get<{ templates?: StrategyTemplate[] }>('/strategy-templates');
  return res.templates ?? [];
}

/** Publishes a PENDING_APPROVAL template — configuration JSON only;
 * admin approval lives in the admin-content curation surface. */
export function publishTemplate(
  api: ApiClient,
  input: { name: string; description: string; kind: string; config: string },
): Promise<StrategyTemplate> {
  let parsed: unknown;
  try {
    parsed = JSON.parse(input.config);
  } catch {
    parsed = input.config;
  }
  return api.post<StrategyTemplate>('/strategy-templates', {
    name: input.name,
    description: input.description,
    kind: input.kind,
    config: parsed,
  });
}

/** POST /strategy-templates/{id}/instantiate — copies the APPROVED
 * template's config into a new account-owned strategy (201). */
export function instantiateTemplate(api: ApiClient, id: number): Promise<Strategy> {
  return api.post<Strategy>(`/strategy-templates/${id}/instantiate`, {});
}

// ---------------------------------------------------------------------------
// Copy-strategy author flow — POST /copy/strategies, POST …/{id}/list
// (Phase-14 Task 14.3.14)
// ---------------------------------------------------------------------------

export interface CopyProfile {
  strategy_id: number;
  manager_account_id?: number;
  display_name: string;
  description?: string;
  currency?: string;
  instrument_class?: string;
  status: string; // INCUBATING | LISTED | SUSPENDED
  profit_share_pct?: string;
  incubating_since?: string;
  listed_at?: string;
  suspended_at?: string;
  suspend_reason?: string;
}

/** NOTE: the mounted API serves GET /copy/strategies as the public
 * LISTED-only discovery feed — there is no "my profiles" read endpoint
 * (store.StrategiesByManager exists but is not exposed). The panel keeps
 * the POST-created row in session state and renders it honestly instead
 * of inventing a list. */

/** Creates an INCUBATING copy-strategy profile — the caller becomes the
 * manager. */
export function createCopyProfile(
  api: ApiClient,
  input: {
    display_name: string;
    description: string;
    currency: string;
    instrument_class: string;
    profit_share_pct: string;
  },
): Promise<CopyProfile> {
  return api.post<CopyProfile>('/copy/strategies', input);
}

/** POST /copy/strategies/{id}/list — request LISTED. The service enforces
 * ≥30d incubation + appropriateness PASS fail-closed; refusals surface
 * as coded errors. */
export function requestListing(api: ApiClient, id: number): Promise<CopyProfile> {
  return api.post<CopyProfile>(`/copy/strategies/${id}/list`, {});
}

// ---------------------------------------------------------------------------
// Basket + promotion views — GET /baskets/{op_id}, GET /promotions/{id}
// ---------------------------------------------------------------------------

export interface BasketStatus {
  op_id: string;
  status: string;
  code?: number;
  leg_count?: number;
  legs_filled?: number;
  legs_unwound?: number;
  slippage_ticks?: number;
  legs?: {
    leg_index?: number;
    order_id?: number;
    instrument_id?: number;
    shard_id?: number;
    order_status?: string;
    filled_qty?: string;
  }[];
}

export function basketStatus(api: ApiClient, opId: string): Promise<BasketStatus> {
  return api.get<BasketStatus>(`/baskets/${encodeURIComponent(opId)}`);
}

export interface PromotionView {
  promotion_id: number;
  slug?: string;
  channel?: string;
  body_ref?: string;
  title?: string;
  version?: number;
  approval_status?: string;
  approved_until?: string;
}

/** Renderable-content view — PROMOTION_NOT_APPROVED (410) for anything
 * not APPROVED+unexpired. */
export async function promotionView(api: ApiClient, id: string): Promise<PromotionView | null> {
  const res = await api.get<unknown>(`/promotions/${id}`);
  if (!isRecord(res)) return null;
  const inner = res['promotion'];
  if (isRecord(inner)) return inner as unknown as PromotionView;
  return res as unknown as PromotionView;
}
