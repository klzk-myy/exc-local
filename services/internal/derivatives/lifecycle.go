// lifecycle.go — Option Lifecycle (Phase-22 Task 22.3.10; spec §15.4
// option lifecycle, §24 #158, #393).
//
// Lifecycle contract (spec §15.4):
//   - Premium: buyer pays writer at trade date + T+2 in premium_currency
//     through the GL/double-entry path (LedgerService.PostJournal inside
//     this service's SERIALIZABLE tx — never a raw balances UPDATE).
//     A failed debit books nothing partial, marks the settlement FAILED
//     and queues the Phase-19 Task 19.3.3 margin-call workflow
//     (PREMIUM_INSUFFICIENT, HTTP 400, §24 #393).
//   - Exercise cutoff: instructions accepted until 15:00 UTC on expiry
//     day (the expiry date nudged to a mutual business day via the
//     HolidayCalendar seam when it lands on a weekend/holiday);
//     post-cutoff instructions reject with EXERCISE_CUTOFF_PASSED (409).
//   - Auto-exercise: at expiry the batch evaluates every open holder
//     against the oracle mark of the underlying; options ≥0.5% ITM
//     auto-exercise (boundary inclusive), OTM expire worthless, the
//     ATM ±0.5% band follows the account's option_exercise_prefs
//     .auto_exercise_atm preference, and a do-not-exercise instruction
//     recorded before the cutoff suppresses auto-exercise.
//   - Assignment: exercised quantity is assigned to writers pro-rata by
//     open interest with a deterministic random tie-break (§15.4
//     amended); every assignment row logs timestamp, exercise_price,
//     assignment_price and margin_impact (AC #33) with the tie-break
//     seed stored for replay determinism (§15.7 item 3).
//   - Delivery: PHYSICAL books the underlying FX position through the
//     same VWAP merge math as position rolls; CASH books the intrinsic
//     payout; BINARY pays its fixed per-unit payout when ITM.
//   - Fail closed everywhere: missing/stale mark, insufficient writer
//     OI, margin-check or journal failure abort the transition — a
//     lifecycle batch never expires an unevaluated ITM candidate
//     silently; unprocessable rows record the error on the run row and
//     stay OPEN for the next sweep.
//
// Scheduler seam: OptionService.Run is the 15:00-UTC daily daemon
// (Phase-22 Task 22.3.10 scheduler; weekday-only per the 24/5 trading
// week — Sat/Sun have no expiries). RunExpiryDay is the single-shot
// entry point the orchestrator/cron binds instead when it owns
// scheduling.
package derivatives

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/audit"
	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	"exchange/internal/risk"
	"exchange/internal/settlement"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// Codes emitted by this file (registered in errs/codes.go).
const (
	// CodeExerciseCutoffPassed — §23: manual instruction after the
	// 15:00 UTC expiry cutoff (HTTP 409).
	CodeExerciseCutoffPassed = "EXERCISE_CUTOFF_PASSED"
	// CodeOptionNotExercisable — lifecycle state/style forbids exercise
	// (HTTP 409 — e.g. already EXERCISED/EXPIRED/ASSIGNED, or a EUROPEAN
	// before its expiry day).
	CodeOptionNotExercisable = "OPTION_NOT_EXERCISABLE"
	// CodeOptionContractInvalid — malformed contract registration
	// (HTTP 400).
	CodeOptionContractInvalid = "OPTION_CONTRACT_INVALID"
	// CodeOptionExerciseMarginShortfall — §23: exercising account fails
	// the IM check (HTTP 400).
	CodeOptionExerciseMarginShortfall = "OPTION_EXERCISE_MARGIN_SHORTFALL"
	// CodeOptionAssignmentFailed — writer OI cannot cover the exercise
	// (HTTP 409, fail-closed).
	CodeOptionAssignmentFailed = "OPTION_ASSIGNMENT_FAILED"
	// CodePremiumInsufficient — §24 #393: buyer lacks premium_currency at
	// the T+2 debit (HTTP 400 → margin-call workflow).
	CodePremiumInsufficient = "PREMIUM_INSUFFICIENT"
	// CodeOptionMarkUnavailable — missing/stale oracle mark halts the
	// transition (fail-closed).
	CodeOptionMarkUnavailable = "PRICE_ORACLE_UNAVAILABLE"
	// CodeExerciseAuctionBlocked — §24 #247: writer assignment blocked
	// while a §13.4 liquidation auction on the option instrument or its
	// underlying is live (HTTP 409).
	CodeExerciseAuctionBlocked = "OPTION_EXERCISE_AUCTION_BLOCKED"
	// CodeExerciseAuctionEvalFailed — the liquidation-auction guard could
	// not be evaluated; admission fails closed (HTTP 503).
	CodeExerciseAuctionEvalFailed = "EXERCISE_AUCTION_EVAL_FAILED"
)

// Exercise cutoff — spec §15.4: "Exercise instructions are accepted
// until 15:00 UTC on expiry day."
const exerciseCutoffHourUTC = 15

// AutoExerciseITMThresholdBps — spec §24 #158/§15.4: options at least
// 0.5% ITM auto-exercise (boundary inclusive).
const AutoExerciseITMThresholdBps = int64(50)

// Settlement modes (option_positions.settlement).
const (
	SettlementPhysical = "PHYSICAL"
	SettlementCash     = "CASH"
)

// Lifecycle states (option_positions.status).
const (
	OptStatusOpen      = "OPEN"
	OptStatusExercised = "EXERCISED"
	OptStatusAssigned  = "ASSIGNED"
	OptStatusExpired   = "EXPIRED"
)

// Pseudo-trade-id bit namespaces for position_fills rows written by the
// lifecycle path. Rolls own bits 61/60; exercise/assignment delivery legs
// own bits 59 (holder leg) and 58 (writer leg).
const (
	exerciseHolderTradeBit = uint64(1) << 59
	exerciseWriterTradeBit = uint64(1) << 58
)

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// BusinessCalendar is the mutual-business-day surface the lifecycle
// needs; *settlement.HolidayCalendar satisfies it. A nil calendar treats
// Mon–Fri as business days (weekend-only adjustment).
type BusinessCalendar interface {
	IsMutualBusinessDay(d time.Time, ccys ...string) bool
	NextMutualBusinessDay(d time.Time, ccys ...string) time.Time
}

// ExerciseMarginChecker is the Phase-22 Task 22.3.5 initial-margin seam:
// it answers whether accountID can carry the delivered spot position
// (required IM in quote currency). Returning ok=false aborts a MANUAL
// exercise with OPTION_EXERCISE_MARGIN_SHORTFALL; an AUTO exercise still
// delivers (the position is real) and the ExerciseLiquidator seam takes
// the account into the liquidation path (Task 22.3.14).
type ExerciseMarginChecker interface {
	// ExerciseMargin returns the IM required (quote currency) to carry
	// the delivery, and whether the account's free collateral covers it.
	ExerciseMargin(ctx context.Context, accountID, underlyingInstrumentID int64,
		qty, price decimal.Decimal) (required decimal.Decimal, ok bool, err error)
}

// ExerciseLiquidator is the Task 22.3.14 seam — called post-commit when
// an auto-exercise delivered into a margin-shortfall account.
type ExerciseLiquidator interface {
	LiquidateForExerciseShortfall(ctx context.Context, accountID int64) error
}

// ExerciseAuctionGuard reports the §13.4 liquidation auctions currently
// live. (*risk.PgLiquidationStore).ActiveAuctions satisfies it.
// Per Task 22.3.10 / §24 #247 writer assignment is blocked while an
// auction on the option instrument or its underlying is active —
// delivering mid-auction would worsen a liquidating account's position.
type ExerciseAuctionGuard interface {
	ActiveAuctions(ctx context.Context) ([]risk.AuctionRow, error)
}

// MarginCallQueuer is the Phase-19 Task 19.3.3 workflow seam — invoked
// post-commit when a premium debit fails (§24 #393).
type MarginCallQueuer interface {
	QueuePremiumShortfall(ctx context.Context, accountID, settlementID int64,
		currency string, amount decimal.Decimal) error
}

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// OptionRegistration registers one option-bearing positions row into the
// lifecycle ledger — the execution-path seam: the trade-settlement code
// calls this once per fill-created option position (holder row carries
// the writer identifiers so the T+2 premium transfer is enumerable).
type OptionRegistration struct {
	PositionID             int64
	AccountID              int64
	WriterPositionID       int64 // counterparty's option_positions-bearing position (0 → unresolved)
	WriterAccountID        int64
	UnderlyingInstrumentID int64 // 0 → resolve SPOT instrument on same pair
	OptionType             string
	ExerciseStyle          string
	Settlement             string // default PHYSICAL
	Strike                 decimal.Decimal
	Quantity               decimal.Decimal
	ExpiryAt               time.Time
	Premium                decimal.Decimal
	PremiumCurrency        string
	Payout                 decimal.Decimal // BINARY only
	TradeDate              time.Time       // premium due = TradeDate + T+2
}

// ExerciseReport summarizes one exercise transition.
type ExerciseReport struct {
	OptionPositionID int64
	AccountID        int64
	Source           string // MANUAL | AUTO
	Mode             string // PHYSICAL | CASH
	Quantity         decimal.Decimal
	Mark             decimal.Decimal
	ITMBps           decimal.Decimal
	Assignments      int
	JournalID        int64
	MarginShortfall  bool
}

// ExpiryRunReport summarizes one RunExpiryDay pass.
type ExpiryRunReport struct {
	RunID           int64 `json:"run_id"`
	Evaluated       int   `json:"positions_evaluated"`
	AutoExercised   int   `json:"auto_exercised"`
	Expired         int   `json:"expired"`
	Assignments     int   `json:"assignments"`
	PremiumsSettled int   `json:"premiums_settled"`
	PremiumsFailed  int   `json:"premiums_failed"`
	Errors          []string
	Replayed        bool
}

