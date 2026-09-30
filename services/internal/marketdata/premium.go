// Phase-23 Task 23.3.3 — premium data feeds (spec §10, Phase-23 task
// prose; §24 #250 kin for the Greeks sibling feed).
//
// The premium bundle is three WS channels over the existing transport:
//
//	premium_l3@{symbol}  — order-level L3 stream (Phase-17 machinery;
//	                       every ORDER_ADD/MODIFY/CANCEL/EXECUTE, never
//	                       conflated, channel seq = the real l3_seq).
//	depth_full@{symbol}  — full-depth book feed: every level the wire
//	                       BookSnapshot carries, emitted per delta (no
//	                       top-N cap, no 100ms conflation).
//	auction@{symbol}     — liquidation-auction events (§13.4). The
//	                       mandatory 2s anti-front-running gate of
//	                       §24 #263 applies to premium feeds too —
//	                       tiering buys the feed, not the delay waiver.
//
// Entitlement (§10.7): FeedEntitlements is an EntitlementChecker — a
// premium channel binds only for an authenticated session whose
// resolved tier is ≥ the feed's floor (Professional for the whole
// bundle — Task 23.3.4 remediation-#35 tier mapping: premium ≡
// Professional/Institutional) AND whose account carries an ACTIVE
// subscription row for that feed (migration 257,
// premium_feed_subscriptions). Denials surface ENTITLEMENT_REQUIRED via
// the shared gateChannel path — fail-closed end to end: nil store ⇒
// deny, store error ⇒ deny, expired/cancelled ⇒ deny.
//
// Billing: PremiumFeedBiller sweeps renews_at-due subscriptions and
// posts one balanced FEE journal per renewal through the
// ledger.JournalPoster seam (settlement.DoubleEntryLedgerService —
// identical shape to internal/funding's poster; debit client liability,
// credit revenue, wallet effect, idempotency key). A nil poster fails
// closed (no renewal is marked billed and no frame is fabricated); an
// optional BillingEventSink republishes each successful charge as a
// premium_feed_billing event for downstream invoicing/statements.
package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ledger"
	"exchange/internal/ratelimit"
	"exchange/internal/ws"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Premium feed vocabulary (channel types registered in channels.go).
// ---------------------------------------------------------------------------

const (
	// FeedPremiumL3 — premium_l3@{symbol}: the Phase-17 order-level
	// stream on the shared marketdata endpoint (the dedicated /ws/v1/l3
	// server remains the replay-ring surface; this channel is the
	// same-hub premium bundle member).
	FeedPremiumL3 = "premium_l3"
	// FeedDepthFull — depth_full@{symbol}: full book depth per delta.
	FeedDepthFull = "depth_full"
	// FeedAuction — auction@{symbol} / auction@all: §13.4 auction events.
	FeedAuction = "auction"
	// FeedGreeks — greeks@{symbol}: Task 23.3.5 real-time Greeks feed.
	FeedGreeks = "greeks"
)

// PremiumFeeds is the canonical bundle — feed_name values are the
// migration-257 CHECK domain; keep the table and this map in lockstep.
var PremiumFeeds = map[string]ratelimit.Tier{
	FeedPremiumL3: ratelimit.TierProfessional,
	FeedDepthFull: ratelimit.TierProfessional,
	FeedAuction:   ratelimit.TierProfessional,
	FeedGreeks:    ratelimit.TierProfessional,
}

// ---------------------------------------------------------------------------
// FeedSubscription + store seam (migration 257)
// ---------------------------------------------------------------------------

// FeedStatus mirrors premium_feed_status_enum.
type FeedStatus string

const (
	FeedStatusActive    FeedStatus = "ACTIVE"
	FeedStatusCancelled FeedStatus = "CANCELLED"
)

// FeedSubscription is one account's monthly subscription to one premium
// feed — the row shape of premium_feed_subscriptions.
type FeedSubscription struct {
	ID                 int64
	AccountID          int64
	Feed               string // channel type token ("premium_l3", ...)
	Status             FeedStatus
	StartedAt          time.Time
	BilledMonthlyMinor int64  // minor currency units (int8 per spec task)
	Currency           string // ISO 4217, char(3)
	RenewsAt           time.Time
	LastBilledAt       *time.Time
	LastJournalID      *int64
}

