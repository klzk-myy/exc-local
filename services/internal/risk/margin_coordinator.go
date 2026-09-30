// margin_coordinator.go — Go Risk Coordinator for cross-shard portfolio
// margin coherence (Phase-19 Task 19.3.11; spec §5.35
// shard_margin_reservations, §13.1 cross-shard coherence + partition-
// degradation safeguard, §24 #176; migration 058).
//
// Topology: every engine shard runs a CrossShardMarginCoordinator
// (core/src/risk/CrossShardMarginCoordinator.cpp) bound to the control
// pair aeron:ipc?alias=margin_ctl_in_{shard} / margin_ctl_out_{shard}
// (stream ids 1101/1102 — see ipc/margin_ctl.go). A shard whose local
// margin floor cannot cover an order emits MARGIN_RESERVE_REQ; this
// service is the global authority answering ACK/NACK, and either side
// can emit MARGIN_RELEASE. The wire codec is ipc/margin_ctl.go —
// byte-identical to the C++ packed structs; nothing here redefines it.
//
// Headroom invariant (spec §13.1, zero-breach):
//
//	Available_Headroom = Total_Equity − Maintenance_Margin − Σ(active reservations)
//
// A REQ is granted iff amount <= Available_Headroom at decision time.
// Account state is haircut-adjusted equity pushed by the margin engine
// (SetAccountMargin — §13.1's <1ms synchronized margin buffers) or
// lazily read from margin_accounts (equity/used_margin; used_margin is
// the conservative maintenance-margin bound — IM ≥ MM ⇒ headroom is
// never overstated).
//
// Ordering — log-then-ACK: a grant is durable in
// shard_margin_reservations BEFORE the ACK is emitted, mirroring the
// C++ rule "a slice we cannot persist is a slice we must not hold". A
// ledger fault revokes the in-memory grant and degrades the REQ to a
// NACK(CoordinatorOverload) — never an unpersisted grant.
//
// Layered budget (spec §13.1, amended 2026-09-19): the 500µs RPC budget
// and >10ms hard deadline are measured engine-side (REQ emit → ACK). The
// coordinator mirrors the ladder by self-compensating any grant whose
// decision latency exceeded cfg.HardDeadline: it emits ACK followed by a
// RELEASE(TimeoutCompensate), so an engine still pending resolves then
// unwinds, and an engine that already fell back simply ignores both.
//
// Commit-on-fill: RELEASE carrying reason Complete converts the slice to
// committed maintenance margin — the coordinator moves the granted
// amount into the account's margin book so headroom never transiently
// inflates ahead of the next margin-buffer push (over-count of MM until
// the push lands is the safe direction). Cancel/reject/IOC-expiry
// releases restore headroom verbatim.
//
// Concurrency: a single mutex serializes all decisions — single-writer
// parity with the C++ matching-loop contract. Cross-shard atomicity is
// total-order; the critical section is O(1) bookkeeping + one
// LedgerTimeout-bounded write. A ledger stall degrades every shard to
// its pessimistic floor — fail-closed by design (§2.7), never a safety
// violation.
package risk

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ipc"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Spec §23 codes registered for the cross-shard margin path. The engine
// emits them on rejections (HTTP mapping lives in internal/errs); the
// coordinator references them for alerts/metrics parity.
const (
	// CodeCrossShardMarginUnavailable — 503: 500µs RPC budget missed;
	// order rejected on the pessimistic floor (engine-side emit).
	CodeCrossShardMarginUnavailable = "CROSS_SHARD_MARGIN_UNAVAILABLE"
	// CodeCrossShardMarginTimeout — 504: >10ms hard deadline; in-flight
	// reservation cancelled + compensated.
	CodeCrossShardMarginTimeout = "CROSS_SHARD_MARGIN_TIMEOUT"
	// CodeCrossShardLimitExceeded — 429: >10 concurrent cross-shard
	// operations per account (Phase-02 Task 2.3.14).
	CodeCrossShardLimitExceeded = "CROSS_SHARD_LIMIT_EXCEEDED"
)

// Internal alert codes (§23 internal-only list — ops alerts, never HTTP).
const (
	// codeMarginLedgerFault — the durable reservation ledger refused a
	// write; the decision was revoked/compensated fail-closed.
	codeMarginLedgerFault = "MARGIN_COORDINATOR_LEDGER_FAULT"
	// codeMarginOrphanSweep — restart recovery tombstoned a reservation
	// that never reached a terminal state.
	codeMarginOrphanSweep = "MARGIN_RESERVATION_RECOVERY_ORPHAN"
)

// CoordinatorReservationPrefix is the high-16 reservation-id prefix for
// coordinator-issued ids (the Reserve API assigns ids only to callers
// that arrive without one — wire REQs always carry the engine's
// issuer-assigned id). 0x7FFF keeps every coordinator id inside BIGINT
// (int64) range; engine issuers use their real shard id (small).
const CoordinatorReservationPrefix uint64 = 0x7FFF

const coordIDLow48Mask uint64 = 0x0000FFFFFFFFFFFF

// ShardReservationStatus mirrors shard_reservation_status_enum
// (migration 058): PENDING → COMMITTED → RELEASED, DENIED tombstone.
type ShardReservationStatus string

const (
	// ShardReservationPending — REQ logged, grant decision in-flight
	// (reserved for the relay path; the direct grant path writes
	// COMMITTED/DENIED atomically).
	ShardReservationPending ShardReservationStatus = "PENDING"
	// ShardReservationCommitted — ACK'd slice counting against headroom.
	ShardReservationCommitted ShardReservationStatus = "COMMITTED"
	// ShardReservationReleased — terminal: released/expired/compensated.
	ShardReservationReleased ShardReservationStatus = "RELEASED"
	// ShardReservationDenied — terminal: NACK tombstone (replay dedupe).
	ShardReservationDenied ShardReservationStatus = "DENIED"
)

// ShardReservationRow is one shard_margin_reservations row.
type ShardReservationRow struct {
	ReservationID  int64
	AccountID      int64
	ConsumerShard  int // shard whose order draws on the slice
	HostShard      int // shard the slice is charged to
	InstrumentID   int64
	OrderID        int64
	ReservedAmount decimal.Decimal
	GrantedAmount  *decimal.Decimal // NULL pre-ACK
	Currency       string           // account base; USD numeraire
	ReqFlags       int              // wire req_flags
	Status         ShardReservationStatus
	ReleaseReason  string // "" → NULL; ReleaseReason/NackReason token
	ExpiresAt      time.Time
	CreatedAt      time.Time
}

// ShardMarginLedger is the durable reservation-ledger seam; the
// production binding is PgShardMarginLedger over
// shard_margin_reservations. Implementations must tolerate being called
// under the coordinator's decision mutex — a ledger must never call back
// into the coordinator.
type ShardMarginLedger interface {
	// InsertReservation writes the decided row (COMMITTED with
	// granted_amount, or a DENIED tombstone). reservation_id is the PK;
	// a duplicate surfaces SQLSTATE 23505.
	InsertReservation(ctx context.Context, row ShardReservationRow) error
	// SetStatus transitions a row (RELEASED also stamps released_at and
	// release_reason). A missing row is an error — never silent.
	SetStatus(ctx context.Context, reservationID int64, status ShardReservationStatus, releaseReason string) error
	// ReservationByID returns the row or (nil, nil) when absent.
	ReservationByID(ctx context.Context, reservationID int64) (*ShardReservationRow, error)
	// LoadActiveReservations returns every PENDING|COMMITTED row —
	// the restart-recovery set (Recover).
	LoadActiveReservations(ctx context.Context) ([]ShardReservationRow, error)
}

