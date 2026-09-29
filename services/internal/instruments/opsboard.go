// Phase-15 Task 15.3.12 — operations-board queries (spec §7.5,
// §24 #352). One read model behind GET /api/v1/admin/ops-board that the
// venue-ops console renders: every non-ACTIVE instrument with its grace
// deadline, pending listing proposals, the instrument-related pending
// four-eyes queue, delisting ladder state, upcoming auction/fixing
// occurrences and operational warnings (status drift vs the engine feed,
// missing reference rows, overdue activations, purge-ready delists).
//
// Everything is a READ over the durable rows — the board never mutates;
// action endpoints (transitions, reviews, calendar edits) stay on their
// own routes so the board works for Read-Only Auditor too.
package instruments

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/admin"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Board model
// ---------------------------------------------------------------------------

// BoardInstrument is one instruments row rendered with its grace window
// (the lifecycle engine's §7.1 grace semantics: SUSPENDED 5m,
// RESTRICTED 24h, DELISTED 30d measured from updated_at).
type BoardInstrument struct {
	InstrumentID    int64      `json:"instrument_id"`
	Symbol          string     `json:"symbol"`
	Status          string     `json:"status"`
	StateEnteredAt  time.Time  `json:"state_entered_at"`
	GraceKind       string     `json:"grace_kind,omitempty"`
	GraceDeadline   *time.Time `json:"grace_deadline,omitempty"`
	EngineStatus    string     `json:"engine_status,omitempty"` // Redis instrument:status gate
	StatusDrift     bool       `json:"status_drift,omitempty"`  // PG ≠ engine gate
	ReferenceSeeded bool       `json:"reference_seeded"`
	DelistPhase     string     `json:"delist_phase,omitempty"`
}

// BoardProposal is a pending listing_proposals row (compact view).
type BoardProposal struct {
	ID         int64           `json:"id"`
	Symbol     string          `json:"symbol"`
	ProposerID int64           `json:"proposer_id"`
	Status     string          `json:"status"`
	AutoChecks json.RawMessage `json:"auto_checks"`
	ActivateAt *time.Time      `json:"activate_at,omitempty"`
	Overdue    bool            `json:"overdue"` // SCHEDULED past activate_at
	CreatedAt  time.Time       `json:"created_at"`
}

// BoardApproval is a pending four-eyes request on the instrument surface.
type BoardApproval struct {
	ID           int64     `json:"id"`
	Operation    string    `json:"operation"`
	TargetID     string    `json:"target_id"`
	RequestedBy  int64     `json:"requested_by"`
	RequiredRole string    `json:"required_role"`
	ExpiresAt    time.Time `json:"expires_at"`
	CreatedAt    time.Time `json:"created_at"`
}

// BoardAuction is the next scheduled occurrence of one enabled
// auction_calendar row.
type BoardAuction struct {
	EntryID     int64      `json:"entry_id"`
	Symbol      string     `json:"symbol"`
	AuctionType string     `json:"auction_type"`
	Benchmark   string     `json:"benchmark,omitempty"`
	NextAt      time.Time  `json:"next_at"`
	TriggerTime string     `json:"trigger_time"`
	Timezone    string     `json:"timezone"`
	Recurrence  string     `json:"recurrence"`
	LastFiredAt *time.Time `json:"last_fired_at,omitempty"`
}

// BoardFixing is today's benchmark-fixing state for one instrument —
// recorded rows from benchmark_fixings joined against expected
// benchmarks (absent rows are the gap the scheduler must close).
type BoardFixing struct {
	Symbol      string     `json:"symbol"`
	Benchmark   string     `json:"benchmark"`
	ScheduledAt time.Time  `json:"scheduled_at,omitempty"`
	Status      string     `json:"status"` // RECORDED|SKIPPED|FAILED|PENDING
	FiredAt     *time.Time `json:"fired_at,omitempty"`
	Rate        string     `json:"rate,omitempty"`
}

