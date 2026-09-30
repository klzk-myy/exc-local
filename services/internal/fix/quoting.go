// Package fix implements the Phase-18 FIX protocol gateway surfaces.
//
// quoting.go — Task 18.3.7: FIX Mass Quoting (Tag 35=i) and Mass Quote
// Cancel (Tag 35=Z) for institutional Liquidity Providers, bound to the
// Task 18.3.10 mm_programs entitlement, with the strict 100% Firm
// Liquidity (No Last Look) rulebook policy per FX Global Code
// Principle 17 (spec §9.4, §6.4, §24 #128).
//
// The layer is transport-agnostic by design: session.go (sibling task)
// owns QuickFIX acceptors and tag framing; this file consumes already-
// parsed quote structures so the firm-liquidity checks, the entitlement
// binding and the quote-set lifecycle are unit-testable without a live
// FIX session. Quotes ride the canonical order admission pipeline
// (orders.Service.Submit → validate → persist intent → Aeron dispatch)
// as ordinary GTC LIMIT orders flagged quote-sourced via the
// "mq:<set>:<gen>:<entry>:<side>" client_order_id and the FIX session's
// session_id attribution — which is also what the MMP mass-cancel and
// cancel-on-disconnect paths scope against.
package fix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"exchange/internal/config"
	"exchange/internal/marketmaking"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// FIX quote-message vocabulary (FIX 4.4/5.0 SP2).
const (
	// QuoteStatus values — MassQuoteAcknowledgement (35=b) / per entry.
	QuoteStatusAccepted = 0
	QuoteStatusRejected = 5

	// QuoteRejectReason per the §24.x matrix: rejections surface with
	// reason 99 (Other) plus a machine-readable Text token.
	QuoteRejectReasonOther = 99

	// QuoteCancelType (35=Z, tag 298) — per the task text.
	QuoteCancelPerSymbol = 1
	QuoteCancelAllQuotes = 4
)

// QuoteEntry is one NoQuoteEntries (295) leg: a two-sided price update
// for one instrument. A nil side leaves that side unchanged in intent —
// in practice the venue's obligation model expects two-sided flow, and
// an entry with neither side present is rejected.
type QuoteEntry struct {
	QuoteEntryID string
	Symbol       string
	BidPx        *decimal.Decimal
	BidSize      *decimal.Decimal
	OfferPx      *decimal.Decimal
	OfferSize    *decimal.Decimal
}

// MassQuote is the parsed 35=i body. HoldTime is any requested hold /
// last-look window the sender attaches (FIX does not define a hold-time
// tag on MassQuote — custom tags like 20010 carry it): a non-zero value
// is a last-look request and the whole set rejects QUOTE_REQUEST_REJECTED
// under the 100% firm-liquidity rule.
type MassQuote struct {
	QuoteSetID string        // tag 302
	Entries    []QuoteEntry  // NoQuoteSets/Entries
	HoldTime   time.Duration // non-zero ⇒ last-look attempt, rejected
}

// QuoteAckEntry is the per-entry MassQuoteAcknowledgement line.
type QuoteAckEntry struct {
	QuoteEntryID string
	Symbol       string
	Status       int // QuoteStatusAccepted | QuoteStatusRejected
	RejectReason int
	Text         string
}

// MassQuoteAck is the 35=b response: per-entry acceptance/rejection.
type MassQuoteAck struct {
	QuoteSetID string
	Entries    []QuoteAckEntry
}

// Accepted reports whether every entry was accepted.
func (a *MassQuoteAck) Accepted() bool {
	for _, e := range a.Entries {
		if e.Status != QuoteStatusAccepted {
			return false
		}
	}
	return true
}

// QuoteCancel is the parsed 35=Z body. QuoteSetID, when non-empty,
// scopes the cancel to one previously-acknowledged set; CancelType 1
// scopes per symbol; CancelType 4 mass-cancels all of the session's
// quotes.
type QuoteCancel struct {
	QuoteSetID    string // optional: cancel just this set
	CancelType    int    // tag 298
	Symbol        string // required for CancelType=1
	QuoteCancelID string // tag 299 (echoed on the report)
}

// ---------------------------------------------------------------------------
// Seams — production wiring satisfies these with *orders.Service and
// *marketmaking.Service / *marketmaking.MMPTracker; tests substitute fakes.
// ---------------------------------------------------------------------------

