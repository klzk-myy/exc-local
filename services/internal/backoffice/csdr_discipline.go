// Task 24.3.13 — CSDR settlement discipline (spec §17.6 extension, §24 #199),
// as amended by Task 24.3.19 / spec §17.14 item 5.
//
// REGIME NOTE (supersession): CSDR Art. 7 cash penalties + mandatory buy-in
// govern CSD-settled securities. For FX settlement legs the regime flag is
// FX_CLOSEOUT — replacement-cost close-out + fail interest (policy rate +
// 100bps from ISD+1) per §17.14/ISDA-FX-Global-Code, computed by
// ops_hardening.go (fx_fail_closeouts). The CSDR penalty/buy-in machinery in
// this file + buyin.go is retained for the securities-venue scope
// (settlement_fails.regime='CSDR'); FX legs are detected by the SAME ISD+1
// fail scan but never accrue CSDR penalties and never reach buy-in.
//
// Canonical values: detect at ISD+1 · 1bp/day LIQUID · 0.5bp/day ILLIQUID ·
// bilateral PAYABLE+RECEIVABLE accruals · buy-in notify ISD+4 / execute ISD+7
// (CSDR regime only) · CLS-settled trades exempt from bilateral penalties.

package backoffice

import (
	"context"
	stderrors "errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// FailRegime mirrors settlement_fail_regime_enum (migration 084).
type FailRegime string

const (
	// RegimeCSDR — securities/CSD scope: Art. 7 penalties + buy-in apply.
	RegimeCSDR FailRegime = "CSDR"
	// RegimeFXCloseout — FX settlement legs: §17.14 close-out + fail
	// interest supersede CSDR economics (Task 24.3.19 item 5).
	RegimeFXCloseout FailRegime = "FX_CLOSEOUT"
)

// LiquidityClass mirrors liquidity_class_enum.
type LiquidityClass string

const (
	LiquidityLiquid   LiquidityClass = "LIQUID"
	LiquidityIlliquid LiquidityClass = "ILLIQUID"
)

// CSDR Art. 7 daily penalty rates (basis points of the unsettled value).
const (
	CSDRLiquidRateBP   = "1.0000"
	CSDRIlliquidRateBP = "0.5000"
)

// FailStatus mirrors settlement_fail_status_enum.
type FailStatus string

const (
	FailOpen          FailStatus = "OPEN"
	FailClosedOut     FailStatus = "CLOSED_OUT"
	FailBuyInNotified FailStatus = "BUYIN_NOTIFIED"
	FailBoughtIn      FailStatus = "BOUGHT_IN"
	FailResolved      FailStatus = "RESOLVED"
)

// SettlementFail is one settlement_fails row.
type SettlementFail struct {
	ID              int64
	InstructionID   int64
	TradeID         int64
	AccountID       int64
	Currency        string
	Amount          decimal.Decimal
	ISD             time.Time // intended settlement date
	DetectedAt      time.Time
	Regime          FailRegime
	Liquidity       LiquidityClass
	CLSSettled      bool // exempt from bilateral CSDR penalties
	Status          FailStatus
	BuyInNotifiedAt *time.Time
	ClosedOutAt     *time.Time
	ResolvedAt      *time.Time
	CreatedAt       time.Time
}

// SettlementPenalty is one settlement_penalties accrual row.
type SettlementPenalty struct {
	ID            int64
	FailID        int64
	AccrualDate   time.Time
	Regime        FailRegime
	RateBP        decimal.Decimal
	BaseAmount    decimal.Decimal
	PenaltyAmount decimal.Decimal
	Currency      string
	Direction     string // PAYABLE | RECEIVABLE
	Counterparty  *int64
	Status        string
}

// LegClassification decides which settlement-discipline regime applies to
// an instruction — the fail-detection pipeline is common; only economics
// differ per regime.
type LegClassification struct {
	Regime     FailRegime
	Liquidity  LiquidityClass
	CLSSettled bool
}

// LegClassifier is the instrument/CLS lookup seam — the production impl
// joins instruments + cls_settlement_instructions (migration 259); FX
// instrument types (SPOT/FORWARD/SWAP/NDF/OPTION — instrument_type_enum)
// classify FX_CLOSEOUT. Nil → every leg classifies FX_CLOSEOUT, which is
// the safe default for this FX-only venue (CSDR machinery then simply
// never fires).
type LegClassifier interface {
	Classify(ctx context.Context, leg ExceptionLeg) (LegClassification, error)
}

// FailStore is the persistence seam for fail detection + penalty accrual.
type FailStore interface {
	InTx(ctx context.Context, fn func(ctx context.Context, tx FailTx) error) error
	// FailByID loads one fail row; found=false when absent.
	FailByID(ctx context.Context, id int64) (SettlementFail, bool, error)
	// OpenFails lists fails still needing processing; regime nil = all.
	OpenFails(ctx context.Context, regime *FailRegime) ([]SettlementFail, error)
	// FailsInRange lists every fail row (any status) detected before `to`
	// — the cumulative view the daily/monthly reports bucket by status;
	// OpenFails deliberately excludes terminal rows the report must count.
	FailsInRange(ctx context.Context, from, to time.Time) ([]SettlementFail, error)
	// PenaltiesInRange returns accruals for the reporting window.
	PenaltiesInRange(ctx context.Context, from, to time.Time) ([]SettlementPenalty, error)
}

// FailTx is the transactional fail-detection/accrual view.
type FailTx interface {
	// LateInstructions lists PENDING/FAILED legs whose settlement_date is
	// strictly before `day` (i.e. ISD+1 has passed) and fail_flag IS NULL.
	LateInstructions(ctx context.Context, day time.Time) ([]ExceptionLeg, error)
	// FlagInstruction stamps fail_flag='SETTLEMENT_FAIL' on the leg.
	FlagInstruction(ctx context.Context, instructionID int64) error
	// InsertFail persists the fail; UNIQUE(settlement_instruction_id)
	// makes daily rescans idempotent — created=false on replay.
	InsertFail(ctx context.Context, f SettlementFail) (SettlementFail, bool, error)
	// SetFailStatus transitions a fail (resolution/close-out/buy-in).
	SetFailStatus(ctx context.Context, id int64, st FailStatus, at time.Time) error
	// SettledInstruction reports whether the leg settled (auto-resolve).
	InstructionSettled(ctx context.Context, instructionID int64) (bool, error)
	// AccruePenalty inserts one accrual; UNIQUE(fail_id, date, direction)
	// makes the daily job replay-safe — inserted=false on replay.
	AccruePenalty(ctx context.Context, p SettlementPenalty) (bool, error)
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// CSDRService owns the ISD+1 fail scan, the Art. 7 penalty accrual and the
// NCA/CSD reporting views. FX legs detected by the scan carry
// regime=FX_CLOSEOUT and are handed to the §17.14 close-out engine
// (ops_hardening.go) — never to penalty accrual or buy-in.
type CSDRService struct {
	store      FailStore
	classifier LegClassifier
	now        func() time.Time
}

// NewCSDRService wires the service; classifier may be nil (all-FX venue).
func NewCSDRService(store FailStore, classifier LegClassifier) *CSDRService {
	return &CSDRService{store: store, classifier: classifier,
		now: func() time.Time { return time.Now().UTC() }}
}

// SetClockForTest overrides the clock; tests only.
func (s *CSDRService) SetClockForTest(now func() time.Time) { s.now = now }

func (s *CSDRService) classify(ctx context.Context, leg ExceptionLeg) (LegClassification, error) {
	if s.classifier == nil {
		// FX-only venue default: every leg is FX_CLOSEOUT scope.
		return LegClassification{Regime: RegimeFXCloseout, Liquidity: LiquidityLiquid}, nil
	}
	c, err := s.classifier.Classify(ctx, leg)
	if err != nil {
		return c, err
	}
	if c.Regime == "" {
		c.Regime = RegimeFXCloseout
	}
	if c.Liquidity == "" {
		c.Liquidity = LiquidityLiquid
	}
	return c, nil
}

// DetectFails runs the ISD+1 scan for `day`: every unsettled leg with
// settlement_date < day is flagged SETTLEMENT_FAIL and a settlement_fails
// row is recorded (idempotent — rescans attach to the existing row).
// Returns the fails recorded this pass (newly inserted only).
func (s *CSDRService) DetectFails(ctx context.Context, day time.Time) ([]SettlementFail, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "fail store not configured")
	}
	var out []SettlementFail
	err := s.store.InTx(ctx, func(ctx context.Context, tx FailTx) error {
		legs, err := tx.LateInstructions(ctx, day)
		if err != nil {
			return fmt.Errorf("scan late instructions: %w", err)
		}
		for _, leg := range legs {
			cls, err := s.classify(ctx, leg)
			if err != nil {
				return fmt.Errorf("classify instruction %d: %w", leg.ID, err)
			}
			if err := tx.FlagInstruction(ctx, leg.ID); err != nil {
				return fmt.Errorf("flag instruction %d: %w", leg.ID, err)
			}
			f, created, err := tx.InsertFail(ctx, SettlementFail{
				InstructionID: leg.ID,
				TradeID:       leg.TradeID,
				AccountID:     leg.AccountID,
				Currency:      leg.Currency,
				Amount:        leg.Amount,
				ISD:           leg.SettlementDate,
				DetectedAt:    s.now(),
				Regime:        cls.Regime,
				Liquidity:     cls.Liquidity,
				CLSSettled:    cls.CLSSettled,
				Status:        FailOpen,
			})
			if err != nil {
				return fmt.Errorf("record fail for instruction %d: %w", leg.ID, err)
			}
			if created {
				out = append(out, f)
			}
		}
		return nil
	})
	return out, err
}