// Board is the ops-board document.
type Board struct {
	GeneratedAt      time.Time         `json:"generated_at"`
	Instruments      []BoardInstrument `json:"instruments"` // non-ACTIVE only
	PendingProposals []BoardProposal   `json:"pending_proposals"`
	PendingApprovals []BoardApproval   `json:"pending_approvals"` // instrument-surface ops only
	UpcomingAuctions []BoardAuction    `json:"upcoming_auctions"`
	TodayFixings     []BoardFixing     `json:"today_fixings"`
	Warnings         []string          `json:"warnings"`
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// StatusProbe reads the engine status gate (admin.RedisStatusFeed
// satisfies it). nil → drift checks skipped with a warning, never
// silently absent.
type StatusProbe interface {
	GetStatus(ctx context.Context, symbol string) (string, error)
}

// OpsBoardDeps wires the board service.
type OpsBoardDeps struct {
	Pool *pgxpool.Pool
	Feed StatusProbe
	Now  func() time.Time
	Logf func(format string, args ...any)
}

// OpsBoardService serves GET /api/v1/admin/ops-board.
type OpsBoardService struct {
	pool *pgxpool.Pool
	feed StatusProbe
	now  func() time.Time
	logf func(format string, args ...any)
}

// NewOpsBoardService wires the service. Pool is required.
func NewOpsBoardService(d OpsBoardDeps) (*OpsBoardService, error) {
	if d.Pool == nil {
		return nil, fmt.Errorf("ops board: pgx pool is nil")
	}
	s := &OpsBoardService{
		pool: d.Pool, feed: d.Feed, now: d.Now, logf: d.Logf,
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s, nil
}

// graceDeadline mirrors the lifecycle engine's withGrace mapping.
func graceDeadline(status string, entered time.Time) (kind string, deadline *time.Time) {
	var d time.Time
	switch status {
	case admin.InstSuspended:
		kind, d = "CANCEL_ONLY_WINDOW", entered.Add(admin.SuspendedGrace)
	case admin.InstRestricted:
		kind, d = "DELIST_NOTICE", entered.Add(admin.RestrictedGrace)
	case admin.InstDelisted:
		kind, d = "CLOSE_ONLY", entered.Add(admin.DelistedGrace)
	default:
		return "", nil
	}
	return kind, &d
}

// Board aggregates every section. A failed section degrades to a
// warning — the board is an operational surface, so a partial read beats
// a 500 (the failed section's data is absent, never fabricated).
func (s *OpsBoardService) Board(ctx context.Context) (*Board, error) {
	b := &Board{
		GeneratedAt:      s.now().UTC(),
		Instruments:      []BoardInstrument{},
		PendingProposals: []BoardProposal{},
		PendingApprovals: []BoardApproval{},
		UpcomingAuctions: []BoardAuction{},
		TodayFixings:     []BoardFixing{},
		Warnings:         []string{},
	}
	now := s.now()

	// --- non-ACTIVE instruments + engine-gate drift -------------------
	rows, err := s.pool.Query(ctx, `
		SELECT i.id, i.symbol, i.status::text, i.updated_at,
		       (r.instrument_id IS NOT NULL), COALESCE(r.delist_schedule->>'phase','')
		  FROM instruments i
		  LEFT JOIN instruments_reference r ON r.instrument_id = i.id
		 WHERE i.status <> 'ACTIVE' ORDER BY i.symbol`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "board instruments", err)
	}
	for rows.Next() {
		var bi BoardInstrument
		if err := rows.Scan(&bi.InstrumentID, &bi.Symbol, &bi.Status,
			&bi.StateEnteredAt, &bi.ReferenceSeeded, &bi.DelistPhase); err != nil {
			rows.Close()
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan board instrument", err)
		}
		bi.GraceKind, bi.GraceDeadline = graceDeadline(bi.Status, bi.StateEnteredAt)
		if s.feed != nil {
			gate, err := s.feed.GetStatus(ctx, bi.Symbol)
			switch {
			case err != nil:
				b.Warnings = append(b.Warnings,
					fmt.Sprintf("%s: engine status probe failed: %v", bi.Symbol, err))
			case gate != bi.Status:
				bi.EngineStatus, bi.StatusDrift = gate, true
				b.Warnings = append(b.Warnings,
					fmt.Sprintf("%s: PG status %s but engine gate %q — reconciler should converge", bi.Symbol, bi.Status, gate))
			default:
				bi.EngineStatus = gate
			}
		}
		if !bi.ReferenceSeeded && bi.Status != admin.InstDraft {
			b.Warnings = append(b.Warnings,
				bi.Symbol+": non-DRAFT instrument has no instruments_reference row")
		}
		b.Instruments = append(b.Instruments, bi)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "board instruments rows", err)
	}
	if s.feed == nil {
		b.Warnings = append(b.Warnings,
			"engine status probe unwired — drift checks skipped")
	}

	// --- pending listing proposals ------------------------------------
	prows, err := s.pool.Query(ctx, `
		SELECT id, symbol, proposer_id, status, auto_checks, activate_at, created_at
		  FROM listing_proposals
		 WHERE status IN ('PROPOSED','IN_REVIEW','APPROVED','SCHEDULED')
		 ORDER BY id`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "board proposals", err)
	}
	for prows.Next() {
		var bp BoardProposal
		if err := prows.Scan(&bp.ID, &bp.Symbol, &bp.ProposerID, &bp.Status,
			&bp.AutoChecks, &bp.ActivateAt, &bp.CreatedAt); err != nil {
			prows.Close()
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan proposal", err)
		}
		if bp.Status == PropScheduled && bp.ActivateAt != nil && bp.ActivateAt.Before(now) {
			bp.Overdue = true
			b.Warnings = append(b.Warnings,
				fmt.Sprintf("proposal %d (%s) overdue for activation since %s",
					bp.ID, bp.Symbol, bp.ActivateAt.UTC().Format(time.RFC3339)))
		}
		b.PendingProposals = append(b.PendingProposals, bp)
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "board proposals rows", err)
	}

	// --- pending four-eyes approvals on the instrument surface --------
	arows, err := s.pool.Query(ctx, `
		SELECT id, operation, target_id, requested_by, required_role,
		       expires_at, created_at
		  FROM admin_dual_control_requests
		 WHERE status='PENDING'
		   AND operation IN ('instrument-create','instrument-resume',
		                     'instrument-delist','instrument-listing',
		                     'instrument-calendar','instrument-maintenance')
		 ORDER BY id`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "board approvals", err)
	}
	for arows.Next() {
		var ba BoardApproval
		if err := arows.Scan(&ba.ID, &ba.Operation, &ba.TargetID,
			&ba.RequestedBy, &ba.RequiredRole, &ba.ExpiresAt, &ba.CreatedAt); err != nil {
			arows.Close()
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan approval", err)
		}
		if ba.ExpiresAt.Before(now) {
			b.Warnings = append(b.Warnings,
				fmt.Sprintf("dual-control request %d (%s %s) expired but still PENDING — sweep will mark EXPIRED",
					ba.ID, ba.Operation, ba.TargetID))
		}
		b.PendingApprovals = append(b.PendingApprovals, ba)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "board approvals rows", err)
	}

	// --- upcoming auction/fixing occurrences --------------------------
	b.UpcomingAuctions, b.TodayFixings, err = s.scheduleBoard(ctx, now)
	if err != nil {
		b.Warnings = append(b.Warnings, "auction/fixing schedule read failed: "+err.Error())
	}
	return b, nil
}