// AccountMarginSource lazily supplies an account's margin book when it
// has not been pushed via SetAccountMargin — the production binding is
// PgShardMarginLedger.AccountMargin over margin_accounts.
type AccountMarginSource interface {
	// AccountMargin returns (equity, margin, ok). ok=false means the
	// account is unknown to the margin engine — the coordinator NACKs
	// UnknownAccount (fail closed).
	AccountMargin(ctx context.Context, accountID int64) (equity, margin decimal.Decimal, ok bool, err error)
}

// MarginCtlEmitter is the Aeron publisher seam for coordinator→engine
// frames: the bridge publishes frame on ipc.MarginCtlOutURI(shard),
// stream ipc.MarginCtlOutStreamID.
type MarginCtlEmitter interface {
	EmitMarginCtl(ctx context.Context, shard uint32, frame []byte) error
}

// OutboundFrame is one encoded margin-ctl frame plus the engine shard it
// is addressed to.
type OutboundFrame struct {
	Shard uint32
	Data  []byte
}

// MarginCoordinatorConfig tunes the coordinator. Durations are
// injectable so tests drive the 500µs/10ms ladder and the TTL sweeper
// with a fake clock — no real sleeps.
type MarginCoordinatorConfig struct {
	// ShardCount is the engine-shard divisor for the pessimistic
	// partition floor (account headroom ÷ shard count, §13.1).
	ShardCount int
	// ReservationTTL is the authoritative expiry the coordinator
	// assigns when the REQ carries none (and the cap applied to any
	// requested expiry). Default 5s (migration 058 "authoritative TTL").
	ReservationTTL time.Duration
	// MaxOpenPerAccount caps concurrent open reservations per account —
	// Phase-02 Task 2.3.14's "max 10 concurrent cross-shard operations"
	// (maps to CROSS_SHARD_LIMIT_EXCEEDED at the API layer).
	MaxOpenPerAccount int
	// RPCBudget mirrors the engine's 500µs soft budget: decisions slower
	// than this are counted (nLateDecisions) — the engine has already
	// fallen back to its local floor.
	RPCBudget time.Duration
	// HardDeadline mirrors the engine's >10ms hard deadline: a grant
	// decided slower than this is self-compensated (ACK + RELEASE).
	HardDeadline time.Duration
	// LedgerTimeout bounds every PG ledger call.
	LedgerTimeout time.Duration
	// SweepInterval is the expiry-sweeper cadence (RunSweeper).
	SweepInterval time.Duration
	// Now is the wall clock (expires_at + decision latency). Injectable.
	Now func() time.Time
	// Margins lazily resolves account books not pushed via
	// SetAccountMargin. Nil ⇒ unknown accounts NACK (fail closed).
	Margins AccountMarginSource
	// Alerter receives P1 ops alerts (ledger faults, orphan sweeps).
	Alerter OpsAlerter
	// Logf is the diagnostic logger; nil ⇒ discard.
	Logf func(format string, args ...any)
}

// MarginCoordinatorOption mutates a config at construction.
type MarginCoordinatorOption func(*MarginCoordinatorConfig)

// WithTimeouts injects the layered reservation budget (rpc 500µs /
// hard 10ms in production; arbitrary values in tests).
func WithTimeouts(rpcBudget, hardDeadline time.Duration) MarginCoordinatorOption {
	return func(c *MarginCoordinatorConfig) {
		c.RPCBudget, c.HardDeadline = rpcBudget, hardDeadline
	}
}

// WithClock injects the wall clock.
func WithClock(now func() time.Time) MarginCoordinatorOption {
	return func(c *MarginCoordinatorConfig) { c.Now = now }
}