// AccruePenalties runs the daily Art. 7 accrual for `day` over every OPEN
// CSDR-regime fail. Bilateral: each accrual writes a PAYABLE row (failing
// party owes) and a RECEIVABLE row (receiving party is owed). CLS-settled
// fails and FX_CLOSEOUT fails are exempt — the §17.14 engine handles FX.
func (s *CSDRService) AccruePenalties(ctx context.Context, day time.Time) (int, error) {
	if s.store == nil {
		return 0, excerrors.New(CodeServiceDegraded, "fail store not configured")
	}
	csdr := RegimeCSDR
	accrualDay := day.Truncate(24 * time.Hour)
	var accrued int
	err := s.store.InTx(ctx, func(ctx context.Context, tx FailTx) error {
		// Auto-resolve legs that settled since the last pass — accrual
		// stops on the resolved fail.
		fails, err := openFailsView(ctx, tx, csdr)
		if err != nil {
			return err
		}
		for _, f := range fails {
			if f.CLSSettled {
				continue // §24 task exemption — CLS handles internally
			}
			settled, err := tx.InstructionSettled(ctx, f.InstructionID)
			if err != nil {
				return fmt.Errorf("check settled instruction %d: %w", f.InstructionID, err)
			}
			if settled {
				if err := tx.SetFailStatus(ctx, f.ID, FailResolved, s.now()); err != nil {
					return fmt.Errorf("resolve fail %d: %w", f.ID, err)
				}
				continue
			}
			rate := decimal.RequireFromString(CSDRLiquidRateBP)
			if f.Liquidity == LiquidityIlliquid {
				rate = decimal.RequireFromString(CSDRIlliquidRateBP)
			}
			// penalty = unsettled value × rate_bp / 10_000
			pen := f.Amount.Mul(rate).Div(decimal.NewFromInt(10000))
			for _, dir := range []string{"PAYABLE", "RECEIVABLE"} {
				cp := f.AccountID
				p := SettlementPenalty{
					FailID: f.ID, AccrualDate: accrualDay, Regime: RegimeCSDR,
					RateBP: rate, BaseAmount: f.Amount, PenaltyAmount: pen,
					Currency: f.Currency, Direction: dir, Counterparty: &cp,
					Status: "ACCRUED",
				}
				if _, err := tx.AccruePenalty(ctx, p); err != nil {
					return fmt.Errorf("accrue penalty fail %d %s: %w", f.ID, dir, err)
				}
			}
			accrued++
		}
		return nil
	})
	return accrued, err
}

