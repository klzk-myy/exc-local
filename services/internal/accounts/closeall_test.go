package accounts

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---- fakes ----

type fakeGuard struct{ err error }

func (g fakeGuard) AssertMutable(context.Context, int64) error { return g.err }

type fakePositions struct {
	positions []OpenPosition
	instrID   int64
	instrErr  error
}

func (f *fakePositions) OpenPositions(context.Context, int64, string, string) ([]OpenPosition, error) {
	return f.positions, nil
}
func (f *fakePositions) ResolveInstrument(context.Context, string) (int64, error) {
	return f.instrID, f.instrErr
}

type staticTOTP struct{ secret string }

func (s staticTOTP) TOTPSecretForAccount(context.Context, int64) (string, error) {
	return s.secret, nil
}

// totpTestCode computes an RFC 6238 SHA1/30s/6-digit code — the venue
// profile (auth.VerifyTOTP semantics, kept test-local so this package
// does not import the mid-flight auth cluster).
func totpTestCode(secret string, at time.Time) (string, error) {
	s := strings.ToUpper(strings.TrimSpace(secret))
	if rem := len(s) % 8; rem != 0 {
		s += strings.Repeat("=", 8-rem)
	}
	raw, err := base32.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(at.Unix()/30))
	mac := hmac.New(sha1.New, raw)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 | (uint32(sum[off+1])&0xff)<<16 |
		(uint32(sum[off+2])&0xff)<<8 | uint32(sum[off+3])&0xff
	return fmt.Sprintf("%06d", bin%1000000), nil
}

// testVerifier checks the current step (no drift needed for tests).
var testVerifier TOTPVerifier = func(secret, code string, at time.Time) bool {
	want, err := totpTestCode(secret, at)
	return err == nil && want == code
}

func totpCodeFor(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	c, err := totpTestCode(secret, at)
	if err != nil {
		t.Fatalf("totp code: %v", err)
	}
	return c
}

const testTOTPSecret = "JBSWY3DPEHPK3PXP" // base32 "Hello!\xDE\xAD\xBE\xEF"

func newCloseAll(guardErr error, disp *fakeDispatcher, pos *fakePositions) *CloseAllService {
	return NewCloseAllService(fakeGuard{guardErr}, pos,
		staticTOTP{secret: testTOTPSecret}, testVerifier, disp)
}

// ---- tests ----

func TestCloseAllFrozenAccountRejected(t *testing.T) {
	disp := &fakeDispatcher{}
	svc := newCloseAll(newError(CodeAccountFrozen, "account is FROZEN"), disp,
		&fakePositions{})
	_, err := svc.CloseAll(context.Background(), 10, CloseAllFilter{}, "", true)
	requireCode(t, err, CodeAccountFrozen)
	if len(disp.cancels) != 0 || len(disp.closes) != 0 {
		t.Fatal("frozen account must reject before any dispatch")
	}
}

func TestCloseAllRequires2FA(t *testing.T) {
	svc := newCloseAll(nil, &fakeDispatcher{}, &fakePositions{})
	// No token, no session elevation → 403.
	_, err := svc.CloseAll(context.Background(), 10, CloseAllFilter{}, "", false)
	requireCode(t, err, CodeTwoFactorRequired)
	// Bad token → 403.
	_, err = svc.CloseAll(context.Background(), 10, CloseAllFilter{}, "000000", false)
	requireCode(t, err, CodeTwoFactorRequired)
	// Session AMR elevation satisfies the gate without a token.
	res, err := svc.CloseAll(context.Background(), 10, CloseAllFilter{}, "", true)
	if err != nil {
		t.Fatalf("session-elevated 2FA must pass: %v", err)
	}
	if res == nil {
		t.Fatal("nil result")
	}
}