// optionRow is the in-tx option_positions + instrument + position view.
type optionRow struct {
	ID            int64
	PositionID    int64
	AccountID     int64
	InstrumentID  int64
	UnderlyingID  *int64
	Side          string
	OptionType    string
	ExerciseStyle string
	Settlement    string
	Strike        decimal.Decimal
	Quantity      decimal.Decimal
	ExpiryAt      time.Time
	Payout        *decimal.Decimal
	Status        string
	DoNotExercise bool
	// instrument join
	Symbol   string
	BaseCcy  string
	QuoteCcy string
	Lev      int64
	// underlying instrument join (nil-able)
	UndSymbol string
	UndLev    int64
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// OptionService runs the option lifecycle: premium settlement, manual
// exercise, the 15:00-UTC expiry batch and writer assignment.
type OptionService struct {
	pool        *pgxpool.Pool
	ls          *settlement.LedgerService
	spot        SpotSource
	locks       AccountLocker
	pub         eventPublisher
	cal         BusinessCalendar      // may be nil → weekday calendar
	margin      ExerciseMarginChecker // may be nil → check skipped (covered)
	liquidator  ExerciseLiquidator    // may be nil
	auctions    ExerciseAuctionGuard  // may be nil → §24 #247 gate unwired
	marginCalls MarginCallQueuer      // may be nil
	alerter     opsAlerter            // may be nil
	now         func() time.Time
	maxTries    int
	backoff     []time.Duration
	postedBy    string
}

// NewOptionService wires the lifecycle service. pool, ls, marks, locks
// and pub are required (fail-closed); cal, margin, liquidator,
// marginCalls and alerter are optional seams.
func NewOptionService(pool *pgxpool.Pool, ls *settlement.LedgerService,
	spot SpotSource, locks AccountLocker, pub eventPublisher,
	opts ...OptionServiceOption) (*OptionService, error) {
	switch {
	case pool == nil:
		return nil, fmt.Errorf("option lifecycle: nil pgx pool")
	case ls == nil:
		return nil, fmt.Errorf("option lifecycle: nil ledger service — premium/exercise postings are GL-mandatory")
	case spot == nil:
		return nil, fmt.Errorf("option lifecycle: nil mark source — ITM never evaluates off a fabricated quote")
	case locks == nil:
		return nil, fmt.Errorf("option lifecycle: nil account locker — §5.3 mutex backend required")
	case pub == nil:
		return nil, fmt.Errorf("option lifecycle: nil publisher — holder notification dispatch required")
	}
	s := &OptionService{
		pool: pool, ls: ls, spot: spot, locks: locks, pub: pub,
		now: time.Now, maxTries: 3,
		backoff:  []time.Duration{5 * time.Millisecond, 15 * time.Millisecond, 45 * time.Millisecond},
		postedBy: "derivatives-lifecycle",
	}
	for _, o := range opts {
		o(s)
	}
	return s, nil
}

// OptionServiceOption configures an optional seam.
type OptionServiceOption func(*OptionService)

// WithBusinessCalendar binds the HolidayCalendar seam.
func WithBusinessCalendar(c BusinessCalendar) OptionServiceOption {
	return func(s *OptionService) { s.cal = c }
}

// WithExerciseMarginChecker binds the Task 22.3.5 IM seam.
func WithExerciseMarginChecker(m ExerciseMarginChecker) OptionServiceOption {
	return func(s *OptionService) { s.margin = m }
}

// WithExerciseLiquidator binds the Task 22.3.14 liquidation seam.
func WithExerciseLiquidator(l ExerciseLiquidator) OptionServiceOption {
	return func(s *OptionService) { s.liquidator = l }
}

// WithExerciseAuctionGuard binds the §13.4 liquidation-auction gate on
// writer assignment (Task 22.3.10, §24 #247). nil → check unwired.
func WithExerciseAuctionGuard(g ExerciseAuctionGuard) OptionServiceOption {
	return func(s *OptionService) { s.auctions = g }
}

// WithMarginCallQueuer binds the Phase-19 Task 19.3.3 margin-call seam.
func WithMarginCallQueuer(q MarginCallQueuer) OptionServiceOption {
	return func(s *OptionService) { s.marginCalls = q }
}

// WithOptionOpsAlerter binds the ops-paging seam.
func WithOptionOpsAlerter(a opsAlerter) OptionServiceOption {
	return func(s *OptionService) { s.alerter = a }
}

// WithOptionClock injects the clock (tests; cutoff boundaries).
func WithOptionClock(now func() time.Time) OptionServiceOption {
	return func(s *OptionService) { s.now = now }
}

// ---------------------------------------------------------------------------
// Registration — the execution-path seam
// ---------------------------------------------------------------------------

// RegisterOptionPosition writes the option_positions lifecycle row (and
// the holder-side premium settlement row when the trade carries a
// premium) for a position opened by the matching/settlement path.
// premium_due_date = TradeDate + T+2 through the mutual-business-day
// calendar (§15.4, §24 #158).
func (s *OptionService) RegisterOptionPosition(ctx context.Context, reg OptionRegistration) (int64, error) {
	if reg.Settlement == "" {
		reg.Settlement = SettlementPhysical
	}
	if err := s.validateRegistration(reg); err != nil {
		return 0, err
	}
	var optID int64
	err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Verify the position exists, belongs to the account, and rides an
		// OPTION instrument; pull the pair for underlying resolution.
		var (
			instrID                    int64
			iType, base, quote, symbol string
			side                       string
		)
		err := tx.QueryRow(ctx, `
			SELECT i.id, i.instrument_type::text, i.base_currency, i.quote_currency,
			       i.symbol, p.side::text
			  FROM positions p JOIN instruments i ON i.id = p.instrument_id
			 WHERE p.id = $1 AND p.account_id = $2 AND p.quantity > 0`,
			reg.PositionID, reg.AccountID).Scan(&instrID, &iType, &base, &quote, &symbol, &side)
		if stderrors.Is(err, pgx.ErrNoRows) {
			return excerrors.New(CodeOptionContractInvalid, fmt.Sprintf(
				"position %d not found/open for account %d", reg.PositionID, reg.AccountID))
		}
		if err != nil {
			return fmt.Errorf("register option: position load: %w", err)
		}
		if iType != "OPTION" {
			return excerrors.New(CodeOptionContractInvalid, fmt.Sprintf(
				"instrument %s type %s is not OPTION", symbol, iType))
		}

		undID := reg.UnderlyingInstrumentID
		if undID == 0 && reg.Settlement == SettlementPhysical {
			// Resolve the deliverable spot instrument on the same pair —
			// fail closed when absent.
			err := tx.QueryRow(ctx, `
				SELECT id FROM instruments
				 WHERE instrument_type='SPOT' AND base_currency=$1 AND quote_currency=$2
				   AND status='ACTIVE' ORDER BY id LIMIT 1`,
				base, quote).Scan(&undID)
			if stderrors.Is(err, pgx.ErrNoRows) {
				return excerrors.New(CodeOptionContractInvalid, fmt.Sprintf(
					"no ACTIVE SPOT instrument on %s/%s — PHYSICAL settlement impossible",
					base, quote))
			}
			if err != nil {
				return fmt.Errorf("register option: underlying resolve: %w", err)
			}
		}

		premiumCcy := reg.PremiumCurrency
		dueDate := mutualBusinessDayOnOrAfter(s.cal,
			reg.TradeDate.UTC().Truncate(24*time.Hour).AddDate(0, 0, 2), base, quote)
		var writerPosID *int64
		if reg.WriterPositionID > 0 {
			writerPosID = &reg.WriterPositionID
		}
		var payout *decimal.Decimal
		if reg.OptionType == "BINARY" {
			p := reg.Payout
			payout = &p
		}
		err = tx.QueryRow(ctx, `
			INSERT INTO option_positions
			    (position_id, account_id, instrument_id, underlying_instrument_id,
			     side, option_type, exercise_style, settlement, strike, quantity,
			     expiry_at, premium, premium_currency, premium_status,
			     premium_due_date, payout)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9::numeric,$10::numeric,$11,
			        $12::numeric,$13,
			        CASE WHEN $12::numeric > 0 THEN 'PENDING' ELSE 'SETTLED' END,
			        $14, $15::numeric)
			RETURNING id`,
			reg.PositionID, reg.AccountID, instrID, nullableID(undID),
			side, reg.OptionType, reg.ExerciseStyle, reg.Settlement,
			reg.Strike.String(), reg.Quantity.String(), reg.ExpiryAt.UTC(),
			reg.Premium.String(), premiumCcy, dueDate, decPtrText(payout)).Scan(&optID)
		if err != nil {
			return fmt.Errorf("register option: insert: %w", err)
		}

		// Holder row carries the premium-owed ledger line.
		if side == "LONG" && reg.Premium.IsPositive() {
			if reg.WriterAccountID <= 0 {
				return excerrors.New(CodeOptionContractInvalid,
					"premium-bearing holder registration requires writer_account_id")
			}
			var wpID *int64
			if writerPosID != nil {
				// Resolve the writer's option_positions row when the
				// counterparty position was registered first; NULLable —
				// the sweep does not require it.
				var wid int64
				werr := tx.QueryRow(ctx,
					`SELECT id FROM option_positions WHERE position_id = $1`,
					*writerPosID).Scan(&wid)
				if werr == nil {
					wpID = &wid
				}
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO option_premium_settlements
				    (holder_position_id, writer_position_id, holder_account_id,
				     writer_account_id, amount, currency, due_date)
				VALUES ($1,$2,$3,$4,$5,$6,$7)`,
				optID, wpID, reg.AccountID, reg.WriterAccountID,
				reg.Premium, premiumCcy, dueDate); err != nil {
				return fmt.Errorf("register option: premium row: %w", err)
			}
		}
		if _, err := audit.Append(ctx, tx, "option_positions", &optID, "INSERT", nil); err != nil {
			return fmt.Errorf("register option: audit: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return optID, nil
}

func (s *OptionService) validateRegistration(reg OptionRegistration) error {
	bad := func(f string, a ...any) error {
		return excerrors.New(CodeOptionContractInvalid, fmt.Sprintf(f, a...))
	}
	if reg.PositionID <= 0 || reg.AccountID <= 0 {
		return bad("position_id and account_id are required")
	}
	switch reg.OptionType {
	case "CALL", "PUT":
	case "BINARY":
		if !reg.Payout.IsPositive() {
			return bad("BINARY requires a positive payout")
		}
	default:
		return bad("option_type must be CALL|PUT|BINARY, got %q", reg.OptionType)
	}
	switch reg.ExerciseStyle {
	case "EUROPEAN", "AMERICAN":
	default:
		return bad("exercise_style must be EUROPEAN|AMERICAN, got %q", reg.ExerciseStyle)
	}
	switch reg.Settlement {
	case SettlementPhysical, SettlementCash:
	default:
		return bad("settlement must be PHYSICAL|CASH, got %q", reg.Settlement)
	}
	if !reg.Strike.IsPositive() {
		return bad("strike must be positive")
	}
	if !reg.Quantity.IsPositive() {
		return bad("quantity must be positive")
	}
	if reg.ExpiryAt.IsZero() {
		return bad("expiry_at required")
	}
	if !reg.ExpiryAt.After(s.now()) {
		return bad("expiry_at %s is not in the future", reg.ExpiryAt.UTC())
	}
	if reg.Premium.IsNegative() {
		return bad("premium cannot be negative")
	}
	if reg.Premium.IsPositive() && len(reg.PremiumCurrency) != 3 {
		return bad("premium_currency (ISO-4217) required when premium > 0")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cutoff + ITM — pure math (deterministic, decimal-only)
// ---------------------------------------------------------------------------

// ExerciseCutoffDay returns the adjusted expiry day: the UTC date of
// expiryAt shifted forward to the next mutual business day for the pair
// when it lands on a weekend/holiday (§15.4 expiry nudge).
func ExerciseCutoffDay(cal BusinessCalendar, expiryAt time.Time, baseCcy, quoteCcy string) time.Time {
	day := expiryAt.UTC().Truncate(24 * time.Hour)
	return mutualBusinessDayOnOrAfter(cal, day, baseCcy, quoteCcy)
}

// ExerciseCutoff is the 15:00 UTC instant of the adjusted expiry day.
func ExerciseCutoff(cal BusinessCalendar, expiryAt time.Time, baseCcy, quoteCcy string) time.Time {
	return ExerciseCutoffDay(cal, expiryAt, baseCcy, quoteCcy).
		Add(exerciseCutoffHourUTC * time.Hour)
}

func mutualBusinessDayOnOrAfter(cal BusinessCalendar, day time.Time, ccys ...string) time.Time {
	day = day.UTC().Truncate(24 * time.Hour)
	for {
		if isMutualBizDay(cal, day, ccys) {
			return day
		}
		day = day.AddDate(0, 0, 1)
	}
}

func isMutualBizDay(cal BusinessCalendar, day time.Time, ccys []string) bool {
	if cal == nil {
		wd := day.Weekday()
		return wd != time.Saturday && wd != time.Sunday
	}
	return cal.IsMutualBusinessDay(day, ccys...)
}

// ITMBps returns the in-the-money distance in basis points against the
// mark. CALL: (mark−strike)/strike; PUT: (strike−mark)/mark; BINARY
// evaluates like CALL (spec §15.4 binary = "strike above/below mark",
// CALL semantics). Negative = OTM. Exactly +50 bps is ITM.
func ITMBps(optionType string, strike, mark decimal.Decimal) (decimal.Decimal, error) {
	if !strike.IsPositive() || !mark.IsPositive() {
		return decimal.Zero, excerrors.New(CodeOptionContractInvalid,
			"strike and mark must be positive for ITM evaluation")
	}
	ten := decimal.NewFromInt(10000)
	switch optionType {
	case "CALL", "BINARY":
		return mark.Sub(strike).Div(strike).Mul(ten), nil
	case "PUT":
		return strike.Sub(mark).Div(mark).Mul(ten), nil
	}
	return decimal.Zero, excerrors.New(CodeOptionContractInvalid,
		fmt.Sprintf("option_type %q has no ITM rule", optionType))
}

// ---------------------------------------------------------------------------
// Manual exercise + instructions
// ---------------------------------------------------------------------------

// ManualExercise exercises the holder's whole option position. Rules
// (§15.4): the instruction is accepted until 15:00 UTC on the adjusted
// expiry day (post-cutoff → EXERCISE_CUTOFF_PASSED); EUROPEAN options
// additionally require the expiry day itself; a do-not-exercise flag
// does NOT block an explicit instruction — it only suppresses
// auto-exercise. Delivery is atomic with writer assignment.
func (s *OptionService) ManualExercise(ctx context.Context, accountID, optionPositionID int64) (*ExerciseReport, error) {
	now := s.now().UTC()
	var rep *ExerciseReport
	var events []ledger.BalanceEvent
	var lastErr error
	for attempt := 0; attempt < s.maxTries; attempt++ {
		rep, events, lastErr = s.exerciseAttempt(ctx, optionPositionID, accountID, "MANUAL", now)
		if lastErr == nil {
			break
		}
		if !isSerializationConflict(lastErr) {
			return nil, lastErr
		}
		if attempt+1 < s.maxTries {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(s.backoff[min(attempt, len(s.backoff)-1)]):
			}
		}
	}
	if lastErr != nil {
		return nil, excerrors.Wrap(ledger.CodeTxnConflictExhausted,
			fmt.Sprintf("manual exercise: aborted after %d attempts", s.maxTries), lastErr)
	}
	if err := s.dispatchExerciseEvents(ctx, events, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// exerciseAttempt locks holder + writer accounts, then runs the
// transition in one SERIALIZABLE transaction.
func (s *OptionService) exerciseAttempt(ctx context.Context, optionPositionID, accountID int64,
	source string, now time.Time) (*ExerciseReport, []ledger.BalanceEvent, error) {

	// Fast-path validation + account set discovery (read-only).
	opt, err := s.loadOption(ctx, optionPositionID)
	if err != nil {
		return nil, nil, err
	}
	if opt == nil {
		return nil, nil, excerrors.New(CodeOptionNotExercisable,
			fmt.Sprintf("option position %d not found", optionPositionID))
	}
	if err := s.checkExerciseAuction(ctx, opt); err != nil {
		return nil, nil, err
	}
	if source == "MANUAL" {
		if opt.AccountID != accountID || opt.Side != "LONG" {
			return nil, nil, excerrors.New(CodeOptionNotExercisable,
				fmt.Sprintf("option %d is not a holder position of account %d", optionPositionID, accountID))
		}
		if err := checkExerciseWindow(opt, now); err != nil {
			return nil, nil, err
		}
	}

	// Lock holder + every open writer account (sorted) under §5.3 mutex.
	writerAccts, err := s.openWriterAccounts(ctx, opt.InstrumentID)
	if err != nil {
		return nil, nil, err
	}
	accts := append(append([]int64(nil), writerAccts...), opt.AccountID)
	token := fmt.Sprintf("exercise-%d", s.now().UnixNano())
	locked, err := s.locks.LockAccounts(ctx, accts, token)
	if err != nil {
		return nil, nil, err
	}
	defer s.locks.UnlockAccounts(locked, token)

	var rep *ExerciseReport
	var events []ledger.BalanceEvent
	err = s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rep, events, err = s.exerciseInTx(ctx, tx, opt, source, now)
		return err
	})
	return rep, events, err
}

// checkExerciseWindow enforces the style window + 15:00 UTC cutoff for a
// manual instruction (pure — exercised with the row re-loaded in-tx).
func checkExerciseWindow(opt *optionRow, now time.Time) error {
	cutoff := ExerciseCutoff(nil, opt.ExpiryAt, opt.BaseCcy, opt.QuoteCcy)
	// NOTE: the cutoff day is calendar-adjusted inside the tx via the
	// service calendar; this fast check uses the weekday rule only as a
	// pre-flight — exerciseInTx repeats it authoritatively.
	if !now.Before(cutoff) {
		return excerrors.New(CodeExerciseCutoffPassed, fmt.Sprintf(
			"exercise cutoff 15:00 UTC on expiry day %s passed (now %s)",
			cutoff.Format("2006-01-02"), now.Format(time.RFC3339)))
	}
	if opt.ExerciseStyle == "EUROPEAN" {
		expiryDay := ExerciseCutoffDay(nil, opt.ExpiryAt, opt.BaseCcy, opt.QuoteCcy)
		if now.Before(expiryDay) {
			return excerrors.New(CodeOptionNotExercisable, fmt.Sprintf(
				"EUROPEAN option %d is exercisable on expiry day %s only",
				opt.ID, expiryDay.Format("2006-01-02")))
		}
	}
	return nil
}

// checkExerciseAuction enforces §24 #247: writer assignment is blocked
// while a §13.4 liquidation auction on the option instrument or its
// underlying is live — delivering mid-auction would worsen a
// liquidating account's position. Evaluated per attempt (serialization
// retries re-check it). A guard read failure is indistinguishable from
// an unverifiable auction state → fails closed. A nil guard means the
// gate is unwired (test/deploy environments without a liquidation
// store) and skips.
func (s *OptionService) checkExerciseAuction(ctx context.Context, opt *optionRow) error {
	if s.auctions == nil {
		return nil
	}
	live, err := s.auctions.ActiveAuctions(ctx)
	if err != nil {
		return excerrors.Wrap(CodeExerciseAuctionEvalFailed,
			fmt.Sprintf("exercise %d: liquidation-auction guard unreadable", opt.ID), err)
	}
	for _, a := range live {
		if a.InstrumentID == opt.InstrumentID ||
			(opt.UnderlyingID != nil && a.InstrumentID == *opt.UnderlyingID) {
			return excerrors.New(CodeExerciseAuctionBlocked, fmt.Sprintf(
				"exercise %d blocked: liquidation auction %d (%s) active on instrument %d",
				opt.ID, a.ID, a.Phase, a.InstrumentID))
		}
	}
	return nil
}

// exerciseInTx performs the full exercise+assignment atomically.
func (s *OptionService) exerciseInTx(ctx context.Context, tx pgx.Tx,
	opt *optionRow, source string, now time.Time) (*ExerciseReport, []ledger.BalanceEvent, error) {

	// Re-load the holder row FOR UPDATE — authoritative state check.
	cur, err := lockOptionRow(ctx, tx, opt.ID)
	if err != nil {
		return nil, nil, err
	}
	if cur.Status != OptStatusOpen || cur.Side != "LONG" {
		return nil, nil, excerrors.New(CodeOptionNotExercisable, fmt.Sprintf(
			"option %d is %s (side %s) — not exercisable", cur.ID, cur.Status, cur.Side))
	}
	cutoff := ExerciseCutoff(s.cal, cur.ExpiryAt, cur.BaseCcy, cur.QuoteCcy)
	if source == "MANUAL" {
		if !now.Before(cutoff) {
			return nil, nil, excerrors.New(CodeExerciseCutoffPassed, fmt.Sprintf(
				"exercise cutoff 15:00 UTC on expiry day %s passed (now %s)",
				cutoff.Format("2006-01-02"), now.Format(time.RFC3339)))
		}
		if cur.ExerciseStyle == "EUROPEAN" &&
			now.Before(ExerciseCutoffDay(s.cal, cur.ExpiryAt, cur.BaseCcy, cur.QuoteCcy)) {
			return nil, nil, excerrors.New(CodeOptionNotExercisable,
				"EUROPEAN option exercisable on expiry day only")
		}
	} else {
		// AUTO at the expiry batch: a do-not-exercise instruction recorded
		// pre-cutoff suppresses auto-exercise (handled by the sweep — the
		// guard here is defense-in-depth).
		if cur.DoNotExercise {
			return nil, nil, excerrors.New(CodeOptionNotExercisable,
				"do-not-exercise instruction recorded before cutoff")
		}
	}

	// Mark — fail closed on absent/stale.
	markSymbol := cur.UndSymbol
	if markSymbol == "" {
		markSymbol = cur.Symbol
	}
	mark, err := s.freshOptMark(ctx, markSymbol)
	if err != nil {
		return nil, nil, err
	}
	itmBps, err := ITMBps(cur.OptionType, cur.Strike, mark)
	if err != nil {
		return nil, nil, err
	}

	// Margin check (holder must be able to carry the delivered spot leg).
	marginShortfall := false
	if s.margin != nil && cur.UnderlyingID != nil {
		_, ok, err := s.margin.ExerciseMargin(ctx, cur.AccountID, *cur.UnderlyingID,
			cur.Quantity, cur.Strike)
		if err != nil {
			return nil, nil, fmt.Errorf("exercise margin check: %w", err)
		}
		if !ok {
			if source == "MANUAL" {
				return nil, nil, excerrors.New(CodeOptionExerciseMarginShortfall,
					"insufficient free collateral to carry the exercised position")
			}
			marginShortfall = true // AUTO: deliver anyway, liquidate after
		}
	}

	// Writers: pro-rata assignment by open interest, deterministic seed.
	writers, err := lockWriters(ctx, tx, cur.InstrumentID)
	if err != nil {
		return nil, nil, err
	}
	var oi decimal.Decimal
	for _, w := range writers {
		oi = oi.Add(w.Quantity)
	}
	if oi.LessThan(cur.Quantity) {
		return nil, nil, excerrors.New(CodeOptionAssignmentFailed, fmt.Sprintf(
			"writer open interest %s < exercise qty %s on instrument %d",
			oi, cur.Quantity, cur.InstrumentID))
	}
	seed := assignmentSeed(cur.ID, cur.Quantity, cur.ExpiryAt)
	slices := assignProRata(writers, cur.Quantity, seed)

	// Journal + effects assembly.
	intrinsicPerUnit := decimal.Zero
	switch cur.OptionType {
	case "CALL", "BINARY":
		intrinsicPerUnit = decMax(decimal.Zero, mark.Sub(cur.Strike))
	case "PUT":
		intrinsicPerUnit = decMax(decimal.Zero, cur.Strike.Sub(mark))
	}
	var payoutPerUnit decimal.Decimal
	if cur.OptionType == "BINARY" {
		if cur.Payout == nil || !cur.Payout.IsPositive() {
			return nil, nil, excerrors.New(CodeOptionContractInvalid,
				"BINARY exercise requires a positive payout")
		}
		payoutPerUnit = *cur.Payout
	} else {
		payoutPerUnit = intrinsicPerUnit
	}

	var events []ledger.BalanceEvent
	assignments := 0
	lev := decimal.NewFromInt(cur.UndLev)
	if cur.UndLev <= 0 {
		lev = decimal.NewFromInt(1)
	}

	for i, w := range writers {
		slice := slices[i]
		if slice.IsZero() {
			continue
		}
		assignments++
		wSliceNotional := cur.Strike.Mul(slice)
		marginImpact := wSliceNotional.Div(lev).Round(8)

		// Assignment row (AC #33 fields).
		var assignmentID int64
		assignmentPrice := cur.Strike
		if cur.Settlement == SettlementCash {
			assignmentPrice = mark
		}
		err := tx.QueryRow(ctx, `
			INSERT INTO option_assignments
			    (holder_position_id, holder_account_id, writer_position_id,
			     writer_account_id, instrument_id, quantity, exercise_price,
			     assignment_price, mark_at_expiry, margin_impact,
			     margin_shortfall, mode, source, assignment_seed)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			RETURNING id`,
			cur.ID, cur.AccountID, w.ID, w.AccountID, cur.InstrumentID,
			slice, cur.Strike, assignmentPrice, mark, marginImpact,
			marginShortfall, cur.Settlement, source, seed).Scan(&assignmentID)
		if err != nil {
			return nil, nil, fmt.Errorf("exercise: assignment row: %w", err)
		}
		if _, err := audit.Append(ctx, tx, "option_assignments", &assignmentID, "INSERT", nil); err != nil {
			return nil, nil, fmt.Errorf("exercise: assignment audit: %w", err)
		}

		// Writer option inventory burns down.
		w.Quantity = w.Quantity.Sub(slice)
		newStatus := OptStatusOpen
		if w.Quantity.IsZero() {
			newStatus = OptStatusAssigned
		}
		if _, err := tx.Exec(ctx, `
			UPDATE option_positions SET quantity=$2, status=$3, updated_at=now()
			 WHERE id=$1`, w.ID, w.Quantity, newStatus); err != nil {
			return nil, nil, fmt.Errorf("exercise: writer %d update: %w", w.ID, err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE positions SET quantity=$2, updated_at=now() WHERE id=$1`,
			w.PositionID, w.Quantity); err != nil {
			return nil, nil, fmt.Errorf("exercise: writer position %d: %w", w.PositionID, err)
		}

		// Delivery legs + wallet journal for this slice.
		sliceJournal, hasMoney, err := s.deliverSlice(ctx, tx, deliverParams{
			AssignmentID:   assignmentID,
			Opt:            cur,
			Writer:         w,
			Slice:          slice,
			Mark:           mark,
			StrikeNotional: wSliceNotional,
			PayoutPerUnit:  payoutPerUnit,
			PostedBy:       s.postedBy,
		})
		if err != nil {
			return nil, nil, err
		}
		if hasMoney {
			pr, err := s.ls.PostJournal(ctx, tx, sliceJournal)
			if err != nil {
				return nil, nil, err // coded — INSUFFICIENT_BALANCE etc.
			}
			events = append(events, pr.Events...)
			if _, err := tx.Exec(ctx,
				`UPDATE option_assignments SET gl_journal_id=$2 WHERE id=$1`,
				assignmentID, pr.JournalID); err != nil {
				return nil, nil, fmt.Errorf("exercise: assignment journal link: %w", err)
			}
		}
	}

	// Holder option → EXERCISED; holder positions row burns to zero.
	if _, err := tx.Exec(ctx, `
		UPDATE option_positions SET status='EXERCISED', quantity=0,
		       exercise_mark=$2, exercised_at=now(), updated_at=now()
		 WHERE id=$1`, cur.ID, mark); err != nil {
		return nil, nil, fmt.Errorf("exercise: holder update: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE positions SET quantity=0, unrealized_pnl=0, updated_at=now()
		 WHERE id=$1`, cur.PositionID); err != nil {
		return nil, nil, fmt.Errorf("exercise: holder position %d: %w", cur.PositionID, err)
	}
	if _, err := audit.Append(ctx, tx, "option_positions", &cur.ID, "UPDATE", nil); err != nil {
		return nil, nil, fmt.Errorf("exercise: holder audit: %w", err)
	}

	return &ExerciseReport{
		OptionPositionID: cur.ID, AccountID: cur.AccountID,
		Source: source, Mode: cur.Settlement, Quantity: cur.Quantity,
		Mark: mark, ITMBps: itmBps, Assignments: assignments,
		MarginShortfall: marginShortfall,
	}, events, nil
}

// deliverParams carries one pro-rata slice into the delivery builder.
type deliverParams struct {
	AssignmentID   int64
	Opt            *optionRow
	Writer         *optionRow
	Slice          decimal.Decimal
	Mark           decimal.Decimal
	StrikeNotional decimal.Decimal
	PayoutPerUnit  decimal.Decimal
	PostedBy       string
}

// deliverSlice books the holder/writer delivery for one assignment slice
// and returns the balanced GL journal for it.
//   - PHYSICAL: underlying positions mutate at the strike (holder long/
//     short the delivered base, writer opposite); the journal reallocates
//     quote (holder pays writer strike·slice) and base (writer delivers)
//     client liabilities.
//   - CASH/BINARY: the writer pays slice·payout_per_unit in the quote
//     currency; no position leg is delivered.
func (s *OptionService) deliverSlice(ctx context.Context, tx pgx.Tx, p deliverParams) (ledger.Journal, bool, error) {
	opt := p.Opt
	j := ledger.Journal{
		EntryType:      ledger.EntrySettlement,
		ReferenceID:    p.AssignmentID,
		PostedBy:       p.PostedBy,
		IdempotencyKey: fmt.Sprintf("option-exercise:%d", p.AssignmentID),
	}
	if opt.Settlement == SettlementPhysical {
		if opt.UnderlyingID == nil {
			return ledger.Journal{}, false, excerrors.New(CodeOptionContractInvalid,
				"PHYSICAL settlement has no underlying instrument")
		}
		// Holder receives base for CALL / delivers base for PUT.
		holderSide := "LONG"
		writerSide := "SHORT"
		holderPaysQuote := true
		if opt.OptionType == "PUT" {
			holderSide, writerSide = "SHORT", "LONG"
			holderPaysQuote = false
		}
		// Position legs at the strike — the netting merge math.
		holderPos, err := lockInstrumentPosition(ctx, tx, opt.AccountID, *opt.UnderlyingID, holderSide)
		if err != nil {
			return ledger.Journal{}, false, err
		}
		applyExerciseLeg(holderPos, holderSide, p.Slice, opt.Strike)
		mk := p.Mark
		if err := upsertOptPosition(ctx, tx, holderPos, &mk); err != nil {
			return ledger.Journal{}, false, err
		}
		if err := recordOptFill(ctx, tx, uint64(p.AssignmentID)|exerciseHolderTradeBit,
			holderPos, buySell(holderSide), p.Slice, opt.Strike, decimal.Zero); err != nil {
			return ledger.Journal{}, false, err
		}
		writerPos, err := lockInstrumentPosition(ctx, tx, p.Writer.AccountID, *opt.UnderlyingID, writerSide)
		if err != nil {
			return ledger.Journal{}, false, err
		}
		applyExerciseLeg(writerPos, writerSide, p.Slice, opt.Strike)
		if err := upsertOptPosition(ctx, tx, writerPos, &mk); err != nil {
			return ledger.Journal{}, false, err
		}
		if err := recordOptFill(ctx, tx, uint64(p.AssignmentID)|exerciseWriterTradeBit,
			writerPos, buySell(writerSide), p.Slice, opt.Strike, decimal.Zero); err != nil {
			return ledger.Journal{}, false, err
		}

		// Journal: quote leg holder↔writer + base leg writer→holder.
		// DR/CR on the same 2010 code reallocates client liabilities —
		// the per-account movement lives in Effects (balance_service
		// convention).
		j.Description = fmt.Sprintf(
			"option exercise assignment=%d %s %s qty=%s strike=%s PHYSICAL",
			p.AssignmentID, opt.OptionType, opt.Symbol, p.Slice, opt.Strike)
		quote := opt.QuoteCcy
		base := opt.BaseCcy
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.CustomerLiability(quote), quote, p.StrikeNotional,
				"exercise quote leg"),
			ledger.CreditLine(ledger.CustomerLiability(quote), quote, p.StrikeNotional,
				"exercise quote leg"),
			ledger.DebitLine(ledger.CustomerLiability(base), base, p.Slice,
				"exercise base leg"),
			ledger.CreditLine(ledger.CustomerLiability(base), base, p.Slice,
				"exercise base leg"),
		)
		holderQuote, writerQuote := p.StrikeNotional.Neg(), p.StrikeNotional
		holderBase, writerBase := p.Slice, p.Slice.Neg()
		if !holderPaysQuote {
			holderQuote, writerQuote = writerQuote, holderQuote
			holderBase, writerBase = writerBase, holderBase
		}
		j.Effects = []ledger.AccountEffect{
			{AccountID: opt.AccountID, Currency: quote, AvailableDelta: holderQuote},
			{AccountID: p.Writer.AccountID, Currency: quote, AvailableDelta: writerQuote},
			{AccountID: opt.AccountID, Currency: base, AvailableDelta: holderBase},
			{AccountID: p.Writer.AccountID, Currency: base, AvailableDelta: writerBase},
		}
	} else {
		// CASH / BINARY — writer pays slice·payout_per_unit in quote ccy.
		payout := p.PayoutPerUnit.Mul(p.Slice)
		if !payout.IsPositive() {
			return ledger.Journal{}, false, excerrors.New(CodeOptionContractInvalid,
				"cash-settled exercise has zero payout — should have expired OTM")
		}
		j.Description = fmt.Sprintf(
			"option exercise assignment=%d %s %s qty=%s CASH payout=%s %s",
			p.AssignmentID, opt.OptionType, opt.Symbol, p.Slice, payout, opt.QuoteCcy)
		j.Lines = append(j.Lines,
			ledger.DebitLine(ledger.CustomerLiability(opt.QuoteCcy), opt.QuoteCcy, payout,
				"writer pays cash settlement"),
			ledger.CreditLine(ledger.CustomerLiability(opt.QuoteCcy), opt.QuoteCcy, payout,
				"holder receives cash settlement"),
		)
		j.Effects = []ledger.AccountEffect{
			{AccountID: opt.AccountID, Currency: opt.QuoteCcy, AvailableDelta: payout},
			{AccountID: p.Writer.AccountID, Currency: opt.QuoteCcy, AvailableDelta: payout.Neg()},
		}
	}
	if err := j.Validate(); err != nil {
		return ledger.Journal{}, false, fmt.Errorf("exercise journal invalid: %w", err)
	}
	return j, true, nil
}

func buySell(side string) string {
	if side == "SHORT" {
		return "SELL"
	}
	return "BUY"
}

// DoNotExercise sets/clears the holder's do-not-exercise instruction —
// amendable until the 15:00 UTC cutoff; at/after cutoff the row is
// closed to instructions (EXERCISE_CUTOFF_PASSED).
func (s *OptionService) DoNotExercise(ctx context.Context, accountID, optionPositionID int64, on bool) error {
	now := s.now().UTC()
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		opt, err := lockOptionRow(ctx, tx, optionPositionID)
		if err != nil {
			return err
		}
		if opt.AccountID != accountID || opt.Side != "LONG" {
			return excerrors.New(CodeOptionNotExercisable, fmt.Sprintf(
				"option %d is not a holder position of account %d", optionPositionID, accountID))
		}
		if opt.Status != OptStatusOpen {
			return excerrors.New(CodeOptionNotExercisable, fmt.Sprintf(
				"option %d already %s", optionPositionID, opt.Status))
		}
		cutoff := ExerciseCutoff(s.cal, opt.ExpiryAt, opt.BaseCcy, opt.QuoteCcy)
		if !now.Before(cutoff) {
			return excerrors.New(CodeExerciseCutoffPassed,
				"instructions closed at the 15:00 UTC expiry cutoff")
		}
		if _, err := tx.Exec(ctx, `
			UPDATE option_positions SET do_not_exercise=$2, updated_at=now()
			 WHERE id=$1`, optionPositionID, on); err != nil {
			return fmt.Errorf("do-not-exercise update: %w", err)
		}
		_, err = audit.Append(ctx, tx, "option_positions", &optionPositionID, "UPDATE", nil)
		return err
	})
}

// SetExercisePref upserts the account's auto_exercise_atm preference.
func (s *OptionService) SetExercisePref(ctx context.Context, accountID int64, autoExerciseATM bool) error {
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO option_exercise_prefs (account_id, auto_exercise_atm, updated_at)
			VALUES ($1,$2,now())
			ON CONFLICT (account_id) DO UPDATE SET auto_exercise_atm=$2, updated_at=now()`,
			accountID, autoExerciseATM); err != nil {
			return fmt.Errorf("exercise pref upsert: %w", err)
		}
		_, err := audit.Append(ctx, tx, "option_exercise_prefs", &accountID, "UPDATE", nil)
		return err
	})
}

