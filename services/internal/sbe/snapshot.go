// Snapshot / full-state recovery (Task 6.3.6 items 3–4, spec §10.4):
// "snapshot/recovery channels rebuild state when replay is unavailable".
// A Snapshot is the ordered message burst a consumer applies atomically:
// SnapshotMarker{Begin} … state messages … SnapshotMarker{End, LastSeq}.
// Incremental packets buffered on the live feeds while the snapshot is in
// flight are applied afterwards, starting at LastSeq+1.
//
// TCP protocol — request (4 bytes): u16 magic 0x5353 ("SS") || u16 channelID
// Response: u16 magic 0x5352 ("SR") || u8 status || u8 reserved ||
// u64 sessionID || u64 lastSeq || u16 count || count × (u16 len || SBE message).
package sbe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"sync"
	"time"
)

const (
	snapshotReqMagic    uint16 = 0x5353 // "SS"
	snapshotRespMagic   uint16 = 0x5352 // "SR"
	snapshotReqSize            = 4
	snapshotRespHdrSize        = 22
)

// Snapshot status bytes.
const (
	SnapshotOK        byte = 0
	SnapshotNotFound  byte = 1 // unknown channel
	SnapshotBadPacket byte = 2
)

// Snapshot is the full channel state valid through channel sequence LastSeq.
// SessionID is the publisher epoch the snapshot belongs to (assembler adopts
// it so post-recovery packets of the same epoch are not misread as a reset).
// Messages MUST begin with SnapshotMarker{Type: SnapshotBegin} and end with
// SnapshotMarker{Type: SnapshotEnd, LastSeq: LastSeq} — BuildSnapshotSeq
// does this; custom providers must too.
type Snapshot struct {
	ChannelID uint16
	SessionID uint64
	LastSeq   uint64
	Messages  []Message
}

// BuildSnapshotSeq wraps state messages in the begin/end markers.
// instrumentID 0 marks a whole-channel snapshot.
func BuildSnapshotSeq(channelID uint16, sessionID uint64, instrumentID uint32, lastSeq uint64, eventTimeNs int64, state []Message) *Snapshot {
	msgs := make([]Message, 0, len(state)+2)
	msgs = append(msgs, SnapshotMarker{InstrumentID: instrumentID, Type: SnapshotBegin, LastSeq: lastSeq, EventTimeNs: eventTimeNs})
	msgs = append(msgs, state...)
	msgs = append(msgs, SnapshotMarker{InstrumentID: instrumentID, Type: SnapshotEnd, LastSeq: lastSeq, EventTimeNs: eventTimeNs})
	return &Snapshot{ChannelID: channelID, SessionID: sessionID, LastSeq: lastSeq, Messages: msgs}
}

// Validate checks marker discipline.
func (s *Snapshot) Validate() error {
	if len(s.Messages) < 2 {
		return errors.New("sbe: snapshot missing markers")
	}
	beg, ok := s.Messages[0].(SnapshotMarker)
	if !ok || beg.Type != SnapshotBegin {
		return errors.New("sbe: snapshot does not start with SnapshotBegin")
	}
	end, ok := s.Messages[len(s.Messages)-1].(SnapshotMarker)
	if !ok || end.Type != SnapshotEnd || end.LastSeq != s.LastSeq {
		return errors.New("sbe: snapshot does not end with matching SnapshotEnd")
	}
	return nil
}

// SnapshotSource abstracts full-state retrieval. Wave-2 implements this over
// the authoritative book store (in-flight internal/marketdata); tests use
// StaticSnapshotSource; the wire path is TCPSnapshotSource.
type SnapshotSource interface {
	Snapshot(ctx context.Context, channelID uint16) (*Snapshot, error)
}

// StaticSnapshotSource serves a fixed snapshot — test/seed provider.
type StaticSnapshotSource struct {
	Snap *Snapshot
	Err  error
}

// Snapshot implements SnapshotSource.
func (s StaticSnapshotSource) Snapshot(_ context.Context, channelID uint16) (*Snapshot, error) {
	if s.Err != nil {
		return nil, s.Err
	}
	if s.Snap == nil || s.Snap.ChannelID != channelID {
		return nil, errors.New("sbe: no snapshot for channel")
	}
	return s.Snap, nil
}

// JournalSnapshotter derives full channel state by folding the retained
// journal through a BookKeeper — the reference provider used when the
// authoritative book store (Wave-2 internal/marketdata) is not wired yet.
// Because it reads the same journal the replay server serves, snapshot and
// replay are consistent by construction.
type JournalSnapshotter struct {
	J         *Journal
	SessionID uint64
}

// Snapshot implements SnapshotSource.
func (s JournalSnapshotter) Snapshot(_ context.Context, channelID uint16) (*Snapshot, error) {
	if s.J == nil || s.J.Len() == 0 {
		return nil, errors.New("sbe: empty journal, no snapshot")
	}
	dgs, err := s.J.Range(s.J.FirstSeq(), s.J.LastSeq())
	if err != nil {
		return nil, err
	}
	bk := NewBookKeeper()
	now := time.Now().UnixNano()
	for _, dg := range dgs {
		p, err := DecodePacket(dg)
		if err != nil {
			return nil, err
		}
		msgs, err := p.DecodeMessages()
		if err != nil {
			return nil, err
		}
		for _, m := range msgs {
			bk.Apply(Envelope{Seq: p.Seq, Msg: m})
		}
	}
	var state []Message
	ids := make([]uint32, 0, len(bk.Books))
	for id := range bk.Books {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		b := bk.Books[id]
		for _, lv := range b.BidsSorted() {
			state = append(state, BookUpdate{InstrumentID: id, Side: SideBid,
				Action: BookActionUpdate, PriceTicks: lv.PriceTicks, QtyLots: lv.QtyLots, EventTimeNs: now})
		}
		for _, lv := range b.AsksSorted() {
			state = append(state, BookUpdate{InstrumentID: id, Side: SideAsk,
				Action: BookActionUpdate, PriceTicks: lv.PriceTicks, QtyLots: lv.QtyLots, EventTimeNs: now})
		}
		if b.HasStatus {
			state = append(state, SecurityStatus{InstrumentID: id, Status: b.Status, EventTimeNs: now})
		}
	}
	return BuildSnapshotSeq(channelID, s.SessionID, 0, s.J.LastSeq(), now, state), nil
}

