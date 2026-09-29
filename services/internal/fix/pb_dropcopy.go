// Task 18.3.6 — Prime Brokerage Drop Copy (spec §5.22, §9.2, §24 #125).
//
// Give-up flow: when a give-up-eligible client account (one with an ACTIVE
// pb_credit_limits row — the give-up agreement record per LP/broker) fills,
// the execution is (a) persisted to pb_giveup_trades as PENDING, (b) copied
// in real time to the PB's dedicated FIX session with the §9.2 Parties group
// populated, and (c) submitted to the affirmation pipeline (affirmation.go).
// PENDING→AFFIRMED/REJECTED is driven by affirmation responses; DISPUTED/
// SETTLED are owned by Phase-24 Task 24.3.7 reconciliation.
package fix

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/quickfixgo/quickfix"
)

// ---------------------------------------------------------------------------
// Domain types (migration 037 / spec §5.22).
// ---------------------------------------------------------------------------

// GiveUpStatus mirrors giveup_status_enum.
type GiveUpStatus string

const (
	GiveUpPending  GiveUpStatus = "PENDING"
	GiveUpAffirmed GiveUpStatus = "AFFIRMED"
	GiveUpRejected GiveUpStatus = "REJECTED"
	GiveUpDisputed GiveUpStatus = "DISPUTED" // Phase-24 Task 24.3.7 break investigation
	GiveUpSettled  GiveUpStatus = "SETTLED"
)

// giveupTransitions is the legal giveup_status state machine. PENDING is the
// only entry state; SETTLED is terminal; DISPUTED is the Phase-24 break
// investigation state reachable from any non-terminal state.
var giveupTransitions = map[GiveUpStatus][]GiveUpStatus{
	GiveUpPending:  {GiveUpAffirmed, GiveUpRejected, GiveUpDisputed},
	GiveUpAffirmed: {GiveUpDisputed, GiveUpSettled},
	GiveUpRejected: {GiveUpDisputed},
	GiveUpDisputed: {GiveUpAffirmed, GiveUpRejected, GiveUpSettled},
	GiveUpSettled:  {},
}