func (c *MarginCoordinatorConfig) normalize() error {
	if c.ShardCount < 1 {
		return fmt.Errorf("margin coordinator: ShardCount %d < 1", c.ShardCount)
	}
	if c.ReservationTTL <= 0 {
		c.ReservationTTL = 5 * time.Second
	}
	if c.MaxOpenPerAccount < 1 {
		c.MaxOpenPerAccount = 10
	}
	if c.RPCBudget <= 0 {
		c.RPCBudget = 500 * time.Microsecond
	}
	if c.HardDeadline <= 0 {
		c.HardDeadline = 10 * time.Millisecond
	}
	if c.HardDeadline < c.RPCBudget {
		return fmt.Errorf("margin coordinator: HardDeadline %s < RPCBudget %s",
			c.HardDeadline, c.RPCBudget)
	}
	if c.LedgerTimeout <= 0 {
		c.LedgerTimeout = 250 * time.Millisecond
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = 200 * time.Millisecond
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Coordinator
// ---------------------------------------------------------------------------

// coordAccountBook is the coordinator's per-account margin book.
type coordAccountBook struct {
	known     bool             // equity/margin authoritative (pushed or loaded)
	equity    decimal.Decimal  // haircut-adjusted total equity (USD numeraire)
	margin    decimal.Decimal  // committed margin bound (MM; used_margin)
	reserved  decimal.Decimal  // Σ counted slices of active reservations
	open      int              // active reservation count
	openInstr map[int64]uint64 // instrument → open reservation id (mig-058 uq)
}

// headroom is equity − margin − reserved (may be negative → denies).
func (b *coordAccountBook) headroom() decimal.Decimal {
	return b.equity.Sub(b.margin).Sub(b.reserved)
}

// coordReservation is one tracked reservation. `counted` is the amount
// currently debited from the account book — cleared on release so
// unwind stays symmetric.
type coordReservation struct {
	id         uint64
	accountID  int64
	orderID    uint64
	consumer   uint32
	host       uint32
	instrument int64
	reqFlags   uint8
	amount     decimal.Decimal // requested slice
	granted    decimal.Decimal // ACK'd slice (== amount; full-grant policy)
	counted    decimal.Decimal // live debit against book.reserved
	status     ShardReservationStatus
	denyReason ipc.MarginNackReason // set for DENIED tombstones
	expiresAt  time.Time
	createdAt  time.Time
}

// rowFor projects the in-memory record onto the ledger row.
func (r *coordReservation) row() ShardReservationRow {
	var granted *decimal.Decimal
	if r.status == ShardReservationCommitted {
		g := r.granted
		granted = &g
	}
	reason := ""
	switch r.status {
	case ShardReservationDenied:
		reason = marginNackToken(r.denyReason)
	}
	return ShardReservationRow{
		ReservationID:  int64(r.id),
		AccountID:      r.accountID,
		ConsumerShard:  int(r.consumer),
		HostShard:      int(r.host),
		InstrumentID:   r.instrument,
		OrderID:        int64(r.orderID),
		ReservedAmount: r.amount,
		GrantedAmount:  granted,
		Currency:       "USD",
		ReqFlags:       int(r.reqFlags),
		Status:         r.status,
		ReleaseReason:  reason,
		ExpiresAt:      r.expiresAt,
		CreatedAt:      r.createdAt,
	}
}

// MarginCoordinatorStats is the monotonic-counter snapshot for
// metrics/tests (Prometheus mapping: Phase-07 Task 7.3.8).
type MarginCoordinatorStats struct {
	Reqs             uint64 // RESERVE_REQ frames / Reserve calls decided
	Acks             uint64
	Nacks            uint64
	Releases         uint64 // released transitions (any reason)
	Expired          uint64 // TTL-expiry releases
	Compensations    uint64 // timeout-compensate releases emitted
	Replayed         uint64 // idempotent re-decisions
	Recovered        uint64 // rows loaded at boot
	OrphansSwept     uint64 // recovered rows tombstoned at boot
	BadFrames        uint64 // malformed/anomalous wire frames
	LedgerFaults     uint64
	LateDecisions    uint64 // decisions slower than RPCBudget
	OpenReservations uint64 // currently live (PENDING|COMMITTED)
}

// MarginCoordinator is the global cross-shard margin authority.
type MarginCoordinator struct {
	cfg    MarginCoordinatorConfig
	ledger ShardMarginLedger

	mu       sync.Mutex
	accounts map[int64]*coordAccountBook
	res      map[uint64]*coordReservation
	seq      uint64 // coordinator-issued id sequence (low 48)

	nReqs, nAcks, nNacks, nRel, nExp, nComp, nReplay uint64
	nRecovered, nOrphans, nBad, nLedger, nLate       uint64
}

// NewMarginCoordinator builds the coordinator. The ledger is required —
// a coordinator without durable reservations would lose granted slices
// across restart and could over-allocate cross-shard headroom (§2.7).
func NewMarginCoordinator(cfg MarginCoordinatorConfig,
	ledger ShardMarginLedger, opts ...MarginCoordinatorOption) (*MarginCoordinator, error) {

	for _, o := range opts {
		o(&cfg)
	}
	if err := cfg.normalize(); err != nil {
		return nil, err
	}
	if ledger == nil {
		return nil, fmt.Errorf("margin coordinator: nil ShardMarginLedger")
	}
	return &MarginCoordinator{
		cfg: cfg, ledger: ledger,
		accounts: map[int64]*coordAccountBook{},
		res:      map[uint64]*coordReservation{},
	}, nil
}

// ---------------------------------------------------------------------------
// Account book feed
// ---------------------------------------------------------------------------

// SetAccountMargin pushes the account's margin buffer (§13.1's
// synchronized margin broadcast): equity is the haircut-adjusted total
// and margin the committed maintenance bound. A negative margin input
// is clamped to 0 — accepting it would inflate headroom (fail-closed).
func (c *MarginCoordinator) SetAccountMargin(accountID int64, equity, margin decimal.Decimal) {
	if accountID <= 0 {
		return
	}
	if margin.IsNegative() {
		margin = decimal.Zero
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.bookForLocked(accountID)
	b.known, b.equity, b.margin = true, equity, margin
}

// UntrackAccount drops the account book only when it carries no active
// reservations — a book in use is never dropped (dropping would
// overstate headroom, fail-closed direction violated).
func (c *MarginCoordinator) UntrackAccount(accountID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.accounts[accountID]
	if b == nil || b.open > 0 {
		return false
	}
	delete(c.accounts, accountID)
	return true
}

// bookForLocked returns the book, creating an unknown one.
func (c *MarginCoordinator) bookForLocked(accountID int64) *coordAccountBook {
	b := c.accounts[accountID]
	if b == nil {
		b = &coordAccountBook{openInstr: map[int64]uint64{}}
		c.accounts[accountID] = b
	}
	return b
}

// loadBookLocked resolves an unknown book through cfg.Margins (one-shot
// per call — unknown is never cached, so a later push/fetch retries).
// Returns (book, true) when authoritative, (book, false) when the
// account is unknown, or an error on source failure.
func (c *MarginCoordinator) loadBookLocked(ctx context.Context, accountID int64) (*coordAccountBook, bool, error) {
	b := c.bookForLocked(accountID)
	if b.known {
		return b, true, nil
	}
	if c.cfg.Margins == nil {
		return b, false, nil
	}
	lctx, cancel := context.WithTimeout(ctx, c.cfg.LedgerTimeout)
	defer cancel()
	eq, mm, ok, err := c.cfg.Margins.AccountMargin(lctx, accountID)
	if err != nil {
		return nil, false, fmt.Errorf("margin coordinator: margin source acct %d: %w", accountID, err)
	}
	if !ok {
		return b, false, nil
	}
	if mm.IsNegative() {
		mm = decimal.Zero
	}
	b.known, b.equity, b.margin = true, eq, mm
	return b, true, nil
}

// ---------------------------------------------------------------------------
// Reserve / Release — the 2PC reservation protocol (decision API)
// ---------------------------------------------------------------------------

// ReserveRequest is the coordinator-side reservation request — the
// decoded MarginReserveReqBody with decimal-native types.
type ReserveRequest struct {
	// ReservationID is issuer-assigned (high16 = issuing shard,
	// low48 = per-shard sequence); 0 → the coordinator assigns one.
	ReservationID uint64
	AccountID     int64
	OrderID       uint64 // admission context (0 = none)
	SrcShard      uint32 // consumer shard
	DstShard      uint32 // host shard, or ipc.MarginCoordinatorPicks
	InstrumentID  int64
	ReqFlags      uint8
	Amount        decimal.Decimal // required margin slice (account base)
	ExpiresAt     time.Time       // zero → coordinator assigns the TTL
}

// ReserveDecision is the outcome of a reservation request.
type ReserveDecision struct {
	ReservationID     uint64
	Granted           bool
	GrantedAmount     decimal.Decimal // == requested (full-grant policy)
	HostShard         uint32          // effective host (Picks resolved)
	NackReason        ipc.MarginNackReason
	AvailableHeadroom decimal.Decimal // headroom observed at decision time
	ExpiresAt         time.Time       // authoritative expiry
	Compensated       bool            // granted then released (>HardDeadline)
	Replayed          bool            // idempotent replay of a known id
}

// Reserve decides one reservation atomically — the test-usable twin of
// the wire path (HandleFrame). Decisions are serialized through the
// coordinator mutex; the ledger write is part of the decision
// (log-then-ACK), so two concurrent calls can never double-grant.
func (c *MarginCoordinator) Reserve(ctx context.Context, req ReserveRequest) (ReserveDecision, error) {
	var d ReserveDecision
	if req.AccountID <= 0 {
		return d, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("margin reserve: account_id %d", req.AccountID))
	}
	if !req.Amount.IsPositive() {
		return d, excerrors.New("INVALID_REQUEST",
			"margin reserve: amount must be > 0")
	}
	start := c.cfg.Now()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.nReqs++

	// Idempotent replay: a redelivered REQ re-answers the recorded
	// decision — mirrors the C++ GrantedHosted re-ACK / Denied tombstone.
	if r, ok := c.res[req.ReservationID]; ok && req.ReservationID != 0 {
		c.nReplay++
		return c.replayLocked(r), nil
	}

	id := req.ReservationID
	if id == 0 {
		c.seq++
		id = (CoordinatorReservationPrefix << 48) | (c.seq & coordIDLow48Mask)
	}
	d.ReservationID = id
	d.HostShard = req.DstShard
	if d.HostShard == ipc.MarginCoordinatorPicks {
		// Coordinator-picks policy: the slice is hosted by the consumer
		// shard itself — the coordinator's book is global per-account,
		// so host is audit/routing metadata only (the C++ engine treats
		// it the same way for RELEASE addressing).
		d.HostShard = req.SrcShard
	}

	// Authoritative expiry: honor the REQ inside (now, now+TTL]; anything
	// else clamps to now+TTL (a shard may not pin headroom forever, and
	// a stale expiry must not reject — it just gets a fresh TTL).
	exp := req.ExpiresAt
	if exp.IsZero() || !exp.After(start) || exp.After(start.Add(c.cfg.ReservationTTL)) {
		exp = start.Add(c.cfg.ReservationTTL)
	}
	d.ExpiresAt = exp

	book, ok, err := c.loadBookLocked(ctx, req.AccountID)
	if err != nil {
		return d, err
	}
	if !ok {
		// Unknown account — NACK, no ledger row (the account may not
		// exist; the FK would fault anyway). Fail closed.
		d.NackReason = ipc.MarginNackUnknownAccount
		c.nNacks++
		return d, nil
	}
	d.AvailableHeadroom = book.headroom()

	// One open reservation per (account, instrument) — the in-memory
	// mirror of shard_margin_reservations_open_uq (mig 058).
	if _, dup := book.openInstr[req.InstrumentID]; dup {
		return c.denyLocked(ctx, &d, req, exp,
			ipc.MarginNackCoordinatorOverload), nil
	}
	// Per-account concurrency cap (Phase-02 Task 2.3.14: max 10).
	if book.open >= c.cfg.MaxOpenPerAccount {
		return c.denyLocked(ctx, &d, req, exp,
			ipc.MarginNackCoordinatorOverload), nil
	}
	// Zero-breach headroom check: grant full amount or refuse.
	if d.AvailableHeadroom.LessThan(req.Amount) {
		return c.denyLocked(ctx, &d, req, exp,
			ipc.MarginNackInsufficientHeadroom), nil
	}

	r := &coordReservation{
		id: id, accountID: req.AccountID, orderID: req.OrderID,
		consumer: req.SrcShard, host: d.HostShard,
		instrument: req.InstrumentID, reqFlags: req.ReqFlags,
		amount: req.Amount, granted: req.Amount, counted: req.Amount,
		status:    ShardReservationCommitted,
		expiresAt: exp, createdAt: start,
	}
	c.res[id] = r
	book.reserved = book.reserved.Add(req.Amount)
	book.openInstr[req.InstrumentID] = id
	book.open++

	if err := c.insertLocked(ctx, r.row()); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if pgErr.ConstraintName == "shard_margin_reservations_pkey" {
				// PK replay across a restart: answer the recorded
				// decision instead of double-granting. The in-memory
				// grant above must unwind first.
				c.unwindLocked(r)
				d2, rerr := c.decisionFromRowLocked(ctx, &d)
				if rerr != nil {
					return d, rerr
				}
				return d2, nil
			}
			// open-(account,instrument) uniqueness raced us — deny.
			c.unwindLocked(r)
			return c.denyLocked(ctx, &d, req, exp,
				ipc.MarginNackCoordinatorOverload), nil
		}
		// Ledger fault: revoke the grant and refuse — a slice that is
		// not durable must never be ACK'd (fail closed, §2.7).
		c.unwindLocked(r)
		c.tombstoneDeniedLocked(ctx, r, ipc.MarginNackCoordinatorOverload)
		c.alertFault("insert reservation", err)
		return c.denyDecisionLocked(&d, r, ipc.MarginNackCoordinatorOverload), nil
	}

	d.Granted, d.GrantedAmount = true, req.Amount
	c.nAcks++

	// Layered-budget mirror: the decision ran past the >10ms hard
	// deadline — the engine already cancelled/compensated or will
	// reject the slice as late. Grant then immediately compensate so
	// the books never hold a slice the consumer cannot use.
	if elapsed := c.cfg.Now().Sub(start); elapsed > c.cfg.HardDeadline {
		c.finalizeLocked(ctx, r, ipc.MarginReleaseTimeoutCompensate, false)
		d.Compensated = true
		c.nComp++
	} else if elapsed > c.cfg.RPCBudget {
		c.nLate++
	}
	return d, nil
}