// openFailsView lists CSDR fails via the store inside the accrual tx —
// OpenFails is pool-scoped; the tx view keeps the read serializable.
func openFailsView(ctx context.Context, tx FailTx, regime FailRegime) ([]SettlementFail, error) {
	if v, ok := tx.(failLister); ok {
		return v.openFailsTx(ctx, regime)
	}
	return nil, excerrors.New(CodeServiceDegraded, "fail store lacks tx fail listing")
}

type failLister interface {
	openFailsTx(ctx context.Context, regime FailRegime) ([]SettlementFail, error)
}

// FailReport is the NCA/CSD regulatory report view (daily + monthly share
// the same shape — monthly aggregates a date range).
type FailReport struct {
	From           time.Time
	To             time.Time
	FailsDetected  int               `json:"fails_detected"`
	FailsOpen      int               `json:"fails_open"`
	FailsResolved  int               `json:"fails_resolved"`
	FailValueByCcy map[string]string `json:"fail_value_by_ccy"` // decimal strings
	PenaltyByCcy   map[string]string `json:"penalty_by_ccy"`    // PAYABLE+RECEIVABLE summed
	PenaltyRows    int               `json:"penalty_rows"`
	MaxFailDays    int               `json:"max_fail_days"`
}

// DailyReport builds the §24 task's daily NCA/CSD settlement-fail report:
// fails detected in the window, open/resolved counts, fail values and
// penalty amounts per currency.
func (s *CSDRService) DailyReport(ctx context.Context, day time.Time) (*FailReport, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "fail store not configured")
	}
	from := day.Truncate(24 * time.Hour)
	return s.report(ctx, from, from.Add(24*time.Hour))
}

