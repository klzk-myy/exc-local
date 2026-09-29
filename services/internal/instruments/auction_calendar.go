// Phase-15 Task 15.3.13 — the per-instrument daily closing-auction
// calendar (spec §7.1/§7.3, §24 #401; migration 087 auction_calendar).
//
// Recurrence is DST-aware by construction: trigger_time is the local
// wall-clock in the row's timezone (16:00 Europe/London stays 16:00
// London across the BST switch); NextOccurrence resolves the UTC instant
// per candidate date.
//
// The scheduler drives the §7.1 close auction on the documented engine
// control contract (shared with Task 15.3.1/15.3.6 — reference.go
// package doc):
//
//	instrument:auction:{symbol}        "CALL:{deadline_unix_ns}" then
//	                                   "EXTEND:{deadline_unix_ns}" on
//	                                   each 30s extension (max 3)
//	instrument:auction:{symbol}:queue  JSON []int64 — queued MOC ids
//	instrument:auction:{symbol}:result "CLEARED" | "FAILED" — written by
//	                                   the engine at each deadline; an
//	                                   absent result counts FAILED
//	                                   (strict pessimism, spec §2.7)
//
// At the final failed extension the instrument transitions to SUSPENDED
// (AUCTION_CLEARING_FAILED, §7.3) through the lifecycle audit shape and
// the committed-publish seam — the 5-minute cancel-only sweep then
// drains resting orders via the existing SUSPENDED grace machinery.
// On a cleared uncross the unfilled MOC remainder is cancelled through
// the scoped mass-cancel seam (spec: MOC remainder cancels).
package instruments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"
	"exchange/internal/audit"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Calendar row model
// ---------------------------------------------------------------------------

// Auction types (087 CHECK).
const (
	AuctionDailyClose = "DAILY_CLOSE"
	AuctionFixing     = "BENCHMARK_FIXING"
	AuctionIntraday   = "INTRADAY"
)

// Benchmarks (087 CHECK + 222 benchmark_fixings).
const (
	BenchWMLondon = "WM_LONDON_4PM" // 16:00 Europe/London
	BenchECB      = "ECB_REF_1415"  // 14:15 Europe/Berlin (CET)
	BenchTokyo    = "TOKYO_0955"    // 09:55 Asia/Tokyo
)

// CalendarEntry is one auction_calendar row. TriggerTime is the local
// wall-clock "HH:MM" in Timezone.
type CalendarEntry struct {
	ID           int64      `json:"id,omitempty"`
	InstrumentID int64      `json:"instrument_id,omitempty"`
	Symbol       string     `json:"symbol,omitempty"`
	AuctionType  string     `json:"auction_type"`
	Benchmark    string     `json:"benchmark,omitempty"`
	TriggerTime  string     `json:"trigger_time"` // "HH:MM" in Timezone
	Timezone     string     `json:"timezone"`     // IANA name
	Recurrence   string     `json:"recurrence"`   // e.g. "MON-FRI", "FRI", "DAILY"
	Enabled      bool       `json:"enabled"`
	LastFiredAt  *time.Time `json:"last_fired_at,omitempty"`
}