// QuotePipeline is the narrow slice of *orders.Service the quoting path
// needs — routing every quote through the canonical order admission
// pipeline (validation, dedup, OTR counting, kill-switch, Aeron
// dispatch) instead of inventing a second ingress (spec §8.4 layer
// boundary). Submit/Cancel/AccountByID deliberately overlap the sibling
// OrderFlow seam (types.go — Task 18.3.2's shared FIX-order surface);
// quoting adds InstrumentBySymbol (mm entitlement binding) and
// MassCancel (35=Z scopes).
type QuotePipeline interface {
	AccountByID(ctx context.Context, id int64) (*orders.Account, error)
	InstrumentBySymbol(ctx context.Context, symbol string) (*orders.Instrument, error)
	Submit(ctx context.Context, acct *orders.Account, req *orders.SubmitRequest) (*orders.Ack, error)
	Cancel(ctx context.Context, acct *orders.Account, orderID int64,
		actor, requestID, ip string) (*orders.Ack, error)
	MassCancel(ctx context.Context, scope orders.MassCancelScope,
		actor, requestID, ip string) (*orders.MassCancelResult, error)
}

// EntitlementSource resolves the ACTIVE mm_programs row covering
// (account, instrument) — *marketmaking.Service satisfies it.
type EntitlementSource interface {
	Entitled(ctx context.Context, accountID, instrumentID int64) (*marketmaking.Program, error)
}

// LockoutSource reports the MMP lockout for (account, instrument) —
// *marketmaking.MMPTracker satisfies it.
type LockoutSource interface {
	MMPLocked(ctx context.Context, accountID, instrumentID int64) bool
}

// ComplianceObserver records one obligation sample per applied quote —
// *marketmaking.Service satisfies it. Nil disables sampling.
type ComplianceObserver interface {
	ObserveQuote(ctx context.Context, accountID, instrumentID int64,
		bidPx, bidQty, offerPx, offerQty *decimal.Decimal) error
}

// LPAccountResolver resolves the liquidity_providers entity owning the
// session's quoting account via lp_accounts (migration 271) —
// *marketmaking.PgStore satisfies it (LPForAccount). The FIX session
// knows only its bound account; this is the account→LP hop the
// SCOPE_LP kill-switch (Phase-11 Task 11.3.12, spec §24 #409) needs.
// lpID==0 means the account is not LP-bound: the LP scope does not
// apply and quoting proceeds under the mm_programs entitlement alone.
type LPAccountResolver interface {
	LPForAccount(ctx context.Context, accountID int64) (int64, error)
}

// LPGuard reports whether an LP entity is kill-switched —
// *admin.KillSwitchResolver satisfies it (LPSuspended over the
// `halt:lp:{lp_id}` flag; SCOPE_LP target_id is the
// liquidity_providers.lp_id in string form). Errors MUST fail closed:
// an unverifiable suspension state rejects quoting rather than admit.
type LPGuard interface {
	LPSuspended(ctx context.Context, lpID string) (bool, string, error)
}

// ---------------------------------------------------------------------------
// Task 7.3.9 feed seam — accepted quotes/withdrawals toward market-data
// distribution (per-LP pricing: lpBook@{lpID}/{symbol}).
// ---------------------------------------------------------------------------

// LPQuoteEventKind discriminates the distribution-facing quote events.
type LPQuoteEventKind string

const (
	// LPQuoteEventUpdate carries the LP's current levels for the symbol.
	LPQuoteEventUpdate LPQuoteEventKind = "UPDATE"
	// LPQuoteEventWithdraw pulls the LP's book for the symbol (35=Z
	// cancel, or a same-set requote whose tracked legs were cancelled
	// but failed to replace).
	LPQuoteEventWithdraw LPQuoteEventKind = "WITHDRAW"
)

// LPQuoteLevel is one distributed price level — decimal price and
// quantity, matching the lpQuoteJSON wire schema the marketdata side
// decodes (internal/marketdata lp_pricing.go).
type LPQuoteLevel struct {
	Price decimal.Decimal
	Qty   decimal.Decimal
}

// LPQuoteEvent is the transport-neutral quote event emitted by
// QuoteService. InstrumentID/Symbol are the venue-canonical pair
// (resolved via the order pipeline's instrument lookup); Seq is the
// service-local event sequence (diagnostics — channel seqs are
// allocated by the marketdata producer); Ts anchors the consumer's
// staleness gate.
type LPQuoteEvent struct {
	Kind         LPQuoteEventKind
	LPID         int64
	InstrumentID int64
	Symbol       string
	Bids         []LPQuoteLevel // best-first
	Asks         []LPQuoteLevel // best-first
	Seq          uint64
	Ts           time.Time
}

