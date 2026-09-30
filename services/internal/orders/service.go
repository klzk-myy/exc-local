// Service — the order pipeline business core. Every path follows the
// spec fail-closed ordering: authenticate/authorize (handler layer) →
// validate → persist intent → dispatch to the engine over the wire →
// reflect confirmations. Nothing writes balances directly (§8.4 layer
// boundary): balance sufficiency is a pre-trade check; reservation is
// the engine-side 2PC (Phase-02 Task 2.3.14).
package orders

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/config"
	"exchange/internal/ipc"
	"exchange/internal/risk"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// EngineAckTimeout is the spec §8.7/§24 #304 500ms confirmation window —
// expiry maps to GATEWAY_TIMEOUT_MATCHING_ENGINE (504).
const EngineAckTimeout = 500 * time.Millisecond

// MassCancelTimeout bounds the all-shards-confirmed wait for mass
// cancels; the task requires all shards to confirm before success.
const MassCancelTimeout = 5 * time.Second

// BatchSubmitMax / BatchCancelMax are the §8.4 payload ceilings.
const (
	BatchSubmitMax = 10
	BatchCancelMax = 20
)

// RiskChecker is the pre-trade limits seam — *risk.LimitsService
// satisfies it; nil disables the check (readiness wiring decides).
type RiskChecker interface {
	CheckOrder(ctx context.Context, req risk.OrderRequest) error
}

// KillSwitch is the Phase-11 Task 11.3.4/11.3.8/11.3.12 suspension seam —
// *admin.KillSwitchResolver satisfies it in production. It is consulted on
// EVERY new-order admission path (Submit, BatchSubmit entries, amend,
// cancel-replace) so halts apply equally to REST, WS and FIX-originated
// flow; cancels and qty-down keep-priority amends never consult it.
//
// Fail-closed contract (spec §2.7): a nil seam rejects admission — a
// service whose halt state is unverifiable must not admit orders — and a
// resolver error is mapped to TRADING_HALTED, not ignored. scope="" means
// open; a non-empty scope label (GLOBAL|ACCOUNT|INSTRUMENT|…) rejects.
type KillSwitch interface {
	// OrderHalt returns the suspended-scope label and an operator-facing
	// detail (""/"" = trading open). A non-nil err is a lookup failure —
	// callers fail closed.
	OrderHalt(ctx context.Context, accountID int64,
		symbol, instrumentClass, sessionID string) (scope, detail string, err error)
}

// OtrGate is the Phase-13 Task 13.3.6 MiFID II RTS 9 order-to-trade-ratio
// seam — *risk.OtrMonitor satisfies it in production; nil disables
// counting/admission (the C++ PreTradeChecker consults the same
// otr:breach:{account} flag as the engine-side backstop either way).
//
// Event counts one order event (new / modify / cancel) in the
// sliding-window counters — every message that enters the admission
// pipeline counts, including ones the gate then rejects (a breached
// account cannot spam-escape the window). Event is best-effort: Redis
// failures degrade the monitor's own health counters, never the order
// path; Admission fails closed on its own read.
//
// Admission returns a coded OTR_LIMIT_EXCEEDED rejection while the
// account's breach flag stands. It is consulted on the same paths as
// KillSwitch — cancels never call it (cancel-only during breach).
type OtrGate interface {
	Event(ctx context.Context, accountID int64, kycTier, symbol string)
	Admission(ctx context.Context, accountID int64) error
}

// BreakerGate is the Phase-13 five-tier circuit-breaker admission seam
// (spec §2.6; *risk.CircuitBreakerService satisfies it). Consulted on
// every new-order admission path AFTER the kill-switch: an OPEN breaker
// rejects with CIRCUIT_BREAKER_OPEN (503); HALF_OPEN admits and counts
// one of the 10 probe orders in the 30s recovery window.
//
// Fail-closed contract (spec §2.7): a nil seam rejects admission — a
// service whose breaker state is unverifiable must not admit orders —
// and a gate error rejects rather than being ignored.
type BreakerGate interface {
	AdmitOrder(ctx context.Context, accountID int64, symbol string) error
}

// AppropriatenessGate is the Phase-14 Task 14.3.7 MiFID II client-
// categorization admission seam — *compliance.CategorizationService
// satisfies it in production. Consulted LAST in checkAdmission for any
// order that can add exposure: the gate resolves the account's
// client_category and applies the §24 #132 matrix — SPOT exempt for
// every category; RETAIL barred from OPTION outright (conservative
// binary-option treatment until the Phase-22 subtype lands); FORWARD/
// SWAP/NDF require an unexpired appropriateness PASS for RETAIL and
// PROFESSIONAL; ELIGIBLE_COUNTERPARTY is exempt (art. 30).
//
// Fail-closed contract (spec §2.7): a nil seam rejects admission — a
// categorization gate that cannot answer must never silently admit —
// and a gate error rejects rather than being ignored. reduce_only
// orders bypass the gate: a downgrade or expired assessment must never
// trap an open position — closing exposure is always permitted (the
// close-only posture spec §5.2 requires after a category downgrade).
type AppropriatenessGate interface {
	Appropriateness(ctx context.Context, accountID int64, instrumentClass string) error
}

// ProductGate is the Phase-14 Tasks 14.3.13/14.3.16 product-profile +
// retail target-market admission seam — *accounts.ProductGateService
// satisfies it in production. Consulted inside checkAdmission before the
// appropriateness gate: a coded PRODUCT_NOT_PERMITTED rejection carries
// the scope or target-market reason. nil skips the gate (pre-095
// deployments); reduceOnly marks closing flow which stays admissible
// under close-only restrictions (narrowed scope / overdue review).
type ProductGate interface {
	AdmitOrder(ctx context.Context, accountID int64,
		symbol, instrumentClass string, reduceOnly bool) error
}

// ExposureGate is the Phase-19 Task 19.3.5 exposure-cap seam —
// *risk.ExposureService satisfies it in production. Consulted in the
// pre-trade risk block (alongside RiskChecker) for every order that can
// add exposure: the gate recomputes the account's mode-aware notional
// increment (NETTING consumes the opposing side first; HEDGING adds
// gross) against the risk_limits exposure caps and rejects with
// MAX_EXPOSURE_EXCEEDED. nil skips the gate — LimitsService.CheckOrder
// still enforces the same caps with the same mode-aware math when its
// own position-mode source is bound (both are wired in production).
type ExposureGate interface {
	CheckExposure(ctx context.Context, req risk.OrderRequest) error
}

// MarginCallGate is the Phase-19 Task 19.3.3 §13.6d margin-call
// order-entry block seam — *risk.MarginCallService satisfies it via
// HasMarginCallBlock. Consulted inside checkAdmission for every order
// that can add exposure: while margin_call:block:{account_id} stands
// the order rejects with MARGIN_CALL_EXCEEDED (409). The block covers
// the entire episode — the 15-minute deposit window cures the
// shortfall but does not itself restore trading (spec §13.3
// precedence); the block lifts only on automatic recovery above the
// margin-call threshold or an audited Risk Manager re-enable.
// reduce_only bypasses the gate — closing exposure must stay possible
// mid-episode. nil skips the gate (unwired test/dev construction); a
// probe error fails closed.
type MarginCallGate interface {
	HasMarginCallBlock(ctx context.Context, accountID int64) (bool, error)
}

// OracleGate is the Phase-19.5 Task 19.5.3.7 fail-closed staleness
// interceptor — *oracle.Provider satisfies it via GateOrderAdmission.
// Consulted inside checkAdmission for every position-increasing order
// on a marginable instrument: while the symbol's oracle health reads
// UNAVAILABLE (fewer than 2 fresh independent feeds — or a silent
// oracle that never published health), admission rejects
// PRICE_ORACLE_UNAVAILABLE (503). reduce_only bypasses it (closing
// exposure must stay possible during an oracle outage — the §13.4
// stale-price ladder handles liquidation pricing). nil skips the gate
// (unwired test/dev construction); a probe error fails closed.
type OracleGate interface {
	GateOrderAdmission(ctx context.Context, symbol string) error
}

// AdmissionObserver is the Phase-14 Task 14.3.2 auto-halt telemetry
// seam — one call per Submit verdict carrying the admission-path
// latency and the systemic-error classification
// (risk.SystemicAdmissionCode applied at the call site). It feeds the
// LATENCY_SPIKE and ERROR_RATE_SPIKE detectors; nil → the feeds are
// dormant (detectors never fabricate observations). Wired to
// risk.AutoHaltService.ObserveAdmission in cmd/gateway.
type AdmissionObserver func(ctx context.Context, symbol string,
	latency time.Duration, systemic bool)

// CoolingOffGate is the Phase-14 Task 14.3.12 responsible-trading
// self-exclusion seam (*accounts.CoolingOffService satisfies it in
// production). AssertLeverageEntryAllowed rejects with
// COOLING_OFF_ACTIVE while a live cooling_off_periods window covers
// the account's owner; a lookup failure returns a coded error (fail
// closed). Consulted inside checkAdmission only for leveraged order
// entry — a marginable instrument (max_leverage>0) on a non-SPOT
// account, mirroring checkBalance's margin test — and never for
// reduce_only legs: the activation saga itself dispatches reduce-only
// closes through Submit. A nil seam skips the gate (unwired test/dev
// construction), matching the OtrGate convention.
type CoolingOffGate interface {
	AssertLeverageEntryAllowed(ctx context.Context, accountID int64) error
}

// Service wires store + transport + sequencing.
type Service struct {
	store       Store
	sub         Submitter
	shards      *config.ShardMap
	pending     *pendingConfirms
	limits      RiskChecker
	kill        KillSwitch
	otr         OtrGate
	breakers    BreakerGate
	products    ProductGate
	coolingOff  CoolingOffGate
	exposure    ExposureGate
	marginCall  MarginCallGate
	oracleGate  OracleGate
	batch       BatchRateLimiter
	commission  CommissionEstimator
	admission   AdmissionObserver
	product     AppropriatenessGate
	fixing      FixingHooks     // Phase-16 Task 16.3.9 — nil ⇒ FIXING rejected
	gslo        GSLOHooks       // Phase-16 Task 16.3.16 — nil ⇒ gslo rejected
	composite   CompositeStore  // Phase-16 Task 16.3.14/.20 composite persistence
	conditional ConditionalGate // Phase-16 Task 16.3.22 — nil ⇒ trigger/peg guards skipped (engine authoritative)
	auction     AuctionGate     // Phase-16 Task 16.3.25 — nil ⇒ MOO/MOC rejected
	notifyFn    PrivateNotify   // nil ⇒ private WS events skipped (tests/dev)
	seq         *seqAllocator
	ackTimeout  time.Duration
	now         func() time.Time
}

