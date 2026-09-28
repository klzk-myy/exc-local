// Transport abstraction behind which the publisher and the consumer-side
// receiver run. Two implementations exist:
//
//   - UDPSender / UDPReceiver: real UDP multicast (deploy-time verified;
//     Feed A 239.255.0.1:10001, Feed B 239.255.0.2:10002 per spec §27.2).
//   - LoopbackBus: deterministic in-memory fan-out used by tests; packet
//     loss and feed asymmetry are injected via FilterSender.
package sbe

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Canonical feed groups (spec §27.2 component catalog).
const (
	DefaultFeedAGroup = "239.255.0.1:10001"
	DefaultFeedBGroup = "239.255.0.2:10002"
)

// Sender publishes one datagram.
type Sender interface {
	Send(ctx context.Context, datagram []byte) error
}

// Receiver consumes datagrams; Receive blocks until ctx is done or a
// datagram arrives, returning ctx.Err() on cancellation.
type Receiver interface {
	Receive(ctx context.Context) ([]byte, error)
}

// --- UDP multicast implementation ---------------------------------------

// UDPSender publishes datagrams to a UDP multicast group ("group:port").
type UDPSender struct {
	conn *net.UDPConn
	addr *net.UDPAddr
}

// NewUDPSender resolves groupAddr (e.g. DefaultFeedAGroup) and opens the
// socket. On hosts without multicast routes this fails at send time — see
// the package doc: real multicast is a deploy-time check.
func NewUDPSender(groupAddr string) (*UDPSender, error) {
	addr, err := net.ResolveUDPAddr("udp4", groupAddr)
	if err != nil {
		return nil, fmt.Errorf("sbe: resolve %s: %w", groupAddr, err)
	}
	if !addr.IP.IsMulticast() {
		return nil, fmt.Errorf("sbe: %s is not a multicast group", groupAddr)
	}
	conn, err := net.DialUDP("udp4", nil, addr)
	if err != nil {
		return nil, fmt.Errorf("sbe: dial %s: %w", groupAddr, err)
	}
	return &UDPSender{conn: conn, addr: addr}, nil
}

func (s *UDPSender) Send(_ context.Context, datagram []byte) error {
	_, err := s.conn.WriteToUDP(datagram, s.addr)
	return err
}

// Close releases the socket.
func (s *UDPSender) Close() error { return s.conn.Close() }

// UDPReceiver joins a multicast group and delivers datagrams.
type UDPReceiver struct {
	conn *net.UDPConn
	buf  []byte
}

// NewUDPReceiver joins groupAddr on iface (nil = system default) bound to
// the group's port on all local addresses.
func NewUDPReceiver(groupAddr string, iface *net.Interface) (*UDPReceiver, error) {
	addr, err := net.ResolveUDPAddr("udp4", groupAddr)
	if err != nil {
		return nil, fmt.Errorf("sbe: resolve %s: %w", groupAddr, err)
	}
	conn, err := net.ListenMulticastUDP("udp4", iface, addr)
	if err != nil {
		return nil, fmt.Errorf("sbe: join %s: %w", groupAddr, err)
	}
	if err := conn.SetReadBuffer(1 << 20); err != nil {
		conn.Close()
		return nil, fmt.Errorf("sbe: SO_RCVBUF: %w", err)
	}
	return &UDPReceiver{conn: conn, buf: make([]byte, 64*1024)}, nil
}

// Receive blocks for the next datagram; ctx cancellation uses a deadline.
func (r *UDPReceiver) Receive(ctx context.Context) ([]byte, error) {
	for {
		if dl, ok := ctx.Deadline(); ok {
			_ = r.conn.SetReadDeadline(dl)
		} else {
			_ = r.conn.SetReadDeadline(time.Now().Add(time.Hour))
		}
		n, _, err := r.conn.ReadFromUDP(r.buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return nil, err
		}
		out := make([]byte, n)
		copy(out, r.buf[:n])
		return out, nil
	}
}

// Close leaves the group and releases the socket.
func (r *UDPReceiver) Close() error { return r.conn.Close() }

// --- Loopback / in-memory implementation ---------------------------------

// LoopbackBus is a deterministic in-memory multicast analogue: a Sender
// copies each datagram to every subscriber's channel. Tests attach a
// Receiver per simulated consumer and wrap senders in FilterSender for
// loss injection.
type LoopbackBus struct {
	mu   sync.Mutex
	subs []chan []byte
}

// NewLoopbackBus returns an empty bus.
func NewLoopbackBus() *LoopbackBus { return &LoopbackBus{} }

// Sender returns the bus write-end; each Send delivers a private copy to
// every subscriber (drop-on-full is impossible: Subscribe depth is chosen
// by the consumer; a full subscriber blocks the sender, matching UDP slow-
// consumer backpressure semantics for test purposes).
func (b *LoopbackBus) Sender() Sender { return busSender{bus: b} }

// Subscribe registers a new receiver with a channel of the given depth.
func (b *LoopbackBus) Subscribe(depth int) *LoopbackReceiver {
	ch := make(chan []byte, depth)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return &LoopbackReceiver{ch: ch}
}

type busSender struct{ bus *LoopbackBus }

func (s busSender) Send(ctx context.Context, datagram []byte) error {
	s.bus.mu.Lock()
	subs := make([]chan []byte, len(s.bus.subs))
	copy(subs, s.bus.subs)
	s.bus.mu.Unlock()
	for _, ch := range subs {
		cp := make([]byte, len(datagram))
		copy(cp, datagram)
		select {
		case ch <- cp:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// LoopbackReceiver is the read-end handed out by LoopbackBus.Subscribe.
type LoopbackReceiver struct {
	ch <-chan []byte
}

// Receive returns the next datagram or ctx.Err().
func (r *LoopbackReceiver) Receive(ctx context.Context) ([]byte, error) {
	select {
	case dg := <-r.ch:
		return dg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Ch exposes the underlying channel for deterministic test pumps.
func (r *LoopbackReceiver) Ch() <-chan []byte { return r.ch }

// FilterSender wraps inner and drops datagrams for which drop returns true.
// The filter receives the decoded channel sequence (0 when the datagram is
// not a parseable packet) so tests can drop precise sequences per feed.
func FilterSender(inner Sender, drop func(seq uint64, datagram []byte) bool) Sender {
	return filterSender{inner: inner, drop: drop}
}

type filterSender struct {
	inner Sender
	drop  func(seq uint64, dg []byte) bool
}

func (f filterSender) Send(ctx context.Context, dg []byte) error {
	seq, _ := PeekPacketSeq(dg)
	if f.drop != nil && f.drop(seq, dg) {
		return nil // silently discarded, like UDP loss
	}
	return f.inner.Send(ctx, dg)
}

// MutateSender wraps inner and rewrites datagrams — used to inject feed
// byte-divergence (SBE_FEED_A_DESYNC) in tests.
func MutateSender(inner Sender, mutate func(seq uint64, dg []byte) []byte) Sender {
	return mutateSender{inner: inner, mutate: mutate}
}

type mutateSender struct {
	inner  Sender
	mutate func(seq uint64, dg []byte) []byte
}

func (m mutateSender) Send(ctx context.Context, dg []byte) error {
	seq, _ := PeekPacketSeq(dg)
	if m.mutate != nil {
		dg = m.mutate(seq, dg)
	}
	return m.inner.Send(ctx, dg)
}