// QuoteEventSink observes the distribution-facing LP quote stream —
// production wiring is JetStreamQuoteSink (quote_events.go), which
// publishes onto the "quotes" JetStream stream for marketdata's
// LPBookProducer. Implementations must be non-blocking: EmitLPQuote is
// invoked inline on the 35=i/35=Z admission path, so a slow sink must
// never stall quote admission (the JetStream sink enqueues and drains
// on its own goroutine). Nil sink disables emission entirely.
type QuoteEventSink interface {
	EmitLPQuote(ctx context.Context, ev LPQuoteEvent) error
}

// QuoteService owns the mass-quote lifecycle: admission checks, order
// placement/replacement through the pipeline, per-set state for
// QuoteCancel (35=Z), and per-entry acknowledgement construction.
type QuoteService struct {
	pipe    QuotePipeline
	ent     EntitlementSource
	mmp     LockoutSource
	obs     ComplianceObserver
	lpRes   LPAccountResolver
	lpGuard LPGuard
	sink    QuoteEventSink
	logf    func(format string, args ...any)

	mu    sync.Mutex
	sets  map[string]*quoteSet // key: account|session|setID
	gen   uint64               // quote-order COID generation counter
	evSeq uint64               // LPQuoteEvent sequence
}

// quoteSet tracks one acknowledged quote set's live order ids so a
// same-set MassQuote atomically replaces its levels and 35=Z can cancel
// them deterministically.
type quoteSet struct {
	entries map[string]*quoteEntryState // key: QuoteEntryID
}

type quoteEntryState struct {
	symbol       string
	instrumentID int64
	bidOrderID   int64
	askOrderID   int64
}

// NewQuoteService wires the service. pipe and ent are required
// (fail-closed: quoting without an order pipeline or an entitlement
// check is not quoting); mmp and obs may be nil for dev/test.
func NewQuoteService(pipe QuotePipeline, ent EntitlementSource,
	mmp LockoutSource, obs ComplianceObserver) (*QuoteService, error) {
	if pipe == nil {
		return nil, fmt.Errorf("fix quoting: nil order pipeline")
	}
	if ent == nil {
		return nil, fmt.Errorf("fix quoting: nil entitlement source")
	}
	return &QuoteService{
		pipe: pipe, ent: ent, mmp: mmp, obs: obs,
		logf: func(string, ...any) {},
		sets: map[string]*quoteSet{},
	}, nil
}

// WithLogger binds the operator log line.
func (s *QuoteService) WithLogger(fn func(format string, args ...any)) *QuoteService {
	if fn != nil {
		s.logf = fn
	}
	return s
}

// WithLPGate binds the Task 11.3.12 SCOPE_LP kill-switch gate: res maps
// the quoting account → its liquidity_providers entity, guard evaluates
// the `halt:lp:{lp_id}` flag. Both or neither — a half-bound gate is a
// wiring defect and fails closed at admission (spec §2.7). A fully-nil
// pair disables the check: dev/test convenience ONLY — production
// wiring must bind it or an LP suspension can never reach the 35=i
// path. The gate is the quote-ingress twin of the order-admission
// kill-switch: suspended LPs stop quoting while their firm CLOB order
// flow (and every other venue participant) continues untouched — the
// LP scope is never consulted on the order path.
func (s *QuoteService) WithLPGate(res LPAccountResolver, guard LPGuard) *QuoteService {
	s.lpRes = res
	s.lpGuard = guard
	return s
}

// WithQuoteEventSink binds the Task 7.3.9 feed seam: accepted MassQuote
// entries emit one aggregated UPDATE per (lp, symbol); 35=Z cancels and
// tracked-but-unreplaced entries emit WITHDRAW. The sink needs the
// account→LP hop to build its subject, so emission silently skips when
// lpRes is unbound or resolves lpID ≤ 0 (non-LP quoting accounts have no
// lpBook channel). Emit failures are logged, never fatal — distribution
// is best-effort and must not fail firm quote admission.
func (s *QuoteService) WithQuoteEventSink(sink QuoteEventSink) *QuoteService {
	s.sink = sink
	return s
}