// replayLocked re-answers the recorded decision for a duplicate REQ.
func (c *MarginCoordinator) replayLocked(r *coordReservation) ReserveDecision {
	d := ReserveDecision{
		ReservationID: r.id, HostShard: r.host,
		ExpiresAt: r.expiresAt, Replayed: true,
	}
	switch r.status {
	case ShardReservationCommitted:
		d.Granted, d.GrantedAmount = true, r.granted
		c.nAcks++
	case ShardReservationDenied:
		d.NackReason = r.denyReason
		c.nNacks++
	default: // RELEASED / PENDING — cannot grant
		d.NackReason = ipc.MarginNackInsufficientHeadroom
		c.nNacks++
	}
	return d
}

// denyLocked records a DENIED tombstone (ledger + in-memory) so a
// replayed REQ deterministically re-NACKs.
func (c *MarginCoordinator) denyLocked(ctx context.Context, d *ReserveDecision,
	req ReserveRequest, exp time.Time, reason ipc.MarginNackReason) ReserveDecision {

	r := &coordReservation{
		id: d.ReservationID, accountID: req.AccountID, orderID: req.OrderID,
		consumer: req.SrcShard, host: d.HostShard,
		instrument: req.InstrumentID, reqFlags: req.ReqFlags,
		amount: req.Amount, status: ShardReservationDenied,
		denyReason: reason, expiresAt: exp, createdAt: c.cfg.Now(),
	}
	c.res[r.id] = r
	if err := c.insertLocked(ctx, r.row()); err != nil {
		c.alertFault("insert denial tombstone", err)
	}
	return c.denyDecisionLocked(d, r, reason)
}

func (c *MarginCoordinator) denyDecisionLocked(d *ReserveDecision,
	r *coordReservation, reason ipc.MarginNackReason) ReserveDecision {
	d.Granted, d.GrantedAmount = false, decimal.Zero
	d.NackReason = reason
	if r != nil {
		d.HostShard = r.host
		d.ExpiresAt = r.expiresAt
	}
	c.nNacks++
	return *d
}

// tombstoneDeniedLocked flips a live reservation record to DENIED after
// a ledger fault revoked its grant (keeps probe/dedupe semantics: a
// replayed REQ re-NACKs rather than re-deciding).
func (c *MarginCoordinator) tombstoneDeniedLocked(ctx context.Context,
	r *coordReservation, reason ipc.MarginNackReason) {
	r.status = ShardReservationDenied
	r.denyReason = reason
	// Best effort: the original insert already failed; this may too —
	// the in-memory DENIED tombstone alone still dedupes replays
	// correctly for the life of this process.
	if err := c.ledgerSetStatus(ctx, r.id, ShardReservationDenied,
		marginNackToken(reason)); err != nil {
		c.logf("margin coordinator: denial tombstone res %d: %v", r.id, err)
	}
}

