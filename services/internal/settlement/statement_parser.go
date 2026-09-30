// statement_parser.go — bank-statement ingestion + automated
// reconciliation matching (Phase-24 Task 24.3.12; spec §17.10, §24 #175).
//
// Format decoding lives in internal/backoffice (statement_parser.go,
// camt053_parser.go) which implements the StatementParser seam below;
// this package owns persistence, validation and match/break semantics
// because the matched targets — settlement_instructions, rail_payments,
// nostro_movements — and the settlement_exceptions break queue are
// settlement-side state.
//
// Guarantees (task text):
//   - file checksum (sha256) + supplied-checksum validation; duplicate
//     transmission is detected BEFORE parsing (bank_statements
//     .file_checksum_sha256 UNIQUE);
//   - IBAN/BIC validated against the nostro_accounts row;
//   - out-of-order statement sequence numbers and opening/closing
//     balance discontinuity open breaks;
//   - entry matching priority: UETR → transaction reference →
//     amount+currency+exact value date; duplicates never re-credit;
//   - every unmatched/broken entry lands a settlement_exceptions row in
//     the same ingestion transaction — well inside the 60s routing
//     budget (§24 #175 criterion 2);
//   - malformed/incomplete input fails closed — nothing is persisted.
package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// Parsed statement model — produced by the backoffice format parsers
// (MT940/MT942/camt.053.001.08); normalized for ingestion.
// ---------------------------------------------------------------------------

// ParsedEntry is one normalized statement line.
type ParsedEntry struct {
	EntryRef        string          // :61: customer ref / NtryRef
	BankRef         string          // :61: //bank ref / AcctSvcrRef
	UETR            string          // camt UETR
	EndToEndID      string          // camt EndToEndId
	ValueDate       time.Time       // bank value date
	BookingDate     time.Time       // booking date
	Amount          decimal.Decimal // always positive
	Currency        string
	Credit          bool   // true=CRDT inbound, false=DBIT outbound
	IsReversal      bool   // MT940 RC/RD reversal marks
	TxCode          string // SWIFT transaction type + idcode / proprietary code
	RemitterName    string // debtor name (:86: parsed / camt Dbtr)
	RemitterAccount string // debtor account
	Narrative       string // :86: / AddtlNtryInf
}

// ParsedStatement is the normalized header + entries.
type ParsedStatement struct {
	Format          string // MT940|MT942|CAMT053
	StatementNumber string
	SequenceNumber  *int
	StatementDate   time.Time
	OpeningBalance  *decimal.Decimal // nil for MT942 interim reports
	ClosingBalance  *decimal.Decimal
	Currency        string
	IBAN            string
	BIC             string
	Entries         []ParsedEntry
}

// StatementParser decodes a raw statement file into ParsedStatement.
// backoffice.StatementParsers implements this; nil → fail closed.
type StatementParser interface {
	Parse(ctx context.Context, format string, raw []byte) (*ParsedStatement, error)
}

// ---------------------------------------------------------------------------
// Error codes (registered on errs.Default via localCodes)
// ---------------------------------------------------------------------------

const (
	// CodeStatementMalformed — parser rejected the file or the
	// caller-supplied checksum does not match the file.
	CodeStatementMalformed = "STATEMENT_MALFORMED"
	// CodeStatementParserMissing — no StatementParser wired.
	CodeStatementParserMissing = "STATEMENT_PARSER_MISSING"
	// CodeStatementAccountMismatch — statement IBAN/BIC/currency do not
	// agree with the nostro account it claims to describe.
	CodeStatementAccountMismatch = "STATEMENT_ACCOUNT_MISMATCH"
	// CodeStatementNotFound — unknown bank_statements row.
	CodeStatementNotFound = "STATEMENT_NOT_FOUND"
)

// Exception types per the shared settlement_exceptions CHECK domain
// (migration 258_settlement_ops + sibling backoffice.ExceptionType).
const (
	stmtExceptionClsUnmatched    = "CLS_MISMATCH"
	stmtExceptionClsRejected     = "CLS_MISMATCH"
	stmtExceptionPrincipalBreach = "OTHER"
	StmtExcUnmatchedStatement    = "UNMATCHED_STATEMENT"
	StmtExcAmountMismatch        = "AMOUNT_MISMATCH"
	StmtExcMissingPayment        = "MISSING_PAYMENT"
	StmtExcUnexpectedCredit      = "UNEXPECTED_CREDIT"
)

// ---------------------------------------------------------------------------
// SettlementException — shared break-queue row (settlement_exceptions,
// sibling-owned shape + 260_statement_exception_links columns).
// ---------------------------------------------------------------------------

// SettlementException is one break-queue write; Code maps to
// exception_type.
type SettlementException struct {
	Code              string // exception_type (CHECK domain above)
	StatementEntryID  *int64
	StatementID       *int64
	InstructionID     *int64 // settlement_instructions.id
	NostroMovementID  *int64
	NettingBatchID    *int64
	ClsInstructionID  *int64
	SuspenseMappingID *int64
	TradeID           *int64
	AccountID         *int64
	Currency          string
	Amount            *decimal.Decimal
	ExpectedAmount    *decimal.Decimal
	ActualAmount      *decimal.Decimal
	DetectedBy        string // required — source component
	Detail            string
}

