// roll.go — Position Roll Management (Phase-22 Task 22.3.8; spec §15,
// Phase-22 AC rows 27–29).
//
// A roll closes an expiring derivative contract (FORWARD / SWAP on
// derivative_contracts, migration 254) and opens the same exposure at a
// later value date in ONE SERIALIZABLE transaction — close+open commit
// together or roll back together, never a one-legged outcome (spec §2.7
// fail-closed zero-loss).
//
// Pricing (curve.go conventions — covered interest parity):
//
//	close_rate = fair value of the expiring leg — the outright forward
//	             to the source's remaining value_date (≈ spot when the
//	             contract matures inside the spot window).
//	open_rate  = the replacement contract's outright forward to the
//	             target value date.
//	roll_price = open_rate − close_rate   (the recorded roll spread)
//
// Both legs price off the Phase-19.5 yield curves + oracle spot mark —
// a missing/stale curve or mark fails the roll closed
// (YIELD_CURVE_UNAVAILABLE / PRICE_ORACLE_UNAVAILABLE); a roll never
// prices off a fabricated quote.
//
// Money path: the expiring contract's mark-to-market close-out
// (close_rate − source.forward_rate) × notional × side-sign is booked as
// a dated settlement_instructions leg (leg_tag FAR, quote currency)
// linked to the source contract, so the Phase-03 dispatch pipeline pays
// it through the GL exactly like a maturity settlement. The source row
// transitions to ROLLED (migration 255) — a terminal state the
// DueContracts sweep never re-picks.
//
// Automatic roll (Task 22.3.8 item 4): auto_roll_config stores the
// per-account switch, the timing rule (roll when value_date ≤ today +
// lead_days) and the replacement tenor. RunAutoRollSweep evaluates every
// due contract; each roll carries a deterministic idempotency key
// ("auto-roll:{contract}:{vd}:{tenor}") so a re-run replays instead of
// double-rolling.
//
// Route seam: RollHandler serves the frozen route
// POST /api/v1/orders/roll (internal/gateway/routes_v1.go, Phase-22);
// the orchestrator binds it.
package derivatives

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/api"
	"exchange/internal/audit"
	"exchange/internal/auth"
	"exchange/internal/gateway"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Codes emitted by this file (errs/codes.go localCodes — Phase-22
// Task 22.3.8 rows appended there).
const (
	// CodeRollNotPermitted — 400: the contract is not a rollable
	// derivative (wrong account, terminal status, NDF/OPTION/SPOT kind).
	CodeRollNotPermitted = "ROLL_NOT_PERMITTED"
	// CodeRollTargetInvalid — 400: target value date/tenor resolves
	// on/before the source maturity or fails the holiday gate.
	CodeRollTargetInvalid = "ROLL_TARGET_INVALID"
	// CodeRollSpreadTolerance — 422: |roll_price/close_rate| exceeded the
	// caller's max_roll_price_bps tolerance.
	CodeRollSpreadTolerance = "ROLL_SPREAD_TOLERANCE_EXCEEDED"
	// CodeRollConfigInvalid — 400: malformed auto-roll configuration.
	CodeRollConfigInvalid = "ROLL_CONFIG_INVALID"
)

// StatusRolled is the terminal derivative_contracts status stamped on
// the expiring leg (enum value added by migration 255).
const StatusRolled ContractStatus = "ROLLED"

// RollSource — contract_rolls.source vocabulary.
const (
	RollSourceManual = "MANUAL"
	RollSourceAuto   = "AUTO"
)

// RollTradeBit namespaces roll-derived pseudo trade ids. Trades and
// positions use the low range; a roll's replacement contract and its
// legs carry trade_id = rollID | 1<<60 — always positive, never a real
// trades.id, stable across replays.
const rollTradeBit = uint64(1) << 60

// ---------------------------------------------------------------------------
// Request / record / result
// ---------------------------------------------------------------------------

// RollRequest is one POST /api/v1/orders/roll call (or one auto-roll
// sweep iteration). Exactly one of NewValueDate / Tenor is required.
type RollRequest struct {
	AccountID       int64
	ContractID      int64            // source (expiring) derivative_contracts row
	NewValueDate    *time.Time       // explicit target maturity (holiday-checked)
	Tenor           string           // §7.4 grid tenor (e.g. "1M") → Dates.TenorDate
	MaxRollPriceBps *decimal.Decimal // optional |roll_price/close_rate| cap (bps)
	IdempotencyKey  string           // ≤128-char replay dedup key
	Source          string           // RollSourceManual | RollSourceAuto
}

// ContractRoll is the persisted roll record (contract_rolls).
type ContractRoll struct {
	ID               int64
	AccountID        int64
	SourceContractID int64
	TargetContractID int64
	Kind             ContractKind
	Side             ContractSide
	Notional         decimal.Decimal
	SourceValueDate  time.Time
	TargetValueDate  time.Time
	CloseRate        decimal.Decimal
	OpenRate         decimal.Decimal
	RollPrice        decimal.Decimal // open_rate − close_rate
	ClosePnL         decimal.Decimal // signed, quote currency
	CloseLegID       int64
	Status           string
	Source           string
	IdempotencyKey   string
	CreatedAt        time.Time
	CompletedAt      *time.Time
}

// RollResult reports the committed roll (JSON payload of the route).
type RollResult struct {
	RollID           int64           `json:"roll_id"`
	AccountID        int64           `json:"account_id"`
	SourceContractID int64           `json:"source_contract_id"`
	TargetContractID int64           `json:"target_contract_id"`
	Kind             string          `json:"kind"`
	Notional         decimal.Decimal `json:"notional"`
	SourceValueDate  string          `json:"source_value_date"`
	TargetValueDate  string          `json:"target_value_date"`
	CloseRate        decimal.Decimal `json:"close_rate"`
	OpenRate         decimal.Decimal `json:"open_rate"`
	RollPrice        decimal.Decimal `json:"roll_price"` // open − close
	ClosePnL         decimal.Decimal `json:"close_pnl"`  // signed, quote ccy
	Status           string          `json:"status"`
	Replayed         bool            `json:"replayed"`
}

// ---------------------------------------------------------------------------
// Store seam — the atomic unit of work
// ---------------------------------------------------------------------------

// RollTx is the transaction-scoped persistence surface for a roll: the
// contract reads/writes mirror ContractTx (types.go) and add the
// contract_rolls / auto_roll_config surface this task owns. Every write
// lands in the same SERIALIZABLE tx, so close+open is atomic.
type RollTx interface {
	// ContractForUpdate loads a derivative_contracts row under FOR UPDATE.
	ContractForUpdate(ctx context.Context, id int64) (*Contract, error)
	// InsertContract writes the replacement contract (idempotent on
	// IdempotencyKey — same semantics as types.go).
	InsertContract(ctx context.Context, c *Contract) (id int64, created bool, err error)
	// InsertLegs writes settlement_instructions rows linked to a contract
	// (idempotent via the shared leg_ux index).
	InsertLegs(ctx context.Context, legs []SettlementLeg) (int, error)
	// InsertLegsReturningIDs behaves like InsertLegs but returns the
	// inserted row ids (the roll records its closing P&L leg id).
	InsertLegsReturningIDs(ctx context.Context, legs []SettlementLeg) ([]int64, error)
	// UpdateContractStatus transitions the contract status.
	UpdateContractStatus(ctx context.Context, id int64, st ContractStatus, settledAt *time.Time) error
	// SettlementCycleDays reads instruments.settlement_cycle (SMALLINT —
	// T+0/T+1/T+2) for the contract's instrument.
	SettlementCycleDays(ctx context.Context, instrumentID int64) (int, error)
	// InsertRoll writes the PENDING audit row; a replayed idempotency key
	// returns created=false.
	InsertRoll(ctx context.Context, r *ContractRoll) (id int64, created bool, err error)
	// CompleteRoll finalizes the row with the committed economics.
	CompleteRoll(ctx context.Context, r *ContractRoll) error
	// LoadRollByKey resolves a replayed idempotency key.
	LoadRollByKey(ctx context.Context, key string) (*ContractRoll, error)
	// PutAutoRollConfig upserts the account's auto_roll_config row.
	PutAutoRollConfig(ctx context.Context, cfg AutoRollConfig) error
	// GetAutoRollConfig loads the config (nil when unset → disabled).
	GetAutoRollConfig(ctx context.Context, accountID int64) (*AutoRollConfig, error)
	// DueAutoRollContracts lists OPEN/PARTIALLY_SETTLED FORWARD|SWAP
	// contracts of enabled accounts joined with their timing rule.
	DueAutoRollContracts(ctx context.Context) ([]AutoRollDue, error)
	// AuditAppend writes a hash-chained audit row (audit.Append).
	AuditAppend(ctx context.Context, table string, recordID *int64, action string) error
}

// AutoRollDue is one auto-roll candidate: the contract plus the account's
// timing rule.
type AutoRollDue struct {
	ContractID int64
	AccountID  int64
	LeadDays   int
	Tenor      string
	ValueDate  time.Time
}

// RollStore owns the SERIALIZABLE transaction with the §5.40
// conflict-retry schedule (40001/40P01 → 5/15/45 ms, 3 attempts —
// isSerializationConflict / conflictBackoff from types.go).
type RollStore interface {
	InTx(ctx context.Context, fn func(context.Context, RollTx) error) error
	// ReadTx runs a read-only query outside the roll transaction — used by
	// the auto-roll sweep's candidate scan and config reads.
	ReadTx(ctx context.Context, fn func(context.Context, RollTx) error) error
}

// ---------------------------------------------------------------------------
// PgxRollStore — PostgreSQL implementation (migration 255)
// ---------------------------------------------------------------------------

// PgxRollStore implements RollStore over pgx; the contract SQL mirrors
// pgxContractTx (types.go) verbatim so contract semantics stay identical.
type PgxRollStore struct {
	Pool     *pgxpool.Pool
	MaxTries int // default 3
}

// NewPgxRollStore wires the store.
func NewPgxRollStore(pool *pgxpool.Pool) *PgxRollStore {
	return &PgxRollStore{Pool: pool, MaxTries: 3}
}

// InTx runs fn inside a SERIALIZABLE transaction with the §5.40 retry
// schedule; exhaustion surfaces TRANSACTION_CONFLICT_RETRY_EXHAUSTED.
func (s *PgxRollStore) InTx(ctx context.Context, fn func(context.Context, RollTx) error) error {
	return s.run(ctx, pgx.Serializable, fn)
}

// ReadTx runs fn at READ COMMITTED — scan/read paths only.
func (s *PgxRollStore) ReadTx(ctx context.Context, fn func(context.Context, RollTx) error) error {
	return s.run(ctx, pgx.ReadCommitted, fn)
}

func (s *PgxRollStore) run(ctx context.Context, iso pgx.TxIsoLevel, fn func(context.Context, RollTx) error) error {
	tries := s.MaxTries
	if tries <= 0 {
		tries = 3
	}
	var lastErr error
	for attempt := 0; attempt < tries; attempt++ {
		tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: iso})
		if err != nil {
			return fmt.Errorf("roll tx begin: %w", err)
		}
		err = fn(ctx, pgxRollTx{tx: tx})
		if err == nil {
			err = tx.Commit(ctx)
			if err == nil {
				return nil
			}
			err = fmt.Errorf("roll tx commit: %w", err)
		} else {
			_ = tx.Rollback(ctx)
		}
		lastErr = err
		if !isSerializationConflict(err) {
			return err
		}
		if attempt+1 < tries {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(conflictBackoff[minInt(attempt, len(conflictBackoff)-1)]):
			}
		}
	}
	return excerrors.Wrap(CodeTxnConflictExhausted,
		"derivatives roll: SERIALIZABLE conflict retry budget exhausted", lastErr)
}