// scheduleBoard computes the next occurrence per enabled calendar row and
// today's recorded fixings. Kept separate so a missing auction_calendar
// (pre-087 schema) degrades the board section, not the endpoint.
func (s *OpsBoardService) scheduleBoard(ctx context.Context, now time.Time) ([]BoardAuction, []BoardFixing, error) {

	arows, err := s.pool.Query(ctx, `
		SELECT c.id, c.symbol, c.auction_type, COALESCE(c.benchmark,''),
		       to_char(c.trigger_time,'HH24:MI'), c.timezone, c.recurrence, c.last_fired_at
		  FROM auction_calendar c
		  JOIN instruments i ON i.id = c.instrument_id
		 WHERE c.enabled AND i.status <> 'DELISTED'
		 ORDER BY c.symbol, c.id`)
	if err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "board calendar scan", err)
	}
	var auctions []BoardAuction
	for arows.Next() {
		var e BoardAuction
		var lastFired *time.Time
		if err := arows.Scan(&e.EntryID, &e.Symbol, &e.AuctionType,
			&e.Benchmark, &e.TriggerTime, &e.Timezone, &e.Recurrence,
			&lastFired); err != nil {
			arows.Close()
			return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "scan calendar row", err)
		}
		e.LastFiredAt = lastFired
		entry := CalendarEntry{
			AuctionType: e.AuctionType, TriggerTime: e.TriggerTime,
			Timezone: e.Timezone, Recurrence: e.Recurrence, Enabled: true,
		}
		next, err := entry.NextOccurrence(now)
		if err != nil {
			continue // bad recurrence — surfaced via tests, skip row
		}
		e.NextAt = next
		auctions = append(auctions, e)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, nil, excerrors.Wrap("INTERNAL_ERROR", "calendar rows", err)
	}
	sort.Slice(auctions, func(i, j int) bool {
		if auctions[i].NextAt.Equal(auctions[j].NextAt) {
			return auctions[i].Symbol < auctions[j].Symbol
		}
		return auctions[i].NextAt.Before(auctions[j].NextAt)
	})

	// Today's fixings: every enabled BENCHMARK_FIXING row's next-past or
	// today-scheduled occurrence joined to its benchmark_fixings record.
	dayStart := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(),
		0, 0, 0, 0, time.UTC)
	fixings := []BoardFixing{}
	frows, err := s.pool.Query(ctx, `
		SELECT c.symbol, c.benchmark, f.scheduled_at, f.status, f.fired_at,
		       COALESCE(f.rate::text,'')
		  FROM auction_calendar c
		  JOIN instruments i ON i.id = c.instrument_id
		  LEFT JOIN benchmark_fixings f
		    ON f.instrument_id = c.instrument_id AND f.benchmark = c.benchmark
		   AND f.scheduled_at >= $1
		 WHERE c.enabled AND c.auction_type='BENCHMARK_FIXING'
		   AND i.status <> 'DELISTED'
		 ORDER BY c.symbol, c.benchmark`, dayStart)
	if err != nil {
		return auctions, nil, excerrors.Wrap("INTERNAL_ERROR", "board fixings scan", err)
	}
	for frows.Next() {
		var bf BoardFixing
		var sched *time.Time
		var fired *time.Time
		var status *string
		if err := frows.Scan(&bf.Symbol, &bf.Benchmark, &sched, &status,
			&fired, &bf.Rate); err != nil {
			frows.Close()
			return auctions, nil, excerrors.Wrap("INTERNAL_ERROR", "scan fixing", err)
		}
		if sched != nil {
			bf.ScheduledAt = *sched
		}
		bf.FiredAt = fired
		if status != nil {
			bf.Status = *status
		} else {
			bf.Status = "PENDING"
		}
		fixings = append(fixings, bf)
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return auctions, nil, excerrors.Wrap("INTERNAL_ERROR", "fixing rows", err)
	}
	return auctions, fixings, nil
}

// PendingDelists returns the delist ladder state for the board —
// instruments_reference rows carrying a non-terminal delist_schedule.
func (s *OpsBoardService) PendingDelists(ctx context.Context) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT symbol, delist_schedule
		  FROM instruments_reference
		 WHERE delist_schedule <> '{}'::jsonb
		 ORDER BY symbol`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "delist ladder read", err)
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var symbol string
		var raw json.RawMessage
		if err := rows.Scan(&symbol, &raw); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "scan ladder", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		m["symbol"] = symbol
		out = append(out, m)
	}
	return out, rows.Err()
}
