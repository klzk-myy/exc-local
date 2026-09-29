// Phase-15 Tasks 15.3.1 / 15.3.9 — instrument lifecycle state machine
// (spec §7.1, §7.2, §24 #290).
//
// Seven canonical states (migration 001 instrument_status_enum):
//
//	DRAFT → ACTIVE → CANCEL_ONLY/SUSPENDED/HALTED/RESTRICTED → ACTIVE → DELISTED
//
// The transition table below is the spec §7.1 graph plus the operational
// edges the phase tasks pin: CANCEL_ONLY → SUSPENDED|HALTED (Task 15.3.9
// item 2 — an operator may escalate a cancel-only book into a suspend or
// halt without resuming first), CANCEL_ONLY/RESTRICTED → DELISTED (§7.5
// delisting ladder: RESTRICTED 24h notice → DELISTED → 30d close-only),
// and the HALTED↔SUSPENDED escalation pair (either control state may
// supersede the other; resting-order treatment differs — SUSPENDED
// force-cancels after grace, HALTED preserves). DELISTED is terminal.
//
// Role + dual-control matrix (spec §7.2 — enforced here at the service
// layer AND mirrored by the route registry):
//
//	Op            Route role           Dual control
//	create        Super Admin          yes (OpInstrumentCreate)
//	activate      Risk Manager         no
//	restrict      Risk Manager         no
//	cancel-only   Risk Manager | Compliance Officer (Task 15.3.9)
//	suspend       Compliance Officer   no
//	halt          Risk Manager         no
//	resume        Risk Manager         yes (OpInstrumentResume)
//	delist        Super Admin          yes (OpInstrumentDelist)
//
// "Role X" admits X and Super Admin ("all permissions", §8.2); "+"
// follows admin.Permits.
//
// Grace periods (§7.1): SUSPENDED gets a 5-minute cancel-only window —
// clients may cancel but not enter; the in-process sweeper then
// mass-cancels every resting order on the instrument through the
// InstrumentOrderCanceller seam (orders pipeline — Task 5.3.24 scoped
// mass cancel). RESTRICTED carries the §7.5 24h delisting-notice window
// (timer surfaced to the ops board; no automatic state move — the
// delist step is operator-driven). DELISTED carries a 30d close-only
// window in which only reduce_only orders are admitted (spec §7.1
// remediation #35 — the C++ PreTradeChecker already implements this;
// the Go order gate mirrors it).
//
// Engine publication contract (Go → C++ control plane):
//
//	instrument:status:{symbol}      = plain enum word ("SUSPENDED" ...).
//	                                Written for EVERY non-DRAFT instrument
//	                                on every transition AND re-published by
//	                                the boot/periodic reconciler (the C++
//	                                refresher polls at 1s; a missing key
//	                                must never read as ACTIVE — the
//	                                reconciler keeps the feed complete).
//	                                DRAFT instruments carry no key. The key
//	                                is deleted only when the PG row is
//	                                purged (delisting purge step).
//	instrument:auction:{symbol}     = "CALL:{deadline_unix_ns}".
//	                                Written on resume when the reopening
//	                                call auction applies (Task 15.3.6 —
//	                                the C++ AuctionManager consumes this;
//	                                status still reports ACTIVE while the
//	                                auction key gates matching into CALL).
//	                                Deleted on direct (skip_auction) resumes.
//	instrument:lifecycle:sweep_deadline:{instrument_id} = unix ms —
//	                                the SUSPENDED grace deadline
//	                                (observability + sweep bookkeeping).
//	instrument:lifecycle:swept:{instrument_id}          = "1" —
//	                                grace mass-cancel completed marker.
//
// WS broadcast: every transition publishes InstrumentStatusEvent on the
// public "venue.instrument_status" channel (the venue-info /
// exchange-info REST document remains the consumer-facing snapshot; the
// channel is the push seam — same convention as venue.status for the
// kill-switch). SBE security-definition + FIX SecurityStatus
// republication are the Phase-18/06 clusters' consumers of the same
// status feed.
//
// Audit: every mutation (create/update/transition) writes
// admin_audit_log + the audit_hash_chain link inside the mutation's own
// transaction via admin.Log (Task 7.3.3). Dual-controlled transitions
// run inside the DualControlService approval transaction — the Redis/WS
// side effects land via the reconciler on the next tick (documented
// post-commit pattern; the engine gate converges within one sweep
// interval, never incorrectly permissive because the C++ reads status,
// not requests).
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// States / ops / grace constants (spec §7.1/§7.2 canonical values)
// ---------------------------------------------------------------------------

// Lifecycle states — verbatim instrument_status_enum values.
const (
	InstDraft      = "DRAFT"
	InstActive     = "ACTIVE"
	InstCancelOnly = "CANCEL_ONLY"
	InstSuspended  = "SUSPENDED"
	InstHalted     = "HALTED"
	InstRestricted = "RESTRICTED"
	InstDelisted   = "DELISTED"
)

// Lifecycle operation names (the REST verb part after /instruments/{id}/).
const (
	LcOpActivate   = "activate"
	LcOpRestrict   = "restrict"
	LcOpCancelOnly = "cancel-only"
	LcOpSuspend    = "suspend"
	LcOpHalt       = "halt"
	LcOpResume     = "resume"
	LcOpDelist     = "delist"
)

// Grace windows (spec §7.1/§7.5).
const (
	SuspendedGrace   = 5 * time.Minute     // cancel-only window → mass-cancel
	RestrictedGrace  = 24 * time.Hour      // delisting notice window
	DelistedGrace    = 30 * 24 * time.Hour // close-only window (reduce_only)
	ReopenAuctionLen = 5 * time.Minute     // reopening CALL default (§24 #142)
)

// InstrumentStatusChannel is the public WS channel every lifecycle
// transition broadcasts on (documented seam — no pre-existing instrument
// status channel exists; venue.status is the kill-switch precedent).
const InstrumentStatusChannel = "venue.instrument_status"

// Redis key layout (documented Go→C++ contract above).
func instrumentStatusKey(symbol string) string  { return "instrument:status:" + symbol }
func instrumentAuctionKey(symbol string) string { return "instrument:auction:" + symbol }
func suspendSweepDeadlineKey(id int64) string {
	return fmt.Sprintf("instrument:lifecycle:sweep_deadline:%d", id)
}
func suspendSweptKey(id int64) string {
	return fmt.Sprintf("instrument:lifecycle:swept:%d", id)
}

