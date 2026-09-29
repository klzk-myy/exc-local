// mdata.go — Phase-18 Task 18.3.3: FIX market data distribution.
//
// Implements the FIX 4.4 MarketDataRequest (35=V) lifecycle over the
// venue's existing L2 book feed (internal/marketdata BookDelta stream —
// the same deltas the WS conflator consumes; attach via DeltaSource,
// e.g. marketdata.IPCDeltaSource or NewJetStreamDeltaSource, or drive
// PushDelta from a hub callback):
//
//   - SubscriptionRequestType(263)=0 Snapshot          → MarketDataSnapshotFullRefresh (35=W), no state kept
//   - SubscriptionRequestType(263)=1 Snapshot+Updates  → register per-session subscription, send W, then
//     MarketDataIncrementalRefresh (35=X) on each book change
//   - SubscriptionRequestType(263)=2 Disable           → remove the subscription (idempotent)
//
// Rejections use MarketDataRequestReject (35=Y) — unknown instrument
// (281=0), duplicate MDReqID (281=1), entitlement failure (281=3),
// unsupported SubscriptionRequestType (281=4) / MarketDepth (281=5) /
// MDUpdateType (281=6). Missing/malformed required fields return
// quickfix.MessageRejectError so the session layer emits Reject (35=3)
// per spec §9.9. Rejections are atomic per request: one bad symbol
// rejects the whole request — nothing partially subscribes (§2.7
// fail-closed).
//
// Incremental encoding: the venue L2 feed is an aggregated positional
// book (top-N levels per side), so each subscribed session's last-
// EMITTED level set is diffed against the new state — entries carry
// MDUpdateAction(279) 0=New / 1=Change / 2=Delete plus
// MDEntryPositionNo(290). Sessions requesting MDUpdateType(265)=0 get a
// full 35=W per book change instead.
//
// Sibling seams (consumed from the Task 18.3.1 session package):
//   - MessageSender: production binding is QuickFIXSender()
//     (quickfix.SendToTarget); tests capture outbound frames.
//   - The acceptor's FromApp dispatch calls HandleMarketDataRequest on
//     MsgType "V"; OnLogout calls DropSession to purge subscriptions.
//   - Entitled(sessionID, symbol) plugs the 046 allowed_instruments
//     entitlement for market-data scope (nil = allow all).
package fix

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/marketdata"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// MsgType / tag / enum constants introduced by this file (see tags.go for
// the shared set — TagSymbol, TagText, TagMsgType, …).
// ---------------------------------------------------------------------------

const (
	MsgMarketDataRequest             = "V"
	MsgMarketDataSnapshotFullRefresh = "W"
	MsgMarketDataIncrementalRefresh  = "X"
	MsgMarketDataRequestReject       = "Y"
	MsgTradingSessionStatusRequest   = "g"
)

const (
	TagNoRelatedSym            quickfix.Tag = 146
	TagMDReqID                 quickfix.Tag = 262
	TagSubscriptionRequestType quickfix.Tag = 263
	TagMarketDepth             quickfix.Tag = 264
	TagMDUpdateType            quickfix.Tag = 265
	TagAggregatedBook          quickfix.Tag = 266
	TagNoMDEntryTypes          quickfix.Tag = 267
	TagNoMDEntries             quickfix.Tag = 268
	TagMDEntryType             quickfix.Tag = 269
	TagMDEntryPx               quickfix.Tag = 270
	TagMDEntrySize             quickfix.Tag = 271
	TagMDUpdateAction          quickfix.Tag = 279
	TagMDReqRejReason          quickfix.Tag = 281
	TagMDEntryPositionNo       quickfix.Tag = 290
)

// SubscriptionRequestType(263)
const (
	SubTypeSnapshot        = 0
	SubTypeSnapshotUpdates = 1
	SubTypeDisable         = 2
)

// MDUpdateType(265)
const (
	MDUpdateFullRefresh = 0
	MDUpdateIncremental = 1
)

// MDUpdateAction(279)
const (
	MDActionNew    = 0
	MDActionChange = 1
	MDActionDelete = 2
)

// MDEntryType(269) — the book feed emits bids and offers only.
const (
	MDEntryBid   = "0"
	MDEntryOffer = "1"
)

