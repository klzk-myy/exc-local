// aeron.go — Task 18.3.8: the Aeron channel binding for SBE order
// entry (spec §9.5 — "over Aeron").
//
// Aeron carries session identity in the channel/stream pair rather than
// a TLS SNI, so the binding model differs from transport.go: the media
// driver only forwards frames on provisioned (channel, streamId)
// tuples, and the session's SessionInfo is attached at subscription
// time by the caller that completed Ed25519 negotiation out-of-band
// (the TCP listener performs the handshake; Aeron subscriptions are
// opened per-session after it succeeds — Aeron's session-scoped
// publications give each client its own image).
package fixsbe

import (
	"context"
	"fmt"
	"time"

	"exchange/internal/ipc/aeron"
)

// AeronBinding is one session's inbound subscription + response
// publication pair over Aeron.
type AeronBinding struct {
	Gateway *Gateway
	Session *SessionInfo
	Sub     *aeron.Subscription
	Pub     *aeron.Publication // nil → responses are counted but dropped
	// IdleSleep bounds the poll wait when the channel is empty —
	// default 1µs (a busy-spin loop is the production setting; the
	// tiny sleep keeps the test path scheduler-friendly).
	IdleSleep time.Duration
}

// AttachFragment returns the subscription handler for one session:
// decode + dispatch through the gateway, responses offered on pub.
// Register it via aeron.Client.AddSubscription when opening the
// session's inbound channel.
func AttachFragment(g *Gateway, sess *SessionInfo, pub *aeron.Publication) aeron.FragmentHandler {
	return func(buf []byte) {
		out, _ := g.Handle(context.Background(), sess, buf)
		if len(out) > 0 && pub != nil {
			pub.Offer(out)
		}
	}
}

// Run is the poll loop: poll the subscription, dispatch every frame,
// offer responses. Returns when ctx is cancelled or Poll errors.
func (b *AeronBinding) Run(ctx context.Context) error {
	if b.Sub == nil || b.Gateway == nil || b.Session == nil {
		return fmt.Errorf("fixsbe: aeron binding incomplete")
	}
	idle := b.IdleSleep
	if idle == 0 {
		idle = time.Microsecond
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n := b.Sub.Poll(10)
		if n < 0 {
			return fmt.Errorf("fixsbe: aeron poll error")
		}
		if n == 0 {
			if idle > 0 {
				time.Sleep(idle)
			}
			continue
		}
	}
}