// lifecycleTransitions is the allowed from→to edge set (spec §7.1 graph +
// the operational edges documented in the file header).
var lifecycleTransitions = map[string]map[string]bool{
	InstDraft:      {InstActive: true},
	InstActive:     {InstCancelOnly: true, InstSuspended: true, InstHalted: true, InstRestricted: true, InstDelisted: true},
	InstCancelOnly: {InstActive: true, InstSuspended: true, InstHalted: true, InstDelisted: true},
	InstRestricted: {InstActive: true, InstSuspended: true, InstHalted: true, InstCancelOnly: true, InstDelisted: true},
	InstSuspended:  {InstActive: true, InstHalted: true, InstDelisted: true},
	InstHalted:     {InstActive: true, InstSuspended: true, InstDelisted: true},
	InstDelisted:   {},
}

// opSpec pins each operation's target state, role gate and whether a
// reason is mandatory (all mutating control transitions demand one —
// the audit row is the regulatory record).
type opSpec struct {
	to        string
	roles     map[string]bool
	reasonReq bool
}

var lifecycleOps = map[string]opSpec{
	LcOpActivate:   {to: InstActive, roles: map[string]bool{RoleRiskManager: true, RoleSuperAdmin: true}},
	LcOpRestrict:   {to: InstRestricted, roles: map[string]bool{RoleRiskManager: true, RoleSuperAdmin: true}, reasonReq: true},
	LcOpCancelOnly: {to: InstCancelOnly, roles: map[string]bool{RoleRiskManager: true, RoleComplianceOfficer: true, RoleSuperAdmin: true}, reasonReq: true},
	LcOpSuspend:    {to: InstSuspended, roles: map[string]bool{RoleComplianceOfficer: true, RoleSuperAdmin: true}, reasonReq: true},
	LcOpHalt:       {to: InstHalted, roles: map[string]bool{RoleRiskManager: true, RoleSuperAdmin: true}, reasonReq: true},
	LcOpResume:     {to: InstActive, roles: map[string]bool{RoleRiskManager: true, RoleSuperAdmin: true}},
	LcOpDelist:     {to: InstDelisted, roles: map[string]bool{RoleSuperAdmin: true}, reasonReq: true},
}

// DualControlledOp reports whether the §7.2 matrix routes the operation
// through the four-eyes queue (create/resume/delist — create is not a
// transition but shares the queue; the handler submits
// OpInstrumentCreate separately).
func DualControlledOp(op string) bool {
	return op == LcOpResume || op == LcOpDelist
}

// ---------------------------------------------------------------------------
// Instrument projection (admin side — write-capable counterpart of the
// marketapi read projection)
// ---------------------------------------------------------------------------

// Instrument is the admin lifecycle view of one instruments row.
type Instrument struct {
	ID              int64     `json:"id"`
	Symbol          string    `json:"symbol"`
	BaseCurrency    string    `json:"base_currency"`
	QuoteCurrency   string    `json:"quote_currency"`
	InstrumentType  string    `json:"instrument_type"`
	Status          string    `json:"status"`
	TickSize        string    `json:"tick_size"`
	LotSize         string    `json:"lot_size"`
	MinOrderQty     string    `json:"min_order_qty"`
	MaxOrderQty     string    `json:"max_order_qty"`
	SettlementCycle int       `json:"settlement_cycle"`
	MaxLeverage     int64     `json:"max_leverage"`
	UpdatedAt       time.Time `json:"-"`
	// Computed grace metadata (nil when the state carries no window).
	StateEnteredAt *time.Time `json:"state_entered_at,omitempty"`
	GraceDeadline  *time.Time `json:"grace_deadline,omitempty"`
	GraceKind      string     `json:"grace_kind,omitempty"`
}

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// InstrumentStatusFeed is the Redis control-plane publisher — the C++
// PreTradeChecker/AuctionManager consume these keys. *redis.Client's
// underlying go-redis client satisfies the small command surface via
// RedisStatusFeed.
type InstrumentStatusFeed interface {
	SetStatus(ctx context.Context, symbol, status string) error
	GetStatus(ctx context.Context, symbol string) (string, error)
	DelStatus(ctx context.Context, symbol string) error
	ScanStatuses(ctx context.Context) (map[string]string, error)
	SetAuctionCall(ctx context.Context, symbol string, deadlineUnixNs int64) error
	DelAuction(ctx context.Context, symbol string) error
	SetSweepDeadline(ctx context.Context, instrumentID int64, deadline time.Time) error
	DelSweepKeys(ctx context.Context, instrumentID int64) error
	IsSwept(ctx context.Context, instrumentID int64) (bool, error)
	MarkSwept(ctx context.Context, instrumentID int64) error
}

// InstrumentOrderCanceller mass-cancels every resting order on an
// instrument — the SUSPENDED grace side effect. Wired to the orders
// dispatcher at the command layer (orders.Dispatcher → scoped
// MassCancel; accounts.OrderDispatcher satisfies the shape via adapter).
type InstrumentOrderCanceller interface {
	CancelInstrumentOrders(ctx context.Context, instrumentID int64, reason string) (int, error)
}

// InstrumentWSPublisher fans transitions out on InstrumentStatusChannel
// — *ws.Server satisfies it; nil disables.
type InstrumentWSPublisher interface {
	Publish(channel string, data any)
}

// InstrumentStatusEvent is the WS payload.
type InstrumentStatusEvent struct {
	Event         string `json:"event"` // "INSTRUMENT_STATUS"
	Symbol        string `json:"symbol"`
	From          string `json:"from,omitempty"`
	To            string `json:"to"`
	Reason        string `json:"reason,omitempty"`
	ActorID       int64  `json:"actor_id,omitempty"`
	ApproverID    int64  `json:"approver_id,omitempty"`
	GraceDeadline int64  `json:"grace_deadline_ms,omitempty"`
	Auction       bool   `json:"auction,omitempty"` // resume routed through reopening CALL
	Source        string `json:"source"`            // "admin" | "reconciler"
	TsMs          int64  `json:"ts_ms"`
}

// ---------------------------------------------------------------------------
// RedisStatusFeed — production InstrumentStatusFeed over go-redis
// ---------------------------------------------------------------------------

// RedisStatusFeed implements InstrumentStatusFeed on the coordination
// Redis (noeviction instance — control keys must never be evicted).
type RedisStatusFeed struct {
	C *goredis.Client
}

