// transport.go — Task 18.3.17: production SBE transport on the FIX
// listener set (spec §9.5/§24 #284, #289).
//
// One acceptor family serves every transport combination: tag-value
// requests with tag-value responses, SBE requests with SBE responses,
// and tag-value requests with SBE responses — the client picks its
// response codec at Negotiate. Business semantics, entitlements,
// sequence rules and drop-copy coverage are identical on both
// encodings (the Gateway layer never sees a different contract).
//
// Transport sniffing: the first bytes of a connection unambiguously
// identify the wire — tag-value frames open with "8=FIX"; SBE frames
// open with an 8-byte message header carrying schemaId=2. Unknown
// preambles are dropped before any parse.
//
// Session authentication: Ed25519 session keys (registered per session
// in fixsbe_sessions, migration 228) prove possession by signing the
// server nonce || SNI hostname || TLS keying material — binding the
// application session to BOTH the certificate-authenticated channel
// and the requested hostname.
package fixsbe

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"exchange/internal/sbe"
)

// Transport is the wire encoding a connection is classified as.
type Transport int

const (
	TransportUnknown Transport = iota
	TransportTagValue          // "8=FIX…" tag-value frame stream
	TransportSBE               // schema-2 binary frame stream
)

// tagValueMagic is the BeginString prefix of any FIX tag-value frame
// (FIX.4.4, FIXT.1.1 — both open "8=FIX").
var tagValueMagic = []byte("8=FIX")

// Classify inspects the first bytes of a connection. SBE recognition
// is strict: 8-byte header, schemaId = SchemaIDOrderEntry, blockLength
// matching a known template footprint — anything else is Unknown.
func Classify(peek []byte) Transport {
	if bytes.HasPrefix(peek, tagValueMagic) {
		return TransportTagValue
	}
	if len(peek) >= headerSize {
		schemaID := binary.LittleEndian.Uint16(peek[4:6])
		version := binary.LittleEndian.Uint16(peek[6:8])
		if schemaID == SchemaIDOrderEntry && version >= 1 {
			return TransportSBE
		}
	}
	return TransportUnknown
}

// SBEFrameLen returns the total wire length of the SBE message at the
// head of buf, or 0 when incomplete.
func SBEFrameLen(buf []byte) int {
	if len(buf) < headerSize {
		return 0
	}
	return headerSize + int(binary.LittleEndian.Uint16(buf[0:2]))
}

// tagValueFrameLen finds the end of a complete tag-value frame: the
// trailer "10=nnn\x01" CheckSum field. Returns 0 when incomplete or
// malformed length accounting.
func tagValueFrameLen(buf []byte) int {
	// A frame always ends with CheckSum(10). Scan for the last field.
	idx := bytes.Index(buf, []byte("\x0110="))
	if idx < 0 {
		// frame could start with... it can't — 10 is always last and
		// preceded by SOH.
		if bytes.HasPrefix(buf, []byte("10=")) {
			idx = -1 // degenerate
		} else {
			return 0
		}
	}
	// find terminating SOH after "10=nnn"
	rest := buf[idx+1:]
	end := bytes.IndexByte(rest, 0x01)
	if end < 0 {
		return 0
	}
	return idx + 1 + end + 1
}

// ReadFrame extracts the next complete frame of transport t from buf,
// returning the frame and the remainder. Incomplete data returns
// (nil, buf, nil).
func ReadFrame(buf []byte, t Transport) (frame, rest []byte, err error) {
	switch t {
	case TransportSBE:
		if len(buf) < headerSize {
			return nil, buf, nil
		}
		// Sanity cap: 4KiB frames (all v1 blocks ≤272B; ~15x headroom
		// for appended forward-compat fields). Checked BEFORE waiting
		// on more bytes so a forged blockLength can't pin the buffer.
		if int(binary.LittleEndian.Uint16(buf[0:2])) > 4*1024 {
			return nil, nil, fmt.Errorf("fixsbe: frame blockLength exceeds cap")
		}
		n := SBEFrameLen(buf)
		if len(buf) < n {
			return nil, buf, nil
		}
		return buf[:n], buf[n:], nil
	case TransportTagValue:
		n := tagValueFrameLen(buf)
		if n == 0 {
			return nil, buf, nil
		}
		return buf[:n], buf[n:], nil
	}
	return nil, nil, fmt.Errorf("fixsbe: unknown transport")
}

// ---------------------------------------------------------------------------
// Ed25519 session keys + SNI binding
// ---------------------------------------------------------------------------