// FeedSubscriptionStore is the entitlement/billing persistence seam.
// PgxFeedSubscriptionStore is the production implementation; tests use
// fakes. All lookups fail closed at the entitlement gate.
type FeedSubscriptionStore interface {
	// ActiveSubscription returns the account's ACTIVE row for feed.
	// ok=false means no active subscription — a denial, not an error.
	ActiveSubscription(ctx context.Context, accountID int64, feed string) (sub FeedSubscription, ok bool, err error)
	// DueSubscriptions lists ACTIVE rows whose renews_at <= asOf (billing
	// sweep input), oldest first, capped at limit.
	DueSubscriptions(ctx context.Context, asOf time.Time, limit int) ([]FeedSubscription, error)
	// MarkBilled records a successful charge: last_billed_at + journal
	// id are stamped and renews_at rolls forward one period.
	MarkBilled(ctx context.Context, id int64, billedAt, renewsAt time.Time, journalID int64) error
}

// PgxFeedSubscriptionStore implements FeedSubscriptionStore over
// migration 257.
type PgxFeedSubscriptionStore struct {
	pool *pgxpool.Pool
}

// NewPgxFeedSubscriptionStore binds the subscription table.
func NewPgxFeedSubscriptionStore(pool *pgxpool.Pool) *PgxFeedSubscriptionStore {
	return &PgxFeedSubscriptionStore{pool: pool}
}

const feedSubColumns = `id, account_id, feed_name, status, started_at,
	billed_monthly_minor, currency, renews_at, last_billed_at, last_journal_id`

func scanFeedSub(row pgx.Row) (FeedSubscription, error) {
	var s FeedSubscription
	err := row.Scan(&s.ID, &s.AccountID, &s.Feed, &s.Status, &s.StartedAt,
		&s.BilledMonthlyMinor, &s.Currency, &s.RenewsAt,
		&s.LastBilledAt, &s.LastJournalID)
	return s, err
}

// ActiveSubscription implements FeedSubscriptionStore.
func (s *PgxFeedSubscriptionStore) ActiveSubscription(ctx context.Context,
	accountID int64, feed string) (FeedSubscription, bool, error) {
	sub, err := scanFeedSub(s.pool.QueryRow(ctx, `
		SELECT `+feedSubColumns+`
		FROM premium_feed_subscriptions
		WHERE account_id = $1 AND feed_name = $2 AND status = 'ACTIVE'`,
		accountID, feed))
	if err == pgx.ErrNoRows {
		return FeedSubscription{}, false, nil
	}
	if err != nil {
		return FeedSubscription{}, false,
			fmt.Errorf("marketdata: feed subscription lookup: %w", err)
	}
	return sub, true, nil
}

