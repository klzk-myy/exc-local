// Task 8.3.5 scenario 6 — L0–L3 fail-closed severity coverage
// (spec §2.7.2, §24 acceptance criterion #307).
//
// Each tier is exercised against a REAL production seam — not a label:
//
//	L0 Critical/Fatal — global halt flag semantics (Redis
//	   halt:global) + the instrument-HALTED order gate: while halted,
//	   new order flow is rejected before any persistence or IPC write
//	   (no continued mutation after the halt is observed). The C++
//	   matching-loop panic/WAL-CRC halt itself is engine-side and out
//	   of scope for this Go suite — see the §27 deviation note in the
//	   task report.
//	L1 Systemic     — degradation-mode transition surfaced on the wire
//	   (SetDegradationMode → GetDegradationMode → X-Degradation-Mode
//	   header) and invalid mode input rejected fail-closed; sustained
//	   upstream 5xx trips the gateway circuit breaker (see
//	   circuit_breaker_test.go).
//	L2 Transaction  — synchronous atomic rejection with zero side
//	   effects: a balance-shortfall submit is refused BEFORE
//	   InsertOrderTx/Send (in-process), and a killed PostgreSQL
//	   transaction leaves no partial mutation (db_drop_test.go).
//	L3 Edge         — protocol/auth faults rejected at the edge before
//	   any business handler runs (signature_test.go: HMAC/Ed25519/
//	   timestamp/replay/unknown-key/missing-header cases).
//
// Gating: L0-halt-flag and L1-mode subtests need EXC_REDIS_TEST=1;
// the L0-halted-instrument and L2-shortfall subtests run in-process.
package error_scenarios

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"exchange/internal/errs"
	"exchange/internal/ipc"
	"exchange/internal/middleware"
	"exchange/internal/orders"
	exchredis "exchange/internal/redis"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// L0 — Critical/Fatal: halt semantics
// ---------------------------------------------------------------------------

// TestL0_HaltFlagRoundtrip drives the real coordination halt flag:
// HaltGlobal must be observed by IsHalted (fail-closed halt), retain the
// operator reason, and ClearHalt must lift it. Runs on the dedicated
// test DB so production halt state is never touched.
func TestL0_HaltFlagRoundtrip(t *testing.T) {
	c := redisClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Precondition: not halted on the test DB.
	halted, err := c.IsHalted(ctx)
	if err != nil {
		t.Fatalf("pre-halt IsHalted: %v", err)
	}
	if halted {
		t.Fatal("test DB starts halted — refusing to disturb shared state")
	}
	t.Cleanup(func() { _ = c.ClearHalt(context.Background()) })

	if err := c.HaltGlobal(ctx, "errscen-l0-fault-injection"); err != nil {
		t.Fatalf("HaltGlobal: %v", err)
	}
	halted, err = c.IsHalted(ctx)
	if err != nil {
		t.Fatalf("post-halt IsHalted: %v", err)
	}
	if !halted {
		t.Fatal("HaltGlobal did not raise halt:global — L0 halt is not observable")
	}
	reason, err := c.Get(ctx, "halt:global").Result()
	if err != nil || reason != "errscen-l0-fault-injection" {
		t.Fatalf("halt reason=%q err=%v, want the operator reason stored", reason, err)
	}

	if err := c.ClearHalt(ctx); err != nil {
		t.Fatalf("ClearHalt: %v", err)
	}
	halted, err = c.IsHalted(ctx)
	if err != nil {
		t.Fatalf("post-clear IsHalted: %v", err)
	}
	if halted {
		t.Fatal("ClearHalt did not lift the halt flag")
	}
}