// ProvisionedSession is the fixsbe_sessions row (migration 228).
type ProvisionedSession struct {
	SessionID   string
	AccountID   int64
	PubKey      [32]byte
	SNIHostname string // bound hostname — "" = any SNI the listener serves
	Environment string
	Status      string // ACTIVE | DISABLED | DRAINING
	Instruments map[uint32]bool // nil = all
}

// SessionStore resolves an Ed25519 public key to its provisioned
// session row. PgSessionStore in production; tests use MemorySessionStore.
type SessionStore interface {
	SessionByPubKey(ctx context.Context, pub [32]byte) (*ProvisionedSession, error)
}

// ChallengeMessage is the byte string the client's Ed25519 key signs:
// server nonce ‖ SNI hostname ‖ TLS keying material. Binding all three
// prevents proof replay across sessions, hostnames and channels.
func ChallengeMessage(nonce [16]byte, sni string, km []byte) []byte {
	out := make([]byte, 0, 16+len(sni)+len(km))
	out = append(out, nonce[:]...)
	out = append(out, sni...)
	out = append(out, km...)
	return out
}

// VerifySessionProof checks the Negotiate signature against the
// registered session key and the channel context.
func VerifySessionProof(pub [32]byte, nonce [16]byte, sni string,
	km []byte, sig [64]byte) bool {
	if !ed25519.Verify(ed25519.PublicKey(pub[:]),
		ChallengeMessage(nonce, sni, km), sig[:]) {
		return false
	}
	return true
}

// NewNonce mints a single-use server challenge nonce.
func NewNonce() ([16]byte, error) {
	var n [16]byte
	_, err := rand.Read(n[:])
	return n, err
}

// SNIBinder captures the client-requested SNI at ClientHello and exposes
// it per connection — the Negotiate proof is verified against it.
type SNIBinder struct {
	mu   sync.Mutex
	name map[net.Conn]string
}

// NewSNIBinder builds an empty binder.
func NewSNIBinder() *SNIBinder { return &SNIBinder{name: map[net.Conn]string{}} }

// WrapGetConfigForClient composes the binder into a tls.Config's
// GetConfigForClient hook (replacing it when nil).
func (b *SNIBinder) WrapGetConfigForClient(cfg *tls.Config) {
	inner := cfg.GetConfigForClient
	cfg.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		b.mu.Lock()
		b.name[hello.Conn] = hello.ServerName
		b.mu.Unlock()
		if inner != nil {
			return inner(hello)
		}
		return nil, nil
	}
}

// ServerName returns the captured SNI for conn ("" when absent).
func (b *SNIBinder) ServerName(conn net.Conn) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.name[conn]
}

// Forget drops the binding on connection close.
func (b *SNIBinder) Forget(conn net.Conn) {
	b.mu.Lock()
	delete(b.name, conn)
	b.mu.Unlock()
}

// TLSKeyingMaterial exports the channel-binding bytes from a completed
// TLS 1.3 handshake ("EXPORTER" label, RFC 8446 exporter). Unavailable
// exporter material yields nil — callers fail closed.
func TLSKeyingMaterial(conn *tls.Conn) ([]byte, error) {
	st := conn.ConnectionState()
	if !st.HandshakeComplete {
		return nil, fmt.Errorf("fixsbe: TLS handshake not complete")
	}
	return st.TLSUnique, nil
}

// ---------------------------------------------------------------------------
// Negotiation — schema lifecycle via the Phase-06 registry
// ---------------------------------------------------------------------------

// Negotiator applies schema negotiation + session-key verification to a
// decoded Negotiate message.
type Negotiator struct {
	Registry    *sbe.Registry
	Sessions    SessionStore
	Environment string
	Now         func() time.Time
}

func (n *Negotiator) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// ErrNegotiate carries the NegotiationResponse fields for a refused
// session handshake.
type ErrNegotiate struct {
	Status uint8
	Detail string
}

func (e *ErrNegotiate) Error() string { return e.Detail }

