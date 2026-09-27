// Malformed-frame + gateway-edge rejection scenarios (L3 tier):
//
//   * FlatBuffers Event decode of garbage/truncated/crafted buffers must
//     surface a typed coded rejection — the panic must never escape to the
//     caller (spec §2.7.2 L3: fast rejection at the gateway edge).
//   * HMAC edge policy per spec §8.1: invalid signature -> 401
//     INVALID_SIGNATURE, replayed signature -> 401 REPLAY_ATTACK_DETECTED,
//     stale timestamp -> 401 TIMESTAMP_OUT_OF_WINDOW, insufficient scope ->
//     403 FORBIDDEN — all as RFC 7807 application/problem+json envelopes
//     (spec §22.7).
//
// The Phase-05 HMAC middleware does not exist yet; the edge verifier below
// is the harness-local policy oracle implementing §8.1 semantics and
// writing envelopes through the real pkg/errors Problem machinery, so the
// wire contract asserted here is the same one Phase-05 must emit.

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	flatbuffers "github.com/google/flatbuffers/go"

	"exchange/internal/ipc"
	"exchange/internal/ipc/wire"
	excerrors "exchange/pkg/errors"
)

// decodeEventGuarded is the fail-closed edge-decoder contract the harness
// asserts: bounds pre-check + panic containment around the zero-copy
// FlatBuffers reader. Returns a coded error; a panic is converted, never
// propagated. (services/internal/ipc/messages.go owns the production
// decode path; Phase-05 Task 5.3.x must apply this same guard.)
func decodeEventGuarded(buf []byte) (ev *wire.Event, err error) {
	defer func() {
		if r := recover(); r != nil {
			ev = nil
			err = excerrors.New("INVALID_REQUEST",
				fmt.Sprintf("malformed frame: decoder panic contained: %v", r))
		}
	}()
	if len(buf) < 8 {
		return nil, excerrors.New("INVALID_REQUEST",
			"malformed frame: shorter than minimum FlatBuffers table")
	}
	root := flatbuffers.GetUOffsetT(buf)
	if root < 4 || int(root) >= len(buf) {
		return nil, excerrors.New("INVALID_REQUEST",
			"malformed frame: root table offset out of bounds")
	}
	ev = ipc.DecodeEvent(buf)
	if ev == nil {
		return nil, excerrors.New("INVALID_REQUEST", "malformed frame: nil event")
	}
	// Touch every field the consumer pipeline reads — on crafted garbage
	// these dereferences are where a panic would escape without the guard.
	_ = ev.Seq()
	_ = ev.Ts()
	tt := ev.TypeType()
	switch tt {
	case wire.EventTypeOrderNew:
		on := ipc.EventOrderNew(ev)
		if on == nil {
			return nil, excerrors.New("INVALID_REQUEST",
				"malformed frame: OrderNew union member missing")
		}
		_ = on.OrderId()
		_ = on.Qty()
	case wire.EventTypeTradeFill:
		if ipc.EventTradeFill(ev) == nil {
			return nil, excerrors.New("INVALID_REQUEST",
				"malformed frame: TradeFill union member missing")
		}
	case wire.EventTypeNONE:
		return nil, excerrors.New("INVALID_REQUEST",
			"malformed frame: empty union discriminator")
	default:
		return nil, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("malformed frame: unknown event discriminator %d", tt))
	}
	return ev, nil
}

func validEventBytes() []byte {
	b := flatbuffers.NewBuilder(256)
	return ipc.EncodeOrderNewEvent(b, 42, 1700000000000000000, ipc.OrderNewMsg{
		OrderID: 1, AccountID: 7, InstrumentID: 100,
		Side: wire.SideBuy, Type: wire.OrderTypeLimit,
		Qty: 1_0000_0000, Price: 1_2345_0000,
		TIF: wire.TimeInForceGTC, ClientOrderID: "fault-probe",
	})
}

