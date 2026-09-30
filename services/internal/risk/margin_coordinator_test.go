// margin_coordinator_test.go — unit + PG-gated integration coverage for
// the cross-shard margin coordinator (Phase-19 Task 19.3.11; spec §5.35,
// §13.1, §24 #176; migration 058).
//
// Unit legs run ungated against an in-memory ledger with an injected
// clock — the 500µs/10ms layered budget and TTL sweeps are exercised
// without real sleeps. Integration legs build a throwaway schema on the
// dev Postgres and apply the real migration files verbatim (same
// pattern as liquidation_store_test.go). Run:
//
//	EXC_PG_TEST=1 go test ./internal/risk/ -run 'TestPgShardMargin|TestMarginCoordinatorPg' -v
//
// (EXC_PG_DSN overrides the DSN; default is the docker-compose dev DB.)
package risk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/ipc"
	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes + helpers (unit tier)
// ---------------------------------------------------------------------------

// testClock is a deterministic wall clock; advance mutates it.
type testClock struct {
	t time.Time
}

func newTestClock() *testClock {
	return &testClock{t: time.Unix(1_700_000_000, 0).UTC()}
}
func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// fakeShardLedger is an in-memory ShardMarginLedger. beforeInsert lets a
// test burn coordinator clock inside the ledger call (driving the
// layered-budget paths without real sleeps).
type fakeShardLedger struct {
	mu           sync.Mutex
	rows         map[int64]ShardReservationRow
	insertErr    error
	statusErr    error
	beforeInsert func()
	inserts      int
}

func newFakeShardLedger() *fakeShardLedger {
	return &fakeShardLedger{rows: map[int64]ShardReservationRow{}}
}

func (f *fakeShardLedger) InsertReservation(_ context.Context, row ShardReservationRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.beforeInsert != nil {
		f.beforeInsert()
	}
	if f.insertErr != nil {
		return f.insertErr
	}
	f.inserts++
	if _, dup := f.rows[row.ReservationID]; dup {
		return fmt.Errorf("duplicate reservation_id %d", row.ReservationID)
	}
	f.rows[row.ReservationID] = row
	return nil
}

func (f *fakeShardLedger) SetStatus(_ context.Context, id int64,
	status ShardReservationStatus, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.statusErr != nil {
		return f.statusErr
	}
	r, ok := f.rows[id]
	if !ok {
		return fmt.Errorf("reservation %d: no row", id)
	}
	r.Status, r.ReleaseReason = status, reason
	f.rows[id] = r
	return nil
}

func (f *fakeShardLedger) ReservationByID(_ context.Context, id int64) (*ShardReservationRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.rows[id]; ok {
		cp := r
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeShardLedger) LoadActiveReservations(_ context.Context) ([]ShardReservationRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ShardReservationRow
	for _, r := range f.rows {
		if r.Status == ShardReservationPending || r.Status == ShardReservationCommitted {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeShardLedger) row(t *testing.T, id int64) ShardReservationRow {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rows[id]
	if !ok {
		t.Fatalf("ledger row %d missing", id)
	}
	return r
}

// fakeMarginSource is the AccountMarginSource test double.
type fakeMarginSource struct {
	books map[int64][2]decimal.Decimal // acct → {equity, margin}
	err   error
}

func (f *fakeMarginSource) AccountMargin(_ context.Context, id int64) (decimal.Decimal, decimal.Decimal, bool, error) {
	if f.err != nil {
		return decimal.Zero, decimal.Zero, false, f.err
	}
	b, ok := f.books[id]
	if !ok {
		return decimal.Zero, decimal.Zero, false, nil
	}
	return b[0], b[1], true, nil
}

// fakeAlerter captures ops alerts.
type fakeAlerter struct {
	mu     sync.Mutex
	alerts []OpsAlert
}

func (f *fakeAlerter) Raise(_ context.Context, a OpsAlert) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alerts = append(f.alerts, a)
	return nil
}
func (f *fakeAlerter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.alerts)
}

// coordTestCfg returns a deterministic config (4 shards, fake clock,
// wired source + alerter).
func coordTestCfg(clk *testClock, src AccountMarginSource, alerter OpsAlerter) MarginCoordinatorConfig {
	return MarginCoordinatorConfig{
		ShardCount:        4,
		ReservationTTL:    5 * time.Second,
		MaxOpenPerAccount: 10,
		RPCBudget:         500 * time.Microsecond,
		HardDeadline:      10 * time.Millisecond,
		LedgerTimeout:     250 * time.Millisecond,
		Now:               clk.now,
		Margins:           src,
		Alerter:           alerter,
	}
}

// resID builds an issuer-style id: high16 = shard, low48 = seq.
func resID(shard uint64, seq uint64) uint64 { return (shard << 48) | seq }

func reserveReq(id uint64, account int64, shard uint32, instr int64, amount string) ReserveRequest {
	return ReserveRequest{
		ReservationID: id,
		AccountID:     account,
		SrcShard:      shard,
		DstShard:      ipc.MarginCoordinatorPicks,
		InstrumentID:  instr,
		Amount:        d(amount),
	}
}

// ---------------------------------------------------------------------------
// Unit: construction + headroom arithmetic
// ---------------------------------------------------------------------------

func TestMarginCoordinatorNilLedger(t *testing.T) {
	clk := newTestClock()
	if _, err := NewMarginCoordinator(coordTestCfg(clk, nil, nil), nil); err == nil {
		t.Fatal("nil ledger must fail construction (fail-closed)")
	}
}

func TestMarginCoordinatorReserveGrantsAndHeadroom(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), d("40000")}, // equity 100k, margin 40k → headroom 60k
	}}
	led := newFakeShardLedger()
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	dec, err := c.Reserve(ctx, reserveReq(resID(1, 1), 100, 1, 7, "10000"))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if !dec.Granted || !dec.GrantedAmount.Equal(d("10000")) {
		t.Fatalf("decision: %+v", dec)
	}
	if dec.HostShard != 1 { // CoordinatorPicks → consumer hosts
		t.Fatalf("host shard: %d", dec.HostShard)
	}
	if dec.ExpiresAt.Sub(clk.t) != 5*time.Second {
		t.Fatalf("expiry: %s", dec.ExpiresAt)
	}
	h, ok, err := c.AvailableHeadroom(ctx, 100)
	if err != nil || !ok {
		t.Fatalf("headroom: %v %v", h, err)
	}
	if !h.Equal(d("50000")) { // 100k − 40k − 10k
		t.Fatalf("headroom %s, want 50000", h)
	}
	row := led.row(t, int64(dec.ReservationID))
	if row.Status != ShardReservationCommitted || row.GrantedAmount == nil ||
		!row.GrantedAmount.Equal(d("10000")) || row.ConsumerShard != 1 ||
		row.HostShard != 1 || row.InstrumentID != 7 {
		t.Fatalf("ledger row: %+v", row)
	}
	if st := c.Stats(); st.Acks != 1 || st.OpenReservations != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestMarginCoordinatorNackInsufficientHeadroom(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("10000"), d("4000")}, // headroom 6000
	}}
	led := newFakeShardLedger()
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	// Boundary: amount == headroom grants.
	dec, err := c.Reserve(ctx, reserveReq(resID(1, 1), 100, 1, 7, "6000"))
	if err != nil || !dec.Granted {
		t.Fatalf("boundary grant: %+v %v", dec, err)
	}
	// Beyond headroom → NACK InsufficientHeadroom + DENIED tombstone.
	dec, err = c.Reserve(ctx, reserveReq(resID(1, 2), 100, 1, 8, "1"))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if dec.Granted || dec.NackReason != ipc.MarginNackInsufficientHeadroom {
		t.Fatalf("want InsufficientHeadroom nack: %+v", dec)
	}
	row := led.row(t, int64(resID(1, 2)))
	if row.Status != ShardReservationDenied || row.GrantedAmount != nil ||
		row.ReleaseReason != "INSUFFICIENT_HEADROOM" {
		t.Fatalf("tombstone row: %+v", row)
	}
	// Replayed REQ for the denied id deterministically re-NACKs.
	dec2, err := c.Reserve(ctx, reserveReq(resID(1, 2), 100, 1, 8, "1"))
	if err != nil || dec2.Granted || !dec2.Replayed ||
		dec2.NackReason != ipc.MarginNackInsufficientHeadroom {
		t.Fatalf("replay: %+v %v", dec2, err)
	}
}