// MDReqRejReason(281)
const (
	MDRejUnknownSymbol          = 0
	MDRejDuplicateMDReqID       = 1
	MDRejInsufficientPermission = 3
	MDRejUnsupportedSubType     = 4
	MDRejUnsupportedMarketDepth = 5
	MDRejUnsupportedMDUpdate    = 6
)

// ---------------------------------------------------------------------------
// Seams
// ---------------------------------------------------------------------------

// MessageSender is the outbound FIX send seam. Production binding is
// quickfix.SendToTarget (QuickFIXSender()); the Task 18.3.1 acceptor's
// session registry resolves live sessions behind it. Implementations
// must not block — sends run while subscription state is held so that
// per-session message order is deterministic.
type MessageSender interface {
	SendTo(msg *quickfix.Message, sessionID quickfix.SessionID) error
}

// MessageSenderFunc adapts a function to MessageSender.
type MessageSenderFunc func(*quickfix.Message, quickfix.SessionID) error

// SendTo implements MessageSender.
func (f MessageSenderFunc) SendTo(m *quickfix.Message, id quickfix.SessionID) error {
	return f(m, id)
}

// QuickFIXSender is the production MessageSender over
// quickfix.SendToTarget (which takes the broader Messagable — adapted
// here so callers pass *Message uniformly).
func QuickFIXSender() MessageSender {
	return MessageSenderFunc(func(m *quickfix.Message, id quickfix.SessionID) error {
		return quickfix.SendToTarget(m, id)
	})
}

// MarketDataDeps wires the market-data handler. Sender and Known are
// required; everything else has a spec-default.
type MarketDataDeps struct {
	// Sender routes outbound app messages to live FIX sessions.
	Sender MessageSender
	// DeltaSource feeds the aggregated L2 book deltas. Production wires
	// marketdata.IPCDeltaSource (engine _out ring) or
	// marketdata.NewJetStreamDeltaSource (bridge republish). Nil → the
	// service is driven via PushDelta only.
	DeltaSource marketdata.DeltaSource
	// Known reports whether symbol is a listed instrument (production
	// wires the instruments reference cache — admin.ValidatePairSymbol
	// shape, "EUR/USD").
	Known func(symbol string) bool
	// Entitled reports whether the session may subscribe to symbol's
	// market data — the 046 allowed_instruments check for the MD scope
	// (order entitlement is enforced separately on order entry). The
	// sessionID argument is the quickfix SessionID.String() form used by
	// the Store. Nil → all instruments allowed.
	Entitled func(sessionID string, symbol string) bool
	// MaxSubscriptions caps symbol-subscriptions per session
	// (default 64). Breach → 35=Y with Text, no state registered.
	MaxSubscriptions int
	// Depth is the venue book depth served (default 20 — spec §10.1).
	// Requests for greater depth are served at this cap, never padded.
	Depth int
	// Now is injectable for tests; default time.Now.
	Now func() time.Time
	// Logf defaults to slog.
	Logf func(format string, args ...any)
}

// mdSymState is one subscribed symbol's last-emitted positional level
// set — the diff baseline for the next incremental.
type mdSymState struct {
	bids []marketdata.Level
	asks []marketdata.Level
}

// mdRequest is one live MarketDataRequest (35=V, 263=1) subscription.
type mdRequest struct {
	reqID       string
	depth       int             // resolved (>0)
	incremental bool            // 265=1 or absent
	entryTypes  map[string]bool // nil → all emitted types
	symbols     map[string]*mdSymState
}

// MarketDataService owns per-session market-data subscription state and
// the delta→35=X fan-out. Methods are goroutine-safe.
type MarketDataService struct {
	deps MarketDataDeps

	mu     sync.Mutex
	subs   map[quickfix.SessionID]map[string]*mdRequest // sessionID → reqID → request
	latest map[string]marketdata.BookDelta              // symbol → newest delta

	deltasIn   atomic.Uint64
	msgsSent   atomic.Uint64
	sendErrors atomic.Uint64
}

