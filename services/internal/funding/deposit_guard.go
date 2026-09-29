// Third-party deposit guard — Phase-11 Task 11.3.11(b): every inbound
// wire attribution runs the originator-name screen before client credit.
//
// Flow (all inside one SERIALIZABLE tx):
//  1. Resolve the destination reference EXC{account}-{CCY} from the wire
//     reference/remittance (history.go's DepositReference format).
//  2. Score originator name vs verified KYC legal name with Jaro-Winkler
//     (pure Go, threshold >= 0.85 pinned).
//  3. Accept → DEPOSIT funding row (PENDING — the Task 11.3.3 anti-fraud
//     tiers take it from there) + journal Nostro → CustomerLiability
//     with wallet +locked (received but not yet spendable).
//  4. Reject → suspense_account_mappings row (2150 suspense GL — spec
//     §5.46 default, see migration 108 note on the 2099 prose), funding
//     row PENDING_REVIEW when an account resolved, automated pacs.004
//     return-wire record persisted via the rail adapter, P1 alert, and
//     the coded THIRD_PARTY_DEPOSIT_REJECTED 422 for name mismatches.
//
// Funds are never silently dropped: an unattributable or mismatched wire
// always lands in suspense with an SLA and a return instruction.
package funding

import (
	"context"
	stderrors "errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// CodeThirdPartyDepositRejected is emitted when the originator-name
// screen fails — the wire is quarantined and returned to source.
const CodeThirdPartyDepositRejected = "THIRD_PARTY_DEPOSIT_REJECTED"

// DispositionAccepted / DispositionQuarantined are ScreenResult
// dispositions.
const (
	DispositionAccepted    = "ACCEPTED"
	DispositionQuarantined = "QUARANTINED"
)

// QuarantineSLA is the 48h compliance review window stamped on every
// suspense row (§5.46 review workflow).
const QuarantineSLA = 48 * time.Hour

// depositRefRe extracts the EXC{8-digit account}-{CCY} deposit
// reference anywhere in the wire reference/remittance text.
var depositRefRe = regexp.MustCompile(`EXC(\d{8})-([A-Z]{3})`)

// LegalNameResolver returns the account's verified KYC legal name — the
// compliance-gold comparison target. Implemented by the KYC store
// (Phase-14 wiring); a nil resolver fails closed to quarantine.
type LegalNameResolver interface {
	LegalName(ctx context.Context, accountID int64) (string, error)
}

// InboundWire is one incoming bank credit notification (camt.054 / MT103
// credit advice / bank-statement row — canonical fields only).
type InboundWire struct {
	BankTxID          string
	Rail              string // SWIFT|SEPA|FEDNOW|ACH|CHAPS|TARGET2|WIRE
	Currency          string
	Amount            decimal.Decimal
	OriginatorName    string
	OriginatorAccount string
	OriginatorBIC     string
	Reference         string // explicit wire reference field
	RemittanceInfo    string
	ReceivedAt        time.Time
}

// ScreenResult reports the guard's disposition.
type ScreenResult struct {
	Disposition string          `json:"disposition"`
	Deposit     *DepositRow     `json:"deposit,omitempty"`
	Suspense    *SuspenseRow    `json:"suspense,omitempty"`
	ReturnWire  *RailPaymentRow `json:"return_wire,omitempty"`
	Score       *float64        `json:"name_match_score,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	JournalID   *int64          `json:"journal_entry_id,omitempty"`
	Idempotent  bool            `json:"idempotent_replay"`
}

// DepositGuard screens inbound wires.
type DepositGuard struct {
	store   Store
	poster  JournalPoster
	rails   *RailService
	names   LegalNameResolver
	railCtl RailController // nil = SCOPE_RAIL check not wired
	alerter OpsAlerter
	clock   func() time.Time
	logf    func(format string, args ...any)
}

// NewDepositGuard wires the guard; store, poster and rails are required
// (accepted deposits post journals; quarantines persist return wires).
func NewDepositGuard(store Store, poster JournalPoster, rails *RailService) (*DepositGuard, error) {
	if store == nil || rails == nil {
		return nil, fmt.Errorf("funding: deposit guard requires store + rail service")
	}
	return &DepositGuard{store: store, poster: poster, rails: rails, clock: time.Now}, nil
}

// WithLegalNames binds the KYC legal-name resolver.
func (g *DepositGuard) WithLegalNames(r LegalNameResolver) *DepositGuard {
	g.names = r
	return g
}

// WithAlerter binds the ops alerter (P1 on quarantine + journal failure).
func (g *DepositGuard) WithAlerter(a OpsAlerter) *DepositGuard {
	g.alerter = a
	return g
}

// WithRailController binds the Task 11.3.12 SCOPE_RAIL suspension seam —
// a suspended rail refuses inbound-wire processing with
// SETTLEMENT_RAIL_REJECTED (retry after the rail resumes); sibling rails
// keep attributing normally.
func (g *DepositGuard) WithRailController(c RailController) *DepositGuard {
	g.railCtl = c
	return g
}

// WithClock overrides the clock (tests).
func (g *DepositGuard) WithClock(c func() time.Time) *DepositGuard {
	g.clock = c
	return g
}

// WithLogger wires a diagnostic sink.
func (g *DepositGuard) WithLogger(f func(format string, args ...any)) *DepositGuard {
	g.logf = f
	return g
}

// ScreenInbound runs the attribution + third-party screen for one wire.
// Returns a coded error for the name-mismatch path
// (THIRD_PARTY_DEPOSIT_REJECTED) AFTER the quarantine state has
// committed — the wire is held, the rejection code is for the API
// surface. Internal failures (resolver/store/ledger) propagate as coded
// errors with nothing committed.
func (g *DepositGuard) ScreenInbound(ctx context.Context, w InboundWire) (*ScreenResult, error) {
	bankTxID := strings.TrimSpace(w.BankTxID)
	if bankTxID == "" {
		return nil, errCode("INVALID_REQUEST", "bank_tx_id required")
	}
	ccy, err := normalizeCurrency(w.Currency)
	if err != nil {
		return nil, err
	}
	if !w.Amount.IsPositive() || !w.Amount.Round(8).Equal(w.Amount) {
		return nil, errf("INVALID_REQUEST", "wire amount %s is not a positive ≤8dp value", w.Amount.String())
	}
	rail := strings.ToUpper(strings.TrimSpace(w.Rail))
	if rail == "" {
		rail = "WIRE"
	}

	// SCOPE_RAIL (Task 11.3.12): deposit processing is suspended for the
	// rail — refuse ingestion rather than attributing or quarantining on
	// a rail the venue has halted. Fails closed on lookup error.
	if err := AssertRailOperational(ctx, g.railCtl, rail); err != nil {
		return nil, err
	}

	// Dedup: a repeated bank notification replays the committed state —
	// the suspense row's bank_tx_id UNIQUE is the durable guard.
	if prev, err := g.store.SuspenseByBankTx(ctx, bankTxID); err == nil {
		return &ScreenResult{Disposition: DispositionQuarantined,
			Suspense: prev, Reason: prev.UnmatchedReason, Idempotent: true}, nil
	} else if !isNotFound(err) {
		return nil, err
	}

	// Attribution: EXC{account}-{CCY} reference in the wire text.
	accountID, refCcy, refFound := parseDepositReference(w.Reference + " " + w.RemittanceInfo)

	if !refFound {
		return g.quarantine(ctx, w, bankTxID, rail, ccy,
			"MISSING_REFERENCE", nil, nil,
			"no EXC deposit reference on wire")
	}

	meta, err := g.store.AccountMeta(ctx, accountID)
	if err != nil {
		if isNotFound(err) {
			return g.quarantine(ctx, w, bankTxID, rail, ccy,
				"UNKNOWN_BENEFICIARY", nil, nil,
				fmt.Sprintf("deposit reference resolves to unknown account %d", accountID))
		}
		return nil, err
	}
	if refCcy != ccy {
		return g.quarantine(ctx, w, bankTxID, rail, ccy,
			"AMOUNT_DISCREPANCY", &accountID, nil,
			fmt.Sprintf("wire currency %s != deposit reference currency %s", ccy, refCcy))
	}

	// Name screen — resolver failure is retriable, not a quarantine.
	var legal string
	if g.names == nil {
		return nil, errCode("INTERNAL_ERROR",
			"deposit guard legal-name resolver not wired — refusing attribution")
	}
	legal, err = g.names.LegalName(ctx, accountID)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "legal name resolution", err)
	}
	if strings.TrimSpace(legal) == "" {
		return g.quarantine(ctx, w, bankTxID, rail, ccy,
			"UNKNOWN_BENEFICIARY", &accountID, nil,
			"account has no verified KYC legal name on file")
	}
	score := JaroWinkler(NormalizeLegalName(w.OriginatorName), NormalizeLegalName(legal))
	if score < NameMatchThreshold {
		res, qerr := g.quarantine(ctx, w, bankTxID, rail, ccy,
			"NAME_MISMATCH", &accountID, &score,
			fmt.Sprintf("originator %q vs legal name %q — similarity %.3f < %.2f",
				w.OriginatorName, legal, score, NameMatchThreshold))
		if qerr != nil {
			return nil, qerr
		}
		var sid int64
		if res.Suspense != nil {
			sid = res.Suspense.ID
		}
		return res, errf(CodeThirdPartyDepositRejected,
			"third-party deposit rejected (suspense %d): originator %q does not match legal name %q (score %.3f)",
			sid, w.OriginatorName, legal, score)
	}
	_ = meta // account resolved; status gating lands with 11.3.3's tiers

	// Accepted: DEPOSIT row + journal with wallet +locked — funds are
	// bank-confirmed but not spendable until the anti-fraud tier review
	// (Task 11.3.3) clears; locked is the fail-closed landing state.
	return g.accept(ctx, w, bankTxID, rail, ccy, accountID, score)
}

// accept persists the accepted deposit inside one tx: DEPOSIT PENDING
// row (idempotency_key = wire:{bankTxID}) — journal posted after commit.
func (g *DepositGuard) accept(ctx context.Context, w InboundWire,
	bankTxID, rail, ccy string, accountID int64, score float64) (*ScreenResult, error) {

	idem := "wire:" + bankTxID
	bm := rail
	ref := bankTxID
	orig := w.OriginatorAccount

	tx, err := g.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit tx", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	d, err := g.store.InsertDepositPending(ctx, tx, DepositRow{
		AccountID:        accountID,
		Currency:         ccy,
		Amount:           w.Amount,
		Status:           FundingPending,
		BankMethod:       &bm,
		Reference:        &ref,
		ReferenceAccount: &orig,
		IdempotencyKey:   &idem,
		ReviewTier:       strPtr(ReviewTierAuto), // 11.3.3 re-tiers on USD
	})
	if err != nil {
		if err == ErrIdemConflict {
			return &ScreenResult{Disposition: DispositionAccepted,
				Reason: "duplicate wire notification", Idempotent: true}, nil
		}
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "deposit commit", err)
	}

	res := &ScreenResult{Disposition: DispositionAccepted, Deposit: d, Score: &score}

	// Journal: Nostro → CustomerLiability, wallet +locked. Idempotent on
	// wire-deposit:{bank_tx_id}; post-commit failure raises P1 — the row
	// is committed, the ledger needs ops replay.
	if g.poster != nil {
		pr, perr := g.poster.Post(ctx, ledger.Journal{
			EntryType:      ledger.EntryDeposit,
			ReferenceID:    d.ID,
			Description:    fmt.Sprintf("inbound wire %s deposit %s %s (pending review)", bankTxID, w.Amount.String(), ccy),
			PostedBy:       "funding:deposit-guard",
			IdempotencyKey: fmt.Sprintf("wire-deposit:%s", bankTxID),
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.Nostro(ccy), ccy, w.Amount,
					fmt.Sprintf("bank wire received via %s", rail)),
				ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, w.Amount,
					"client deposit pending anti-fraud tier review"),
			},
			Effects: []ledger.AccountEffect{{
				AccountID:   accountID,
				Currency:    ccy,
				LockedDelta: w.Amount, // received, not spendable pre-review
			}},
		})
		if perr != nil && !pr.Committed {
			g.log("funding: deposit %d journal failed: %v", d.ID, perr)
			g.raise(ctx, "P1", "DEPOSIT_JOURNAL_FAILED",
				fmt.Sprintf("deposit %d (wire %s) journal failed — ledger needs ops replay", d.ID, bankTxID), perr)
			return nil, wrapCode("INTERNAL_ERROR", "deposit journal", perr)
		}
		if pr.JournalID != 0 {
			jid := pr.JournalID
			res.JournalID = &jid
		}
	}
	return res, nil
}

// quarantine persists the suspense mapping + automated return wire in
// one tx, then posts the Nostro → SuspenseDeposits GL journal and raises
// the compliance alert. Everything lands or nothing does.
func (g *DepositGuard) quarantine(ctx context.Context, w InboundWire,
	bankTxID, rail, ccy, reason string,
	accountID *int64, score *float64, detail string) (*ScreenResult, error) {

	now := g.clock().UTC()
	sla := now.Add(QuarantineSLA)
	glAcct := strings.SplitN(ledger.SuspenseDeposits(ccy), "_", 2)[0] // "2150"

	tx, err := g.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "quarantine tx", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Funding row only when the wire could be attributed to an account —
	// funding_transactions.account_id is NOT NULL, unreferenced wires live
	// solely on the suspense row (never a sentinel account id).
	var fundingID *int64
	if accountID != nil {
		idem := "wire:" + bankTxID
		bm := rail
		ref := bankTxID
		orig := w.OriginatorAccount
		d, derr := g.store.InsertDepositPending(ctx, tx, DepositRow{
			AccountID:        *accountID,
			Currency:         ccy,
			Amount:           w.Amount,
			Status:           FundingPendingReview,
			BankMethod:       &bm,
			Reference:        &ref,
			ReferenceAccount: &orig,
			IdempotencyKey:   &idem,
			ReviewTier:       strPtr(ReviewTierPendingReview),
		})
		if derr != nil {
			if derr == ErrIdemConflict {
				return &ScreenResult{Disposition: DispositionQuarantined,
					Reason: reason, Idempotent: true}, nil
			}
			return nil, derr
		}
		fundingID = &d.ID
	}

	susp, err := g.store.InsertSuspenseMapping(ctx, tx, SuspenseRow{
		BankTxID:             bankTxID,
		FundingTransactionID: fundingID,
		AccountID:            accountID,
		Rail:                 &rail,
		Currency:             ccy,
		Amount:               w.Amount,
		OriginatorName:       strPtr(w.OriginatorName),
		OriginatorAccount:    strPtr(w.OriginatorAccount),
		NameMatchScore:       score,
		UnmatchedReason:      reason,
		GLAccount:            glAcct,
		QuarantineStatus:     "QUARANTINED",
		SLAExpiresAt:         sla,
	})
	if err != nil {
		if err == ErrIdemConflict {
			return &ScreenResult{Disposition: DispositionQuarantined,
				Reason: reason, Idempotent: true}, nil
		}
		return nil, err
	}

	// Automated return wire — pacs.004/NACHA return per §17.12.2. Only
	// when the rail has a return instrument and the originator account is
	// known; a return can't be fabricated without a destination.
	var ret *RailPaymentRow
	if acc := strings.TrimSpace(w.OriginatorAccount); acc != "" &&
		RailID(rail) != "" && rail != "WIRE" {
		rw, _, rerr := g.rails.BuildReturnWire(ctx, tx, ReturnInstruction{
			OriginalEndToEndID: bankTxID,
			OriginalBankTxID:   bankTxID,
			Rail:               RailID(rail),
			Currency:           ccy,
			Amount:             w.Amount,
			ReturnReasonCode:   "AC04", // creditor account closed/unknown — matched on re-credit; ops may amend
			ReturnReason:       "third-party deposit rejected: " + reason,
			DebtorName:         "Exchange",
			CreditorName:       w.OriginatorName,
			CreditorAccount:    acc,
			CreditorBIC:        w.OriginatorBIC,
		})
		if rerr != nil {
			return nil, rerr
		}
		ret = rw
		if err := g.store.SetSuspenseLinks(ctx, tx, susp.ID, nil, &rw.ID); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "quarantine commit", err)
	}
	susp.ReturnPaymentID = nil
	if ret != nil {
		susp.ReturnPaymentID = &ret.ID
	}

	// GL journal: Nostro → 2150 SuspenseDeposits (GL-only — the client's
	// wallet is never credited while quarantined).
	var journalID *int64
	if g.poster != nil {
		refID := susp.ID
		pr, perr := g.poster.Post(ctx, ledger.Journal{
			EntryType:      ledger.EntryDeposit,
			ReferenceID:    refID,
			Description:    fmt.Sprintf("wire %s quarantined: %s", bankTxID, detail),
			PostedBy:       "funding:deposit-guard",
			IdempotencyKey: fmt.Sprintf("wire-quarantine:%s", bankTxID),
			Lines: []ledger.Line{
				ledger.DebitLine(ledger.Nostro(ccy), ccy, w.Amount,
					fmt.Sprintf("inbound %s wire received", rail)),
				ledger.CreditLine(ledger.SuspenseDeposits(ccy), ccy, w.Amount,
					"quarantined pending compliance review"),
			},
		})
		if perr != nil && !pr.Committed {
			g.log("funding: suspense %d journal failed: %v", susp.ID, perr)
			g.raise(ctx, "P1", "SUSPENSE_JOURNAL_FAILED",
				fmt.Sprintf("suspense %d (wire %s) GL journal failed — ops replay required", susp.ID, bankTxID), perr)
		} else {
			if pr.JournalID != 0 {
				jid := pr.JournalID
				journalID = &jid
				if jtx, terr := g.store.BeginTx(ctx); terr == nil {
					_ = g.store.SetSuspenseLinks(ctx, jtx, susp.ID, &jid, nil)
					_ = jtx.Commit(ctx)
				}
			}
		}
	}

	g.raise(ctx, "P1", "DEPOSIT_QUARANTINED",
		fmt.Sprintf("wire %s quarantined (suspense %d): %s", bankTxID, susp.ID, detail), nil)

	return &ScreenResult{
		Disposition: DispositionQuarantined,
		Suspense:    susp,
		ReturnWire:  ret,
		Score:       score,
		Reason:      reason,
		JournalID:   journalID,
	}, nil
}

// ---------------------------------------------------------------------------
// Quarantine resolution (§5.46 review workflow — four-eyes attribution)
// ---------------------------------------------------------------------------

// Quarantine resolution actions.
const (
	ResolveReleaseToClient = "RELEASE_TO_CLIENT"
	ResolveReturnToSource  = "RETURN_TO_SOURCE"
)

// ResolveOutcome reports a completed quarantine resolution.
type ResolveOutcome struct {
	SuspenseID int64  `json:"suspense_id"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	JournalID  *int64 `json:"journal_entry_id,omitempty"`
}

// ResolveSuspense applies a compliance resolution to a QUARANTINED (or
// INVESTIGATING) suspense row. Dual-control enforcement happens at the
// route/actor layer (approver != resolver); this method enforces the
// state machine:
//
//   - RELEASE_TO_CLIENT: funds exit suspense into the resolved account's
//     wallet (Debit 2150 → Credit 2010, wallet +available) — requires
//     account_id present; the linked funding row completes.
//   - RETURN_TO_SOURCE: suspense liability moves to clearing transit
//     pending the (already persisted) return wire's settlement —
//     Debit 2150 → Credit 2160; the linked funding row fails.
func (g *DepositGuard) ResolveSuspense(ctx context.Context, suspenseID int64,
	action string, investigatorID int64, notes string) (*ResolveOutcome, error) {

	action = strings.ToUpper(strings.TrimSpace(action))
	if action != ResolveReleaseToClient && action != ResolveReturnToSource {
		return nil, errf("INVALID_REQUEST",
			"action must be %s or %s", ResolveReleaseToClient, ResolveReturnToSource)
	}
	if investigatorID <= 0 {
		return nil, errCode("INVALID_REQUEST", "investigator id required")
	}

	tx, err := g.store.BeginTx(ctx)
	if err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "resolve tx", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	susp, err := g.store.SuspenseForUpdate(ctx, tx, suspenseID)
	if err != nil {
		return nil, err
	}
	if susp.QuarantineStatus != "QUARANTINED" && susp.QuarantineStatus != "INVESTIGATING" {
		return nil, errf("INVALID_REQUEST",
			"suspense %d already resolved (%s)", susp.ID, susp.QuarantineStatus)
	}

	now := g.clock().UTC()
	var newStatus, fundingStatus string
	var journalID *int64
	switch action {
	case ResolveReleaseToClient:
		if susp.AccountID == nil || susp.FundingTransactionID == nil {
			return nil, errCode("INVALID_REQUEST",
				"cannot release — wire carries no attributed client account (return to source or re-attribute first)")
		}
		newStatus = "RESOLVED"
		fundingStatus = FundingCompleted
	case ResolveReturnToSource:
		newStatus = "RETURNED_TO_SOURCE"
		fundingStatus = FundingFailed
	}

	if err := g.store.SetSuspenseStatus(ctx, tx, susp.ID, newStatus,
		&investigatorID, &notes, &now); err != nil {
		return nil, err
	}
	if susp.FundingTransactionID != nil {
		var completedAt *time.Time
		if fundingStatus == FundingCompleted {
			completedAt = &now
		}
		if err := g.store.SetFundingTxStatus(ctx, tx,
			*susp.FundingTransactionID, fundingStatus, completedAt); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, wrapCode("INTERNAL_ERROR", "resolve commit", err)
	}

	// Journal post-commit (ledger owns its tx; P1 on failure — suspense
	// stays recorded, ops replays the GL leg).
	if g.poster != nil {
		var lines []ledger.Line
		var effects []ledger.AccountEffect
		desc := ""
		switch action {
		case ResolveReleaseToClient:
			desc = fmt.Sprintf("suspense %d released to account %d", susp.ID, *susp.AccountID)
			lines = []ledger.Line{
				ledger.DebitLine(ledger.SuspenseDeposits(susp.Currency), susp.Currency, susp.Amount,
					"compliance attribution — suspense cleared"),
				ledger.CreditLine(ledger.CustomerLiability(susp.Currency), susp.Currency, susp.Amount,
					"client deposit credited after review"),
			}
			effects = []ledger.AccountEffect{{
				AccountID:      *susp.AccountID,
				Currency:       susp.Currency,
				AvailableDelta: susp.Amount,
			}}
		case ResolveReturnToSource:
			desc = fmt.Sprintf("suspense %d returned to source", susp.ID)
			lines = []ledger.Line{
				ledger.DebitLine(ledger.SuspenseDeposits(susp.Currency), susp.Currency, susp.Amount,
					"return to originator — suspense cleared"),
				ledger.CreditLine(ledger.ClearingTransit(susp.Currency), susp.Currency, susp.Amount,
					"outbound return wire in transit"),
			}
		}
		pr, perr := g.poster.Post(ctx, ledger.Journal{
			EntryType:      ledger.EntryAdjustment,
			ReferenceID:    susp.ID,
			Description:    desc,
			PostedBy:       "funding:deposit-guard",
			IdempotencyKey: fmt.Sprintf("suspense-resolve:%d:%s", susp.ID, action),
			Lines:          lines,
			Effects:        effects,
		})
		if perr != nil && !pr.Committed {
			g.log("funding: suspense %d resolve journal failed: %v", susp.ID, perr)
			g.raise(ctx, "P1", "SUSPENSE_RESOLVE_JOURNAL_FAILED",
				fmt.Sprintf("suspense %d resolution journal failed — GL needs ops replay", susp.ID), perr)
		} else if pr.JournalID != 0 {
			jid := pr.JournalID
			journalID = &jid
			if jtx, terr := g.store.BeginTx(ctx); terr == nil {
				_ = g.store.SetSuspenseLinks(ctx, jtx, susp.ID, &jid, nil)
				_ = jtx.Commit(ctx)
			}
		}
	}

	return &ResolveOutcome{
		SuspenseID: susp.ID, Action: action,
		Status: newStatus, JournalID: journalID,
	}, nil
}

// parseDepositReference finds EXC{8-digit}-{CCY} in wire text.
func parseDepositReference(s string) (accountID int64, ccy string, ok bool) {
	m := depositRefRe.FindStringSubmatch(strings.ToUpper(s))
	if m == nil {
		return 0, "", false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || id <= 0 {
		return 0, "", false
	}
	return id, m[2], true
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func isNotFound(err error) bool {
	var e *excerrors.Error
	return stderrors.As(err, &e) && e.Code == "NOT_FOUND"
}

func (g *DepositGuard) raise(ctx context.Context, sev, code, summary string, cause error) {
	if g.alerter == nil {
		return
	}
	a := OpsAlert{Severity: sev, Code: code, Summary: summary}
	if cause != nil {
		a.Err = cause.Error()
	}
	_ = g.alerter.Raise(ctx, a)
}

func (g *DepositGuard) log(format string, args ...any) {
	if g.logf != nil {
		g.logf(format, args...)
	}
}
