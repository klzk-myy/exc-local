// drain.go — Task 18.3.17 item 3 (spec §24 #289): graceful maintenance
// drain. During a planned shutdown the venue emits News advisories
// repeatedly on every live session until clients disconnect, names the
// ready replacement endpoint, and keeps cancels flowing (the Gateway
// drain gate rejects only new/replace flow).
//
// The advisory goes out on BOTH encodings: SBE sessions get News
// template 103; tag-value sessions get FIX News (35=B) with
// Headline(148), Urgency(61), Text(58).
package fixsbe

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// DrainTarget is one live session the drain broadcast can reach.
type DrainTarget interface {
	// SessionID labels the target for logs.
	SessionID() string
	// Codec returns the session's negotiated response encoding.
	Codec() ResponseCodec
	// SendFrame writes one complete outbound frame.
	SendFrame(ctx context.Context, frame []byte) error
}

// DrainSource returns the current live drain targets — re-sampled each
// broadcast round so disconnecting sessions drop out naturally.
type DrainSource func() []DrainTarget

// Drainer is the broadcast loop. A process constructs one when a
// maintenance window opens, points clients at Replacement, and lets it
// run until the listener empties or the operator forces shutdown.
type Drainer struct {
	Interval    time.Duration // between broadcast rounds (default 2s)
	Replacement string        // ready replacement endpoint advertised
	Reason      string        // e.g. "scheduled maintenance"
	Source      DrainSource
	Now         func() time.Time

	seq atomic.Uint32
}

func (d *Drainer) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// newsID is a per-process monotonic advisory id.
func (d *Drainer) newsID() uint32 { return d.seq.Add(1) }

// Headline is the fixed advisory headline.
func (d *Drainer) Headline() string {
	return "VENUE MAINTENANCE DRAIN"
}

// BodyText is the advisory body: reason + replacement endpoint pointer.
func (d *Drainer) BodyText() string {
	body := d.Reason
	if body == "" {
		body = "scheduled maintenance"
	}
	if d.Replacement != "" {
		body += "; reconnect to " + d.Replacement
	}
	if len(body) > len(News{}.Body) {
		body = body[:len(News{}.Body)]
	}
	return body
}

// EncodeNews renders one advisory frame in the session's codec.
func EncodeNews(codec ResponseCodec, id uint32, headline, body string,
	urgency uint8, now time.Time) []byte {
	if codec == CodecTagValue {
		return encodeTagValueNews(id, headline, body, urgency, now)
	}
	var n News
	copy(n.Headline[:], headline)
	copy(n.Body[:], body)
	n.Urgency = urgency
	n.NewsID = id
	n.EventTimeNs = uint64(now.UnixNano())
	return MarshalMessage(n)
}

// encodeTagValueNews builds a wire-valid 35=B frame:
// 8=FIXT.1.1 | 9=<bodylen> | 35=B | 148=<headline> | 61=<urgency> |
// 58=<body> | 52=<ts> | 10=<checksum>.
func encodeTagValueNews(id uint32, headline, body string, urgency uint8,
	now time.Time) []byte {
	var b strings.Builder
	b.WriteString("35=B\x01")
	fmt.Fprintf(&b, "148=%s\x01", truncate(headline, 64))
	fmt.Fprintf(&b, "61=%d\x01", urgency)
	fmt.Fprintf(&b, "58=%s\x01", truncate(body, 192))
	fmt.Fprintf(&b, "34=%d\x01", id) // advisory sequence doubles as MsgSeqNum
	fmt.Fprintf(&b, "52=%s\x01", now.UTC().Format("20060102-15:04:05.000"))
	payload := b.String()
	frame := fmt.Sprintf("8=FIXT.1.1\x019=%d\x01%s", len(payload), payload)
	return []byte(frame + "10=" + checksum(frame) + "\x01")
}

// checksum is the FIX modulo-256 frame checksum (SOH counted as byte 1).
func checksum(s string) string {
	sum := 0
	for i := 0; i < len(s); i++ {
		sum += int(s[i])
	}
	return fmt.Sprintf("%03d", sum%256)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// Drain broadcasts advisories every Interval until the source empties
// or ctx ends. Each round re-samples live targets so disconnecting
// clients drop out; the round report feeds ops telemetry.
func (d *Drainer) Drain(ctx context.Context) error {
	interval := d.Interval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		targets := d.Source()
		if len(targets) == 0 {
			return nil
		}
		id := d.newsID()
		for _, t := range targets {
			frame := EncodeNews(t.Codec(), id, d.Headline(), d.BodyText(),
				NewsUrgencyFlash, d.now())
			// Best-effort: a dead session errors and is reaped by the
			// listener's own lifecycle — the drain never blocks on one.
			_ = t.SendFrame(ctx, frame)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}
