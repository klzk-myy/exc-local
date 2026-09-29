// connector.go — the VenueConnector contract implementations.
//
// LoopbackConnector is the dev/test adapter: it exercises the full
// initiator path — outbound orders are encoded as real 35=D frames
// (parseTagValue re-decodes them, catching encoding drift) and venue
// responses flow as 35=8 frames decoded by parseExecReport. Its fill
// behavior is scriptable: ack delay, silent venue (timeout path),
// reject, partial-fill plans, cancel confirmation.
//
// Production connectors are FIX initiator sessions to ECNs/bank LPs;
// wiring is env-blocked (SOR_EXTERNAL_VENUES unset → no venues, router
// only ever returns the local decision).
package sor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"exchange/pkg/decimal"
)

// FillPlan is one scripted venue fill.
type FillPlan struct {
	Qty   decimal.Decimal
	Price decimal.Decimal
	Done  bool // mark terminal (leaves = 0)
}

// LoopbackConnector is the scriptable test/dev VenueConnector. Submit
// encodes a real FIX 35=D frame, and scheduled venue responses are
// emitted as real 35=8 frames that parseExecReport decodes — so tests
// exercise actual initiator wire framing end to end.
type LoopbackConnector struct {
	venueID  string
	SenderID string // this exchange's CompID on the initiator session
	TargetID string // venue CompID

	// Script knobs (read at Submit time):
	AckDelay   time.Duration // delay before the first report (ack/fill)
	Silent     bool          // never emit any report → timeout path
	RejectWith string        // non-empty → emit ExecType=8 reject
	FillScript []FillPlan    // fills emitted after the ack
	DelayFills time.Duration // gap between script entries

	events chan VenueEvent
	seq    atomic.Int64

	mu     sync.Mutex
	orders map[string]*VenueOrder // extID → submitted order
	extSeq atomic.Int64
	closed atomic.Bool
}

// NewLoopbackConnector builds a named loopback venue.
func NewLoopbackConnector(venueID, senderComp, targetComp string) *LoopbackConnector {
	return &LoopbackConnector{
		venueID:  venueID,
		SenderID: senderComp,
		TargetID: targetComp,
		events:   make(chan VenueEvent, 256),
		orders:   map[string]*VenueOrder{},
	}
}

func (l *LoopbackConnector) VenueID() string { return l.venueID }

// Submit encodes the order onto the wire, decodes it back (initiator
// path proof), registers the order, then schedules the scripted reports.
func (l *LoopbackConnector) Submit(ctx context.Context, o *VenueOrder) (string, error) {
	if l.closed.Load() {
		return "", fmt.Errorf("sor: loopback venue %s closed", l.venueID)
	}
	seq := l.seq.Add(1)
	frame := encodeNewOrder(o, l.SenderID, l.TargetID, seq)
	fs := parseTagValue(frame)
	if tvGet(fs, "35") != "D" || tvGet(fs, "11") != o.ClOrdID {
		return "", fmt.Errorf("sor: initiator frame round-trip failed")
	}
	extID := fmt.Sprintf("%s-E%06d", l.venueID, l.extSeq.Add(1))
	l.mu.Lock()
	l.orders[extID] = o
	l.mu.Unlock()

	go l.schedule(ctx, extID, o)
	return extID, nil
}

// Cancel emits a cancel-confirm report for a live order.
func (l *LoopbackConnector) Cancel(ctx context.Context, externalOrderID string) error {
	l.mu.Lock()
	_, ok := l.orders[externalOrderID]
	l.mu.Unlock()
	if !ok {
		return fmt.Errorf("sor: venue %s unknown order %s", l.venueID, externalOrderID)
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Millisecond):
		}
		l.emit(execReportFrame(l.venueID, externalOrderID, "cxl-"+externalOrderID,
			"4", "4", decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, ""))
	}()
	return nil
}

// Events streams decoded venue reports.
func (l *LoopbackConnector) Events() <-chan VenueEvent { return l.events }

// Close terminates the venue.
func (l *LoopbackConnector) Close() {
	if l.closed.CompareAndSwap(false, true) {
		close(l.events)
	}
}