// Validate checks the row against the 087 constraints — writes go
// through this so a bad row cannot persist.
func (e *CalendarEntry) Validate() error {
	switch e.AuctionType {
	case AuctionDailyClose, AuctionFixing, AuctionIntraday:
	default:
		return excerrors.New("INVALID_REQUEST",
			"auction_type must be DAILY_CLOSE|BENCHMARK_FIXING|INTRADAY")
	}
	if e.AuctionType == AuctionFixing {
		switch e.Benchmark {
		case BenchWMLondon, BenchECB, BenchTokyo:
		default:
			return excerrors.New("INVALID_REQUEST",
				"benchmark must be WM_LONDON_4PM|ECB_REF_1415|TOKYO_0955")
		}
	} else if e.Benchmark != "" {
		return excerrors.New("INVALID_REQUEST",
			"benchmark is only valid on BENCHMARK_FIXING rows")
	}
	if _, err := parseHM(e.TriggerTime); err != nil {
		return excerrors.New("INVALID_REQUEST", err.Error())
	}
	if _, err := time.LoadLocation(e.Timezone); err != nil {
		return excerrors.New("INVALID_REQUEST", "unknown timezone "+e.Timezone)
	}
	if _, err := parseRecurrence(e.Recurrence); err != nil {
		return excerrors.New("INVALID_REQUEST", err.Error())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Recurrence — weekday-mask + wall-clock resolution
// ---------------------------------------------------------------------------

var weekdayNames = map[string]time.Weekday{
	"SUN": time.Sunday, "MON": time.Monday, "TUE": time.Tuesday,
	"WED": time.Wednesday, "THU": time.Thursday, "FRI": time.Friday,
	"SAT": time.Saturday,
}

// parseRecurrence parses "MON-FRI", "FRI", "MON,WED,FRI", "DAILY" into a
// weekday mask.
func parseRecurrence(s string) ([7]bool, error) {
	var mask [7]bool
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || s == "DAILY" {
		for i := range mask {
			mask[i] = true
		}
		return mask, nil
	}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if i := strings.Index(part, "-"); i > 0 {
			from, ok1 := weekdayNames[part[:i]]
			to, ok2 := weekdayNames[part[i+1:]]
			if !ok1 || !ok2 {
				return mask, fmt.Errorf("auction calendar: bad range %q", part)
			}
			for d := from; ; d = (d + 1) % 7 {
				mask[d] = true
				if d == to {
					break
				}
			}
			continue
		}
		d, ok := weekdayNames[part]
		if !ok {
			return mask, fmt.Errorf("auction calendar: bad weekday %q", part)
		}
		mask[d] = true
	}
	return mask, nil
}

// NextOccurrence resolves the first trigger instant strictly after
// `after` — the DST-aware core: the wall-clock trigger is resolved on
// each candidate date in the row's timezone, so a 16:00 London trigger
// yields 16:00 UTC in winter and 15:00 UTC under BST.
func (e *CalendarEntry) NextOccurrence(after time.Time) (time.Time, error) {
	mask, err := parseRecurrence(e.Recurrence)
	if err != nil {
		return time.Time{}, err
	}
	mark, err := parseHM(e.TriggerTime)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(e.Timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("auction calendar: load tz %s: %w",
			e.Timezone, err)
	}
	local := after.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	for i := 0; i < 400; i++ { // covers masks with ≤1 hit/week plus margin
		d := day.AddDate(0, 0, i)
		if !mask[d.Weekday()] {
			continue
		}
		at := time.Date(d.Year(), d.Month(), d.Day(), mark.h, mark.m, 0, 0, loc)
		if at.After(after) {
			return at.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("auction calendar: no occurrence within 400 days")
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

// CalendarStore persists auction_calendar rows.
type CalendarStore struct {
	pool *pgxpool.Pool
}

// NewCalendarStore wires the store.
func NewCalendarStore(pool *pgxpool.Pool) *CalendarStore {
	return &CalendarStore{pool: pool}
}

const calendarCols = `
	id, instrument_id, symbol, auction_type, COALESCE(benchmark,''),
	to_char(trigger_time,'HH24:MI'), timezone, recurrence, enabled, last_fired_at`

// CalendarFor returns the rows for one symbol ordered by id.
func (s *CalendarStore) CalendarFor(ctx context.Context, symbol string) ([]CalendarEntry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+calendarCols+` FROM auction_calendar WHERE symbol=$1 ORDER BY id`,
		strings.ToUpper(strings.TrimSpace(symbol)))
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "calendar read", err)
	}
	defer rows.Close()
	out := []CalendarEntry{}
	for rows.Next() {
		var e CalendarEntry
		if err := rows.Scan(&e.ID, &e.InstrumentID, &e.Symbol, &e.AuctionType,
			&e.Benchmark, &e.TriggerTime, &e.Timezone, &e.Recurrence,
			&e.Enabled, &e.LastFiredAt); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan calendar row", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DueAuctions returns enabled DAILY_CLOSE/INTRADAY rows whose latest
// occurrence is due — the first trigger strictly after last_fired_at
// that lands at or before now. A never-fired row looks back only
// missedAuctionWindow so a freshly seeded calendar does not arm a
// stale auction mid-session.
func (s *CalendarStore) DueAuctions(ctx context.Context, now time.Time) ([]CalendarEntry, time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+calendarCols+` FROM auction_calendar
		 WHERE enabled AND auction_type IN ('DAILY_CLOSE','INTRADAY')
		 ORDER BY id`)
	if err != nil {
		return nil, now, excerrors.Wrap("INTERNAL_ERROR", "due scan", err)
	}
	defer rows.Close()
	out := []CalendarEntry{}
	for rows.Next() {
		var e CalendarEntry
		if err := rows.Scan(&e.ID, &e.InstrumentID, &e.Symbol, &e.AuctionType,
			&e.Benchmark, &e.TriggerTime, &e.Timezone, &e.Recurrence,
			&e.Enabled, &e.LastFiredAt); err != nil {
			return nil, now, excerrors.Wrap("INTERNAL_ERROR", "scan due row", err)
		}
		anchor := now.Add(-missedAuctionWindow)
		if e.LastFiredAt != nil {
			anchor = *e.LastFiredAt
		}
		next, err := e.NextOccurrence(anchor)
		if err != nil || next.After(now) {
			continue
		}
		out = append(out, e)
	}
	return out, now, rows.Err()
}

// MarkFired stamps last_fired_at for a fired occurrence (durable
// deduplication — a restarted scheduler does not re-fire the slot).
func (s *CalendarStore) MarkFired(ctx context.Context, id int64, at time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE auction_calendar SET last_fired_at=$2, updated_at=now() WHERE id=$1`,
		id, at)
	return err
}

// ReplaceInTx replaces the full calendar for an instrument inside the
// caller's transaction (the OpInstrumentCalendar approval tx) and writes
// the audit row. Full-replace semantics: the PUT body is authoritative —
// rows absent from the body are removed; the id-keyed uniqueness rule
// means an unchanged row reinserts identically.
func (s *CalendarStore) ReplaceInTx(ctx context.Context, tx pgx.Tx,
	actorID int64, symbol string, entries []CalendarEntry) error {

	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	var instID int64
	err := tx.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol=$1 FOR UPDATE`, symbol).Scan(&instID)
	if errors.Is(err, pgx.ErrNoRows) {
		return excerrors.New("NOT_FOUND", "instrument "+symbol+" not found")
	}
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "lock instrument", err)
	}
	for i := range entries {
		entries[i].Symbol = symbol
		entries[i].InstrumentID = instID
		if err := entries[i].Validate(); err != nil {
			return err
		}
	}

	var before json.RawMessage
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(jsonb_agg(to_jsonb(c) ORDER BY c.id), '[]'::jsonb)
		  FROM auction_calendar c WHERE c.instrument_id=$1`,
		instID).Scan(&before); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "calendar snapshot", err)
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM auction_calendar WHERE instrument_id=$1`, instID); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "calendar delete", err)
	}
	for _, e := range entries {
		var bench any
		if e.Benchmark != "" {
			bench = e.Benchmark
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO auction_calendar
			    (instrument_id, symbol, auction_type, benchmark,
			     trigger_time, timezone, recurrence, enabled)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			instID, symbol, e.AuctionType, bench, e.TriggerTime,
			e.Timezone, e.Recurrence, e.Enabled); err != nil {
			return excerrors.Wrap("INTERNAL_ERROR", "calendar insert", err)
		}
	}
	after, _ := json.Marshal(entries)
	if _, _, err := admin.Log(ctx, tx, admin.AuditEntry{
		AdminUserID: actorID, Action: "instrument.calendar.replace",
		TargetType: "instrument", TargetID: &instID,
		BeforeState: json.RawMessage(before),
		AfterState:  json.RawMessage(after),
	}); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "audit log", err)
	}
	return nil
}

