// Phase-19 — multi-asset margin API surface (Tasks 19.3.1/.8/.14/.15–.17/.23).
//
// Handlers in this file are thin adapters over the Phase-19 risk services
// (internal/risk margin.go, leverage.go, insurance_fund.go, collateral.go):
// the services own the business invariants — mode-change blocking with open
// positions, ESMA/CFTC/category/tier leverage caps, atomic audit rows, the
// ≤5s collateral-schedule propagation — while this layer owns auth identity,
// request/response shaping and the §8.7 error envelope.
//
// Nil-binding rule (Task text): every service seam here is checked before
// use; a handler constructed without its backing service emits
// SERVICE_DEGRADED (503) rather than panicking — the route registry's
// fail-closed shim convention mirrored inside the handler so construction
// order in cmd/gateway can degrade gracefully on partial wiring.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/gateway"
	exchredis "exchange/internal/redis"
	"exchange/internal/risk"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// POST /api/v1/account/margin-mode — Task 19.3.1 item 5 / 19.3.23
// ---------------------------------------------------------------------------

// MarginModeSetter is the narrow write seam — *risk.MarginService
// satisfies it. SetMode returns the settled row (post-normalization),
// so the response needs no second read. Exported so the gateway can bind
// a true-nil interface when construction degrades (typed-nil pointers
// would defeat the handler's svc == nil check).
type MarginModeSetter interface {
	SetMode(ctx context.Context, accountID int64, requested string) (*risk.MarginAccount, error)
}

// marginModeReader is the read seam used by the positions decoration —
// *risk.MarginService satisfies it too.
type marginModeReader interface {
	ModeFor(ctx context.Context, accountID int64) (risk.MarginMode, error)
}

type marginModeRequest struct {
	Mode string `json:"mode"`
}

// AccountMarginMode serves POST /api/v1/account/margin-mode. Accepts
// CROSS|ISOLATED|PORTFOLIO; the service rejects a switch while open
// positions exist with MARGIN_MODE_SWITCH_BLOCKED (409, registered at
// gateway construction — see cmd/gateway main). The account id is claims-
// bound; no body field can retarget another account.
func AccountMarginMode(svc MarginModeSetter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, _, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "margin mode service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var in marginModeRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeServiceErr(w, r, excerrors.New("INVALID_REQUEST", "malformed JSON body"))
			return
		}
		acct, err := svc.SetMode(r.Context(), accountID, in.Mode)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"account_id":  acct.AccountID,
			"margin_mode": string(acct.Mode),
		})
	}
}

// ---------------------------------------------------------------------------
// POST /api/v1/account/leverage — Task 19.3.16/.17/.23
// ---------------------------------------------------------------------------

// leverageSetter is the narrow write seam — *risk.LeverageService
// satisfies it. SetLeverage enforces the min(entity policy, ESMA/CFTC
// category cap, §19.3.17 tier band, instrument max) chain and the
// open-position margin re-check; the audit row rides the same tx.
type leverageSetter interface {
	SetLeverage(ctx context.Context, in risk.SetLeverageInput) (*risk.SetLeverageResult, error)
}

// leverageInstrumentResolver maps a request symbol onto its instrument —
// *risk.PgLeverageStore satisfies it. Bound only for the per-symbol call
// shape; the account-default call (symbol omitted) never consults it.
type leverageInstrumentResolver interface {
	InstrumentLeverageViewBySymbol(ctx context.Context, symbol string) (*risk.LeverageInstrumentView, error)
}

type leverageRequest struct {
	Symbol   string `json:"symbol,omitempty"`
	Leverage int    `json:"leverage"`
}