func (f RedisStatusFeed) SetStatus(ctx context.Context, symbol, status string) error {
	return f.C.Set(ctx, instrumentStatusKey(symbol), status, 0).Err()
}

func (f RedisStatusFeed) GetStatus(ctx context.Context, symbol string) (string, error) {
	v, err := f.C.Get(ctx, instrumentStatusKey(symbol)).Result()
	if errors.Is(err, goredis.Nil) {
		return "", nil
	}
	return v, err
}

func (f RedisStatusFeed) DelStatus(ctx context.Context, symbol string) error {
	return f.C.Del(ctx, instrumentStatusKey(symbol)).Err()
}

func (f RedisStatusFeed) ScanStatuses(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	var cursor uint64
	for {
		keys, next, err := f.C.Scan(ctx, cursor, "instrument:status:*", 200).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			symbol := strings.TrimPrefix(k, "instrument:status:")
			v, err := f.C.Get(ctx, k).Result()
			if errors.Is(err, goredis.Nil) {
				continue // raced delete
			}
			if err != nil {
				return nil, err
			}
			out[symbol] = v
		}
		cursor = next
		if cursor == 0 {
			return out, nil
		}
	}
}

func (f RedisStatusFeed) SetAuctionCall(ctx context.Context, symbol string, deadlineUnixNs int64) error {
	return f.C.Set(ctx, instrumentAuctionKey(symbol),
		fmt.Sprintf("CALL:%d", deadlineUnixNs), 0).Err()
}

func (f RedisStatusFeed) DelAuction(ctx context.Context, symbol string) error {
	// The auction key family — the reopening path only ever writes the
	// CALL key; the Task 15.3.13 daily-close scheduler additionally owns
	// :queue (JSON order ids) and :result (engine verdict). Deleting
	// absent sub-keys is a no-op, so teardown clears the whole set.
	return f.C.Del(ctx,
		instrumentAuctionKey(symbol),
		instrumentAuctionKey(symbol)+":queue",
		instrumentAuctionKey(symbol)+":result").Err()
}

// Task 15.3.13 key legs (spec §7.1/§7.3) — the daily closing-auction
// scheduler's control surface beyond the reopening CALL:
//
//	instrument:auction:{symbol}        "EXTEND:{deadline_unix_ns}"
//	instrument:auction:{symbol}:queue  JSON []int64 (MOC/MOO ids)
//	instrument:auction:{symbol}:result "CLEARED" | "FAILED" (engine→Go)
//	instrument:fixing:{symbol}         "FIXING:{benchmark}:{rate}:{ns}"

// SetAuctionExtend arms "EXTEND:{deadline_unix_ns}" — a §7.3 30-second
// extension of a failed uncross.
func (f RedisStatusFeed) SetAuctionExtend(ctx context.Context, symbol string, deadlineUnixNs int64) error {
	return f.C.Set(ctx, instrumentAuctionKey(symbol),
		fmt.Sprintf("EXTEND:%d", deadlineUnixNs), 0).Err()
}

// SetAuctionQueue publishes the queued order ids the uncross consumes.
func (f RedisStatusFeed) SetAuctionQueue(ctx context.Context, symbol string, orderIDs []int64) error {
	raw, err := json.Marshal(orderIDs)
	if err != nil {
		return err
	}
	return f.C.Set(ctx, instrumentAuctionKey(symbol)+":queue", raw, 0).Err()
}

// GetAuctionResult reads the engine's deadline verdict on the armed
// auction — "CLEARED"|"FAILED"; "" (no error) when absent.
func (f RedisStatusFeed) GetAuctionResult(ctx context.Context, symbol string) (string, error) {
	v, err := f.C.Get(ctx, instrumentAuctionKey(symbol)+":result").Result()
	if errors.Is(err, goredis.Nil) {
		return "", nil
	}
	return v, err
}

// SetFixing publishes the benchmark-fixing key the engine's FIXING-order
// machinery consumes (Task 15.3.13/16.3.9):
// "FIXING:{benchmark}:{rate}:{scheduled_unix_ns}".
func (f RedisStatusFeed) SetFixing(ctx context.Context, symbol, payload string) error {
	return f.C.Set(ctx, "instrument:fixing:"+symbol, payload, 0).Err()
}

func (f RedisStatusFeed) SetSweepDeadline(ctx context.Context, instrumentID int64, deadline time.Time) error {
	return f.C.Set(ctx, suspendSweepDeadlineKey(instrumentID),
		deadline.UnixMilli(), 0).Err()
}

func (f RedisStatusFeed) DelSweepKeys(ctx context.Context, instrumentID int64) error {
	return f.C.Del(ctx, suspendSweepDeadlineKey(instrumentID),
		suspendSweptKey(instrumentID)).Err()
}