// decisionFromRowLocked reconstructs the decision for a PK-collided
// reservation id — the durable row is authoritative across restarts.
func (c *MarginCoordinator) decisionFromRowLocked(ctx context.Context,
	d *ReserveDecision) (ReserveDecision, error) {

	lctx, cancel := context.WithTimeout(ctx, c.cfg.LedgerTimeout)
	defer cancel()
	row, err := c.ledger.ReservationByID(lctx, int64(d.ReservationID))
	if err != nil {
		return *d, fmt.Errorf("margin coordinator: reservation replay read %d: %w",
			d.ReservationID, err)
	}
	if row == nil {
		// 23505 yet no row — impossible state; fail closed.
		return *d, excerrors.New("INTERNAL_ERROR",
			fmt.Sprintf("margin coordinator: reservation %d PK conflict with no row",
				d.ReservationID))
	}
	d.HostShard = uint32(row.HostShard)
	d.ExpiresAt = row.ExpiresAt
	d.Replayed = true
	switch row.Status {
	case ShardReservationCommitted:
		d.Granted = true
		if row.GrantedAmount != nil {
			d.GrantedAmount = *row.GrantedAmount
		} else {
			d.GrantedAmount = row.ReservedAmount
		}
		c.nAcks++
	case ShardReservationDenied:
		d.NackReason = marginNackFromToken(row.ReleaseReason)
		c.nNacks++
	default: // RELEASED / PENDING
		d.NackReason = ipc.MarginNackInsufficientHeadroom
		c.nNacks++
	}
	return *d, nil
}

// Release idempotently releases a reservation — cancel/reject/IOC-expiry
// (restores headroom) or Complete (commits the slice into maintenance
// margin). Unknown or already-terminal ids are a no-op; returns false
// only when the id was never seen.
func (c *MarginCoordinator) Release(ctx context.Context, reservationID uint64,
	accountID int64, reason ipc.MarginReleaseReason) (bool, error) {

	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.res[reservationID]
	if r == nil {
		return false, nil // idempotent by contract
	}
	if accountID != 0 && accountID != r.accountID {
		// Account mismatch — anomaly, but revocation is the safe
		// direction; proceed and count it.
		c.nBad++
	}
	switch r.status {
	case ShardReservationReleased, ShardReservationDenied:
		return true, nil
	}
	c.finalizeLocked(ctx, r, reason, reason == ipc.MarginReleaseComplete)
	return true, nil
}

// Commit is the fill-side twin of Release: the granted slice converts to
// committed maintenance margin (reason Complete absorbs the debit into
// the account's margin book).
func (c *MarginCoordinator) Commit(ctx context.Context, reservationID uint64,
	accountID int64) (bool, error) {
	return c.Release(ctx, reservationID, accountID, ipc.MarginReleaseComplete)
}

// unwindLocked removes the reservation's live debit from its account
// book. The record itself is left for the caller to tombstone.
func (c *MarginCoordinator) unwindLocked(r *coordReservation) {
	if b := c.accounts[r.accountID]; b != nil && r.counted.IsPositive() {
		b.reserved = b.reserved.Sub(r.counted)
		if b.openInstr[r.instrument] == r.id {
			delete(b.openInstr, r.instrument)
		}
		if b.open > 0 {
			b.open--
		}
	}
	r.counted = decimal.Zero
}

// finalizeLocked transitions a live reservation to RELEASED: unwinds the
// headroom debit, optionally absorbs the slice into maintenance margin
// (Complete ⇒ commit-on-fill), updates the ledger (log-failure is an
// alert, never a resurrect — the in-memory tombstone keeps the engine's
// view honest; a stale open row only over-reserves on recovery, the
// safe direction).
func (c *MarginCoordinator) finalizeLocked(ctx context.Context,
	r *coordReservation, reason ipc.MarginReleaseReason, absorb bool) {

	if b := c.accounts[r.accountID]; b != nil {
		if absorb && r.counted.IsPositive() {
			b.margin = b.margin.Add(r.counted)
		}
	}
	c.unwindLocked(r)
	r.status = ShardReservationReleased
	c.nRel++
	token := marginReleaseToken(reason)
	if err := c.ledgerSetStatus(ctx, r.id, ShardReservationReleased, token); err != nil {
		c.alertFault("release reservation", err)
	}
}

// ---------------------------------------------------------------------------
// Wire path — the Aeron bridge drives HandleFrame / IngressFrame
// ---------------------------------------------------------------------------

// HandleFrame decodes one frame arriving on margin_ctl_in_{ingressShard}
// and returns the frames to publish (each OutboundFrame.Shard names the
// engine shard; publish on ipc.MarginCtlOutURI(shard), stream
// ipc.MarginCtlOutStreamID). Returns:
//
//	REQ     → [ACK] or [NACK] or [ACK, RELEASE] (hard-deadline compensate)
//	RELEASE → [] (idempotent application)
//	ACK/NACK → [] — protocol anomaly (the coordinator never issues REQs)
//
// A malformed frame is dropped with an error and never answered (the
// engine's own timeout ladder bounds the wait — answering garbage would
// only confuse the id space).
func (c *MarginCoordinator) HandleFrame(ctx context.Context,
	ingressShard uint32, buf []byte) ([]OutboundFrame, error) {

	var v ipc.MarginCtlView
	if rc := ipc.MarginCtlDecodeFrame(buf, &v); rc != ipc.MarginCtlDecodeOK {
		c.mu.Lock()
		c.nBad++
		c.mu.Unlock()
		return nil, fmt.Errorf("margin coordinator: bad frame on shard %d: %s",
			ingressShard, rc)
	}
	switch v.Type {
	case ipc.MarginCtlReserveReq:
		m := v.Req
		if m.ReservationID == 0 || m.AccountID == 0 || m.Amount <= 0 {
			c.mu.Lock()
			c.nBad++
			c.mu.Unlock()
			return nil, fmt.Errorf("margin coordinator: malformed REQ id=%d acct=%d amount=%d",
				m.ReservationID, m.AccountID, m.Amount)
		}
		dec, err := c.Reserve(ctx, ReserveRequest{
			ReservationID: m.ReservationID,
			AccountID:     int64(m.AccountID),
			OrderID:       m.OrderID,
			SrcShard:      m.SrcShard,
			DstShard:      m.DstShard,
			InstrumentID:  int64(m.InstrumentID),
			ReqFlags:      m.ReqFlags,
			Amount:        decimal.NewFromScaled(m.Amount),
			ExpiresAt:     nsToTime(m.ExpiresAtNs),
		})
		if err != nil {
			// Infrastructure fault — refuse the slice rather than let
			// the engine hang to its hard deadline (fail closed).
			return []OutboundFrame{c.nackFrame(ingressShard, m.ReservationID,
				m.AccountID, m.SrcShard, ipc.MarginNackCoordinatorOverload)}, err
		}
		if !dec.Granted {
			return []OutboundFrame{c.nackFrame(ingressShard, dec.ReservationID,
				m.AccountID, dec.HostShard, dec.NackReason)}, nil
		}
		out := []OutboundFrame{c.ackFrame(ingressShard, m.AccountID, dec)}
		if dec.Compensated {
			out = append(out, c.releaseFrame(ingressShard, dec.ReservationID,
				m.AccountID, dec.HostShard, ipc.MarginReleaseTimeoutCompensate))
		}
		return out, nil
	case ipc.MarginCtlRelease:
		m := v.Rel
		if _, err := c.Release(ctx, m.ReservationID, int64(m.AccountID),
			ipc.MarginReleaseReason(m.Reason)); err != nil {
			return nil, err
		}
		return nil, nil
	default:
		// ACK/NACK inbound: the coordinator issues no REQs — anomaly.
		c.mu.Lock()
		c.nBad++
		c.mu.Unlock()
		return nil, nil
	}
}

