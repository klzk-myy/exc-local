// Task 8.3.5 scenario 1 — gateway behaviour while the matching engine is
// paused or absent (spec §2.7.3 IPC resilience + §8.7 item 4).
//
// Three real fault seams are exercised, no external services required:
//
//  1. Engine absent: the shm endpoint lazily creates the ring, so a
//     send lands in a buffer nobody drains; the fail-closed contract is
//     that ProducerAlive() reports the missing peer (no fabricated
//     liveness) while the ack timeout below bounds the client-visible
//     latency.
//  2. Engine paused / ingress stalled: a live ring whose consumer never
//     drains fills to capacity; the submitter then fails closed with
//     ENGINE_OVERLOAD (503) — the §2.7.3 backpressure contract —
//     counted in the ring's Drops() counter.
//  3. Engine swallowing acks: a submitted cancel that receives no outbound
//     echo within EngineAckTimeout (500ms) surfaces as
//     GATEWAY_TIMEOUT_MATCHING_ENGINE (504), and the REST-facing
//     EngineTimeout middleware caps a stalled handler at the same
//     budget with the same coded envelope — requests never hang.
package error_scenarios

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"exchange/internal/config"
	"exchange/internal/errs"
	"exchange/internal/gateway"
	"exchange/internal/ipc"
	"exchange/internal/orders"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// ---------------------------------------------------------------------------
// fakes — orders.Store + orders.Submitter seams
// ---------------------------------------------------------------------------

// fakeOrderStore is the minimal Store backing a RESTING order so the
// service's real cancel dispatch path (Send → await out-ring echo)
// runs end to end. inst/avail feed the Submit-path tiers tests;
// insertCalls proves a rejected order never reached persistence.
type fakeOrderStore struct {
	order       *orders.Order
	inst        *orders.Instrument
	avail       *decimal.Decimal
	insertCalls atomic.Int64
}

func (f *fakeOrderStore) InstrumentBySymbol(_ context.Context, sym string) (*orders.Instrument, error) {
	if f.inst != nil && f.inst.Symbol == sym {
		return f.inst, nil
	}
	return nil, nil
}
func (f *fakeOrderStore) InstrumentByID(context.Context, int64) (*orders.Instrument, error) {
	return nil, nil
}
func (f *fakeOrderStore) AccountByID(_ context.Context, id int64) (*orders.Account, error) {
	return &orders.Account{ID: id, Status: "ACTIVE"}, nil
}
func (f *fakeOrderStore) ReferencePrice(context.Context, int64) (*decimal.Decimal, error) {
	return nil, nil
}
func (f *fakeOrderStore) AvailableBalance(context.Context, int64, string) (*decimal.Decimal, error) {
	return f.avail, nil
}
func (f *fakeOrderStore) DedupLookup(context.Context, int64, string) (*orders.DedupRow, error) {
	return nil, nil
}
func (f *fakeOrderStore) InsertOrderTx(context.Context, orders.InsertParams) (*orders.Order, *orders.DedupRow, error) {
	f.insertCalls.Add(1)
	return nil, nil, fmt.Errorf("fake store: InsertOrderTx not implemented")
}
func (f *fakeOrderStore) GetOrder(_ context.Context, id int64) (*orders.Order, error) {
	if f.order != nil && f.order.ID == id {
		return f.order, nil
	}
	return nil, nil
}
func (f *fakeOrderStore) ListOrders(context.Context, orders.ListQuery) ([]orders.Order, int64, error) {
	return nil, 0, nil
}
func (f *fakeOrderStore) OpenOrders(context.Context, orders.MassCancelScope) ([]orders.Order, error) {
	return nil, nil
}
func (f *fakeOrderStore) ApplyCancel(_ context.Context, id int64) error {
	if f.order != nil && f.order.ID == id {
		f.order.Status = "CANCELLED"
	}
	return nil
}
func (f *fakeOrderStore) ApplyFill(context.Context, int64, decimal.Decimal, decimal.Decimal) error {
	return nil
}
func (f *fakeOrderStore) MarkActive(context.Context, int64) error   { return nil }
func (f *fakeOrderStore) MarkRejected(context.Context, int64) error { return nil }
func (f *fakeOrderStore) AmendCAS(context.Context, int64, uint64, uint64,
	orders.AmendFields, []orders.AuditEntry) (*orders.Order, bool, error) {
	return nil, false, fmt.Errorf("fake store: AmendCAS not implemented")
}
func (f *fakeOrderStore) RevertAmend(context.Context, *orders.Order) error { return nil }
func (f *fakeOrderStore) WriteAudit(context.Context, []orders.AuditEntry) error {
	return nil
}
func (f *fakeOrderStore) AuditTrail(context.Context, int64) ([]orders.AuditEntry, error) {
	return nil, nil
}
func (f *fakeOrderStore) Amendments(context.Context, int64) ([]orders.AuditEntry, error) {
	return nil, nil
}
func (f *fakeOrderStore) IdemLookup(context.Context, int64, string) (*orders.IdemRow, error) {
	return nil, nil
}
func (f *fakeOrderStore) IdemStore(context.Context, int64, string, string, string, int, []byte) error {
	return nil
}