func TestMarginCoordinatorAggregateNeverExceedsEquity(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero}, // headroom 100k
	}}
	led := newFakeShardLedger()
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	// 4 engine shards race for 30k slices against 100k headroom.
	const shards = 4
	var wg sync.WaitGroup
	granted := make(chan decimal.Decimal, shards)
	for s := 0; s < shards; s++ {
		wg.Add(1)
		go func(shard uint32) {
			defer wg.Done()
			dec, err := c.Reserve(ctx, reserveReq(
				resID(uint64(shard)+1, 1), 100, shard, int64(shard)+10, "30000"))
			if err != nil {
				t.Errorf("reserve: %v", err)
				return
			}
			if dec.Granted {
				granted <- dec.GrantedAmount
			}
		}(uint32(s))
	}
	wg.Wait()
	close(granted)

	total := decimal.Zero
	n := 0
	for g := range granted {
		total = total.Add(g)
		n++
	}
	// Exactly 3 of 4 fit (3×30k=90k ≤ 100k); the 4th NACKs — atomic,
	// never over-allocated (§24 #176).
	if n != 3 || !total.Equal(d("90000")) {
		t.Fatalf("grants: n=%d total=%s", n, total)
	}
	st := c.Stats()
	if st.Acks != 3 || st.Nacks != 1 {
		t.Fatalf("stats: %+v", st)
	}
	h, _, _ := c.AvailableHeadroom(ctx, 100)
	if !h.Equal(d("10000")) {
		t.Fatalf("residual headroom %s, want 10000", h)
	}
}

func TestMarginCoordinatorReleaseRestoresHeadroom(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero},
	}}
	led := newFakeShardLedger()
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	id := resID(2, 1)
	dec, err := c.Reserve(ctx, reserveReq(id, 100, 2, 7, "50000"))
	if err != nil || !dec.Granted {
		t.Fatalf("reserve: %+v %v", dec, err)
	}
	// Cancel path: release restores the full headroom.
	found, err := c.Release(ctx, id, 100, ipc.MarginReleaseOrderRejected)
	if err != nil || !found {
		t.Fatalf("release: found=%v err=%v", found, err)
	}
	h, _, _ := c.AvailableHeadroom(ctx, 100)
	if !h.Equal(d("100000")) {
		t.Fatalf("headroom after release %s, want 100000", h)
	}
	if c.ReservationStatus(id) != ShardReservationReleased {
		t.Fatalf("status %q", c.ReservationStatus(id))
	}
	row := led.row(t, int64(id))
	if row.Status != ShardReservationReleased || row.ReleaseReason != "ORDER_REJECTED" {
		t.Fatalf("row: %+v", row)
	}
	// Idempotent: second release + unknown id.
	if found, _ := c.Release(ctx, id, 100, ipc.MarginReleaseComplete); !found {
		t.Fatal("re-release must be idempotent success")
	}
	if found, _ := c.Release(ctx, 0xDEADBEEF, 0, ipc.MarginReleaseComplete); found {
		t.Fatal("unknown id must report found=false")
	}
}