// AccountLeverageSet serves POST /api/v1/account/leverage. With "symbol"
// set the row is per-symbol leverage; without it the account default is
// updated (risk.SetLeverageInput.InstrumentID nil). Caps are the service's
// job — above-cap requests come back as its coded error
// (INVALID_REQUEST / MARGIN_INSUFFICIENT family per the §23 rows).
func AccountLeverageSet(svc leverageSetter, instruments leverageInstrumentResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if svc == nil {
			WriteError(w, "SERVICE_DEGRADED", "leverage service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var in leverageRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeServiceErr(w, r, excerrors.New("INVALID_REQUEST", "malformed JSON body"))
			return
		}
		if in.Leverage <= 0 {
			writeServiceErr(w, r, excerrors.New("INVALID_REQUEST",
				"leverage must be a positive integer"))
			return
		}
		var instrumentID *int64
		var symbol string
		if in.Symbol != "" {
			if instruments == nil {
				WriteError(w, "SERVICE_DEGRADED", "instrument resolution unavailable",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			view, err := instruments.InstrumentLeverageViewBySymbol(r.Context(), in.Symbol)
			if err != nil {
				writeServiceErr(w, r, err)
				return
			}
			if view == nil {
				writeServiceErr(w, r, excerrors.New("NOT_FOUND",
					fmt.Sprintf("instrument %q not found", in.Symbol)))
				return
			}
			id := view.ID
			instrumentID = &id
			symbol = view.Symbol
		}
		userID, _ := parseSubjectID(claims.Subject)
		res, err := svc.SetLeverage(r.Context(), risk.SetLeverageInput{
			AccountID:    accountID,
			InstrumentID: instrumentID,
			Requested:    in.Leverage,
			UserID:       userID,
		})
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		resp := map[string]any{
			"account_id": res.AccountID,
			"leverage":   res.Requested,
		}
		if res.InstrumentID != nil {
			resp["instrument_id"] = *res.InstrumentID
			resp["symbol"] = symbol
		}
		if res.Effective != nil {
			resp["effective_leverage"] = res.Effective.Effective
			resp["cap_components"] = res.Effective.Components
		}
		WriteJSON(w, http.StatusOK, resp)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/account/liquidations — Task 19.3.x liquidation history
// ---------------------------------------------------------------------------

// LiquidationEventView is the wire projection of one liquidation_events
// row (migration 230) joined to its instrument symbol.
type LiquidationEventView struct {
	ID                        int64           `json:"id"`
	PositionID                int64           `json:"position_id"`
	InstrumentID              int64           `json:"instrument_id"`
	Symbol                    string          `json:"symbol"`
	AuctionID                 *int64          `json:"auction_id,omitempty"`
	MarginCallEventID         *int64          `json:"margin_call_event_id,omitempty"`
	Kind                      string          `json:"kind"` // DIRECT_CLOSE|AUCTION_FILL|FORCE_CASH|ADL
	Side                      string          `json:"side"`
	Quantity                  decimal.Decimal `json:"quantity"`
	Price                     decimal.Decimal `json:"price"`
	MarkPrice                 decimal.Decimal `json:"mark_price"`
	InsuranceFundContribution decimal.Decimal `json:"insurance_fund_contribution"`
	PenaltyAmount             decimal.Decimal `json:"penalty_amount"`
	ADLQuintile               *int            `json:"adl_quintile,omitempty"`
	JournalEntryID            *int64          `json:"journal_entry_id,omitempty"`
	CreatedAt                 time.Time       `json:"created_at"`
}

// LiquidationListFilter is the account-private list query: symbol and
// RFC3339 from/to bounds plus the (created_at, id) keyset page.
type LiquidationListFilter struct {
	AccountID int64
	Symbol    string
	From      *time.Time
	To        *time.Time
	After     *Cursor // keyset continuation (created_at, id) — strictly before
	Limit     int
}

// AccountLiquidationLister is the Task-19 store seam for
// GET /api/v1/account/liquidations — returns the page rows plus the
// filtered total for the §8.8 envelope.
type AccountLiquidationLister interface {
	AccountLiquidations(ctx context.Context, f LiquidationListFilter) ([]LiquidationEventView, int64, error)
}

// PgLiquidationLister implements AccountLiquidationLister over pgx.
type PgLiquidationLister struct {
	Pool *pgxpool.Pool
}

// NewPgLiquidationLister binds the pool.
func NewPgLiquidationLister(pool *pgxpool.Pool) *PgLiquidationLister {
	return &PgLiquidationLister{Pool: pool}
}

// AccountLiquidations runs the filtered count + the keyset page. Filters
// build the same WHERE for both queries so total matches the stream.
func (s *PgLiquidationLister) AccountLiquidations(ctx context.Context,
	f LiquidationListFilter) ([]LiquidationEventView, int64, error) {

	if s.Pool == nil {
		return nil, 0, fmt.Errorf("liquidation lister: nil pgx pool")
	}
	where := `WHERE le.account_id = $1`
	args := []any{f.AccountID}
	if f.Symbol != "" {
		args = append(args, f.Symbol)
		where += fmt.Sprintf(` AND i.symbol = $%d`, len(args))
	}
	if f.From != nil {
		args = append(args, *f.From)
		where += fmt.Sprintf(` AND le.created_at >= $%d`, len(args))
	}
	if f.To != nil {
		args = append(args, *f.To)
		where += fmt.Sprintf(` AND le.created_at <= $%d`, len(args))
	}
	var total int64
	if err := s.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM liquidation_events le
		JOIN instruments i ON i.id = le.instrument_id `+where,
		args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("liquidation count acct %d: %w", f.AccountID, err)
	}
	pageWhere := where
	if f.After != nil {
		args = append(args, f.After.CreatedAt, f.After.ID)
		pageWhere += fmt.Sprintf(` AND (le.created_at, le.id) < ($%d, $%d)`,
			len(args)-1, len(args))
	}
	args = append(args, f.Limit)
	rows, err := s.Pool.Query(ctx, `
		SELECT le.id, le.position_id, le.instrument_id, i.symbol,
		       le.auction_id, le.margin_call_event_id,
		       le.kind, le.side::text,
		       le.quantity::text, le.price::text, le.mark_price::text,
		       le.insurance_fund_contribution::text, le.penalty_amount::text,
		       le.adl_quintile, le.journal_entry_id, le.created_at
		  FROM liquidation_events le
		  JOIN instruments i ON i.id = le.instrument_id `+pageWhere+`
		  ORDER BY le.created_at DESC, le.id DESC
		  LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("liquidation page acct %d: %w", f.AccountID, err)
	}
	defer rows.Close()
	out := []LiquidationEventView{}
	for rows.Next() {
		var v LiquidationEventView
		var qty, price, mark, ifc, pen string
		if err := rows.Scan(&v.ID, &v.PositionID, &v.InstrumentID, &v.Symbol,
			&v.AuctionID, &v.MarginCallEventID, &v.Kind, &v.Side,
			&qty, &price, &mark, &ifc, &pen,
			&v.ADLQuintile, &v.JournalEntryID, &v.CreatedAt); err != nil {
			return nil, 0, fmt.Errorf("liquidation scan: %w", err)
		}
		v.Quantity = decimal.RequireFromString(qty)
		v.Price = decimal.RequireFromString(price)
		v.MarkPrice = decimal.RequireFromString(mark)
		v.InsuranceFundContribution = decimal.RequireFromString(ifc)
		v.PenaltyAmount = decimal.RequireFromString(pen)
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("liquidation page acct %d: %w", f.AccountID, err)
	}
	return out, total, nil
}

// AccountLiquidations serves GET /api/v1/account/liquidations — the
// account-private force-order history. Filters: symbol, from, to
// (RFC3339); pagination is the §8.8 (created_at, id) keyset envelope.
func AccountLiquidations(lister AccountLiquidationLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if rejectForeignAccount(w, r, claims, r.URL.Query().Get("account_id")) {
			return
		}
		if lister == nil {
			WriteError(w, "SERVICE_DEGRADED", "liquidation history unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		p, err := ParseListParams(r, ListSpecFor("/api/v1/account/liquidations"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		f := LiquidationListFilter{
			AccountID: accountID,
			Symbol:    q.Get("symbol"),
			Limit:     p.Limit,
		}
		if p.Decoded != nil {
			c := *p.Decoded
			f.After = &c
		}
		if f.From, err = parseTimeQuery(q.Get("from")); err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if f.To, err = parseTimeQuery(q.Get("to")); err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		rows, total, err := lister.AccountLiquidations(r.Context(), f)
		if err != nil {
			writeServiceErr(w, r, err)
			return
		}
		env := NewListEnvelope(rows, p,
			PageCursors(rows, func(v LiquidationEventView) (time.Time, int64) {
				return v.CreatedAt, v.ID
			}), total)
		WriteJSON(w, http.StatusOK, env)
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/admin/insurance-fund — Task 19.3.14
// ---------------------------------------------------------------------------

// InsuranceFundReader is the narrow read seam — *risk.InsuranceFundService
// satisfies it (Balances + History). Exported for the same true-nil
// binding reason as MarginModeSetter.
type InsuranceFundReader interface {
	Balances(ctx context.Context) ([]risk.FundBalance, error)
	History(ctx context.Context, ccy string, cursor int64, limit int) ([]risk.FundTxRow, error)
}

// riskManagerRoles gates Risk Manager-and-stronger admin surfaces — same
// pair as EligibleLiquidationRoles but named for the generic gate.
var riskManagerRoles = map[string]bool{
	gateway.RoleSuperAdmin:  true,
	gateway.RoleRiskManager: true,
}

// requireRiskManagerRole resolves the admin actor and enforces Risk
// Manager-or-stronger; the route's adminAuth(RoleRiskManager) row is the
// primary gate — this is the defense-in-depth double check every admin
// handler in this package runs (nil resolver fails closed).
func requireRiskManagerRole(w http.ResponseWriter, r *http.Request,
	resolver AdminRoleResolver) (int64, bool) {

	adminID, ok := adminActor(w, r)
	if !ok {
		return 0, false
	}
	if resolver == nil {
		WriteError(w, "UNAUTHORIZED_ROLE",
			"role resolver not configured (Phase-07 RBAC stub boundary)",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	role, err := resolver(r.Context(), adminID)
	if err != nil {
		WriteError(w, "INTERNAL_ERROR", "role lookup failed",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	if !riskManagerRoles[role] {
		WriteError(w, "UNAUTHORIZED_ROLE",
			"insurance-fund administration requires Risk Manager or Super Admin",
			gateway.RequestIDFrom(r.Context()), nil)
		return 0, false
	}
	return adminID, true
}

// AdminInsuranceFund serves GET /api/v1/admin/insurance-fund: per-currency
// balances plus transaction history (?currency=&cursor=&limit= — cursor is
// the last seen insurance_fund_transactions.id, id-DESC keyset).
func AdminInsuranceFund(rdr InsuranceFundReader, resolver AdminRoleResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := requireRiskManagerRole(w, r, resolver); !ok {
			return
		}
		if rdr == nil {
			WriteError(w, "SERVICE_DEGRADED", "insurance fund service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		var cursor int64
		if raw := q.Get("cursor"); raw != "" {
			v, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || v < 0 {
				WriteError(w, "INVALID_REQUEST", "cursor must be a non-negative integer",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			cursor = v
		}
		limit := 100
		if raw := q.Get("limit"); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v < 1 || v > 1000 {
				WriteError(w, "INVALID_REQUEST", "limit must be an integer in 1..1000",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			limit = v
		}
		balances, err := rdr.Balances(r.Context())
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		if balances == nil {
			balances = []risk.FundBalance{}
		}
		history, err := rdr.History(r.Context(), q.Get("currency"), cursor, limit)
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		if history == nil {
			history = []risk.FundTxRow{}
		}
		next := ""
		if len(history) == limit && limit > 0 {
			next = strconv.FormatInt(history[len(history)-1].ID, 10)
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"balances": balances,
			"transactions": map[string]any{
				"data":        history,
				"next_cursor": next,
				"limit":       limit,
			},
		})
	}
}

// ---------------------------------------------------------------------------
// PUT /api/v1/admin/collateral-schedule — Task 19.3.8
// ---------------------------------------------------------------------------

// CollateralScheduleUpdater is the Task-19 admin seam — *risk.CollateralService
// satisfies it. UpdateSchedule validates rows, upserts per currency and
// writes the admin_audit_log entry in the same tx; the service cache TTL
// (≤5s) plus post-write invalidation bounds the propagation delay.
type CollateralScheduleUpdater interface {
	Schedule(ctx context.Context) ([]risk.CollateralScheduleEntry, error)
	UpdateSchedule(ctx context.Context, actor int64, rows []risk.CollateralScheduleEntry) error
}

// collateralScheduleRow is the wire shape — percentages are decimal JSON
// (string or number both decode through shopspring).
type collateralScheduleRow struct {
	Currency            string          `json:"currency"`
	Eligible            bool            `json:"eligible"`
	HaircutPct          decimal.Decimal `json:"haircut_pct"`
	MaxConcentrationPct decimal.Decimal `json:"max_concentration_pct"`
}

type collateralScheduleRequest struct {
	Rows []collateralScheduleRow `json:"rows"`
}

// collateralEntryJSON projects a store row for the response.
func collateralEntryJSON(e risk.CollateralScheduleEntry) map[string]any {
	return map[string]any{
		"currency":              e.Currency,
		"eligible":              e.Eligible,
		"haircut_pct":           e.HaircutPct,
		"max_concentration_pct": e.MaxConcentrationPct,
	}
}

// AdminCollateralSchedulePut serves PUT /api/v1/admin/collateral-schedule.
// The handler validates shape; the service validates values, persists the
// upsert + audit atomically and refreshes the valuation cache — schedule
// edits take effect on the next equity recompute, bounded by the 5s TTL.
func AdminCollateralSchedulePut(upd CollateralScheduleUpdater, resolver AdminRoleResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		adminID, ok := requireRiskManagerRole(w, r, resolver)
		if !ok {
			return
		}
		if upd == nil {
			WriteError(w, "SERVICE_DEGRADED", "collateral schedule service unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		var in collateralScheduleRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeSvcErr(w, r, excerrors.New("INVALID_REQUEST", "malformed JSON body"))
			return
		}
		rows := make([]risk.CollateralScheduleEntry, 0, len(in.Rows))
		for _, rr := range in.Rows {
			rows = append(rows, risk.CollateralScheduleEntry{
				Currency:            rr.Currency,
				Eligible:            rr.Eligible,
				HaircutPct:          rr.HaircutPct,
				MaxConcentrationPct: rr.MaxConcentrationPct,
			})
		}
		if err := upd.UpdateSchedule(r.Context(), adminID, rows); err != nil {
			writeSvcErr(w, r, err)
			return
		}
		// Echo the durable post-write image — Schedule reads the store,
		// never the ≤5s cache, so the response is the committed truth.
		schedule, err := upd.Schedule(r.Context())
		if err != nil {
			writeSvcErr(w, r, err)
			return
		}
		out := make([]map[string]any, 0, len(schedule))
		for _, e := range schedule {
			out = append(out, collateralEntryJSON(e))
		}
		WriteJSON(w, http.StatusOK, map[string]any{
			"schedule":                   out,
			"propagation_within_seconds": 5,
		})
	}
}

// ---------------------------------------------------------------------------
// GET /api/v1/account/positions decoration — Tasks 19.3.15–.17/.19
// ---------------------------------------------------------------------------

// ADLIndicatorReader is the adl:indicator:{account_id} HASH seam —
// position_id → quintile (1–5). Production binds RedisADLIndicatorReader.
type ADLIndicatorReader interface {
	ADLIndicators(ctx context.Context, accountID int64) (map[int64]int, error)
}

// RedisADLIndicatorReader reads the canonical ADL indicator hash
// (risk.AdlIndicatorKey). Malformed fields are skipped — a half-written
// hash degrades individual positions, it never fails the view.
type RedisADLIndicatorReader struct {
	C *exchredis.Client
}

// ADLIndicators implements ADLIndicatorReader over HGETALL.
func (r RedisADLIndicatorReader) ADLIndicators(ctx context.Context,
	accountID int64) (map[int64]int, error) {

	if r.C == nil {
		return nil, fmt.Errorf("adl indicator reader: nil redis client")
	}
	m, err := r.C.HGetAll(ctx, fmt.Sprintf("adl:indicator:%d", accountID)).Result()
	if err != nil {
		return nil, fmt.Errorf("adl:indicator read acct %d: %w", accountID, err)
	}
	out := make(map[int64]int, len(m))
	for field, raw := range m {
		posID, err := strconv.ParseInt(field, 10, 64)
		if err != nil {
			continue
		}
		q, err := strconv.Atoi(raw)
		if err != nil || q < 1 || q > 5 {
			continue // quintile domain is 1..5 — junk fields are ignored
		}
		out[posID] = q
	}
	return out, nil
}

// positionModeReader is the accounts.position_mode seam —
// *risk.PositionModeService satisfies it.
type positionModeReader interface {
	PositionMode(ctx context.Context, accountID int64) (string, error)
}

// AccountPositionsEnriched serves GET /api/v1/account/positions — the
// Phase-19 account-scoped positions view (Task 19.3.15 netting/hedging
// awareness + 19.3.19 ADL indicator + margin-level decoration). The
// shared read core lives beside AccountPositions in account.go; a nil
// dec reproduces the legacy response exactly.
func AccountPositionsEnriched(src positionSource, dec *PositionsDecoration) http.HandlerFunc {
	return accountPositions(src, dec)
}

// PositionsDecoration carries the optional read-only enrichment seams for
// the Phase-19 positions view. Every field is nil-tolerant: an absent
// reader simply omits its fields — the base position rows never depend on
// the decoration reads (decoration failure degrades, never fails).
type PositionsDecoration struct {
	ADL          ADLIndicatorReader     // adl_indicator per position
	MarginLevel  risk.MarginLevelReader // margin_level_pct + status block
	MarginMode   marginModeReader       // margin_mode
	PositionMode positionModeReader     // position_mode
	Leverage     risk.LeverageResolver  // effective_leverage per position
}

// positionView — one PositionRow plus the optional per-position
// decoration fields — is declared in account.go beside the read it
// projects (funding.PositionRow embedding).

// marginLevelJSON renders the margin:level:{account} hash projection —
// nil-safe: a missing hash leaves the whole block absent.
func marginLevelJSON(lv *risk.MarginLevel) map[string]any {
	m := map[string]any{
		"equity":      lv.Equity,
		"used_margin": lv.UsedMargin,
		"status":      lv.Status,
		"updated_at":  lv.UpdatedAt,
	}
	if lv.MarginLevelPct.IsPositive() {
		m["margin_level_pct"] = lv.MarginLevelPct
	}
	return m
}

// compile-time seam assertions — fail the build if the sibling-owned
// services drift off the handler contracts.
var (
	_ MarginModeSetter           = (*risk.MarginService)(nil)
	_ marginModeReader           = (*risk.MarginService)(nil)
	_ leverageSetter             = (*risk.LeverageService)(nil)
	_ leverageInstrumentResolver = (*risk.PgLeverageStore)(nil)
	_ InsuranceFundReader        = (*risk.InsuranceFundService)(nil)
	_ CollateralScheduleUpdater  = (*risk.CollateralService)(nil)
	_ positionModeReader         = (*risk.PositionModeService)(nil)
	_ risk.MarginLevelReader     = risk.RedisMarginLevelReader{}
	_ ADLIndicatorReader         = RedisADLIndicatorReader{}
	_ AccountLiquidationLister   = (*PgLiquidationLister)(nil)
)