// IngressFrame is the bridge-facing alias of HandleFrame (the bridge
// drains margin_ctl_in_{shard} and publishes every returned frame).
func (c *MarginCoordinator) IngressFrame(ctx context.Context,
	ingressShard uint32, buf []byte) ([]OutboundFrame, error) {
	return c.HandleFrame(ctx, ingressShard, buf)
}

// ackFrame encodes MARGIN_RESERVE_ACK for the decision.
func (c *MarginCoordinator) ackFrame(shard uint32, accountID uint64,
	d ReserveDecision) OutboundFrame {
	f := OutboundFrame{Shard: shard, Data: make([]byte, ipc.MarginCtlMaxFrame)}
	n := ipc.MarginCtlEncode(f.Data, ipc.MarginCtlReserveAck, &ipc.MarginReserveAckBody{
		ReservationID: d.ReservationID,
		AccountID:     accountID,
		ShardID:       d.HostShard,
		GrantedAmount: decimal.Scaled(d.GrantedAmount),
		ExpiresAtNs:   uint64(d.ExpiresAt.UnixNano()),
	})
	f.Data = f.Data[:n]
	return f
}

func (c *MarginCoordinator) nackFrame(shard uint32, id, accountID uint64,
	host uint32, reason ipc.MarginNackReason) OutboundFrame {
	f := OutboundFrame{Shard: shard, Data: make([]byte, ipc.MarginCtlMaxFrame)}
	n := ipc.MarginCtlEncode(f.Data, ipc.MarginCtlReserveNack, &ipc.MarginReserveNackBody{
		ReservationID: id, AccountID: accountID, ShardID: host,
		Reason: uint32(reason),
	})
	f.Data = f.Data[:n]
	return f
}

func (c *MarginCoordinator) releaseFrame(shard uint32, id, accountID uint64,
	host uint32, reason ipc.MarginReleaseReason) OutboundFrame {
	f := OutboundFrame{Shard: shard, Data: make([]byte, ipc.MarginCtlMaxFrame)}
	n := ipc.MarginCtlEncode(f.Data, ipc.MarginCtlRelease, &ipc.MarginReleaseBody{
		ReservationID: id, AccountID: accountID, ShardID: host,
		Reason: uint8(reason),
	})
	f.Data = f.Data[:n]
	return f
}

// ---------------------------------------------------------------------------
// Expiry sweep + restart recovery
// ---------------------------------------------------------------------------

// SweepExpired releases every active reservation whose expires_at has
// passed and returns RELEASE frames the bridge must publish (one per
// consumer shard). Engine-side sweeps expire the same slices — the wire
// RELEASE just converges both books promptly.
func (c *MarginCoordinator) SweepExpired(ctx context.Context) []OutboundFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Now()
	var out []OutboundFrame
	for _, r := range c.res {
		if r.status != ShardReservationCommitted &&
			r.status != ShardReservationPending {
			continue
		}
		if r.expiresAt.IsZero() || r.expiresAt.After(now) {
			continue
		}
		out = append(out, c.releaseFrame(r.consumer, r.id,
			uint64(r.accountID), r.host, ipc.MarginReleaseExpired))
		c.finalizeLocked(ctx, r, ipc.MarginReleaseExpired, false)
		c.nExp++
	}
	return out
}

// RunSweeper drives SweepExpired on cfg.SweepInterval until ctx is done;
// emitter publishes each outbound frame (production: the Aeron bridge).
func (c *MarginCoordinator) RunSweeper(ctx context.Context, emitter MarginCtlEmitter) {
	t := time.NewTicker(c.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, f := range c.SweepExpired(ctx) {
				if emitter == nil {
					continue
				}
				if err := emitter.EmitMarginCtl(ctx, f.Shard, f.Data); err != nil {
					c.logf("margin coordinator: sweep emit shard %d: %v", f.Shard, err)
				}
			}
		}
	}
}