func setKey(accountID int64, sessionID, setID string) string {
	return fmt.Sprintf("%d|%s|%s", accountID, sessionID, setID)
}

// quoteCOID derives the deterministic quote-sourced client_order_id —
// ≤64 chars; long identifiers hash-truncate (deterministic, so a replay
// of the same logical quote still dedups through client_order_id).
func quoteCOID(setID string, gen uint64, entryID, side string) string {
	coid := fmt.Sprintf("mq:%s:%d:%s:%s", setID, gen, entryID, side)
	if len(coid) <= 64 {
		return coid
	}
	sum := sha256.Sum256([]byte(coid))
	return "mq:" + hex.EncodeToString(sum[:])[:60]
}

// SubmitMassQuote applies one 35=i set: firm-liquidity gate → per-entry
// entitlement/MMP checks → prior-level cancel → LIMIT submission via
// the canonical pipeline → per-entry ack.
func (s *QuoteService) SubmitMassQuote(ctx context.Context, sessionID string,
	accountID int64, q *MassQuote) (*MassQuoteAck, error) {
	if q == nil || strings.TrimSpace(q.QuoteSetID) == "" {
		return nil, excerrors.New(marketmaking.CodeQuoteRequestRejected,
			"mass quote requires a non-empty QuoteSetID (302)")
	}
	ack := &MassQuoteAck{QuoteSetID: q.QuoteSetID}
	// 100% firm liquidity (§6.4 / FX Global Code P17): any non-zero hold
	// time is a last-look attempt — the entire set rejects; the venue
	// never queues, never holds, never confirms subjectively.
	if q.HoldTime != 0 {
		for _, e := range q.Entries {
			ack.Entries = append(ack.Entries, QuoteAckEntry{
				QuoteEntryID: e.QuoteEntryID, Symbol: e.Symbol,
				Status: QuoteStatusRejected, RejectReason: QuoteRejectReasonOther,
				Text: "QUOTE_REQUEST_REJECTED: non-zero hold time — 100% firm liquidity, no last look",
			})
		}
		return ack, excerrors.New(marketmaking.CodeQuoteRequestRejected,
			"mass quote rejected: non-zero hold time — 100% firm liquidity, no last look")
	}
	acct, err := s.pipe.AccountByID(ctx, accountID)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "account lookup", err)
	}
	if acct == nil {
		return nil, excerrors.New(marketmaking.CodeSessionNotEntitled,
			fmt.Sprintf("account %d not found", accountID))
	}
	// Task 11.3.12 SCOPE_LP: a kill-switched liquidity provider stops
	// quoting entirely — the whole set rejects with per-entry ack
	// rejections while the LP's (and everyone else's) firm CLOB order
	// flow continues untouched on the order path.
	lpID, halt := s.lpGateCheck(ctx, acct.ID)
	if halt != "" {
		for _, e := range q.Entries {
			ack.Entries = append(ack.Entries, QuoteAckEntry{
				QuoteEntryID: e.QuoteEntryID, Symbol: e.Symbol,
				Status: QuoteStatusRejected, RejectReason: QuoteRejectReasonOther,
				Text: halt,
			})
		}
		return ack, excerrors.New("TRADING_HALTED",
			"mass quote rejected: "+halt)
	}
	key := setKey(accountID, sessionID, q.QuoteSetID)

	// Task 7.3.9 feed seam: per-symbol distribution outcomes accumulate
	// across the batch, then emit once per (lp, symbol) — an LP quoting
	// one symbol through several entries yields a single multi-level
	// update; a rejected entry whose tracked legs were cancelled
	// withdraws the symbol unless a sibling entry in the same set
	// re-established levels.
	emits := map[string]*quoteEmit{}
	var emitOrder []string
	for _, e := range q.Entries {
		ae, em := s.applyEntry(ctx, acct, key, sessionID, q.QuoteSetID, e)
		ack.Entries = append(ack.Entries, ae)
		if em != nil {
			agg, ok := emits[em.symbol]
			if !ok {
				agg = &quoteEmit{symbol: em.symbol, instrumentID: em.instrumentID}
				emits[em.symbol] = agg
				emitOrder = append(emitOrder, em.symbol)
			}
			agg.bids = append(agg.bids, em.bids...)
			agg.asks = append(agg.asks, em.asks...)
			agg.withdrew = agg.withdrew || em.withdrew
		}
	}
	for _, sym := range emitOrder {
		agg := emits[sym]
		ev := LPQuoteEvent{LPID: lpID, InstrumentID: agg.instrumentID,
			Symbol: agg.symbol, Ts: time.Now()}
		if len(agg.bids)+len(agg.asks) > 0 {
			ev.Kind = LPQuoteEventUpdate
			ev.Bids, ev.Asks = agg.bids, agg.asks
		} else if agg.withdrew {
			ev.Kind = LPQuoteEventWithdraw
		} else {
			continue
		}
		s.emitLPQuote(ctx, ev)
	}
	return ack, nil
}