func scenarioMalformedFrames(_ context.Context, _ *env) *Checks {
	c := &Checks{}

	valid := validEventBytes()

	corpus := []struct {
		name string
		buf  []byte
	}{
		{"empty", nil},
		{"three_bytes", []byte{0x01, 0x02, 0x03}},
		{"garbage_64", bytes.Repeat([]byte{0xA5}, 64)},
		{"zeros_64", make([]byte, 64)},
		{"truncated_half", valid[:len(valid)/2]},
		{"root_past_eof", func() []byte {
			b := append([]byte(nil), valid...)
			// Root uoffset at [0:4] — point it past the buffer end.
			b[0], b[1], b[2], b[3] = 0xFF, 0xFF, 0xFF, 0xFF
			return b
		}()},
		{"vtable_smashed", func() []byte {
			b := append([]byte(nil), valid...)
			// Corrupt the soffset that locates the vtable: the root table
			// starts at buf[root]; the i32 at that position is the signed
			// vtable distance.
			root := int(flatbuffers.GetUOffsetT(b))
			if root+4 <= len(b) {
				b[root], b[root+1], b[root+2], b[root+3] = 0x7F, 0x7F, 0x7F, 0x7F
			}
			return b
		}()},
		{"discriminator_garbage", func() []byte {
			b := flatbuffers.NewBuilder(256)
			wire.EventStart(b)
			wire.EventAddSeq(b, 1)
			wire.EventAddTypeType(b, wire.EventType(200)) // out of enum range
			b.Finish(wire.EventEnd(b))
			return b.FinishedBytes()
		}()},
	}
	for _, k := range corpus {
		_, err := decodeEventGuarded(k.buf)
		c.okf(k.name+":typed_reject", err != nil &&
			errCode(err) == "INVALID_REQUEST",
			"err=%v", err)
	}

	// Positive control: a well-formed event must still decode.
	ev, err := decodeEventGuarded(valid)
	c.okf("valid_event:decodes", err == nil && ev != nil && ev.Seq() == 42,
		"err=%v seq=%d", err, func() uint64 {
			if ev == nil {
				return 0
			}
			return ev.Seq()
		}())

	// Nested frames inside the shm transport: a consumer Poll buffer smaller
	// than the slot message must report -1 (not truncate silently).
	c.info("corpus", fmt.Sprintf("%d malformed cases asserted", len(corpus)))
	return c
}

// --- HMAC edge policy oracle (spec §8.1) -------------------------------------
//
// signature = hex HMAC-SHA256(apiSecret, X-TIMESTAMP + method + path + body)
// headers: X-API-KEY, X-SIGNATURE, X-TIMESTAMP; replay window 30s.

type edgeOracle struct {
	secret  []byte
	scopes  map[string][]string // apiKey -> scopes
	seenSig map[string]time.Time
	now     func() time.Time
}

func (o *edgeOracle) verify(r *http.Request, body []byte, needScope string) error {
	key := r.Header.Get("X-API-KEY")
	tsStr := r.Header.Get("X-TIMESTAMP")
	sig := r.Header.Get("X-SIGNATURE")
	scopes, known := o.scopes[key]
	if !known || key == "" || tsStr == "" || sig == "" {
		return excerrors.New("UNAUTHORIZED", "missing or unknown credentials")
	}
	tsMs, perr := strconv.ParseInt(tsStr, 10, 64)
	if perr != nil {
		return excerrors.New("INVALID_SIGNATURE", "unparseable timestamp")
	}
	ts := time.UnixMilli(tsMs)
	if d := o.now().Sub(ts); d > 30*time.Second || d < -30*time.Second {
		return excerrors.New("TIMESTAMP_OUT_OF_WINDOW",
			"signature timestamp outside 30s replay window")
	}
	mac := hmac.New(sha256.New, o.secret)
	mac.Write([]byte(tsStr + r.Method + r.URL.Path))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return excerrors.New("INVALID_SIGNATURE", "HMAC signature mismatch")
	}
	if _, dup := o.seenSig[sig]; dup {
		return excerrors.New("REPLAY_ATTACK_DETECTED",
			"signature already presented inside replay window")
	}
	o.seenSig[sig] = o.now()
	if needScope != "" {
		granted := false
		for _, s := range scopes {
			if s == needScope {
				granted = true
			}
		}
		if !granted {
			return excerrors.New("FORBIDDEN", "scope not granted")
		}
	}
	return nil
}

