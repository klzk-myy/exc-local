// shortfall.go — Phase-24 Task 24.3.16: CLS match discrepancy quarantine
// and the client-money shortfall top-up protocol (spec §2.7, §17.12,
// §24 #326).
//
//   - QuarantineBatch: a CLS match report that disagrees with the internal
//     settlement batch (amounts, currency, settlement-date drift) parks the
//     batch under reason CLS_SETTLEMENT_MISMATCH; AssertOutboundAllowed
//     blocks outbound funds for quarantined batches until a dual-control
//     affirmation releases them.
//   - EvaluateSegregation: the real-time segregated-vs-entitlement check.
//     A deficit raises CLIENT_MONEY_SHORTFALL (HTTP 503 per §23 — the
//     task text's 422 was superseded by remediation #35) and drives the
//     Task 24.3.11 4-tier waterfall — Tier-1 insurance-fund debit
//     automatic/immediate, Tier-2 house top-up inside its 30-minute SLA —
//     with the 1-hour top-up obligation and 60-minute CCO escalation +
//     Tier-3 regulatory notifications delivered by SweepDeadlines.
package backoffice

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/admin"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Quarantine statuses (settlement_quarantines.status, migration 056).
const (
	QuarantineActive   = "QUARANTINED"
	QuarantineReleased = "RELEASED"
)