// Recover reloads the durable ledger at boot — call BEFORE opening for
// traffic. COMMITTED rows within TTL return to the books (engine may
// still hold the granted slice); expired rows and PENDING rows are
// tombstoned RELEASED(RecoveryOrphan) — PENDING was never ACK'd by
// construction (log-then-ACK), so it provably holds no granted slice and
// the RELEASE cancels the still-in-flight REQ cleanly. Returns the count
// of reactivated reservations plus the outbound RELEASE frames the
// bridge must publish.
func (c *MarginCoordinator) Recover(ctx context.Context) (int, []OutboundFrame, error) {
	lctx, cancel := context.WithTimeout(ctx, c.cfg.LedgerTimeout)
	defer cancel()
	rows, err := c.ledger.LoadActiveReservations(lctx)
	if err != nil {
		return 0, nil, fmt.Errorf("margin coordinator: load active reservations: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.cfg.Now()
	var out []OutboundFrame
	applied := 0
	for i := range rows {
		row := rows[i]
		id := uint64(row.ReservationID)
		if _, dup := c.res[id]; dup {
			continue // already tracked — idempotent
		}
		// Reseed the coordinator id sequence past coordinator-issued ids.
		if (id >> 48) == CoordinatorReservationPrefix {
			if low := id & coordIDLow48Mask; low > c.seq {
				c.seq = low
			}
		}
		amount := row.ReservedAmount
		if row.GrantedAmount != nil {
			amount = *row.GrantedAmount
		}
		r := &coordReservation{
			id: id, accountID: row.AccountID, orderID: uint64(row.OrderID),
			consumer: uint32(row.ConsumerShard), host: uint32(row.HostShard),
			instrument: row.InstrumentID, reqFlags: uint8(row.ReqFlags),
			amount: row.ReservedAmount, granted: amount,
			status:    row.Status,
			expiresAt: row.ExpiresAt, createdAt: row.CreatedAt,
		}
		needsTombstone := row.Status == ShardReservationPending ||
			row.ExpiresAt.IsZero() || !row.ExpiresAt.After(now)
		if needsTombstone {
			out = append(out, c.releaseFrame(r.consumer, r.id,
				uint64(r.accountID), r.host, ipc.MarginReleaseRecoveryOrphan))
			r.status = ShardReservationReleased
			if err := c.ledgerSetStatus(ctx, r.id, ShardReservationReleased,
				marginReleaseToken(ipc.MarginReleaseRecoveryOrphan)); err != nil {
				c.alertFault("recover orphan tombstone", err)
			}
			c.res[id] = r
			c.nOrphans++
			continue
		}
		r.counted = amount
		c.res[id] = r
		b := c.bookForLocked(row.AccountID)
		b.reserved = b.reserved.Add(amount)
		if _, ok := b.openInstr[row.InstrumentID]; !ok {
			b.openInstr[row.InstrumentID] = id
		}
		b.open++
		applied++
		c.nRecovered++
	}
	return applied, out, nil
}

// ---------------------------------------------------------------------------
// Pessimistic partition floor + introspection
// ---------------------------------------------------------------------------

// AvailableHeadroom reports the account's current global headroom
// (equity − margin − Σ reservations). ok=false when the account is
// unknown — callers must treat unknown as zero headroom (fail closed).
func (c *MarginCoordinator) AvailableHeadroom(ctx context.Context,
	accountID int64) (decimal.Decimal, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok, err := c.loadBookLocked(ctx, accountID)
	if err != nil || !ok {
		return decimal.Zero, ok, err
	}
	h := b.headroom()
	if h.IsNegative() {
		h = decimal.Zero
	}
	return h, true, nil
}

// PessimisticFloor is the §13.1 partition-degradation floor: current
// account headroom divided equally across shards. It is the per-shard
// guaranteed slice — floor × ShardCount never exceeds headroom
// (truncated division), so an engine admitting on its floor alone can
// never breach the global book while the coordinator is unreachable.
func (c *MarginCoordinator) PessimisticFloor(ctx context.Context,
	accountID int64) (decimal.Decimal, bool, error) {
	h, ok, err := c.AvailableHeadroom(ctx, accountID)
	if err != nil || !ok {
		return decimal.Zero, ok, err
	}
	floor := h.Div(decimal.NewFromInt(int64(c.cfg.ShardCount))).Truncate(int32(decimal.Scale))
	if floor.IsNegative() {
		floor = decimal.Zero
	}
	return floor, true, nil
}

// ReservationStatus reports the tracked status of one reservation id
// ("" when unknown) — introspection for tests/metrics.
func (c *MarginCoordinator) ReservationStatus(reservationID uint64) ShardReservationStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r := c.res[reservationID]; r != nil {
		return r.status
	}
	return ""
}

// Stats returns the monotonic counters.
func (c *MarginCoordinator) Stats() MarginCoordinatorStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	open := uint64(0)
	for _, r := range c.res {
		if r.status == ShardReservationCommitted || r.status == ShardReservationPending {
			open++
		}
	}
	return MarginCoordinatorStats{
		Reqs: c.nReqs, Acks: c.nAcks, Nacks: c.nNacks, Releases: c.nRel,
		Expired: c.nExp, Compensations: c.nComp, Replayed: c.nReplay,
		Recovered: c.nRecovered, OrphansSwept: c.nOrphans,
		BadFrames: c.nBad, LedgerFaults: c.nLedger, LateDecisions: c.nLate,
		OpenReservations: open,
	}
}

// ---------------------------------------------------------------------------
// internals
// ---------------------------------------------------------------------------

func (c *MarginCoordinator) insertLocked(ctx context.Context, row ShardReservationRow) error {
	lctx, cancel := context.WithTimeout(ctx, c.cfg.LedgerTimeout)
	defer cancel()
	if err := c.ledger.InsertReservation(lctx, row); err != nil {
		c.nLedger++
		return err
	}
	return nil
}

func (c *MarginCoordinator) ledgerSetStatus(ctx context.Context, id uint64,
	status ShardReservationStatus, reason string) error {
	lctx, cancel := context.WithTimeout(ctx, c.cfg.LedgerTimeout)
	defer cancel()
	if err := c.ledger.SetStatus(lctx, int64(id), status, reason); err != nil {
		c.nLedger++
		return err
	}
	return nil
}

func (c *MarginCoordinator) alertFault(op string, err error) {
	if c.cfg.Alerter == nil {
		c.logf("margin coordinator: %s failed (no alerter): %v", op, err)
		return
	}
	actx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if rerr := c.cfg.Alerter.Raise(actx, OpsAlert{
		Severity: SeverityP1, Code: codeMarginLedgerFault,
		Summary: fmt.Sprintf("margin coordinator ledger fault during %s", op),
		Err:     err.Error(),
	}); rerr != nil {
		c.logf("margin coordinator: alert dispatch failed: %v", rerr)
	}
}

func (c *MarginCoordinator) logf(format string, args ...any) {
	c.cfg.Logf(format, args...)
}

// nsToTime converts a CLOCK_REALTIME ns value to time.Time; 0 or
// overflow yields the zero Time (the REQ-expiry clamp handles it).
func nsToTime(ns uint64) time.Time {
	if ns == 0 || ns > math.MaxInt64 {
		return time.Time{}
	}
	return time.Unix(0, int64(ns)).UTC()
}

// marginReleaseToken renders the wire ReleaseReason for
// release_reason VARCHAR(32) (migration 058: "carries the wire
// ReleaseReason/NackReason token").
func marginReleaseToken(r ipc.MarginReleaseReason) string {
	switch r {
	case ipc.MarginReleaseComplete:
		return "COMPLETE"
	case ipc.MarginReleaseOrderRejected:
		return "ORDER_REJECTED"
	case ipc.MarginReleaseTimeoutCompensate:
		return "TIMEOUT_COMPENSATE"
	case ipc.MarginReleaseExpired:
		return "EXPIRED"
	case ipc.MarginReleaseCoordinatorInitiated:
		return "COORDINATOR_INITIATED"
	case ipc.MarginReleaseRecoveryOrphan:
		return "RECOVERY_ORPHAN"
	}
	return fmt.Sprintf("UNKNOWN_%d", uint8(r))
}

func marginNackToken(r ipc.MarginNackReason) string {
	switch r {
	case ipc.MarginNackInsufficientHeadroom:
		return "INSUFFICIENT_HEADROOM"
	case ipc.MarginNackUnknownAccount:
		return "UNKNOWN_ACCOUNT"
	case ipc.MarginNackCoordinatorOverload:
		return "COORDINATOR_OVERLOAD"
	case ipc.MarginNackShardUnreachable:
		return "SHARD_UNREACHABLE"
	}
	return fmt.Sprintf("UNKNOWN_%d", uint32(r))
}

func marginNackFromToken(t string) ipc.MarginNackReason {
	switch t {
	case "INSUFFICIENT_HEADROOM":
		return ipc.MarginNackInsufficientHeadroom
	case "UNKNOWN_ACCOUNT":
		return ipc.MarginNackUnknownAccount
	case "COORDINATOR_OVERLOAD":
		return ipc.MarginNackCoordinatorOverload
	case "SHARD_UNREACHABLE":
		return ipc.MarginNackShardUnreachable
	}
	return ipc.MarginNackInsufficientHeadroom
}

// ---------------------------------------------------------------------------
// PgShardMarginLedger — PostgreSQL implementation (migration 058)
// ---------------------------------------------------------------------------

// PgShardMarginLedger implements ShardMarginLedger and AccountMarginSource
// over pgx. Monetary columns scan as ::text into the decimal facade —
// never float64 (package convention).
type PgShardMarginLedger struct {
	Pool *pgxpool.Pool
}