// lpGateCheck resolves the session account → LP entity and evaluates
// the SCOPE_LP flag. Returns the resolved lpID (0 when the account is
// not LP-bound or the check could not run) plus the rejection detail
// ("" = clear to quote). Fail closed on every unverifiable leg — a
// lookup error, a flag-scan error or a half-wired gate rejects quoting
// rather than admit a possibly-suspended LP (spec §2.7). Accounts with
// no lp_accounts binding skip the scope: they were never an LP.
func (s *QuoteService) lpGateCheck(ctx context.Context, accountID int64) (int64, string) {
	if s.lpRes == nil && s.lpGuard == nil {
		return s.lpIDForEmit(ctx, accountID), "" // gate unwired — still resolve for the feed seam
	}
	if s.lpRes == nil || s.lpGuard == nil {
		return 0, "TRADING_HALTED: LP kill-switch gate partially wired — cannot verify LP state"
	}
	lpID, err := s.lpRes.LPForAccount(ctx, accountID)
	if err != nil {
		return 0, fmt.Sprintf("TRADING_HALTED: LP account binding unverifiable: %v", err)
	}
	if lpID <= 0 {
		return 0, "" // not an LP-bound account — SCOPE_LP does not apply
	}
	suspended, reason, err := s.lpGuard.LPSuspended(ctx,
		strconv.FormatInt(lpID, 10))
	if err != nil {
		return 0, fmt.Sprintf("TRADING_HALTED: LP[%d] kill-switch check failed: %v", lpID, err)
	}
	if !suspended {
		return lpID, ""
	}
	if strings.TrimSpace(reason) == "" {
		reason = "liquidity provider suspended"
	}
	return lpID, fmt.Sprintf("TRADING_HALTED: LP[%d] %s", lpID, reason)
}

// lpIDForEmit resolves the account→LP hop for the feed seam alone —
// used when the SCOPE_LP gate is unwired (dev/test WithLPGate opt-out)
// so quote events still carry the lp_id their subject routes on.
func (s *QuoteService) lpIDForEmit(ctx context.Context, accountID int64) int64 {
	if s.lpRes == nil {
		return 0
	}
	lpID, err := s.lpRes.LPForAccount(ctx, accountID)
	if err != nil {
		s.logf("fix quoting: lp quote feed — account %d → lp lookup failed: %v",
			accountID, err)
		return 0
	}
	return lpID
}

// emitLPQuote hands one event to the bound sink. lpID ≤ 0 means the
// account has no lpBook channel to route to — skip silently. Emit
// errors are logged and swallowed: distribution must never fail firm
// quote admission (spec §2.7 — the feed degrades, quoting continues).
func (s *QuoteService) emitLPQuote(ctx context.Context, ev LPQuoteEvent) {
	if s.sink == nil || ev.LPID <= 0 {
		return
	}
	s.mu.Lock()
	s.evSeq++
	ev.Seq = s.evSeq
	s.mu.Unlock()
	if ev.Ts.IsZero() {
		ev.Ts = time.Now()
	}
	if err := s.sink.EmitLPQuote(ctx, ev); err != nil {
		s.logf("fix quoting: lp quote emit failed (lp=%d %s %s): %v",
			ev.LPID, ev.Kind, ev.Symbol, err)
	}
}

// emitWithdrawals resolves the LP once and fans out WITHDRAW events for
// the supplied (instrument, symbol) pairs — the 35=Z distribution path.
func (s *QuoteService) emitWithdrawals(ctx context.Context, accountID int64,
	ems []quoteEmit) {
	if s.sink == nil || len(ems) == 0 {
		return
	}
	lpID := s.lpIDForEmit(ctx, accountID)
	if lpID <= 0 {
		return
	}
	seen := map[string]bool{}
	now := time.Now()
	for _, em := range ems {
		if em.symbol == "" || seen[em.symbol] {
			continue
		}
		seen[em.symbol] = true
		s.emitLPQuote(ctx, LPQuoteEvent{
			Kind: LPQuoteEventWithdraw, LPID: lpID,
			InstrumentID: em.instrumentID, Symbol: em.symbol, Ts: now,
		})
	}
}

