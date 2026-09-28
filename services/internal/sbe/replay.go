// TCP replay service (Task 6.3.6 item 3, spec §10.4): "a TCP replay service
// recovers bounded gaps". One request per connection keeps the protocol
// trivially auditable; retained-log range requests are answered verbatim so
// replayed streams reproduce identical sequence ordering.
//
// Request  (20 bytes, little-endian):
//
//	u16 magic = 0x5251 ("RQ")
//	u16 channelID
//	u64 fromSeq   — first sequence wanted (inclusive)
//	u64 toSeq     — last sequence wanted (inclusive)
//
// Response:
//
//	u16 magic = 0x5250 ("RP")
//	u8  status: 0=ReplayOK, 1=ReplayGapExceeded, 2=ReplayBadRequest
//	u8  reserved
//	u16 count
//	count × datagram: u16 len || raw packet bytes
package sbe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	replayReqMagic    uint16 = 0x5251 // "RQ"
	replayRespMagic   uint16 = 0x5250 // "RP"
	replayReqSize            = 20
	replayRespHdrSize        = 6
	// MaxReplayPackets caps one replay request's packet count (bounded
	// retention policy — larger gaps must use snapshot recovery).
	MaxReplayPackets = 10000
)

// Replay response status bytes.
const (
	ReplayOK          byte = 0
	ReplayGapExceeded byte = 1 // out of retained range → snapshot required
	ReplayBadRequest  byte = 2
)

// ReplaySource abstracts gap-fill retrieval. Implementations: JournalSource
// (in-process, tests / same-binary consumer) and TCPReplaySource (wire).
type ReplaySource interface {
	// Replay returns the raw datagrams for channel seqs [fromSeq, toSeq]
	// inclusive in ascending order, or ErrReplayGapExceeded when the range
	// precedes the retention horizon.
	Replay(ctx context.Context, channelID uint16, fromSeq, toSeq uint64) ([][]byte, error)
}

// JournalSource serves replay directly from a Journal.
type JournalSource struct {
	J        *Journal
	MaxBatch int // 0 → MaxReplayPackets
}

// Replay implements ReplaySource.
func (s JournalSource) Replay(_ context.Context, channelID uint16, fromSeq, toSeq uint64) ([][]byte, error) {
	if s.J == nil {
		return nil, ErrReplayGapExceeded
	}
	limit := s.MaxBatch
	if limit <= 0 {
		limit = MaxReplayPackets
	}
	if toSeq-fromSeq+1 > uint64(limit) {
		return nil, ErrReplayGapExceeded // outside bounded-retention policy
	}
	dgs, err := s.J.Range(fromSeq, toSeq)
	if err != nil {
		return nil, err
	}
	for _, dg := range dgs {
		p, err := DecodePacket(dg)
		if err != nil {
			return nil, fmt.Errorf("sbe: corrupt journal entry: %w", err)
		}
		if p.ChannelID != channelID {
			return nil, fmt.Errorf("sbe: journal channel mismatch")
		}
	}
	return dgs, nil
}

// --- TCP server ----------------------------------------------------------

// ReplayServer answers replay requests over TCP for a set of per-channel
// journals.
type ReplayServer struct {
	Sources  map[uint16]*Journal // channelID → retained log
	MaxBatch int                 // per-request packet cap; 0 → MaxReplayPackets
	Logger   *slog.Logger        // nil → slog.Default()

	mu     sync.Mutex
	ln     net.Listener
	closed bool
}

// Addr returns the bound address once ListenAndServe has started.
func (s *ReplayServer) Addr() net.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ln == nil {
		return nil
	}
	return s.ln.Addr()
}

// ListenAndServe binds addr ("127.0.0.1:0" for ephemeral) and serves until
// ctx is cancelled. Returns the bound address for tests via Addr().
func (s *ReplayServer) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sbe: replay listen %s: %w", addr, err)
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
			return fmt.Errorf("sbe: replay accept: %w", err)
		}
		go s.serveConn(conn, log)
	}
}