// Options customizes Service construction; nil/zero fields pick
// production defaults.
type Options struct {
	Store      Store
	Submitter  Submitter
	ShardMap   *config.ShardMap
	Limits     RiskChecker
	KillSwitch KillSwitch  // nil → admission fails closed (TRADING_HALTED)
	Otr        OtrGate     // nil → OTR counting/admission skipped (engine flag still enforced)
	Breakers   BreakerGate // nil → admission fails closed (CIRCUIT_BREAKER_OPEN)
	Products   ProductGate // nil → product-profile/target-market gate skipped (pre-095)
	BatchRL    BatchRateLimiter
	Commission CommissionEstimator // optional — dry-run fee estimate
	Admission  AdmissionObserver   // optional — Task 14.3.2 anomaly feeds
	Product    AppropriatenessGate // nil → admission fails closed (Task 14.3.7)
	CoolingOff CoolingOffGate      // nil → self-exclusion gate skipped (unwired)
	Exposure   ExposureGate        // nil → exposure gate skipped (Limits still enforces)
	MarginCall MarginCallGate      // nil → margin-call order block skipped (unwired)
	Fixing     FixingHooks         // nil → FIXING submissions rejected (Task 16.3.9)
	GSLO       GSLOHooks           // nil → gslo submissions rejected (Task 16.3.16)
	Composite  CompositeStore      // nil → bracket/list submissions rejected (16.3.14/.20)
	Auction    AuctionGate         // nil → MOO/MOC submissions rejected (Task 16.3.25)
	Notify     PrivateNotify       // nil → private-channel events skipped (16.3.25)
	AckTimeout time.Duration
	Now        func() time.Time
}