// CanTransition reports whether from→to is a legal give-up status move.
func CanTransition(from, to GiveUpStatus) bool {
	for _, s := range giveupTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// PrimeBroker mirrors the prime_brokers row.
type PrimeBroker struct {
	ID          int64
	Name        string
	BIC         string
	FixCompID   string // FIX session CompID the drop copy routes to
	TraianaCode string // Traiana Harmony participant id ("" = not onboarded)
	Status      string // 'ACTIVE' | 'SUSPENDED'
}

// GiveUpTrade mirrors the pb_giveup_trades row.
type GiveUpTrade struct {
	ID                    int64
	TradeID               int64
	PrimeBrokerID         int64
	ExecutingBrokerAcctID int64
	ClientAccountID       int64
	Status                GiveUpStatus
	TraianaMessageID      string
	RejectionReason       string
	AffirmedAt            *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// ---------------------------------------------------------------------------
// PBStore — persistence for migration 037 tables.
// ---------------------------------------------------------------------------

// PBStore is the PostgreSQL store for prime brokerage state.
type PBStore struct{ pool *pgxpool.Pool }

// NewPBStore binds the store to a pool.
func NewPBStore(pool *pgxpool.Pool) *PBStore { return &PBStore{pool: pool} }

// pbColumns is the canonical prime_brokers projection.
const pbColumns = `id, pb_name, bic_code, fix_comp_id, COALESCE(traiana_code,''), status::text`

// PrimeBrokerByCompID resolves a PB by its FIX drop-copy CompID.
func (s *PBStore) PrimeBrokerByCompID(ctx context.Context, compID string) (PrimeBroker, error) {
	var pb PrimeBroker
	err := s.pool.QueryRow(ctx,
		`SELECT `+pbColumns+` FROM prime_brokers WHERE fix_comp_id=$1`, compID).
		Scan(&pb.ID, &pb.Name, &pb.BIC, &pb.FixCompID, &pb.TraianaCode, &pb.Status)
	return pb, err
}

// EligiblePrimeBrokers returns the ACTIVE prime brokers a client account may
// give up to: one per ACTIVE pb_credit_limits row under that account — the
// lp/broker record is the give-up eligibility registry.
func (s *PBStore) EligiblePrimeBrokers(ctx context.Context, clientAccountID int64) ([]PrimeBroker, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT pb.id, pb.pb_name, pb.bic_code, pb.fix_comp_id,
		        COALESCE(pb.traiana_code,''), pb.status::text
		 FROM prime_brokers pb
		 JOIN pb_credit_limits cl ON cl.prime_broker_id = pb.id
		 WHERE cl.client_account_id = $1 AND pb.status = 'ACTIVE'
		 ORDER BY pb.id`, clientAccountID)
	if err != nil {
		return nil, fmt.Errorf("fix: pb eligibility: %w", err)
	}
	defer rows.Close()
	var out []PrimeBroker
	for rows.Next() {
		var pb PrimeBroker
		if err := rows.Scan(&pb.ID, &pb.Name, &pb.BIC, &pb.FixCompID, &pb.TraianaCode, &pb.Status); err != nil {
			return nil, err
		}
		out = append(out, pb)
	}
	return out, rows.Err()
}

const giveupColumns = `id, trade_id, prime_broker_id, executing_broker_account_id,
	client_account_id, giveup_status::text, COALESCE(traiana_message_id,''),
	COALESCE(rejection_reason,''), affirmed_at, created_at, updated_at`

// RecordGiveUp inserts a PENDING give-up row for (trade, pb). The
// UNIQUE(trade_id, prime_broker_id) constraint makes re-copied fills
// idempotent: a repeat returns the existing row, never a duplicate give-up.
func (s *PBStore) RecordGiveUp(ctx context.Context, tradeID, pbID, execAcct, clientAcct int64) (GiveUpTrade, error) {
	var g GiveUpTrade
	err := s.pool.QueryRow(ctx,
		`INSERT INTO pb_giveup_trades
		   (trade_id, prime_broker_id, executing_broker_account_id, client_account_id)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (trade_id, prime_broker_id) DO NOTHING
		 RETURNING `+giveupColumns, tradeID, pbID, execAcct, clientAcct).
		Scan(&g.ID, &g.TradeID, &g.PrimeBrokerID, &g.ExecutingBrokerAcctID,
			&g.ClientAccountID, &g.Status, &g.TraianaMessageID, &g.RejectionReason,
			&g.AffirmedAt, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return s.GiveUpByTradeAndPB(ctx, tradeID, pbID)
	}
	return g, err
}

// GiveUpByTradeAndPB loads the unique (trade, pb) give-up row.
func (s *PBStore) GiveUpByTradeAndPB(ctx context.Context, tradeID, pbID int64) (GiveUpTrade, error) {
	var g GiveUpTrade
	err := s.pool.QueryRow(ctx,
		`SELECT `+giveupColumns+` FROM pb_giveup_trades
		 WHERE trade_id=$1 AND prime_broker_id=$2`, tradeID, pbID).
		Scan(&g.ID, &g.TradeID, &g.PrimeBrokerID, &g.ExecutingBrokerAcctID,
			&g.ClientAccountID, &g.Status, &g.TraianaMessageID, &g.RejectionReason,
			&g.AffirmedAt, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

// GiveUpByID loads a give-up row by primary key.
func (s *PBStore) GiveUpByID(ctx context.Context, id int64) (GiveUpTrade, error) {
	var g GiveUpTrade
	err := s.pool.QueryRow(ctx,
		`SELECT `+giveupColumns+` FROM pb_giveup_trades WHERE id=$1`, id).
		Scan(&g.ID, &g.TradeID, &g.PrimeBrokerID, &g.ExecutingBrokerAcctID,
			&g.ClientAccountID, &g.Status, &g.TraianaMessageID, &g.RejectionReason,
			&g.AffirmedAt, &g.CreatedAt, &g.UpdatedAt)
	return g, err
}

// ErrGiveUpNotFound / ErrGiveUpIllegalTransition are the store's contract
// errors for the affirmation sync path.
var (
	ErrGiveUpNotFound          = errors.New("fix: give-up trade not found")
	ErrGiveUpIllegalTransition = errors.New("fix: illegal give-up status transition")
)

// UpdateGiveUpStatus applies a guarded status transition: the UPDATE only
// lands when the row is currently in a legal predecessor state, so a late
// affirmation can never resurrect a REJECTED trade or corrupt SETTLED.
// affirmed_at is stamped on the AFFIRMED transition.
func (s *PBStore) UpdateGiveUpStatus(ctx context.Context, id int64, to GiveUpStatus, reason string) error {
	cur, err := s.GiveUpByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrGiveUpNotFound
	}
	if err != nil {
		return err
	}
	if cur.Status == to {
		return nil // idempotent re-delivery
	}
	if !CanTransition(cur.Status, to) {
		return fmt.Errorf("%w: %s -> %s", ErrGiveUpIllegalTransition, cur.Status, to)
	}
	tag, err := s.pool.Exec(ctx,
		`UPDATE pb_giveup_trades
		    SET giveup_status=$2,
		        rejection_reason=CASE WHEN $2='REJECTED'::giveup_status_enum THEN $3 ELSE rejection_reason END,
		        affirmed_at=CASE WHEN $2='AFFIRMED'::giveup_status_enum THEN now() ELSE affirmed_at END,
		        updated_at=now()
		  WHERE id=$1 AND giveup_status=$4`, id, string(to), reason, string(cur.Status))
	if err != nil {
		return fmt.Errorf("fix: give-up status update: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Lost a transition race — re-read and report truthfully.
		return fmt.Errorf("%w: %s -> %s (concurrent move)", ErrGiveUpIllegalTransition, cur.Status, to)
	}
	return nil
}

// AttachTraianaRef stores the external affirmation message id returned by the
// Traiana/MarkitSERV submission so inbound responses can correlate.
func (s *PBStore) AttachTraianaRef(ctx context.Context, id int64, msgID string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE pb_giveup_trades SET traiana_message_id=$2, updated_at=now()
		  WHERE id=$1`, id, msgID)
	return err
}

// UpdateStatusByTraianaID applies a transition keyed on the external
// affirmation reference (the two-way sync entry point).
func (s *PBStore) UpdateStatusByTraianaID(ctx context.Context, msgID string, to GiveUpStatus, reason string) error {
	var id int64
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM pb_giveup_trades WHERE traiana_message_id=$1`, msgID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrGiveUpNotFound
	}
	if err != nil {
		return err
	}
	return s.UpdateGiveUpStatus(ctx, id, to, reason)
}

// PendingOlderThan lists PENDING give-ups created before cutoff — the
// affirmation-timeout sweep input (default window 60s per Task 18.3.6 step 5).
func (s *PBStore) PendingOlderThan(ctx context.Context, cutoff time.Time) ([]GiveUpTrade, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+giveupColumns+` FROM pb_giveup_trades
		 WHERE giveup_status='PENDING' AND created_at < $1
		 ORDER BY created_at`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GiveUpTrade
	for rows.Next() {
		var g GiveUpTrade
		if err := rows.Scan(&g.ID, &g.TradeID, &g.PrimeBrokerID, &g.ExecutingBrokerAcctID,
			&g.ClientAccountID, &g.Status, &g.TraianaMessageID, &g.RejectionReason,
			&g.AffirmedAt, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// PBDropCopy — ReportBus tap routing fills to PB sessions.
// ---------------------------------------------------------------------------

// ExecReportEvent is the parsed view of one emitted ExecutionReport for the
// PB path. Session-core's ReportEvent carries no account/trade ids — they
// are resolved here (OrderAccount / TradeOfOrder lookups) before routing.
type ExecReportEvent struct {
	OriginSessionID string             // emitting session's canonical key
	SessionID       quickfix.SessionID // emitting session
	OrderID         int64
	AccountID       int64 // owning account (resolved)
	TradeID         int64 // engine trade id (resolved; 0 = unresolvable)
	Msg             *quickfix.Message
}

// IsFill reports whether the message is a fill report (ExecType 1/2/F or
// a positive LastQty) — give-up rows are recorded on fills only.
func (e ExecReportEvent) IsFill() bool {
	if e.Msg == nil {
		return false
	}
	if et, err := e.Msg.Body.GetString(TagExecType); err == nil {
		switch et {
		case ExecTypeTrade, ExecTypeFill, ExecTypePartialFill:
			return true
		case ExecTypeNew, ExecTypeCanceled, ExecTypeRejected:
			return false
		}
	}
	if lq, err := e.Msg.Body.GetString(TagLastQty); err == nil && lq != "" && lq != "0" {
		return true
	}
	return false
}

// PBSessionLookup resolves a live acceptor-side session for a PB's
// fix_comp_id (the TargetCompID on our acceptor sessions).
type PBSessionLookup interface {
	SessionByTargetCompID(compID string) (quickfix.SessionID, bool)
}

// AppLiveSessions adapts the session-core App.ActiveSessions registry —
// the production PB session binding.
type AppLiveSessions struct{ App *App }

// SessionByTargetCompID implements PBSessionLookup.
func (a AppLiveSessions) SessionByTargetCompID(compID string) (quickfix.SessionID, bool) {
	for _, id := range a.App.ActiveSessions() {
		if id.TargetCompID == compID {
			return id, true
		}
	}
	return quickfix.SessionID{}, false
}

// AffirmationSubmitter exports a give-up trade to an external affirmation
// venue (Traiana Harmony / MarkitSERV — see affirmation.go).
type AffirmationSubmitter interface {
	Submit(ctx context.Context, trade GiveUpTrade, pb PrimeBroker) (externalRef string, err error)
}

// PBDropCopy routes execution reports for give-up-eligible accounts to the
// client's prime broker FIX session in real time and records the give-up.
// It attaches to the session-core emitter as a ReportBus tap:
//
//	app.Report().WithTap(pb.Tap())
type PBDropCopy struct {
	store    *PBStore
	sessions PBSessionLookup
	sender   MessageSender
	submit   []AffirmationSubmitter
	// OrderAccount resolves the owning account of a report's order;
	// TradeOfOrder resolves the engine trade id for give-up persistence.
	// Both bind to the order read-model in production; when nil the copy
	// still flows but the give-up row is skipped (loudly, via OnError).
	OrderAccount func(ctx context.Context, orderID int64) (int64, error)
	TradeOfOrder func(ctx context.Context, orderID int64) (int64, error)
	// OnError receives non-fatal routing failures (PB session down,
	// affirmation submit failure). The give-up row is already durably
	// PENDING at that point so the affirmation-timeout monitor still
	// catches the break — the copy itself must never drop silently.
	OnError func(err error)
}

// NewPBDropCopy wires the sink. sessions may be nil in tests — unresolved PB
// sessions then surface through OnError.
func NewPBDropCopy(store *PBStore, sessions PBSessionLookup, submitters ...AffirmationSubmitter) *PBDropCopy {
	return &PBDropCopy{store: store, sessions: sessions, sender: QuickFIXSender(), submit: submitters}
}

// SetSender overrides the outbound binding (tests).
func (p *PBDropCopy) SetSender(s MessageSender) {
	if s != nil {
		p.sender = s
	}
}

// Tap adapts the sink to the session-core ReportBus tap signature.
func (p *PBDropCopy) Tap() ReportTap {
	return func(ev ReportEvent) {
		ctx, cancel := context.WithTimeout(context.Background(), ReportSendTimeout)
		defer cancel()
		if err := p.OnReportEvent(ctx, ev); err != nil && p.OnError != nil {
			p.OnError(err)
		}
	}
}

// OnReportEvent resolves the ReportEvent into an ExecReportEvent and routes.
func (p *PBDropCopy) OnReportEvent(ctx context.Context, ev ReportEvent) error {
	if ev.Msg == nil {
		return nil
	}
	eev := ExecReportEvent{
		OriginSessionID: ev.SessionID.String(),
		SessionID:       ev.SessionID,
		OrderID:         ev.OrderID,
		Msg:             ev.Msg,
	}
	if p.OrderAccount != nil && ev.OrderID != 0 {
		if id, err := p.OrderAccount(ctx, ev.OrderID); err == nil {
			eev.AccountID = id
		}
	}
	if eev.AccountID == 0 {
		// No account → cannot determine eligibility; skip (NOT an error —
		// the report may simply not be give-up relevant).
		return nil
	}
	if eev.IsFill() && p.TradeOfOrder != nil && ev.OrderID != 0 {
		if tid, err := p.TradeOfOrder(ctx, ev.OrderID); err == nil {
			eev.TradeID = tid
		}
	}
	return p.OnExecutionReport(ctx, eev)
}

// OnExecutionReport routes one report (Task 18.3.6 step 1): fills on
// give-up-eligible accounts are persisted + copied to each PB's FIX session
// with the §9.2 Parties group; non-fill reports are still copied to
// eligible PBs (lifecycle visibility) but record no give-up row.
func (p *PBDropCopy) OnExecutionReport(ctx context.Context, ev ExecReportEvent) error {
	pbs, err := p.store.EligiblePrimeBrokers(ctx, ev.AccountID)
	if err != nil {
		return fmt.Errorf("fix: pb drop copy eligibility: %w", err)
	}
	if len(pbs) == 0 {
		return nil
	}
	var errs []error
	reportErr := func(e error) {
		errs = append(errs, e)
		if p.OnError != nil {
			p.OnError(e)
		}
	}
	for _, pb := range pbs {
		var trade GiveUpTrade
		if ev.IsFill() {
			if ev.TradeID == 0 {
				reportErr(fmt.Errorf("fix: give-up fill without resolvable trade id (order %d)", ev.OrderID))
			} else {
				trade, err = p.store.RecordGiveUp(ctx, ev.TradeID, pb.ID,
					pbAccountID(ev, pb), ev.AccountID)
				if err != nil {
					reportErr(fmt.Errorf("fix: record give-up trade %d pb %d: %w", ev.TradeID, pb.ID, err))
					continue
				}
			}
		}
		if p.sessions != nil {
			if sid, ok := p.sessions.SessionByTargetCompID(pb.FixCompID); ok {
				cp := CloneMessage(ev.Msg)
				SetParties(cp, pbParties(ev, pb))
				if err := p.sender.SendTo(cp, sid); err != nil {
					reportErr(fmt.Errorf("fix: pb drop copy %s: %w", pb.FixCompID, err))
				}
			} else {
				reportErr(fmt.Errorf("fix: pb session %s not live for give-up %d", pb.FixCompID, trade.ID))
			}
		}
		if trade.ID > 0 {
			for _, sub := range p.submit {
				ref, err := sub.Submit(ctx, trade, pb)
				if err != nil {
					reportErr(fmt.Errorf("fix: affirmation submit give-up %d: %w", trade.ID, err))
					continue
				}
				if ref != "" {
					if err := p.store.AttachTraianaRef(ctx, trade.ID, ref); err != nil {
						reportErr(fmt.Errorf("fix: attach traiana ref give-up %d: %w", trade.ID, err))
					}
				}
			}
		}
	}
	return errors.Join(errs...)
}

// pbAccountID resolves the executing-broker account side of the give-up. The
// venue executing-broker account is the counterparty side of the fill; at the
// FIX layer the binding is session-level, so we carry the origin account and
// let Phase-24 reconciliation map the house side — seam documented for §27.
func pbAccountID(ev ExecReportEvent, _ PrimeBroker) int64 {
	return ev.AccountID
}

// OnAllocationChange implements AllocationChangeSink — PB drop copy sessions
// receive the 35=AK allocation report whenever a give-up-eligible master
// account's instruction commits or corrects (Task 18.3.13 step 5 / DoD row 6).
func (p *PBDropCopy) OnAllocationChange(ctx context.Context, ev AllocationChange) error {
	if ev.Report == nil || p.sessions == nil {
		return nil
	}
	pbs, err := p.store.EligiblePrimeBrokers(ctx, ev.Allocation.MasterAccountID)
	if err != nil {
		return fmt.Errorf("fix: pb alloc report eligibility: %w", err)
	}
	var errs []error
	for _, pb := range pbs {
		sid, ok := p.sessions.SessionByTargetCompID(pb.FixCompID)
		if !ok {
			err := fmt.Errorf("fix: pb session %s not live for alloc %s", pb.FixCompID, ev.Allocation.AllocID)
			errs = append(errs, err)
			if p.OnError != nil {
				p.OnError(err)
			}
			continue
		}
		cp := CloneMessage(ev.Report)
		SetParties(cp, pbParties(ExecReportEvent{OriginSessionID: ev.Allocation.SessionID, AccountID: ev.Allocation.MasterAccountID}, pb))
		if err := p.sender.SendTo(cp, sid); err != nil {
			errs = append(errs, fmt.Errorf("fix: pb alloc report %s: %w", pb.FixCompID, err))
		}
	}
	return errors.Join(errs...)
}

// pbParties builds the §9.2 Parties group for a PB-bound execution report:
// Executing Firm (452=1), Client ID (452=3), Prime Broker (452=36, BIC).
func pbParties(ev ExecReportEvent, pb PrimeBroker) []Party {
	return []Party{
		{ID: ev.OriginSessionID, Source: PartyIDSourceProprietary, Role: PartyRoleExecutingFirm},
		{ID: fmt.Sprintf("%d", ev.AccountID), Source: PartyIDSourceProprietary, Role: PartyRoleClientID},
		{ID: pb.BIC, Source: PartyIDSourceBIC, Role: PartyRolePrimeBroker},
	}
}