func (f RedisStatusFeed) IsSwept(ctx context.Context, instrumentID int64) (bool, error) {
	v, err := f.C.Get(ctx, suspendSweptKey(instrumentID)).Result()
	if errors.Is(err, goredis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v != "", nil
}

func (f RedisStatusFeed) MarkSwept(ctx context.Context, instrumentID int64) error {
	return f.C.Set(ctx, suspendSweptKey(instrumentID), "1", 0).Err()
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// InstrumentService is the §7.1 lifecycle engine + §7.2 role/dual gate.
type InstrumentService struct {
	pool     *pgxpool.Pool
	roles    AdminRoleResolver
	feed     InstrumentStatusFeed
	cancels  InstrumentOrderCanceller
	ws       InstrumentWSPublisher
	onStatus func() // post-transition hook (venue cache invalidation)
	logf     func(format string, args ...any)
	now      func() time.Time
}

// InstrumentDeps wires the service. Pool, Roles and Feed are required —
// a transition that cannot publish its engine gate fails closed.
type InstrumentDeps struct {
	Pool      *pgxpool.Pool
	Roles     AdminRoleResolver
	Feed      InstrumentStatusFeed
	Canceller InstrumentOrderCanceller // nil → sweep logs + retries
	WS        InstrumentWSPublisher
	OnStatus  func() // e.g. marketapi cache invalidation
	Logf      func(format string, args ...any)
	Now       func() time.Time
}

// NewInstrumentService wires the lifecycle engine.
func NewInstrumentService(d InstrumentDeps) (*InstrumentService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("instrument lifecycle: pgx pool is nil")
	}
	if d.Roles == nil {
		return nil, fmt.Errorf("instrument lifecycle: role resolver is nil")
	}
	if d.Feed == nil {
		return nil, fmt.Errorf("instrument lifecycle: status feed is nil")
	}
	s := &InstrumentService{
		pool: d.Pool, roles: d.Roles, feed: d.Feed, cancels: d.Canceller,
		ws: d.WS, onStatus: d.OnStatus, logf: d.Logf, now: d.Now,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Reads
// ---------------------------------------------------------------------------

const instrumentAdminCols = `
	id, symbol, base_currency, quote_currency, instrument_type::text,
	status::text, tick_size::text, lot_size::text, min_order_qty::text,
	max_order_qty::text, settlement_cycle, max_leverage, updated_at`

func scanAdminInstrument(row pgx.Row) (*Instrument, error) {
	var i Instrument
	err := row.Scan(&i.ID, &i.Symbol, &i.BaseCurrency, &i.QuoteCurrency,
		&i.InstrumentType, &i.Status, &i.TickSize, &i.LotSize,
		&i.MinOrderQty, &i.MaxOrderQty, &i.SettlementCycle, &i.MaxLeverage,
		&i.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &i, nil
}

// withGrace annotates the computed grace window the state carries.
func (s *InstrumentService) withGrace(i *Instrument) *Instrument {
	entered := i.UpdatedAt
	i.StateEnteredAt = &entered
	var d time.Time
	switch i.Status {
	case InstSuspended:
		d = entered.Add(SuspendedGrace)
		i.GraceKind = "CANCEL_ONLY_WINDOW"
	case InstRestricted:
		d = entered.Add(RestrictedGrace)
		i.GraceKind = "DELIST_NOTICE"
	case InstDelisted:
		d = entered.Add(DelistedGrace)
		i.GraceKind = "CLOSE_ONLY"
	default:
		return i
	}
	i.GraceDeadline = &d
	return i
}

// Get returns one instrument by id (nil when unknown).
func (s *InstrumentService) Get(ctx context.Context, id int64) (*Instrument, error) {
	i, err := scanAdminInstrument(s.pool.QueryRow(ctx,
		`SELECT `+instrumentAdminCols+` FROM instruments WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "instrument lookup", err)
	}
	return s.withGrace(i), nil
}

// List returns every instrument (all states) ordered by symbol — the
// GET /admin/instruments surface incl. non-ACTIVE states.
func (s *InstrumentService) List(ctx context.Context) ([]Instrument, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+instrumentAdminCols+` FROM instruments ORDER BY symbol`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "list instruments", err)
	}
	defer rows.Close()
	out := []Instrument{}
	for rows.Next() {
		var i Instrument
		if err := rows.Scan(&i.ID, &i.Symbol, &i.BaseCurrency, &i.QuoteCurrency,
			&i.InstrumentType, &i.Status, &i.TickSize, &i.LotSize,
			&i.MinOrderQty, &i.MaxOrderQty, &i.SettlementCycle, &i.MaxLeverage,
			&i.UpdatedAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan instrument", err)
		}
		out = append(out, *s.withGrace(&i))
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Create (dual-control executor path — inserts DRAFT)
// ---------------------------------------------------------------------------

// InstrumentCreate is the POST /admin/instruments payload.
type InstrumentCreate struct {
	Symbol          string `json:"symbol"`
	BaseCurrency    string `json:"base_currency"`
	QuoteCurrency   string `json:"quote_currency"`
	InstrumentType  string `json:"instrument_type"`
	TickSize        string `json:"tick_size"`
	LotSize         string `json:"lot_size"`
	MinOrderQty     string `json:"min_order_qty"`
	MaxOrderQty     string `json:"max_order_qty"`
	PriceBandPctUp  string `json:"price_band_pct_up"`
	PriceBandPctDn  string `json:"price_band_pct_down"`
	SettlementCycle int    `json:"settlement_cycle"`
	MaxLeverage     int64  `json:"max_leverage"`
	Reason          string `json:"reason"`
}

func validDecimal(s string) bool {
	if s == "" {
		return false
	}
	dot := false
	for i, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.' && !dot && i > 0:
			dot = true
		case r == '-' && i == 0:
		default:
			return false
		}
	}
	return true
}

// CreateInTx inserts the DRAFT row + audit inside the caller's
// transaction (the OpInstrumentCreate approval tx). The new row has no
// book yet, so no Redis publication is needed — DRAFT carries no
// instrument:status key by contract.
func (s *InstrumentService) CreateInTx(ctx context.Context, tx pgx.Tx,
	actor AdminActor, in InstrumentCreate) (*Instrument, error) {

	in.Symbol = strings.ToUpper(strings.TrimSpace(in.Symbol))
	in.BaseCurrency = strings.ToUpper(strings.TrimSpace(in.BaseCurrency))
	in.QuoteCurrency = strings.ToUpper(strings.TrimSpace(in.QuoteCurrency))
	in.InstrumentType = strings.ToUpper(strings.TrimSpace(in.InstrumentType))

	if len(in.Symbol) == 0 || len(in.Symbol) > 32 ||
		len(in.BaseCurrency) != 3 || len(in.QuoteCurrency) != 3 {
		return nil, excerrors.New("INVALID_REQUEST",
			"symbol (≤32 chars) and ISO 4217 base/quote currencies are required")
	}
	switch in.InstrumentType {
	case "SPOT", "FORWARD", "SWAP", "NDF", "OPTION":
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			"instrument_type must be SPOT|FORWARD|SWAP|NDF|OPTION")
	}
	for _, d := range []string{in.TickSize, in.LotSize, in.MinOrderQty, in.MaxOrderQty} {
		if !validDecimal(d) || strings.HasPrefix(d, "-") {
			return nil, excerrors.New("INVALID_REQUEST",
				"tick_size/lot_size/min_order_qty/max_order_qty must be non-negative decimals")
		}
	}
	if in.PriceBandPctUp == "" {
		in.PriceBandPctUp = "2.00"
	}
	if in.PriceBandPctDn == "" {
		in.PriceBandPctDn = "5.00"
	}
	if !validDecimal(in.PriceBandPctUp) || !validDecimal(in.PriceBandPctDn) {
		return nil, excerrors.New("INVALID_REQUEST",
			"price_band_pct_up/down must be decimal percentages")
	}
	if in.SettlementCycle < 0 || in.SettlementCycle > 2 {
		return nil, excerrors.New("INVALID_REQUEST",
			"settlement_cycle must be 0 (same-day), 1 (T+1) or 2 (T+2)")
	}
	if in.MaxLeverage <= 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"max_leverage must be positive")
	}

	var i Instrument
	err := tx.QueryRow(ctx, `
		INSERT INTO instruments
		    (symbol, base_currency, quote_currency, instrument_type,
		     tick_size, lot_size, min_order_qty, max_order_qty,
		     price_band_pct_up, price_band_pct_down,
		     settlement_cycle, max_leverage, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'DRAFT')
		RETURNING `+instrumentAdminCols,
		in.Symbol, in.BaseCurrency, in.QuoteCurrency, in.InstrumentType,
		in.TickSize, in.LotSize, in.MinOrderQty, in.MaxOrderQty,
		in.PriceBandPctUp, in.PriceBandPctDn,
		in.SettlementCycle, in.MaxLeverage).
		Scan(&i.ID, &i.Symbol, &i.BaseCurrency, &i.QuoteCurrency,
			&i.InstrumentType, &i.Status, &i.TickSize, &i.LotSize,
			&i.MinOrderQty, &i.MaxOrderQty, &i.SettlementCycle, &i.MaxLeverage,
			&i.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, excerrors.New("INVALID_REQUEST",
				"symbol "+in.Symbol+" already exists")
		}
		return nil, excerrors.Wrap("INTERNAL_ERROR", "insert instrument", err)
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "instrument.create",
		TargetType:  "instrument",
		TargetID:    &i.ID,
		AfterState: map[string]any{
			"symbol": i.Symbol, "status": InstDraft,
			"type": i.InstrumentType, "reason": in.Reason,
			"approved_by": actor.ApproverID,
		},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	return &i, nil
}

// ---------------------------------------------------------------------------
// Update (PUT — parameter edits; Task 15.3.2 route)
// ---------------------------------------------------------------------------

// InstrumentUpdate carries optional per-field edits. nil = unchanged.
// Editable while DRAFT or ACTIVE only — parameter changes under control
// states are the Task 15.3.8 maker-checker effective-date machinery's
// domain (and would poison the SUSPENDED grace deadline that keys off
// updated_at).
type InstrumentUpdate struct {
	TickSize        *string `json:"tick_size"`
	LotSize         *string `json:"lot_size"`
	MinOrderQty     *string `json:"min_order_qty"`
	MaxOrderQty     *string `json:"max_order_qty"`
	MinNotional     *string `json:"min_notional"`
	MinPrice        *string `json:"min_price"`
	MaxPrice        *string `json:"max_price"`
	PriceBandPctUp  *string `json:"price_band_pct_up"`
	PriceBandPctDn  *string `json:"price_band_pct_down"`
	MaxSpreadPips   *string `json:"max_spread_pips"`
	MaxOpenOrders   *int64  `json:"max_open_orders"`
	MaxAlgoOrders   *int64  `json:"max_algo_orders"`
	MaxLeverage     *int64  `json:"max_leverage"`
	SettlementCycle *int    `json:"settlement_cycle"`
	Reason          string  `json:"reason"`
}

// Update applies a parameter edit: DRAFT/ACTIVE instruments only,
// Risk Manager+ per the route registry, audit-logged with before/after.
func (s *InstrumentService) Update(ctx context.Context, actor AdminActor,
	id int64, in InstrumentUpdate) (*Instrument, error) {

	if err := s.requireRole(ctx, actor.UserID,
		map[string]bool{RoleRiskManager: true, RoleSuperAdmin: true},
		"instrument update"); err != nil {
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cur, err := s.lockRow(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if cur.Status != InstDraft && cur.Status != InstActive {
		return nil, excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			"parameter edits require DRAFT or ACTIVE state (current "+cur.Status+")")
	}

	sets := []string{}
	args := []any{}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	addStr := func(col string, v *string) error {
		if v == nil {
			return nil
		}
		if !validDecimal(*v) {
			return excerrors.New("INVALID_REQUEST",
				col+" must be a non-negative decimal")
		}
		add(col, *v)
		return nil
	}
	for _, p := range []struct {
		col string
		v   *string
	}{
		{"tick_size", in.TickSize}, {"lot_size", in.LotSize},
		{"min_order_qty", in.MinOrderQty}, {"max_order_qty", in.MaxOrderQty},
		{"min_notional", in.MinNotional},
		{"min_price", in.MinPrice}, {"max_price", in.MaxPrice},
		{"price_band_pct_up", in.PriceBandPctUp},
		{"price_band_pct_down", in.PriceBandPctDn},
		{"max_spread_pips", in.MaxSpreadPips},
	} {
		if err := addStr(p.col, p.v); err != nil {
			return nil, err
		}
	}
	if in.MaxOpenOrders != nil {
		add("max_open_orders", *in.MaxOpenOrders)
	}
	if in.MaxAlgoOrders != nil {
		add("max_algo_orders", *in.MaxAlgoOrders)
	}
	if in.MaxLeverage != nil {
		if *in.MaxLeverage <= 0 {
			return nil, excerrors.New("INVALID_REQUEST", "max_leverage must be positive")
		}
		add("max_leverage", *in.MaxLeverage)
	}
	if in.SettlementCycle != nil {
		if *in.SettlementCycle < 0 || *in.SettlementCycle > 2 {
			return nil, excerrors.New("INVALID_REQUEST",
				"settlement_cycle must be 0, 1 or 2")
		}
		add("settlement_cycle", *in.SettlementCycle)
	}
	if len(sets) == 0 {
		return nil, excerrors.New("INVALID_REQUEST", "no updatable fields supplied")
	}
	args = append(args, id)
	var i Instrument
	err = tx.QueryRow(ctx,
		`UPDATE instruments SET `+strings.Join(sets, ", ")+
			`, updated_at = now() WHERE id = $`+strconv.Itoa(len(args))+
			` RETURNING `+instrumentAdminCols, args...).
		Scan(&i.ID, &i.Symbol, &i.BaseCurrency, &i.QuoteCurrency,
			&i.InstrumentType, &i.Status, &i.TickSize, &i.LotSize,
			&i.MinOrderQty, &i.MaxOrderQty, &i.SettlementCycle, &i.MaxLeverage,
			&i.UpdatedAt)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "update instrument", err)
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "instrument.update",
		TargetType:  "instrument",
		TargetID:    &i.ID,
		BeforeState: map[string]any{"row": cur},
		AfterState: map[string]any{
			"fields": sets, "reason": in.Reason,
		},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	s.postStatusChange()
	return s.withGrace(&i), nil
}

// ---------------------------------------------------------------------------
// Transitions
// ---------------------------------------------------------------------------

// TransitionInput carries the per-call transition parameters.
type TransitionInput struct {
	Reason      string `json:"reason"`
	SkipAuction bool   `json:"skip_auction"` // resume: bypass reopening CALL
	Auction     bool   `json:"auction"`      // resume: opt-in CALL on direct-resume states
	ClientIP    string `json:"-"`
}

// lockRow SELECT ... FOR UPDATEs the instruments row.
func (s *InstrumentService) lockRow(ctx context.Context, tx pgx.Tx, id int64) (*Instrument, error) {
	i, err := scanAdminInstrument(tx.QueryRow(ctx,
		`SELECT `+instrumentAdminCols+` FROM instruments WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("NOT_FOUND",
			fmt.Sprintf("instrument %d not found", id))
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "lock instrument", err)
	}
	return i, nil
}

// requireRole resolves the actor's binding and enforces the op's role set.
func (s *InstrumentService) requireRole(ctx context.Context, userID int64,
	allowed map[string]bool, what string) error {
	if s.roles == nil {
		return excerrors.New("UNAUTHORIZED_ROLE",
			what+" requires an admin role — resolver unavailable")
	}
	role, err := s.roles(ctx, userID)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "role resolution", err)
	}
	if !allowed[role] {
		return excerrors.New("UNAUTHORIZED_ROLE",
			what+" requires a binding in the operation's role set")
	}
	return nil
}

// reopeningAuction reports whether this resume runs through the Task
// 15.3.6 reopening CALL auction: mandatory-by-default for HALTED and
// SUSPENDED sources (spec §7.1 — gapped-book protection; admin
// skip_auction bypasses for trivial resumes) and opt-in for
// RESTRICTED/CANCEL_ONLY sources (coherent books resume direct).
func reopeningAuction(from string, in TransitionInput) bool {
	if in.Auction {
		return true
	}
	if in.SkipAuction {
		return false
	}
	return from == InstHalted || from == InstSuspended
}

// auctionNeeded is used by the executor wrapper (same rule, shared).
func auctionDeadline(now time.Time) int64 {
	return now.Add(ReopenAuctionLen).UnixNano()
}

// Transition runs a single-approver lifecycle operation (§7.2 matrix:
// activate/restrict/cancel-only/suspend/halt — create/resume/delist are
// four-eyes and flow through the executor path). Validates the §7.1
// edge set, flips status + audit in one tx, then publishes the engine
// feed post-commit (status key, auction control, sweep bookkeeping,
// WS event).
func (s *InstrumentService) Transition(ctx context.Context, actor AdminActor,
	instrumentID int64, op string, in TransitionInput) (*Instrument, error) {

	spec, ok := lifecycleOps[op]
	if !ok {
		return nil, excerrors.New("INVALID_REQUEST", "unknown lifecycle op "+op)
	}
	if DualControlledOp(op) {
		return nil, excerrors.New("DUAL_CONTROL_REQUIRED",
			op+" requires the four-eyes queue — submit via the dual-control endpoint")
	}
	if spec.reasonReq && strings.TrimSpace(in.Reason) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"a reason is mandatory for "+op)
	}
	if err := s.requireRole(ctx, actor.UserID, spec.roles, op); err != nil {
		return nil, err
	}
	in.ClientIP = actor.ClientIP

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	i, from, err := s.transitionTx(ctx, tx, actor, instrumentID, op, spec, in)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	s.publishTransition(i, from, in, actor, "admin")
	return s.withGrace(i), nil
}