// silentSubmitter accepts every frame and never produces an outbound
// echo — the "engine alive but not answering" fault.
type silentSubmitter struct {
	sent int
}

func (s *silentSubmitter) Send(context.Context, uint16, []byte) error {
	s.sent++
	return nil
}
func (s *silentSubmitter) Channel(uint16) (*ipc.Channel, error) {
	return nil, fmt.Errorf("silentSubmitter has no channel")
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// codedErr unwraps the *excerrors.Error the service emits.
func codedErr(t *testing.T, err error) *excerrors.Error {
	t.Helper()
	var e *excerrors.Error
	if !stderrors.As(err, &e) {
		t.Fatalf("error %v is not a coded *errors.Error", err)
	}
	return e
}

// shmBase returns a unique shm ring base for this test and removes the
// images on cleanup.
func shmBase(t *testing.T, shard uint16) string {
	t.Helper()
	base := fmt.Sprintf("errscen_%s_%d", t.Name(), os.Getpid())
	t.Cleanup(func() {
		_ = os.Remove("/dev/shm/" + ipc.InName(base, shard))
		_ = os.Remove("/dev/shm/" + ipc.OutName(base, shard))
	})
	return base
}

// ---------------------------------------------------------------------------
// Scenario 1a — engine absent: the submitter lazily creates the shm ring
// (OpenRing uses O_CREATE for either endpoint — whichever arrives first
// initializes), so Send succeeds into the void. The fail-closed contract
// is OBSERVABILITY, not a write error: ProducerAlive() must report the
// absent peer (pid never stamped) rather than silently implying health,
// and the client-facing bound on an absent engine is the ack timeout in
// scenario 1c — requests never hang and never fabricate an ack.
// ---------------------------------------------------------------------------

func TestEngineAbsent_PeerLivenessFailsClosed(t *testing.T) {
	const shard = 0
	sub := orders.NewShmSubmitter(shmBase(t, shard))

	// Writes land in the lazily-created ring — the IPC layer does not
	// refuse them; absence is detected at the liveness seam.
	if err := sub.Send(context.Background(), shard, []byte("order-new")); err != nil {
		t.Fatalf("send into fresh ring failed: %v", err)
	}
	ch, err := sub.Channel(shard)
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if ch.ProducerAlive() {
		t.Fatal("ProducerAlive reports a peer that never attached — ambiguous liveness is a fail-open leak")
	}
	// The write is buffered, unacknowledged — inbound occupancy shows
	// the absent engine never answered.
	if ch.Occupancy() != 0 {
		// Occupancy() on the gateway side counts inbound (out-ring)
		// messages — must stay 0 while the engine never writes.
		t.Fatalf("phantom engine output: occupancy=%d with no peer", ch.Occupancy())
	}
}

// ---------------------------------------------------------------------------
// Scenario 1b — engine paused: ingress ring fills ⇒ ENGINE_OVERLOAD,
//               no frame dropped silently (Drops counter grows)
// ---------------------------------------------------------------------------

func TestEnginePaused_IngressRingStall(t *testing.T) {
	const shard = 0
	const capacity = 8 // small ring so the stall is cheap to induce
	base := shmBase(t, shard)

	// The "engine": creates the images but never consumes — the pause.
	core, err := ipc.OpenChannel(base, shard, ipc.EndpointCore, true,
		capacity, ipc.DefaultRingSlotPayload)
	if err != nil {
		t.Fatalf("open core side: %v", err)
	}
	defer core.Close()

	sub := orders.NewShmSubmitter(base)
	payload := make([]byte, 64)

	// Drain-free writes succeed until the ring is full.
	var firstErr error
	for i := 0; i < int(capacity); i++ {
		if err := sub.Send(context.Background(), shard, payload); err != nil {
			firstErr = err
			break
		}
	}
	if firstErr != nil {
		t.Fatalf("send into non-full ring failed early: %v", firstErr)
	}

	// The next send MUST fail closed with ENGINE_OVERLOAD (spec §2.7.3).
	err = sub.Send(context.Background(), shard, payload)
	e := codedErr(t, err)
	if e.Code != "ENGINE_OVERLOAD" {
		t.Fatalf("stalled-ring send code=%s, want ENGINE_OVERLOAD", e.Code)
	}
	if got := errs.New().HTTPStatus(e.Code); got != http.StatusServiceUnavailable {
		t.Fatalf("ENGINE_OVERLOAD status=%d, want 503", got)
	}

	// And the overload is observable at the transport seam, not just the
	// error path: the producer's drop counter records the refused write.
	ch, err := sub.Channel(shard)
	if err != nil {
		t.Fatalf("channel: %v", err)
	}
	if ch.Drops() == 0 {
		t.Fatal("ring Drops() did not record the refused frame")
	}
}

// ---------------------------------------------------------------------------
// Scenario 1c — engine swallows acks: cancel echo never arrives ⇒
//               GATEWAY_TIMEOUT_MATCHING_ENGINE within EngineAckTimeout
// ---------------------------------------------------------------------------

func TestEnginePaused_CancelAckTimeout(t *testing.T) {
	const ackBudget = 200 * time.Millisecond // shortened; contract is the deadline
	store := &fakeOrderStore{order: &orders.Order{
		ID: 42, AccountID: 7, InstrumentID: 1, Status: "ACTIVE", OrderSeq: 1,
	}}
	svc, err := orders.NewService(orders.Options{
		KillSwitch: openKillSwitch{}, Breakers: openBreakers{},
		Store:      store,
		Submitter:  &silentSubmitter{},
		ShardMap:   mustShardMap(t),
		AckTimeout: ackBudget,
	})
	if err != nil {
		t.Fatalf("orders.NewService: %v", err)
	}

	start := time.Now()
	_, err = svc.Cancel(context.Background(), &orders.Account{ID: 7}, 42,
		"test", "req-1", "127.0.0.1")
	elapsed := time.Since(start)

	e := codedErr(t, err)
	if e.Code != "GATEWAY_TIMEOUT_MATCHING_ENGINE" {
		t.Fatalf("cancel ack timeout code=%s, want GATEWAY_TIMEOUT_MATCHING_ENGINE", e.Code)
	}
	if got := errs.New().HTTPStatus(e.Code); got != http.StatusGatewayTimeout {
		t.Fatalf("GATEWAY_TIMEOUT_MATCHING_ENGINE status=%d, want 504", got)
	}
	if elapsed < ackBudget || elapsed > 3*ackBudget {
		t.Fatalf("cancel returned after %s; want ~%s (bounded wait)", elapsed, ackBudget)
	}
	// Fail-closed means the read model must NOT show the cancel as applied:
	// the engine never confirmed, so the order stays ACTIVE.
	if store.order.Status != "ACTIVE" {
		t.Fatalf("order status=%s after unconfirmed cancel, want ACTIVE", store.order.Status)
	}
}

// mustShardMap loads the repo's config/sharding.yaml — the service
// requires a non-nil map but this scenario never consults it (cancels
// route on the order's recorded shard).
func mustShardMap(t *testing.T) *config.ShardMap {
	t.Helper()
	m, err := config.LoadShardMap("")
	if err != nil {
		t.Fatalf("load shard map: %v", err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Scenario 1d — REST surface: EngineTimeout middleware caps a stalled
//               order handler at 500ms with the §8.7 envelope
// ---------------------------------------------------------------------------

func TestEnginePaused_RESTGatewayTimeoutEnvelope(t *testing.T) {
	r := newTestRouter()

	// Handler mimics a synchronous await on a paused engine: it writes
	// success only after far longer than the budget.
	stalled := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		time.Sleep(2 * gateway.EngineTimeoutBudget)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"order_id":1}`))
	})
	r.Mux().Handle("POST /api/v1/orders", r.EngineTimeout(stalled))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/orders", nil)

	start := time.Now()
	r.Mux().ServeHTTP(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status=%d, want 504; body=%s", rec.Code, rec.Body.String())
	}
	p := decodeProblem(t, rec)
	if p.Error != "GATEWAY_TIMEOUT_MATCHING_ENGINE" {
		t.Fatalf("envelope code=%s, want GATEWAY_TIMEOUT_MATCHING_ENGINE", p.Error)
	}
	if p.Status != http.StatusGatewayTimeout {
		t.Fatalf("envelope status=%d, want 504", p.Status)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("content-type=%q, want application/problem+json", ct)
	}
	// The budget is the contract: the client answer lands at ~500ms even
	// though the handler is still asleep.
	if elapsed < gateway.EngineTimeoutBudget ||
		elapsed > gateway.EngineTimeoutBudget+2*time.Second {
		t.Fatalf("timeout fired after %s, want ~%s", elapsed, gateway.EngineTimeoutBudget)
	}
}

// openKillSwitch reports trading open for every scope — the Phase-11
// admission seam wired to a permanently-clear resolver so these tests
// exercise the pre-suspension error contracts they were written for.
type openKillSwitch struct{}

func (openKillSwitch) OrderHalt(context.Context, int64, string, string, string) (string, string, error) {
	return "", "", nil
}

// openBreakers is the always-admit Phase-13 circuit-breaker fake — the
// admission seam fails closed when nil, so these tests (which predate the
// seam and assert pre-breaker error contracts) wire a permanently-open gate.
type openBreakers struct{}

func (openBreakers) AdmitOrder(context.Context, int64, string) error { return nil }