// --- TCP server ----------------------------------------------------------

// SnapshotServer answers snapshot requests over TCP.
type SnapshotServer struct {
	Sources map[uint16]SnapshotSource // channelID → provider
	Logger  *slog.Logger

	mu     sync.Mutex
	ln     net.Listener
	closed bool
}

// Addr returns the bound address once serving.
func (s *SnapshotServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// ListenAndServe binds addr and serves until ctx is cancelled.
func (s *SnapshotServer) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sbe: snapshot listen %s: %w", addr, err)
	}
	s.mu.Lock()
	s.ln = ln
	s.mu.Unlock()
	log := s.Logger
	if log == nil {
		log = slog.Default()
	}
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		if !s.closed {
			s.closed = true
			ln.Close()
		}
		s.mu.Unlock()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("sbe: snapshot accept: %w", err)
		}
		go s.serveConn(conn, log)
	}
}

func (s *SnapshotServer) serveConn(conn net.Conn, log *slog.Logger) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	var req [snapshotReqSize]byte
	if _, err := io.ReadFull(conn, req[:]); err != nil {
		return
	}
	if binary.LittleEndian.Uint16(req[0:2]) != snapshotReqMagic {
		return
	}
	channelID := binary.LittleEndian.Uint16(req[2:4])

	status := byte(SnapshotOK)
	var snap *Snapshot
	src, ok := s.Sources[channelID]
	if !ok {
		status = SnapshotNotFound
	} else {
		var err error
		snap, err = src.Snapshot(context.Background(), channelID)
		if err != nil || snap == nil || snap.Validate() != nil {
			status = SnapshotNotFound
			snap = nil
		}
	}
	resp := make([]byte, 0, snapshotRespHdrSize)
	resp = binary.LittleEndian.AppendUint16(resp, snapshotRespMagic)
	resp = append(resp, status, 0)
	var sessionID, lastSeq uint64
	if snap != nil {
		sessionID, lastSeq = snap.SessionID, snap.LastSeq
	}
	resp = binary.LittleEndian.AppendUint64(resp, sessionID)
	resp = binary.LittleEndian.AppendUint64(resp, lastSeq)
	if snap != nil && len(snap.Messages) > 0xFFFF {
		status = SnapshotBadPacket
		snap = nil
	}
	var count uint16
	if snap != nil {
		count = uint16(len(snap.Messages))
	}
	resp = binary.LittleEndian.AppendUint16(resp, count)
	if snap != nil {
		for _, m := range snap.Messages {
			enc := MarshalMessage(m)
			resp = binary.LittleEndian.AppendUint16(resp, uint16(len(enc)))
			resp = append(resp, enc...)
		}
	}
	// status was fixed before message encoding; patch byte 2 for the
	// >0xFFFF guard above.
	resp[2] = status
	if _, err := conn.Write(resp); err != nil {
		log.Warn("sbe: snapshot write failed", "channel", channelID, "err", err)
	}
}

// TCPSnapshotSource queries a SnapshotServer over TCP.
type TCPSnapshotSource struct {
	Addr string
}

// Snapshot implements SnapshotSource.
func (s TCPSnapshotSource) Snapshot(ctx context.Context, channelID uint16) (*Snapshot, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return nil, fmt.Errorf("sbe: snapshot dial %s: %w", s.Addr, err)
	}
	defer conn.Close()
	req := make([]byte, snapshotReqSize)
	binary.LittleEndian.PutUint16(req[0:2], snapshotReqMagic)
	binary.LittleEndian.PutUint16(req[2:4], channelID)
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("sbe: snapshot write: %w", err)
	}
	hdr := make([]byte, snapshotRespHdrSize)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, fmt.Errorf("sbe: snapshot read header: %w", err)
	}
	if binary.LittleEndian.Uint16(hdr[0:2]) != snapshotRespMagic {
		return nil, fmt.Errorf("sbe: snapshot bad response magic")
	}
	if hdr[2] != SnapshotOK {
		return nil, fmt.Errorf("sbe: snapshot unavailable (status %d)", hdr[2])
	}
	sessionID := binary.LittleEndian.Uint64(hdr[4:12])
	lastSeq := binary.LittleEndian.Uint64(hdr[12:20])
	count := int(binary.LittleEndian.Uint16(hdr[20:22]))
	snap := &Snapshot{ChannelID: channelID, SessionID: sessionID, LastSeq: lastSeq}
	for i := 0; i < count; i++ {
		var lb [2]byte
		if _, err := io.ReadFull(conn, lb[:]); err != nil {
			return nil, fmt.Errorf("sbe: snapshot read len: %w", err)
		}
		raw := make([]byte, binary.LittleEndian.Uint16(lb[:]))
		if _, err := io.ReadFull(conn, raw); err != nil {
			return nil, fmt.Errorf("sbe: snapshot read msg: %w", err)
		}
		m, _, err := DecodeMessage(raw)
		if err != nil {
			return nil, fmt.Errorf("sbe: snapshot decode msg %d: %w", i, err)
		}
		snap.Messages = append(snap.Messages, m)
	}
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	return snap, nil
}