func (s *ReplayServer) serveConn(conn net.Conn, log *slog.Logger) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	var req [replayReqSize]byte
	if _, err := io.ReadFull(conn, req[:]); err != nil {
		return
	}
	if binary.LittleEndian.Uint16(req[0:2]) != replayReqMagic {
		return
	}
	channelID := binary.LittleEndian.Uint16(req[2:4])
	fromSeq := binary.LittleEndian.Uint64(req[4:12])
	toSeq := binary.LittleEndian.Uint64(req[12:20])

	status := byte(ReplayOK)
	var dgs [][]byte
	src := JournalSource{J: s.Sources[channelID], MaxBatch: s.MaxBatch}
	switch {
	case fromSeq == 0 || fromSeq > toSeq:
		status = ReplayBadRequest
	default:
		var err error
		dgs, err = src.Replay(context.Background(), channelID, fromSeq, toSeq)
		switch {
		case errors.Is(err, ErrReplayGapExceeded):
			status = ReplayGapExceeded
		case err != nil:
			status = ReplayBadRequest
		}
	}
	resp := make([]byte, 0, replayRespHdrSize)
	resp = binary.LittleEndian.AppendUint16(resp, replayRespMagic)
	resp = append(resp, status, 0)
	resp = binary.LittleEndian.AppendUint16(resp, uint16(len(dgs)))
	for _, dg := range dgs {
		resp = binary.LittleEndian.AppendUint16(resp, uint16(len(dg)))
		resp = append(resp, dg...)
	}
	if _, err := conn.Write(resp); err != nil {
		log.Warn("sbe: replay write failed", "channel", channelID, "err", err)
	}
}

// TCPReplaySource queries a ReplayServer over TCP (one conn per request).
type TCPReplaySource struct {
	Addr string // host:port of a ReplayServer
}

// Replay implements ReplaySource.
func (s TCPReplaySource) Replay(ctx context.Context, channelID uint16, fromSeq, toSeq uint64) ([][]byte, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", s.Addr)
	if err != nil {
		return nil, fmt.Errorf("sbe: replay dial %s: %w", s.Addr, err)
	}
	defer conn.Close()
	req := make([]byte, replayReqSize)
	binary.LittleEndian.PutUint16(req[0:2], replayReqMagic)
	binary.LittleEndian.PutUint16(req[2:4], channelID)
	binary.LittleEndian.PutUint64(req[4:12], fromSeq)
	binary.LittleEndian.PutUint64(req[12:20], toSeq)
	if _, err := conn.Write(req); err != nil {
		return nil, fmt.Errorf("sbe: replay write: %w", err)
	}
	hdr := make([]byte, replayRespHdrSize)
	if _, err := io.ReadFull(conn, hdr); err != nil {
		return nil, fmt.Errorf("sbe: replay read header: %w", err)
	}
	if binary.LittleEndian.Uint16(hdr[0:2]) != replayRespMagic {
		return nil, fmt.Errorf("sbe: replay bad response magic")
	}
	switch hdr[2] {
	case ReplayGapExceeded:
		return nil, ErrReplayGapExceeded
	case ReplayBadRequest:
		return nil, fmt.Errorf("sbe: replay request rejected")
	case ReplayOK:
	default:
		return nil, fmt.Errorf("sbe: replay unknown status %d", hdr[2])
	}
	count := int(binary.LittleEndian.Uint16(hdr[4:6]))
	out := make([][]byte, 0, count)
	for i := 0; i < count; i++ {
		var lb [2]byte
		if _, err := io.ReadFull(conn, lb[:]); err != nil {
			return nil, fmt.Errorf("sbe: replay read len: %w", err)
		}
		dg := make([]byte, binary.LittleEndian.Uint16(lb[:]))
		if _, err := io.ReadFull(conn, dg); err != nil {
			return nil, fmt.Errorf("sbe: replay read packet: %w", err)
		}
		out = append(out, dg)
	}
	return out, nil
}