// ---------------------------------------------------------------------------
// Expiry batch — 15:00 UTC daily (spec §15.4 / §24 #158)
// ---------------------------------------------------------------------------

// RunExpiryDay processes every option whose adjusted expiry day is <=
// runDate (UTC): holders ≥0.5% ITM auto-exercise, the ATM band follows
// the account preference, do-not-exercise rows and OTM rows expire, and
// residual writer inventory expires once its holders are done. Due
// premium settlements are swept in the same pass. The run is deduplicated
// by run_date (option_expiry_runs UNIQUE — rollover_runs pattern) so a
// replayed scheduler tick resolves idempotently.
func (s *OptionService) RunExpiryDay(ctx context.Context, runDate time.Time) (*ExpiryRunReport, error) {
	day := runDate.UTC().Truncate(24 * time.Hour)
	runID, fresh, err := s.claimExpiryRun(ctx, day)
	if err != nil {
		return nil, err
	}
	if !fresh {
		return s.loadExpiryRun(ctx, runID)
	}
	rep := &ExpiryRunReport{RunID: runID}

	// 1. Due premiums (trade date + T+2, §24 #158/#393).
	settled, failed, perr := s.runPremiumSweep(ctx, day)
	rep.PremiumsSettled, rep.PremiumsFailed = settled, failed
	if perr != nil {
		rep.Errors = append(rep.Errors, perr.Error())
	}

	// 2. Expiry evaluation — holders first, writers' residual after.
	due, err := s.dueOptions(ctx, day)
	if err != nil {
		s.failExpiryRun(ctx, runID, err)
		return rep, err
	}
	rep.Evaluated = len(due)
	for _, d := range due {
		outcome, err := s.evaluateAtExpiry(ctx, d, day)
		switch {
		case err != nil:
			rep.Errors = append(rep.Errors,
				fmt.Sprintf("option %d: %v", d.ID, err))
		case outcome == "EXERCISED":
			rep.AutoExercised++
		case outcome == "EXPIRED":
			rep.Expired++
		}
	}
	// Residual writer inventory expires at end of expiry day.
	wExpired, err := s.expireResidualWriters(ctx, day)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
	}
	rep.Expired += wExpired

	// Persist run counters (COMPLETED even with row-level errors — the
	// error text is the ops signal; rows stay OPEN for the next run).
	if err := s.completeExpiryRun(ctx, runID, rep); err != nil {
		return rep, err
	}
	return rep, nil
}