// insertSettlementException writes one break row inside q's transaction
// (PostgreSQL ≥15). expected/actual amounts carry the mismatch evidence.
func insertSettlementException(ctx context.Context, q Querier, e SettlementException) (int64, error) {
	var amount any
	if e.Amount != nil {
		amount = e.Amount.String()
	}
	var expected, actual any
	if e.ExpectedAmount != nil {
		expected = e.ExpectedAmount.String()
	}
	if e.ActualAmount != nil {
		actual = e.ActualAmount.String()
	}
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO settlement_exceptions
		    (settlement_instruction_id, trade_id, account_id, currency, amount,
		     exception_type, status, detected_by, detail, detected_at,
		     statement_entry_id, statement_id, nostro_movement_id,
		     netting_batch_id, cls_instruction_id, suspense_mapping_id,
		     expected_amount, actual_amount)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,'OPEN',$7,$8,now(),
		        $9,$10,$11,$12,$13,$14,$15::numeric,$16::numeric)
		RETURNING id`,
		e.InstructionID, e.TradeID, e.AccountID, nullStr(e.Currency), amount,
		e.Code, e.DetectedBy, e.Detail,
		e.StatementEntryID, e.StatementID, e.NostroMovementID,
		e.NettingBatchID, e.ClsInstructionID, e.SuspenseMappingID,
		expected, actual).Scan(&id)
	return id, err
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------
// Domain rows
// ---------------------------------------------------------------------------

// BankStatement is one bank_statements header row.
type BankStatement struct {
	ID              int64
	NostroAccountID int64
	Format          string
	StatementNumber string
	SequenceNumber  *int
	StatementDate   *time.Time
	OpeningBalance  *decimal.Decimal
	ClosingBalance  *decimal.Decimal
	Currency        string
	IBAN            string
	BIC             string
	ChecksumSHA256  string
	EntryCount      int
	MatchedCount    int
	Status          string
	IngestSource    string
	CreatedAt       time.Time
}

// StatementEntryRow is one persisted statement_entries row.
type StatementEntryRow struct {
	ID        int64
	Entry     ParsedEntry
	MatchKey  string // UETR|REF|AMOUNT once matched
	Status    string // UNMATCHED|MATCHED|EXCEPTION|REVERSED
	Duplicate bool   // true when the natural-key dedup hit
}

// EntryMatch is the resolved internal linkage for a statement line.
type EntryMatch struct {
	InstructionID  *int64           // settlement_instructions.id
	PaymentID      *int64           // rail_payments.id
	MovementID     *int64           // nostro_movements.id
	ClsID          *int64           // cls_settlement_instructions.id
	Key            string           // UETR | REF | AMOUNT
	ExpectedAmount *decimal.Decimal // populated on AMOUNT-key matches
}

// MissingPayment is an expected-outbound leg absent from the statement.
type MissingPayment struct {
	InstructionID  int64
	TradeID        int64
	Currency       string
	Amount         decimal.Decimal
	SettlementDate time.Time
	Reference      string // swift_message_id
}

// ---------------------------------------------------------------------------
// Store seam
// ---------------------------------------------------------------------------

// StatementStore is the ingestion persistence seam.
type StatementStore interface {
	// NostroByID loads the nostro_accounts row (IBAN/BIC validation).
	NostroByID(ctx context.Context, id int64) (*NostroAccount, bool, error)
	// StatementByChecksum dedupes a retransmitted file.
	StatementByChecksum(ctx context.Context, sha256 string) (*BankStatement, bool, error)
	// LatestStatement returns the most recent prior statement for the
	// (nostro, statement_number) stream — sequence + continuity checks.
	LatestStatement(ctx context.Context, nostroID int64, stmtNumber string) (*BankStatement, bool, error)
	// OpenExceptionFor reports whether an OPEN exception already exists
	// for the (instruction|entry, type) pair — break dedup.
	OpenExceptionFor(ctx context.Context, typ string, instructionID, entryID int64) (bool, error)
	// ListStatements pages the statement journal (nostroID<=0 = all).
	ListStatements(ctx context.Context, nostroID int64, limit int) ([]BankStatement, error)
	// ListEntries returns one statement's persisted entries.
	ListEntries(ctx context.Context, statementID int64) ([]StatementEntryRow, error)
	InTx(ctx context.Context, fn func(ctx context.Context, tx StatementTx) error) error
	// MissingPayments lists dispatched-but-unsettled instructions whose
	// settlement_date is on/before the statement date — the
	// MISSING_PAYMENT scan.
	MissingPayments(ctx context.Context, nostroID int64, ccy string, onOrBefore time.Time) ([]MissingPayment, error)
}

// StatementTx is the transactional view inside StatementStore.InTx.
type StatementTx interface {
	InsertStatement(ctx context.Context, s BankStatement) (int64, error)
	// InsertEntry persists one normalized entry on the natural key;
	// id=0, dup=true when the entry already exists (re-ingest).
	InsertEntry(ctx context.Context, statementID int64, e ParsedEntry) (id int64, dup bool, err error)
	SetStatementStatus(ctx context.Context, id int64, status string, matched, total int) error
	// Matchers — priority order enforced by the caller.
	MatchByUETR(ctx context.Context, uetr, ccy string) (*EntryMatch, bool, error)
	MatchByRef(ctx context.Context, ref, ccy string) (*EntryMatch, bool, error)
	MatchByAmount(ctx context.Context, ccy string, amount decimal.Decimal, valueDate time.Time, credit bool) (*EntryMatch, bool, error)
	SetEntryMatch(ctx context.Context, entryID int64, m EntryMatch) error
	MarkEntryStatus(ctx context.Context, entryID int64, status string) error
	InsertException(ctx context.Context, e SettlementException) (int64, error)
}

// ---------------------------------------------------------------------------
// Ingestion service
// ---------------------------------------------------------------------------

// SuspenseRouter routes an unmatched inbound credit into the Phase-11
// deposit-guard quarantine (Task 24.3.21 — suspense_service.go adapts
// funding.DepositGuard; nil → the UNEXPECTED_CREDIT break still opens
// and RoutingFailed is flagged).
type SuspenseRouter interface {
	RouteUnmatchedCredit(ctx context.Context, e ParsedEntry, bankTxID string) (suspenseID int64, err error)
}

// IngestRequest carries one raw statement file.
type IngestRequest struct {
	NostroAccountID  int64
	Format           string // MT940|MT942|CAMT053
	Raw              []byte
	Source           string // SFTP|MQ|MANUAL (default MANUAL)
	ExpectedChecksum string // optional caller-supplied sha256 hex
}

// IngestResult reports the ingest outcome.
type IngestResult struct {
	StatementID  int64
	Duplicate    bool // file checksum already ingested — idempotent replay
	Statement    *BankStatement
	Entries      []StatementEntryRow
	Matched      int
	Unmatched    int
	BreaksOpened int
	SequenceGap  bool
	BalanceBreak bool
}

// StatementIngestionService ingests + reconciles bank statements.
type StatementIngestionService struct {
	store    StatementStore
	parser   StatementParser
	suspense SuspenseRouter
	alerter  OpsAlerter
	clock    func() time.Time
}

// StatementIngestionOptions wires optional collaborators.
type StatementIngestionOptions struct {
	Parser   StatementParser // required — nil fails closed on Ingest
	Suspense SuspenseRouter  // optional — nil still opens the break
	Alerter  OpsAlerter
	Clock    func() time.Time
}

// NewStatementIngestionService wires the service; store is required.
func NewStatementIngestionService(store StatementStore, opts StatementIngestionOptions) (*StatementIngestionService, error) {
	if store == nil {
		return nil, fmt.Errorf("statement ingestion: nil store")
	}
	clk := opts.Clock
	if clk == nil {
		clk = time.Now
	}
	return &StatementIngestionService{
		store: store, parser: opts.Parser,
		suspense: opts.Suspense, alerter: opts.Alerter, clock: clk,
	}, nil
}

// Ingest validates, persists, matches and break-routes one statement
// file. Whole-file transactional: a malformed file persists nothing.
func (s *StatementIngestionService) Ingest(ctx context.Context, req IngestRequest) (*IngestResult, error) {
	if s.parser == nil {
		return nil, excerrors.New(CodeStatementParserMissing,
			"statement ingestion: parser not wired (fail-closed)")
	}
	if req.NostroAccountID <= 0 {
		return nil, excerrors.New("INVALID_REQUEST", "statement: nostro account id required")
	}
	if len(req.Raw) == 0 {
		return nil, excerrors.New(CodeStatementMalformed, "statement: empty file")
	}
	format := strings.ToUpper(strings.TrimSpace(req.Format))
	if format != "MT940" && format != "MT942" && format != "CAMT053" {
		return nil, excerrors.New(CodeStatementMalformed,
			fmt.Sprintf("statement: unsupported format %q", req.Format))
	}
	sum := sha256.Sum256(req.Raw)
	checksum := hex.EncodeToString(sum[:])
	if req.ExpectedChecksum != "" &&
		!strings.EqualFold(strings.TrimSpace(req.ExpectedChecksum), checksum) {
		return nil, excerrors.New(CodeStatementMalformed,
			"statement: supplied checksum does not match file contents")
	}

	// Duplicate transmission guard — idempotent replay returns the
	// original row, no second credit can flow.
	if prior, found, err := s.store.StatementByChecksum(ctx, checksum); err != nil {
		return nil, fmt.Errorf("statement: checksum lookup: %w", err)
	} else if found {
		return &IngestResult{StatementID: prior.ID, Duplicate: true, Statement: prior}, nil
	}

	nostro, found, err := s.store.NostroByID(ctx, req.NostroAccountID)
	if err != nil {
		return nil, fmt.Errorf("statement: nostro lookup: %w", err)
	}
	if !found {
		return nil, excerrors.New(CodeStatementAccountMismatch,
			fmt.Sprintf("statement: nostro account %d not found", req.NostroAccountID))
	}

	parsed, err := s.parser.Parse(ctx, format, req.Raw)
	if err != nil {
		return nil, excerrors.Wrap(CodeStatementMalformed, "statement: parse", err)
	}
	if parsed == nil {
		return nil, excerrors.New(CodeStatementMalformed, "statement: parser returned nil")
	}
	if err := s.validateHeader(parsed, nostro); err != nil {
		return nil, err
	}
	source := strings.ToUpper(strings.TrimSpace(req.Source))
	if source == "" {
		source = "MANUAL"
	}
	if parsed.Format == "" {
		parsed.Format = format
	}

	var res *IngestResult
	var alerts []OpsAlert
	err = s.store.InTx(ctx, func(ctx context.Context, tx StatementTx) error {
		res = &IngestResult{}
		hdr := BankStatement{
			NostroAccountID: req.NostroAccountID,
			Format:          parsed.Format,
			StatementNumber: parsed.StatementNumber,
			SequenceNumber:  parsed.SequenceNumber,
			Currency:        parsed.Currency,
			IBAN:            parsed.IBAN,
			BIC:             parsed.BIC,
			ChecksumSHA256:  checksum,
			IngestSource:    source,
			Status:          "INGESTED",
		}
		if !parsed.StatementDate.IsZero() {
			d := normalizeDay(parsed.StatementDate)
			hdr.StatementDate = &d
		}
		hdr.OpeningBalance, hdr.ClosingBalance = parsed.OpeningBalance, parsed.ClosingBalance

		// Sequence + balance-continuity checks against the prior
		// statement of the same stream (statement_number family).
		prior, foundPrior, perr := s.store.LatestStatement(ctx, req.NostroAccountID, parsed.StatementNumber)
		if perr != nil {
			return perr
		}
		if foundPrior {
			res.Statement = prior // continuity evidence
			if parsed.SequenceNumber != nil && prior.SequenceNumber != nil {
				if *parsed.SequenceNumber <= *prior.SequenceNumber {
					// Out-of-order/duplicate sequence — the file is
					// rejected, no entries persist (fail-closed).
					return excerrors.New(CodeStatementMalformed, fmt.Sprintf(
						"statement: out-of-order sequence %d (prior %d) for nostro %d stream %q",
						*parsed.SequenceNumber, *prior.SequenceNumber, req.NostroAccountID,
						parsed.StatementNumber))
				}
				if *parsed.SequenceNumber > *prior.SequenceNumber+1 {
					res.SequenceGap = true
				}
			}
			if prior.ClosingBalance != nil && parsed.OpeningBalance != nil &&
				!prior.ClosingBalance.Equal(*parsed.OpeningBalance) {
				res.BalanceBreak = true
			}
		}

		stmtID, err := tx.InsertStatement(ctx, hdr)
		if err != nil {
			return err
		}
		res.StatementID = stmtID

		if res.SequenceGap {
			if _, err := tx.InsertException(ctx, SettlementException{
				Code: StmtExcUnmatchedStatement, StatementID: &stmtID,
				Currency: parsed.Currency, DetectedBy: "settlement:statement-ingest",
				Detail: fmt.Sprintf("statement sequence gap: prior seq %v, received %v",
					prior.SeqNo(), parsed.SeqNo()),
			}); err != nil {
				return err
			}
			res.BreaksOpened++
		}
		if res.BalanceBreak {
			if _, err := tx.InsertException(ctx, SettlementException{
				Code: StmtExcAmountMismatch, StatementID: &stmtID,
				Currency: parsed.Currency, ExpectedAmount: prior.ClosingBalance,
				ActualAmount: parsed.OpeningBalance, DetectedBy: "settlement:statement-ingest",
				Detail: "balance discontinuity: opening != prior closing",
			}); err != nil {
				return err
			}
			res.BreaksOpened++
		}

		// Persist + match each entry.
		for _, e := range parsed.Entries {
			e.Currency = strings.ToUpper(e.Currency)
			entryID, dup, ierr := tx.InsertEntry(ctx, stmtID, e)
			if ierr != nil {
				return ierr
			}
			row := StatementEntryRow{ID: entryID, Entry: e, Status: "UNMATCHED", Duplicate: dup}
			if dup {
				row.Status = "MATCHED" // replay — keep prior disposition marker
				res.Entries = append(res.Entries, row)
				continue
			}
			matched, merr := s.matchEntry(ctx, tx, stmtID, entryID, e, nostro.Currency, res)
			if merr != nil {
				return merr
			}
			if matched != "" {
				row.MatchKey, row.Status = matched, "MATCHED"
				res.Matched++
			} else {
				row.Status = "EXCEPTION"
				res.Unmatched++
			}
			res.Entries = append(res.Entries, row)
		}

		// MISSING_PAYMENT scan — dispatched instructions expected on or
		// before the statement date that no entry covered.
		if hdr.StatementDate != nil {
			missing, merr := s.store.MissingPayments(ctx, req.NostroAccountID, parsed.Currency, *hdr.StatementDate)
			if merr != nil {
				return merr
			}
			for _, mp := range missing {
				open, oerr := s.store.OpenExceptionFor(ctx, StmtExcMissingPayment, mp.InstructionID, 0)
				if oerr != nil {
					return oerr
				}
				if open {
					continue // break already tracked
				}
				if _, err := tx.InsertException(ctx, SettlementException{
					Code: StmtExcMissingPayment, StatementID: &stmtID,
					InstructionID: &mp.InstructionID, TradeID: &mp.TradeID,
					Currency: mp.Currency, Amount: &mp.Amount,
					ExpectedAmount: &mp.Amount, DetectedBy: "settlement:statement-ingest",
					Detail: fmt.Sprintf("expected payment %s (%s %s, value %s) absent from statement %d",
						mp.Reference, mp.Amount, mp.Currency,
						mp.SettlementDate.Format("2006-01-02"), stmtID),
				}); err != nil {
					return err
				}
				res.BreaksOpened++
			}
		}

		status := "INGESTED"
		switch {
		case res.SequenceGap:
			status = "SEQUENCE_GAP"
		case res.Unmatched > 0 && res.Matched > 0:
			status = "PARTIALLY_MATCHED"
		case res.Unmatched > 0:
			status = "PARTIALLY_MATCHED"
		case res.Matched == len(parsed.Entries) && len(parsed.Entries) > 0:
			status = "MATCHED"
		}
		if err := tx.SetStatementStatus(ctx, stmtID, status, res.Matched, len(parsed.Entries)); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Post-commit alerting (P2) for breaks opened — routing happens inside
	// the 60s budget; the tx already persisted everything.
	if res.BreaksOpened > 0 {
		alerts = append(alerts, OpsAlert{Severity: "P2", Code: "STATEMENT_BREAKS",
			Summary: fmt.Sprintf("statement %d: %d breaks opened (%d matched, %d unmatched entries)",
				res.StatementID, res.BreaksOpened, res.Matched, res.Unmatched)})
	}
	for _, a := range alerts {
		s.raise(ctx, a.Severity, a.Code, a.Summary, nil)
	}
	return res, nil
}

// SeqNo renders the sequence component for diagnostics.
func (b *BankStatement) SeqNo() any {
	if b == nil || b.SequenceNumber == nil {
		return nil
	}
	return *b.SequenceNumber
}

// SeqNo on the parsed header.
func (p *ParsedStatement) SeqNo() any {
	if p == nil || p.SequenceNumber == nil {
		return nil
	}
	return *p.SequenceNumber
}

// validateHeader cross-checks the parsed header against the nostro
// account row (currency, IBAN, BIC) — a statement for a different
// account/currency is rejected, never ingested.
func (s *StatementIngestionService) validateHeader(p *ParsedStatement, n *NostroAccount) error {
	if !currencyRe.MatchString(strings.ToUpper(p.Currency)) {
		return excerrors.New(CodeStatementMalformed,
			fmt.Sprintf("statement: bad currency %q", p.Currency))
	}
	if !strings.EqualFold(p.Currency, n.Currency) {
		return excerrors.New(CodeStatementAccountMismatch, fmt.Sprintf(
			"statement: currency %s ≠ nostro %d currency %s", p.Currency, n.ID, n.Currency))
	}
	if p.IBAN != "" && n.IBAN != "" && !strings.EqualFold(p.IBAN, n.IBAN) {
		return excerrors.New(CodeStatementAccountMismatch, fmt.Sprintf(
			"statement: IBAN %s ≠ nostro %d IBAN %s", p.IBAN, n.ID, n.IBAN))
	}
	if p.BIC != "" && n.BankCode != "" &&
		!strings.EqualFold(bic11(strings.ToUpper(p.BIC)), bic11(strings.ToUpper(n.BankCode))) {
		return excerrors.New(CodeStatementAccountMismatch, fmt.Sprintf(
			"statement: BIC %s ≠ nostro %d bank %s", p.BIC, n.ID, n.BankCode))
	}
	for i, e := range p.Entries {
		if !e.Amount.IsPositive() || !e.Amount.Round(8).Equal(e.Amount) {
			return excerrors.New(CodeStatementMalformed,
				fmt.Sprintf("statement: entry %d amount %s invalid", i, e.Amount))
		}
		if e.ValueDate.IsZero() {
			return excerrors.New(CodeStatementMalformed,
				fmt.Sprintf("statement: entry %d missing value date", i))
		}
	}
	return nil
}

// matchEntry runs the priority ladder and persists the outcome. Returns
// the match key ("" = unmatched → break opened by raiseEntryBreak).
func (s *StatementIngestionService) matchEntry(ctx context.Context, tx StatementTx,
	stmtID, entryID int64, e ParsedEntry, nostroCcy string, res *IngestResult) (string, error) {

	var m *EntryMatch
	var found bool
	var err error
	if e.UETR != "" {
		m, found, err = tx.MatchByUETR(ctx, e.UETR, nostroCcy)
		if err != nil {
			return "", err
		}
	}
	if !found {
		for _, ref := range []string{e.EndToEndID, e.EntryRef, e.BankRef} {
			if ref == "" {
				continue
			}
			if m, found, err = tx.MatchByRef(ctx, ref, nostroCcy); err != nil {
				return "", err
			}
			if found {
				break
			}
		}
	}
	if !found {
		if m, found, err = tx.MatchByAmount(ctx, nostroCcy, e.Amount,
			normalizeDay(e.ValueDate), e.Credit); err != nil {
			return "", err
		}
	}

	if found {
		// Exact-amount check on REF/UETR matches: a linked instruction
		// with a different amount is an AMOUNT_MISMATCH break, not a
		// clean match.
		if m.ExpectedAmount != nil && !m.ExpectedAmount.Equal(e.Amount) {
			if _, err := tx.InsertException(ctx, SettlementException{
				Code: StmtExcAmountMismatch, StatementEntryID: &entryID, StatementID: &stmtID,
				InstructionID: m.InstructionID,
				Currency:      e.Currency, ExpectedAmount: m.ExpectedAmount, ActualAmount: &e.Amount,
				DetectedBy: "settlement:statement-ingest",
				Detail: fmt.Sprintf("entry amount %s ≠ expected %s (match key %s)",
					e.Amount, *m.ExpectedAmount, m.Key),
			}); err != nil {
				return "", err
			}
			res.BreaksOpened++
			_ = tx.MarkEntryStatus(ctx, entryID, "EXCEPTION")
			return "EXCEPTION", nil
		}
		if err := tx.SetEntryMatch(ctx, entryID, *m); err != nil {
			return "", err
		}
		return m.Key, nil
	}
	return "", s.raiseEntryBreak(ctx, tx, stmtID, entryID, e, nostroCcy, res)
}

// raiseEntryBreak opens the appropriate settlement_exception for an
// unmatched line. Credits additionally route to the suspense queue when
// a SuspenseRouter is wired (Task 24.3.21).
func (s *StatementIngestionService) raiseEntryBreak(ctx context.Context, tx StatementTx,
	stmtID, entryID int64, e ParsedEntry, nostroCcy string, res *IngestResult) error {

	typ := StmtExcUnmatchedStatement
	detail := "unmatched statement entry"
	if e.Credit && !e.IsReversal {
		typ = StmtExcUnexpectedCredit
		detail = "unmatched inbound credit — routed to suspense review"
	}
	exc := SettlementException{
		Code: typ, StatementEntryID: &entryID, StatementID: &stmtID,
		Currency: nostroCcy, ActualAmount: &e.Amount,
		DetectedBy: "settlement:statement-ingest", Detail: detail,
	}
	if typ == StmtExcUnexpectedCredit && s.suspense != nil {
		// Suspense routing runs inside the ingestion tx's 60s window;
		// the guard's own persist+post is idempotent on BankTxID.
		bankTxID := e.BankRef
		if bankTxID == "" {
			bankTxID = e.EntryRef
		}
		if sid, rerr := s.suspense.RouteUnmatchedCredit(ctx, e, bankTxID); rerr == nil && sid > 0 {
			exc.SuspenseMappingID = &sid
		}
		// A nil/failed router never blocks ingestion — the break is the
		// contractual artifact (§24 #175), quarantine is the ops path.
	}
	if _, err := tx.InsertException(ctx, exc); err != nil {
		return err
	}
	res.BreaksOpened++
	return tx.MarkEntryStatus(ctx, entryID, "EXCEPTION")
}

func (s *StatementIngestionService) raise(ctx context.Context, sev, code, summary string, err error) {
	if s.alerter == nil {
		return
	}
	a := OpsAlert{Severity: sev, Code: code, Summary: summary}
	if err != nil {
		a.Err = err.Error()
	}
	_ = s.alerter.Raise(ctx, a)
}

// ---------------------------------------------------------------------------
// PgxStatementStore
// ---------------------------------------------------------------------------

// PgxStatementStore implements StatementStore/StatementTx over pgx.
type PgxStatementStore struct{ Pool *pgxpool.Pool }

// NewPgxStatementStore wires the store.
func NewPgxStatementStore(pool *pgxpool.Pool) *PgxStatementStore {
	return &PgxStatementStore{Pool: pool}
}

// NostroByID loads one nostro_accounts row.
func (s *PgxStatementStore) NostroByID(ctx context.Context, id int64) (*NostroAccount, bool, error) {
	var n NostroAccount
	err := s.Pool.QueryRow(ctx, `
		SELECT id, currency, bank_name, COALESCE(bank_code,''),
		       COALESCE(account_number,''), COALESCE(iban,'')
		  FROM nostro_accounts WHERE id = $1`, id).
		Scan(&n.ID, &n.Currency, &n.BankName, &n.BankCode, &n.AccountNumber, &n.IBAN)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &n, true, nil
}

const stmtSelect = `
	SELECT id, nostro_account_id, format::text, COALESCE(statement_number,''),
	       sequence_number, statement_date, opening_balance::text, closing_balance::text,
	       currency, COALESCE(iban,''), COALESCE(bic,''), file_checksum_sha256,
	       entry_count, matched_count, status::text, ingest_source, created_at
	  FROM bank_statements`

func scanStatement(row pgx.Row) (*BankStatement, bool, error) {
	var b BankStatement
	var open, close_ *string
	err := row.Scan(&b.ID, &b.NostroAccountID, &b.Format, &b.StatementNumber,
		&b.SequenceNumber, &b.StatementDate, &open, &close_, &b.Currency,
		&b.IBAN, &b.BIC, &b.ChecksumSHA256, &b.EntryCount, &b.MatchedCount,
		&b.Status, &b.IngestSource, &b.CreatedAt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if open != nil {
		d, derr := decimal.NewFromString(*open)
		if derr != nil {
			return nil, false, fmt.Errorf("statement %d opening balance: %w", b.ID, derr)
		}
		b.OpeningBalance = &d
	}
	if close_ != nil {
		d, derr := decimal.NewFromString(*close_)
		if derr != nil {
			return nil, false, fmt.Errorf("statement %d closing balance: %w", b.ID, derr)
		}
		b.ClosingBalance = &d
	}
	return &b, true, nil
}

// StatementByChecksum dedupes a retransmitted file on its sha256.
func (s *PgxStatementStore) StatementByChecksum(ctx context.Context, sha string) (*BankStatement, bool, error) {
	return scanStatement(s.Pool.QueryRow(ctx,
		stmtSelect+` WHERE file_checksum_sha256 = $1`, sha))
}

// LatestStatement returns the newest prior statement of the stream.
func (s *PgxStatementStore) LatestStatement(ctx context.Context, nostroID int64, stmtNumber string) (*BankStatement, bool, error) {
	return scanStatement(s.Pool.QueryRow(ctx,
		stmtSelect+` WHERE nostro_account_id = $1 AND statement_number = $2
		 ORDER BY COALESCE(sequence_number,0) DESC, id DESC LIMIT 1`,
		nostroID, stmtNumber))
}

// OpenExceptionFor dedupes break creation.
func (s *PgxStatementStore) OpenExceptionFor(ctx context.Context, typ string, instructionID, entryID int64) (bool, error) {
	var ok bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM settlement_exceptions
		     WHERE exception_type = $1 AND status IN ('OPEN','INVESTIGATING')
		       AND (($2::bigint IS NOT NULL AND settlement_instruction_id = $2)
		        OR  ($3::bigint IS NOT NULL AND statement_entry_id = $3)))`,
		typ, nullInt(instructionID), nullInt(entryID)).Scan(&ok)
	return ok, err
}

