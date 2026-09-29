// transport_test.go — Task 18.3.17: transport sniffing, frame
// extraction, Ed25519 session-key + SNI binding, schema negotiation,
// maintenance News drain.
package fixsbe

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"exchange/internal/sbe"
)

func TestClassify(t *testing.T) {
	if Classify([]byte("8=FIX.4.4\x019=5\x0135=A\x01")) != TransportTagValue {
		t.Fatal("tag-value preamble not classified")
	}
	if Classify([]byte("8=FIXT.1.1\x019=5\x0135=A\x01")) != TransportTagValue {
		t.Fatal("FIXT preamble not classified")
	}
	sbeFrame := MarshalMessage(NewOrder{ClOrdID: SetClOrdID("x"), Qty: 1})
	if Classify(sbeFrame) != TransportSBE {
		t.Fatal("SBE preamble not classified")
	}
	// Schema 1 (market-data) is not the order-entry listener's traffic.
	md := append([]byte(nil), sbeFrame...)
	md[4], md[5] = 1, 0
	if Classify(md) != TransportUnknown {
		t.Fatal("market-data schema misclassified as order-entry")
	}
	if Classify([]byte("GET / HTTP/1.1")) != TransportUnknown {
		t.Fatal("garbage preamble accepted")
	}
}

func TestReadFrameSBE(t *testing.T) {
	f1 := MarshalMessage(NewOrder{ClOrdID: SetClOrdID("a"), Qty: 1})
	f2 := MarshalMessage(CancelOrder{ClOrdID: SetClOrdID("b")})
	buf := append(f1, f2...)
	fr, rest, err := ReadFrame(buf, TransportSBE)
	if err != nil || len(fr) != len(f1) {
		t.Fatalf("frame1: %v %d", err, len(fr))
	}
	fr, rest, err = ReadFrame(rest, TransportSBE)
	if err != nil || len(fr) != len(f2) || len(rest) != 0 {
		t.Fatalf("frame2: %v", err)
	}
	// Incomplete tails wait for more bytes.
	fr, rest, _ = ReadFrame(f1[:10], TransportSBE)
	if fr != nil || len(rest) != 10 {
		t.Fatal("partial frame consumed")
	}
	// Absurd blockLength fails closed.
	huge := append([]byte(nil), f1...)
	huge[0], huge[1] = 0xFF, 0xFF
	if _, _, err := ReadFrame(huge, TransportSBE); err == nil {
		t.Fatal("oversized blockLength accepted")
	}
}

func TestReadFrameTagValue(t *testing.T) {
	tv := []byte("8=FIX.4.4\x019=10\x0135=D\x0111=x\x0110=001\x01")
	extra := []byte("8=FIX.4.4\x019=10\x0135=F\x0111=y\x0110=002\x01")
	buf := append(tv, extra...)
	fr, rest, err := ReadFrame(buf, TransportTagValue)
	if err != nil || string(fr) != string(tv) || string(rest) != string(extra) {
		t.Fatalf("tag-value frame split failed: %q | %q | %v", fr, rest, err)
	}
	// Incomplete frame waits.
	fr, rest, _ = ReadFrame(tv[:len(tv)-3], TransportTagValue)
	if fr != nil {
		t.Fatal("partial tag-value frame consumed")
	}
}

// memSessions is the test SessionStore.
type memSessions map[[32]byte]*ProvisionedSession

func (m memSessions) SessionByPubKey(_ context.Context, pub [32]byte) (*ProvisionedSession, error) {
	return m[pub], nil
}

func signAll(priv ed25519.PrivateKey, nonce [16]byte, sni string, km []byte) [64]byte {
	var sig [64]byte
	copy(sig[:], ed25519.Sign(priv, ChallengeMessage(nonce, sni, km)))
	return sig
}