func TestMarginCoordinatorCommitConvertsToMaintenance(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero},
	}}
	led := newFakeShardLedger()
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	id := resID(3, 1)
	dec, err := c.Reserve(ctx, reserveReq(id, 100, 3, 7, "30000"))
	if err != nil || !dec.Granted {
		t.Fatalf("reserve: %+v %v", dec, err)
	}
	// Fill: the slice converts to committed maintenance margin — the
	// reservation unwinds AND margin rises by the same amount, so
	// headroom is conserved (never transiently inflated).
	if found, err := c.Commit(ctx, id, 100); err != nil || !found {
		t.Fatalf("commit: found=%v err=%v", found, err)
	}
	h, _, _ := c.AvailableHeadroom(ctx, 100)
	if !h.Equal(d("70000")) { // 100k − 30k MM − 0 reserved
		t.Fatalf("headroom after commit %s, want 70000", h)
	}
	row := led.row(t, int64(id))
	if row.Status != ShardReservationReleased || row.ReleaseReason != "COMPLETE" {
		t.Fatalf("row: %+v", row)
	}
	// The freed instrument slot lets a new REQ land.
	dec, err = c.Reserve(ctx, reserveReq(resID(3, 2), 100, 3, 7, "70000"))
	if err != nil || !dec.Granted {
		t.Fatalf("re-reserve: %+v %v", dec, err)
	}
}

func TestMarginCoordinatorPessimisticFloor(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000.00000001"), decimal.Zero},
	}}
	led := newFakeShardLedger()
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	floor, ok, err := c.PessimisticFloor(ctx, 100)
	if err != nil || !ok {
		t.Fatalf("floor: %s %v %v", floor, ok, err)
	}
	// floor × ShardCount must never exceed headroom (truncated division).
	if floor.Mul(decimal.NewFromInt(4)).GreaterThan(d("100000.00000001")) {
		t.Fatalf("floor %s breaches headroom", floor)
	}
	if !floor.Equal(d("25000.00000000")) {
		t.Fatalf("floor %s, want 25000.00000000", floor)
	}
	// Unknown account → ok=false, zero (fail closed).
	if floor, ok, err = c.PessimisticFloor(ctx, 999); err != nil || ok || !floor.IsZero() {
		t.Fatalf("unknown acct floor: %s %v %v", floor, ok, err)
	}
}

// ---------------------------------------------------------------------------
// Unit: layered budget (500µs / 10ms) via injected clock
// ---------------------------------------------------------------------------

func TestMarginCoordinatorHardDeadlineCompensates(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero},
	}}
	led := newFakeShardLedger()
	led.beforeInsert = func() { clk.advance(11 * time.Millisecond) } // >10ms
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	id := resID(1, 9)
	dec, err := c.Reserve(ctx, reserveReq(id, 100, 1, 7, "30000"))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// Granted then compensated: the slice is released before admission.
	if !dec.Granted || !dec.Compensated {
		t.Fatalf("decision: %+v", dec)
	}
	if c.ReservationStatus(id) != ShardReservationReleased {
		t.Fatalf("status %q, want RELEASED", c.ReservationStatus(id))
	}
	h, _, _ := c.AvailableHeadroom(ctx, 100)
	if !h.Equal(d("100000")) {
		t.Fatalf("headroom %s — compensation must restore it", h)
	}
	if st := c.Stats(); st.Compensations != 1 {
		t.Fatalf("stats: %+v", st)
	}
	row := led.row(t, int64(id))
	if row.Status != ShardReservationReleased || row.ReleaseReason != "TIMEOUT_COMPENSATE" {
		t.Fatalf("row: %+v", row)
	}
}

func TestMarginCoordinatorSoftBudgetCountsLate(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero},
	}}
	led := newFakeShardLedger()
	led.beforeInsert = func() { clk.advance(600 * time.Microsecond) } // >500µs, <10ms
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	id := resID(1, 1)
	dec, err := c.Reserve(ctx, reserveReq(id, 100, 1, 7, "30000"))
	if err != nil || !dec.Granted || dec.Compensated {
		t.Fatalf("decision: %+v %v", dec, err)
	}
	if st := c.Stats(); st.LateDecisions != 1 || st.Compensations != 0 {
		t.Fatalf("stats: %+v", st)
	}
}

// ---------------------------------------------------------------------------
// Unit: wire frames (Aeron bridge seam)
// ---------------------------------------------------------------------------

func encodeReqFrame(t *testing.T, req *ipc.MarginReserveReqBody) []byte {
	t.Helper()
	buf := make([]byte, ipc.MarginCtlMaxFrame)
	n := ipc.MarginCtlEncode(buf, ipc.MarginCtlReserveReq, req)
	if n == 0 {
		t.Fatal("encode REQ")
	}
	return buf[:n]
}