// transitionTx is the in-tx core shared by the direct path and the
// dual-control executors: lock → edge check → flip → audit. Callers own
// the tx boundary and the post-commit publication. Returns the updated
// row and the FROM state for the status event.
func (s *InstrumentService) transitionTx(ctx context.Context, tx pgx.Tx,
	actor AdminActor, instrumentID int64, op string, spec opSpec,
	in TransitionInput) (*Instrument, string, error) {

	cur, err := s.lockRow(ctx, tx, instrumentID)
	if err != nil {
		return nil, "", err
	}
	if !lifecycleTransitions[cur.Status][spec.to] {
		return nil, "", excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			fmt.Sprintf("%s → %s is not a permitted transition",
				cur.Status, spec.to))
	}
	// Op-level edge constraints beyond the raw state pair: activate is
	// DRAFT→ACTIVE only; resume is the ACTIVE-ward move for every state
	// that once traded (a DRAFT listing activates, it does not resume).
	if op == LcOpActivate && cur.Status != InstDraft {
		return nil, "", excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			"activate requires DRAFT state — use resume for "+cur.Status)
	}
	if op == LcOpResume && cur.Status == InstDraft {
		return nil, "", excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			"resume requires a live state — use activate for DRAFT")
	}
	var i Instrument
	err = tx.QueryRow(ctx,
		`UPDATE instruments SET status = $2::instrument_status_enum,
		        updated_at = now()
		 WHERE id = $1 RETURNING `+instrumentAdminCols,
		instrumentID, spec.to).
		Scan(&i.ID, &i.Symbol, &i.BaseCurrency, &i.QuoteCurrency,
			&i.InstrumentType, &i.Status, &i.TickSize, &i.LotSize,
			&i.MinOrderQty, &i.MaxOrderQty, &i.SettlementCycle, &i.MaxLeverage,
			&i.UpdatedAt)
	if err != nil {
		return nil, "", excerrors.Wrap("INTERNAL_ERROR", "status update", err)
	}
	if _, _, err := Log(ctx, tx, AuditEntry{
		AdminUserID: actor.UserID,
		Action:      "instrument.lifecycle." + op,
		TargetType:  "instrument",
		TargetID:    &i.ID,
		BeforeState: map[string]any{"status": cur.Status},
		AfterState: map[string]any{
			"status": spec.to, "reason": in.Reason,
			"approver_id": actor.ApproverID,
			"auction":     reopeningAuction(cur.Status, in),
		},
		IPAddress: actor.ClientIP,
	}); err != nil {
		return nil, "", excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	i.Status = spec.to
	return &i, cur.Status, nil
}