type pgxRollTx struct{ tx pgx.Tx }

func (t pgxRollTx) ContractForUpdate(ctx context.Context, id int64) (*Contract, error) {
	c, err := scanContract(t.tx.QueryRow(ctx, contractCols+`
		  FROM derivative_contracts WHERE id = $1 FOR UPDATE`, id))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, excerrors.New(CodeNotFound,
			fmt.Sprintf("derivative contract %d not found", id))
	}
	if err != nil {
		return nil, fmt.Errorf("roll: load contract %d: %w", id, err)
	}
	return c, nil
}

func (t pgxRollTx) InsertContract(ctx context.Context, c *Contract) (int64, bool, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `
		INSERT INTO derivative_contracts
		    (trade_id, account_id, instrument_id, kind, side,
		     base_currency, quote_currency, notional, spot_rate,
		     forward_rate, swap_points, spot_value_date, value_date,
		     near_leg_value_date, ndf_fixing_date, ndf_fixing_source,
		     settlement_currency, status, idempotency_key)
		VALUES ($1,$2,$3,$4::derivative_kind_enum,$5,$6,$7,$8::numeric,$9::numeric,
		        $10::numeric,$11::numeric,$12,$13,$14,$15,NULLIF($16,''),
		        NULLIF($17,''),'OPEN',NULLIF($18,''))
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id`,
		c.TradeID, c.AccountID, c.InstrumentID, string(c.Kind), string(c.Side),
		c.Pair.Base, c.Pair.Quote, c.Notional.String(), c.SpotRate.String(),
		c.ForwardRate.String(), c.SwapPoints.String(), c.SpotValueDate, c.ValueDate,
		c.NearLegValueDate, c.NdfFixingDate, c.NdfFixingSource,
		c.SettlementCurrency, c.IdempotencyKey).Scan(&id)
	if stderrors.Is(err, pgx.ErrNoRows) {
		var existing int64
		if err := t.tx.QueryRow(ctx,
			`SELECT id FROM derivative_contracts WHERE idempotency_key = $1`,
			c.IdempotencyKey).Scan(&existing); err != nil {
			return 0, false, fmt.Errorf("roll: contract replay lookup: %w", err)
		}
		return existing, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("roll: insert contract: %w", err)
	}
	return id, true, nil
}