// NewMarketDataService fails closed on missing required deps.
func NewMarketDataService(d MarketDataDeps) (*MarketDataService, error) {
	if d.Sender == nil {
		return nil, fmt.Errorf("fix: market data requires a MessageSender")
	}
	if d.Known == nil {
		return nil, fmt.Errorf("fix: market data requires a Known-instrument lookup")
	}
	if d.MaxSubscriptions <= 0 {
		d.MaxSubscriptions = 64
	}
	if d.Depth <= 0 {
		d.Depth = 20
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		l := slog.Default()
		d.Logf = func(f string, a ...any) { l.Warn(fmt.Sprintf("fix md: "+f, a...)) }
	}
	return &MarketDataService{
		deps:   d,
		subs:   map[quickfix.SessionID]map[string]*mdRequest{},
		latest: map[string]marketdata.BookDelta{},
	}, nil
}

// ---------------------------------------------------------------------------
// Feed consumption — Run (source-driven) + PushDelta (injection seam)
// ---------------------------------------------------------------------------

// Run consumes the configured DeltaSource until ctx ends. A nil source
// simply parks the loop — embedders drive PushDelta.
func (s *MarketDataService) Run(ctx context.Context) error {
	var ch <-chan marketdata.BookDelta
	if s.deps.DeltaSource != nil {
		var err error
		ch, err = s.deps.DeltaSource.Deltas(ctx)
		if err != nil {
			return fmt.Errorf("fix md: delta source: %w", err)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-ch:
			if !ok {
				return nil
			}
			s.onDelta(d)
		}
	}
}

// PushDelta injects one book delta — the test seam and the hub-callback
// drive path when no DeltaSource is configured.
func (s *MarketDataService) PushDelta(d marketdata.BookDelta) { s.onDelta(d) }

func (s *MarketDataService) onDelta(d marketdata.BookDelta) {
	s.deltasIn.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest[d.Symbol] = d
	for sessID, reqs := range s.subs {
		for _, rq := range reqs {
			st, ok := rq.symbols[d.Symbol]
			if !ok {
				continue
			}
			bids := sliceDepth(d.Bids, rq.depth)
			asks := sliceDepth(d.Asks, rq.depth)
			var msg *quickfix.Message
			if rq.incremental {
				entries := diffBook(d.Symbol, st.bids, st.asks, bids, asks, rq.entryTypes)
				if len(entries) == 0 {
					st.bids, st.asks = bids, asks
					continue
				}
				msg = buildIncremental(rq.reqID, entries)
			} else {
				msg = buildSnapshot(rq.reqID, d.Symbol, bids, asks, rq.entryTypes)
			}
			st.bids, st.asks = bids, asks
			s.sendLocked(sessID, msg)
		}
	}
}

func sliceDepth(lv []marketdata.Level, depth int) []marketdata.Level {
	if depth > 0 && len(lv) > depth {
		lv = lv[:depth]
	}
	out := make([]marketdata.Level, len(lv))
	copy(out, lv)
	return out
}

// sendLocked ships one built message; errors are counted and logged —
// a dead session's subscriptions are purged via DropSession on logout,
// not eagerly here (a transient send failure must not silently strip a
// live client's entitlement).
// Caller holds s.mu.
func (s *MarketDataService) sendLocked(id quickfix.SessionID, msg *quickfix.Message) {
	if err := s.deps.Sender.SendTo(msg, id); err != nil {
		s.sendErrors.Add(1)
		s.deps.Logf("send %s to %s failed: %v", msgTypeOf(msg), id.String(), err)
		return
	}
	s.msgsSent.Add(1)
}

func msgTypeOf(m *quickfix.Message) string {
	if t, err := m.MsgType(); err == nil {
		return t
	}
	return "?"
}

// ---------------------------------------------------------------------------
// Request handling — 35=V
// ---------------------------------------------------------------------------