func testNegotiator(t *testing.T) (*Negotiator, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var pubArr [32]byte
	copy(pubArr[:], pub)
	reg := sbe.NewRegistry()
	if err := reg.Register(sbe.SchemaVersion{
		SchemaID: SchemaIDOrderEntry, Version: 1, State: sbe.LifecycleActive,
	}); err != nil {
		t.Fatalf("registry: %v", err)
	}
	n := &Negotiator{
		Registry: reg,
		Sessions: memSessions{
			pubArr: {
				SessionID:   "sess-1",
				AccountID:   7,
				PubKey:      pubArr,
				SNIHostname: "fixsbe.exchange.example",
				Environment: "production",
				Status:      "ACTIVE",
			},
		},
		Environment: "production",
	}
	return n, priv
}

func goodNegotiate(priv ed25519.PrivateKey, nonce [16]byte, sni string, km []byte) *Negotiate {
	var m Negotiate
	m.SchemaID = SchemaIDOrderEntry
	m.SchemaVersion = 1
	m.ResponseCodec = CodecSBE
	pub := priv.Public().(ed25519.PublicKey)
	copy(m.SessionPubKey[:], pub)
	m.Nonce = nonce
	m.Signature = signAll(priv, nonce, sni, km)
	return &m
}

func TestHandshakeHappyPath(t *testing.T) {
	n, priv := testNegotiator(t)
	nonce, _ := NewNonce()
	sess, resp, err := n.Handshake(context.Background(),
		"fixsbe.exchange.example", []byte("km"),
		goodNegotiate(priv, nonce, "fixsbe.exchange.example", []byte("km")))
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if sess.ID != "sess-1" || sess.AccountID != 7 || sess.Draining {
		t.Fatalf("session binding: %+v", sess)
	}
	if resp.Status != NegotiateOK {
		t.Fatalf("response status %d", resp.Status)
	}
}

func TestHandshakeMatrix(t *testing.T) {
	n, priv := testNegotiator(t)
	nonce, _ := NewNonce()
	sni := "fixsbe.exchange.example"
	km := []byte("km")
	ctx := context.Background()

	// Wrong SNI → proof signs a different message AND binding mismatches.
	m := goodNegotiate(priv, nonce, "other.example", km)
	if _, _, err := n.Handshake(ctx, "other.example", km, m); err == nil {
		t.Fatal("SNI mismatch accepted")
	}
	// Signature over correct material but wrong channel SNI.
	m = goodNegotiate(priv, nonce, sni, km)
	if _, _, err := n.Handshake(ctx, "other.example", km, m); err == nil {
		t.Fatal("proof replayed across SNI")
	}
	// Tampered signature.
	m = goodNegotiate(priv, nonce, sni, km)
	m.Signature[0] ^= 0xFF
	if _, _, err := n.Handshake(ctx, sni, km, m); err == nil {
		t.Fatal("bad signature accepted")
	}
	// Unknown key.
	roguePub, rogue, _ := ed25519.GenerateKey(rand.Reader)
	m = goodNegotiate(rogue, nonce, sni, km)
	if _, _, err := n.Handshake(ctx, sni, km, m); err == nil {
		t.Fatal("unprovisioned session key accepted")
	}
	// Disabled session.
	var roguePubArr [32]byte
	copy(roguePubArr[:], roguePub)
	n.Sessions.(memSessions)[roguePubArr] = &ProvisionedSession{
		SessionID: "dead", PubKey: roguePubArr, Status: "DISABLED",
	}
	m = goodNegotiate(rogue, nonce, sni, km)
	if _, _, err := n.Handshake(ctx, sni, km, m); err == nil {
		t.Fatal("disabled session accepted")
	}
	// Environment binding: production listener rejects a sandbox-bound row.
	n.Sessions.(memSessions)[roguePubArr].Status = "ACTIVE"
	n.Sessions.(memSessions)[roguePubArr].Environment = "sandbox"
	m = goodNegotiate(rogue, nonce, sni, km)
	if _, _, err := n.Handshake(ctx, sni, km, m); err == nil {
		t.Fatal("sandbox session admitted to production")
	}
	// Draining session still binds (drain gates order flow, not logon).
	var pk [32]byte
	copy(pk[:], priv.Public().(ed25519.PublicKey))
	n.Sessions.(memSessions)[pk].Status = "DRAINING"
	sess, _, err := n.Handshake(ctx, sni, km, goodNegotiate(priv, nonce, sni, km))
	if err != nil || !sess.Draining {
		t.Fatalf("draining session: %v %+v", err, sess)
	}
}