func TestMarginCoordinatorHandleFrameAck(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero},
	}}
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), newFakeShardLedger())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	req := &ipc.MarginReserveReqBody{
		ReservationID: resID(2, 1), AccountID: 100, OrderID: 55,
		SrcShard: 2, DstShard: ipc.MarginCoordinatorPicks,
		InstrumentID: 7, Amount: 5_000_000_000, // 50.0 in 1e8 ticks
	}
	frames, err := c.HandleFrame(ctx, 2, encodeReqFrame(t, req))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(frames) != 1 || frames[0].Shard != 2 {
		t.Fatalf("frames: %+v", frames)
	}
	var v ipc.MarginCtlView
	if rc := ipc.MarginCtlDecodeFrame(frames[0].Data, &v); rc != ipc.MarginCtlDecodeOK {
		t.Fatalf("decode: %s", rc)
	}
	if v.Type != ipc.MarginCtlReserveAck {
		t.Fatalf("frame type %d", v.Type)
	}
	if v.Ack.ReservationID != req.ReservationID || v.Ack.AccountID != 100 ||
		v.Ack.ShardID != 2 || v.Ack.GrantedAmount != req.Amount {
		t.Fatalf("ack body: %+v", v.Ack)
	}
	if v.Ack.ExpiresAtNs == 0 {
		t.Fatal("ack must carry authoritative expiry")
	}
}

func TestMarginCoordinatorHandleFrameNackAndRelease(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("1000"), decimal.Zero}, // headroom 1000
	}}
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), newFakeShardLedger())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	req := &ipc.MarginReserveReqBody{
		ReservationID: resID(3, 1), AccountID: 100,
		SrcShard: 3, DstShard: 9, InstrumentID: 7,
		Amount: 200_000_000_000, // 2000.0
	}
	frames, err := c.HandleFrame(ctx, 3, encodeReqFrame(t, req))
	if err != nil || len(frames) != 1 {
		t.Fatalf("frames: %v %+v", err, frames)
	}
	var v ipc.MarginCtlView
	if rc := ipc.MarginCtlDecodeFrame(frames[0].Data, &v); rc != ipc.MarginCtlDecodeOK ||
		v.Type != ipc.MarginCtlReserveNack ||
		v.Nack.Reason != uint32(ipc.MarginNackInsufficientHeadroom) ||
		v.Nack.ShardID != 9 {
		t.Fatalf("nack frame: rc=%v %+v", rc, v.Nack)
	}

	// Unknown/malformed frames are dropped with error, never answered.
	if frames, err := c.HandleFrame(ctx, 3, []byte{0, 1, 2}); err == nil || frames != nil {
		t.Fatalf("malformed frame: %v %+v", err, frames)
	}
	// REQ with amount ≤ 0 → dropped, counted.
	bad := &ipc.MarginReserveReqBody{ReservationID: resID(3, 2), AccountID: 100,
		SrcShard: 3, InstrumentID: 7, Amount: -5}
	if frames, err := c.HandleFrame(ctx, 3, encodeReqFrame(t, bad)); err == nil || frames != nil {
		t.Fatalf("bad REQ: %v %+v", err, frames)
	}
	// Inbound ACK/NACK are protocol anomalies for the coordinator.
	buf := make([]byte, ipc.MarginCtlMaxFrame)
	n := ipc.MarginCtlEncode(buf, ipc.MarginCtlReserveAck, &ipc.MarginReserveAckBody{
		ReservationID: 1, AccountID: 100, ShardID: 3, GrantedAmount: 1})
	if frames, err := c.HandleFrame(ctx, 3, buf[:n]); err != nil || frames != nil {
		t.Fatalf("inbound ack: %v %+v", err, frames)
	}
	if st := c.Stats(); st.BadFrames != 3 {
		t.Fatalf("bad frame count: %+v", st)
	}
}

func TestMarginCoordinatorHandleFrameRelease(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero},
	}}
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), newFakeShardLedger())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	id := resID(4, 1)
	req := &ipc.MarginReserveReqBody{
		ReservationID: id, AccountID: 100, SrcShard: 4,
		DstShard: ipc.MarginCoordinatorPicks, InstrumentID: 7,
		Amount: 5_000_000_000}
	if _, err := c.HandleFrame(ctx, 4, encodeReqFrame(t, req)); err != nil {
		t.Fatalf("req: %v", err)
	}
	buf := make([]byte, ipc.MarginCtlMaxFrame)
	n := ipc.MarginCtlEncode(buf, ipc.MarginCtlRelease, &ipc.MarginReleaseBody{
		ReservationID: id, AccountID: 100, ShardID: 4,
		Reason: uint8(ipc.MarginReleaseComplete)})
	frames, err := c.HandleFrame(ctx, 4, buf[:n])
	if err != nil || frames != nil {
		t.Fatalf("release: %v %+v", err, frames)
	}
	if c.ReservationStatus(id) != ShardReservationReleased {
		t.Fatalf("status %q", c.ReservationStatus(id))
	}
}

// ---------------------------------------------------------------------------
// Unit: expiry sweep, recovery, caps, ledger faults
// ---------------------------------------------------------------------------