// MonthlyReport aggregates one calendar month for the NCA/CSD filing.
func (s *CSDRService) MonthlyReport(ctx context.Context, year int, month time.Month) (*FailReport, error) {
	if s.store == nil {
		return nil, excerrors.New(CodeServiceDegraded, "fail store not configured")
	}
	from := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	return s.report(ctx, from, from.AddDate(0, 1, 0))
}

func (s *CSDRService) report(ctx context.Context, from, to time.Time) (*FailReport, error) {
	rep := &FailReport{
		From:           from,
		To:             to,
		FailValueByCcy: map[string]string{},
		PenaltyByCcy:   map[string]string{},
	}
	// Full row set — resolved/bought-in fails still count toward the
	// window's resolved totals; OpenFails would hide them.
	fails, err := s.store.FailsInRange(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("list fails: %w", err)
	}
	now := s.now()
	for _, f := range fails {
		switch f.Status {
		case FailResolved, FailBoughtIn, FailClosedOut:
			if f.DetectedAt.Before(to) {
				rep.FailsResolved++
			}
		default:
			rep.FailsOpen++
			if d := int(now.Sub(f.DetectedAt).Hours() / 24); d > rep.MaxFailDays {
				rep.MaxFailDays = d
			}
		}
		if !f.DetectedAt.Before(from) && f.DetectedAt.Before(to) {
			rep.FailsDetected++
			cur, _ := decimal.NewFromString(rep.FailValueByCcy[f.Currency])
			rep.FailValueByCcy[f.Currency] = cur.Add(f.Amount).String()
		}
	}
	pens, err := s.store.PenaltiesInRange(ctx, from, to)
	if err != nil {
		return nil, fmt.Errorf("list penalties: %w", err)
	}
	for _, p := range pens {
		if p.Direction != "PAYABLE" {
			continue // bilateral pair — report the payable leg once
		}
		rep.PenaltyRows++
		cur, _ := decimal.NewFromString(rep.PenaltyByCcy[p.Currency])
		rep.PenaltyByCcy[p.Currency] = cur.Add(p.PenaltyAmount).String()
	}
	rep.PenaltyRows *= 2 // both bilateral legs were written
	return rep, nil
}