// RegisterCalendarExecutor attaches the OpInstrumentCalendar executor —
// the PUT /auction-calendar approval applies the full-replace inside the
// approval transaction (spec §7.5/Task 15.3.13 item 6 dual control).
func RegisterCalendarExecutor(dual *admin.DualControlService,
	store *CalendarStore) {
	dual.RegisterExecutor(admin.OpInstrumentCalendar,
		func(ctx context.Context, tx pgx.Tx, req *admin.DualControlRequest) error {
			var p struct {
				Symbol   string          `json:"symbol"`
				Entries  []CalendarEntry `json:"entries"`
				ClientIP string          `json:"client_ip"`
			}
			if err := json.Unmarshal(req.Payload, &p); err != nil {
				return excerrors.Wrap("INVALID_REQUEST", "decode calendar payload", err)
			}
			actorID := req.RequestedBy
			_ = p.ClientIP
			return store.ReplaceInTx(ctx, tx, actorID, p.Symbol, p.Entries)
		})
}

// ---------------------------------------------------------------------------
// Engine seams
// ---------------------------------------------------------------------------

// AuctionFeed is the Redis control-plane writer for the auction keys —
// the same key contract admin.RedisStatusFeed owns for the reopening
// CALL, extended with the queue/result legs this scheduler drives.
type AuctionFeed interface {
	SetAuctionCall(ctx context.Context, symbol string, deadlineUnixNs int64) error
	SetAuctionExtend(ctx context.Context, symbol string, deadlineUnixNs int64) error
	SetAuctionQueue(ctx context.Context, symbol string, orderIDs []int64) error
	GetAuctionResult(ctx context.Context, symbol string) (string, error)
	DelAuction(ctx context.Context, symbol string) error
}