func TestMarginCoordinatorExpirySweep(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero},
	}}
	led := newFakeShardLedger()
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	id := resID(5, 1)
	dec, err := c.Reserve(ctx, reserveReq(id, 100, 5, 7, "30000"))
	if err != nil || !dec.Granted {
		t.Fatalf("reserve: %+v %v", dec, err)
	}
	clk.advance(6 * time.Second) // past the 5s TTL
	frames := c.SweepExpired(ctx)
	if len(frames) != 1 || frames[0].Shard != 5 {
		t.Fatalf("sweep frames: %+v", frames)
	}
	var v ipc.MarginCtlView
	if rc := ipc.MarginCtlDecodeFrame(frames[0].Data, &v); rc != ipc.MarginCtlDecodeOK ||
		v.Type != ipc.MarginCtlRelease ||
		v.Rel.Reason != uint8(ipc.MarginReleaseExpired) {
		t.Fatalf("sweep frame: %+v", v)
	}
	if c.ReservationStatus(id) != ShardReservationReleased {
		t.Fatalf("status %q", c.ReservationStatus(id))
	}
	if st := c.Stats(); st.Expired != 1 {
		t.Fatalf("stats: %+v", st)
	}
	if row := led.row(t, int64(id)); row.Status != ShardReservationReleased ||
		row.ReleaseReason != "EXPIRED" {
		t.Fatalf("row: %+v", row)
	}
	// Sweep is idempotent — nothing left to expire.
	if frames := c.SweepExpired(ctx); len(frames) != 0 {
		t.Fatalf("re-sweep: %+v", frames)
	}
}

func TestMarginCoordinatorRecover(t *testing.T) {
	clk := newTestClock()
	led := newFakeShardLedger()
	liveExp := clk.t.Add(5 * time.Second)
	pastExp := clk.t.Add(-time.Second)
	g50 := d("50000")
	led.rows[int64(resID(7, 1))] = ShardReservationRow{
		ReservationID: int64(resID(7, 1)), AccountID: 100, ConsumerShard: 7,
		HostShard: 7, InstrumentID: 7, ReservedAmount: d("50000"),
		GrantedAmount: &g50, Status: ShardReservationCommitted,
		ExpiresAt: liveExp, CreatedAt: clk.t,
	}
	led.rows[int64(resID(8, 1))] = ShardReservationRow{
		ReservationID: int64(resID(8, 1)), AccountID: 100, ConsumerShard: 8,
		HostShard: 8, InstrumentID: 8, ReservedAmount: d("1000"),
		GrantedAmount: &g50, Status: ShardReservationCommitted,
		ExpiresAt: pastExp, CreatedAt: clk.t, // expired on load
	}
	led.rows[int64(resID(9, 1))] = ShardReservationRow{
		ReservationID: int64(resID(9, 1)), AccountID: 100, ConsumerShard: 9,
		HostShard: 9, InstrumentID: 9, ReservedAmount: d("2000"),
		Status:    ShardReservationPending, // never ACK'd — tombstone + cancel
		ExpiresAt: liveExp, CreatedAt: clk.t,
	}

	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero},
	}}
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	applied, frames, err := c.Recover(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if applied != 1 {
		t.Fatalf("applied %d, want 1 live row", applied)
	}
	if len(frames) != 2 { // expired COMMITTED + PENDING → RECOVERY_ORPHAN releases
		t.Fatalf("orphan frames: %+v", frames)
	}
	for _, f := range frames {
		var v ipc.MarginCtlView
		if rc := ipc.MarginCtlDecodeFrame(f.Data, &v); rc != ipc.MarginCtlDecodeOK ||
			v.Type != ipc.MarginCtlRelease ||
			v.Rel.Reason != uint8(ipc.MarginReleaseRecoveryOrphan) {
			t.Fatalf("orphan frame: %+v", v)
		}
	}
	// The live row counts against headroom again.
	h, ok, err := c.AvailableHeadroom(ctx, 100)
	if err != nil || !ok || !h.Equal(d("50000")) {
		t.Fatalf("headroom %s %v %v — recovered 50k not counted", h, ok, err)
	}
	// Tombstoned rows are terminal in the ledger.
	for _, id := range []uint64{resID(8, 1), resID(9, 1)} {
		if row := led.row(t, int64(id)); row.Status != ShardReservationReleased ||
			row.ReleaseReason != "RECOVERY_ORPHAN" {
			t.Fatalf("orphan row %d: %+v", id, row)
		}
	}
	// A replayed REQ for the recovered id re-ACKs the recorded grant.
	dec, err := c.Reserve(ctx, reserveReq(resID(7, 1), 100, 7, 7, "50000"))
	if err != nil || !dec.Granted || !dec.Replayed {
		t.Fatalf("replayed REQ: %+v %v", dec, err)
	}
}

func TestMarginCoordinatorAccountCapAndDuplicateInstrument(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("1000000"), decimal.Zero},
	}}
	led := newFakeShardLedger()
	cfg := coordTestCfg(clk, src, nil)
	cfg.MaxOpenPerAccount = 2
	c, err := NewMarginCoordinator(cfg, led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	if dec, _ := c.Reserve(ctx, reserveReq(resID(1, 1), 100, 1, 7, "10")); !dec.Granted {
		t.Fatal("first grant")
	}
	// Same (account, instrument) → NACK (mirrors the open_uq index).
	if dec, _ := c.Reserve(ctx, reserveReq(resID(1, 2), 100, 1, 7, "10")); dec.Granted ||
		dec.NackReason != ipc.MarginNackCoordinatorOverload {
		t.Fatalf("dup instrument: %+v", dec)
	}
	if dec, _ := c.Reserve(ctx, reserveReq(resID(1, 3), 100, 1, 8, "10")); !dec.Granted {
		t.Fatal("second grant (other instrument)")
	}
	// Cap reached: third open reservation → NACK CoordinatorOverload
	// (maps to CROSS_SHARD_LIMIT_EXCEEDED / 429 at the API layer).
	if dec, _ := c.Reserve(ctx, reserveReq(resID(1, 4), 100, 1, 9, "10")); dec.Granted ||
		dec.NackReason != ipc.MarginNackCoordinatorOverload {
		t.Fatalf("cap nack: %+v", dec)
	}
}