func scenarioAuthEdge(ctx context.Context, _ *env) *Checks {
	c := &Checks{}

	oracle := &edgeOracle{
		secret:  []byte("fault-injection-secret"),
		scopes:  map[string][]string{"k-good": {"trade"}, "k-readonly": {"read"}},
		seenSig: map[string]time.Time{},
		now:     time.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/orders", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := oracle.verify(r, body, "trade"); err != nil {
			p := excerrors.ProblemFor(err)
			// Phase-05 owns the code->status registry (Task 5.3.21); the
			// scaffold HTTPStatus() only knows Task-1.3.12 codes, so the
			// oracle maps the §8.1 auth codes explicitly per §23.
			switch errCode(err) {
			case "UNAUTHORIZED", "INVALID_SIGNATURE", "REPLAY_ATTACK_DETECTED",
				"TIMESTAMP_OUT_OF_WINDOW":
				p = excerrors.NewProblem(http.StatusUnauthorized, errCode(err),
					errCode(err), err.Error())
			case "FORBIDDEN":
				p = excerrors.NewProblem(http.StatusForbidden, errCode(err),
					errCode(err), err.Error())
			}
			p.WriteTo(w)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	body := []byte(`{"instrument":"EUR/USD","qty":"100000"}`)
	sign := func(key, tsMs string, b []byte) string {
		mac := hmac.New(sha256.New, oracle.secret)
		mac.Write([]byte(tsMs + "POST" + "/api/v1/orders"))
		mac.Write(b)
		return hex.EncodeToString(mac.Sum(nil))
	}
	do := func(key, tsMs, sig string) (int, string, map[string]any) {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
			srv.URL+"/api/v1/orders", bytes.NewReader(body))
		req.Header.Set("X-API-KEY", key)
		req.Header.Set("X-TIMESTAMP", tsMs)
		req.Header.Set("X-SIGNATURE", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return -1, "", nil
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header.Get("Content-Type"), decodeJSON(string(b))
	}
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)

	// 1) invalid signature -> 401 INVALID_SIGNATURE
	st, ct, pb := do("k-good", now, "deadbeef")
	c.okf("bad_signature:401", st == http.StatusUnauthorized, "status=%d", st)
	c.okf("bad_signature:problem_json", ct == excerrors.ProblemMediaType, "ct=%q", ct)
	c.okf("bad_signature:code", jstr(pb, "code") == "INVALID_SIGNATURE" &&
		jstr(pb, "type") == "urn:exc:problem:INVALID_SIGNATURE" &&
		jnum(pb, "status") == 401, "problem=%v", pb)

	// 2) replay of an identical signed request -> 401 REPLAY_ATTACK_DETECTED
	sig := sign("k-good", now, body)
	st, _, _ = do("k-good", now, sig)
	c.okf("replay:first_accepted", st == http.StatusAccepted, "status=%d", st)
	st, ct, pb = do("k-good", now, sig)
	c.okf("replay:401", st == http.StatusUnauthorized, "status=%d", st)
	c.okf("replay:code", jstr(pb, "code") == "REPLAY_ATTACK_DETECTED" &&
		ct == excerrors.ProblemMediaType, "problem=%v ct=%q", pb, ct)

	// 3) timestamp outside the 30s window -> 401 TIMESTAMP_OUT_OF_WINDOW
	stale := strconv.FormatInt(time.Now().Add(-60*time.Second).UnixMilli(), 10)
	st, ct, pb = do("k-good", stale, sign("k-good", stale, body))
	c.okf("stale_ts:401", st == http.StatusUnauthorized, "status=%d", st)
	c.okf("stale_ts:code", jstr(pb, "code") == "TIMESTAMP_OUT_OF_WINDOW" &&
		ct == excerrors.ProblemMediaType, "problem=%v", pb)

	// 4) valid signature, insufficient scope -> 403 FORBIDDEN
	now2 := strconv.FormatInt(time.Now().UnixMilli(), 10)
	// distinct ts so the signature is fresh (not a replay)
	time.Sleep(time.Millisecond * 2)
	st, ct, pb = do("k-readonly", now2, sign("k-readonly", now2, body))
	c.okf("scope:403", st == http.StatusForbidden, "status=%d", st)
	c.okf("scope:code", jstr(pb, "code") == "FORBIDDEN" &&
		ct == excerrors.ProblemMediaType, "problem=%v", pb)

	// 5) unknown API key -> 401 UNAUTHORIZED
	now3 := strconv.FormatInt(time.Now().UnixMilli(), 10)
	st, _, pb = do("k-nope", now3, sign("k-nope", now3, body))
	c.okf("unknown_key:401", st == http.StatusUnauthorized &&
		jstr(pb, "code") == "UNAUTHORIZED", "status=%d problem=%v", st, pb)

	// 6) ProblemFor on a registered core code resolves through the shared
	//    severity machinery (TIME_SYNC_LOSS_HALT -> 503/L0 envelope).
	rec := httptest.NewRecorder()
	excerrors.WriteProblem(rec,
		excerrors.New(excerrors.CodeTimeSyncLossHalt, "clock drift 250us"))
	c.okf("problemfor:registered_code_503",
		rec.Code == 503 && rec.Header().Get("Content-Type") == excerrors.ProblemMediaType &&
			jstr(decodeJSON(rec.Body.String()), "code") == excerrors.CodeTimeSyncLossHalt,
		"status=%d body=%s", rec.Code, rec.Body.String())

	return c
}