func nullInt(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// MissingPayments finds dispatched PAY-direction instructions whose
// settlement date has arrived without a matching statement entry or a
// settled nostro movement.
func (s *PgxStatementStore) MissingPayments(ctx context.Context, nostroID int64, ccy string, onOrBefore time.Time) ([]MissingPayment, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT si.id, si.trade_id, si.currency, si.amount::text,
		       si.settlement_date, COALESCE(si.swift_message_id,'')
		  FROM settlement_instructions si
		 WHERE si.nostro_account_id = $1 AND si.currency = $2
		   AND si.direction = 'PAY' AND si.settlement_date <= $3
		   AND si.swift_message_id IS NOT NULL
		   AND si.status NOT IN ('SETTLED','RECONCILED')
		   AND NOT EXISTS (
		       SELECT 1 FROM statement_entries e
		       JOIN bank_statements st ON st.id = e.statement_id
		        WHERE e.reconciled_instruction_id = si.id
		          AND e.status = 'MATCHED')`,
		nostroID, ccy, onOrBefore)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingPayment
	for rows.Next() {
		var m MissingPayment
		var amt string
		if err := rows.Scan(&m.InstructionID, &m.TradeID, &m.Currency, &amt,
			&m.SettlementDate, &m.Reference); err != nil {
			return nil, err
		}
		if m.Amount, err = decimal.NewFromString(amt); err != nil {
			return nil, fmt.Errorf("missing-payment %d amount: %w", m.InstructionID, err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListStatements pages the statement journal, newest first.
func (s *PgxStatementStore) ListStatements(ctx context.Context, nostroID int64, limit int) ([]BankStatement, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := stmtSelect
	var args []any
	if nostroID > 0 {
		q += ` WHERE nostro_account_id = $1`
		args = append(args, nostroID)
	}
	q += ` ORDER BY id DESC LIMIT ` + fmt.Sprintf("%d", limit)
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BankStatement
	for rows.Next() {
		var b BankStatement
		var open, close_ *string
		if err := rows.Scan(&b.ID, &b.NostroAccountID, &b.Format, &b.StatementNumber,
			&b.SequenceNumber, &b.StatementDate, &open, &close_, &b.Currency,
			&b.IBAN, &b.BIC, &b.ChecksumSHA256, &b.EntryCount, &b.MatchedCount,
			&b.Status, &b.IngestSource, &b.CreatedAt); err != nil {
			return nil, err
		}
		if open != nil {
			if d, derr := decimal.NewFromString(*open); derr == nil {
				b.OpeningBalance = &d
			}
		}
		if close_ != nil {
			if d, derr := decimal.NewFromString(*close_); derr == nil {
				b.ClosingBalance = &d
			}
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ListEntries returns one statement's normalized entries.
func (s *PgxStatementStore) ListEntries(ctx context.Context, statementID int64) ([]StatementEntryRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, COALESCE(entry_ref,''), COALESCE(bank_ref,''), COALESCE(uetr,''),
		       COALESCE(end_to_end_id,''), value_date, COALESCE(booking_date, value_date),
		       amount::text, currency, credit_debit_indicator, is_reversal,
		       COALESCE(transaction_code,''), COALESCE(remitter_name,''),
		       COALESCE(remitter_account,''), COALESCE(narrative,''),
		       COALESCE(match_key,''), status::text
		  FROM statement_entries WHERE statement_id = $1 ORDER BY id`, statementID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementEntryRow
	for rows.Next() {
		var r StatementEntryRow
		var amt, cd string
		if err := rows.Scan(&r.ID, &r.Entry.EntryRef, &r.Entry.BankRef, &r.Entry.UETR,
			&r.Entry.EndToEndID, &r.Entry.ValueDate, &r.Entry.BookingDate, &amt,
			&r.Entry.Currency, &cd, &r.Entry.IsReversal, &r.Entry.TxCode,
			&r.Entry.RemitterName, &r.Entry.RemitterAccount, &r.Entry.Narrative,
			&r.MatchKey, &r.Status); err != nil {
			return nil, err
		}
		if r.Entry.Amount, err = decimal.NewFromString(amt); err != nil {
			return nil, fmt.Errorf("entry %d amount: %w", r.ID, err)
		}
		r.Entry.Credit = cd == "CRDT"
		out = append(out, r)
	}
	return out, rows.Err()
}

// InTx runs fn inside a SERIALIZABLE transaction.
func (s *PgxStatementStore) InTx(ctx context.Context, fn func(ctx context.Context, tx StatementTx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("statement tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, pgxStatementTx{tx}); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("statement tx commit: %w", err)
	}
	return nil
}

type pgxStatementTx struct{ tx pgx.Tx }

func (t pgxStatementTx) InsertStatement(ctx context.Context, b BankStatement) (int64, error) {
	var id int64
	err := t.tx.QueryRow(ctx, `
		INSERT INTO bank_statements
		    (nostro_account_id, format, statement_number, sequence_number,
		     statement_date, opening_balance, closing_balance, currency,
		     iban, bic, file_checksum_sha256, status, ingest_source)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7::numeric,$8,$9,$10,$11,$12,$13)
		RETURNING id`,
		b.NostroAccountID, b.Format, nullStr(b.StatementNumber), b.SequenceNumber,
		b.StatementDate, decStr(b.OpeningBalance), decStr(b.ClosingBalance),
		b.Currency, nullStr(b.IBAN), nullStr(b.BIC), b.ChecksumSHA256,
		b.Status, b.IngestSource).Scan(&id)
	return id, err
}

func decStr(d *decimal.Decimal) any {
	if d == nil {
		return nil
	}
	return d.String()
}

func (t pgxStatementTx) InsertEntry(ctx context.Context, stmtID int64, e ParsedEntry) (int64, bool, error) {
	cd := "DBIT"
	if e.Credit {
		cd = "CRDT"
	}
	var id int64
	err := t.tx.QueryRow(ctx, `
		INSERT INTO statement_entries
		    (statement_id, entry_ref, bank_ref, uetr, end_to_end_id,
		     value_date, booking_date, amount, currency, credit_debit_indicator,
		     is_reversal, transaction_code, remitter_name, remitter_account, narrative)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::numeric,$9,$10,$11,$12,$13,$14,$15)
		ON CONFLICT (statement_id, COALESCE(entry_ref,''), COALESCE(uetr,''),
		             value_date, amount, credit_debit_indicator) DO NOTHING
		RETURNING id`,
		stmtID, nullStr(e.EntryRef), nullStr(e.BankRef), nullStr(e.UETR),
		nullStr(e.EndToEndID), e.ValueDate, nullTime(e.BookingDate),
		e.Amount.String(), strings.ToUpper(e.Currency), cd, e.IsReversal,
		nullStr(e.TxCode), nullStr(e.RemitterName), nullStr(e.RemitterAccount),
		nullStr(e.Narrative)).Scan(&id)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return 0, true, nil
	}
	return id, false, err
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (t pgxStatementTx) SetStatementStatus(ctx context.Context, id int64, status string, matched, total int) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE bank_statements
		   SET status = $2, matched_count = $3, entry_count = $4
		 WHERE id = $1`, id, status, matched, total)
	return err
}

// MatchByUETR — highest priority: UETR against rail_payments /
// cls_settlement_instructions.
func (t pgxStatementTx) MatchByUETR(ctx context.Context, uetr, ccy string) (*EntryMatch, bool, error) {
	var m EntryMatch
	var pid, cid int64
	err := t.tx.QueryRow(ctx, `
		SELECT id FROM rail_payments
		 WHERE uetr = $1 AND status <> 'FAILED' LIMIT 1`, uetr).Scan(&pid)
	if err == nil {
		m.PaymentID = &pid
		m.Key = "UETR"
		return &m, true, nil
	}
	if !stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	err = t.tx.QueryRow(ctx, `
		SELECT id FROM cls_settlement_instructions
		 WHERE uetr = $1 LIMIT 1`, uetr).Scan(&cid)
	if err == nil {
		m.ClsID = &cid
		m.Key = "UETR"
		return &m, true, nil
	}
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return nil, false, err
}

// MatchByRef — transaction reference against settlement instruction
// swift_message_id / rail payment end_to_end_id / CLS instruction_ref.
func (t pgxStatementTx) MatchByRef(ctx context.Context, ref, ccy string) (*EntryMatch, bool, error) {
	var m EntryMatch
	var iid int64
	var amt string
	err := t.tx.QueryRow(ctx, `
		SELECT id, amount::text FROM settlement_instructions
		 WHERE swift_message_id = $1 AND currency = $2
		 ORDER BY id LIMIT 1`, ref, ccy).Scan(&iid, &amt)
	if err == nil {
		m.InstructionID = &iid
		m.Key = "REF"
		d, derr := decimal.NewFromString(amt)
		if derr == nil {
			m.ExpectedAmount = &d
		}
		return &m, true, nil
	}
	if !stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	var pid int64
	err = t.tx.QueryRow(ctx, `
		SELECT id FROM rail_payments WHERE end_to_end_id = $1 LIMIT 1`, ref).Scan(&pid)
	if err == nil {
		m.PaymentID = &pid
		m.Key = "REF"
		return &m, true, nil
	}
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return nil, false, err
}

// MatchByAmount — amount + currency + exact value date against unsettled
// nostro movements (joined through settlement_instructions so the value
// date match is the instruction's settlement_date).
func (t pgxStatementTx) MatchByAmount(ctx context.Context, ccy string, amount decimal.Decimal,
	valueDate time.Time, credit bool) (*EntryMatch, bool, error) {
	dir := "CREDIT"
	if !credit {
		dir = "DEBIT"
	}
	var m EntryMatch
	var mid, iid int64
	var amt string
	err := t.tx.QueryRow(ctx, `
		SELECT nm.id, nm.settlement_instruction_id, nm.amount::text
		  FROM nostro_movements nm
		  JOIN settlement_instructions si ON si.id = nm.settlement_instruction_id
		 WHERE nm.currency = $1 AND nm.direction = $2
		   AND nm.amount = $3::numeric AND si.settlement_date = $4
		   AND nm.status = 'POSTED'
		 ORDER BY nm.id LIMIT 1`,
		ccy, dir, amount.String(), valueDate).Scan(&mid, &iid, &amt)
	if stderrors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	m.MovementID = &mid
	m.InstructionID = &iid
	m.Key = "AMOUNT"
	return &m, true, nil
}

func (t pgxStatementTx) SetEntryMatch(ctx context.Context, entryID int64, m EntryMatch) error {
	tag, err := t.tx.Exec(ctx, `
		UPDATE statement_entries
		   SET reconciled_instruction_id = $2, reconciled_payment_id = $3,
		       reconciled_movement_id = $4, match_key = $5, status = 'MATCHED'
		 WHERE id = $1 AND status = 'UNMATCHED'`,
		entryID, m.InstructionID, m.PaymentID, m.MovementID, m.Key)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New(CodeSettlementStateConflict,
			fmt.Sprintf("statement entry %d already resolved — duplicate credit refused", entryID))
	}
	return nil
}

func (t pgxStatementTx) MarkEntryStatus(ctx context.Context, entryID int64, status string) error {
	_, err := t.tx.Exec(ctx, `
		UPDATE statement_entries SET status = $2 WHERE id = $1`, entryID, status)
	return err
}

func (t pgxStatementTx) InsertException(ctx context.Context, e SettlementException) (int64, error) {
	return insertSettlementException(ctx, t.tx, e)
}