// Quarantine is one settlement_quarantines row — a settlement batch whose
// CLS match report conflicts with internal records; outbound funds stay
// blocked until dual-control affirmation releases it.
type Quarantine struct {
	ID                int64           `json:"id"`
	BatchRef          string          `json:"batch_ref"`
	ReasonCode        string          `json:"reason_code"`
	Discrepancy       json.RawMessage `json:"discrepancy"`
	OutboundBlocked   bool            `json:"outbound_blocked"`
	Status            string          `json:"status"`
	QuarantinedBy     int64           `json:"quarantined_by"`
	ReleasedBy        *int64          `json:"released_by,omitempty"`
	ReleaseApprovedBy *int64          `json:"release_approved_by,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	ReleasedAt        *time.Time      `json:"released_at,omitempty"`
}

// SegregationStatus is the result of a real-time segregated-vs-entitlement
// evaluation (Task 24.3.16 item 2).
type SegregationStatus struct {
	Currency    string          `json:"currency"`
	Entitlement decimal.Decimal `json:"entitlement"`
	Segregated  decimal.Decimal `json:"segregated"`
	Deficit     decimal.Decimal `json:"deficit"`
	Shortfall   bool            `json:"shortfall"`
	BreakID     int64           `json:"break_id,omitempty"`
	EvaluatedAt time.Time       `json:"evaluated_at"`
}

// QuarantineBatch parks a settlement batch whose CLS match report shows a
// discrepancy. Idempotent per batch_ref — an already-quarantined batch
// returns the live row (repeat match-report deliveries dedup).
func (s *ClientMoneyService) QuarantineBatch(ctx context.Context, actor admin.AdminActor,
	batchRef string, discrepancy json.RawMessage) (*Quarantine, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if batchRef == "" {
		return nil, excerrors.New("INVALID_REQUEST", "batch_ref required")
	}
	if len(discrepancy) == 0 {
		discrepancy = json.RawMessage(`{}`)
	} else if !json.Valid(discrepancy) {
		return nil, excerrors.New("INVALID_REQUEST", "discrepancy must be valid JSON")
	}
	var out *Quarantine
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		if existing, err := tx.QuarantineByBatch(ctx, batchRef); err != nil {
			return err
		} else if existing != nil && existing.Status == QuarantineActive {
			out = existing
			return nil
		}
		q, err := tx.InsertQuarantine(ctx, Quarantine{
			BatchRef: batchRef, ReasonCode: CodeCLSSettlementMismatch,
			Discrepancy: discrepancy, OutboundBlocked: true,
			Status: QuarantineActive, QuarantinedBy: actor.UserID,
		})
		if err != nil {
			return err
		}
		out = q
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.raise(ctx, SeverityP1, alertCLSQuarantine,
		fmt.Sprintf("settlement batch %s quarantined — CLS match discrepancy; outbound funds blocked", batchRef),
		map[string]string{"batch_ref": batchRef})
	return out, nil
}

// AssertOutboundAllowed gates outbound fund release for a settlement
// batch: an active quarantine refuses with CLS_SETTLEMENT_MISMATCH
// (HTTP 409 — spec §17.12.3).
func (s *ClientMoneyService) AssertOutboundAllowed(ctx context.Context, batchRef string) error {
	q, err := s.store.QuarantineByBatch(ctx, batchRef)
	if err != nil {
		return err
	}
	if q != nil && q.Status == QuarantineActive && q.OutboundBlocked {
		return excerrors.New(CodeCLSSettlementMismatch, fmt.Sprintf(
			"settlement batch %s is quarantined pending operational reconciliation — outbound funds blocked",
			batchRef))
	}
	return nil
}

// AffirmQuarantineRelease lifts the outbound block after operational
// reconciliation — dual-control: the affirming principal (actor) and the
// approver (actor.ApproverID) must be distinct, role-eligible admins.
func (s *ClientMoneyService) AffirmQuarantineRelease(ctx context.Context, actor admin.AdminActor,
	quarantineID int64) (*Quarantine, error) {
	if err := s.requireRole(ctx, actor.UserID, financeWriteRoles); err != nil {
		return nil, err
	}
	if err := s.requireDualControl(ctx, actor, complianceOrFinance); err != nil {
		return nil, err
	}
	var out *Quarantine
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		q, err := tx.QuarantineByID(ctx, quarantineID)
		if err != nil {
			return err
		}
		if q == nil {
			return excerrors.New("NOT_FOUND", "quarantine not found")
		}
		if q.Status != QuarantineActive {
			return excerrors.New("INVALID_LIFECYCLE_TRANSITION",
				fmt.Sprintf("quarantine %d already %s", quarantineID, q.Status))
		}
		now := s.now()
		q.Status = QuarantineReleased
		q.OutboundBlocked = false
		q.ReleasedBy = &actor.UserID
		q.ReleaseApprovedBy = &actor.ApproverID
		q.ReleasedAt = &now
		if err := tx.UpdateQuarantine(ctx, *q); err != nil {
			return err
		}
		out = q
		return nil
	})
	return out, err
}

// EvaluateSegregation is the real-time segregated-vs-entitlement check
// (Task 24.3.16 item 2): when segregated resource drops below client
// entitlement it opens (or joins) a SHORTFALL break, runs the waterfall —
// Tier-1 insurance debit immediate, Tier-2 house top-up inside its
// 30-minute SLA — and returns the status. Withdrawal paths pair this with
// AssertClientMoneyMovement to reject worsening movements as
// CLIENT_MONEY_SHORTFALL 503.
func (s *ClientMoneyService) EvaluateSegregation(ctx context.Context, ccy string) (*SegregationStatus, error) {
	if len(ccy) != 3 {
		return nil, excerrors.New("INVALID_REQUEST", "currency must be 3-letter ISO")
	}
	st := &SegregationStatus{Currency: ccy, EvaluatedAt: s.now()}
	err := s.store.InTx(ctx, func(ctx context.Context, tx Tx) error {
		ent, err := tx.ClientEntitlements(ctx, ccy)
		if err != nil {
			return err
		}
		requirement := decimal.Zero
		for _, e := range ent {
			requirement = requirement.Add(e.Amount)
		}
		unid, _, err := tx.UnidentifiedReceipts(ctx, ccy)
		if err != nil {
			return err
		}
		requirement = requirement.Add(unid)
		clientBal, err := tx.ClassifiedBalance(ctx, ccy, ClassClient)
		if err != nil {
			return err
		}
		marginBal, err := tx.ClassifiedBalance(ctx, ccy, ClassMargin)
		if err != nil {
			return err
		}
		st.Entitlement = requirement
		st.Segregated = clientBal.Add(marginBal)
		st.Shortfall = st.Segregated.LessThan(st.Entitlement)
		if !st.Shortfall {
			return nil
		}
		st.Deficit = st.Entitlement.Sub(st.Segregated)
		// Join an existing open break for this currency rather than
		// minting duplicates on every evaluation.
		open, err := tx.OpenBreaks(ctx, BreakShortfall, ccy)
		if err != nil {
			return err
		}
		for _, b := range open {
			st.BreakID = b.ID
			return nil // waterfall already running; sweep owns escalation
		}
		brk, err := tx.InsertBreak(ctx, Break{
			Kind: BreakShortfall, Currency: ccy, Amount: st.Deficit,
			Status: BreakOpen, Severity: "P1", DetectedAt: s.now(),
			Detail: json.RawMessage(fmt.Sprintf(
				`{"source":"realtime_segregation","entitlement":%q,"segregated":%q}`,
				st.Entitlement.String(), st.Segregated.String())),
		})
		if err != nil {
			return err
		}
		st.BreakID = brk.ID
		return s.runWaterfall(ctx, tx, *brk)
	})
	if err != nil {
		return nil, err
	}
	if st.Shortfall {
		s.raise(ctx, SeverityP1, alertClientMoneyShortfall,
			fmt.Sprintf("real-time segregation deficit %s %s", st.Deficit.String(), ccy),
			map[string]string{"currency": ccy, "break_id": fmt.Sprint(st.BreakID)})
	}
	return st, nil
}

// --- PgStore: settlement_quarantines ----------------------------------------

const quarantineCols = `id, batch_ref, reason_code, discrepancy, outbound_blocked,
	status, quarantined_by, released_by, release_approved_by, created_at, released_at`

func scanQuarantine(row pgx.Row) (*Quarantine, error) {
	var q Quarantine
	if err := row.Scan(&q.ID, &q.BatchRef, &q.ReasonCode, &q.Discrepancy,
		&q.OutboundBlocked, &q.Status, &q.QuarantinedBy, &q.ReleasedBy,
		&q.ReleaseApprovedBy, &q.CreatedAt, &q.ReleasedAt); err != nil {
		return nil, err
	}
	return &q, nil
}

func (s *PgStore) InsertQuarantine(ctx context.Context, q Quarantine) (*Quarantine, error) {
	return scanQuarantine(s.q.QueryRow(ctx, `
		INSERT INTO settlement_quarantines
		 (batch_ref, reason_code, discrepancy, outbound_blocked, status, quarantined_by)
		 VALUES ($1,$2,$3,$4,$5,$6) RETURNING `+quarantineCols,
		q.BatchRef, q.ReasonCode, q.Discrepancy, q.OutboundBlocked, q.Status,
		q.QuarantinedBy))
}

func (s *PgStore) QuarantineByID(ctx context.Context, id int64) (*Quarantine, error) {
	q, err := scanQuarantine(s.q.QueryRow(ctx,
		`SELECT `+quarantineCols+` FROM settlement_quarantines WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return q, err
}