// AuctionOrderReader lists the resting MOC order ids the uncross
// consumes (the 15-minute pre-close accumulation per §7.1).
type AuctionOrderReader interface {
	RestingOrderIDs(ctx context.Context, instrumentID int64,
		orderType string) ([]int64, error)
}

// AuctionOrderCanceller cancels the unfilled remainder after the uncross
// (scoped mass-cancel: instrument × order_type).
type AuctionOrderCanceller interface {
	CancelOrderType(ctx context.Context, instrumentID int64,
		orderType, reason string) (int, error)
}

// PgAuctionOrders is the Pg-backed AuctionOrderReader — open MOC/MOO ids.
type PgAuctionOrders struct{ pool *pgxpool.Pool }

// NewPgAuctionOrders wires the reader.
func NewPgAuctionOrders(pool *pgxpool.Pool) *PgAuctionOrders {
	return &PgAuctionOrders{pool: pool}
}

// RestingOrderIDs returns open orders of the given type for the
// instrument (PENDING/RESERVED/ACTIVE/PARTIALLY_FILLED — the book-live set).
func (r *PgAuctionOrders) RestingOrderIDs(ctx context.Context, instrumentID int64,
	orderType string) ([]int64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id FROM orders
		 WHERE instrument_id=$1 AND order_type=$2
		   AND status IN ('PENDING','RESERVED','ACTIVE','PARTIALLY_FILLED')
		 ORDER BY id`, instrumentID, orderType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Scheduler — CALL arm, deadline eval, failure ladder
// ---------------------------------------------------------------------------

// Auction timing constants (spec §7.1/§7.3).
const (
	CallAuctionLen   = 5 * time.Minute  // 5-minute call
	AuctionExtend    = 30 * time.Second // per-extension increment
	AuctionMaxExtend = 3                // max extensions before SUSPENDED

	// missedAuctionWindow bounds the never-fired catch-up: a trigger
	// older than this is stale — the CALL would hold the book mid-session
	// for a slot that already closed.
	missedAuctionWindow = time.Hour
)

// auctionState is the in-memory ladder bookkeeping for one armed
// auction; the durable marks (CALL/EXTEND keys, last_fired_at, the
// SUSPENDED row + audit) make restarts safe — the in-flight extension
// count is best-effort (a mid-auction restart re-arms evaluation from
// the live key's deadline).
type auctionState struct {
	entryID      int64
	instrumentID int64
	deadline     time.Time
	extensions   int
}

// AuctionScheduler fires the daily close (and intraday) auctions and
// walks the failure ladder.
type AuctionScheduler struct {
	pool        *pgxpool.Pool
	store       *CalendarStore
	feed        AuctionFeed
	orders      AuctionOrderReader
	canceller   AuctionOrderCanceller
	instruments *admin.InstrumentService // PublishCommitted seam
	ws          admin.InstrumentWSPublisher
	now         func() time.Time
	logf        func(format string, args ...any)
	armed       map[string]*auctionState // symbol → in-flight auction
}

// AuctionDeps wires the scheduler. Pool, Store, Feed and Instruments are
// required — a scheduler that cannot publish the CALL fails closed.
type AuctionDeps struct {
	Pool        *pgxpool.Pool
	Store       *CalendarStore
	Feed        AuctionFeed
	Orders      AuctionOrderReader
	Canceller   AuctionOrderCanceller // nil → remainder cancel skipped + logged
	Instruments *admin.InstrumentService
	WS          admin.InstrumentWSPublisher
	Now         func() time.Time
	Logf        func(format string, args ...any)
}

// NewAuctionScheduler wires the scheduler.
func NewAuctionScheduler(d AuctionDeps) (*AuctionScheduler, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("auction scheduler: pgx pool is nil")
	}
	if d.Store == nil {
		return nil, fmt.Errorf("auction scheduler: calendar store is nil")
	}
	if d.Feed == nil {
		return nil, fmt.Errorf("auction scheduler: auction feed is nil")
	}
	if d.Instruments == nil {
		return nil, fmt.Errorf("auction scheduler: lifecycle service is nil")
	}
	s := &AuctionScheduler{
		pool: d.Pool, store: d.Store, feed: d.Feed, orders: d.Orders,
		canceller: d.Canceller, instruments: d.Instruments, ws: d.WS,
		now: d.Now, logf: d.Logf, armed: map[string]*auctionState{},
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// Tick runs one scheduler pass — fire due auctions, evaluate armed ones.
// Called by Run on the sweep interval; exported for tests.
func (s *AuctionScheduler) Tick(ctx context.Context) {
	now := s.now()
	s.evaluateArmed(ctx, now)
	s.fireDue(ctx, now)
}

// Run is the tick loop. interval ≤ 0 defaults to 5s — auction deadlines
// are minute-scale; the eval cadence only needs to bound the extension
// observability lag.
func (s *AuctionScheduler) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

// fireDue arms every enabled row whose occurrence is due.
func (s *AuctionScheduler) fireDue(ctx context.Context, now time.Time) {
	if s.orders == nil {
		return // cannot build the MOC queue — skip firing entirely
	}
	due, _, err := s.store.DueAuctions(ctx, now)
	if err != nil {
		s.logf("auction scheduler: due scan: %v", err)
		return
	}
	for _, e := range due {
		if _, armed := s.armed[e.Symbol]; armed {
			continue // one auction per symbol at a time
		}
		if err := s.fire(ctx, e, now); err != nil {
			s.logf("auction scheduler: fire %s: %v", e.Symbol, err)
		}
	}
}

// fire arms the CALL: queue the resting MOC orders, publish the
// instrument:auction:{symbol} keys, mark fired.
func (s *AuctionScheduler) fire(ctx context.Context, e CalendarEntry, now time.Time) error {
	deadline := now.Add(CallAuctionLen)

	moc, err := s.orders.RestingOrderIDs(ctx, e.InstrumentID, "MOC")
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "MOC queue read", err)
	}
	moo, err := s.orders.RestingOrderIDs(ctx, e.InstrumentID, "MOO")
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "MOO queue read", err)
	}
	queue := append(append([]int64{}, moc...), moo...)

	// Order: queue first, then CALL — the engine reads the queue once the
	// CALL key arms it; a CALL without a queue row risks an empty uncross.
	if err := s.feed.SetAuctionQueue(ctx, e.Symbol, queue); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "auction queue publish", err)
	}
	if err := s.feed.SetAuctionCall(ctx, e.Symbol, deadline.UnixNano()); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "auction CALL publish", err)
	}
	if err := s.store.MarkFired(ctx, e.ID, now); err != nil {
		s.logf("auction scheduler: mark fired %d: %v", e.ID, err)
	}
	s.armed[e.Symbol] = &auctionState{
		entryID: e.ID, instrumentID: e.InstrumentID, deadline: deadline,
	}
	s.logf("auction scheduler: CALL armed %s (queue=%d, deadline=%s)",
		e.Symbol, len(queue), deadline.UTC().Format(time.RFC3339))
	if s.ws != nil {
		s.ws.Publish(admin.InstrumentStatusChannel, admin.InstrumentStatusEvent{
			Event: "AUCTION_CALL", Symbol: e.Symbol,
			GraceDeadline: deadline.UnixMilli(),
			Source:        "auction-scheduler", TsMs: now.UnixMilli(),
		})
	}
	return nil
}

// evaluateArmed walks every armed auction's deadline → CLEARED /
// EXTEND ladder → SUSPENDED.
func (s *AuctionScheduler) evaluateArmed(ctx context.Context, now time.Time) {
	for symbol, st := range s.armed {
		if now.Before(st.deadline) {
			continue
		}
		result, err := s.feed.GetAuctionResult(ctx, symbol)
		if err != nil {
			s.logf("auction scheduler: result probe %s: %v", symbol, err)
		}
		switch strings.ToUpper(result) {
		case "CLEARED":
			s.clear(ctx, symbol, st)
		default:
			// FAILED, absent, or a probe error — strict pessimism: the
			// ladder advances; the uncross either confirms CLEARED at the
			// next deadline or the instrument suspends.
			s.extendOrSuspend(ctx, symbol, st)
		}
	}
}

// clear handles a successful uncross — drop the control keys and cancel
// the unfilled MOC remainder (spec §7.1: unfilled MOC remainder cancels).
func (s *AuctionScheduler) clear(ctx context.Context, symbol string, st *auctionState) {
	delete(s.armed, symbol)
	if err := s.feed.DelAuction(ctx, symbol); err != nil {
		s.logf("auction scheduler: clear keys %s: %v", symbol, err)
	}
	if s.canceller != nil {
		for _, typ := range []string{"MOC", "MOO"} {
			n, err := s.canceller.CancelOrderType(ctx, st.instrumentID,
				typ, "AUCTION_UNFILLED_REMAINDER")
			if err != nil {
				s.logf("auction scheduler: remainder cancel %s %s: %v",
					symbol, typ, err)
			} else if n > 0 {
				s.logf("auction scheduler: %s cancelled %d unfilled %s orders",
					symbol, n, typ)
			}
		}
	}
	if s.ws != nil {
		s.ws.Publish(admin.InstrumentStatusChannel, admin.InstrumentStatusEvent{
			Event: "AUCTION_CLEARED", Symbol: symbol,
			Source: "auction-scheduler", TsMs: s.now().UnixMilli(),
		})
	}
}

// extendOrSuspend walks the failure ladder: EXTEND +30s (≤3) then
// SUSPENDED with AUCTION_CLEARING_FAILED (spec §7.3).
func (s *AuctionScheduler) extendOrSuspend(ctx context.Context, symbol string, st *auctionState) {
	now := s.now()
	if st.extensions < AuctionMaxExtend {
		st.extensions++
		st.deadline = now.Add(AuctionExtend)
		if err := s.feed.SetAuctionExtend(ctx, symbol, st.deadline.UnixNano()); err != nil {
			s.logf("auction scheduler: extend %s: %v", symbol, err)
		}
		s.logf("auction scheduler: %s auction not cleared — EXTEND %d/%d",
			symbol, st.extensions, AuctionMaxExtend)
		if s.ws != nil {
			s.ws.Publish(admin.InstrumentStatusChannel, admin.InstrumentStatusEvent{
				Event: "AUCTION_EXTEND", Symbol: symbol,
				GraceDeadline: st.deadline.UnixMilli(),
				Source:        "auction-scheduler", TsMs: now.UnixMilli(),
			})
		}
		return
	}
	// Ladder exhausted — SUSPENDED via the lifecycle audit shape; the
	// committed-publish seam then lands the status key + sweep deadline.
	delete(s.armed, symbol)
	if err := s.suspendForFailedAuction(ctx, st.instrumentID, symbol); err != nil {
		s.logf("auction scheduler: suspend %s: %v", symbol, err)
		return
	}
	if err := s.feed.DelAuction(ctx, symbol); err != nil {
		s.logf("auction scheduler: clear keys %s: %v", symbol, err)
	}
}

// suspendForFailedAuction flips the instrument to SUSPENDED in-tx with
// the lifecycle-shaped audit row, then publishes via PublishCommitted —
// the engine gate, sweep deadline and WS event mirror a manual suspend.
func (s *AuctionScheduler) suspendForFailedAuction(ctx context.Context,
	instrumentID int64, symbol string) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "begin tx", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var cur string
	err = tx.QueryRow(ctx,
		`SELECT status::text FROM instruments WHERE id=$1 FOR UPDATE`,
		instrumentID).Scan(&cur)
	if err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "lock instrument", err)
	}
	switch cur {
	case admin.InstActive, admin.InstCancelOnly, admin.InstRestricted:
		// legal SUSPENDED sources (spec §7.1 edge set)
	case admin.InstSuspended:
		_ = tx.Rollback(ctx)
		return nil // already suspended
	default:
		_ = tx.Rollback(ctx)
		return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
			"cannot suspend "+cur+" after auction failure")
	}
	if _, err := tx.Exec(ctx, `
		UPDATE instruments SET status='SUSPENDED', updated_at=now()
		 WHERE id=$1`, instrumentID); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "suspend", err)
	}
	// admin_user_id 0 = the scheduler (system principal — no FK on the
	// column; the audit payload records the machine attribution).
	var auditID int64
	after, _ := json.Marshal(map[string]any{
		"status": admin.InstSuspended, "reason": "AUCTION_CLEARING_FAILED",
		"extensions": AuctionMaxExtend, "system": true,
	})
	before, _ := json.Marshal(map[string]any{"status": cur})
	if err := tx.QueryRow(ctx, `
		INSERT INTO admin_audit_log
		    (admin_user_id, action, target_type, target_id,
		     before_state, after_state)
		VALUES (0,'instrument.lifecycle.suspend','instrument',$1,$2,$3)
		RETURNING id`, instrumentID, json.RawMessage(before),
		json.RawMessage(after)).Scan(&auditID); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "audit insert", err)
	}
	if _, err := audit.Append(ctx, tx, "admin_audit_log", &auditID,
		"INSERT", nil); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "audit chain", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return excerrors.Wrap("INTERNAL_ERROR", "commit", err)
	}
	if err := s.instruments.PublishCommitted(ctx, instrumentID); err != nil {
		s.logf("auction scheduler: publish suspend %s: %v", symbol, err)
	}
	return nil
}

// PendingQueue returns the currently queued auction order ids for a
// symbol — the ops board's view into the pre-close MOC accumulation.
func (s *AuctionScheduler) PendingQueue(ctx context.Context, symbol string) ([]int64, error) {
	var instID int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol=$1`,
		strings.ToUpper(strings.TrimSpace(symbol))).Scan(&instID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New("NOT_FOUND", "instrument "+symbol+" not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "instrument lookup", err)
	}
	if s.orders == nil {
		return nil, excerrors.New("SERVICE_DEGRADED", "order reader unwired")
	}
	moc, err := s.orders.RestingOrderIDs(ctx, instID, "MOC")
	if err != nil {
		return nil, err
	}
	return moc, nil
}
