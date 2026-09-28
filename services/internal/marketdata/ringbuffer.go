// Task 6.3.9 — bounded per-channel replay buffer.
//
// Each channel keeps a ring of the last N published frames within an age
// horizon: capacity 10,000 messages OR a 60-second window, whichever is
// smaller (spec §10.7 / Task 6.3.9 item 2). Entries are the marshaled
// event frames, replayed verbatim so resumed clients observe the original
// seq/ts_ms — the replay is indistinguishable from having stayed live.
package marketdata

import (
	"container/list"
	"sync"
	"time"
)

// ReplayVerdict classifies a resume request against the buffer horizon.
type ReplayVerdict int

const (
	// ReplayOK — the cursor is inside the buffered range; Msgs carries the
	// frames strictly after the cursor, in order.
	ReplayOK ReplayVerdict = iota
	// ReplayUpToDate — the cursor equals the buffer tail; nothing to replay.
	ReplayUpToDate
	// ReplayGapTooLarge — the cursor predates the oldest buffered seq
	// (eviction horizon exceeded); client must resync from a snapshot.
	ReplayGapTooLarge
	// ReplayInvalidSeq — the cursor is ahead of the buffer tail (seq
	// regression across restart or client confusion).
	ReplayInvalidSeq
	// ReplayEmpty — the buffer holds no frames yet.
	ReplayEmpty
)

// ReplayResult is the resume outcome: the verdict plus the buffered
// frames strictly after the client's cursor.
type ReplayResult struct {
	Verdict ReplayVerdict
	Msgs    [][]byte // ordered frames to replay (verbatim wire bytes)
	Tail    uint64   // current buffer tail seq
	Oldest  uint64   // oldest buffered seq (0 when empty)
}

// ringEntry is one buffered frame.
type ringEntry struct {
	seq   uint64
	frame []byte
	at    time.Time
}

// ringBuffer is a bounded, age-trimmed frame log. Writes and replays are
// serialized per channel via the hub's channel mutex, so a resume replay
// is always a consistent cut — frames published mid-replay appear after
// the replayed range with a strictly larger seq.
type ringBuffer struct {
	mu      sync.Mutex
	l       *list.List // front = oldest
	bySeq   map[uint64]*list.Element
	maxMsgs int
	maxAge  time.Duration
	now     func() time.Time
}

func newRingBuffer(maxMsgs int, maxAge time.Duration, now func() time.Time) *ringBuffer {
	if maxMsgs < 1 {
		maxMsgs = 1
	}
	return &ringBuffer{
		l:       list.New(),
		bySeq:   make(map[uint64]*list.Element, 1024),
		maxMsgs: maxMsgs,
		maxAge:  maxAge,
		now:     now,
	}
}

// append stores a frame under its channel seq. Out-of-order appends are
// rejected — the seq domain is strictly monotonic per channel.
func (r *ringBuffer) append(seq uint64, frame []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if back := r.l.Back(); back != nil && seq <= back.Value.(ringEntry).seq {
		return // non-monotonic append — seq domain violation, drop loudly?
		// (dropped frames surface as a client-visible gap; the caller logs)
	}
	r.l.PushBack(ringEntry{seq: seq, frame: frame, at: r.now()})
	r.bySeq[seq] = r.l.Back()
	r.trimLocked()
}

// trimLocked evicts entries beyond the count cap and the age horizon.
// "60 seconds OR 10,000 messages, whichever is smaller" — both bounds
// apply; whichever bites first wins.
func (r *ringBuffer) trimLocked() {
	for r.l.Len() > r.maxMsgs {
		r.evictFrontLocked()
	}
	cutoff := r.now().Add(-r.maxAge)
	for {
		front := r.l.Front()
		if front == nil {
			return
		}
		e := front.Value.(ringEntry)
		if !e.at.Before(cutoff) {
			return
		}
		r.evictFrontLocked()
	}
}

func (r *ringBuffer) evictFrontLocked() {
	front := r.l.Front()
	if front == nil {
		return
	}
	delete(r.bySeq, front.Value.(ringEntry).seq)
	r.l.Remove(front)
}

// replay answers the resume contract for a cursor. The result is a
// consistent snapshot of the buffer state.
func (r *ringBuffer) replay(lastSeq uint64) ReplayResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trimLocked()

	tail := uint64(0)
	if back := r.l.Back(); back != nil {
		tail = back.Value.(ringEntry).seq
	}
	oldest := uint64(0)
	if front := r.l.Front(); front != nil {
		oldest = front.Value.(ringEntry).seq
	}

	res := ReplayResult{Tail: tail, Oldest: oldest}
	switch {
	case r.l.Len() == 0:
		res.Verdict = ReplayEmpty
	case lastSeq > tail:
		res.Verdict = ReplayInvalidSeq
	case lastSeq == tail:
		res.Verdict = ReplayUpToDate
	case lastSeq+1 < oldest:
		res.Verdict = ReplayGapTooLarge
	default:
		res.Verdict = ReplayOK
		// Collect every frame with seq > lastSeq. Walk from the element
		// after the cursor when it is still buffered, else from front —
		// bySeq gives O(1) positioning for the common case.
		start := r.l.Front()
		if el, ok := r.bySeq[lastSeq]; ok {
			start = el.Next()
		}
		for e := start; e != nil; e = e.Next() {
			ent := e.Value.(ringEntry)
			if ent.seq > lastSeq {
				res.Msgs = append(res.Msgs, ent.frame)
			}
		}
	}
	return res
}

// expired trims aged-out entries and reports whether the ring is now
// empty — the per-account private-ring GC hook (a disconnected account's
// ring survives for the age horizon, then self-cleans on next touch).
func (r *ringBuffer) expired() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trimLocked()
	return r.l.Len() == 0
}

// tailSeq returns the newest buffered seq (0 = empty) — cheap status for
// resume handling without materializing frames.
func (r *ringBuffer) tailSeq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if back := r.l.Back(); back != nil {
		return back.Value.(ringEntry).seq
	}
	return 0
}

// len reports the buffered count (metrics/tests).
func (r *ringBuffer) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.l.Len()
}