func TestMarginCoordinatorUnknownAccountNacks(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{}}
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), newFakeShardLedger())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	dec, err := c.Reserve(context.Background(),
		reserveReq(resID(1, 1), 424242, 1, 7, "10"))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if dec.Granted || dec.NackReason != ipc.MarginNackUnknownAccount {
		t.Fatalf("decision: %+v", dec)
	}
}

func TestMarginCoordinatorLedgerFaultFailsClosed(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{books: map[int64][2]decimal.Decimal{
		100: {d("100000"), decimal.Zero},
	}}
	led := newFakeShardLedger()
	led.insertErr = errors.New("pg down")
	led.statusErr = errors.New("pg down")
	alerts := &fakeAlerter{}
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, alerts), led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()

	id := resID(1, 1)
	dec, err := c.Reserve(ctx, reserveReq(id, 100, 1, 7, "30000"))
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	// The grant was revoked — a slice that is not durable never ACKs.
	if dec.Granted || dec.NackReason != ipc.MarginNackCoordinatorOverload {
		t.Fatalf("decision: %+v", dec)
	}
	h, _, _ := c.AvailableHeadroom(ctx, 100)
	if !h.Equal(d("100000")) {
		t.Fatalf("headroom %s — revoked grant must unwind", h)
	}
	if c.ReservationStatus(id) != ShardReservationDenied {
		t.Fatalf("tombstone %q", c.ReservationStatus(id))
	}
	if alerts.count() == 0 {
		t.Fatal("ledger fault must raise a P1 alert")
	}
	if st := c.Stats(); st.LedgerFaults == 0 {
		t.Fatalf("stats: %+v", st)
	}
	// Books stay consistent for subsequent grants once the ledger heals.
	led.insertErr, led.statusErr = nil, nil
	if dec, _ := c.Reserve(ctx, reserveReq(resID(1, 2), 100, 1, 8, "10")); !dec.Granted {
		t.Fatalf("post-heal reserve: %+v", dec)
	}
}

func TestMarginCoordinatorMarginSourceFailure(t *testing.T) {
	clk := newTestClock()
	src := &fakeMarginSource{err: errors.New("pg down")}
	c, err := NewMarginCoordinator(coordTestCfg(clk, src, nil), newFakeShardLedger())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if _, err := c.Reserve(context.Background(),
		reserveReq(resID(1, 1), 100, 1, 7, "10")); err == nil {
		t.Fatal("margin-source failure must surface an error")
	}
}

func TestMarginCoordinatorSetAccountMarginPush(t *testing.T) {
	clk := newTestClock()
	// No Margins source — push-only accounts.
	c, err := NewMarginCoordinator(coordTestCfg(clk, nil, nil), newFakeShardLedger())
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	ctx := context.Background()
	c.SetAccountMargin(100, d("8000"), d("2000"))
	dec, err := c.Reserve(ctx, reserveReq(resID(1, 1), 100, 1, 7, "6000"))
	if err != nil || !dec.Granted {
		t.Fatalf("reserve: %+v %v", dec, err)
	}
	// Unpushed account → unknown → NACK.
	if dec, _ := c.Reserve(ctx, reserveReq(resID(1, 2), 555, 1, 7, "1")); dec.Granted ||
		dec.NackReason != ipc.MarginNackUnknownAccount {
		t.Fatalf("unpushed acct: %+v", dec)
	}
	// Margin-buffer update shrinks headroom live:
	// 8000 equity − 1000 margin − 6000 reserved = 1000.
	c.SetAccountMargin(100, d("8000"), d("1000"))
	h, _, _ := c.AvailableHeadroom(ctx, 100)
	if !h.Equal(d("1000")) {
		t.Fatalf("headroom %s, want 1000", h)
	}
}

// ---------------------------------------------------------------------------
// Integration tier — EXC_PG_TEST=1, throwaway schema + real migrations
// ---------------------------------------------------------------------------

const mcoordTestDSN = "postgres://exchange:exchange_dev@127.0.0.1:5433/exchange?sslmode=disable"

func mcoordDSN() string {
	if d := os.Getenv("EXC_PG_DSN"); d != "" {
		return d
	}
	return mcoordTestDSN
}

// mcoordMigExec applies one migration file on a simple-protocol
// connection inside the scratch schema (pgx extended protocol cannot
// carry BEGIN..COMMIT multi-statement scripts).
func mcoordMigExec(t *testing.T, ctx context.Context, dsn, schema, file string) {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	cfg.RuntimeParams["search_path"] = schema + ",public"
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	defer conn.Close(ctx)
	sql, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("apply %s: %v", file, err)
	}
}

// mcoordFixture creates the scratch schema, applies the migration subset
// (users → accounts → margin_accounts → shard_margin_reservations), and
// returns the pool.
func mcoordFixture(t *testing.T) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("EXC_PG_TEST") != "1" {
		t.Skip("EXC_PG_TEST not set")
	}
	dsn := mcoordDSN()
	schema := fmt.Sprintf("mcoord_itest_%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	boot, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("postgres unreachable (%v)", err)
	}
	if err := boot.Ping(ctx); err != nil {
		boot.Close(ctx)
		t.Skipf("postgres unreachable (%v)", err)
	}
	if _, err := boot.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		boot.Close(ctx)
		t.Fatalf("create schema: %v", err)
	}
	boot.Close(ctx)
	t.Cleanup(func() {
		c2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel2()
		conn, err := pgx.Connect(c2, dsn)
		if err == nil {
			_, _ = conn.Exec(c2, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			conn.Close(c2)
		}
	})

	migDir := "../db/migrations"
	for _, m := range []string{
		"002_create_users.up.sql",
		"003_create_accounts.up.sql",
		"013_create_margin_accounts.up.sql",
		"058_shard_margin_reservations.up.sql",
	} {
		mcoordMigExec(t, ctx, dsn, schema, migDir+"/"+m)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func mcoordSeedAccount(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var uid, aid int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, status) VALUES ($1,'ACTIVE') RETURNING id`,
		fmt.Sprintf("mcoord_%d@example.com", time.Now().UnixNano())).Scan(&uid); err != nil {
		t.Fatalf("user: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO accounts (user_id, account_type)
		VALUES ($1,'MARGIN') RETURNING id`, uid).Scan(&aid); err != nil {
		t.Fatalf("account: %v", err)
	}
	return aid
}