func (t pgxRollTx) InsertLegs(ctx context.Context, legs []SettlementLeg) (int, error) {
	ids, err := t.InsertLegsReturningIDs(ctx, legs)
	return len(ids), err
}

func (t pgxRollTx) InsertLegsReturningIDs(ctx context.Context, legs []SettlementLeg) ([]int64, error) {
	out := make([]int64, 0, len(legs))
	for _, l := range legs {
		var id int64
		err := t.tx.QueryRow(ctx, `
			INSERT INTO settlement_instructions
			    (trade_id, account_id, currency, amount, direction,
			     settlement_date, status, derivative_contract_id, leg_tag)
			VALUES ($1,$2,$3,$4::numeric,$5::settlement_direction_enum,$6,'PENDING',$7,$8)
			ON CONFLICT (trade_id, account_id, currency, direction) DO NOTHING
			RETURNING id`,
			l.TradeID, l.AccountID, l.Currency, l.Amount.String(),
			string(l.Direction), l.ValueDate, l.ContractID, string(l.Tag)).Scan(&id)
		if stderrors.Is(err, pgx.ErrNoRows) {
			out = append(out, 0) // absorbed by a prior writer — replay-safe
			continue
		}
		if err != nil {
			return out, fmt.Errorf("roll: insert leg %s %s %s: %w",
				l.Tag, l.Direction, l.Currency, err)
		}
		out = append(out, id)
	}
	return out, nil
}