// evaluateAtExpiry decides one holder option's fate at expiry:
// EXERCISED (auto) / EXPIRED / "" (skipped — error reported upstream).
// Pure decision → single-purpose tx per row so one failure never strands
// the batch.
func (s *OptionService) evaluateAtExpiry(ctx context.Context, d optionRow, day time.Time) (string, error) {
	// do-not-exercise recorded pre-cutoff → expire worthless (§15.4).
	if d.DoNotExercise {
		return "EXPIRED", s.expireOption(ctx, d.ID, "do-not-exercise instruction")
	}
	markSymbol := d.UndSymbol
	if markSymbol == "" {
		markSymbol = d.Symbol
	}
	mark, err := s.freshOptMark(ctx, markSymbol)
	if err != nil {
		return "", err // fail-closed: stays OPEN for the next sweep
	}
	itmBps, err := ITMBps(d.OptionType, d.Strike, mark)
	if err != nil {
		return "", err
	}
	threshold := decimal.NewFromInt(AutoExerciseITMThresholdBps)
	itm := itmBps.GreaterThanOrEqual(threshold)
	if !itm && d.OptionType == "BINARY" {
		itm = itmBps.IsPositive() // binary: any ITM pays; <=0 expires
	}
	if !itm {
		// ATM ±0.5% band → account preference (§15.4).
		if itmBps.Abs().LessThan(threshold) && d.OptionType != "BINARY" {
			pref, err := s.exercisePref(ctx, d.AccountID)
			if err != nil {
				return "", err
			}
			itm = pref
		}
	}
	if !itm {
		return "EXPIRED", s.expireOption(ctx, d.ID, fmt.Sprintf(
			"OTM at expiry: %s bps vs mark %s", itmBps.Round(4), mark))
	}
	if _, _, err := s.exerciseAttempt(ctx, d.ID, d.AccountID, "AUTO", s.now().UTC()); err != nil {
		return "", err
	}
	return "EXERCISED", nil
}

