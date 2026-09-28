// Dual-feed publisher (Task 6.3.6 item 2). Each Publish call encodes the
// packet ONCE and sends the identical datagram on Feed A and Feed B — A/B
// byte-equivalence and identical channel sequences (spec §10.4, §24 #166)
// are guaranteed by construction. Every packet is journaled first so the
// replay service can repair gaps even if a feed send subsequently fails.
package sbe

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// FeedID identifies one redundant feed.
type FeedID int

const (
	FeedA FeedID = 0
	FeedB FeedID = 1
)

func (f FeedID) String() string {
	if f == FeedA {
		return "A"
	}
	return "B"
}

// PublisherStats are per-feed cumulative counters for the instrumentation
// of Task 6.3.6 item 5 (per-feed lag/loss surfaced to Phase-07 metrics).
type PublisherStats struct {
	PacketsSent   atomic.Uint64 // packets accepted onto >=1 feed
	FeedASendErr  atomic.Uint64
	FeedBSendErr  atomic.Uint64
	DualSendErr   atomic.Uint64 // both feeds failed — packet only in journal
	JournalAppend atomic.Uint64
}

// Publisher fans a sequenced channel onto two transports.
//
// Concurrency: Publish is NOT goroutine-safe by design — market-data
// publication is a single hot-path goroutine in the marketdata service
// (the engine event loop is single-writer). Callers needing concurrent
// publish must serialize externally.
type Publisher struct {
	channelID uint16
	sessionID uint64
	feeds     [2]Sender
	journal   *Journal
	log       *slog.Logger
	now       func() int64
	seq       uint64
	Stats     PublisherStats
}

// PublisherConfig wires a Publisher.
type PublisherConfig struct {
	ChannelID uint16
	// SessionID is the publisher epoch stamped into every packet header;
	// it MUST change on restart so consumers detect the channel reset
	// (MoldUDP64 session semantics). Zero → time.Now().UnixNano().
	SessionID uint64
	FeedA     Sender // required
	FeedB     Sender // required
	Journal   *Journal
	Logger    *slog.Logger // nil → slog.Default()
	// NowNs supplies EventTimeNs for heartbeats; nil → wall clock.
	// Inject a fake clock in tests for determinism.
	NowNs func() int64
}

// NewPublisher validates cfg.
func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	if cfg.FeedA == nil || cfg.FeedB == nil {
		return nil, errNilTransport
	}
	if cfg.Journal == nil {
		return nil, errNilJournal
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	now := cfg.NowNs
	if now == nil {
		now = func() int64 { return time.Now().UnixNano() }
	}
	sess := cfg.SessionID
	if sess == 0 {
		sess = uint64(now())
	}
	return &Publisher{
		channelID: cfg.ChannelID,
		sessionID: sess,
		feeds:     [2]Sender{cfg.FeedA, cfg.FeedB},
		journal:   cfg.Journal,
		log:       log,
		now:       now,
	}, nil
}

// Seq returns the last published channel sequence (0 before first publish).
func (p *Publisher) Seq() uint64 { return p.seq }

// SessionID returns the publisher epoch stamped into packet headers.
func (p *Publisher) SessionID() uint64 { return p.sessionID }

// Publish stamps msgs with the next channel sequence, journals the datagram,
// then sends identical bytes on both feeds. A single-feed failure is logged
// and counted but does not fail the publish (spec §10.4: either feed may
// fail with no client-visible gap); a dual failure returns an error — the
// packet remains journaled and replayable.
func (p *Publisher) Publish(ctx context.Context, msgs ...Message) (uint64, error) {
	enc := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		enc = append(enc, MarshalMessage(m))
	}
	seq := p.seq + 1
	dg, err := EncodePacket(p.channelID, p.sessionID, seq, enc)
	if err != nil {
		return p.seq, err
	}
	if err := p.journal.Append(seq, dg); err != nil {
		return p.seq, err
	}
	p.Stats.JournalAppend.Add(1)
	var errA, errB error
	if errA = p.feeds[FeedA].Send(ctx, dg); errA != nil {
		p.Stats.FeedASendErr.Add(1)
		p.log.Warn("sbe: feed A send failed", "channel", p.channelID, "seq", seq, "err", errA)
	}
	if errB = p.feeds[FeedB].Send(ctx, dg); errB != nil {
		p.Stats.FeedBSendErr.Add(1)
		p.log.Warn("sbe: feed B send failed", "channel", p.channelID, "seq", seq, "err", errB)
	}
	if errA != nil && errB != nil {
		p.Stats.DualSendErr.Add(1)
		// The datagram is journaled under seq; advancing keeps channel
		// sequence monotonic and lets the replay server fill the hole.
		p.seq = seq
		return seq, errA
	}
	p.seq = seq
	p.Stats.PacketsSent.Add(1)
	return seq, nil
}

// Heartbeat publishes a Heartbeat message, consuming one channel sequence so
// idle periods do not mask feed loss.
func (p *Publisher) Heartbeat(ctx context.Context) (uint64, error) {
	return p.Publish(ctx, Heartbeat{EventTimeNs: p.now()})
}