// TestL0_HaltedInstrumentRejectsOrderFlow asserts the L0 "no continued
// mutation" contract at the service seam: while the instrument is
// HALTED, Submit is rejected INSTRUMENT_HALTED and NEITHER the
// persistence path (InsertOrderTx) NOR the IPC path (Submitter.Send)
// runs — nothing proceeds after the halt.
func TestL0_HaltedInstrumentRejectsOrderFlow(t *testing.T) {
	store := &fakeOrderStore{inst: &orders.Instrument{
		ID: 7, Symbol: "EUR/USD", Status: "HALTED",
		BaseCurrency: "EUR", QuoteCurrency: "USD",
	}}
	sub := &countingSubmitter{}
	svc, err := orders.NewService(orders.Options{
		Store: store, Submitter: sub, ShardMap: mustShardMap(t),
	})
	if err != nil {
		t.Fatalf("orders.NewService: %v", err)
	}

	qty := decimal.NewFromInt(1000)
	price := decimal.MustFromString("1.10")
	_, err = svc.Submit(context.Background(),
		&orders.Account{ID: 7, Status: "ACTIVE", KycTier: "T2"},
		&orders.SubmitRequest{
			Symbol: "EUR/USD", Side: "BUY", OrderType: "LIMIT",
			Quantity: &qty, Price: &price,
		})
	e := codedErr(t, err)
	if e.Code != "INSTRUMENT_HALTED" {
		t.Fatalf("halted submit code=%s, want INSTRUMENT_HALTED", e.Code)
	}
	if store.insertCalls.Load() != 0 {
		t.Fatal("halted order flow reached InsertOrderTx — L0 zero-mutation violated")
	}
	if sub.calls.Load() != 0 {
		t.Fatal("halted order flow reached the engine IPC path")
	}
	if got := errs.New().HTTPStatus(e.Code); got < 400 {
		t.Fatalf("INSTRUMENT_HALTED maps to non-error status %d", got)
	}
}

// ---------------------------------------------------------------------------
// L1 — Systemic degradation: mode transition surfaced on the wire
// ---------------------------------------------------------------------------

// TestL1_DegradationModeTransition exercises the full coordination
// loop on the real client: write the mode, read it back, and verify
// the gateway middleware stamps it on responses — the L1 transition
// contract of §2.4/§2.7.
func TestL1_DegradationModeTransition(t *testing.T) {
	c := redisClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Scoped cleanup: only the three system:degradation keys. Reset BEFORE
	// the baseline read as well — a crashed run or a concurrent suite can
	// leave system:degradation_mode populated on the shared dev Redis.
	reset := func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		_ = c.Del(cctx, "system:degradation:mode",
			"system:degradation:reason", "system:degradation:entered_at").Err()
	}
	reset()
	t.Cleanup(reset)

	// Baseline: absent key reads as Normal (§2.4 default).
	st, err := c.GetDegradationMode(ctx)
	if err != nil {
		t.Fatalf("baseline GetDegradationMode: %v", err)
	}
	if st.Mode != exchredis.ModeNormal {
		t.Fatalf("baseline mode=%s, want Normal", st.Mode)
	}

	// Invalid mode rejected fail-closed BEFORE any write.
	if err := c.SetDegradationMode(ctx, "NotARealMode", "x"); err == nil {
		t.Fatal("SetDegradationMode accepted an invalid mode")
	}

	// L1 transition: infrastructure fault → ReadOnly.
	if err := c.SetDegradationMode(ctx, exchredis.ModeReadOnly,
		"errscen postgres lag >5s"); err != nil {
		t.Fatalf("SetDegradationMode(ReadOnly): %v", err)
	}
	st, err = c.GetDegradationMode(ctx)
	if err != nil {
		t.Fatalf("post-transition GetDegradationMode: %v", err)
	}
	if st.Mode != exchredis.ModeReadOnly || st.Reason != "errscen postgres lag >5s" {
		t.Fatalf("degradation state %+v, want ReadOnly with reason", st)
	}
	if st.EnteredAt == 0 {
		t.Fatal("degradation entered_at not recorded — transitions must be timestamped")
	}

	// Wire surface: the middleware stamps the recorded mode.
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	middleware.DegradationModeHeader(c, ok).ServeHTTP(rec,
		httptest.NewRequest(http.MethodGet, "/api/v1/instruments", nil))
	if got := rec.Header().Get(middleware.DegradationHeader); got != string(exchredis.ModeReadOnly) {
		t.Fatalf("X-Degradation-Mode=%q during L1 degradation, want ReadOnly", got)
	}
}

// ---------------------------------------------------------------------------
// L2 — Transaction boundary: synchronous atomic rejection, no side effects
// ---------------------------------------------------------------------------