// expireOption marks one holder option EXPIRED (OTM/DNE) — the
// zero-value terminal transition.
func (s *OptionService) expireOption(ctx context.Context, optionID int64, reason string) error {
	return s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE option_positions SET status='EXPIRED', updated_at=now()
			 WHERE id=$1 AND status='OPEN'`, optionID)
		if err != nil {
			return fmt.Errorf("expire option %d: %w", optionID, err)
		}
		if tag.RowsAffected() == 0 {
			return nil // concurrently transitioned — idempotent
		}
		if _, err := tx.Exec(ctx, `
			UPDATE positions SET quantity=0, unrealized_pnl=0, updated_at=now()
			 WHERE id=(SELECT position_id FROM option_positions WHERE id=$1)`,
			optionID); err != nil {
			return fmt.Errorf("expire option %d position: %w", optionID, err)
		}
		_, err = audit.Append(ctx, tx, "option_positions", &optionID, "UPDATE", nil)
		_ = reason
		return err
	})
}

// expireResidualWriters expires SHORT option rows whose adjusted expiry
// day is <= day and whose holders left unassigned residual open interest.
func (s *OptionService) expireResidualWriters(ctx context.Context, day time.Time) (int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id FROM option_positions o
		 WHERE o.side='SHORT' AND o.status='OPEN' AND o.expiry_at::date <= $1::date`,
		day)
	if err != nil {
		return 0, fmt.Errorf("residual writer scan: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	n := 0
	for _, id := range ids {
		err := s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
			opt, err := lockOptionRow(ctx, tx, id)
			if err != nil {
				return err
			}
			// Re-check the adjusted expiry day against the calendar.
			if ExerciseCutoffDay(s.cal, opt.ExpiryAt, opt.BaseCcy, opt.QuoteCcy).After(day) {
				return nil
			}
			if _, err := tx.Exec(ctx, `
				UPDATE option_positions SET status='EXPIRED', quantity=0, updated_at=now()
				 WHERE id=$1`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE positions SET quantity=0, unrealized_pnl=0, updated_at=now()
				 WHERE id=$1`, opt.PositionID); err != nil {
				return err
			}
			_, err = audit.Append(ctx, tx, "option_positions", &id, "UPDATE", nil)
			return err
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------------------------------------------------------------------------
// Premium settlement — trade date + T+2 (spec §15.4, §24 #158/#393)
// ---------------------------------------------------------------------------

// runPremiumSweep settles every PENDING premium row with due_date <=
// day. Each settlement is its own SERIALIZABLE tx with a deterministic
// idempotency key; a failed debit marks the row FAILED and queues the
// margin-call workflow — it never posts a partial journal.
func (s *OptionService) runPremiumSweep(ctx context.Context, day time.Time) (settled, failed int, err error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM option_premium_settlements
		 WHERE status='PENDING' AND due_date <= $1 ORDER BY id`, day)
	if err != nil {
		return 0, 0, fmt.Errorf("premium sweep scan: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	var firstErr error
	for _, id := range ids {
		ok, err := s.settlePremium(ctx, id)
		switch {
		case err != nil:
			failed++
			if firstErr == nil {
				firstErr = err
			}
		case ok:
			settled++
		default:
			failed++ // recorded FAILED
		}
	}
	return settled, failed, firstErr
}

// RunPremiumSweep settles premiums due as of now — the standalone seam
// for schedulers that decouple premium T+2 from the expiry batch.
func (s *OptionService) RunPremiumSweep(ctx context.Context) (settled, failed int, err error) {
	return s.runPremiumSweep(ctx, s.now().UTC().Truncate(24*time.Hour))
}

// settlePremium posts one holder→writer premium transfer through the GL.
// Returns ok=false when the debit failed (row → FAILED, margin-call
// queued); err only for infrastructure faults.
func (s *OptionService) settlePremium(ctx context.Context, settlementID int64) (bool, error) {
	var (
		holderID, writerID int64
		amount             decimal.Decimal
		ccy                string
		optID              int64
	)
	err := s.pool.QueryRow(ctx, `
		SELECT holder_account_id, writer_account_id, amount::text, currency,
		       holder_position_id
		  FROM option_premium_settlements WHERE id=$1`, settlementID).
		Scan(&holderID, &writerID, scanDec(&amount), &ccy, &optID)
	if err != nil {
		return false, fmt.Errorf("premium %d load: %w", settlementID, err)
	}

	// §5.3 account mutex on both parties (sorted inside the locker).
	token := fmt.Sprintf("premium-%d", s.now().UnixNano())
	locked, err := s.locks.LockAccounts(ctx, []int64{holderID, writerID}, token)
	if err != nil {
		return false, err
	}
	defer s.locks.UnlockAccounts(locked, token)

	var events []ledger.BalanceEvent
	var debitErr error
	err = s.inTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Guard: concurrent sweep could have settled it.
		var status string
		if err := tx.QueryRow(ctx,
			`SELECT status FROM option_premium_settlements WHERE id=$1 FOR UPDATE`,
			settlementID).Scan(&status); err != nil {
			return err
		}
		if status != "PENDING" {
			return errPremiumDone
		}
		j := ledger.Journal{
			EntryType:   ledger.EntrySettlement,
			ReferenceID: settlementID,
			Description: fmt.Sprintf(
				"option premium T+2 settlement=%d %s %s holder=%d→writer=%d",
				settlementID, amount, ccy, holderID, writerID),
			PostedBy:       s.postedBy,
			IdempotencyKey: fmt.Sprintf("option-premium:%d", settlementID),
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.CustomerLiability(ccy), ccy, amount,
					"holder pays option premium"),
				ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, amount,
					"writer receives option premium"),
			},
			Effects: []ledger.AccountEffect{
				{AccountID: holderID, Currency: ccy, AvailableDelta: amount.Neg()},
				{AccountID: writerID, Currency: ccy, AvailableDelta: amount},
			},
		}
		pr, postErr := s.ls.PostJournal(ctx, tx, j)
		if postErr != nil {
			var e *excerrors.Error
			if stderrors.As(postErr, &e) && e.Code == ledger.CodeInsufficientBalance {
				debitErr = postErr // record FAILED below in the same tx
			} else {
				return postErr
			}
		}
		var journalID *int64
		if debitErr == nil {
			journalID = &pr.JournalID
			events = pr.Events
		}
		if debitErr != nil {
			if _, err := tx.Exec(ctx, `
				UPDATE option_premium_settlements SET status='FAILED',
				       failure_reason=$2 WHERE id=$1`,
				settlementID, "PREMIUM_INSUFFICIENT: "+debitErr.Error()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE option_positions SET premium_status='FAILED', updated_at=now()
				 WHERE id=$1`, optID); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `
				UPDATE option_premium_settlements SET status='SETTLED',
				       journal_entry_id=$2, settled_at=now() WHERE id=$1`,
				settlementID, journalID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				UPDATE option_positions SET premium_status='SETTLED', updated_at=now()
				 WHERE id=$1`, optID); err != nil {
				return err
			}
		}
		_, err = audit.Append(ctx, tx, "option_premium_settlements", &settlementID, "UPDATE", nil)
		return err
	})
	if stderrors.Is(err, errPremiumDone) {
		return true, nil // already settled — idempotent replay
	}
	if err != nil {
		return false, err
	}
	if debitErr != nil {
		// §24 #393 — queue the Phase-19 Task 19.3.3 margin-call workflow.
		if s.marginCalls != nil {
			qctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if qerr := s.marginCalls.QueuePremiumShortfall(qctx, holderID,
				settlementID, ccy, amount); qerr != nil {
				return false, excerrors.Wrap("MARGIN_CALL_QUEUE_FAILED",
					fmt.Sprintf("premium %d failed; margin-call queue failed", settlementID), qerr)
			}
			if _, err := s.pool.Exec(ctx,
				`UPDATE option_premium_settlements SET margin_call_queued=TRUE WHERE id=$1`,
				settlementID); err != nil {
				return false, fmt.Errorf("premium %d margin-call flag: %w", settlementID, err)
			}
		}
		s.alertOps(ctx, "P1", "PREMIUM_INSUFFICIENT",
			fmt.Sprintf("premium %d (%s %s) failed for holder %d", settlementID, amount, ccy, holderID), debitErr)
		return false, nil
	}
	if err := s.dispatchExerciseEvents(ctx, events, nil); err != nil {
		return true, err
	}
	return true, nil
}