var (
	_ ShardMarginLedger   = (*PgShardMarginLedger)(nil)
	_ AccountMarginSource = (*PgShardMarginLedger)(nil)
)

// NewPgShardMarginLedger binds the pool — nil is rejected fail-closed.
func NewPgShardMarginLedger(pool *pgxpool.Pool) (*PgShardMarginLedger, error) {
	if pool == nil {
		return nil, fmt.Errorf("shard margin ledger: nil pgx pool")
	}
	return &PgShardMarginLedger{Pool: pool}, nil
}

// InsertReservation implements ShardMarginLedger.
func (l *PgShardMarginLedger) InsertReservation(ctx context.Context,
	row ShardReservationRow) error {

	var granted, reason any
	if row.GrantedAmount != nil {
		granted = row.GrantedAmount.String()
	}
	if row.ReleaseReason != "" {
		reason = row.ReleaseReason
	}
	ccy := row.Currency
	if ccy == "" {
		ccy = "USD"
	}
	created := any(nil)
	if !row.CreatedAt.IsZero() {
		created = row.CreatedAt
	}
	_, err := l.Pool.Exec(ctx, `
		INSERT INTO shard_margin_reservations
		    (reservation_id, account_id, consumer_shard, host_shard,
		     instrument_id, order_id, reserved_amount, granted_amount,
		     currency, req_flags, status, release_reason, expires_at, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8::numeric,$9,$10,
		        $11::shard_reservation_status_enum,$12,$13,
		        COALESCE($14::timestamptz, now()))`,
		row.ReservationID, row.AccountID, row.ConsumerShard, row.HostShard,
		row.InstrumentID, row.OrderID, row.ReservedAmount.String(), granted,
		ccy, row.ReqFlags, string(row.Status), reason, row.ExpiresAt, created)
	if err != nil {
		return fmt.Errorf("insert shard reservation %d: %w", row.ReservationID, err)
	}
	return nil
}

// SetStatus implements ShardMarginLedger — a missing row is an error,
// never a silent pass (§2.7: the coordinator must not believe a
// transition persisted when it did not).
func (l *PgShardMarginLedger) SetStatus(ctx context.Context, reservationID int64,
	status ShardReservationStatus, releaseReason string) error {

	var reason any
	if releaseReason != "" {
		reason = releaseReason
	}
	tag, err := l.Pool.Exec(ctx, `
		UPDATE shard_margin_reservations
		SET status = $2::shard_reservation_status_enum,
		    release_reason = COALESCE($3, release_reason),
		    released_at = CASE WHEN $2::shard_reservation_status_enum = 'RELEASED'
		                       THEN now() ELSE released_at END,
		    updated_at = now()
		WHERE reservation_id = $1`, reservationID, string(status), reason)
	if err != nil {
		return fmt.Errorf("shard reservation %d → %s: %w", reservationID, status, err)
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New("NOT_FOUND",
			fmt.Sprintf("shard reservation %d: no row for status %s", reservationID, status))
	}
	return nil
}

// shardReservationCols is the shared projection (decimals as ::text).
const shardReservationCols = `
	reservation_id, account_id, consumer_shard, host_shard, instrument_id,
	order_id, reserved_amount::text, granted_amount::text, currency,
	req_flags, status::text, COALESCE(release_reason,''), expires_at, created_at`

func scanShardReservation(row pgx.Row) (*ShardReservationRow, error) {
	var (
		r        ShardReservationRow
		reserved string
		granted  *string
		status   string
	)
	if err := row.Scan(&r.ReservationID, &r.AccountID, &r.ConsumerShard,
		&r.HostShard, &r.InstrumentID, &r.OrderID, &reserved, &granted,
		&r.Currency, &r.ReqFlags, &status, &r.ReleaseReason,
		&r.ExpiresAt, &r.CreatedAt); err != nil {
		return nil, err
	}
	var err error
	if r.ReservedAmount, err = decimal.NewFromString(reserved); err != nil {
		return nil, fmt.Errorf("reservation %d reserved_amount %q: %w",
			r.ReservationID, reserved, err)
	}
	if granted != nil {
		g, err := decimal.NewFromString(*granted)
		if err != nil {
			return nil, fmt.Errorf("reservation %d granted_amount %q: %w",
				r.ReservationID, *granted, err)
		}
		r.GrantedAmount = &g
	}
	r.Status = ShardReservationStatus(status)
	return &r, nil
}

// ReservationByID implements ShardMarginLedger — (nil, nil) when absent.
func (l *PgShardMarginLedger) ReservationByID(ctx context.Context,
	reservationID int64) (*ShardReservationRow, error) {
	r, err := scanShardReservation(l.Pool.QueryRow(ctx,
		`SELECT `+shardReservationCols+`
		 FROM shard_margin_reservations WHERE reservation_id = $1`, reservationID))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("shard reservation %d read: %w", reservationID, err)
	}
	return r, nil
}

// LoadActiveReservations implements ShardMarginLedger — every
// PENDING|COMMITTED row, id order for deterministic replay.
func (l *PgShardMarginLedger) LoadActiveReservations(ctx context.Context) ([]ShardReservationRow, error) {
	rows, err := l.Pool.Query(ctx, `
		SELECT `+shardReservationCols+`
		FROM shard_margin_reservations
		WHERE status IN ('PENDING','COMMITTED')
		ORDER BY reservation_id`)
	if err != nil {
		return nil, fmt.Errorf("load active shard reservations: %w", err)
	}
	defer rows.Close()
	var out []ShardReservationRow
	for rows.Next() {
		r, err := scanShardReservation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// AccountMargin implements AccountMarginSource over margin_accounts —
// equity is the haircut-adjusted snapshot the margin engine writes;
// used_margin is the committed-margin bound (IM ≥ MM ⇒ conservative).
// A missing row is (0,0,false,nil): unknown account → NACK.
func (l *PgShardMarginLedger) AccountMargin(ctx context.Context,
	accountID int64) (decimal.Decimal, decimal.Decimal, bool, error) {

	var eqTxt, usedTxt *string
	err := l.Pool.QueryRow(ctx, `
		SELECT equity::text, used_margin::text
		FROM margin_accounts WHERE account_id = $1`, accountID).
		Scan(&eqTxt, &usedTxt)
	if err == pgx.ErrNoRows {
		return decimal.Zero, decimal.Zero, false, nil
	}
	if err != nil {
		return decimal.Zero, decimal.Zero, false,
			fmt.Errorf("account margin %d: %w", accountID, err)
	}
	eq, used := decimal.Zero, decimal.Zero
	if eqTxt != nil {
		if eq, err = decimal.NewFromString(*eqTxt); err != nil {
			return decimal.Zero, decimal.Zero, false,
				fmt.Errorf("account %d equity %q: %w", accountID, *eqTxt, err)
		}
	}
	if usedTxt != nil {
		if used, err = decimal.NewFromString(*usedTxt); err != nil {
			return decimal.Zero, decimal.Zero, false,
				fmt.Errorf("account %d used_margin %q: %w", accountID, *usedTxt, err)
		}
	}
	return eq, used, true, nil
}