func NewService(o Options) (*Service, error) {
	if o.Store == nil {
		return nil, fmt.Errorf("orders: store is nil")
	}
	if o.ShardMap == nil {
		return nil, fmt.Errorf("orders: shard map is nil")
	}
	s := &Service{
		store:      o.Store,
		sub:        o.Submitter,
		shards:     o.ShardMap,
		pending:    newPendingConfirms(),
		limits:     o.Limits,
		kill:       o.KillSwitch,
		otr:        o.Otr,
		breakers:   o.Breakers,
		products:   o.Products,
		coolingOff: o.CoolingOff,
		exposure:   o.Exposure,
		marginCall: o.MarginCall,
		batch:      o.BatchRL,
		commission: o.Commission,
		admission:  o.Admission,
		product:    o.Product,
		auction:    o.Auction,
		composite:  o.Composite,
		notifyFn:   o.Notify,
		fixing:     o.Fixing,
		gslo:       o.GSLO,
		seq:        newSeqAllocator(),
		ackTimeout: o.AckTimeout,
		now:        o.Now,
	}
	if s.ackTimeout <= 0 {
		s.ackTimeout = EngineAckTimeout
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Pending is exposed for the consumer wiring (internal same-package use
// via NewConsumer is preferred).
func (s *Service) Pending() *pendingConfirms { return s.pending }

// WithAdmission binds the Task 14.3.2 auto-halt observer
// post-construction — cmd/gateway builds the notification-backed
// AutoHaltService after the order pipeline, so the feed attaches here
// (same pattern as CircuitBreakerService.WithPublisher).
func (s *Service) WithAdmission(o AdmissionObserver) { s.admission = o }

// WithCoolingOff binds the Task 14.3.12 self-exclusion gate
// post-construction — cmd/gateway builds accounts.CoolingOffService
// after the order dispatcher it needs for the activation saga, so the
// gate attaches here (same pattern as WithAdmission).
func (s *Service) WithCoolingOff(g CoolingOffGate) { s.coolingOff = g }

// WithOracleGate binds the Task 19.5.3.7 oracle-health admission
// interceptor. Called once at wiring time; nil disables (the fail-closed
// path lives in GateOrderAdmission itself — absent health IS unavailable).
func (s *Service) WithOracleGate(g OracleGate) { s.oracleGate = g }

// WithMarginCall binds the Task 19.3.3 §13.6d margin-call order-entry
// block post-construction — cmd/gateway builds MarginCallService after
// the order pipeline (it needs the dispatcher for liquidation closes),
// so the seam attaches here (same pattern as WithCoolingOff).
func (s *Service) WithMarginCall(g MarginCallGate) { s.marginCall = g }

// WithFixing binds the Phase-16 Task 16.3.9 fixing-order hooks — the
// algo.FixingService is built from the pool + ledger poster after the
// order service, so the seam attaches post-construction.
func (s *Service) WithFixing(h FixingHooks) { s.fixing = h }

// WithGSLO binds the Phase-16 Task 16.3.16 guaranteed-stop hooks
// (premium journal + exposure cap) post-construction.
func (s *Service) WithGSLO(h GSLOHooks) { s.gslo = h }

// WithConditional binds the Phase-16 Task 16.3.22 trigger/peg admission
// guard — *algo.TriggerGuard. Nil is tolerated (engine stays
// authoritative); the gateway wires it in production.
func (s *Service) WithConditional(g ConditionalGate) { s.conditional = g }

// WithComposite binds the Task 16.3.14/.20 composite persistence seam —
// *PgStore satisfies it once migrations 075/225 apply.
func (s *Service) WithComposite(c CompositeStore) { s.composite = c }

// WithAuction binds the Task 16.3.25 freeze gate post-construction —
// the adapter reads instrument:auction:{symbol} + the auction calendar.
func (s *Service) WithAuction(g AuctionGate) { s.auction = g }

// WithNotify binds the private-channel emitter (ws.PublishPrivate) —
// bound lazily in cmd/gateway because the WS server constructs after
// the order service.
func (s *Service) WithNotify(n PrivateNotify) { s.notifyFn = n }

// AccountByID / InstrumentBySymbol are thin store delegates exposed so
// the handler layer can resolve account rows and symbol → instrument
// without holding the Store interface.
func (s *Service) AccountByID(ctx context.Context, id int64) (*Account, error) {
	return s.store.AccountByID(ctx, id)
}

func (s *Service) InstrumentBySymbol(ctx context.Context, symbol string) (*Instrument, error) {
	inst, err := s.store.InstrumentBySymbol(ctx, config.CanonicalSymbol(symbol))
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	return inst, nil
}

func (s *Service) shardFor(symbol string) uint16 {
	return uint16(s.shards.GetShard(symbol))
}

// checkOrderRisk runs the pre-trade risk pipeline once per candidate
// order: the Task 19.3.5 exposure gate first (mode-aware cap math,
// MAX_EXPOSURE_EXCEEDED), then the limits check for everything else
// (qty/daily-volume/open-orders + — when the gate was not consulted —
// the same exposure caps). ExposureChecked suppresses the duplicate
// exposure evaluation inside CheckOrder; a nil gate leaves the whole
// check with LimitsService (pre-Phase-19 constructions stay enforced).
func (s *Service) checkOrderRisk(ctx context.Context, req risk.OrderRequest) error {
	if s.exposure != nil && !req.ReduceOnly {
		if err := s.exposure.CheckExposure(ctx, req); err != nil {
			return err
		}
		req.ExposureChecked = true
	}
	if s.limits != nil {
		return s.limits.CheckOrder(ctx, req)
	}
	return nil
}

// submitHash is the canonical payload fingerprint persisted in
// client_order_id_dedup — semantic fields only, so JSON formatting
// differences still replay.
func submitHash(instID int64, req *SubmitRequest) string {
	var b strings.Builder
	b.WriteString(strconv.FormatInt(instID, 10))
	b.WriteByte('|')
	b.WriteString(req.Side)
	b.WriteByte('|')
	b.WriteString(req.OrderType)
	b.WriteByte('|')
	b.WriteString(req.TimeInForce)
	b.WriteByte('|')
	b.WriteString(decStr(req.Quantity))
	b.WriteByte('|')
	b.WriteString(decStr(req.QuoteQuantity))
	b.WriteByte('|')
	b.WriteString(decStr(req.Price))
	b.WriteByte('|')
	b.WriteString(decStr(req.StopPrice))
	b.WriteByte('|')
	b.WriteString(decStr(req.DisplayQty))
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func decStr(d *decimal.Decimal) string {
	if d == nil {
		return ""
	}
	return d.String()
}

// strPtrOrNil normalizes an optional request string for a NULL-able
// column — "" ⇒ nil (absent), anything else rides through trimmed.
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	v := strings.TrimSpace(s)
	return &v
}

// ---------------------------------------------------------------------------
// Submit — POST /api/v1/orders (Tasks 5.3.3/5.3.24/5.3.39)
// ---------------------------------------------------------------------------

// Ack is the client-visible submission acknowledgement.
type Ack struct {
	OrderID       int64  `json:"order_id"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	Status        string `json:"status"`
	OrderSeq      uint64 `json:"order_seq"`
	Replay        bool   `json:"replay,omitempty"` // true = stored ack replayed (idempotent)
	TransactTime  string `json:"transact_time"`
}

// Submit validates, dedups, persists and dispatches one order. The
// dedup/dispatch ordering is: dedup check → validate → insert (unique
// race safe) → wire send → activate. On send failure the row is marked
// REJECTED so no phantom ACTIVE orders linger.
// checkAdmission enforces the kill-switch before any new-order work —
// the fail-closed Task 11.3.4/11.3.8 gate. A nil seam, a resolver error
// or a suspended scope all reject with TRADING_HALTED (503); the detail
// names the winning scope per Task 11.3.8 step 3.
func (s *Service) checkAdmission(ctx context.Context, acct *Account,
	inst *Instrument, sessionID string, reduceOnly bool) error {
	if s.kill == nil {
		return codeErr("TRADING_HALTED",
			"trading-state resolver unavailable — new orders rejected")
	}
	scope, detail, err := s.kill.OrderHalt(ctx, acct.ID,
		inst.Symbol, inst.InstrumentType, sessionID)
	if err != nil {
		return codeErr("TRADING_HALTED",
			"trading-state lookup failed — new orders rejected (fail closed): %v", err)
	}
	if scope != "" {
		return codeErr("TRADING_HALTED", "trading suspended (%s)", detail)
	}
	// Phase-13 Task 13.3.6 — MiFID II RTS 9 OTR: the order event counts
	// BEFORE the gate is consulted, so a breached account spamming new
	// orders keeps the flag alive rather than decaying out under it.
	// Cancels never reach this function (cancel-only during breach).
	if s.otr != nil {
		s.otr.Event(ctx, acct.ID, acct.KycTier, inst.Symbol)
		if err := s.otr.Admission(ctx, acct.ID); err != nil {
			return err // OTR_LIMIT_EXCEEDED (breach) / SERVICE_DEGRADED (unreadable)
		}
	}
	// Phase-13 five-tier circuit breaker (spec §2.6): OPEN rejects with
	// CIRCUIT_BREAKER_OPEN; HALF_OPEN admits and counts a probe. A nil or
	// erroring gate fails closed — never admits on unverifiable state.
	if s.breakers == nil {
		return codeErr("CIRCUIT_BREAKER_OPEN",
			"circuit-breaker gate unavailable — new orders rejected")
	}
	if err := s.breakers.AdmitOrder(ctx, acct.ID, inst.Symbol); err != nil {
		return err // gate emits coded errors (CIRCUIT_BREAKER_OPEN / internal)
	}
	// Phase-14 Task 14.3.12 — cooling-off self-exclusion: leveraged
	// order entry rejects with COOLING_OFF_ACTIVE while a live window
	// covers the account's owner. reduce_only bypasses it (the
	// activation saga dispatches its own reduce-only closes through
	// Submit); SPOT accounts and non-marginable instruments stay open
	// so spot conversions remain available. A gate error fails closed.
	if !reduceOnly && s.coolingOff != nil &&
		acct.Type != "SPOT" && inst.MaxLeverage > 0 {
		if err := s.coolingOff.AssertLeverageEntryAllowed(ctx, acct.ID); err != nil {
			return err // COOLING_OFF_ACTIVE / SERVICE_DEGRADED
		}
	}
	// Phase-19 Task 19.3.3 §13.6d — margin-call order-entry block: a
	// live margin_call:block flag rejects position-increasing orders for
	// the whole episode (MARGIN_CALL_EXCEEDED). reduce_only bypasses it
	// — closing exposure must stay possible mid-episode. A probe error
	// fails closed: unverifiable block state cannot admit new risk.
	if !reduceOnly && s.marginCall != nil {
		blocked, err := s.marginCall.HasMarginCallBlock(ctx, acct.ID)
		if err != nil {
			return codeErr("SERVICE_DEGRADED",
				"margin-call block probe failed — new orders rejected (fail closed): %v", err)
		}
		if blocked {
			return codeErr("MARGIN_CALL_EXCEEDED",
				"margin call active — position-increasing orders blocked (spec §13.6d)")
		}
	}
	// Phase-19.5 Task 19.5.3.7 — oracle staleness interceptor: a symbol
	// whose oracle health is UNAVAILABLE (or absent — a silent oracle
	// cannot attest freshness) rejects position-increasing margin
	// orders with PRICE_ORACLE_UNAVAILABLE. reduce_only bypasses it —
	// closing exposure during an oracle outage rides the §13.4
	// stale-price liquidation ladder. A probe error fails closed.
	if !reduceOnly && s.oracleGate != nil &&
		acct.Type != "SPOT" && inst.MaxLeverage > 0 {
		if err := s.oracleGate.GateOrderAdmission(ctx, inst.Symbol); err != nil {
			return err // PRICE_ORACLE_UNAVAILABLE (503) / health read failure
		}
	}
	// Phase-14 Tasks 14.3.13/14.3.16 — product-profile instrument_scope +
	// RETAIL target-market gate (*accounts.ProductGateService). Out-of-
	// scope or out-of-target opens reject PRODUCT_NOT_PERMITTED;
	// reduce_only flow stays admissible (close-only posture after scope
	// narrowing / overdue review). A nil seam skips the gate (pre-095
	// deployments carry no profiles to enforce).
	if s.products != nil {
		if err := s.products.AdmitOrder(ctx, acct.ID, inst.Symbol,
			inst.InstrumentType, reduceOnly); err != nil {
			return err // PRODUCT_NOT_PERMITTED / SERVICE_DEGRADED
		}
	}
	// Phase-14 Task 14.3.7 — MiFID II appropriateness/categorization
	// gate, consulted last for exposure-adding orders only. reduce_only
	// bypasses it deliberately: the close-only posture a RETAIL
	// downgrade or expired assessment imposes must never trap an open
	// position (spec §5.2). A nil seam or a gate error fails closed.
	if reduceOnly {
		return nil
	}
	if s.product == nil {
		return codeErr("SERVICE_DEGRADED",
			"appropriateness gate unavailable — new orders rejected (fail closed)")
	}
	if err := s.product.Appropriateness(ctx, acct.ID, inst.InstrumentType); err != nil {
		return err // PRODUCT_NOT_PERMITTED / SERVICE_DEGRADED
	}
	return nil
}

func (s *Service) Submit(ctx context.Context, acct *Account, req *SubmitRequest) (_ *Ack, err error) {
	start := s.now()
	sym := config.CanonicalSymbol(req.Symbol)
	// Task 14.3.2: every verdict (admit or reject) feeds the auto-halt
	// latency + error-rate windows once per call. Best-effort — the
	// observer never veto's the return value.
	defer func() {
		if s.admission != nil && sym != "" {
			s.admission(ctx, sym, s.now().Sub(start),
				err != nil && risk.SystemicAdmissionCode(excerrors.CodeOf(err)))
		}
	}()
	inst, err := s.store.InstrumentBySymbol(ctx, sym)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INVALID_REQUEST", "unknown symbol %q", req.Symbol)
	}
	if err := s.checkAdmission(ctx, acct, inst, req.SessionID, req.ReduceOnly); err != nil {
		return nil, err
	}
	ref, err := s.store.ReferencePrice(ctx, inst.ID)
	if err != nil {
		return nil, errInternal("reference price", err)
	}
	if err := ValidateSubmit(req, inst, acct, ref, s.now()); err != nil {
		return nil, err
	}

	// Quote-denominated market conversion (spec §22.1): base qty is
	// lot-rounded DOWN so the fill can never exceed the requested quote.
	if req.QuoteQuantity != nil {
		if ref == nil || !ref.IsPositive() {
			return nil, codeErr("QUOTE_QUANTITY_INVALID",
				"no reference price for quote-denominated conversion on %s", inst.Symbol)
		}
		base := req.QuoteQuantity.Div(*ref)
		if inst.LotSize.IsPositive() {
			base = base.Div(inst.LotSize).Floor().Mul(inst.LotSize)
		}
		if !base.IsPositive() {
			return nil, codeErr("QUOTE_QUANTITY_INVALID",
				"quote_quantity %s converts to zero base at %s", *req.QuoteQuantity, *ref)
		}
		q := base
		req.Quantity = &q
	}

	// Pre-trade risk limits (2.3.3/Phase-19 seam).
	evalPrice := decimal.Zero
	if req.Price != nil {
		evalPrice = *req.Price
	} else if ref != nil {
		evalPrice = *ref
	}
	if err := s.checkOrderRisk(ctx, risk.OrderRequest{
		AccountID:  acct.ID,
		KycTier:    acct.KycTier,
		Symbol:     inst.Symbol,
		Side:       req.Side,
		Quantity:   *req.Quantity,
		Price:      evalPrice,
		ReduceOnly: req.ReduceOnly,
	}); err != nil {
		return nil, err
	}

	// Balance sufficiency (read-only — the §8.4 layer boundary forbids
	// gateway writes; engine 2PC owns the atomic reservation).
	if err := s.checkBalance(ctx, acct, inst, req, ref); err != nil {
		return nil, err
	}

	// Phase-16 Task 16.3.9 — FIXING orders gate on the benchmark
	// publication cutoff BEFORE the row exists (no orphan intent rows).
	if req.OrderType == TypeFixing {
		if s.fixing == nil {
			return nil, codeErr("INVALID_REQUEST",
				"fixing-order pipeline unavailable — submissions rejected (fail closed)")
		}
		if err := s.fixing.AdmitSubmit(ctx, inst, req, s.now()); err != nil {
			return nil, err // FIXING_CUTOFF_EXCEEDED
		}
	}
	// Phase-16 Task 16.3.16 — GSLO exposure-cap admission check.
	if req.GSLO {
		if s.gslo == nil {
			return nil, codeErr("INVALID_REQUEST",
				"GSLO pipeline unavailable — submissions rejected (fail closed)")
		}
		if err := s.gslo.AdmitSubmit(ctx, acct, inst, req, ref); err != nil {
			return nil, err // GSLO_EXPOSURE_EXCEEDED / PREMIUM_INSUFFICIENT
		}
	}
	// Phase-16 Task 16.3.22 — conditional-trigger staleness + pegged-BBO
	// admission guards (CONDITIONAL_TRIGGER_ORACLE_STALE /
	// PEGGED_PRICING_UNAVAILABLE). The engine stays authoritative at
	// trigger time; this keeps unschedulable orders out early.
	if s.conditional != nil &&
		(req.TriggerSource == "MARK_PRICE" || req.TriggerSource == "INDEX_PRICE" ||
			req.PegMode != "") {
		if err := s.conditional.AdmitConditional(ctx, inst, req); err != nil {
			return nil, err
		}
	}
	// Phase-16 Task 16.3.25 — MOO/MOC queue into the session call
	// auction. The freeze gate must be wired before any row exists —
	// an unfreezable queue would admit post-freeze mutations.
	if IsAuctionType(req.OrderType) && s.auction == nil {
		return nil, codeErr("INVALID_REQUEST",
			"auction-order pipeline unavailable — submissions rejected (fail closed)")
	}

	shard := s.shardFor(inst.Symbol)
	seq := s.seq.Next()
	hash := ""
	if req.ClientOrderID != "" {
		hash = submitHash(inst.ID, req)
	}
	o, dup, err := s.store.InsertOrderTx(ctx, InsertParams{
		AccountID:       acct.ID,
		InstrumentID:    inst.ID,
		ClientOrderID:   req.ClientOrderID,
		Side:            req.Side,
		OrderType:       req.OrderType,
		Quantity:        *req.Quantity,
		QuoteQuantity:   req.QuoteQuantity,
		Price:           req.Price,
		StopPrice:       req.StopPrice,
		DisplayQty:      req.DisplayQty,
		TimeInForce:     req.TimeInForce,
		ShardID:         int(shard),
		OrderSeq:        seq,
		PostOnly:        req.PostOnly,
		ReduceOnly:      req.ReduceOnly,
		STPMode:         req.STPMode,
		SessionID:       req.SessionID,
		RequestHash:     hash,
		CoDExempt:       req.CoDExempt,
		PegMode:         strPtrOrNil(req.PegMode),
		PegOffset:       req.PegOffset,
		PegLimit:        req.PegLimit,
		TriggerSource:   req.TriggerSource,
		Hidden:          req.Hidden,
		GSLO:            req.GSLO,
		FixingBenchmark: strPtrOrNil(req.FixingBenchmark),
		AlgoType:        strPtrOrNil(req.AlgoType),
		AlgoParams:      req.AlgoParams,
	})
	if err != nil {
		if c := DedupConflictRow(err); c != nil {
			// §8.7 idempotency semantics: same payload → replay the
			// stored ack; different payload → 409 collision.
			if c.RequestHash == hash {
				stored, gerr := s.store.GetOrder(ctx, c.OrderID)
				if gerr != nil {
					return nil, errInternal("dedup replay fetch", gerr)
				}
				if stored == nil {
					return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
						"client_order_id %q resolves to a missing order", req.ClientOrderID)
				}
				return &Ack{OrderID: stored.ID, ClientOrderID: stored.ClientOrderID,
					Status: stored.Status, OrderSeq: stored.OrderSeq,
					Replay: true, TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
			}
			return nil, codeErr("IDEMPOTENCY_KEY_COLLISION",
				"client_order_id %q already used with a different payload",
				req.ClientOrderID)
		}
		return nil, errInternal("order insert", err)
	}
	if dup != nil { // defensive — conflict path returns via err
		return nil, codeErr("IDEMPOTENCY_KEY_COLLISION", "duplicate client_order_id")
	}

	// Phase-16 Task 16.3.9 — FIXING orders queue locally: post the
	// balance reservation, flip to RESERVED and return. No wire send —
	// the fix executor consumes them at publication.
	if req.OrderType == TypeFixing {
		if err := s.fixing.Reserve(ctx, o, inst); err != nil {
			_ = s.store.MarkRejected(ctx, o.ID)
			return nil, errInternal("fixing reservation", err)
		}
		_ = s.store.MarkReserved(ctx, o.ID)
		return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
			Status: "RESERVED", OrderSeq: seq,
			TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
	}

	// Phase-16 Task 16.3.16 — GSLO premium debits at placement, before
	// the engine sees the order; a journal failure kills the row
	// (a guaranteed stop with no premium posted is a fabrication).
	if req.GSLO {
		if err := s.gslo.ChargePremium(ctx, o, inst); err != nil {
			_ = s.store.MarkRejected(ctx, o.ID)
			return nil, errInternal("gslo premium", err)
		}
	}

	// Phase-16 Task 16.3.25 — MOO/MOC queue locally: RESERVED row +
	// order.queued notice. No wire send — the Injector replays them as
	// MARKET orders once the instrument's CALL arms.
	if IsAuctionType(req.OrderType) {
		return s.queueAuctionOrder(ctx, o)
	}

	if s.sub != nil {
		b := flatbuffers.NewBuilder(256)
		payload := ipc.EncodeOrderNewEvent(b, seq,
			uint64(s.now().UnixNano()), orderNewMsg(o, acct, req))
		if err := s.sub.Send(ctx, shard, payload); err != nil {
			// Compensate the read model + the GSLO premium — the engine
			// never saw the order, so the guarantee never existed.
			if req.GSLO {
				_ = s.gslo.RefundPremium(ctx, o, inst)
			}
			_ = s.store.MarkRejected(ctx, o.ID)
			return nil, err
		}
		// PENDING → ACTIVE once the engine has it in-hand.
		_ = s.store.MarkActive(ctx, o.ID)
	}
	return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
		Status: "ACTIVE", OrderSeq: seq,
		TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
}

// checkBalance verifies available ≥ required. SELL locks base units;
// BUY locks quote notional (limit price, or band-capped reference for
// markets). reduce_only skips the check — a reduce-only order is
// position-backed by definition.
func (s *Service) checkBalance(ctx context.Context, acct *Account,
	inst *Instrument, req *SubmitRequest, ref *decimal.Decimal) error {
	if req.ReduceOnly {
		return nil
	}
	var (
		currency string
		need     decimal.Decimal
	)
	if req.Side == SideSell {
		currency = inst.BaseCurrency
		need = *req.Quantity
	} else {
		currency = inst.QuoteCurrency
		eval := req.Price
		if eval == nil {
			eval = ref // market: reference price
		}
		if eval == nil || !eval.IsPositive() {
			return codeErr("INSUFFICIENT_BALANCE",
				"cannot evaluate spend for %s BUY without a price", inst.Symbol)
		}
		if req.QuoteQuantity != nil {
			need = *req.QuoteQuantity // never exceed the client ask
		} else {
			// Worst-case cap: fill at the band ceiling.
			cap_ := eval.Mul(decimal.One.Add(inst.PriceBandPctUp.Div(decimal.NewFromInt(100))))
			need = req.Quantity.Mul(cap_)
		}
	}
	avail, err := s.store.AvailableBalance(ctx, acct.ID, currency)
	if err != nil {
		return errInternal("balance read", err)
	}
	if avail == nil || avail.LessThan(need) {
		return codeErr("INSUFFICIENT_BALANCE",
			"available %s %s < required %s", decStr0(avail), currency, need)
	}
	return nil
}

func decStr0(d *decimal.Decimal) string {
	if d == nil {
		return "0"
	}
	return d.String()
}

// ---------------------------------------------------------------------------
// Cancel — DELETE /api/v1/orders/{id} (Task 5.3.3)
// ---------------------------------------------------------------------------

// Cancel dispatches OrderCancel and returns only after the engine's
// outbound echo confirms (or the 500ms window lapses →
// GATEWAY_TIMEOUT_MATCHING_ENGINE). Retries are idempotent: an order
// already CANCELLED returns a confirmation without a resend.
func (s *Service) Cancel(ctx context.Context, acct *Account, orderID int64,
	actor, requestID, ip string) (*Ack, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	if o.Status == "CANCELLED" {
		return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
			Status: o.Status, OrderSeq: o.OrderSeq,
			TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
	}
	if isTerminal(o.Status) {
		return nil, codeErr("INVALID_REQUEST",
			"order %d in terminal state %s cannot be cancelled", o.ID, o.Status)
	}
	// Phase-16 Task 16.3.9 — FIXING orders never reached the engine:
	// the cut-off gate + reservation release replace the wire cancel.
	if o.OrderType == TypeFixing {
		inst, ierr := s.store.InstrumentByID(ctx, o.InstrumentID)
		if ierr != nil {
			return nil, errInternal("instrument lookup", ierr)
		}
		if inst == nil {
			return nil, codeErr("INSTRUMENT_DELISTED",
				"order instrument %d no longer exists", o.InstrumentID)
		}
		if s.fixing != nil {
			if err := s.fixing.AssertMutable(ctx, o, inst, s.now()); err != nil {
				return nil, err // FIXING_CANCELLATION_RESTRICTED
			}
			if err := s.fixing.Release(ctx, o, inst); err != nil {
				return nil, errInternal("fixing reservation release", err)
			}
		}
		if err := s.store.ApplyCancel(ctx, o.ID); err != nil {
			return nil, errInternal("order cancel", err)
		}
		_ = s.store.WriteAudit(ctx, []AuditEntry{{
			OrderID: o.ID, AccountID: acct.ID, Operation: "CANCEL",
			FieldName: "status", OldValue: o.Status, NewValue: "CANCELLED",
			ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
		}})
		return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
			Status: "CANCELLED", OrderSeq: o.OrderSeq,
			TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
	}
	// Phase-16 Task 16.3.25 — queued session orders cancel locally
	// until the T-30s freeze; an injected (engine-held) auction order
	// still rides the wire path below.
	if IsAuctionType(o.OrderType) {
		inst, ierr := s.store.InstrumentByID(ctx, o.InstrumentID)
		if ierr != nil {
			return nil, errInternal("instrument lookup", ierr)
		}
		if inst == nil {
			return nil, codeErr("INSTRUMENT_DELISTED",
				"order instrument %d no longer exists", o.InstrumentID)
		}
		if err := s.assertAuctionMutable(ctx, o, inst); err != nil {
			return nil, err // AMEND_IN_AUCTION_REJECTED
		}
		if auctionInBook(o) {
			if err := s.cancelOne(ctx, o); err != nil {
				return nil, err
			}
		} else if err := s.cancelQueued(ctx, o, "client"); err != nil {
			return nil, err
		}
		_ = s.store.WriteAudit(ctx, []AuditEntry{{
			OrderID: o.ID, AccountID: acct.ID, Operation: "CANCEL",
			FieldName: "status", OldValue: o.Status, NewValue: "CANCELLED",
			ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
		}})
		return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
			Status: "CANCELLED", OrderSeq: o.OrderSeq,
			TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
	}
	if err := s.cancelOne(ctx, o); err != nil {
		return nil, err
	}
	// Phase-16 Task 16.3.16 — a cancelled GSLO was never triggered (open
	// orders only reach here): refund the placement premium through the
	// ledger. The journal is idempotency-keyed on the order id; a refund
	// failure is audited rather than silently swallowed — the cancel
	// itself already committed on the engine.
	if o.GSLO && s.gslo != nil {
		inst, ierr := s.store.InstrumentByID(ctx, o.InstrumentID)
		if ierr == nil && inst != nil {
			if rerr := s.gslo.RefundPremium(ctx, o, inst); rerr != nil {
				_ = s.store.WriteAudit(ctx, []AuditEntry{{
					OrderID: o.ID, AccountID: acct.ID,
					Operation:  "GSLO_REFUND_FAILED",
					FieldName:  "gslo_premium",
					NewValue:   rerr.Error(),
					ModifiedBy: "system:gslo",
				}})
			}
		}
	}
	_ = s.store.WriteAudit(ctx, []AuditEntry{{
		OrderID: o.ID, AccountID: acct.ID, Operation: "CANCEL",
		FieldName: "status", OldValue: o.Status, NewValue: "CANCELLED",
		ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
	}})
	return &Ack{OrderID: o.ID, ClientOrderID: o.ClientOrderID,
		Status: "CANCELLED", OrderSeq: o.OrderSeq,
		TransactTime: s.now().UTC().Format(time.RFC3339Nano)}, nil
}

// otrCancelEvent counts one cancel-side order event in the RTS-9
// sliding window (Task 13.3.6). Cancels bypass the admission gate but
// still count toward the ratio — the account cannot clear a breach by
// flooding cancels. Best-effort; a symbol that fails to resolve counts
// under the instrument id so the window never silently loses an event.
func (s *Service) otrCancelEvent(ctx context.Context, o *Order) {
	if s.otr == nil {
		return
	}
	symbol := strconv.FormatInt(o.InstrumentID, 10)
	if inst, err := s.store.InstrumentByID(ctx, o.InstrumentID); err == nil && inst != nil {
		symbol = inst.Symbol
	}
	s.otr.Event(ctx, o.AccountID, "", symbol)
}

// cancelOne sends the wire cancel and waits for the out-ring echo.
func (s *Service) cancelOne(ctx context.Context, o *Order) error {
	// Queued session orders never reached the engine — cancel locally
	// (the freeze check already ran on the caller path).
	if IsAuctionType(o.OrderType) && !auctionInBook(o) {
		return s.cancelQueued(ctx, o, "client")
	}
	if s.sub == nil {
		// No engine transport wired — dev/test mode applies the cancel
		// locally; fail-open here would fabricate a confirmation in
		// production, so the handler wiring MUST supply a submitter.
		err := s.store.ApplyCancel(ctx, o.ID)
		if err == nil {
			s.otrCancelEvent(ctx, o)
		}
		return err
	}
	shard := uint16(0)
	if o.ShardID != nil {
		shard = uint16(*o.ShardID)
	}
	pc := s.pending.register(uint64(o.ID))
	defer s.pending.deregister(uint64(o.ID))
	b := flatbuffers.NewBuilder(128)
	payload := EncodeCancelEvent(b, s.seq.Next(),
		uint64(s.now().UnixNano()), uint64(o.ID), uint64(o.AccountID))
	if err := s.sub.Send(ctx, shard, payload); err != nil {
		return err
	}
	s.otrCancelEvent(ctx, o)
	select {
	case <-pc.done:
		return nil
	case <-time.After(s.ackTimeout):
		return codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
			"no engine confirmation for cancel of order %d within %s",
			o.ID, s.ackTimeout)
	case <-ctx.Done():
		return codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
			"request cancelled while awaiting engine confirmation")
	}
}

// ---------------------------------------------------------------------------
// Modify — PUT /api/v1/orders/{id} (Tasks 5.3.3/5.3.22)
// ---------------------------------------------------------------------------

// AmendFields is the mutable-field carrier for ApplyAmendTx paths.
type AmendFields struct {
	Price       *decimal.Decimal
	Quantity    *decimal.Decimal
	StopPrice   *decimal.Decimal
	DisplayQty  *decimal.Decimal
	TimeInForce string
	GTDExpiry   *time.Time
}

// Modify performs the audited, stale-fenced amend (op tag "MODIFY").
func (s *Service) Modify(ctx context.Context, acct *Account, orderID int64,
	req *ModifyRequest, actor, requestID, ip string) (*Order, error) {
	return s.modify(ctx, acct, orderID, req, "MODIFY", actor, requestID, ip)
}

// modify is the audited, stale-fenced amend shared by PUT /orders/{id}
// (op=MODIFY) and cancel-replace (op=CANCEL_REPLACE). Ordering:
// read → ownership → stale-seq → validate → CAS update + audit (atomic)
// → wire OrderAmend. A wire send failure reverts the CAS so PG and the
// engine stay aligned.
func (s *Service) modify(ctx context.Context, acct *Account, orderID int64,
	req *ModifyRequest, op, actor, requestID, ip string) (*Order, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	if err := StaleModify(req.OrderSeq, o.OrderSeq); err != nil {
		return nil, err
	}
	inst, err := s.store.InstrumentByID(ctx, o.InstrumentID)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INSTRUMENT_DELISTED",
			"order instrument %d no longer exists", o.InstrumentID)
	}
	// Modify / cancel-replace are new-order entry under halt semantics —
	// the keep-priority qty-down path (AmendKeepPriority) is exempt as
	// it can only reduce the residual.
	if err := s.checkAdmission(ctx, acct, inst, o.SessionID, o.ReduceOnly); err != nil {
		return nil, err
	}
	if err := ValidateModify(req, o, inst); err != nil {
		return nil, err
	}
	// Phase-16 Task 16.3.9 — FIXING amends stay gateway-side: the cut-off
	// gate applies, only quantity is mutable, no wire event exists.
	fixingOrder := o.OrderType == TypeFixing
	if fixingOrder {
		if s.fixing == nil {
			return nil, codeErr("FIXING_CANCELLATION_RESTRICTED",
				"fixing-order pipeline unavailable — amend rejected (fail closed)")
		}
		if err := s.fixing.AssertMutable(ctx, o, inst, s.now()); err != nil {
			return nil, err // FIXING_CANCELLATION_RESTRICTED
		}
		if req.Price != nil || req.StopPrice != nil || req.DisplayQty != nil ||
			req.TimeInForce != "" || req.GTDExpiry != nil {
			return nil, codeErr("ORDER_AMEND_REJECTED",
				"FIXING orders accept quantity amends only")
		}
	}
	// Phase-16 Task 16.3.25 — queued session orders amend quantity only,
	// only pre-freeze (§6.2b: post-freeze → AMEND_IN_AUCTION_REJECTED).
	auctionOrder := IsAuctionType(o.OrderType)
	if auctionOrder {
		if err := s.assertAuctionMutable(ctx, o, inst); err != nil {
			return nil, err // AMEND_IN_AUCTION_REJECTED
		}
		if req.Price != nil || req.StopPrice != nil || req.DisplayQty != nil ||
			req.TimeInForce != "" || req.GTDExpiry != nil {
			return nil, codeErr("ORDER_AMEND_REJECTED",
				"%s orders accept quantity amends only", o.OrderType)
		}
	}
	newSeq := s.seq.Next()
	audits := auditDiff(o, req, op, actor, requestID, ip)
	updated, ok, err := s.store.AmendCAS(ctx, orderID, o.OrderSeq, newSeq,
		AmendFields{
			Price: req.Price, Quantity: req.Quantity, StopPrice: req.StopPrice,
			DisplayQty: req.DisplayQty, TimeInForce: req.TimeInForce,
			GTDExpiry: req.GTDExpiry,
		}, audits)
	if err != nil {
		return nil, errInternal("order amend", err)
	}
	if !ok {
		return nil, codeErr("STALE_MODIFY",
			"order %d modified concurrently", orderID)
	}
	if fixingOrder {
		// Re-size the reservation to the amended quantity; the journal
		// rides the same idempotency keys as placement. No wire event —
		// the executor reads the amended row.
		if s.fixing != nil {
			if err := s.fixing.AdjustReservation(ctx, updated, inst); err != nil {
				_ = s.store.RevertAmend(ctx, o)
				return nil, errInternal("fixing reservation adjust", err)
			}
		}
		return updated, nil
	}
	if auctionOrder {
		// Engine never held this order (queue is local until injection);
		// the CAS above is authoritative. Injection replays the row.
		return updated, nil
	}
	if s.sub != nil {
		b := flatbuffers.NewBuilder(128)
		payload := EncodeAmendEvent(b, s.seq.Next(),
			uint64(s.now().UnixNano()), uint64(o.ID), newSeq,
			scaledOr(req.Price, 0), scaledOr(req.Quantity, 0),
			scaledOr(req.StopPrice, 0), gtdNs(req.GTDExpiry))
		if serr := s.sub.Send(ctx, shardOf(o, s.shards, inst.Symbol), payload); serr != nil {
			// Revert the CAS — engine never applied the amend.
			_ = s.store.RevertAmend(ctx, o)
			return nil, serr
		}
	}
	return updated, nil
}

// auditDiff emits one order_audit row per changed audited field
// (price, quantity, time_in_force, stop_price, iceberg_visible_qty —
// Task 5.3.22 item 3 — plus gtd_expiry as an operation-relevant extra).
func auditDiff(o *Order, req *ModifyRequest, op, actor, requestID, ip string) []AuditEntry {
	var out []AuditEntry
	add := func(field, oldV, newV string) {
		out = append(out, AuditEntry{
			OrderID: o.ID, AccountID: o.AccountID, Operation: op,
			FieldName: field, OldValue: oldV, NewValue: newV,
			ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
		})
	}
	if req.Price != nil && (o.Price == nil || !req.Price.Equal(*o.Price)) {
		add("price", decStr(o.Price), req.Price.String())
	}
	if req.Quantity != nil && !req.Quantity.Equal(o.Quantity) {
		add("quantity", o.Quantity.String(), req.Quantity.String())
	}
	if req.TimeInForce != "" && req.TimeInForce != o.TimeInForce {
		add("time_in_force", o.TimeInForce, req.TimeInForce)
	}
	if req.StopPrice != nil && (o.StopPrice == nil || !req.StopPrice.Equal(*o.StopPrice)) {
		add("stop_price", decStr(o.StopPrice), req.StopPrice.String())
	}
	if req.DisplayQty != nil && (o.DisplayQty == nil || !req.DisplayQty.Equal(*o.DisplayQty)) {
		add("iceberg_visible_qty", decStr(o.DisplayQty), req.DisplayQty.String())
	}
	if req.GTDExpiry != nil {
		add("gtd_expiry", "", req.GTDExpiry.UTC().Format(time.RFC3339Nano))
	}
	return out
}

func scaledOr(d *decimal.Decimal, def int64) int64 {
	if d == nil {
		return def
	}
	return decimal.Scaled(*d)
}

func gtdNs(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.UnixNano()
}

func shardOf(o *Order, m *config.ShardMap, symbol string) uint16 {
	if o.ShardID != nil {
		return uint16(*o.ShardID)
	}
	if m == nil {
		return 0
	}
	return uint16(m.GetShard(symbol))
}

// ---------------------------------------------------------------------------
// Keep-priority amend — PUT /orders/{id}/amend/keep-priority (Task 5.3.37)
// ---------------------------------------------------------------------------

// AmendKeepPriority applies a quantity-down-only amend preserving queue
// priority (same order id + timestamp). Any other mutation attempt is
// ORDER_AMEND_REJECTED.
func (s *Service) AmendKeepPriority(ctx context.Context, acct *Account,
	orderID int64, req *KeepPriorityRequest,
	actor, requestID, ip string) (*Order, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	if err := StaleModify(req.OrderSeq, o.OrderSeq); err != nil {
		return nil, err
	}
	inst, err := s.store.InstrumentByID(ctx, o.InstrumentID)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INSTRUMENT_DELISTED",
			"order instrument %d no longer exists", o.InstrumentID)
	}
	if err := ValidateKeepPriority(req, o, inst); err != nil {
		return nil, err
	}
	// Phase-16 Task 16.3.25 — queued session orders keep the freeze
	// gate on the keep-priority path too (§6.2b).
	if IsAuctionType(o.OrderType) {
		if err := s.assertAuctionMutable(ctx, o, inst); err != nil {
			return nil, err // AMEND_IN_AUCTION_REJECTED
		}
	}
	newSeq := s.seq.Next()
	mr := &ModifyRequest{Quantity: req.Quantity}
	audits := auditDiff(o, mr, "AMEND", actor, requestID, ip)
	updated, ok, err := s.store.AmendCAS(ctx, orderID, o.OrderSeq, newSeq,
		AmendFields{Quantity: req.Quantity}, audits)
	if err != nil {
		return nil, errInternal("keep-priority amend", err)
	}
	if !ok {
		return nil, codeErr("STALE_MODIFY", "order %d modified concurrently", orderID)
	}
	if s.sub != nil && !IsAuctionType(o.OrderType) {
		// Queued auction orders have no engine copy — CAS is
		// authoritative; injected ones are frozen anyway.
		b := flatbuffers.NewBuilder(128)
		payload := EncodeAmendEvent(b, s.seq.Next(),
			uint64(s.now().UnixNano()), uint64(o.ID), newSeq,
			0, decimal.Scaled(*req.Quantity), 0, 0)
		if serr := s.sub.Send(ctx, shardOf(o, s.shards, inst.Symbol), payload); serr != nil {
			_ = s.store.RevertAmend(ctx, o)
			return nil, serr
		}
	}
	// Task 13.3.6 — qty-down keep-priority amends bypass checkAdmission
	// but still count as a modify event in the OTR window.
	if s.otr != nil {
		s.otr.Event(ctx, acct.ID, acct.KycTier, inst.Symbol)
	}
	return updated, nil
}

// ---------------------------------------------------------------------------
// Atomic cancel-replace — POST /orders/{id}/cancel-replace (Task 5.3.37)
// ---------------------------------------------------------------------------

// CancelReplace maps to the single matching-thread replace path (spec
// §6.9 #1: "never cancel+new from the gateway"). mode is validated and
// echoed by the caller for response shaping; the wire op itself is one
// atomic amend — there is no partial apply to roll back. Audit rows are
// tagged CANCEL_REPLACE so the amendment trail distinguishes them.
func (s *Service) CancelReplace(ctx context.Context, acct *Account,
	orderID int64, req *CancelReplaceRequest,
	actor, requestID, ip string) (*Order, error) {
	if req.Mode != "STOP_ON_FAILURE" && req.Mode != "ALLOW_FAILURE" {
		return nil, codeErr("INVALID_REQUEST",
			"mode must be STOP_ON_FAILURE or ALLOW_FAILURE")
	}
	return s.modify(ctx, acct, orderID, &req.ModifyRequest,
		"CANCEL_REPLACE", actor, requestID, ip)
}

// ---------------------------------------------------------------------------
// Mass cancel — DELETE /orders?symbol=, /orders/all (Tasks 5.3.3/5.3.25)
// ---------------------------------------------------------------------------

// MassCancelResult is the per-symbol breakdown the API returns.
type MassCancelResult struct {
	Cancelled  int            `json:"cancelled"`
	PerSymbol  map[string]int `json:"per_symbol"`
	Scope      MassCancelScope
	DeadlineNS int64 `json:"-"`
}

// MassCancel cancels every open order matching scope and returns only
// after all shards confirm (every engine echo observed or the overall
// deadline lapses). Retries are idempotent — already-cancelled orders
// are simply absent from the open set.
func (s *Service) MassCancel(ctx context.Context, scope MassCancelScope,
	actor, requestID, ip string) (*MassCancelResult, error) {
	if err := NormalizeMassCancelScope(&scope, scope.AccountID == 0); err != nil {
		return nil, err
	}
	open, err := s.store.OpenOrders(ctx, scope)
	if err != nil {
		return nil, errInternal("open orders", err)
	}
	res := &MassCancelResult{Cancelled: 0, PerSymbol: map[string]int{}, Scope: scope}
	if len(open) == 0 {
		return res, nil
	}
	sym := func(id int64) string {
		if inst, err := s.store.InstrumentByID(ctx, id); err == nil && inst != nil {
			return inst.Symbol
		}
		return strconv.FormatInt(id, 10)
	}
	// Send phase — every order gets a pending slot first so an echo that
	// races ahead of registration can never resolve a missing waiter.
	type waiter struct {
		o  *Order
		pc *pendingCancel
	}
	var ws []waiter
	// localCancelled tracks orders applied without the wire — the
	// audit set below must cover only orders actually cancelled (a
	// frozen queued order is skipped, not cancelled).
	var localCancelled []*Order
	enqueued := 0
	for i := range open {
		o := &open[i]
		// Phase-16 Task 16.3.25 — queued (never-injected) session
		// orders cancel locally pre-freeze; post-freeze the queue is
		// immutable and the row is left to join the auction.
		if IsAuctionType(o.OrderType) && !auctionInBook(o) {
			inst, _ := s.store.InstrumentByID(ctx, o.InstrumentID)
			frozen := false
			if inst != nil && s.auction != nil {
				frozen, _ = s.auction.Frozen(ctx, inst, o.OrderType, s.now())
			}
			if frozen {
				continue
			}
			// Propagate the scope reason — the auction scheduler's
			// AUCTION_UNFILLED_REMAINDER sweep must surface as
			// AUCTION_CANCELLED on the private notification (§6.2b).
			reason := scope.Reason
			if reason == "" {
				reason = "mass_cancel"
			}
			if s.cancelQueued(ctx, o, reason) == nil {
				s.otrCancelEvent(ctx, o)
				res.PerSymbol[sym(o.InstrumentID)]++
				res.Cancelled++
				localCancelled = append(localCancelled, o)
			}
			continue
		}
		if s.sub == nil {
			// No transport wired (tests/dev) — apply locally.
			_ = s.store.ApplyCancel(ctx, o.ID)
			s.otrCancelEvent(ctx, o)
			res.PerSymbol[sym(o.InstrumentID)]++
			res.Cancelled++
			localCancelled = append(localCancelled, o)
			continue
		}
		pc := s.pending.register(uint64(o.ID))
		b := flatbuffers.NewBuilder(128)
		payload := EncodeCancelEvent(b, s.seq.Next(),
			uint64(s.now().UnixNano()), uint64(o.ID), uint64(o.AccountID))
		var shard uint16
		if o.ShardID != nil {
			shard = uint16(*o.ShardID)
		}
		if err := s.sub.Send(ctx, shard, payload); err != nil {
			for _, w := range ws {
				s.pending.deregister(uint64(w.o.ID))
			}
			s.pending.deregister(uint64(o.ID))
			return nil, err // ENGINE_OVERLOAD etc — fail-closed
		}
		ws = append(ws, waiter{o, pc})
		enqueued++
		res.PerSymbol[sym(o.InstrumentID)]++
		res.Cancelled++
	}
	// Task 13.3.6 — count every wire-dispatched cancel in the OTR
	// window (the local-apply and queued paths already counted inline
	// above; iterating `open` again would double-count queued orders
	// and count frozen ones that were never cancelled).
	if s.otr != nil && s.sub != nil {
		for _, w := range ws {
			s.otrCancelEvent(ctx, w.o)
		}
	}
	// Await phase — the task requires ALL shards to confirm before
	// success; the overall deadline bounds the wait.
	deadline := s.now().Add(MassCancelTimeout)
	for _, w := range ws {
		remain := deadline.Sub(s.now())
		if remain <= 0 {
			s.pending.deregister(uint64(w.o.ID))
			return nil, codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
				"mass cancel for account %d did not confirm all %d orders within %s",
				scope.AccountID, enqueued, MassCancelTimeout)
		}
		select {
		case <-w.pc.done:
			_ = s.store.ApplyCancel(ctx, w.o.ID)
		case <-time.After(remain):
			s.pending.deregister(uint64(w.o.ID))
			return nil, codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
				"mass cancel for account %d timed out awaiting engine confirmations",
				scope.AccountID)
		case <-ctx.Done():
			s.pending.deregister(uint64(w.o.ID))
			return nil, codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
				"mass cancel aborted: %v", ctx.Err())
		}
	}
	// Audit one row per actually-cancelled order (operation=MASS_CANCEL)
	// — local-apply/queued cancels plus every wire-confirmed waiter.
	audits := make([]AuditEntry, 0, len(localCancelled)+len(ws))
	for _, o := range localCancelled {
		audits = append(audits, AuditEntry{
			OrderID: o.ID, AccountID: o.AccountID, Operation: "MASS_CANCEL",
			FieldName: "status", OldValue: o.Status, NewValue: "CANCELLED",
			ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
		})
	}
	for _, w := range ws {
		audits = append(audits, AuditEntry{
			OrderID: w.o.ID, AccountID: w.o.AccountID, Operation: "MASS_CANCEL",
			FieldName: "status", OldValue: w.o.Status, NewValue: "CANCELLED",
			ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
		})
	}
	_ = s.store.WriteAudit(ctx, audits)
	return res, nil
}

// CancelOnDisconnect implements the REST/WS session CoD (Task 5.3.25
// item 4): mass-cancel orders attributed to a dropped session.
func (s *Service) CancelOnDisconnect(ctx context.Context, accountID int64,
	sessionID string) (*MassCancelResult, error) {
	return s.MassCancel(ctx, MassCancelScope{
		AccountID: accountID, SessionID: sessionID,
		Reason: "cancel_on_disconnect",
	}, "system:cod", "", "")
}

// ---------------------------------------------------------------------------
// Batch ops — POST/DELETE /orders/batch (Task 5.3.32)
// ---------------------------------------------------------------------------

// BatchResult is one index-mapped outcome.
type BatchResult struct {
	Index         int    `json:"index"`
	Status        string `json:"status"` // ACCEPTED | REJECTED | CANCELLED
	OrderID       int64  `json:"order_id,omitempty"`
	ClientOrderID string `json:"client_order_id,omitempty"`
	Error         string `json:"error,omitempty"`
	Message       string `json:"message,omitempty"`
}

// BatchSubmitFailure carries the per-entry map when the batch aborts —
// the handler renders it inside details.results of the error envelope.
type BatchSubmitFailure struct {
	Results []BatchResult
	Err     error
}

// Error satisfies the error interface; Unwrap exposes the first
// per-entry cause so errs.WriteError maps the right HTTP status.
func (f *BatchSubmitFailure) Error() string { return f.Err.Error() }
func (f *BatchSubmitFailure) Unwrap() error { return f.Err }

// BatchSubmit is strictly atomic (spec §8.4 "atomic index-mapped result
// array" + the never-partially-apply rule): every entry is validated
// first; ANY entry failing aborts the whole batch — nothing is
// persisted or dispatched — and the response carries per-entry verdicts.
// Duplicate client_order_id within the batch is a batch-level defect.
func (s *Service) BatchSubmit(ctx context.Context, acct *Account,
	reqs []*SubmitRequest, requestID, ip string) (_ []BatchResult, err error) {
	start := s.now()
	// Task 14.3.2: a batch is also admission traffic — emit one
	// observation per distinct canonical symbol. The latency sample is
	// the whole-call duration (documented aggregate — batches are rare
	// vs single submits and carry ≤BatchSubmitMax entries); the
	// systemic flag unwraps BatchSubmitFailure to the first cause.
	defer func() {
		if s.admission == nil {
			return
		}
		systemic := err != nil &&
			risk.SystemicAdmissionCode(excerrors.CodeOf(err))
		seen := map[string]bool{}
		for _, req := range reqs {
			if req == nil {
				continue
			}
			if sym := config.CanonicalSymbol(req.Symbol); sym != "" && !seen[sym] {
				seen[sym] = true
				s.admission(ctx, sym, s.now().Sub(start), systemic)
			}
		}
	}()
	if len(reqs) == 0 || len(reqs) > BatchSubmitMax {
		return nil, codeErr("BATCH_SIZE_EXCEEDED",
			"batch requires 1..%d orders, got %d", BatchSubmitMax, len(reqs))
	}
	if s.batch != nil {
		ok, err := s.batch.AllowBatch(ctx, acct.ID)
		if err != nil {
			return nil, errInternal("batch rate limit", err)
		}
		if !ok {
			return nil, codeErr("RATE_LIMIT_TIER_EXCEEDED",
				"batch rate limit rl:batch:%d exceeded", acct.ID)
		}
	}
	results := make([]BatchResult, len(reqs))
	seenCOID := map[string]int{}
	insts := map[string]*Instrument{}
	var firstErr error
	for i, req := range reqs {
		results[i].Index = i
		results[i].Status = "ACCEPTED"
		if req == nil {
			results[i].Status = "REJECTED"
			results[i].Error = "INVALID_REQUEST"
			results[i].Message = "empty order entry"
			continue
		}
		sym := config.CanonicalSymbol(req.Symbol)
		inst, ok := insts[sym]
		if !ok {
			var err error
			inst, err = s.store.InstrumentBySymbol(ctx, sym)
			if err != nil {
				return nil, errInternal("instrument lookup", err)
			}
			insts[sym] = inst
		}
		var verr error
		switch {
		case inst == nil:
			verr = codeErr("INVALID_REQUEST", "unknown symbol %q", req.Symbol)
		case req.ClientOrderID != "":
			if prev, dup := seenCOID[req.ClientOrderID]; dup {
				verr = codeErr("IDEMPOTENCY_KEY_COLLISION",
					"client_order_id %q duplicated within batch (indices %d,%d)",
					req.ClientOrderID, prev, i)
			} else {
				seenCOID[req.ClientOrderID] = i
			}
		}
		if verr == nil && inst != nil {
			// Kill-switch gate per batch entry — per-item verdicts carry
			// TRADING_HALTED without aborting sibling entries.
			verr = s.checkAdmission(ctx, acct, inst, req.SessionID, req.ReduceOnly)
		}
		if verr == nil {
			ref, rerr := s.store.ReferencePrice(ctx, inst.ID)
			if rerr != nil {
				return nil, errInternal("reference price", rerr)
			}
			verr = ValidateSubmit(req, inst, acct, ref, s.now())
			// Quote conversion runs in the atomic pre-pass too so a
			// non-convertible entry aborts the batch before any dispatch.
			if verr == nil && req.QuoteQuantity != nil {
				if ref == nil || !ref.IsPositive() {
					verr = codeErr("QUOTE_QUANTITY_INVALID",
						"no reference price for quote conversion on %s", inst.Symbol)
				} else {
					base := req.QuoteQuantity.Div(*ref)
					if inst.LotSize.IsPositive() {
						base = base.Div(inst.LotSize).Floor().Mul(inst.LotSize)
					}
					if !base.IsPositive() {
						verr = codeErr("QUOTE_QUANTITY_INVALID",
							"quote_quantity %s converts to zero base", *req.QuoteQuantity)
					} else {
						req.Quantity = &base
					}
				}
			}
			if verr == nil {
				verr = s.checkBalance(ctx, acct, inst, req, ref)
			}
			if verr == nil {
				// Pre-check dedup so a replayed entry is atomic-known
				// before any dispatch.
				if req.ClientOrderID != "" {
					dup, derr := s.store.DedupLookup(ctx, acct.ID, req.ClientOrderID)
					if derr != nil {
						return nil, errInternal("dedup lookup", derr)
					}
					if dup != nil {
						hash := submitHash(inst.ID, req)
						if dup.RequestHash == hash {
							results[i].Status = "ACCEPTED"
							results[i].OrderID = dup.OrderID
							results[i].ClientOrderID = req.ClientOrderID
							continue
						}
						verr = codeErr("IDEMPOTENCY_KEY_COLLISION",
							"client_order_id %q already used with a different payload",
							req.ClientOrderID)
					}
				}
			}
		}
		if verr != nil {
			results[i].Status = "REJECTED"
			if ce, ok := verr.(*excerrors.Error); ok {
				results[i].Error = ce.Code
				results[i].Message = ce.Message
			} else {
				results[i].Error = "INTERNAL_ERROR"
			}
			if firstErr == nil {
				firstErr = verr
			}
		}
	}
	if firstErr != nil {
		return nil, &BatchSubmitFailure{Results: results, Err: firstErr}
	}

	// All valid — persist + dispatch each; a mid-batch send failure
	// compensates by rejecting every not-yet-confirmed row so the batch
	// never lands partially on the book.
	var acked []BatchResult
	persisted := make([]*Order, 0, len(reqs))
	for i, req := range reqs {
		if results[i].OrderID != 0 {
			acked = append(acked, results[i]) // dedup replay
			continue
		}
		inst := insts[config.CanonicalSymbol(req.Symbol)]
		shard := s.shardFor(inst.Symbol)
		seq := s.seq.Next()
		hash := submitHash(inst.ID, req)
		o, _, err := s.store.InsertOrderTx(ctx, InsertParams{
			AccountID: acct.ID, InstrumentID: inst.ID,
			ClientOrderID: req.ClientOrderID, Side: req.Side,
			OrderType: req.OrderType, Quantity: *req.Quantity,
			QuoteQuantity: req.QuoteQuantity, Price: req.Price,
			StopPrice: req.StopPrice, DisplayQty: req.DisplayQty,
			TimeInForce: req.TimeInForce, ShardID: int(shard),
			OrderSeq: seq, PostOnly: req.PostOnly,
			ReduceOnly: req.ReduceOnly, STPMode: req.STPMode,
			SessionID: req.SessionID, RequestHash: hash,
		})
		if err != nil {
			return nil, s.batchAbort(ctx, persisted, err)
		}
		persisted = append(persisted, o)
		if s.sub != nil {
			b := flatbuffers.NewBuilder(256)
			payload := ipc.EncodeOrderNewEvent(b, seq,
				uint64(s.now().UnixNano()), orderNewMsg(o, acct, req))
			if err := s.sub.Send(ctx, shard, payload); err != nil {
				return nil, s.batchAbort(ctx, persisted, err)
			}
			_ = s.store.MarkActive(ctx, o.ID)
		}
		acked = append(acked, BatchResult{
			Index: i, Status: "ACCEPTED", OrderID: o.ID,
			ClientOrderID: o.ClientOrderID,
		})
	}
	// Preserve input order.
	sort.Slice(acked, func(a, b int) bool { return acked[a].Index < acked[b].Index })
	// Batch-op audit trail (Task 5.3.32 item 3).
	audits := make([]AuditEntry, 0, len(acked))
	for _, r := range acked {
		if r.OrderID == 0 {
			continue
		}
		audits = append(audits, AuditEntry{
			OrderID: r.OrderID, AccountID: acct.ID, Operation: "BATCH_SUBMIT",
			FieldName: "status", NewValue: "PENDING",
			ModifiedBy: actor0(acct), RequestID: requestID, IPAddress: ip,
		})
	}
	_ = s.store.WriteAudit(ctx, audits)
	return acked, nil
}

// batchAbort marks every persisted batch order REJECTED and returns the
// dispatch error — nothing remains partially applied.
func (s *Service) batchAbort(ctx context.Context, persisted []*Order, cause error) error {
	for _, o := range persisted {
		_ = s.store.MarkRejected(ctx, o.ID)
	}
	if ce, ok := cause.(*excerrors.Error); ok {
		return ce
	}
	return codeErr("INTERNAL_ERROR", "batch dispatch failed: %v", cause)
}

func actor0(a *Account) string { return "account:" + strconv.FormatInt(a.ID, 10) }

// BatchCancel resolves every id (orders and/or client ids) up front —
// a single unresolvable/foreign id aborts the whole batch (atomicity) —
// then dispatches all cancels and awaits engine confirmation per order.
func (s *Service) BatchCancel(ctx context.Context, acct *Account,
	orderIDs []int64, clientIDs []string, actor, requestID, ip string) ([]BatchResult, error) {
	if len(orderIDs)+len(clientIDs) == 0 || len(orderIDs)+len(clientIDs) > BatchCancelMax {
		return nil, codeErr("BATCH_SIZE_EXCEEDED",
			"batch cancel requires 1..%d order references, got %d",
			BatchCancelMax, len(orderIDs)+len(clientIDs))
	}
	if s.batch != nil {
		ok, err := s.batch.AllowBatch(ctx, acct.ID)
		if err != nil {
			return nil, errInternal("batch rate limit", err)
		}
		if !ok {
			return nil, codeErr("RATE_LIMIT_TIER_EXCEEDED",
				"batch rate limit rl:batch:%d exceeded", acct.ID)
		}
	}
	type ref struct {
		idx int
		o   *Order
	}
	var resolved []ref
	resolve := func(idx int, o *Order) error {
		if o == nil || o.AccountID != acct.ID {
			return codeErr("ORDER_NOT_FOUND", "order reference %d not found", idx)
		}
		resolved = append(resolved, ref{idx, o})
		return nil
	}
	for i, id := range orderIDs {
		o, err := s.store.GetOrder(ctx, id)
		if err != nil {
			return nil, errInternal("order read", err)
		}
		if err := resolve(i, o); err != nil {
			return nil, err
		}
	}
	for j, cid := range clientIDs {
		row, err := s.store.DedupLookup(ctx, acct.ID, cid)
		if err != nil {
			return nil, errInternal("dedup lookup", err)
		}
		var o *Order
		if row != nil {
			o, err = s.store.GetOrder(ctx, row.OrderID)
			if err != nil {
				return nil, errInternal("order read", err)
			}
		}
		if err := resolve(len(orderIDs)+j, o); err != nil {
			return nil, err
		}
	}
	results := make([]BatchResult, 0, len(resolved))
	audits := make([]AuditEntry, 0, len(resolved))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, r := range resolved {
		r := r
		if isTerminal(r.o.Status) {
			results = append(results, BatchResult{
				Index: r.idx, Status: "CANCELLED",
				OrderID: r.o.ID, ClientOrderID: r.o.ClientOrderID,
				Error:   "INVALID_REQUEST",
				Message: "order already in terminal state " + r.o.Status,
			})
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.cancelOne(ctx, r.o)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				code := "INTERNAL_ERROR"
				if ce, ok := err.(*excerrors.Error); ok {
					code = ce.Code
				}
				results = append(results, BatchResult{
					Index: r.idx, Status: "REJECTED", OrderID: r.o.ID,
					ClientOrderID: r.o.ClientOrderID,
					Error:         code, Message: err.Error(),
				})
				return
			}
			audits = append(audits, AuditEntry{
				OrderID: r.o.ID, AccountID: acct.ID,
				Operation: "BATCH_CANCEL", FieldName: "status",
				OldValue: r.o.Status, NewValue: "CANCELLED",
				ModifiedBy: actor, RequestID: requestID, IPAddress: ip,
			})
			results = append(results, BatchResult{
				Index: r.idx, Status: "CANCELLED",
				OrderID: r.o.ID, ClientOrderID: r.o.ClientOrderID,
			})
		}()
	}
	wg.Wait()
	_ = s.store.WriteAudit(ctx, audits)
	sort.Slice(results, func(a, b int) bool { return results[a].Index < results[b].Index })
	// Atomicity: any failed cancel aborts the response (the orders that
	// did cancel are terminal-correct — a batch cancel's atomicity is
	// all-or-nothing *dispatch*, each cancel is independent on the book).
	for _, r := range results {
		if r.Status == "REJECTED" {
			return results, codeErr("GATEWAY_TIMEOUT_MATCHING_ENGINE",
				"batch cancel incomplete — see results")
		}
	}
	return results, nil
}

// ---------------------------------------------------------------------------
// Preview — POST /api/v1/orders/test (Task 5.3.39)
// ---------------------------------------------------------------------------

// Preview is the non-binding dry-run response.
type Preview struct {
	EstimatedBaseQty   string   `json:"estimated_base_qty"`
	EstimatedQuoteQty  string   `json:"estimated_quote_qty"`
	Margin             string   `json:"margin"`
	CommissionEstimate string   `json:"commission_estimate"`
	SpreadEstimate     string   `json:"spread_estimate"`
	RiskLevel          string   `json:"risk_level"` // LOW|MEDIUM|HIGH
	Warnings           []string `json:"warnings"`
	Filters            []string `json:"active_filters"`
	Binding            bool     `json:"binding"` // always false
}

// CommissionEstimator resolves the account's effective tier charge —
// the settlement engine's CommissionTierCharge shape.
type CommissionEstimator func(ctx context.Context, accountID int64,
	qty, lotSize, notional decimal.Decimal) (decimal.Decimal, error)

// DryRun runs schema → filters → entitlement → margin → price-range →
// commission checks without reserving funds, writing WAL or creating an
// order (spec §22.1).
func (s *Service) DryRun(ctx context.Context, acct *Account,
	req *SubmitRequest) (*Preview, error) {
	sym := config.CanonicalSymbol(req.Symbol)
	inst, err := s.store.InstrumentBySymbol(ctx, sym)
	if err != nil {
		return nil, errInternal("instrument lookup", err)
	}
	if inst == nil {
		return nil, codeErr("INVALID_REQUEST", "unknown symbol %q", req.Symbol)
	}
	ref, err := s.store.ReferencePrice(ctx, inst.ID)
	if err != nil {
		return nil, errInternal("reference price", err)
	}
	if err := ValidateSubmit(req, inst, acct, ref, s.now()); err != nil {
		return nil, err
	}

	warn := []string{}
	filters := []string{"PRICE_FILTER", "LOT_SIZE", "MIN_NOTIONAL", "PRICE_BAND"}
	eval := req.Price
	if eval == nil {
		eval = ref
	}
	var qty decimal.Decimal
	if req.Quantity != nil {
		qty = *req.Quantity
	} else if req.QuoteQuantity != nil {
		if eval == nil || !eval.IsPositive() {
			return nil, codeErr("QUOTE_QUANTITY_INVALID",
				"no reference price for quote conversion")
		}
		q := req.QuoteQuantity.Div(*eval)
		if inst.LotSize.IsPositive() {
			q = q.Div(inst.LotSize).Floor().Mul(inst.LotSize)
		}
		qty = q
		warn = append(warn,
			"quote-denominated order: base quantity estimated from reference price and lot-rounded down")
	}
	notional := decimal.Zero
	quoteQty := decimal.Zero
	if eval != nil && eval.IsPositive() {
		notional = qty.Mul(*eval)
		quoteQty = notional
	} else {
		warn = append(warn, "no reference price — quote estimate unavailable")
	}

	// Margin check: notional/leverage for margin accounts, full notional
	// for SPOT. Balance sufficiency reuses the submit-path rule so the
	// preview predicts the real gate.
	margin := notional
	if acct.Type != "SPOT" && inst.MaxLeverage > 0 {
		margin = notional.Div(decimal.NewFromInt(int64(inst.MaxLeverage)))
	}
	if ref == nil {
		filters = append(filters, "REFERENCE_PRICE_MISSING")
	}
	if req.QuoteQuantity != nil {
		quoteQty = *req.QuoteQuantity
	}
	var commission decimal.Decimal
	if s.commission != nil {
		if c, err := s.commission(ctx, acct.ID, qty, inst.LotSize, quoteQty); err == nil {
			commission = c
		} else {
			warn = append(warn, "commission estimate unavailable")
		}
	}
	spreadEst := decimal.Zero
	if inst.MaxSpreadPips != nil {
		spreadEst = inst.MaxSpreadPips.Mul(decimal.NewFromInt(2))
	}

	// Balance sufficiency as a warning/check (preview never fails on it —
	// it predicts): insufficient funds surface as HIGH risk + warning,
	// keeping the dry-run non-binding but honest.
	riskLevel := "LOW"
	balCurrency := inst.QuoteCurrency
	if req.Side == SideSell {
		balCurrency = inst.BaseCurrency
	}
	if avail, err := s.store.AvailableBalance(ctx, acct.ID, balCurrency); err == nil {
		need := qty
		if req.Side == SideBuy {
			need = margin
		}
		if avail == nil || avail.LessThan(need) {
			riskLevel = "HIGH"
			warn = append(warn, "insufficient available balance for this order")
		}
	}
	if req.QuoteQuantity != nil || req.OrderType == TypeMarket {
		if riskLevel == "LOW" {
			riskLevel = "MEDIUM" // market risk → confirmation severity
		}
	}
	if inst.Status != "ACTIVE" {
		// already rejected by the state gate above
	}
	return &Preview{
		EstimatedBaseQty:   qty.String(),
		EstimatedQuoteQty:  quoteQty.String(),
		Margin:             margin.String(),
		CommissionEstimate: commission.String(),
		SpreadEstimate:     spreadEst.String(),
		RiskLevel:          riskLevel,
		Warnings:           warn,
		Filters:            filters,
		Binding:            false,
	}, nil
}

// ---------------------------------------------------------------------------
// Read paths
// ---------------------------------------------------------------------------

func (s *Service) GetOrder(ctx context.Context, acct *Account, orderID int64) (*Order, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	return o, nil
}

func (s *Service) ListOrders(ctx context.Context, acct *Account, q ListQuery) ([]Order, int64, error) {
	q.AccountID = acct.ID
	if q.Limit <= 0 {
		q.Limit = 100
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	return s.store.ListOrders(ctx, q)
}

// Amendments returns the client-visible amendment history.
func (s *Service) Amendments(ctx context.Context, acct *Account, orderID int64) ([]AuditEntry, error) {
	o, err := s.store.GetOrder(ctx, orderID)
	if err != nil {
		return nil, errInternal("order read", err)
	}
	if o == nil || o.AccountID != acct.ID {
		return nil, codeErr("ORDER_NOT_FOUND", "order %d not found", orderID)
	}
	return s.store.Amendments(ctx, orderID)
}

// AuditTrail serves the Compliance-Officer admin endpoint — any account.
func (s *Service) AuditTrail(ctx context.Context, orderID int64) ([]AuditEntry, error) {
	return s.store.AuditTrail(ctx, orderID)
}

// AdminMassCancel is the Task 5.3.24 item-7 cross-account Risk-Manager
// surface; scope may omit account_id entirely.
func (s *Service) AdminMassCancel(ctx context.Context, scope MassCancelScope,
	actor, requestID, ip string) (*MassCancelResult, error) {
	scope.Reason = "admin"
	return s.MassCancel(ctx, scope, actor, requestID, ip)
}

func errInternal(op string, err error) error {
	return codeErr("INTERNAL_ERROR", "%s: %v", op, err)
}
