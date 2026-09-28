// Task 6.3.5 — private order stream (spec §10.5 private channel set,
// §10.7 seq durability, §24 #265 execution-event distribution).
//
// `private:orders` is the canonical account-scoped order-lifecycle
// channel served on the /ws/v1/orders surface: acknowledgement, fill,
// partial fill, cancellation, expiry (with expiry_reason) and rejection
// events. Account isolation is structural — the hub fans out through
// PublishPrivate which delivers only to sessions whose AccountID
// matches, and replay lives on the per-account ring (Task 6.3.9
// item 7).
//
// Sequence domain: each (channel, account) pair keeps its own monotonic
// cursor persisted as md:seq:private:{channel}:{accountID} — the same
// high-water-mark discipline as the public md:seq:{symbol} mirror
// (Task 6.3.22: a restart continues the cursor, never resets to 0). A
// restarted stream seeds from SeqStore and journals the restart
// boundary so gap auditing covers the private surface too.
package marketdata

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"exchange/internal/ws"
)

// Order lifecycle event names emitted on private:orders (§10.5). The
// vocabulary mirrors the gateway's order-event names so private-feed
// clients parse one schema.
const (
	OrderEventAck       = "orderAck"       // engine acknowledged the order
	OrderEventFill      = "orderFill"      // fill or partial fill
	OrderEventCancel    = "orderCancel"    // user/system cancellation
	OrderEventExpire    = "orderExpire"    // TIF/expiry — expiry_reason set
	OrderEventReject    = "orderReject"    // validation/risk rejection
	OrderEventAmend     = "orderAmend"     // amendment accepted
	OrderEventTrigger   = "orderTrigger"   // conditional trigger fired
	OrderEventLiquidate = "orderLiquidate" // liquidation-driven event
)

// OrderEvent is the private:orders wire payload. Quantities and prices
// render as decimal strings (1e-8 wire scale, same as the public L2
// surface — clients parse one numeric format). Event carries the
// OrderEvent* name; ExpiryReason carries the instrument-level expiry
// cause for OrderEventExpire ("gtc_90d", "day_end", "gtd", ...) per the
// execution-rule distribution contract (spec §24 #265).
type OrderEvent struct {
	Event        string `json:"event"`
	OrderID      string `json:"order_id"`
	ClientID     string `json:"client_order_id,omitempty"`
	Symbol       string `json:"symbol"`
	Side         string `json:"side,omitempty"` // BUY | SELL
	Type         string `json:"type,omitempty"` // LIMIT | MARKET | ...
	Status       string `json:"status"`         // NEW|PARTIALLY_FILLED|FILLED|CANCELLED|EXPIRED|REJECTED
	Price        string `json:"price,omitempty"`
	Quantity     string `json:"quantity,omitempty"`
	FilledQty    string `json:"filled_qty,omitempty"`
	LeavesQty    string `json:"leaves_qty,omitempty"`
	LastFillQty  string `json:"last_fill_qty,omitempty"`
	LastFillPx   string `json:"last_fill_price,omitempty"`
	ExpiryReason string `json:"expiry_reason,omitempty"`
	Reason       string `json:"reason,omitempty"` // reject/cancel cause
	TsMs         int64  `json:"ts_ms"`
}

// PrivateSeqKey is the durable seq domain for one (channel, account)
// pair — feeds SeqStore so the key materializes as
// md:seq:private:orders:{accountID}.
func PrivateSeqKey(channel string, accountID int64) string {
	return "private:" + channel + ":" + strconv.FormatInt(accountID, 10)
}

// PrivateOrderStream sequences and fans out account-scoped order
// events. Producers (the order-event bridge, Phase-05 dispatcher
// feedback, Phase-19 margin machinery) call Publish; the stream owns
// the per-account seq cursor and the durable mirror.
//
// Safe for concurrent use: seq allocation per account is serialized;
// cross-account Publish calls proceed in parallel.
type PrivateOrderStream struct {
	srv     *Server
	seq     SeqStore
	journal GapJournal
	log     *slog.Logger
	now     func() time.Time

	mu      sync.Mutex
	cursors map[string]uint64 // private seq domain → last emitted seq
	seeded  map[string]bool   // domain seeded from SeqStore
}

// NewPrivateOrderStream binds the stream to the hub's private fanout.
// seq may be nil (in-memory cursors — restart then resyncs clients,
// never resets silently: the ring empties and replay verdicts direct
// resync). journal may be nil (gap auditing disabled).
func NewPrivateOrderStream(srv *Server, seq SeqStore,
	journal GapJournal, log *slog.Logger) *PrivateOrderStream {
	if log == nil {
		log = slog.Default()
	}
	return &PrivateOrderStream{
		srv:     srv,
		seq:     seq,
		journal: journal,
		log:     log,
		now:     time.Now,
		cursors: map[string]uint64{},
		seeded:  map[string]bool{},
	}
}

// Publish emits one order event to the bound account's private channel
// under a fresh per-account seq. channel must be a registered
// private:* channel (ws.PrivateChannels); empty selects the canonical
// "private:orders". Returns the emitted seq.
func (p *PrivateOrderStream) Publish(accountID int64,
	channel string, ev OrderEvent) (uint64, error) {
	if accountID <= 0 {
		return 0, fmt.Errorf("marketdata: private publish requires accountID > 0, got %d", accountID)
	}
	if channel == "" {
		channel = "private:orders"
	}
	if !ws.PrivateChannels[channel] {
		return 0, fmt.Errorf("marketdata: %q is not a private channel", channel)
	}
	if ev.Event == "" {
		return 0, fmt.Errorf("marketdata: order event requires event type")
	}
	if ev.TsMs == 0 {
		ev.TsMs = p.now().UnixMilli()
	}
	seq, err := p.allocSeq(channel, accountID)
	if err != nil {
		return 0, err
	}
	p.srv.PublishPrivate(accountID, channel, seq, ev)
	return seq, nil
}

// allocSeq bumps the per-(channel,account) cursor, seeding from the
// durable SeqStore on first touch so a restart continues the sequence
// (Task 6.3.22 item 1). Seed failure degrades to local-0 with a
// GapSeedFailed journal entry — clients holding higher cursors resync
// instead of silently accepting a regression.
func (p *PrivateOrderStream) allocSeq(channel string, accountID int64) (uint64, error) {
	key := PrivateSeqKey(channel, accountID)
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seeded[key] {
		p.seeded[key] = true
		if p.seq != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			v, err := p.seq.Load(ctx, key)
			cancel()
			if err != nil {
				p.log.Error("marketdata: private seq seed failed",
					"key", key, "err", err)
				noteGap(p.journal, p.log, key, SeqGap{
					From: 0, To: 0, Reason: GapSeedFailed,
				})
			} else {
				p.cursors[key] = v
				if v > 0 {
					noteGap(p.journal, p.log, key, SeqGap{
						From: v, To: v, Reason: GapRestartBoundary,
					})
				}
			}
		}
	}
	p.cursors[key]++
	cur := p.cursors[key]
	// High-water mirror, fire-and-forget — same discipline as the L2
	// conflator's md:seq writes (mirror failure never stalls fanout).
	if p.seq != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := p.seq.Store(ctx, key, cur); err != nil {
				p.log.Warn("marketdata: private seq mirror failed",
					"key", key, "seq", cur, "err", err)
			}
		}()
	}
	return cur, nil
}

// Cursor reports the last emitted seq for the (channel, account) domain
// — tests and health probes.
func (p *PrivateOrderStream) Cursor(channel string, accountID int64) uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cursors[PrivateSeqKey(channel, accountID)]
}