var errPremiumDone = stderrors.New("premium already settled")

// ---------------------------------------------------------------------------
// Daemon — 15:00 UTC weekday expiry batch (Task 22.3.10 scheduler seam)
// ---------------------------------------------------------------------------

// Run is the lifecycle daemon: every weekday it sleeps until 15:00 UTC,
// then processes that UTC date's expiries + due premiums. Saturday and
// Sunday have no expiries under the 24/5 FX week. Context cancel stops
// the loop between ticks. The orchestrator may instead call
// RunExpiryDay on its own schedule — this loop is the self-contained
// default.
func (s *OptionService) Run(ctx context.Context) error {
	for {
		now := s.now().UTC()
		next := nextExpiryTick(now)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(next.Sub(now)):
		}
		day := next.Truncate(24 * time.Hour)
		if _, err := s.RunExpiryDay(ctx, day); err != nil {
			s.alertOps(ctx, "P1", "OPTION_EXPIRY_RUN_FAILED",
				fmt.Sprintf("expiry batch %s failed", day.Format("2006-01-02")), err)
		}
	}
}

// nextExpiryTick returns the next weekday 15:00 UTC at/after now.
func nextExpiryTick(now time.Time) time.Time {
	t := time.Date(now.Year(), now.Month(), now.Day(),
		exerciseCutoffHourUTC, 0, 0, 0, time.UTC)
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	for t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

// ---------------------------------------------------------------------------
// Persistence helpers
// ---------------------------------------------------------------------------

func (s *OptionService) inTx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("lifecycle tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("lifecycle tx commit: %w", err)
	}
	return nil
}

func (s *OptionService) freshOptMark(ctx context.Context, symbol string) (decimal.Decimal, error) {
	mv, err := s.spot.Mark(ctx, symbol)
	if err != nil {
		return decimal.Zero, excerrors.Wrap(CodeOptionMarkUnavailable,
			fmt.Sprintf("mark read %s", symbol), err)
	}
	if !mv.Found || mv.Stale || !mv.Price.IsPositive() {
		return decimal.Zero, excerrors.New(CodeOptionMarkUnavailable, fmt.Sprintf(
			"mark for %s missing or stale (found=%v stale=%v) — lifecycle halted", symbol, mv.Found, mv.Stale))
	}
	return mv.Price, nil
}

// loadOption reads the option row + instrument (read-only path).
func (s *OptionService) loadOption(ctx context.Context, id int64) (*optionRow, error) {
	row := s.pool.QueryRow(ctx, optionSelectSQL+` WHERE o.id=$1`, id)
	return scanOptionRow(row)
}

const optionSelectSQL = `
	SELECT o.id, o.position_id, o.account_id, o.instrument_id,
	       o.underlying_instrument_id, o.side::text, o.option_type, o.exercise_style,
	       o.settlement, o.strike::text, o.quantity::text, o.expiry_at, o.payout::text,
	       o.status, o.do_not_exercise,
	       i.symbol, i.base_currency, i.quote_currency, i.max_leverage,
	       COALESCE(u.symbol,''), COALESCE(u.max_leverage,0)
	  FROM option_positions o
	  JOIN instruments i ON i.id = o.instrument_id
	  LEFT JOIN instruments u ON u.id = o.underlying_instrument_id`

type pgxRow interface{ Scan(dest ...any) error }

func scanOptionRow(row pgxRow) (*optionRow, error) {
	var (
		o        optionRow
		undID    *int64
		payoutTx *string
	)
	err := row.Scan(&o.ID, &o.PositionID, &o.AccountID, &o.InstrumentID,
		&undID, &o.Side, &o.OptionType, &o.ExerciseStyle, &o.Settlement,
		scanDec(&o.Strike), scanDec(&o.Quantity), &o.ExpiryAt, &payoutTx,
		&o.Status, &o.DoNotExercise,
		&o.Symbol, &o.BaseCcy, &o.QuoteCcy, &o.Lev, &o.UndSymbol, &o.UndLev)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("option row scan: %w", err)
	}
	o.UnderlyingID = undID
	o.Payout = parseDecPtr(payoutTx)
	return &o, nil
}

// lockOptionRow re-reads the row FOR UPDATE inside the tx.
func lockOptionRow(ctx context.Context, tx pgx.Tx, id int64) (*optionRow, error) {
	o, err := scanOptionRow(tx.QueryRow(ctx, optionSelectSQL+` WHERE o.id=$1 FOR UPDATE OF o`, id))
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, excerrors.New(CodeOptionNotExercisable,
			fmt.Sprintf("option position %d not found", id))
	}
	return o, nil
}