func (s *PgStore) QuarantineByBatch(ctx context.Context, batchRef string) (*Quarantine, error) {
	q, err := scanQuarantine(s.q.QueryRow(ctx,
		`SELECT `+quarantineCols+` FROM settlement_quarantines
		  WHERE batch_ref=$1 ORDER BY id DESC LIMIT 1`, batchRef))
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return q, err
}

func (s *PgStore) UpdateQuarantine(ctx context.Context, q Quarantine) error {
	_, err := s.q.Exec(ctx, `
		UPDATE settlement_quarantines
		   SET status=$2, outbound_blocked=$3, released_by=$4,
		       release_approved_by=$5, released_at=$6
		 WHERE id=$1`,
		q.ID, q.Status, q.OutboundBlocked, q.ReleasedBy, q.ReleaseApprovedBy,
		q.ReleasedAt)
	return err
}

func (s *PgStore) ActiveQuarantines(ctx context.Context) ([]Quarantine, error) {
	rows, err := s.q.Query(ctx,
		`SELECT `+quarantineCols+` FROM settlement_quarantines
		  WHERE status='QUARANTINED' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Quarantine{}
	for rows.Next() {
		var q Quarantine
		if err := rows.Scan(&q.ID, &q.BatchRef, &q.ReasonCode, &q.Discrepancy,
			&q.OutboundBlocked, &q.Status, &q.QuarantinedBy, &q.ReleasedBy,
			&q.ReleaseApprovedBy, &q.CreatedAt, &q.ReleasedAt); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}