// ResolveFail marks a fail resolved (manual disposition — e.g. the leg
// settled off-book, or FX close-out completed via the §17.14 path which
// stamps CLOSED_OUT itself).
func (s *CSDRService) ResolveFail(ctx context.Context, failID int64) error {
	if s.store == nil {
		return excerrors.New(CodeServiceDegraded, "fail store not configured")
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx FailTx) error {
		return tx.SetFailStatus(ctx, failID, FailResolved, s.now())
	})
}

// ---------------------------------------------------------------------------
// PgxFailStore — production FailStore
// ---------------------------------------------------------------------------

// PgxFailStore implements FailStore (and the buy-in store pieces in
// buyin.go share the same pgx plumbing via BuyInStore).
type PgxFailStore struct{ Q Querier }

// NewPgxFailStore binds the store to a pool.
func NewPgxFailStore(pool *pgxpool.Pool) *PgxFailStore {
	return &PgxFailStore{Q: pool}
}

const failCols = `id, settlement_instruction_id, trade_id, account_id, currency,
	amount::text, isd, detected_at, regime::text, liquidity_class::text,
	cls_settled, status::text, buyin_notified_at, closed_out_at, resolved_at, created_at`

func scanFail(row interface{ Scan(...any) error }) (SettlementFail, error) {
	var f SettlementFail
	var amt string
	var reg, liq, st string
	err := row.Scan(&f.ID, &f.InstructionID, &f.TradeID, &f.AccountID, &f.Currency,
		&amt, &f.ISD, &f.DetectedAt, &reg, &liq, &f.CLSSettled, &st,
		&f.BuyInNotifiedAt, &f.ClosedOutAt, &f.ResolvedAt, &f.CreatedAt)
	if err != nil {
		return f, err
	}
	if f.Amount, err = decimal.NewFromString(amt); err != nil {
		return f, err
	}
	f.Regime, f.Liquidity, f.Status = FailRegime(reg), LiquidityClass(liq), FailStatus(st)
	return f, nil
}

func (s *PgxFailStore) InTx(ctx context.Context, fn func(ctx context.Context, tx FailTx) error) error {
	return RunInTx(ctx, s.Q, func(q Querier) error {
		return fn(ctx, pgxFailTx{q: q})
	})
}