// DueSubscriptions implements FeedSubscriptionStore.
func (s *PgxFeedSubscriptionStore) DueSubscriptions(ctx context.Context,
	asOf time.Time, limit int) ([]FeedSubscription, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+feedSubColumns+`
		FROM premium_feed_subscriptions
		WHERE status = 'ACTIVE' AND renews_at <= $1
		ORDER BY renews_at
		LIMIT $2`, asOf.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("marketdata: due feed subscriptions query: %w", err)
	}
	defer rows.Close()
	var out []FeedSubscription
	for rows.Next() {
		sub, err := scanFeedSub(rows)
		if err != nil {
			return nil, fmt.Errorf("marketdata: feed subscription row: %w", err)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// MarkBilled implements FeedSubscriptionStore.
func (s *PgxFeedSubscriptionStore) MarkBilled(ctx context.Context, id int64,
	billedAt, renewsAt time.Time, journalID int64) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE premium_feed_subscriptions
		SET last_billed_at = $2, renews_at = $3, last_journal_id = $4,
		    updated_at = now()
		WHERE id = $1 AND status = 'ACTIVE'`,
		id, billedAt.UTC(), renewsAt.UTC(), journalID)
	if err != nil {
		return fmt.Errorf("marketdata: mark feed subscription billed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("marketdata: feed subscription %d not ACTIVE — refusing to mark billed", id)
	}
	return nil
}

// ---------------------------------------------------------------------------
// FeedEntitlements — the §10.7 premium gate (Task 23.3.3 item 4).
// ---------------------------------------------------------------------------

// FeedEntitlement names one gated feed and its minimum caller tier —
// the {Feed, Tier} pair the entitlement resolver consults before the
// subscription lookup runs.
type FeedEntitlement struct {
	Feed string         // channel type token (FeedPremiumL3, ...)
	Tier ratelimit.Tier // minimum tier (Professional for the bundle)
}

// tierRank orders tiers for the ≥ floor comparison. Demo deliberately
// ranks with basic — a demo key never unlocks a billed feed.
func tierRank(t ratelimit.Tier) int {
	switch t {
	case ratelimit.TierPublic:
		return 0
	case ratelimit.TierBasic, ratelimit.TierDemo:
		return 1
	case ratelimit.TierStandard:
		return 2
	case ratelimit.TierProfessional:
		return 3
	case ratelimit.TierInstitutional:
		return 4
	case ratelimit.TierAdmin:
		return 5
	}
	return 0 // unknown → lowest (fail-closed)
}

// tierAtLeast reports whether t meets the floor.
func tierAtLeast(t, floor ratelimit.Tier) bool { return tierRank(t) >= tierRank(floor) }

// FeedEntitlements is the EntitlementChecker gating the premium bundle.
// Non-gated channel types pass through unharmed — compose with
// StaticEntitlements via ChainEntitlements when both apply.
//
// Fail-closed invariants:
//   - unauthenticated sessions deny (premium feeds need identity);
//   - unknown tier labels resolve to public and deny;
//   - a nil/erroring store denies — entitlement that cannot be proven
//     is entitlement that does not exist;
//   - lookups carry a bounded TTL cache so the subscribe hot path never
//     hits Postgres per bind; errors are never cached.
type FeedEntitlements struct {
	// Feeds maps channel type → minimum tier. Nil/empty defaults to
	// PremiumFeeds (whole bundle at Professional).
	Feeds map[string]ratelimit.Tier
	// Store resolves ACTIVE subscription rows. Nil ⇒ every premium bind
	// denies (deployment does not sell feeds).
	Store FeedSubscriptionStore
	// TierResolver optionally maps a session to its tier (deployment
	// override, e.g. JWT-role mapping). Nil ⇒ sess.Tier (the API key's
	// stored RateLimitTier) parsed via ratelimit.ParseTier.
	TierResolver func(sess *ws.Session) ratelimit.Tier
	// CacheTTL bounds subscription lookups (default 30s); errors bypass
	// the cache entirely.
	CacheTTL time.Duration
	// LookupTimeout bounds each store call (default 500ms).
	LookupTimeout time.Duration
	Now           func() time.Time

	mu    sync.Mutex
	cache map[string]cachedSub
}

type cachedSub struct {
	active bool
	at     time.Time
}

func (e *FeedEntitlements) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *FeedEntitlements) gated(feed string) (ratelimit.Tier, bool) {
	if e.Feeds == nil {
		t, ok := PremiumFeeds[feed]
		return t, ok
	}
	t, ok := e.Feeds[feed]
	return t, ok
}

// subscribed resolves the ACTIVE-subscription verdict with a small
// positive/negative cache (subscription rows change on billing/admin
// timescales, not subscribe-path timescales).
func (e *FeedEntitlements) subscribed(accountID int64, feed string) (bool, error) {
	key := fmt.Sprintf("%d|%s", accountID, feed)
	ttl := e.CacheTTL
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	e.mu.Lock()
	if c, ok := e.cache[key]; ok && e.now().Sub(c.at) < ttl {
		e.mu.Unlock()
		return c.active, nil
	}
	e.mu.Unlock()

	to := e.LookupTimeout
	if to <= 0 {
		to = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), to)
	_, active, err := e.Store.ActiveSubscription(ctx, accountID, feed)
	cancel()
	if err != nil {
		return false, err // never cached — a store blip must not pin a denial
	}
	e.mu.Lock()
	if e.cache == nil {
		e.cache = map[string]cachedSub{}
	}
	e.cache[key] = cachedSub{active: active, at: e.now()}
	e.mu.Unlock()
	return active, nil
}

// Check implements EntitlementChecker.
func (e *FeedEntitlements) Check(sess *ws.Session, ch Channel) (bool, string) {
	minTier, gated := e.gated(ch.Type)
	if !gated {
		return true, "" // not a premium channel — nothing to gate
	}
	if sess == nil || !sess.Authenticated {
		return false, ch.Raw + " requires an authenticated session"
	}
	tier := ratelimit.ParseTier(sess.Tier)
	if e.TierResolver != nil {
		tier = e.TierResolver(sess)
	}
	if !tierAtLeast(tier, minTier) {
		return false, fmt.Sprintf(
			"%s requires %s tier or higher (session tier %q)",
			ch.Raw, minTier, tier)
	}
	if sess.AccountID == 0 {
		return false, ch.Raw + " requires an account-bound session"
	}
	if e.Store == nil {
		return false, ch.Raw + " subscriptions are not enabled on this deployment"
	}
	ok, err := e.subscribed(sess.AccountID, ch.Type)
	if err != nil {
		return false, "entitlement lookup failed — " + ch.Raw + " denied"
	}
	if !ok {
		return false, "no active " + ch.Type + " subscription for this account"
	}
	return true, ""
}

// ChainEntitlements composes checkers: the first denial wins, all must
// admit for the bind to proceed (e.g. StaticEntitlements symbol lists +
// FeedEntitlements premium gates).
func ChainEntitlements(checkers ...EntitlementChecker) EntitlementChecker {
	return EntitlementFunc(func(sess *ws.Session, ch Channel) (bool, string) {
		for _, c := range checkers {
			if c == nil {
				continue
			}
			if ok, why := c.Check(sess, ch); !ok {
				return false, why
			}
		}
		return true, ""
	})
}

// ---------------------------------------------------------------------------
// premium_l3@{symbol} — order-level relay over the L3 machinery.
// ---------------------------------------------------------------------------

// premiumL3Data is the emitted payload: the standard L3 event fields
// plus the real l3_seq mirrored into the data body (the frame envelope's
// seq already carries it — duplicating makes the resume cursor explicit).
type premiumL3Data struct {
	l3EventPayload
	L3Seq uint64 `json:"l3_seq"`
}

// PremiumL3Producer relays an L3Source onto premium_l3@{symbol}
// channels. Every event forwards unmodified — no conflation, the
// Phase-17 contract. The channel sequence IS the engine's per-symbol
// l3_seq so WS resume cursors are the same domain the dedicated
// /ws/v1/l3 endpoint replays.
type PremiumL3Producer struct {
	src  L3Source
	emit EmitFunc
	log  *slog.Logger
}

// NewPremiumL3Producer wires the relay; emit is Server.Publish.
func NewPremiumL3Producer(src L3Source, emit EmitFunc, log *slog.Logger) *PremiumL3Producer {
	if log == nil {
		log = slog.Default()
	}
	return &PremiumL3Producer{src: src, emit: emit, log: log}
}

// Run drives the relay until ctx is cancelled or the source closes.
func (p *PremiumL3Producer) Run(ctx context.Context) error {
	var ch <-chan L3Event
	if p.src != nil {
		evs, err := p.src.Events(ctx)
		if err != nil {
			return fmt.Errorf("marketdata: premium l3 source: %w", err)
		}
		ch = evs
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			p.Publish(ev)
		}
	}
}

// Publish emits one L3 event on premium_l3@{symbol}. Exported for
// tests/embedders driving the hub's event stream directly.
func (p *PremiumL3Producer) Publish(ev L3Event) {
	if ev.Symbol == "" {
		return // unrouted event — never publish to an untyped channel
	}
	p.emit(FeedPremiumL3+"@"+ev.Symbol, ev.Seq,
		premiumL3Data{l3EventPayload: ev.payload(), L3Seq: ev.Seq})
}

// ---------------------------------------------------------------------------
// depth_full@{symbol} — full-depth book feed (all wire levels, per delta).
// ---------------------------------------------------------------------------

// FullDepthProducer republishes every book delta un-sliced: the public
// book@/depth@ channels cap at the §10.1 top-20 conflated frame; this
// premium channel emits every level the wire snapshot carries, on every
// delta — no conflation window (premium subscribers bought resolution,
// not bandwidth savings). The §10.9 seq envelope and §24 #83 CRC32 are
// computed over the FULL level set so resync verifies what was emitted.
type FullDepthProducer struct {
	src  DeltaSource
	emit EmitFunc
	seq  *seqAllocator
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex
	lastSeq map[string]uint64 // prev_last_seq per channel
}

// NewFullDepthProducer wires the producer; emit is Server.Publish.
// now defaults to time.Now.
func NewFullDepthProducer(src DeltaSource, emit EmitFunc,
	now func() time.Time, log *slog.Logger) *FullDepthProducer {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &FullDepthProducer{
		src: src, emit: emit, seq: newSeqAllocator(),
		log: log, now: now, lastSeq: map[string]uint64{},
	}
}

// Push injects a delta directly (tests/embedders).
func (p *FullDepthProducer) Push(d BookDelta) { p.Publish(d) }

// Publish emits one full-depth frame on depth_full@{symbol}.
func (p *FullDepthProducer) Publish(d BookDelta) {
	if d.Symbol == "" {
		return
	}
	channel := FeedDepthFull + "@" + d.Symbol
	seq := p.seq.next(channel)
	p.mu.Lock()
	prev := p.lastSeq[channel]
	p.lastSeq[channel] = seq
	p.mu.Unlock()
	p.emit(channel, seq, depthUpdate{
		Event:       "depthUpdate",
		Symbol:      d.Symbol,
		Bids:        renderLevels(d.Bids, 0), // 0 = every level — full depth
		Asks:        renderLevels(d.Asks, 0),
		FirstSeq:    seq,
		LastSeq:     seq,
		PrevLastSeq: prev,
		Seq:         seq,
		EngineSeq:   d.EngineSeq,
		Coalesced:   1,
		CRC32:       depthChecksum(d.Bids, d.Asks, 0),
		TsMs:        p.now().UnixMilli(),
	})
}

// Run drives the producer until ctx is cancelled or the source closes.
func (p *FullDepthProducer) Run(ctx context.Context) error {
	var srcCh <-chan BookDelta
	if p.src != nil {
		ch, err := p.src.Deltas(ctx)
		if err != nil {
			return fmt.Errorf("marketdata: full-depth source: %w", err)
		}
		srcCh = ch
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-srcCh:
			if !ok {
				return nil
			}
			p.Publish(d)
		}
	}
}

// ---------------------------------------------------------------------------
// auction@{symbol} / auction@all — §13.4 liquidation-auction events.
// ---------------------------------------------------------------------------

// auctionData is the emitted payload — anonymized like the liquidations
// feed (no account/order identity can reach the wire).
type auctionData struct {
	Event     string `json:"event"` // "auction"
	Symbol    string `json:"symbol"`
	Side      string `json:"side"`
	OrderType string `json:"order_type"`
	Price     string `json:"price"`
	Quantity  string `json:"quantity"`
	TsMs      int64  `json:"ts_ms"`
	PubMs     int64  `json:"pub_ts_ms"`
}

// AuctionsProducer publishes auction-phase liquidation events to the
// premium auction channels. It consumes the same LiquidationSource as
// the public feed and filters to IsAuction events — the §13.4 auction
// lifecycle (call/extension/fill) is what subscribers bought.
//
// THE 2s GATE STILL APPLIES: premium access does not relax §24 #263 —
// an auction front-runner on a paid feed is still front-running. The
// DelayGate floor is enforced in the constructor exactly like the
// public producer.
type AuctionsProducer struct {
	cfg  LiquidationsProducerConfig
	src  LiquidationSource
	emit EmitFunc
	seq  *seqAllocator
	in   chan LiquidationEvent
	gate *DelayGate[LiquidationEvent]
}

// NewAuctionsProducer wires the producer; emit is Server.Publish. The
// configured delay clamps UP to LiquidationDelay — a smaller value is a
// config bug, never a premium feature.
func NewAuctionsProducer(cfg LiquidationsProducerConfig,
	src LiquidationSource, emit EmitFunc) *AuctionsProducer {
	cfg.defaults()
	p := &AuctionsProducer{
		cfg: cfg, src: src, emit: emit,
		seq: newSeqAllocator(),
		in:  make(chan LiquidationEvent, cfg.InputBuffer),
	}
	p.gate = NewDelayGate[LiquidationEvent](cfg.Delay, p.publish, cfg.Now)
	return p
}

// Push injects an event directly (tests/embedders). Non-auction events
// are still gated and then dropped at publish — the feed contract is
// "auction events only".
func (p *AuctionsProducer) Push(ev LiquidationEvent) {
	select {
	case p.in <- ev:
	default:
		p.cfg.Logger.Error("marketdata: auction input saturated — event dropped",
			"symbol", ev.Symbol)
	}
}

// publish emits one released auction event post-delay.
func (p *AuctionsProducer) publish(ev LiquidationEvent) {
	if ev.Symbol == "" || !ev.IsAuction {
		return
	}
	ot := ev.OrderType
	if ot == "" {
		ot = "UNKNOWN"
	}
	data := auctionData{
		Event: "auction", Symbol: ev.Symbol, Side: ev.Side,
		OrderType: ot, Price: ev.Price, Quantity: ev.Qty,
		TsMs:  ev.Ts.UnixMilli(),
		PubMs: p.cfg.Now().UnixMilli(),
	}
	p.emit(FeedAuction+"@"+ev.Symbol,
		p.seq.next(FeedAuction+"@"+ev.Symbol), data)
	p.emit(FeedAuction+"@all", p.seq.next(FeedAuction+"@all"), data)
}

// Gate exposes the delay gate's depth counters (metrics/tests).
func (p *AuctionsProducer) Gate() *DelayGate[LiquidationEvent] { return p.gate }

// Run drives the producer until ctx is cancelled.
func (p *AuctionsProducer) Run(ctx context.Context) error {
	var srcCh <-chan LiquidationEvent
	if p.src != nil {
		ch, err := p.src.Liquidations(ctx)
		if err != nil {
			return fmt.Errorf("marketdata: auction source: %w", err)
		}
		srcCh = ch
	}
	gateDone := make(chan error, 1)
	go func() { gateDone <- p.gate.Run(ctx) }()
	for {
		select {
		case err := <-gateDone:
			return err
		case <-ctx.Done():
			<-gateDone
			return ctx.Err()
		case ev, ok := <-srcCh:
			if !ok {
				srcCh = nil
				continue
			}
			p.gate.Push(ev, ev.Ts)
		case ev := <-p.in:
			p.gate.Push(ev, ev.Ts)
		}
	}
}

// ---------------------------------------------------------------------------
// Monthly billing (Task 23.3.3 item 5) — per-feed subscription fee.
// ---------------------------------------------------------------------------

// PremiumFeedBillingEvent is emitted per successful renewal charge —
// downstream consumers (statements, invoices, revenue reporting) treat
// it as the billing fact. Post-commit only: a failed journal never
// produces an event.
type PremiumFeedBillingEvent struct {
	Event          string `json:"event"` // "premium_feed_billing"
	SubscriptionID int64  `json:"subscription_id"`
	AccountID      int64  `json:"account_id"`
	Feed           string `json:"feed"`
	Amount         string `json:"amount"` // decimal string, major units
	Currency       string `json:"currency"`
	Period         string `json:"period"` // "2006-01" — the month the renewal covers
	JournalID      int64  `json:"journal_id"`
	TsMs           int64  `json:"ts_ms"`
}

// BillingEventSink republishes a committed charge (e.g. JetStream on
// the "funding" stream). Nil disables the hop — billing still posts.
type BillingEventSink interface {
	PublishBilling(ctx context.Context, ev PremiumFeedBillingEvent) error
}

// BillingEventSinkFunc adapts a function to BillingEventSink.
type BillingEventSinkFunc func(ctx context.Context, ev PremiumFeedBillingEvent) error

// PublishBilling implements BillingEventSink.
func (f BillingEventSinkFunc) PublishBilling(ctx context.Context, ev PremiumFeedBillingEvent) error {
	return f(ctx, ev)
}

// ccyExponent returns the ISO 4217 minor-unit exponent for the fiat
// currencies this venue lists (0-decimal and 3-decimal exceptions;
// everything else is 2). billed_monthly_minor is a minor-unit count —
// the exponent converts it to a major-unit decimal for the ledger.
func ccyExponent(ccy string) int32 {
	switch strings.ToUpper(ccy) {
	case "JPY", "KRW", "VND", "CLP", "HUF", "ISK":
		return 0
	case "BHD", "KWD", "OMR", "JOD", "IQD", "TND", "LYD":
		return 3
	}
	return 2
}

// PremiumFeedBiller sweeps due subscriptions and posts the monthly fee.
// The poster is the §5.3 double-entry seam — settlement.
// DoubleEntryLedgerService satisfies ledger.JournalPoster; a nil poster
// fails closed (nothing is marked billed, the sweep errors loudly).
type PremiumFeedBiller struct {
	store  FeedSubscriptionStore
	poster ledger.JournalPoster
	events BillingEventSink // optional
	now    func() time.Time
	log    *slog.Logger

	BatchSize int // due-row cap per sweep; default 500
}

// NewPremiumFeedBiller wires the sweeper.
func NewPremiumFeedBiller(store FeedSubscriptionStore, poster ledger.JournalPoster,
	events BillingEventSink, now func() time.Time, log *slog.Logger) *PremiumFeedBiller {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return &PremiumFeedBiller{
		store: store, poster: poster, events: events,
		now: now, log: log,
	}
}

// BillDue bills every subscription at/past renews_at. Per-row failures
// (insufficient balance, poster outage) are logged and skipped — the
// row stays ACTIVE and unpaid, retried on the next sweep; the
// subscription gate never consults payment state mid-period (the
// backoffice CANCELLED transition is the revocation path).
func (b *PremiumFeedBiller) BillDue(ctx context.Context) (int, error) {
	if b.poster == nil {
		return 0, fmt.Errorf("marketdata: premium feed biller has no ledger poster — refusing to bill (fail-closed)")
	}
	limit := b.BatchSize
	if limit <= 0 {
		limit = 500
	}
	due, err := b.store.DueSubscriptions(ctx, b.now(), limit)
	if err != nil {
		return 0, err
	}
	billed := 0
	for _, sub := range due {
		if err := b.bill(ctx, sub); err != nil {
			b.log.Error("marketdata: premium feed billing failed",
				"subscription_id", sub.ID, "account_id", sub.AccountID,
				"feed", sub.Feed, "err", err)
			continue
		}
		billed++
	}
	return billed, nil
}

// bill posts one renewal charge and rolls the subscription forward.
func (b *PremiumFeedBiller) bill(ctx context.Context, sub FeedSubscription) error {
	if sub.BilledMonthlyMinor <= 0 {
		return fmt.Errorf("subscription %d has non-positive monthly price %d", sub.ID, sub.BilledMonthlyMinor)
	}
	if len(sub.Currency) != 3 {
		return fmt.Errorf("subscription %d currency %q is not ISO 4217 char(3)", sub.ID, sub.Currency)
	}
	amount := decimal.New(sub.BilledMonthlyMinor, -ccyExponent(sub.Currency))
	// The renewal being billed covers the month STARTING at renews_at
	// (the due date marks the period boundary). Idempotency keys on
	// (subscription, period) — a replayed sweep row resolves to the
	// committed journal instead of double-charging.
	period := sub.RenewsAt.UTC().Format("2006-01")
	res, err := b.poster.Post(ctx, ledger.Journal{
		EntryType:      ledger.EntryFee,
		ReferenceID:    sub.ID,
		Description:    fmt.Sprintf("premium feed %s monthly subscription %s", sub.Feed, period),
		PostedBy:       "marketdata-premium-billing",
		IdempotencyKey: fmt.Sprintf("premium-feed:%d:%s", sub.ID, period),
		Lines: []ledger.Line{
			ledger.DebitLine(ledger.CustomerLiability(sub.Currency), sub.Currency, amount,
				"premium data feed subscription fee"),
			// 4030_COMMISSION_REVENUE is the nearest existing GL revenue
			// line — the chart has no dedicated market-data-revenue code;
			// a 4xxx MARKET_DATA_FEE_REVENUE row is a ledger-chart-owner
			// follow-up (Phase-03), not something this task invents here.
			ledger.CreditLine(ledger.CommissionRevenue(sub.Currency), sub.Currency, amount,
				"premium data feed revenue"),
		},
		Effects: []ledger.AccountEffect{{
			AccountID:      sub.AccountID,
			Currency:       sub.Currency,
			AvailableDelta: amount.Neg(),
		}},
	})
	if err != nil && !res.Committed {
		return err
	}
	next := sub.RenewsAt.UTC().AddDate(0, 1, 0) // roll from the boundary, not now — no drift
	if err := b.store.MarkBilled(ctx, sub.ID, b.now(), next, res.JournalID); err != nil {
		return err
	}
	if b.events != nil {
		ev := PremiumFeedBillingEvent{
			Event:          "premium_feed_billing",
			SubscriptionID: sub.ID, AccountID: sub.AccountID,
			Feed: sub.Feed, Amount: amount.String(), Currency: sub.Currency,
			Period: period, JournalID: res.JournalID,
			TsMs: b.now().UnixMilli(),
		}
		if perr := b.events.PublishBilling(ctx, ev); perr != nil {
			// Post-commit: the charge is final; the event is downstream
			// fan-out (statements/invoices). Log loud, never roll back.
			b.log.Error("marketdata: premium_feed_billing event dispatch failed",
				"subscription_id", sub.ID, "err", perr)
		}
	}
	return nil
}