// SubmittedFrameCount is a test hook — how many orders hit the wire.
func (l *LoopbackConnector) SubmittedCount() int64 { return l.seq.Load() }

func (l *LoopbackConnector) schedule(ctx context.Context, extID string, o *VenueOrder) {
	if l.Silent {
		return
	}
	delay := l.AckDelay
	select {
	case <-ctx.Done():
		return
	case <-time.After(delay):
	}
	if l.RejectWith != "" {
		l.emit(execReportFrame(l.venueID, extID, "rej-"+extID, "8", "8",
			decimal.Zero, decimal.Zero, decimal.Zero, decimal.Zero, l.RejectWith))
		return
	}
	// Ack (ExecType=0, OrdStatus=0 New).
	l.emit(execReportFrame(l.venueID, extID, "ack-"+extID, "0", "0",
		decimal.Zero, decimal.Zero, decimal.Zero, o.Qty, ""))
	var cum decimal.Decimal
	for i, f := range l.FillScript {
		if l.DelayFills > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(l.DelayFills):
			}
		}
		cum = cum.Add(f.Qty)
		leaves := o.Qty.Sub(cum)
		if leaves.IsNegative() {
			leaves = decimal.Zero
		}
		done := f.Done || leaves.IsZero()
		execType := "1"
		ordStatus := "1"
		if done {
			execType, ordStatus = "2", "2"
			leaves = decimal.Zero
		}
		l.emit(execReportFrame(l.venueID, extID,
			fmt.Sprintf("exec-%s-%d", extID, i), execType, ordStatus,
			f.Qty, f.Price, cum, leaves, ""))
		if done {
			return
		}
	}
}

// emit encodes the frame then decodes it into the event channel — a
// venue-side encoding defect surfaces as a dropped event (log-worthy),
// matching real connector failure semantics.
func (l *LoopbackConnector) emit(frame []byte) {
	ev, err := parseExecReport(l.venueID, frame, time.Now())
	if err != nil || l.closed.Load() {
		return
	}
	l.events <- *ev
}

// execReportFrame builds the venue-side 35=8 frame.
func execReportFrame(venueID, extID, execID, execType, ordStatus string,
	lastQty, lastPx, cumQty, leavesQty decimal.Decimal, text string) []byte {
	fields := [][2]string{
		{"35", "8"},
		{"49", venueID},
		{"56", "EXCHANGE"},
		{"34", "1"},
		{"52", fixNow(time.Now())},
		{"37", extID},
		{"17", execID},
		{"150", execType},
		{"39", ordStatus},
		{"14", cumQty.String()},
		{"151", leavesQty.String()},
	}
	if lastQty.IsPositive() {
		fields = append(fields, [2]string{"32", lastQty.String()})
	}
	if lastPx.IsPositive() {
		fields = append(fields, [2]string{"31", lastPx.String()})
	}
	if text != "" {
		fields = append(fields, [2]string{"58", text})
	}
	return buildTagValue(fields)
}

// LoadVenueConfig parses SOR_EXTERNAL_VENUES ("id:host:port,id:host:port")
// — production wiring seam. Empty env → no venues → router only ever
// returns local decisions (env-blocked by design; real connectors are
// not yet shipped).
func LoadVenueConfig(env string) ([]VenueSpec, error) {
	if env == "" {
		return nil, nil
	}
	var out []VenueSpec
	for _, tok := range strings.Split(env, ",") {
		parts := strings.SplitN(strings.TrimSpace(tok), ":", 3)
		if len(parts) != 3 || parts[0] == "" {
			return nil, fmt.Errorf("sor: bad venue spec %q", tok)
		}
		port, err := strconv.Atoi(parts[2])
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("sor: bad venue port in %q", tok)
		}
		out = append(out, VenueSpec{ID: parts[0], Host: parts[1], Port: port})
	}
	return out, nil
}

// VenueSpec is one external venue endpoint from configuration.
type VenueSpec struct {
	ID   string
	Host string
	Port int
}
