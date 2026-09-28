// Task 9.3.23 item 4 — bridge flush on shutdown: the in-memory event
// buffer drains to JetStream before the process exits so a rolling
// update loses nothing the WAL/bridge already acknowledged.
//
// Flush preserves the Run() ordering contract (FIFO, per-event subject
// fan-out, head-of-line retry). It is a bounded best-effort drain: ctx
// carries the service's shutdown budget; on expiry the residual buffer
// is reported, not silently discarded — the counters + caller's log are
// the audit trail for what the WAL replay/archive must cover.
package bridge

import (
	"context"
	"time"
)

// Flush publishes buffered events in order until the buffer empties or
// ctx expires. Returns nil on a fully drained buffer; the context error
// (with the residual depth logged) otherwise.
func (b *Bridge) Flush(ctx context.Context) error {
	for {
		e, gen, ok := b.buf.Head()
		if !ok {
			b.m.setBufferDepth(0)
			return nil
		}
		for subjIdx := 0; subjIdx < len(e.subjects); subjIdx++ {
			subj := e.subjects[subjIdx]
			pctx, cancel := context.WithTimeout(ctx, b.cfg.PublishTimeout)
			err := b.pub.PublishEvent(pctx, subj, e.msgID, e.payload)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					b.m.setBufferDepth(int64(b.buf.Len()))
					b.log.Warn("bridge: flush stopped — buffer residual",
						"depth", b.buf.Len(), "err", ctx.Err())
					return ctx.Err()
				}
				b.m.incPublishErr()
				// Bounded retry — same cadence as Run's reconnect wait.
				select {
				case <-ctx.Done():
					b.m.setBufferDepth(int64(b.buf.Len()))
					return ctx.Err()
				case <-time.After(b.cfg.ReconnectWait):
				}
				if b.buf.HeadGen() != gen {
					break // head evicted mid-fanout — restart on new head
				}
				subjIdx-- // retry this subject
				continue
			}
			b.m.incPublished()
		}
		b.buf.PopIfGen(gen)
		b.m.setBufferDepth(int64(b.buf.Len()))
	}
}