// lockWriters FOR-UPDATEs every open SHORT row on the instrument ordered
// by id — the pro-rata OI pool.
func lockWriters(ctx context.Context, tx pgx.Tx, instrumentID int64) ([]*optionRow, error) {
	rows, err := tx.Query(ctx, optionSelectSQL+`
		 WHERE o.instrument_id=$1 AND o.side='SHORT' AND o.status='OPEN'
		   AND o.quantity > 0
		 ORDER BY o.id FOR UPDATE OF o`, instrumentID)
	if err != nil {
		return nil, fmt.Errorf("writer lock: %w", err)
	}
	defer rows.Close()
	var out []*optionRow
	for rows.Next() {
		o, err := scanOptionRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// openWriterAccounts lists writer account ids for the mutex pre-lock.
func (s *OptionService) openWriterAccounts(ctx context.Context, instrumentID int64) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT account_id FROM option_positions
		 WHERE instrument_id=$1 AND side='SHORT' AND status='OPEN' AND quantity > 0`,
		instrumentID)
	if err != nil {
		return nil, fmt.Errorf("writer account scan: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// dueOptions lists holder options whose adjusted expiry day is <= day.
// The coarse SQL predicate (expiry date <= day + a 3-day adjustment
// buffer) is refined per-row through the calendar.
func (s *OptionService) dueOptions(ctx context.Context, day time.Time) ([]optionRow, error) {
	rows, err := s.pool.Query(ctx, optionSelectSQL+`
		 WHERE o.side='LONG' AND o.status='OPEN'
		   AND o.expiry_at::date <= $1::date + 3 ORDER BY o.id`,
		day)
	if err != nil {
		return nil, fmt.Errorf("due options scan: %w", err)
	}
	defer rows.Close()
	var out []optionRow
	for rows.Next() {
		o, err := scanOptionRow(rows)
		if err != nil {
			return nil, err
		}
		if ExerciseCutoffDay(s.cal, o.ExpiryAt, o.BaseCcy, o.QuoteCcy).After(day) {
			continue // expiry nudged past today
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

func (s *OptionService) exercisePref(ctx context.Context, accountID int64) (bool, error) {
	var atm bool
	err := s.pool.QueryRow(ctx,
		`SELECT auto_exercise_atm FROM option_exercise_prefs WHERE account_id=$1`,
		accountID).Scan(&atm)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return atm, err
}

// ---------------------------------------------------------------------------
// Pro-rata assignment — deterministic, seeded (§15.4 amended)
// ---------------------------------------------------------------------------

// assignmentSeed derives the deterministic tie-break seed stored on each
// assignment row for replay (§15.7 item 3).
func assignmentSeed(optionPositionID int64, qty decimal.Decimal, expiry time.Time) int64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%d",
		optionPositionID, qty.String(), expiry.UTC().Unix())))
	return int64(binary.BigEndian.Uint64(h[:8]) &^ (1 << 63))
}

// assignProRata splits qty across writers by open interest — largest-
// remainder rounding so the slices sum exactly to qty — with equal
// fractional remainders tie-broken by a seeded shuffle (random tie-break,
// spec §15.4; deterministic given the seed, §15.7 item 3). The result is
// parallel to writers: slices[i] ≤ writers[i].Quantity, Σslices = qty.
func assignProRata(writers []*optionRow, qty decimal.Decimal, seed int64) []decimal.Decimal {
	n := len(writers)
	out := make([]decimal.Decimal, n)
	if n == 0 || qty.IsZero() {
		return out
	}
	var oi decimal.Decimal
	for _, w := range writers {
		oi = oi.Add(w.Quantity)
	}
	if oi.IsZero() {
		return out
	}
	type rem struct {
		idx  int
		frac decimal.Decimal
	}
	var assigned decimal.Decimal
	rems := make([]rem, 0, n)
	for i, w := range writers {
		exact := qty.Mul(w.Quantity).Div(oi)
		floor := exact.Truncate(8)
		if floor.GreaterThan(w.Quantity) {
			floor = w.Quantity
		}
		out[i] = floor
		assigned = assigned.Add(floor)
		rems = append(rems, rem{i, exact.Sub(floor)})
	}
	leftover := qty.Sub(assigned)
	if leftover.IsPositive() {
		// Deterministic shuffle for the tie-break.
		r := rand.New(rand.NewSource(seed))
		r.Shuffle(len(rems), func(a, b int) { rems[a], rems[b] = rems[b], rems[a] })
		sort.SliceStable(rems, func(a, b int) bool {
			return rems[a].frac.GreaterThan(rems[b].frac)
		})
		unit := decimal.NewFromFloat(1e-8) // quanta of DECIMAL(28,8)
		for _, rm := range rems {
			if leftover.IsZero() {
				break
			}
			headroom := writers[rm.idx].Quantity.Sub(out[rm.idx])
			add := decMin(leftover, headroom)
			if add.IsPositive() {
				add = add.Mul(decimal.NewFromInt(1)).Truncate(8)
				if add.IsPositive() {
					out[rm.idx] = out[rm.idx].Add(add)
					leftover = leftover.Sub(add)
				}
			}
		}
		// Residual dust (<1e-8 quanta per writer) goes to the first writer
		// with headroom — the column is DECIMAL(28,8).
		if leftover.IsPositive() {
			for i := range out {
				headroom := writers[i].Quantity.Sub(out[i])
				add := decMin(leftover, headroom).Truncate(8)
				if add.IsPositive() {
					out[i] = out[i].Add(add)
					leftover = leftover.Sub(add)
				}
				if leftover.LessThan(unit) || leftover.IsZero() {
					break
				}
			}
		}
	}
	return out
}

// decMax / decMin — the facade (pkg/decimal) does not re-export
// shopspring's Max/Min; decimal-only comparisons.
func decMax(a, b decimal.Decimal) decimal.Decimal {
	if a.GreaterThanOrEqual(b) {
		return a
	}
	return b
}

func decMin(a, b decimal.Decimal) decimal.Decimal {
	if a.LessThanOrEqual(b) {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// Expiry-run bookkeeping (rollover_runs pattern)
// ---------------------------------------------------------------------------

func (s *OptionService) claimExpiryRun(ctx context.Context, day time.Time) (int64, bool, error) {
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO option_expiry_runs (run_date, status)
		VALUES ($1, 'RUNNING')
		ON CONFLICT (run_date) DO NOTHING
		RETURNING id`, day).Scan(&id)
	if stderrors.Is(err, pgx.ErrNoRows) {
		err = s.pool.QueryRow(ctx,
			`SELECT id FROM option_expiry_runs WHERE run_date=$1`, day).Scan(&id)
		if err != nil {
			return 0, false, fmt.Errorf("expiry run claim: %w", err)
		}
		return id, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("expiry run claim: %w", err)
	}
	return id, true, nil
}

func (s *OptionService) loadExpiryRun(ctx context.Context, runID int64) (*ExpiryRunReport, error) {
	var rep ExpiryRunReport
	var errText *string
	err := s.pool.QueryRow(ctx, `
		SELECT positions_evaluated, auto_exercised, expired, assignments,
		       premiums_settled, premiums_failed, error_text
		  FROM option_expiry_runs WHERE id=$1`, runID).Scan(
		&rep.Evaluated, &rep.AutoExercised, &rep.Expired, &rep.Assignments,
		&rep.PremiumsSettled, &rep.PremiumsFailed, &errText)
	if err != nil {
		return nil, fmt.Errorf("expiry run %d load: %w", runID, err)
	}
	rep.RunID = runID
	rep.Replayed = true
	if errText != nil && *errText != "" {
		rep.Errors = []string{*errText}
	}
	return &rep, nil
}