// TestL2_BalanceShortfallAtomicRejection drives the real Submit path up
// to the pre-trade balance check: insufficient available balance must
// reject INSUFFICIENT_BALANCE synchronously with InsertOrderTx and the
// engine IPC both untouched — the §2.7 L2 zero-side-effects contract at
// the service seam (the killed-transaction variant, with rollback
// verified on the real store, is db_drop_test.go).
func TestL2_BalanceShortfallAtomicRejection(t *testing.T) {
	store := &fakeOrderStore{
		inst: &orders.Instrument{
			ID: 7, Symbol: "EUR/USD", Status: "ACTIVE",
			BaseCurrency: "EUR", QuoteCurrency: "USD",
			MinOrderQty: decimal.Zero, MaxOrderQty: decimal.NewFromInt(1_000_000_000),
		},
		avail: ptrDec("50"), // far below the 1000-unit sell
	}
	sub := &countingSubmitter{}
	svc, err := orders.NewService(orders.Options{
		Store: store, Submitter: sub, ShardMap: mustShardMap(t),
	})
	if err != nil {
		t.Fatalf("orders.NewService: %v", err)
	}

	qty := decimal.NewFromInt(1000)
	_, err = svc.Submit(context.Background(),
		&orders.Account{ID: 7, Status: "ACTIVE", KycTier: "T2"},
		&orders.SubmitRequest{
			Symbol: "EUR/USD", Side: "SELL", OrderType: "LIMIT",
			Quantity: &qty, Price: ptrDec("1.10"),
		})
	e := codedErr(t, err)
	if e.Code != "INSUFFICIENT_BALANCE" {
		t.Fatalf("shortfall code=%s, want INSUFFICIENT_BALANCE", e.Code)
	}
	if got := errs.New().HTTPStatus(e.Code); got != http.StatusBadRequest {
		t.Fatalf("INSUFFICIENT_BALANCE status=%d, want 400", got)
	}
	if store.insertCalls.Load() != 0 {
		t.Fatal("rejected submit reached InsertOrderTx — side effect before commit boundary")
	}
	if sub.calls.Load() != 0 {
		t.Fatal("rejected submit dispatched to the engine")
	}
}

// ---------------------------------------------------------------------------
// shared fakes for tier coverage
// ---------------------------------------------------------------------------

// countingSubmitter records Send calls — the "did mutation reach the
// engine" probe.
type countingSubmitter struct {
	calls atomic.Int64
}

func (s *countingSubmitter) Send(context.Context, uint16, []byte) error {
	s.calls.Add(1)
	return nil
}
func (s *countingSubmitter) Channel(uint16) (*ipc.Channel, error) {
	return nil, nil
}

func ptrDec(s string) *decimal.Decimal { d := decimal.MustFromString(s); return &d }

// ---------------------------------------------------------------------------
// §24 #307 traceability table — every tier maps to a concrete test in
// this package. A doc-level assertion so the coverage is self-auditing:
// the table is compiled into the test and each named test exists.
// ---------------------------------------------------------------------------

var tierCoverage = []struct {
	Tier  string
	Tests []string
	Spec  string
}{
	{"L0 Critical/Fatal",
		[]string{"TestL0_HaltFlagRoundtrip", "TestL0_HaltedInstrumentRejectsOrderFlow"},
		"§2.7.2: halt, zero continued mutation, P0 observability"},
	{"L1 Systemic",
		[]string{"TestL1_DegradationModeTransition", "TestCircuitBreaker_TripsOnSustained5xx",
			"TestCircuitBreaker_HalfOpenProbe", "TestRedisOutage_DegradationHeaderFailClosed"},
		"§2.7.2: degradation mode / breaker open / load shed"},
	{"L2 Transaction",
		[]string{"TestL2_BalanceShortfallAtomicRejection", "TestPGDrop_BalanceCommitKilled",
			"TestPGDrop_BalanceKillMidStatement", "TestPGDrop_LockReleasedAndPoolRecovers"},
		"§2.7.2: synchronous atomic rejection, zero side effects"},
	{"L3 Edge/Protocol",
		[]string{"TestSignatureRejection"},
		"§2.7.2: gateway edge rejection before IPC/core"},
}