func TestCloseAllCancelsThenCloses(t *testing.T) {
	disp := &fakeDispatcher{}
	pos := &fakePositions{
		positions: []OpenPosition{
			{ID: 1, AccountID: 10, InstrumentID: 100, Symbol: "EURUSD", Side: "LONG",
				Quantity: decimal.NewFromInt(5000), EntryPrice: decimal.MustFromString("1.10")},
			{ID: 2, AccountID: 10, InstrumentID: 101, Symbol: "USDJPY", Side: "SHORT",
				Quantity: decimal.NewFromInt(3000), EntryPrice: decimal.MustFromString("150.0")},
		},
		instrID: 100,
	}
	svc := newCloseAll(nil, disp, pos)
	at := svc.now()
	res, err := svc.CloseAll(context.Background(), 10,
		CloseAllFilter{}, totpCodeFor(t, testTOTPSecret, at), false)
	if err != nil {
		t.Fatalf("close-all: %v", err)
	}
	if res.OrdersCancelled != 7 {
		t.Fatalf("cancelled=%d, want 7 (fake)", res.OrdersCancelled)
	}
	if len(disp.cancels) != 1 || disp.cancels[0].AccountID != 10 || disp.cancels[0].Reason != "close_all" {
		t.Fatalf("mass cancel scope wrong: %+v", disp.cancels)
	}
	if len(res.Closes) != 2 || !res.Closes[0].OK || !res.Closes[1].OK {
		t.Fatalf("closes=%+v", res.Closes)
	}
	// LONG position → SELL close; SHORT → BUY close; reduce-only always.
	if disp.closes[0].Side != SideSell || disp.closes[1].Side != SideBuy {
		t.Fatalf("close sides wrong: %+v", disp.closes)
	}
	for _, c := range disp.closes {
		if !c.ReduceOnly {
			t.Fatal("close order without reduce_only")
		}
		if c.MaxSlippageBps != DefaultCloseAllSlippageBps {
			t.Fatalf("slippage bps=%d, want %d", c.MaxSlippageBps, DefaultCloseAllSlippageBps)
		}
	}
}

func TestCloseAllPartialFailure(t *testing.T) {
	disp := &fakeDispatcher{
		closeErrs: map[int64]error{101: errPlain("engine rejected")},
	}
	pos := &fakePositions{
		positions: []OpenPosition{
			{ID: 1, AccountID: 10, InstrumentID: 100, Symbol: "EURUSD", Side: "LONG",
				Quantity: decimal.NewFromInt(1), EntryPrice: decimal.MustFromString("1.1")},
			{ID: 2, AccountID: 10, InstrumentID: 101, Symbol: "USDJPY", Side: "SHORT",
				Quantity: decimal.NewFromInt(1), EntryPrice: decimal.MustFromString("150")},
		},
	}
	svc := newCloseAll(nil, disp, pos)
	res, err := svc.CloseAll(context.Background(), 10, CloseAllFilter{}, "", true)
	requireCode(t, err, CodeCloseAllPartialFailure)
	if res == nil || !res.PartialFailure {
		t.Fatal("partial failure flag missing")
	}
	if !res.Closes[0].OK || res.Closes[1].OK {
		t.Fatalf("per-position results wrong: %+v", res.Closes)
	}
	if res.Closes[1].Code == "" {
		t.Fatal("failed close lacks error code")
	}
}

func TestCloseAllSymbolFilterScopesMassCancel(t *testing.T) {
	disp := &fakeDispatcher{}
	pos := &fakePositions{positions: []OpenPosition{}, instrID: 42}
	svc := newCloseAll(nil, disp, pos)
	if _, err := svc.CloseAll(context.Background(), 10,
		CloseAllFilter{Symbol: "EURUSD"}, "", true); err != nil {
		t.Fatalf("close-all: %v", err)
	}
	if disp.cancels[0].InstrumentID != 42 {
		t.Fatalf("mass cancel instrument scope = %d, want 42", disp.cancels[0].InstrumentID)
	}
}

func TestCloseAllRejectsBadSideFilter(t *testing.T) {
	svc := newCloseAll(nil, &fakeDispatcher{}, &fakePositions{})
	_, err := svc.CloseAll(context.Background(), 10, CloseAllFilter{Side: "UP"}, "", true)
	requireCode(t, err, CodeInvalidRequest)
}

func TestPgxReadSeamsCompile(t *testing.T) {
	// Interface conformance — compile-time assertions.
	var _ PositionReader = (*PgxPositionReader)(nil)
	var _ TOTPSecretProvider = (*PgxTOTPSecrets)(nil)
	var _ OrderDispatcher = (*fakeDispatcher)(nil)
	var _ MutationGuard = (*FreezeService)(nil)
	_ = excerrors.Error{}
}