// TransitionTx is the dual-control executor entrypoint: runs the
// transition inside the approval transaction. Publication is deferred
// to the reconciler (see Run/Sweep) — Redis cannot join the approval
// tx and the engine feed converges within one sweep interval.
func (s *InstrumentService) TransitionTx(ctx context.Context, tx pgx.Tx,
	actor AdminActor, instrumentID int64, op string, in TransitionInput) error {

	spec, ok := lifecycleOps[op]
	if !ok {
		return excerrors.New("INVALID_REQUEST", "unknown lifecycle op "+op)
	}
	if spec.reasonReq && strings.TrimSpace(in.Reason) == "" {
		return excerrors.New("INVALID_REQUEST",
			"a reason is mandatory for "+op)
	}
	if err := s.requireRole(ctx, actor.UserID, spec.roles, op); err != nil {
		return err
	}
	_, _, err := s.transitionTx(ctx, tx, actor, instrumentID, op, spec, in)
	return err
}

// ---------------------------------------------------------------------------
// Engine feed publication + reconciliation + SUSPENDED grace sweeper
// ---------------------------------------------------------------------------

// publishTransition writes the post-commit side effects for one
// committed transition: status key, auction control key, sweep
// bookkeeping, WS event, venue-cache invalidation. Feed failures are
// logged — the reconciler converges the key on the next tick (the PG
// row is authoritative; the C++ refresher follows the key).
func (s *InstrumentService) publishTransition(i *Instrument, from string,
	in TransitionInput, actor AdminActor, source string) {

	ctx := context.Background()
	to := i.Status
	if to == InstDraft {
		if err := s.feed.DelStatus(ctx, i.Symbol); err != nil {
			s.logf("instrument lifecycle: del status key %s: %v", i.Symbol, err)
		}
	} else if err := s.feed.SetStatus(ctx, i.Symbol, to); err != nil {
		s.logf("instrument lifecycle: set status %s=%s: %v", i.Symbol, to, err)
	}

	// Auction control key: written when a resume routes through CALL;
	// cleared on direct resumes AND on any non-ACTIVE target — a stale
	// CALL must never outlive the state that armed it.
	auction := from != "" && reopeningAuction(from, in)
	if to == InstActive {
		if auction {
			if err := s.feed.SetAuctionCall(ctx, i.Symbol, auctionDeadline(s.now())); err != nil {
				s.logf("instrument lifecycle: set auction key %s: %v", i.Symbol, err)
			}
		} else if err := s.feed.DelAuction(ctx, i.Symbol); err != nil {
			s.logf("instrument lifecycle: clear auction key %s: %v", i.Symbol, err)
		}
	} else if err := s.feed.DelAuction(ctx, i.Symbol); err != nil {
		s.logf("instrument lifecycle: clear auction key %s: %v", i.Symbol, err)
	}

	// Sweep bookkeeping on SUSPENDED entry/exit.
	if to == InstSuspended {
		if err := s.feed.SetSweepDeadline(ctx, i.ID, s.now().Add(SuspendedGrace)); err != nil {
			s.logf("instrument lifecycle: sweep deadline %d: %v", i.ID, err)
		}
	} else if from == InstSuspended {
		if err := s.feed.DelSweepKeys(ctx, i.ID); err != nil {
			s.logf("instrument lifecycle: clear sweep keys %d: %v", i.ID, err)
		}
	}

	if s.ws != nil {
		ev := InstrumentStatusEvent{
			Event: "INSTRUMENT_STATUS", Symbol: i.Symbol, From: from, To: to,
			Reason: in.Reason, ActorID: actor.UserID, ApproverID: actor.ApproverID,
			Auction: auction, Source: source, TsMs: s.now().UnixMilli(),
		}
		if to == InstSuspended {
			ev.GraceDeadline = s.now().Add(SuspendedGrace).UnixMilli()
		}
		s.ws.Publish(InstrumentStatusChannel, ev)
	}
	s.postStatusChange()
}