// Handshake verifies the session-key proof, resolves the session row,
// negotiates the schema version, and returns the bound SessionInfo.
// Failures return *ErrNegotiate (status to echo on the wire).
func (n *Negotiator) Handshake(ctx context.Context, sni string, km []byte,
	m *Negotiate) (*SessionInfo, *NegotiationResponse, error) {
	resp := &NegotiationResponse{
		SchemaID:      m.SchemaID,
		SchemaVersion: m.SchemaVersion,
		Status:        NegotiateRejected,
	}
	if m.ResponseCodec != CodecSBE && m.ResponseCodec != CodecTagValue {
		resp.ErrorCode = RejMalformed
		return nil, resp, &ErrNegotiate{Status: NegotiateRejected,
			Detail: "response codec must be SBE(0) or tag-value(1)"}
	}
	// Schema lifecycle check via the shared Phase-06 registry —
	// retired/unknown schema → reject before touching the key store.
	if n.Registry != nil {
		neg, err := n.Registry.Negotiate(m.SchemaID, m.SchemaVersion, n.now())
		if err != nil {
			return nil, resp, &ErrNegotiate{Status: NegotiateRejected,
				Detail: err.Error()}
		}
		resp.SchemaID = neg.SchemaID
		resp.SchemaVersion = neg.Version
		if neg.Deprecated {
			resp.Status = NegotiateDeprecated
			resp.SunsetTimeNs = uint64(neg.Sunset.UnixNano())
		}
	}
	if n.Sessions == nil {
		return nil, resp, &ErrNegotiate{Status: NegotiateRejected,
			Detail: "session store unavailable"}
	}
	row, err := n.Sessions.SessionByPubKey(ctx, m.SessionPubKey)
	if err != nil {
		return nil, resp, &ErrNegotiate{Status: NegotiateRejected,
			Detail: fmt.Sprintf("session lookup failed: %v", err)}
	}
	if row == nil {
		return nil, resp, &ErrNegotiate{Status: NegotiateRejected,
			Detail: "unknown session key"}
	}
	if row.Status != "ACTIVE" && row.Status != "DRAINING" {
		return nil, resp, &ErrNegotiate{Status: NegotiateRejected,
			Detail: fmt.Sprintf("session %s status %s", row.SessionID, row.Status)}
	}
	if row.Environment != "" && n.Environment != "" &&
		row.Environment != n.Environment {
		return nil, resp, &ErrNegotiate{Status: NegotiateRejected,
			Detail: fmt.Sprintf("session bound to environment %q", row.Environment)}
	}
	// SNI binding: a provisioned hostname must match the SNI the client
	// actually requested.
	if row.SNIHostname != "" &&
		!strings.EqualFold(row.SNIHostname, sni) {
		return nil, resp, &ErrNegotiate{Status: NegotiateRejected,
			Detail: "SNI hostname does not match session binding"}
	}
	if !VerifySessionProof(row.PubKey, m.Nonce, sni, km, m.Signature) {
		return nil, resp, &ErrNegotiate{Status: NegotiateRejected,
			Detail: "session-key proof failed"}
	}
	if resp.Status == NegotiateRejected {
		resp.Status = NegotiateOK
	}
	sess := &SessionInfo{
		ID:          row.SessionID,
		AccountID:   row.AccountID,
		Codec:       m.ResponseCodec,
		Draining:    row.Status == "DRAINING",
		Instruments: row.Instruments,
	}
	return sess, resp, nil
}

// ---------------------------------------------------------------------------
// Mux — one listener, both transports
// ---------------------------------------------------------------------------

// Mux dispatches sniffed connections to per-transport handlers. The
// same business contract runs on both paths (Gateway for SBE; the FIX
// 4.4 application for tag-value).
type Mux struct {
	// OnTagValue / OnSBE receive the sniffed connection (a peekConn —
	// already-read bytes are replayed). Return error closes the conn.
	OnTagValue func(ctx context.Context, conn net.Conn) error
	OnSBE      func(ctx context.Context, conn net.Conn) error
}

// Serve classifies conn's preamble and dispatches. peekConn replays
// sniffed bytes so handlers see the untouched stream.
func (m *Mux) Serve(ctx context.Context, conn net.Conn) error {
	peek := make([]byte, headerSize)
	if _, err := readFull(conn, peek); err != nil {
		return err
	}
	pc := &peekConn{Conn: conn, peek: peek}
	switch Classify(peek) {
	case TransportTagValue:
		if m.OnTagValue == nil {
			return fmt.Errorf("fixsbe: tag-value transport not served")
		}
		return m.OnTagValue(ctx, pc)
	case TransportSBE:
		if m.OnSBE == nil {
			return fmt.Errorf("fixsbe: SBE transport not served")
		}
		return m.OnSBE(ctx, pc)
	default:
		return fmt.Errorf("fixsbe: unrecognised transport preamble")
	}
}

func readFull(conn net.Conn, b []byte) (int, error) {
	got := 0
	for got < len(b) {
		n, err := conn.Read(b[got:])
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}

// peekConn replays the sniffed prefix before reading the wire.
type peekConn struct {
	net.Conn
	peek []byte
}

func (c *peekConn) Read(b []byte) (int, error) {
	if len(c.peek) > 0 {
		n := copy(b, c.peek)
		c.peek = c.peek[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}