// HandleMarketDataRequest processes an inbound MarketDataRequest (35=V).
// Called from the acceptor's FromApp dispatch; a non-nil return is a
// session-layer MessageRejectError (quickfix emits Reject 35=3).
func (s *MarketDataService) HandleMarketDataRequest(id quickfix.SessionID,
	msg *quickfix.Message) quickfix.MessageRejectError {
	req, rejErr := parseMDRequest(msg)
	if rejErr != nil {
		return rejErr
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if req.badUpdateType {
		s.sendLocked(id, buildMDReject(req.reqID, MDRejUnsupportedMDUpdate,
			"Unsupported MDUpdateType", req.symbols))
		return nil
	}

	switch req.subType {
	case SubTypeSnapshot, SubTypeSnapshotUpdates:
		// Atomic validation: every symbol must be known + entitled or
		// the whole request rejects with 35=Y.
		var bad []string
		badReason := MDRejUnknownSymbol
		for _, sym := range req.symbols {
			if !s.deps.Known(sym) {
				bad = append(bad, sym)
				continue
			}
			if s.deps.Entitled != nil && !s.deps.Entitled(id.String(), sym) {
				bad = append(bad, sym)
				badReason = MDRejInsufficientPermission
			}
		}
		if len(bad) > 0 {
			s.sendLocked(id, buildMDReject(req.reqID, badReason,
				mdRejText(badReason), bad))
			return nil
		}
		if req.subType == SubTypeSnapshot {
			for _, sym := range req.symbols {
				s.sendLocked(id, s.snapshotForLocked(req.reqID, sym, req))
			}
			return nil
		}
		// Subscribe.
		reqs := s.subs[id]
		if reqs == nil {
			reqs = map[string]*mdRequest{}
			s.subs[id] = reqs
		}
		if _, dup := reqs[req.reqID]; dup {
			s.sendLocked(id, buildMDReject(req.reqID, MDRejDuplicateMDReqID,
				"Duplicate MDReqID", req.symbols))
			return nil
		}
		if n := s.symbolSubCountLocked(id); n+len(req.symbols) > s.deps.MaxSubscriptions {
			s.sendLocked(id, buildMDReject(req.reqID, -1,
				"MAX_SUBSCRIPTIONS_EXCEEDED", req.symbols))
			return nil
		}
		rq := &mdRequest{
			reqID:       req.reqID,
			depth:       req.depth,
			incremental: req.incremental,
			entryTypes:  req.entryTypes,
			symbols:     map[string]*mdSymState{},
		}
		reqs[req.reqID] = rq
		for _, sym := range req.symbols {
			st := &mdSymState{}
			rq.symbols[sym] = st
			// Subscription ack = the initial full snapshot (35=W); the
			// emitted levels become the incremental diff baseline.
			m, bids, asks := s.snapshotMsgLocked(req.reqID, sym, req)
			st.bids, st.asks = bids, asks
			s.sendLocked(id, m)
		}
		return nil

	case SubTypeDisable:
		// Unsubscribe removes the whole request keyed by MDReqID —
		// idempotent: unknown reqIDs are a no-op (still acked silently;
		// FIX defines no ack for a successful disable).
		if reqs := s.subs[id]; reqs != nil {
			delete(reqs, req.reqID)
			if len(reqs) == 0 {
				delete(s.subs, id)
			}
		}
		return nil

	default:
		s.sendLocked(id, buildMDReject(req.reqID, MDRejUnsupportedSubType,
			"Unsupported SubscriptionRequestType", req.symbols))
		return nil
	}
}

// mdReq is the parsed 35=V surface. badUpdateType flags a present but
// unsupported MDUpdateType(265) — rejected at the business layer with
// 35=Y 281=6 rather than a session-layer reject.
type mdReq struct {
	reqID         string
	subType       int
	depth         int
	incremental   bool
	badUpdateType bool
	entryTypes    map[string]bool
	symbols       []string
}

// parseMDRequest extracts and validates the required fields; failures
// are session-layer rejects (35=3 via the returned MessageRejectError)
// for missing/malformed required fields.
func parseMDRequest(msg *quickfix.Message) (*mdReq, quickfix.MessageRejectError) {
	reqID, err := msg.Body.GetString(TagMDReqID)
	if err != nil {
		return nil, err // required — session reject
	}
	subType, err := msg.Body.GetInt(TagSubscriptionRequestType)
	if err != nil {
		return nil, err // required
	}

	// Optional MarketDepth(264): 0/absent = full available depth,
	// N = top-N. Negative is not a FIX value.
	depth := 0
	if msg.Body.Has(TagMarketDepth) {
		depth, err = msg.Body.GetInt(TagMarketDepth)
		if err != nil {
			return nil, err
		}
	}
	incremental := true
	if msg.Body.Has(TagMDUpdateType) {
		ut, err := msg.Body.GetInt(TagMDUpdateType)
		if err != nil {
			return nil, err
		}
		if ut != MDUpdateFullRefresh && ut != MDUpdateIncremental {
			// Defer to the business layer: 35=Y with 281=6.
			return &mdReq{reqID: reqID, subType: subType, badUpdateType: true}, nil
		}
		incremental = ut == MDUpdateIncremental
	}

	var entryTypes map[string]bool
	if msg.Body.Has(TagNoMDEntryTypes) {
		grp := quickfix.NewRepeatingGroup(TagNoMDEntryTypes, quickfix.GroupTemplate{
			quickfix.GroupElement(TagMDEntryType),
		})
		if err := msg.Body.GetGroup(grp); err != nil {
			return nil, err
		}
		entryTypes = map[string]bool{}
		for i := 0; i < grp.Len(); i++ {
			et, err := grp.Get(i).GetString(TagMDEntryType)
			if err != nil {
				return nil, err
			}
			entryTypes[et] = true
		}
	}

	if !msg.Body.Has(TagNoRelatedSym) {
		return nil, quickfix.RequiredTagMissing(TagNoRelatedSym)
	}
	symGrp := quickfix.NewRepeatingGroup(TagNoRelatedSym, quickfix.GroupTemplate{
		quickfix.GroupElement(TagSymbol),
	})
	if err := msg.Body.GetGroup(symGrp); err != nil {
		return nil, err
	}
	if symGrp.Len() == 0 {
		return nil, quickfix.RequiredTagMissing(TagNoRelatedSym)
	}
	symbols := make([]string, 0, symGrp.Len())
	for i := 0; i < symGrp.Len(); i++ {
		sym, err := symGrp.Get(i).GetString(TagSymbol)
		if err != nil {
			return nil, err
		}
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" {
			return nil, quickfix.IncorrectDataFormatForValue(TagSymbol)
		}
		symbols = append(symbols, sym)
	}
	return &mdReq{
		reqID: reqID, subType: subType, depth: depth,
		incremental: incremental, entryTypes: entryTypes, symbols: symbols,
	}, nil
}