func (s *InstrumentService) postStatusChange() {
	if s.onStatus != nil {
		s.onStatus()
	}
}

// PublishCommitted re-derives the post-commit side effects for an
// instrument's latest committed lifecycle transition from its audit row —
// the DualControlService.SetOnExecuted path (OpInstrumentResume/Delist),
// where Redis and WS cannot be written inside the approval transaction.
// The audit row is the committed record: it carries before/after status,
// reason, actor, approver and the reopening-auction decision — so the
// emitted side effects exactly mirror a direct-path transition.
func (s *InstrumentService) PublishCommitted(ctx context.Context, instrumentID int64) error {

	var action string
	var beforeRaw, afterRaw []byte
	var auditedAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT action, before_state, after_state, created_at
		  FROM admin_audit_log
		 WHERE target_type = 'instrument' AND target_id = $1
		   AND action LIKE 'instrument.lifecycle.%'
		 ORDER BY id DESC LIMIT 1`, instrumentID).
		Scan(&action, &beforeRaw, &afterRaw, &auditedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no committed transition yet
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "publish committed: audit read", err)
	}
	var before, after map[string]any
	_ = json.Unmarshal(beforeRaw, &before)
	_ = json.Unmarshal(afterRaw, &after)
	from, _ := before["status"].(string)
	to, _ := after["status"].(string)
	reason, _ := after["reason"].(string)
	auction, _ := after["auction"].(bool)
	var approver int64
	switch v := after["approver_id"].(type) {
	case float64:
		approver = int64(v)
	}

	var inst struct {
		symbol    string
		status    string
		updatedAt time.Time
	}
	err = s.pool.QueryRow(ctx,
		`SELECT symbol, status::text, updated_at FROM instruments WHERE id = $1`,
		instrumentID).Scan(&inst.symbol, &inst.status, &inst.updatedAt)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "publish committed: instrument read", err)
	}
	// The committed row is authoritative — publish its state (normally
	// identical to the audit's `to`; a newer transition may have raced in,
	// in which case the newer state wins and the reconciler converges).
	to = inst.status

	if to == InstDraft {
		if err := s.feed.DelStatus(ctx, inst.symbol); err != nil {
			s.logf("publish committed: del status %s: %v", inst.symbol, err)
		}
	} else if err := s.feed.SetStatus(ctx, inst.symbol, to); err != nil {
		s.logf("publish committed: set status %s=%s: %v", inst.symbol, to, err)
	}
	if to == InstActive {
		if auction {
			deadline := auditedAt.Add(ReopenAuctionLen)
			if s.now().Before(deadline) {
				if err := s.feed.SetAuctionCall(ctx, inst.symbol, deadline.UnixNano()); err != nil {
					s.logf("publish committed: auction key %s: %v", inst.symbol, err)
				}
			}
		} else if err := s.feed.DelAuction(ctx, inst.symbol); err != nil {
			s.logf("publish committed: clear auction %s: %v", inst.symbol, err)
		}
	} else if err := s.feed.DelAuction(ctx, inst.symbol); err != nil {
		s.logf("publish committed: clear auction %s: %v", inst.symbol, err)
	}
	if to == InstSuspended {
		if err := s.feed.SetSweepDeadline(ctx, instrumentID, inst.updatedAt.Add(SuspendedGrace)); err != nil {
			s.logf("publish committed: sweep deadline %d: %v", instrumentID, err)
		}
	} else if from == InstSuspended {
		if err := s.feed.DelSweepKeys(ctx, instrumentID); err != nil {
			s.logf("publish committed: clear sweep keys %d: %v", instrumentID, err)
		}
	}
	if s.ws != nil {
		ev := InstrumentStatusEvent{
			Event: "INSTRUMENT_STATUS", Symbol: inst.symbol, From: from,
			To: to, Reason: reason, ApproverID: approver,
			Auction: auction, Source: "admin", TsMs: s.now().UnixMilli(),
		}
		if to == InstSuspended {
			ev.GraceDeadline = inst.updatedAt.Add(SuspendedGrace).UnixMilli()
		}
		s.ws.Publish(InstrumentStatusChannel, ev)
	}
	s.postStatusChange()
	return nil
}

// Sweep performs one reconciler pass (the Run ticker calls it; boot
// reconciliation calls it once at startup):
//  1. Every non-DRAFT instrument's `instrument:status:{symbol}` key is
//     re-published on drift — covers dual-control transitions (whose
//     commits cannot publish in-tx), lost writes and manual edits.
//     DRAFT instruments carry no key; keys for purged rows are deleted.
//  2. SUSPENDED instruments past their 5-minute grace get every resting
//     order mass-cancelled through the orders pipeline (idempotent —
//     the swept marker dedupes, and a lost marker merely re-runs a
//     no-op cancel).
func (s *InstrumentService) Sweep(ctx context.Context) error {
	rows, err := s.pool.Query(ctx,
		`SELECT id, symbol, status::text, updated_at FROM instruments`)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "lifecycle sweep: list instruments", err)
	}
	type row struct {
		id        int64
		symbol    string
		status    string
		updatedAt time.Time
	}
	var insts []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.symbol, &r.status, &r.updatedAt); err != nil {
			rows.Close()
			return excerrors.Wrap("INTERNAL_ERROR", "lifecycle sweep: scan", err)
		}
		insts = append(insts, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "lifecycle sweep: rows", err)
	}

	seen := map[string]bool{}
	for _, inst := range insts {
		seen[inst.symbol] = true
		if inst.status == InstDraft {
			// DRAFT carries no key by contract — remove any stale one.
			if cur, err := s.feed.GetStatus(ctx, inst.symbol); err == nil && cur != "" {
				if err := s.feed.DelStatus(ctx, inst.symbol); err != nil {
					s.logf("lifecycle reconcile: del draft key %s: %v", inst.symbol, err)
				}
			}
			continue
		}
		cur, err := s.feed.GetStatus(ctx, inst.symbol)
		if err != nil {
			s.logf("lifecycle reconcile: get status %s: %v", inst.symbol, err)
			continue
		}
		if cur != inst.status {
			if err := s.feed.SetStatus(ctx, inst.symbol, inst.status); err != nil {
				s.logf("lifecycle reconcile: set status %s=%s: %v",
					inst.symbol, inst.status, err)
				continue
			}
			if s.ws != nil {
				s.ws.Publish(InstrumentStatusChannel, InstrumentStatusEvent{
					Event: "INSTRUMENT_STATUS", Symbol: inst.symbol,
					From: cur, To: inst.status, Source: "reconciler",
					TsMs: s.now().UnixMilli(),
				})
			}
		}
	}
	// Purge: keys with no PG row are deleted (delisting purge step).
	remote, err := s.feed.ScanStatuses(ctx)
	if err != nil {
		s.logf("lifecycle reconcile: scan statuses: %v", err)
	} else {
		for symbol := range remote {
			if !seen[symbol] {
				if err := s.feed.DelStatus(ctx, symbol); err != nil {
					s.logf("lifecycle reconcile: purge status %s: %v", symbol, err)
				}
			}
		}
	}

	// SUSPENDED grace sweeps — and the exit-side cleanup: instruments no
	// longer SUSPENDED get their sweep keys cleared (covers the
	// dual-controlled resume path whose in-tx commit cannot publish).
	for _, inst := range insts {
		if inst.status != InstSuspended {
			if err := s.feed.DelSweepKeys(ctx, inst.id); err != nil {
				s.logf("lifecycle sweep: clear keys %d: %v", inst.id, err)
			}
			continue
		}
		deadline := inst.updatedAt.Add(SuspendedGrace)
		if err := s.feed.SetSweepDeadline(ctx, inst.id, deadline); err != nil {
			s.logf("lifecycle sweep: deadline key %d: %v", inst.id, err)
		}
		if s.now().Before(deadline) {
			continue
		}
		swept, err := s.feed.IsSwept(ctx, inst.id)
		if err != nil {
			s.logf("lifecycle sweep: swept probe %d: %v", inst.id, err)
			continue
		}
		if swept {
			continue
		}
		if s.cancels == nil {
			s.logf("lifecycle sweep: instrument %s past grace — mass-cancel unwired", inst.symbol)
			continue
		}
		n, err := s.cancels.CancelInstrumentOrders(ctx, inst.id, "SUSPENDED_GRACE")
		if err != nil {
			s.logf("lifecycle sweep: mass-cancel %s: %v", inst.symbol, err)
			continue // retry next tick — fail closed on the sweep
		}
		if err := s.feed.MarkSwept(ctx, inst.id); err != nil {
			s.logf("lifecycle sweep: mark swept %d: %v", inst.id, err)
		}
		s.logf("lifecycle sweep: %s grace expired — mass-cancelled %d resting orders",
			inst.symbol, n)
	}
	return nil
}

// Reconcile runs a single Sweep pass — the boot-time publication
// reconciliation (re-publishes every non-DRAFT status key).
func (s *InstrumentService) Reconcile(ctx context.Context) error {
	return s.Sweep(ctx)
}

// Run is the periodic reconciler + grace sweeper loop. interval ≤ 0
// defaults to 1s — the same cadence the C++ status refresher polls at.
func (s *InstrumentService) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sweep(ctx); err != nil {
				s.logf("instrument lifecycle sweep: %v", err)
			}
		}
	}
}
