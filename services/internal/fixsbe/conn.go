// conn.go — Task 18.3.17: the per-connection serving loop that ties the
// listener set together: transport sniff → Negotiate handshake
// (Ed25519 + SNI + schema) → steady-state frame dispatch through the
// Gateway → drain-aware shutdown.
package fixsbe

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// ConnServer serves one sniffed SBE connection. Constructed by the Mux's
// OnSBE handler (or directly by a dedicated binary listener).
type ConnServer struct {
	Gateway    *Gateway
	Negotiator *Negotiator
	SNI        func(net.Conn) string // SNIBinder.ServerName
	KeyingMat  func(net.Conn) ([]byte, error)
	// ReadDeadline bounds frame starvation; 0 = none (Aeron-bound
	// sessions pace themselves).
	ReadDeadline time.Duration
}

// drainConn is the live-session adapter for Drainer.Source: it carries
// the negotiated response codec and writes outbound frames.
type drainConn struct {
	mu    sync.Mutex
	id    string
	codec ResponseCodec
	conn  net.Conn
}

func (d *drainConn) SessionID() string    { return d.id }
func (d *drainConn) Codec() ResponseCodec { return d.codec }
func (d *drainConn) SendFrame(ctx context.Context, frame []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dl, ok := ctx.Deadline(); ok {
		_ = d.conn.SetWriteDeadline(dl)
	}
	_, err := d.conn.Write(frame)
	return err
}

// Serve runs the connection lifecycle. The first complete frame must be
// Negotiate; anything else rejects and closes (fail closed). On success
// the returned DrainTarget is registered for maintenance broadcasts —
// callers pass it to Drainer.Source.
func (s *ConnServer) Serve(ctx context.Context, conn net.Conn, reg func(DrainTarget)) error {
	defer conn.Close()

	sni := ""
	if s.SNI != nil {
		sni = s.SNI(conn)
	}
	var km []byte
	if s.KeyingMat != nil {
		b, err := s.KeyingMat(conn)
		if err != nil {
			return fmt.Errorf("fixsbe: channel binding unavailable: %w", err)
		}
		km = b
	}

	var buf []byte
	tmp := make([]byte, 32*1024)

	// Phase 1: negotiate. Bounded by ReadDeadline or a hard 10s budget.
	negDeadline := s.ReadDeadline
	if negDeadline == 0 {
		negDeadline = 10 * time.Second
	}
	var sess *SessionInfo
	for sess == nil {
		_ = conn.SetReadDeadline(time.Now().Add(negDeadline))
		n, err := conn.Read(tmp)
		if err != nil {
			return err
		}
		buf = append(buf, tmp[:n]...)
		frame, rest, err := ReadFrame(buf, TransportSBE)
		if err != nil {
			return err
		}
		if frame == nil {
			if len(buf) > 4*1024 {
				return fmt.Errorf("fixsbe: negotiate frame exceeds cap")
			}
			continue
		}
		buf = rest
		m, _, err := DecodeInbound(frame)
		if err != nil {
			return err
		}
		neg, ok := m.(Negotiate)
		if !ok {
			return fmt.Errorf("fixsbe: first frame must be Negotiate, got template %d",
				m.TemplateID())
		}
		bound, resp, herr := s.Negotiator.Handshake(ctx, sni, km, &neg)
		out := MarshalMessage(*resp)
		if herr != nil {
			_, _ = conn.Write(out)
			return herr
		}
		if _, err := conn.Write(out); err != nil {
			return err
		}
		sess = bound
	}

	// Phase 2: steady state — every complete frame dispatches through
	// the gateway; response framing follows the negotiated codec (drain
	// advisories likewise land in that encoding).
	dc := &drainConn{id: sess.ID, codec: sess.Codec, conn: conn}
	if reg != nil {
		reg(dc)
	}
	for {
		if s.ReadDeadline > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(s.ReadDeadline))
		}
		n, err := conn.Read(tmp)
		if err != nil {
			return err
		}
		buf = append(buf, tmp[:n]...)
		for {
			frame, rest, rerr := ReadFrame(buf, TransportSBE)
			if rerr != nil {
				return rerr
			}
			if frame == nil {
				break
			}
			buf = rest
			out, herr := s.Gateway.Handle(ctx, sess, frame)
			if len(out) > 0 {
				if _, werr := conn.Write(out); werr != nil {
					return werr
				}
			}
			if herr != nil {
				return herr // malformed stream — reject frame emitted, close
			}
		}
	}
}