// symbolSubCountLocked counts live symbol-subscriptions for a session.
// Caller holds s.mu.
func (s *MarketDataService) symbolSubCountLocked(id quickfix.SessionID) int {
	n := 0
	for _, rq := range s.subs[id] {
		n += len(rq.symbols)
	}
	return n
}

// snapshotForLocked emits the request-scoped snapshot for a one-shot
// (263=0) request — no subscription state is retained.
// Caller holds s.mu.
func (s *MarketDataService) snapshotForLocked(reqID, symbol string, req *mdReq) *quickfix.Message {
	d, ok := s.latest[symbol]
	if !ok {
		return buildSnapshot(reqID, symbol, nil, nil, req.entryTypes)
	}
	bids := sliceDepth(d.Bids, reqDepth(req.depth, s.deps.Depth))
	asks := sliceDepth(d.Asks, reqDepth(req.depth, s.deps.Depth))
	return buildSnapshot(reqID, symbol, bids, asks, req.entryTypes)
}

// snapshotMsgLocked builds the subscribe-ack snapshot AND returns the
// emitted level slices so the caller can seed the diff baseline.
// Caller holds s.mu.
func (s *MarketDataService) snapshotMsgLocked(reqID, symbol string,
	req *mdReq) (*quickfix.Message, []marketdata.Level, []marketdata.Level) {
	d, ok := s.latest[symbol]
	var bids, asks []marketdata.Level
	if ok {
		bids = sliceDepth(d.Bids, reqDepth(req.depth, s.deps.Depth))
		asks = sliceDepth(d.Asks, reqDepth(req.depth, s.deps.Depth))
	}
	return buildSnapshot(reqID, symbol, bids, asks, req.entryTypes), bids, asks
}

