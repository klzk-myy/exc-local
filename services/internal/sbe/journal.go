// Retained journal: the bounded circular packet buffer (spec §27.2 row
// "Circular packet buffers") backing TCP replay. The publisher appends every
// emitted datagram; the replay server answers range requests strictly from
// this log, so a replayed stream reproduces identical bytes and sequence
// ordering (deterministic replay requirement, Task 6.3.6).
package sbe

import (
	"errors"
	"fmt"
)

// ErrReplayGapExceeded is returned when the requested range starts before
// the journal's retained horizon — the §27.2 SBE_REPLAY_GAP_EXCEEDED (L3)
// condition. Consumers fall back to snapshot recovery on this error.
var ErrReplayGapExceeded = errors.New("sbe: replay range exceeds retained journal horizon")

// Journal is a bounded retention log of encoded packets keyed by channel
// sequence. It is NOT goroutine-safe: the publisher owns it (single writer)
// and replay reads are serialized through the publisher's path — guard with
// a mutex if embedding in a concurrent service.
type Journal struct {
	max   int
	bySeq map[uint64][]byte
	order []uint64 // ring of retained seqs, oldest first
	head  int      // index of oldest live entry in order
	count int
	hi    uint64 // highest appended seq
	hasHi bool
}

// NewJournal retains up to maxPackets of the most recent sequences.
func NewJournal(maxPackets int) *Journal {
	if maxPackets < 1 {
		maxPackets = 1
	}
	return &Journal{
		max:   maxPackets,
		bySeq: make(map[uint64][]byte, maxPackets),
		order: make([]uint64, maxPackets),
	}
}

// Append records datagram under its channel sequence. Sequences must be
// appended in monotonically increasing order — the journal enforces this so
// a replayed stream provably reproduces publish ordering.
func (j *Journal) Append(seq uint64, datagram []byte) error {
	if j.hasHi && seq <= j.hi {
		return fmt.Errorf("sbe: journal append out of order (seq %d <= last %d)", seq, j.hi)
	}
	cp := make([]byte, len(datagram))
	copy(cp, datagram)
	if j.count == j.max {
		old := j.order[j.head]
		delete(j.bySeq, old)
		j.head = (j.head + 1) % j.max
		j.count--
	}
	j.order[(j.head+j.count)%j.max] = seq
	j.bySeq[seq] = cp
	j.count++
	j.hi = seq
	j.hasHi = true
	return nil
}

// FirstSeq returns the lowest retained sequence (0 when empty).
func (j *Journal) FirstSeq() uint64 {
	if j.count == 0 {
		return 0
	}
	return j.order[j.head]
}

// LastSeq returns the highest appended sequence (0 when empty).
func (j *Journal) LastSeq() uint64 {
	if !j.hasHi {
		return 0
	}
	return j.hi
}

// Len reports retained packet count.
func (j *Journal) Len() int { return j.count }

// Range returns the encoded datagrams for [fromSeq, toSeq] inclusive, in
// sequence order. Any uncovered sequence inside the range — including
// ranges starting below the retention horizon — returns
// ErrReplayGapExceeded so callers deterministically escalate to snapshot
// recovery rather than emitting a silently incomplete stream.
func (j *Journal) Range(fromSeq, toSeq uint64) ([][]byte, error) {
	if fromSeq > toSeq {
		return nil, fmt.Errorf("sbe: invalid replay range %d..%d", fromSeq, toSeq)
	}
	if j.count == 0 || fromSeq < j.FirstSeq() {
		return nil, ErrReplayGapExceeded
	}
	if toSeq > j.hi {
		toSeq = j.hi
	}
	out := make([][]byte, 0, toSeq-fromSeq+1)
	for s := fromSeq; s <= toSeq; s++ {
		dg, ok := j.bySeq[s]
		if !ok {
			// Hole inside the retained window: fail closed to snapshot.
			return nil, ErrReplayGapExceeded
		}
		out = append(out, dg)
	}
	return out, nil
}