func mcoordSeedMarginAccount(t *testing.T, pool *pgxpool.Pool,
	accountID int64, equity, used string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO margin_accounts (account_id, margin_mode, equity, used_margin, available_margin)
		VALUES ($1, 'PORTFOLIO', $2::numeric, $3::numeric, $2::numeric - $3::numeric)`,
		accountID, equity, used); err != nil {
		t.Fatalf("margin account: %v", err)
	}
}

func TestPgShardMarginLedgerCRUD(t *testing.T) {
	pool := mcoordFixture(t)
	ctx := context.Background()
	acct := mcoordSeedAccount(t, pool)
	led, err := NewPgShardMarginLedger(pool)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}

	id := int64(resID(3, 77))
	exp := time.Now().Add(5 * time.Second).UTC()
	granted := d("30000")
	row := ShardReservationRow{
		ReservationID: id, AccountID: acct, ConsumerShard: 3, HostShard: 3,
		InstrumentID: 7, OrderID: 42, ReservedAmount: d("30000"),
		GrantedAmount: &granted, Currency: "USD", ReqFlags: 1,
		Status: ShardReservationCommitted, ExpiresAt: exp,
	}
	if err := led.InsertReservation(ctx, row); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := led.ReservationByID(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("read: %v %+v", err, got)
	}
	if got.Status != ShardReservationCommitted || got.GrantedAmount == nil ||
		!got.GrantedAmount.Equal(d("30000")) || !got.ReservedAmount.Equal(d("30000")) ||
		got.ConsumerShard != 3 || got.HostShard != 3 || got.OrderID != 42 ||
		got.Currency != "USD" || got.ReqFlags != 1 {
		t.Fatalf("round-trip: %+v", got)
	}

	// Active set includes it; release flips it terminal.
	active, err := led.LoadActiveReservations(ctx)
	if err != nil || len(active) != 1 {
		t.Fatalf("active: %v %d", err, len(active))
	}
	if err := led.SetStatus(ctx, id, ShardReservationReleased, "EXPIRED"); err != nil {
		t.Fatalf("release: %v", err)
	}
	got, _ = led.ReservationByID(ctx, id)
	if got.Status != ShardReservationReleased || got.ReleaseReason != "EXPIRED" {
		t.Fatalf("released row: %+v", got)
	}
	var releasedAt *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT released_at FROM shard_margin_reservations WHERE reservation_id=$1`,
		id).Scan(&releasedAt); err != nil || releasedAt == nil {
		t.Fatalf("released_at: %v %v", releasedAt, err)
	}
	active, _ = led.LoadActiveReservations(ctx)
	if len(active) != 0 {
		t.Fatalf("released row still active: %+v", active)
	}
	// Missing row → error, never silent.
	if err := led.SetStatus(ctx, 4242424242, ShardReservationReleased, "X"); err == nil {
		t.Fatal("SetStatus on missing row must error")
	}
	if got, _ := led.ReservationByID(ctx, 4242424242); got != nil {
		t.Fatalf("phantom row: %+v", got)
	}
	// DENIED tombstone with NULL granted.
	if err := led.InsertReservation(ctx, ShardReservationRow{
		ReservationID: int64(resID(3, 78)), AccountID: acct,
		ConsumerShard: 3, HostShard: 3, InstrumentID: 8,
		ReservedAmount: d("5"), Status: ShardReservationDenied,
		ReleaseReason: "INSUFFICIENT_HEADROOM", ExpiresAt: exp,
	}); err != nil {
		t.Fatalf("denied insert: %v", err)
	}
}