// TestSchemaNegotiationLifecycle — deprecated schema negotiates with a
// sunset stamp; retired schema rejects outright (§24 #284). The
// six-month deprecation floor is enforced inside sbe.Registry.Register.
func TestSchemaNegotiationLifecycle(t *testing.T) {
	n, priv := testNegotiator(t)
	// Rebuild registry with a deprecated row (sunset > 6 months out).
	reg := sbe.NewRegistry()
	if err := reg.Register(sbe.SchemaVersion{
		SchemaID:     SchemaIDOrderEntry,
		Version:      1,
		State:        sbe.LifecycleDeprecated,
		DeprecatedAt: time.Now(),
		RetiresAt:    time.Now().AddDate(0, 7, 0),
	}); err != nil {
		t.Fatalf("deprecate register: %v", err)
	}
	n.Registry = reg
	nonce, _ := NewNonce()
	sni := "fixsbe.exchange.example"
	m := goodNegotiate(priv, nonce, sni, []byte("km"))
	_, resp, err := n.Handshake(context.Background(), sni, []byte("km"), m)
	if err != nil {
		t.Fatalf("deprecated schema should negotiate: %v", err)
	}
	if resp.Status != NegotiateDeprecated || resp.SunsetTimeNs == 0 {
		t.Fatalf("expected deprecated status with sunset: %+v", resp)
	}

	// Retired schema → handshake fails closed.
	reg2 := sbe.NewRegistry()
	if err := reg2.Register(sbe.SchemaVersion{
		SchemaID: SchemaIDOrderEntry, Version: 1, State: sbe.LifecycleRetired,
	}); err != nil {
		t.Fatal(err)
	}
	n.Registry = reg2
	if _, _, err := n.Handshake(context.Background(), sni, []byte("km"), m); err == nil {
		t.Fatal("retired schema negotiated")
	}
}

// drainT is a capture DrainTarget.
type drainT struct {
	id     string
	codec  ResponseCodec
	frames [][]byte
}

func (d *drainT) SessionID() string    { return d.id }
func (d *drainT) Codec() ResponseCodec { return d.codec }

func (d *drainT) SendFrame(_ context.Context, f []byte) error {
	d.frames = append(d.frames, f)
	return nil
}

// TestDrainBroadcast — the drainer emits dual-encoding News advisories
// per round until the listener empties.
func TestDrainBroadcast(t *testing.T) {
	t1 := &drainT{id: "s1", codec: CodecSBE}
	t2 := &drainT{id: "s2", codec: CodecTagValue}
	d := &Drainer{
		Interval:    time.Millisecond,
		Replacement: "gw2.exchange.example:5001",
		Reason:      "test drain",
		Source:      func() []DrainTarget { return []DrainTarget{t1, t2} },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_ = d.Drain(ctx)
	if len(t1.frames) == 0 || len(t2.frames) == 0 {
		t.Fatal("no advisories emitted")
	}
	// SBE target decodes as News.
	m, _, err := DecodeMessage(t1.frames[0])
	if err != nil {
		t.Fatalf("SBE news decode: %v", err)
	}
	news := m.(News)
	if news.Urgency != NewsUrgencyFlash {
		t.Fatalf("urgency %d", news.Urgency)
	}
	// Tag-value target gets a 35=B frame containing the headline and the
	// replacement endpoint.
	got := string(t2.frames[0])
	for _, want := range []string{"35=B", "148=", "61=1", "gw2.exchange.example:5001"} {
		if !strings.Contains(got, want) {
			t.Fatalf("tag-value News missing %q: %q", want, got)
		}
	}
	// Empty source ends the drain cleanly.
	d.Source = func() []DrainTarget { return nil }
	if err := d.Drain(context.Background()); err != nil {
		t.Fatalf("empty drain should return nil: %v", err)
	}
}