// quoteEmit is the distribution-facing outcome of one quote entry:
// accepted levels append to the symbol's update; a rejected entry whose
// previously-tracked legs were cancelled marks the symbol for withdraw
// (the book no longer stands behind the old levels).
type quoteEmit struct {
	instrumentID int64
	symbol       string
	bids, asks   []LPQuoteLevel
	withdrew     bool
}

// applyEntry handles one quote entry end-to-end and returns its ack
// line plus the optional distribution event (nil = nothing to emit —
// rejects that never touched live legs stay silent on the feed).
// Rejections are per-entry and never abort the set (a bad symbol must
// not strand the other instruments — the same per-entry semantics 35=b
// encodes).
func (s *QuoteService) applyEntry(ctx context.Context, acct *orders.Account,
	key, sessionID, setID string, e QuoteEntry) (QuoteAckEntry, *quoteEmit) {
	reject := func(text string) QuoteAckEntry {
		return QuoteAckEntry{
			QuoteEntryID: e.QuoteEntryID, Symbol: e.Symbol,
			Status: QuoteStatusRejected, RejectReason: QuoteRejectReasonOther,
			Text: text,
		}
	}
	if strings.TrimSpace(e.QuoteEntryID) == "" {
		return reject("QUOTE_REQUEST_REJECTED: QuoteEntryID required"), nil
	}
	hasBid := e.BidPx != nil && e.BidSize != nil
	hasAsk := e.OfferPx != nil && e.OfferSize != nil
	if !hasBid && !hasAsk {
		return reject("QUOTE_REQUEST_REJECTED: entry carries no quote side"), nil
	}
	inst, err := s.pipe.InstrumentBySymbol(ctx, e.Symbol)
	if err != nil {
		return reject(fmt.Sprintf("INTERNAL_ERROR: instrument lookup: %v", err)), nil
	}
	if inst == nil {
		return reject(fmt.Sprintf("QUOTE_REQUEST_REJECTED: unknown symbol %q", e.Symbol)), nil
	}
	// mm_programs entitlement: the session's bound account must hold an
	// ACTIVE program covering this instrument (program-wide or
	// per-instrument row) — quoting without registration rejects
	// SESSION_NOT_ENTITLED.
	prog, err := s.ent.Entitled(ctx, acct.ID, inst.ID)
	if err != nil {
		return reject(fmt.Sprintf("SERVICE_DEGRADED: entitlement lookup failed: %v", err)), nil
	}
	if prog == nil {
		return reject(fmt.Sprintf("SESSION_NOT_ENTITLED: no active mm program covers %s", inst.Symbol)), nil
	}
	// MMP lockout: quotes stay rejected until the explicit reset while a
	// protection trigger stands (§24.x MM Program row).
	if s.mmp != nil && s.mmp.MMPLocked(ctx, acct.ID, inst.ID) {
		return reject(fmt.Sprintf("MMP_LOCKED_OUT: market-maker protection triggered on %s — reset required", inst.Symbol)), nil
	}
	// Atomic level replacement: the new generation cancels the prior
	// bid/ask before placing, so the book never carries both.
	prev := s.entryState(key, e.QuoteEntryID)
	if prev != nil {
		s.cancelEntryOrders(ctx, acct, prev, "replace")
	}
	s.mu.Lock()
	s.gen++
	gen := s.gen
	s.mu.Unlock()

	st := &quoteEntryState{symbol: inst.Symbol, instrumentID: inst.ID}
	// The reject-after-cancel outcomes below withdraw the feed levels —
	// prev's legs are gone from the book even though the replacement
	// never posted.
	feedDrop := func() *quoteEmit {
		if prev == nil {
			return nil
		}
		return &quoteEmit{instrumentID: prev.instrumentID,
			symbol: prev.symbol, withdrew: true}
	}
	submitSide := func(side string, px, qty *decimal.Decimal) (*orders.Ack, error) {
		return s.pipe.Submit(ctx, acct, &orders.SubmitRequest{
			Symbol:        inst.Symbol,
			Side:          side,
			OrderType:     orders.TypeLimit,
			TimeInForce:   orders.TIFGTC,
			Quantity:      qty,
			Price:         px,
			ClientOrderID: quoteCOID(setID, gen, e.QuoteEntryID, side),
			SessionID:     sessionID,
		})
	}
	// Both present sides are submitted; a second-side failure cancels
	// the first leg so the MM never posts a phantom one-sided quote.
	if hasBid {
		ack, err := submitSide(orders.SideBuy, e.BidPx, e.BidSize)
		if err != nil {
			return reject("ORDER_REJECTED: bid leg: "+excerrors.CodeOf(err)+" "+err.Error()),
				feedDrop()
		}
		st.bidOrderID = ack.OrderID
	}
	if hasAsk {
		ack, err := submitSide(orders.SideSell, e.OfferPx, e.OfferSize)
		if err != nil {
			if st.bidOrderID != 0 {
				if _, cerr := s.pipe.Cancel(ctx, acct, st.bidOrderID,
					"system:fix-quoting", "", ""); cerr != nil {
					s.logf("fix quoting: dangling bid leg %d unwind failed: %v",
						st.bidOrderID, cerr)
				}
				st.bidOrderID = 0
			}
			return reject("ORDER_REJECTED: ask leg: "+excerrors.CodeOf(err)+" "+err.Error()),
				feedDrop()
		}
		st.askOrderID = ack.OrderID
	}
	s.putEntryState(key, e.QuoteEntryID, st)
	// Obligation sampling (Task 18.3.10): each applied quote feeds the
	// presence counters. Best-effort — a compliance write hiccup must
	// never reject firm liquidity.
	if s.obs != nil {
		if err := s.obs.ObserveQuote(ctx, acct.ID, inst.ID,
			e.BidPx, e.BidSize, e.OfferPx, e.OfferSize); err != nil {
			s.logf("fix quoting: obligation sample failed: %v", err)
		}
	}
	em := &quoteEmit{instrumentID: inst.ID, symbol: inst.Symbol}
	if hasBid {
		em.bids = []LPQuoteLevel{{Price: *e.BidPx, Qty: *e.BidSize}}
	}
	if hasAsk {
		em.asks = []LPQuoteLevel{{Price: *e.OfferPx, Qty: *e.OfferSize}}
	}
	return QuoteAckEntry{
		QuoteEntryID: e.QuoteEntryID, Symbol: inst.Symbol,
		Status: QuoteStatusAccepted,
	}, em
}