// reqDepth resolves the served level count: 0/absent or beyond the
// venue cap both resolve to the cap (the feed's full depth — §10.1
// never pads a thin book).
func reqDepth(requested, cap int) int {
	if requested <= 0 || requested > cap {
		return cap
	}
	return requested
}

func mdRejText(reason int) string {
	switch reason {
	case MDRejUnknownSymbol:
		return "Unknown symbol"
	case MDRejDuplicateMDReqID:
		return "Duplicate MDReqID"
	case MDRejInsufficientPermission:
		return "SESSION_NOT_ENTITLED"
	default:
		return "MarketDataRequest rejected"
	}
}

// ---------------------------------------------------------------------------
// Outbound message builders
// ---------------------------------------------------------------------------

func mdEntryTypeAllowed(types map[string]bool, t string) bool {
	return types == nil || types[t]
}

// buildSnapshot emits MarketDataSnapshotFullRefresh (35=W) — one
// NoMDEntries group entry per positional level, bids then asks.
func buildSnapshot(reqID, symbol string, bids, asks []marketdata.Level,
	entryTypes map[string]bool) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgMarketDataSnapshotFullRefresh)
	m.Body.SetString(TagMDReqID, reqID)
	m.Body.SetString(TagSymbol, symbol)
	// AggregatedBook(266)=Y — the venue L2 feed is aggregated per §10.1.
	m.Body.SetString(TagAggregatedBook, "Y")

	grp := quickfix.NewRepeatingGroup(TagNoMDEntries, quickfix.GroupTemplate{
		quickfix.GroupElement(TagMDEntryType),
		quickfix.GroupElement(TagMDEntryPx),
		quickfix.GroupElement(TagMDEntrySize),
		quickfix.GroupElement(TagMDEntryPositionNo),
	})
	pos := 1
	if mdEntryTypeAllowed(entryTypes, MDEntryBid) {
		for _, l := range bids {
			e := grp.Add()
			e.SetString(TagMDEntryType, MDEntryBid)
			e.SetString(TagMDEntryPx, decimal.NewFromScaled(l.Price).String())
			e.SetString(TagMDEntrySize, decimal.NewFromScaled(l.Qty).String())
			e.SetInt(TagMDEntryPositionNo, pos)
			pos++
		}
	}
	pos = 1
	if mdEntryTypeAllowed(entryTypes, MDEntryOffer) {
		for _, l := range asks {
			e := grp.Add()
			e.SetString(TagMDEntryType, MDEntryOffer)
			e.SetString(TagMDEntryPx, decimal.NewFromScaled(l.Price).String())
			e.SetString(TagMDEntrySize, decimal.NewFromScaled(l.Qty).String())
			e.SetInt(TagMDEntryPositionNo, pos)
			pos++
		}
	}
	m.Body.SetGroup(grp)
	return m
}

// mdEntry is one diffed NoMDEntries row for 35=X.
type mdEntry struct {
	action int    // 279: 0 New / 1 Change / 2 Delete
	etype  string // 269
	symbol string // 55 — carried per-entry on incrementals
	px     string // 270
	qty    string // 271 — "" on deletes
	pos    int    // 290
}

// diffBook computes the positional update between the last-emitted
// level set and the new one for one symbol. Same price → qty-only
// Change; a different price at a position replaces the slot (New);
// positions beyond the new depth Delete.
func diffBook(symbol string, oldBids, oldAsks, newBids, newAsks []marketdata.Level,
	entryTypes map[string]bool) []mdEntry {
	var out []mdEntry
	if mdEntryTypeAllowed(entryTypes, MDEntryBid) {
		diffSide(symbol, MDEntryBid, oldBids, newBids, &out)
	}
	if mdEntryTypeAllowed(entryTypes, MDEntryOffer) {
		diffSide(symbol, MDEntryOffer, oldAsks, newAsks, &out)
	}
	return out
}