func (t pgxRollTx) UpdateContractStatus(ctx context.Context, id int64, st ContractStatus, settledAt *time.Time) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE derivative_contracts
		   SET status = $2::derivative_contract_status_enum, settled_at = $3,
		       updated_at = now()
		 WHERE id = $1`, id, string(st), settledAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return excerrors.New(CodeNotFound,
			fmt.Sprintf("derivative contract %d not found", id))
	}
	return nil
}

func (t pgxRollTx) SettlementCycleDays(ctx context.Context, instrumentID int64) (int, error) {
	var days int
	err := t.tx.QueryRow(ctx,
		`SELECT settlement_cycle FROM instruments WHERE id = $1`, instrumentID).Scan(&days)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return 0, excerrors.New(CodeNotFound,
			fmt.Sprintf("instrument %d not found", instrumentID))
	}
	if err != nil {
		return 0, fmt.Errorf("roll: settlement_cycle: %w", err)
	}
	return days, nil
}

func (t pgxRollTx) InsertRoll(ctx context.Context, r *ContractRoll) (int64, bool, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `
		INSERT INTO contract_rolls
		    (account_id, source_contract_id, kind, side, notional,
		     source_value_date, target_value_date, source, idempotency_key)
		VALUES ($1,$2,$3::derivative_kind_enum,$4,$5::numeric,$6,$7,$8,NULLIF($9,''))
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING id`,
		r.AccountID, r.SourceContractID, string(r.Kind), string(r.Side),
		r.Notional.String(), r.SourceValueDate, r.TargetValueDate,
		r.Source, r.IdempotencyKey).Scan(&id)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("roll: insert audit row: %w", err)
	}
	return id, true, nil
}

func (t pgxRollTx) CompleteRoll(ctx context.Context, r *ContractRoll) error {
	var closeLeg *int64
	if r.CloseLegID > 0 {
		closeLeg = &r.CloseLegID
	}
	_, err := t.tx.Exec(ctx, `
		UPDATE contract_rolls SET status='COMPLETED', target_contract_id=$2,
		       close_rate=$3::numeric, open_rate=$4::numeric, roll_price=$5::numeric,
		       close_pnl=$6::numeric, close_leg_id=$7, completed_at=now()
		 WHERE id=$1`,
		r.ID, r.TargetContractID, r.CloseRate.String(), r.OpenRate.String(),
		r.RollPrice.String(), r.ClosePnL.String(), closeLeg)
	if err != nil {
		return fmt.Errorf("roll: complete row %d: %w", r.ID, err)
	}
	return nil
}

func (t pgxRollTx) LoadRollByKey(ctx context.Context, key string) (*ContractRoll, error) {
	var (
		r                                ContractRoll
		closeRate, openRate, rollPx, pnl *string
		tgt                              *int64
		closeLeg                         *int64
		srcVD, tgtVD                     time.Time
		completedAt                      *time.Time
	)
	err := t.tx.QueryRow(ctx, `
		SELECT id, account_id, source_contract_id, target_contract_id,
		       kind::text, side, notional::text, source_value_date, target_value_date,
		       close_rate::text, open_rate::text, roll_price::text, close_pnl::text,
		       close_leg_id, status, source, created_at, completed_at
		  FROM contract_rolls WHERE idempotency_key = $1`, key).Scan(
		&r.ID, &r.AccountID, &r.SourceContractID, &tgt,
		(*string)(&r.Kind), (*string)(&r.Side), scanDec(&r.Notional),
		&srcVD, &tgtVD, &closeRate, &openRate, &rollPx, &pnl,
		&closeLeg, &r.Status, &r.Source, &r.CreatedAt, &completedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, nil // no prior write under this key
	}
	if err != nil {
		return nil, fmt.Errorf("roll: replay lookup %q: %w", key, err)
	}
	r.SourceValueDate, r.TargetValueDate = srcVD, tgtVD
	r.TargetContractID = derefI64(tgt)
	r.CloseLegID = derefI64(closeLeg)
	r.CloseRate = parseOr0(closeRate)
	r.OpenRate = parseOr0(openRate)
	r.RollPrice = parseOr0(rollPx)
	r.ClosePnL = parseOr0(pnl)
	r.CompletedAt = completedAt
	return &r, nil
}

func (t pgxRollTx) AuditAppend(ctx context.Context, table string, recordID *int64, action string) error {
	_, err := audit.Append(ctx, t.tx, table, recordID, action, nil)
	return err
}

func (t pgxRollTx) PutAutoRollConfig(ctx context.Context, cfg AutoRollConfig) error {
	_, err := t.tx.Exec(ctx, `
		INSERT INTO auto_roll_config (account_id, enabled, lead_days, tenor, updated_at)
		VALUES ($1,$2,$3,$4, now())
		ON CONFLICT (account_id) DO UPDATE SET
		    enabled = EXCLUDED.enabled, lead_days = EXCLUDED.lead_days,
		    tenor = EXCLUDED.tenor, updated_at = now()`,
		cfg.AccountID, cfg.Enabled, cfg.LeadDays, cfg.Tenor)
	if err != nil {
		return fmt.Errorf("auto_roll_config upsert: %w", err)
	}
	return nil
}

func (t pgxRollTx) GetAutoRollConfig(ctx context.Context, accountID int64) (*AutoRollConfig, error) {
	var c AutoRollConfig
	err := t.tx.QueryRow(ctx, `
		SELECT account_id, enabled, lead_days, tenor
		  FROM auto_roll_config WHERE account_id = $1`, accountID).
		Scan(&c.AccountID, &c.Enabled, &c.LeadDays, &c.Tenor)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auto_roll_config read: %w", err)
	}
	return &c, nil
}

func (t pgxRollTx) DueAutoRollContracts(ctx context.Context) ([]AutoRollDue, error) {
	rows, err := t.tx.Query(ctx, `
		SELECT c.id, c.account_id, a.lead_days, a.tenor, c.value_date
		  FROM derivative_contracts c
		  JOIN auto_roll_config a ON a.account_id = c.account_id
		 WHERE a.enabled
		   AND c.status IN ('OPEN','PARTIALLY_SETTLED')
		   AND c.kind IN ('FORWARD','SWAP')
		 ORDER BY c.value_date, c.id`)
	if err != nil {
		return nil, fmt.Errorf("auto-roll scan: %w", err)
	}
	defer rows.Close()
	var out []AutoRollDue
	for rows.Next() {
		var d AutoRollDue
		if err := rows.Scan(&d.ContractID, &d.AccountID, &d.LeadDays, &d.Tenor, &d.ValueDate); err != nil {
			return nil, fmt.Errorf("auto-roll scan row: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func derefI64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func parseOr0(s *string) decimal.Decimal {
	if s == nil {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(*s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// RollService executes contract rolls and the auto-roll sweep. Dates and
// Pricer are the sibling seams (holiday calendar + CIP pricing);
// everything else rides the RollStore transaction boundary.
type RollService struct {
	Store  RollStore
	Dates  *Dates
	Pricer *Pricer
	now    func() time.Time
}

// NewRollService wires the service; nil deps fail closed on first use.
func NewRollService(store RollStore, d *Dates, p *Pricer) *RollService {
	return &RollService{Store: store, Dates: d, Pricer: p, now: time.Now}
}

// SetNowFunc injects the clock (tests).
func (s *RollService) SetNowFunc(now func() time.Time) { s.now = now }

// Roll executes one close+open contract roll atomically.
func (s *RollService) Roll(ctx context.Context, req RollRequest) (*RollResult, error) {
	if err := validateRollRequest(req); err != nil {
		return nil, err
	}
	if req.Source == "" {
		req.Source = RollSourceManual
	}
	var res *RollResult
	err := s.Store.InTx(ctx, func(ctx context.Context, tx RollTx) error {
		var err error
		res, err = s.rollInTx(ctx, tx, req)
		return err
	})
	return res, err
}

func (s *RollService) rollInTx(ctx context.Context, tx RollTx, req RollRequest) (*RollResult, error) {
	// 0. Idempotent replay — a committed roll row resolves before any
	//    source-state validation (the source is ROLLED by construction).
	if req.IdempotencyKey != "" {
		prior, err := tx.LoadRollByKey(ctx, req.IdempotencyKey)
		if err != nil {
			return nil, err
		}
		if prior != nil {
			if prior.AccountID != req.AccountID || prior.SourceContractID != req.ContractID {
				return nil, excerrors.New("IDEMPOTENCY_KEY_MISMATCH", fmt.Sprintf(
					"roll idempotency key %q bound to roll %d (account %d, contract %d)",
					req.IdempotencyKey, prior.ID, prior.AccountID, prior.SourceContractID))
			}
			if prior.Status != "COMPLETED" {
				return nil, excerrors.New(CodeDerivativeStateConflict, fmt.Sprintf(
					"roll %d is %s — resume-safe state requires operator resolution",
					prior.ID, prior.Status))
			}
			return rollResultFromRecord(prior, true), nil
		}
	}

	// 1. Source contract, locked — must be the caller's live forward/swap.
	src, err := tx.ContractForUpdate(ctx, req.ContractID)
	if err != nil {
		return nil, err
	}
	if src.AccountID != req.AccountID {
		return nil, excerrors.New(CodeRollNotPermitted, fmt.Sprintf(
			"contract %d belongs to account %d, not %d", src.ID, src.AccountID, req.AccountID))
	}
	if src.Status != StatusOpen && src.Status != StatusPartiallySettled {
		return nil, excerrors.New(CodeRollNotPermitted, fmt.Sprintf(
			"contract %d is %s — only OPEN/PARTIALLY_SETTLED rolls", src.ID, src.Status))
	}
	if src.Kind != KindForward && src.Kind != KindSwap {
		return nil, excerrors.New(CodeRollNotPermitted, fmt.Sprintf(
			"contract %d kind %s is not rollable (FORWARD|SWAP)", src.ID, src.Kind))
	}
	if s.Dates == nil || s.Pricer == nil {
		return nil, excerrors.New(CodeInvalidRequest,
			"roll: pricing/date seams unwired — fail-closed")
	}

	// 2. Dates: spot value date at roll time, then the target maturity
	//    (explicit date through the holiday gate, or a §7.4 tenor).
	tradeDay := normDay(s.now())
	cycleDays, err := tx.SettlementCycleDays(ctx, src.InstrumentID)
	if err != nil {
		return nil, err
	}
	spotDate, err := s.Dates.SpotDate(src.Pair, tradeDay, cycleDays)
	if err != nil {
		return nil, err
	}
	var newVD time.Time
	if req.NewValueDate != nil {
		newVD = normDay(*req.NewValueDate)
		if err := s.Dates.CheckValueDate(src.Pair, spotDate, newVD); err != nil {
			return nil, excerrors.Wrap(CodeRollTargetInvalid,
				"roll target value date rejected", err)
		}
	} else {
		newVD, err = s.Dates.TenorDate(src.Pair, tradeDay, cycleDays, req.Tenor)
		if err != nil {
			return nil, excerrors.Wrap(CodeRollTargetInvalid,
				fmt.Sprintf("roll target tenor %q", req.Tenor), err)
		}
	}
	if !newVD.After(normDay(src.ValueDate)) {
		return nil, excerrors.New(CodeRollTargetInvalid, fmt.Sprintf(
			"roll target %s must be after the expiring maturity %s",
			newVD.Format("2006-01-02"), src.ValueDate.Format("2006-01-02")))
	}

	// 3. Prices — oracle spot + curve forwards, fail-closed.
	spot, err := s.Pricer.SpotRate(ctx, src.Pair)
	if err != nil {
		return nil, err
	}
	closeRate := spot
	if normDay(src.ValueDate).After(spotDate) {
		q, err := s.Pricer.PriceForward(ctx, src.Pair, spot, spotDate, src.ValueDate)
		if err != nil {
			return nil, err
		}
		closeRate = q.ForwardRate
	}
	openQ, err := s.Pricer.PriceForward(ctx, src.Pair, spot, spotDate, newVD)
	if err != nil {
		return nil, err
	}
	openRate := openQ.ForwardRate
	rollPrice := openRate.Sub(closeRate)
	if req.MaxRollPriceBps != nil && closeRate.IsPositive() {
		devBps := rollPrice.Abs().Div(closeRate).Mul(decimal.NewFromInt(10000))
		if devBps.GreaterThan(*req.MaxRollPriceBps) {
			return nil, excerrors.New(CodeRollSpreadTolerance, fmt.Sprintf(
				"roll price %s deviates %s bps from close %s, tolerance %s bps",
				rollPrice, devBps.Round(4), closeRate, req.MaxRollPriceBps))
		}
	}

	// 4. Audit row — the idempotency anchor. A committed replay resolves
	//    to the stored row without re-applying.
	roll := &ContractRoll{
		AccountID: req.AccountID, SourceContractID: src.ID,
		Kind: src.Kind, Side: src.Side, Notional: src.Notional,
		SourceValueDate: src.ValueDate, TargetValueDate: newVD,
		Status: "PENDING", Source: req.Source, IdempotencyKey: req.IdempotencyKey,
	}
	rollID, created, err := tx.InsertRoll(ctx, roll)
	if err != nil {
		return nil, err
	}
	if !created {
		// A writer committed between the step-0 probe and this INSERT
		// (SERIALIZABLE makes this unreachable in practice, but the
		// partial unique index guarantees the guard under any isolation).
		prior, err := tx.LoadRollByKey(ctx, req.IdempotencyKey)
		if err != nil || prior == nil {
			return nil, excerrors.Wrap("IDEMPOTENCY_KEY_MISMATCH",
				"roll: idempotency-key replay could not resolve the original row", err)
		}
		return rollResultFromRecord(prior, true), nil
	}
	roll.ID = rollID
	pseudoTrade := int64(uint64(rollID) | rollTradeBit)

	// 5. Closing mark-to-market — a dated settlement leg on the source
	//    contract (quote currency, PAY/RECEIVE by sign). The Phase-03
	//    dispatch pipeline pays it through the GL.
	sign := decimal.NewFromInt(1)
	if src.Side == SideSell {
		sign = decimal.NewFromInt(-1)
	}
	closePnL := closeRate.Sub(src.ForwardRate).Mul(src.Notional).Mul(sign).Round(8)
	var closeLegID int64
	if !closePnL.IsZero() {
		dir := LegReceive
		if closePnL.IsNegative() {
			dir = LegPay
		}
		ids, err := tx.InsertLegsReturningIDs(ctx, []SettlementLeg{{
			ContractID: src.ID, TradeID: pseudoTrade,
			AccountID: src.AccountID, Tag: LegTagFar,
			Currency: src.Pair.Quote, Amount: closePnL.Abs(),
			Direction: dir, ValueDate: src.ValueDate,
		}})
		if err != nil {
			return nil, err
		}
		if len(ids) > 0 {
			closeLegID = ids[0]
		}
	}

	// 6. Replacement contract — same account/instrument/kind/side/
	//    notional, fresh spot + forward rate, new maturity. A rolled swap
	//    re-legs near at the old maturity, far at the new one.
	nearVD := src.ValueDate // swap roll: near leg settles at the roll point
	newContract := &Contract{
		TradeID:        pseudoTrade,
		AccountID:      src.AccountID,
		InstrumentID:   src.InstrumentID,
		Kind:           src.Kind,
		Side:           src.Side,
		Pair:           src.Pair,
		Notional:       src.Notional,
		SpotRate:       spot,
		ForwardRate:    openRate,
		SwapPoints:     openRate.Sub(spot),
		SpotValueDate:  spotDate,
		ValueDate:      newVD,
		IdempotencyKey: fmt.Sprintf("roll:%d", rollID),
	}
	if src.Kind == KindSwap {
		nv := nearVD
		newContract.NearLegValueDate = &nv
		// The rolled swap's near leg prices at the expiring leg's rate —
		// economically it IS the old delivery exchanged again.
		newContract.SpotRate = closeRate
		newContract.SwapPoints = openRate.Sub(closeRate)
	}
	newID, _, err := tx.InsertContract(ctx, newContract)
	if err != nil {
		return nil, err
	}
	newContract.ID = newID
	if _, err := tx.InsertLegs(ctx, rollDeliveryLegs(newContract)); err != nil {
		return nil, err
	}

	// 7. Close the source — terminal ROLLED, never re-swept.
	now := s.now().UTC()
	if err := tx.UpdateContractStatus(ctx, src.ID, StatusRolled, &now); err != nil {
		return nil, err
	}
	if err := tx.AuditAppend(ctx, "derivative_contracts", &src.ID, "UPDATE"); err != nil {
		return nil, fmt.Errorf("roll: contract audit: %w", err)
	}
	if err := tx.AuditAppend(ctx, "derivative_contracts", &newID, "INSERT"); err != nil {
		return nil, fmt.Errorf("roll: replacement audit: %w", err)
	}

	// 8. Finalize the roll record + its audit append.
	roll.TargetContractID = newID
	roll.CloseRate, roll.OpenRate, roll.RollPrice = closeRate, openRate, rollPrice
	roll.ClosePnL, roll.CloseLegID = closePnL, closeLegID
	roll.Status = "COMPLETED"
	if err := tx.CompleteRoll(ctx, roll); err != nil {
		return nil, err
	}
	if err := tx.AuditAppend(ctx, "contract_rolls", &rollID, "INSERT"); err != nil {
		return nil, fmt.Errorf("roll: audit append: %w", err)
	}
	return rollResultFromRecord(roll, false), nil
}

// rollDeliveryLegs renders the replacement contract's delivery legs —
// the same shapes the sibling services book (forwards.go deliveryLegs /
// swaps.go legs), so the settlement pipeline treats a rolled contract
// identically to a fresh one.
func rollDeliveryLegs(c *Contract) []SettlementLeg {
	if c.Kind == KindForward {
		quoteAmount := c.Notional.Mul(c.ForwardRate).Round(8)
		baseLeg := SettlementLeg{
			ContractID: c.ID, TradeID: c.TradeID, AccountID: c.AccountID, Tag: LegTagFar,
			Currency: c.Pair.Base, Amount: c.Notional.Round(8), ValueDate: c.ValueDate,
		}
		quoteLeg := SettlementLeg{
			ContractID: c.ID, TradeID: c.TradeID, AccountID: c.AccountID, Tag: LegTagFar,
			Currency: c.Pair.Quote, Amount: quoteAmount, ValueDate: c.ValueDate,
		}
		if c.Side == SideBuy {
			baseLeg.Direction, quoteLeg.Direction = LegReceive, LegPay
		} else {
			baseLeg.Direction, quoteLeg.Direction = LegPay, LegReceive
		}
		return []SettlementLeg{baseLeg, quoteLeg}
	}
	// SWAP — near/far legs at the roll point / new maturity.
	nearQuote := c.Notional.Mul(c.SpotRate).Round(8)
	farQuote := c.Notional.Mul(c.ForwardRate).Round(8)
	mk := func(tag LegTag, ccy string, amt decimal.Decimal, dir LegDirection, d time.Time) SettlementLeg {
		return SettlementLeg{
			ContractID: c.ID, TradeID: c.TradeID, AccountID: c.AccountID, Tag: tag,
			Currency: ccy, Amount: amt, Direction: dir, ValueDate: d,
		}
	}
	near := *c.NearLegValueDate
	if c.Side == SideBuy {
		return []SettlementLeg{
			mk(LegTagNear, c.Pair.Base, c.Notional.Round(8), LegReceive, near),
			mk(LegTagNear, c.Pair.Quote, nearQuote, LegPay, near),
			mk(LegTagFar, c.Pair.Base, c.Notional.Round(8), LegPay, c.ValueDate),
			mk(LegTagFar, c.Pair.Quote, farQuote, LegReceive, c.ValueDate),
		}
	}
	return []SettlementLeg{
		mk(LegTagNear, c.Pair.Base, c.Notional.Round(8), LegPay, near),
		mk(LegTagNear, c.Pair.Quote, nearQuote, LegReceive, near),
		mk(LegTagFar, c.Pair.Base, c.Notional.Round(8), LegReceive, c.ValueDate),
		mk(LegTagFar, c.Pair.Quote, farQuote, LegPay, c.ValueDate),
	}
}

func rollResultFromRecord(r *ContractRoll, replayed bool) *RollResult {
	return &RollResult{
		RollID: r.ID, AccountID: r.AccountID,
		SourceContractID: r.SourceContractID, TargetContractID: r.TargetContractID,
		Kind: string(r.Kind), Notional: r.Notional,
		SourceValueDate: r.SourceValueDate.Format("2006-01-02"),
		TargetValueDate: r.TargetValueDate.Format("2006-01-02"),
		CloseRate:       r.CloseRate, OpenRate: r.OpenRate, RollPrice: r.RollPrice,
		ClosePnL: r.ClosePnL, Status: r.Status, Replayed: replayed,
	}
}

func validateRollRequest(req RollRequest) error {
	if req.AccountID <= 0 || req.ContractID <= 0 {
		return excerrors.New(CodeInvalidRequest, "account_id and contract_id are required")
	}
	if req.NewValueDate == nil && req.Tenor == "" {
		return excerrors.New(CodeInvalidRequest,
			"one of new_value_date or tenor is required")
	}
	if req.NewValueDate != nil && req.Tenor != "" {
		return excerrors.New(CodeInvalidRequest,
			"new_value_date and tenor are mutually exclusive")
	}
	if req.MaxRollPriceBps != nil && req.MaxRollPriceBps.IsNegative() {
		return excerrors.New(CodeInvalidRequest, "max_roll_price_bps must be >= 0")
	}
	if len(req.IdempotencyKey) > 128 {
		return excerrors.New(CodeInvalidRequest, "idempotency_key exceeds 128 chars")
	}
	switch req.Source {
	case "", RollSourceManual, RollSourceAuto:
	default:
		return excerrors.New(CodeInvalidRequest, fmt.Sprintf("unknown roll source %q", req.Source))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Auto-roll configuration + sweep (Task 22.3.8 item 4)
// ---------------------------------------------------------------------------

// AutoRollConfig is the per-account automatic-roll row (auto_roll_config).
type AutoRollConfig struct {
	AccountID int64  `json:"account_id"`
	Enabled   bool   `json:"enabled"`
	LeadDays  int    `json:"lead_days"` // roll when value_date <= today + lead_days
	Tenor     string `json:"tenor"`     // replacement maturity on the §7.4 grid
}

var autoRollTenors = map[string]bool{
	"1W": true, "2W": true, "1M": true, "2M": true,
	"3M": true, "6M": true, "9M": true, "1Y": true,
}

// SetAutoRoll upserts the per-account auto-roll config (audit-appended).
func (s *RollService) SetAutoRoll(ctx context.Context, cfg AutoRollConfig) error {
	if cfg.AccountID <= 0 {
		return excerrors.New(CodeRollConfigInvalid, "account_id required")
	}
	if cfg.LeadDays < 0 || cfg.LeadDays > 10 {
		return excerrors.New(CodeRollConfigInvalid,
			fmt.Sprintf("lead_days %d outside 0..10", cfg.LeadDays))
	}
	if cfg.Tenor == "" {
		cfg.Tenor = "1M"
	}
	if !autoRollTenors[cfg.Tenor] {
		return excerrors.New(CodeRollConfigInvalid,
			fmt.Sprintf("tenor %q not on the §7.4 grid", cfg.Tenor))
	}
	return s.Store.InTx(ctx, func(ctx context.Context, tx RollTx) error {
		if err := tx.PutAutoRollConfig(ctx, cfg); err != nil {
			return err
		}
		return tx.AuditAppend(ctx, "auto_roll_config", &cfg.AccountID, "UPDATE")
	})
}

// AutoRoll returns the account's config (nil when unset → disabled).
func (s *RollService) AutoRoll(ctx context.Context, accountID int64) (*AutoRollConfig, error) {
	var cfg *AutoRollConfig
	err := s.Store.ReadTx(ctx, func(ctx context.Context, tx RollTx) error {
		var err error
		cfg, err = tx.GetAutoRollConfig(ctx, accountID)
		return err
	})
	return cfg, err
}

// AutoRollReport summarizes one RunAutoRollSweep pass.
type AutoRollReport struct {
	Evaluated int             `json:"evaluated"`
	Rolled    int             `json:"rolled"`
	Errors    []AutoRollError `json:"errors,omitempty"`
}

// AutoRollError is one contract-level auto-roll failure.
type AutoRollError struct {
	ContractID int64  `json:"contract_id"`
	AccountID  int64  `json:"account_id"`
	Err        string `json:"error"`
}

// RunAutoRollSweep rolls every due derivative contract of accounts with
// auto_roll_config.enabled: a contract is due when value_date <= today +
// lead_days. Each roll is a separate transaction with a deterministic
// idempotency key — a failed contract is collected into the report and
// never strands the batch.
func (s *RollService) RunAutoRollSweep(ctx context.Context, now time.Time) (AutoRollReport, error) {
	var list []AutoRollDue
	err := s.Store.ReadTx(ctx, func(ctx context.Context, tx RollTx) error {
		var err error
		list, err = tx.DueAutoRollContracts(ctx)
		return err
	})
	if err != nil {
		return AutoRollReport{}, err
	}

	rep := AutoRollReport{}
	today := normDay(now)
	for _, d := range list {
		if normDay(d.ValueDate).After(today.AddDate(0, 0, d.LeadDays)) {
			continue // not due yet
		}
		rep.Evaluated++
		_, err := s.Roll(ctx, RollRequest{
			AccountID: d.AccountID, ContractID: d.ContractID,
			Tenor:  d.Tenor,
			Source: RollSourceAuto,
			IdempotencyKey: fmt.Sprintf("auto-roll:%d:%s:%s",
				d.ContractID, d.Tenor, normDay(d.ValueDate).Format("2006-01-02")),
		})
		if err != nil {
			rep.Errors = append(rep.Errors, AutoRollError{d.ContractID, d.AccountID, err.Error()})
			continue
		}
		rep.Rolled++
	}
	return rep, nil
}

// ---------------------------------------------------------------------------
// Handler — POST /api/v1/orders/roll (frozen route, orchestrator-wired)
// ---------------------------------------------------------------------------

// RollRequestBody is the REST payload for POST /api/v1/orders/roll.
// Decimal fields arrive as strings — JSON never carries float money
// (spec §5.3).
type RollRequestBody struct {
	ContractID      int64  `json:"contract_id"`
	NewValueDate    string `json:"new_value_date,omitempty"`     // RFC 3339 / YYYY-MM-DD
	Tenor           string `json:"tenor,omitempty"`              // e.g. "1M"
	MaxRollPriceBps string `json:"max_roll_price_bps,omitempty"` // decimal string
	IdempotencyKey  string `json:"idempotency_key,omitempty"`
	AccountID       int64  `json:"account_id,omitempty"` // must equal the caller's account
}

// RollHandler is the handler for the frozen POST /api/v1/orders/roll
// route (routes_v1.go Phase-22; the orchestrator binds it). Claims supply
// the trading account; a body account_id must match — no cross-account
// rolls.
func RollHandler(svc *RollService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqID := gateway.RequestIDFrom(r.Context())
		claims := auth.ClaimsFrom(r.Context())
		if claims == nil || claims.AccountID == 0 {
			api.WriteError(w, "UNAUTHORIZED", "authentication required", reqID, nil)
			return
		}
		var body RollRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			api.WriteError(w, "INVALID_REQUEST", "malformed body", reqID, nil)
			return
		}
		if body.AccountID != 0 && body.AccountID != claims.AccountID {
			api.WriteError(w, "FORBIDDEN", "account_id does not match the authenticated account", reqID, nil)
			return
		}
		req := RollRequest{
			AccountID:      claims.AccountID,
			ContractID:     body.ContractID,
			Tenor:          body.Tenor,
			IdempotencyKey: body.IdempotencyKey,
			Source:         RollSourceManual,
		}
		if body.NewValueDate != "" {
			d, err := time.Parse("2006-01-02", body.NewValueDate)
			if err != nil {
				if t, rerr := time.Parse(time.RFC3339, body.NewValueDate); rerr == nil {
					d = t
				} else {
					api.WriteError(w, "INVALID_REQUEST",
						"new_value_date must be YYYY-MM-DD or RFC3339", reqID, nil)
					return
				}
			}
			req.NewValueDate = &d
		}
		if body.MaxRollPriceBps != "" {
			b, err := decimal.NewFromString(body.MaxRollPriceBps)
			if err != nil {
				api.WriteError(w, "INVALID_REQUEST",
					"max_roll_price_bps must be a decimal string", reqID, nil)
				return
			}
			req.MaxRollPriceBps = &b
		}
		res, err := svc.Roll(r.Context(), req)
		if err != nil {
			var e *excerrors.Error
			code, msg := "INTERNAL_ERROR", "internal error"
			if stderrors.As(err, &e) {
				code, msg = e.Code, e.Message
			}
			api.WriteError(w, code, msg, reqID, nil)
			return
		}
		api.WriteJSON(w, http.StatusOK, res)
	}
}