func (s *QuoteService) entryState(key, entryID string) *quoteEntryState {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.sets[key]
	if set == nil {
		return nil
	}
	return set.entries[entryID]
}

func (s *QuoteService) putEntryState(key, entryID string, st *quoteEntryState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.sets[key]
	if set == nil {
		set = &quoteSet{entries: map[string]*quoteEntryState{}}
		s.sets[key] = set
	}
	set.entries[entryID] = st
}

func (s *QuoteService) cancelEntryOrders(ctx context.Context, acct *orders.Account,
	st *quoteEntryState, reason string) {
	for _, id := range []int64{st.bidOrderID, st.askOrderID} {
		if id == 0 {
			continue
		}
		if _, err := s.pipe.Cancel(ctx, acct, id,
			"system:fix-quoting:"+reason, "", ""); err != nil {
			s.logf("fix quoting: cancel quote leg %d (%s): %v", id, reason, err)
		}
	}
}

// CancelQuotes implements 35=Z: per-set (QuoteSetID), per-symbol
// (CancelType=1) or all-quotes (CancelType=4) cancellation. Symbol-
// scoped and all-scoped cancels ride the canonical mass-cancel path so
// the sweep is atomic across shards and audit-stamped; the per-set path
// cancels the tracked legs individually (the set boundary is quoting-
// layer state, not an order attribute).
func (s *QuoteService) CancelQuotes(ctx context.Context, sessionID string,
	accountID int64, c *QuoteCancel) (int, error) {
	if c == nil {
		return 0, excerrors.New("INVALID_REQUEST", "quote cancel body required")
	}
	acct, err := s.pipe.AccountByID(ctx, accountID)
	if err != nil {
		return 0, excerrors.Wrap("INTERNAL_ERROR", "account lookup", err)
	}
	if acct == nil {
		return 0, excerrors.New(marketmaking.CodeSessionNotEntitled,
			fmt.Sprintf("account %d not found", accountID))
	}
	// Per-quote-set cancel — the narrowest scope.
	if strings.TrimSpace(c.QuoteSetID) != "" {
		n, ems, err := s.cancelSet(ctx, acct, sessionID, c.QuoteSetID)
		if err == nil {
			s.emitWithdrawals(ctx, accountID, ems)
		}
		return n, err
	}
	switch c.CancelType {
	case QuoteCancelPerSymbol:
		sym := config.CanonicalSymbol(c.Symbol)
		if sym == "" {
			return 0, excerrors.New("INVALID_REQUEST", "CancelType=1 requires Symbol (55)")
		}
		inst, err := s.pipe.InstrumentBySymbol(ctx, sym)
		if err != nil {
			return 0, excerrors.Wrap("INTERNAL_ERROR", "instrument lookup", err)
		}
		if inst == nil {
			return 0, excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("unknown symbol %q", c.Symbol))
		}
		res, err := s.pipe.MassCancel(ctx, orders.MassCancelScope{
			AccountID:    accountID,
			InstrumentID: inst.ID,
			SessionID:    sessionID,
			Reason:       "quote_cancel",
		}, "system:fix-quoting", "", "")
		if err != nil {
			return 0, err
		}
		s.dropEntries(func(st *quoteEntryState) bool { return st.instrumentID == inst.ID })
		s.emitWithdrawals(ctx, accountID, []quoteEmit{
			{instrumentID: inst.ID, symbol: inst.Symbol},
		})
		return res.Cancelled, nil
	case QuoteCancelAllQuotes:
		res, err := s.pipe.MassCancel(ctx, orders.MassCancelScope{
			AccountID: accountID,
			SessionID: sessionID,
			Reason:    "quote_cancel",
		}, "system:fix-quoting", "", "")
		if err != nil {
			return 0, err
		}
		var ems []quoteEmit
		s.mu.Lock()
		for k, set := range s.sets {
			if strings.HasPrefix(k, fmt.Sprintf("%d|%s|", accountID, sessionID)) {
				for _, st := range set.entries {
					ems = append(ems, quoteEmit{
						instrumentID: st.instrumentID, symbol: st.symbol})
				}
				delete(s.sets, k)
			}
		}
		s.mu.Unlock()
		s.emitWithdrawals(ctx, accountID, ems)
		return res.Cancelled, nil
	default:
		return 0, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("unsupported QuoteCancelType %d (want 1=symbol, 4=all, or QuoteSetID)", c.CancelType))
	}
}