func diffSide(symbol, etype string, old, new []marketdata.Level, out *[]mdEntry) {
	n := len(new)
	if len(old) > n {
		n = len(old)
	}
	for i := 0; i < n; i++ {
		pos := i + 1
		switch {
		case i >= len(old):
			*out = append(*out, mdEntry{
				action: MDActionNew, etype: etype, symbol: symbol,
				px:  decimal.NewFromScaled(new[i].Price).String(),
				qty: decimal.NewFromScaled(new[i].Qty).String(),
				pos: pos,
			})
		case i >= len(new):
			*out = append(*out, mdEntry{
				action: MDActionDelete, etype: etype, symbol: symbol,
				px:  decimal.NewFromScaled(old[i].Price).String(),
				pos: pos,
			})
		case old[i].Price != new[i].Price:
			*out = append(*out, mdEntry{
				action: MDActionNew, etype: etype, symbol: symbol,
				px:  decimal.NewFromScaled(new[i].Price).String(),
				qty: decimal.NewFromScaled(new[i].Qty).String(),
				pos: pos,
			})
		case old[i].Qty != new[i].Qty:
			*out = append(*out, mdEntry{
				action: MDActionChange, etype: etype, symbol: symbol,
				px:  decimal.NewFromScaled(new[i].Price).String(),
				qty: decimal.NewFromScaled(new[i].Qty).String(),
				pos: pos,
			})
		}
	}
}

// buildIncremental emits MarketDataIncrementalRefresh (35=X).
func buildIncremental(reqID string, entries []mdEntry) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgMarketDataIncrementalRefresh)
	m.Body.SetString(TagMDReqID, reqID)
	m.Body.SetString(TagAggregatedBook, "Y")

	grp := quickfix.NewRepeatingGroup(TagNoMDEntries, quickfix.GroupTemplate{
		quickfix.GroupElement(TagMDUpdateAction),
		quickfix.GroupElement(TagMDEntryType),
		quickfix.GroupElement(TagSymbol),
		quickfix.GroupElement(TagMDEntryPx),
		quickfix.GroupElement(TagMDEntrySize),
		quickfix.GroupElement(TagMDEntryPositionNo),
	})
	for _, e := range entries {
		g := grp.Add()
		g.SetInt(TagMDUpdateAction, e.action)
		g.SetString(TagMDEntryType, e.etype)
		g.SetString(TagSymbol, e.symbol)
		if e.px != "" {
			g.SetString(TagMDEntryPx, e.px)
		}
		if e.qty != "" {
			g.SetString(TagMDEntrySize, e.qty)
		}
		g.SetInt(TagMDEntryPositionNo, e.pos)
	}
	m.Body.SetGroup(grp)
	return m
}

// buildMDReject emits MarketDataRequestReject (35=Y). reason < 0 omits
// MDReqRejReason (optional in FIX 4.4) — used where no standard code
// fits (e.g. subscription cap). The FIX 4.4 35=Y dictionary carries no
// NoRelatedSym group, so failed symbols are folded into Text(58)
// instead of a repeating group.
func buildMDReject(reqID string, reason int, text string, symbols []string) *quickfix.Message {
	m := quickfix.NewMessage()
	m.Header.SetString(TagMsgType, MsgMarketDataRequestReject)
	m.Body.SetString(TagMDReqID, reqID)
	if reason >= 0 {
		m.Body.SetInt(TagMDReqRejReason, reason)
	}
	if len(symbols) > 0 {
		text += ": " + strings.Join(symbols, ",")
	}
	if text != "" {
		m.Body.SetString(TagText, text)
	}
	return m
}

// ---------------------------------------------------------------------------
// Lifecycle + introspection
// ---------------------------------------------------------------------------

// DropSession purges every subscription of a departed session — the
// acceptor's OnLogout/OnDisconnect path calls it.
func (s *MarketDataService) DropSession(id quickfix.SessionID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, id)
}

// SubscriptionCount reports live subscriptions for a session (tests +
// admin introspection).
func (s *MarketDataService) SubscriptionCount(id quickfix.SessionID) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[id])
}

// MDStats is the service's counter snapshot.
type MDStats struct {
	DeltasIn   uint64
	MsgsSent   uint64
	SendErrors uint64
}

// Stats returns the counter snapshot.
func (s *MarketDataService) Stats() MDStats {
	return MDStats{
		DeltasIn:   s.deltasIn.Load(),
		MsgsSent:   s.msgsSent.Load(),
		SendErrors: s.sendErrors.Load(),
	}
}