func TestPgShardMarginLedgerUniqueOpen(t *testing.T) {
	pool := mcoordFixture(t)
	ctx := context.Background()
	acct := mcoordSeedAccount(t, pool)
	led, _ := NewPgShardMarginLedger(pool)
	exp := time.Now().Add(5 * time.Second).UTC()
	mk := func(id int64, status ShardReservationStatus) ShardReservationRow {
		return ShardReservationRow{
			ReservationID: id, AccountID: acct, ConsumerShard: 1, HostShard: 1,
			InstrumentID: 7, ReservedAmount: d("1"), Status: status,
			ExpiresAt: exp,
		}
	}
	if err := led.InsertReservation(ctx, mk(int64(resID(1, 1)), ShardReservationCommitted)); err != nil {
		t.Fatalf("first: %v", err)
	}
	// Second open row on (account, instrument) violates the unique index.
	if err := led.InsertReservation(ctx, mk(int64(resID(1, 2)), ShardReservationPending)); err == nil {
		t.Fatal("open-(account,instrument) uniqueness must be enforced")
	}
	// A terminal row does not collide — DENIED tombstone coexists.
	if err := led.InsertReservation(ctx, mk(int64(resID(1, 3)), ShardReservationDenied)); err != nil {
		t.Fatalf("denied tombstone must not collide: %v", err)
	}
	// Release the open row → a fresh reservation for the pair succeeds.
	if err := led.SetStatus(ctx, int64(resID(1, 1)), ShardReservationReleased, "COMPLETE"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := led.InsertReservation(ctx, mk(int64(resID(1, 4)), ShardReservationCommitted)); err != nil {
		t.Fatalf("re-reserve after release: %v", err)
	}
}

func TestMarginCoordinatorPgEndToEnd(t *testing.T) {
	pool := mcoordFixture(t)
	ctx := context.Background()
	acct := mcoordSeedAccount(t, pool)
	mcoordSeedMarginAccount(t, pool, acct, "100000", "40000")

	led, err := NewPgShardMarginLedger(pool)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	clk := newTestClock()
	cfg := coordTestCfg(clk, led, nil) // PgShardMarginLedger is also the source
	c, err := NewMarginCoordinator(cfg, led)
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}

	// Wire-level REQ → ACK, durable row COMMITTED.
	req := &ipc.MarginReserveReqBody{
		ReservationID: resID(1, 1), AccountID: uint64(acct), OrderID: 9,
		SrcShard: 1, DstShard: ipc.MarginCoordinatorPicks,
		InstrumentID: 7, Amount: 1_000_000_000_000} // 10000.0 ticks
	frames, err := c.HandleFrame(ctx, 1, encodeReqFrame(t, req))
	if err != nil || len(frames) != 1 {
		t.Fatalf("req: %v %+v", err, frames)
	}
	var v ipc.MarginCtlView
	if rc := ipc.MarginCtlDecodeFrame(frames[0].Data, &v); rc != ipc.MarginCtlDecodeOK ||
		v.Type != ipc.MarginCtlReserveAck || v.Ack.GrantedAmount != req.Amount {
		t.Fatalf("ack: %+v", v)
	}
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status::text FROM shard_margin_reservations WHERE reservation_id=$1`,
		int64(req.ReservationID)).Scan(&status); err != nil || status != "COMMITTED" {
		t.Fatalf("row: %q %v", status, err)
	}
	// Headroom: 100k − 40k − 10k = 50k.
	h, ok, err := c.AvailableHeadroom(ctx, acct)
	if err != nil || !ok || !h.Equal(d("50000")) {
		t.Fatalf("headroom %s %v %v", h, ok, err)
	}

	// Restart: a fresh coordinator on the same pool must reload the
	// committed slice (coordinator-restart-during-active edge case).
	c2, err := NewMarginCoordinator(coordTestCfg(clk, led, nil), led)
	if err != nil {
		t.Fatalf("ctor2: %v", err)
	}
	applied, frames2, err := c2.Recover(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if applied != 1 || len(frames2) != 0 {
		t.Fatalf("recover: applied=%d frames=%v", applied, frames2)
	}
	h2, ok, _ := c2.AvailableHeadroom(ctx, acct)
	if !ok || !h2.Equal(d("50000")) {
		t.Fatalf("post-recover headroom %s %v", h2, ok)
	}

	// Engine release → row RELEASED; headroom restored on the live coord.
	buf := make([]byte, ipc.MarginCtlMaxFrame)
	n := ipc.MarginCtlEncode(buf, ipc.MarginCtlRelease, &ipc.MarginReleaseBody{
		ReservationID: req.ReservationID, AccountID: uint64(acct), ShardID: 1,
		Reason: uint8(ipc.MarginReleaseComplete)})
	if _, err := c2.HandleFrame(ctx, 1, buf[:n]); err != nil {
		t.Fatalf("release: %v", err)
	}
	h3, ok, _ := c2.AvailableHeadroom(ctx, acct)
	// Commit semantics: fill converts the slice to MM — headroom stays
	// 50k (margin absorbed the slice).
	if !ok || !h3.Equal(d("50000")) {
		t.Fatalf("post-commit headroom %s %v", h3, ok)
	}
	if err := pool.QueryRow(ctx,
		`SELECT status::text, COALESCE(release_reason,'') FROM shard_margin_reservations
		 WHERE reservation_id=$1`, int64(req.ReservationID)).
		Scan(&status, new(string)); err != nil || status != "RELEASED" {
		t.Fatalf("released row: %q %v", status, err)
	}

	// PK-replay across restarts: a COMMITTED row exists in PG but the
	// (fresh) coordinator has no in-memory record — the REQ re-answers
	// the recorded decision instead of double-granting.
	c3, err := NewMarginCoordinator(coordTestCfg(clk, led, nil), led)
	if err != nil {
		t.Fatalf("ctor3: %v", err)
	}
	replayID := int64(resID(1, 90))
	if _, err := pool.Exec(ctx, `
		INSERT INTO shard_margin_reservations
		    (reservation_id, account_id, consumer_shard, host_shard, instrument_id,
		     reserved_amount, granted_amount, currency, status, expires_at)
		VALUES ($1,$2,1,1,11,'25000','25000','USD','COMMITTED', now() + interval '5s')`,
		replayID, acct); err != nil {
		t.Fatalf("seed committed row: %v", err)
	}
	dec, err := c3.Reserve(ctx, reserveReq(uint64(replayID), acct, 1, 11, "25000"))
	if err != nil {
		t.Fatalf("pk replay reserve: %v", err)
	}
	if !dec.Granted || !dec.Replayed || !dec.GrantedAmount.Equal(d("25000")) {
		t.Fatalf("pk replay decision: %+v", dec)
	}
	// The in-memory phantom grant was unwound — no double-debit of the
	// account book beyond the recorded row.
}