// cancelSet cancels every tracked leg of one quote set and drops its
// state. Idempotent: a second cancel of the same set reports 0. The
// returned quoteEmit list carries the affected (instrument, symbol)
// pairs for the distribution-feed withdrawal fan-out.
func (s *QuoteService) cancelSet(ctx context.Context, acct *orders.Account,
	sessionID, setID string) (int, []quoteEmit, error) {
	key := setKey(acct.ID, sessionID, setID)
	s.mu.Lock()
	set := s.sets[key]
	if set != nil {
		delete(s.sets, key)
	}
	s.mu.Unlock()
	if set == nil {
		return 0, nil, nil
	}
	cancelled := 0
	ems := make([]quoteEmit, 0, len(set.entries))
	for _, st := range set.entries {
		ems = append(ems, quoteEmit{
			instrumentID: st.instrumentID, symbol: st.symbol})
		for _, id := range []int64{st.bidOrderID, st.askOrderID} {
			if id == 0 {
				continue
			}
			if _, err := s.pipe.Cancel(ctx, acct, id,
				"system:fix-quoting:quote_cancel", "", ""); err != nil {
				s.logf("fix quoting: cancel leg %d failed: %v", id, err)
				continue
			}
			cancelled++
		}
	}
	return cancelled, ems, nil
}

// dropEntries removes tracked entries matching pred across all sets —
// used after symbol-scoped mass cancels so the local mirror cannot
// resurrect cancelled legs.
func (s *QuoteService) dropEntries(pred func(*quoteEntryState) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, set := range s.sets {
		for id, st := range set.entries {
			if pred(st) {
				delete(set.entries, id)
			}
		}
	}
}

// OpenSetCount reports the number of tracked quote sets — ops/test
// introspection only.
func (s *QuoteService) OpenSetCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sets)
}