func (s *PgxFailStore) FailByID(ctx context.Context, id int64) (SettlementFail, bool, error) {
	f, err := scanFail(s.Q.QueryRow(ctx,
		`SELECT `+failCols+` FROM settlement_fails WHERE id=$1`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return SettlementFail{}, false, nil
	}
	return f, err == nil, err
}

func (s *PgxFailStore) OpenFails(ctx context.Context, regime *FailRegime) ([]SettlementFail, error) {
	q := `SELECT ` + failCols + ` FROM settlement_fails WHERE status IN ('OPEN','BUYIN_NOTIFIED')`
	var args []any
	if regime != nil {
		args = append(args, string(*regime))
		q += " AND regime=$1::settlement_fail_regime_enum"
	}
	q += " ORDER BY id"
	rows, err := s.Q.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettlementFail
	for rows.Next() {
		f, err := scanFail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *PgxFailStore) FailsInRange(ctx context.Context, from, to time.Time) ([]SettlementFail, error) {
	rows, err := s.Q.Query(ctx,
		`SELECT `+failCols+` FROM settlement_fails
		 WHERE detected_at < $1 ORDER BY id`, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettlementFail
	for rows.Next() {
		f, err := scanFail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *PgxFailStore) PenaltiesInRange(ctx context.Context, from, to time.Time) ([]SettlementPenalty, error) {
	rows, err := s.Q.Query(ctx, `
		SELECT id, fail_id, accrual_date, regime::text, rate_bp::text,
		       base_amount::text, penalty_amount::text, currency, direction::text,
		       counterparty_id, status::text
		  FROM settlement_penalties
		 WHERE accrual_date >= $1 AND accrual_date < $2 ORDER BY id`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettlementPenalty
	for rows.Next() {
		var p SettlementPenalty
		var rate, base, pen string
		if err := rows.Scan(&p.ID, &p.FailID, &p.AccrualDate, &p.Regime,
			&rate, &base, &pen, &p.Currency, &p.Direction, &p.Counterparty, &p.Status); err != nil {
			return nil, err
		}
		if p.RateBP, err = decimal.NewFromString(rate); err != nil {
			return nil, err
		}
		if p.BaseAmount, err = decimal.NewFromString(base); err != nil {
			return nil, err
		}
		if p.PenaltyAmount, err = decimal.NewFromString(pen); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// pgxFailTx implements FailTx over any Querier.
type pgxFailTx struct{ q Querier }

func (t pgxFailTx) LateInstructions(ctx context.Context, day time.Time) ([]ExceptionLeg, error) {
	rows, err := t.q.Query(ctx, `
		SELECT `+legCols+` FROM settlement_instructions
		 WHERE status IN ('PENDING','FAILED')
		   AND settlement_date < $1
		   AND fail_flag IS NULL
		 ORDER BY id`, day.Truncate(24*time.Hour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExceptionLeg
	for rows.Next() {
		l, err := scanLeg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (t pgxFailTx) FlagInstruction(ctx context.Context, instructionID int64) error {
	_, err := t.q.Exec(ctx, `
		UPDATE settlement_instructions SET fail_flag='SETTLEMENT_FAIL', updated_at=now()
		 WHERE id=$1 AND fail_flag IS NULL`, instructionID)
	return err
}

func (t pgxFailTx) InsertFail(ctx context.Context, f SettlementFail) (SettlementFail, bool, error) {
	out, err := scanFail(t.q.QueryRow(ctx, `
		INSERT INTO settlement_fails
		    (settlement_instruction_id, trade_id, account_id, currency, amount,
		     isd, detected_at, regime, liquidity_class, cls_settled, status)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7,$8::settlement_fail_regime_enum,
		        $9::liquidity_class_enum,$10,'OPEN')
		ON CONFLICT (settlement_instruction_id) DO NOTHING
		RETURNING `+failCols,
		f.InstructionID, f.TradeID, f.AccountID, f.Currency, f.Amount.String(),
		f.ISD, f.DetectedAt, string(f.Regime), string(f.Liquidity), f.CLSSettled))
	if stderrors.Is(err, pgx.ErrNoRows) {
		existing, err2 := t.failByInstruction(ctx, f.InstructionID)
		return existing, false, err2
	}
	return out, err == nil, err
}

func (t pgxFailTx) failByInstruction(ctx context.Context, instructionID int64) (SettlementFail, error) {
	return scanFail(t.q.QueryRow(ctx,
		`SELECT `+failCols+` FROM settlement_fails WHERE settlement_instruction_id=$1`,
		instructionID))
}

func (t pgxFailTx) SetFailStatus(ctx context.Context, id int64, st FailStatus, at time.Time) error {
	tag, err := t.q.Exec(ctx, `
		UPDATE settlement_fails
		   SET status=$2::settlement_fail_status_enum, updated_at=$3,
		       resolved_at=CASE WHEN $2='RESOLVED' THEN $3 ELSE resolved_at END,
		       closed_out_at=CASE WHEN $2='CLOSED_OUT' THEN $3 ELSE closed_out_at END,
		       buyin_notified_at=CASE WHEN $2='BUYIN_NOTIFIED' THEN $3 ELSE buyin_notified_at END
		 WHERE id=$1 AND status <> $2::settlement_fail_status_enum`,
		id, string(st), at)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 && st != "" {
		// already in that state → idempotent
		var cur string
		if err := t.q.QueryRow(ctx,
			`SELECT status::text FROM settlement_fails WHERE id=$1`, id).Scan(&cur); err != nil {
			return err
		}
		if cur != string(st) {
			return excerrors.New(CodeExceptionConflict,
				fmt.Sprintf("fail %d transition to %s rejected (now %s)", id, st, cur))
		}
	}
	return nil
}

func (t pgxFailTx) InstructionSettled(ctx context.Context, instructionID int64) (bool, error) {
	var settled bool
	err := t.q.QueryRow(ctx, `
		SELECT status IN ('SETTLED','RECONCILED') FROM settlement_instructions WHERE id=$1`,
		instructionID).Scan(&settled)
	return settled, err
}

func (t pgxFailTx) AccruePenalty(ctx context.Context, p SettlementPenalty) (bool, error) {
	var id int64
	err := t.q.QueryRow(ctx, `
		INSERT INTO settlement_penalties
		    (fail_id, accrual_date, regime, rate_bp, base_amount, penalty_amount,
		     currency, direction, counterparty_id, status)
		VALUES ($1,$2,$3::settlement_fail_regime_enum,$4::numeric,$5::numeric,
		        $6::numeric,$7,$8::penalty_direction_enum,$9,'ACCRUED')
		ON CONFLICT (fail_id, accrual_date, direction) DO NOTHING
		RETURNING id`,
		p.FailID, p.AccrualDate, string(p.Regime), p.RateBP.String(),
		p.BaseAmount.String(), p.PenaltyAmount.String(), p.Currency,
		p.Direction, p.Counterparty).Scan(&id)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// openFailsTx satisfies failLister for the tx-scoped accrual scan.
func (t pgxFailTx) openFailsTx(ctx context.Context, regime FailRegime) ([]SettlementFail, error) {
	rows, err := t.q.Query(ctx, `
		SELECT `+failCols+` FROM settlement_fails
		 WHERE status IN ('OPEN','BUYIN_NOTIFIED') AND regime=$1::settlement_fail_regime_enum
		 ORDER BY id FOR UPDATE`, string(regime))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SettlementFail
	for rows.Next() {
		f, err := scanFail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// PgxLegClassifier — production LegClassifier (instruments + CLS join)
// ---------------------------------------------------------------------------

// PgxLegClassifier classifies legs by joining trades → instruments (every
// FX instrument_type → FX_CLOSEOUT) and checking cls_settlement_instructions
// for a live CLS route (exempt from bilateral penalties).
type PgxLegClassifier struct{ Q Querier }

// NewPgxLegClassifier binds the classifier to a pool.
func NewPgxLegClassifier(pool *pgxpool.Pool) *PgxLegClassifier {
	return &PgxLegClassifier{Q: pool}
}

// Classify resolves the regime: CLS-routed legs → cls_settled flag; every
// instrument_type in this venue's enum (SPOT/FORWARD/SWAP/NDF/OPTION) is FX
// → FX_CLOSEOUT. A non-FX instrument type (future securities venue support)
// classifies CSDR.
func (c *PgxLegClassifier) Classify(ctx context.Context, leg ExceptionLeg) (LegClassification, error) {
	var typ string
	var clsSettled bool
	err := c.Q.QueryRow(ctx, `
		SELECT COALESCE(i.instrument_type::text,'SPOT'),
		       EXISTS (SELECT 1 FROM cls_settlement_instructions c
		                WHERE c.trade_id = t.id
		                  AND c.status::text IN
		                      ('VALIDATED','MATCHED','ELIGIBLE','PAY_IN','SETTLED'))
		  FROM trades t LEFT JOIN instruments i ON i.id = t.instrument_id
		 WHERE t.id = $1`, leg.TradeID).Scan(&typ, &clsSettled)
	if stderrors.Is(err, pgx.ErrNoRows) {
		// Unknown trade — safest default is FX close-out (the venue's
		// only product family today).
		return LegClassification{Regime: RegimeFXCloseout, Liquidity: LiquidityLiquid}, nil
	}
	if err != nil {
		return LegClassification{}, fmt.Errorf("classify leg %d: %w", leg.ID, err)
	}
	regime := RegimeFXCloseout
	switch typ {
	case "SPOT", "FORWARD", "SWAP", "NDF", "OPTION":
		// FX family — §17.14 economics
	default:
		regime = RegimeCSDR // non-FX instrument type → CSD/securities scope
	}
	return LegClassification{Regime: regime, Liquidity: LiquidityLiquid, CLSSettled: clsSettled}, nil
}