func (s *OptionService) completeExpiryRun(ctx context.Context, runID int64, rep *ExpiryRunReport) error {
	var errText *string
	if len(rep.Errors) > 0 {
		t := ""
		for _, e := range rep.Errors {
			t += e + "; "
		}
		errText = &t
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE option_expiry_runs SET status='COMPLETED', completed_at=now(),
		       positions_evaluated=$2, auto_exercised=$3, expired=$4,
		       assignments=$5, premiums_settled=$6, premiums_failed=$7,
		       error_text=$8
		 WHERE id=$1`,
		runID, rep.Evaluated, rep.AutoExercised, rep.Expired, rep.Assignments,
		rep.PremiumsSettled, rep.PremiumsFailed, errText)
	return err
}

func (s *OptionService) failExpiryRun(ctx context.Context, runID int64, cause error) {
	_, _ = s.pool.Exec(ctx, `
		UPDATE option_expiry_runs SET status='FAILED', completed_at=now(), error_text=$2
		 WHERE id=$1`, runID, cause.Error())
	s.alertOps(ctx, "P1", "OPTION_EXPIRY_RUN_FAILED",
		fmt.Sprintf("expiry run %d failed", runID), cause)
}

func (s *OptionService) alertOps(ctx context.Context, severity, code, summary string, cause error) {
	if s.alerter == nil {
		return
	}
	a := settlement.OpsAlert{Severity: severity, Code: code, Summary: summary}
	if cause != nil {
		a.Err = cause.Error()
	}
	actx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.alerter.Raise(actx, a)
}

// dispatchExerciseEvents publishes BalanceChanged events post-commit and
// the holder notification on "exchange.v1.notify.option-exercise"
// (Task 22.3.10 AC "holder notified"; the notification adapter consumes
// the subject — email/push delivery is outside this package).
func (s *OptionService) dispatchExerciseEvents(ctx context.Context, events []ledger.BalanceEvent, rep *ExerciseReport) error {
	sort.Slice(events, func(a, b int) bool {
		if events[a].AccountID != events[b].AccountID {
			return events[a].AccountID < events[b].AccountID
		}
		return events[a].Currency < events[b].Currency
	})
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			return excerrors.Wrap(ledger.CodeBalanceDispatchFailed, "lifecycle: marshal event", err)
		}
		if err := publishWithRetry(ctx, s.pub, ledger.BalanceChangedSubject(ev.AccountID), payload); err != nil {
			return excerrors.Wrap(ledger.CodeBalanceDispatchFailed,
				fmt.Sprintf("lifecycle committed; dispatch failed acct=%d", ev.AccountID), err)
		}
	}
	if rep != nil {
		payload, err := json.Marshal(map[string]any{
			"option_position_id": rep.OptionPositionID,
			"account_id":         rep.AccountID,
			"source":             rep.Source,
			"mode":               rep.Mode,
			"quantity":           rep.Quantity.String(),
			"mark":               rep.Mark.String(),
			"itm_bps":            rep.ITMBps.String(),
			"assignments":        rep.Assignments,
			"margin_shortfall":   rep.MarginShortfall,
		})
		if err != nil {
			return excerrors.Wrap(ledger.CodeBalanceDispatchFailed, "lifecycle: marshal notify", err)
		}
		if err := publishWithRetry(ctx, s.pub, "exchange.v1.notify.option-exercise", payload); err != nil {
			return excerrors.Wrap(ledger.CodeBalanceDispatchFailed,
				"lifecycle committed; holder notification failed", err)
		}
	}
	if rep != nil && rep.MarginShortfall && s.liquidator != nil {
		lctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.liquidator.LiquidateForExerciseShortfall(lctx, rep.AccountID); err != nil {
			s.alertOps(ctx, "P1", "EXERCISE_LIQUIDATION_FAILED",
				fmt.Sprintf("post-exercise liquidation failed acct=%d", rep.AccountID), err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Shared seams (owned here — roll.go carries its own store surface)
// ---------------------------------------------------------------------------

// AccountLocker is the §5.3 Redis account-mutex surface; the production
// binding is RedisAccountLocker over *redis.Client.
type AccountLocker interface {
	LockAccounts(ctx context.Context, ids []int64, token string) ([]int64, error)
	UnlockAccounts(ids []int64, token string)
}

// CodePositionBusy — §23 ACCOUNT_BUSY (HTTP 423): account mutex held.
const CodePositionBusy = "ACCOUNT_BUSY"

// RedisAccountLocker binds AccountLocker to the coordination Redis
// client (account:lock:{id} SET NX PX, §4.2/§5.3).
type RedisAccountLocker struct {
	C   *excredis.Client
	TTL time.Duration // <=0 → excredis.AccountLockTTL (10s)
}

// LockAccounts acquires account:lock:{id} in sorted id order —
// all-or-nothing, deadlock-free (same discipline as the ledger service).
func (l RedisAccountLocker) LockAccounts(ctx context.Context, ids []int64, token string) ([]int64, error) {
	if l.C == nil {
		return nil, excerrors.New(ledger.CodeLedgerLockUnavailable,
			"account mutex requires the Redis backend — client is nil")
	}
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	held := make([]int64, 0, len(sorted))
	for _, id := range sorted {
		ok, err := l.C.TryLockAccount(ctx, fmt.Sprint(id), token, l.TTL)
		if err != nil {
			l.UnlockAccounts(held, token)
			return nil, excerrors.Wrap(ledger.CodeLedgerLockUnavailable,
				fmt.Sprintf("lock account %d", id), err)
		}
		if !ok {
			l.UnlockAccounts(held, token)
			return nil, excerrors.New(CodePositionBusy,
				fmt.Sprintf("account %d mutex held by another operation", id))
		}
		held = append(held, id)
	}
	return held, nil
}

// UnlockAccounts releases held locks in reverse order on a fresh bounded
// context (the caller's ctx may be dead).
func (l RedisAccountLocker) UnlockAccounts(ids []int64, token string) {
	if l.C == nil {
		return
	}
	for i := len(ids) - 1; i >= 0; i-- {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = l.C.UnlockAccount(ctx, fmt.Sprint(ids[i]), token)
		cancel()
	}
}

// eventPublisher dispatches committed events — the settlement.Publisher
// contract (NATS JetStream in production).
type eventPublisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}

// opsAlerter is the optional ops-paging seam (settlement.OpsAlerter).
type opsAlerter interface {
	Raise(ctx context.Context, a settlement.OpsAlert) error
}

// ---------------------------------------------------------------------------
// Position helpers — the deliverable spot position bookkeeping a physical
// exercise books (netting math mirrors the position service; numerics
// cross the wire as text, never float)
// ---------------------------------------------------------------------------

// optPos is the in-tx view of one positions row.
type optPos struct {
	ID           int64
	AccountID    int64
	InstrumentID int64
	Side         string // LONG | SHORT
	Quantity     decimal.Decimal
	EntryPrice   decimal.Decimal
	MarginUsed   decimal.Decimal
	RealizedPnl  decimal.Decimal
}

func (p *optPos) sign() decimal.Decimal {
	if p.Side == "SHORT" {
		return decimal.NewFromInt(-1)
	}
	return decimal.NewFromInt(1)
}

// lockInstrumentPosition FOR-UPDATEs the account's positions row on
// (instrument, side); absent → a zero row to be inserted.
func lockInstrumentPosition(ctx context.Context, tx pgx.Tx, accountID, instrumentID int64, side string) (*optPos, error) {
	var p optPos
	err := tx.QueryRow(ctx, `
		SELECT id, account_id, instrument_id, side::text, quantity::text,
		       entry_price::text, COALESCE(margin_used,0)::text, realized_pnl::text
		  FROM positions
		 WHERE account_id = $1 AND instrument_id = $2 AND side = $3
		 LIMIT 1
		 FOR UPDATE`, accountID, instrumentID, side).Scan(
		&p.ID, &p.AccountID, &p.InstrumentID, &p.Side,
		scanDec(&p.Quantity), scanDec(&p.EntryPrice),
		scanDec(&p.MarginUsed), scanDec(&p.RealizedPnl))
	if stderrors.Is(err, pgx.ErrNoRows) {
		return &optPos{AccountID: accountID, InstrumentID: instrumentID, Side: side}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lifecycle: position lock: %w", err)
	}
	return &p, nil
}

// applyExerciseLeg merges qty of `side` into pos at price — the NETTING
// math of the position service distilled for one leg: same side → VWAP
// increase; opposing → reduce with realized P&L; over-close flips the
// remainder. Returns realized P&L (quote currency).
func applyExerciseLeg(pos *optPos, side string, qty, price decimal.Decimal) decimal.Decimal {
	fq := qty
	if side == "SHORT" {
		fq = fq.Neg()
	}
	signed := pos.Quantity
	if pos.Side == "SHORT" {
		signed = signed.Neg()
	}
	newSigned := signed.Add(fq)

	switch {
	case signed.IsZero() || signed.Sign() == fq.Sign():
		total := signed.Abs().Add(fq.Abs())
		if signed.IsZero() {
			pos.EntryPrice = price
		} else {
			pos.EntryPrice = signed.Abs().Mul(pos.EntryPrice).
				Add(fq.Abs().Mul(price)).Div(total)
		}
		pos.Quantity = total
		if newSigned.IsNegative() {
			pos.Side = "SHORT"
		} else {
			pos.Side = "LONG"
		}
		return decimal.Zero

	case fq.Abs().LessThan(signed.Abs()):
		realized := price.Sub(pos.EntryPrice).Mul(fq.Abs()).
			Mul(decimal.NewFromInt(int64(signed.Sign())))
		pos.Quantity = newSigned.Abs()
		pos.RealizedPnl = pos.RealizedPnl.Add(realized.Round(8))
		return realized.Round(8)

	default:
		realized := price.Sub(pos.EntryPrice).Mul(signed.Abs()).
			Mul(decimal.NewFromInt(int64(signed.Sign())))
		pos.RealizedPnl = pos.RealizedPnl.Add(realized.Round(8))
		if newSigned.IsZero() {
			pos.Quantity = decimal.Zero
			return realized.Round(8)
		}
		pos.Quantity = newSigned.Abs()
		if newSigned.IsNegative() {
			pos.Side = "SHORT"
		} else {
			pos.Side = "LONG"
		}
		pos.EntryPrice = price
		return realized.Round(8)
	}
}

// upsertOptPosition writes the position row (insert on first delivery).
func upsertOptPosition(ctx context.Context, tx pgx.Tx, p *optPos, mark *decimal.Decimal) error {
	unrealized := decimal.Zero
	if !p.Quantity.IsZero() && mark != nil {
		unrealized = mark.Sub(p.EntryPrice).Mul(p.Quantity).Mul(p.sign()).Round(8)
	}
	if p.ID == 0 {
		return tx.QueryRow(ctx, `
			INSERT INTO positions
			    (account_id, instrument_id, side, quantity, entry_price,
			     mark_price, unrealized_pnl, realized_pnl, margin_used, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, now())
			ON CONFLICT (account_id, instrument_id, side) DO UPDATE SET
			    quantity=EXCLUDED.quantity, entry_price=EXCLUDED.entry_price,
			    mark_price=EXCLUDED.mark_price, unrealized_pnl=EXCLUDED.unrealized_pnl,
			    realized_pnl=EXCLUDED.realized_pnl, margin_used=EXCLUDED.margin_used,
			    updated_at=now()
			RETURNING id`,
			p.AccountID, p.InstrumentID, p.Side, p.Quantity, p.EntryPrice,
			mark, unrealized, p.RealizedPnl, p.MarginUsed).Scan(&p.ID)
	}
	_, err := tx.Exec(ctx, `
		UPDATE positions SET quantity=$2, entry_price=$3, mark_price=$4,
		       unrealized_pnl=$5, realized_pnl=$6, margin_used=$7, updated_at=now()
		 WHERE id=$1`,
		p.ID, p.Quantity, p.EntryPrice, mark, unrealized, p.RealizedPnl, p.MarginUsed)
	if err != nil {
		return fmt.Errorf("lifecycle: upsert position %d: %w", p.ID, err)
	}
	return nil
}

// recordOptFill appends the position_fills dedup/audit row for one
// delivery leg (pseudo trade id — never a real trades.id).
func recordOptFill(ctx context.Context, tx pgx.Tx, pseudoTradeID uint64, p *optPos, side string, qty, price, realized decimal.Decimal) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO position_fills (trade_id, account_id, instrument_id, side, quantity, price, realized_pnl)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (trade_id, account_id) DO NOTHING`,
		int64(pseudoTradeID), p.AccountID, p.InstrumentID, side, qty, price, realized)
	if err != nil {
		return fmt.Errorf("lifecycle: record fill leg: %w", err)
	}
	return nil
}

// scanDec adapts a *decimal.Decimal destination to sql.Scanner input —
// numerics cross the wire as text (::text select, string param on write).
func scanDec(d *decimal.Decimal) any { return &decScanner{d: d} }

type decScanner struct{ d *decimal.Decimal }

func (s *decScanner) Scan(v any) error {
	switch t := v.(type) {
	case nil:
		*s.d = decimal.Zero
		return nil
	case string:
		d, err := decimal.NewFromString(t)
		if err != nil {
			return err
		}
		*s.d = d
		return nil
	case []byte:
		d, err := decimal.NewFromString(string(t))
		if err != nil {
			return err
		}
		*s.d = d
		return nil
	default:
		return fmt.Errorf("decScanner: unsupported type %T", v)
	}
}

func parseDecPtr(s *string) *decimal.Decimal {
	if s == nil {
		return nil
	}
	d, err := decimal.NewFromString(*s)
	if err != nil {
		return nil
	}
	return &d
}

func publishWithRetry(ctx context.Context, pub eventPublisher, subject string, payload []byte) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = pub.Publish(ctx, subject, payload); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
		}
	}
	return err
}

// decPtrText renders an optional decimal as a nullable text param
// (numerics cross the wire as strings — see scanDec).
func decPtrText(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.String()
}

func nullableID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}
