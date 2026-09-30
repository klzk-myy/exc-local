// margin_level_test.go — coverage for margin_level.go +
// margin_level_reader.go (Phase-19 Task 19.3.16; spec §13.3/§13.6d,
// §24 #163).
//
// Unit legs cover the MarginLevel threshold helpers (ESMA 120/100/50
// semantics), the per-account threshold store guards, the REST view
// composer, and the change-feed watcher. Gated legs cover the
// RedisMarginLevelReader hash contract (EXC_REDIS_TEST=1, db 14) and
// PgMarginThresholdStore round-trips (EXC_PG_TEST=1).
//
//	EXC_PG_TEST=1    go test ./internal/risk/ -run 'TestPgMarginThreshold' -v
//	EXC_REDIS_TEST=1 go test ./internal/risk/ -run 'TestRedisMarginLevel' -v
package risk

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

// ---------------------------------------------------------------------------
// Fakes (namespaced; shared fakes — staticLevels, d(), requireCode — live
// in sibling test files of the same package)
// ---------------------------------------------------------------------------

type mgnThresholdWrite struct {
	acct   int64
	th     MarginThresholds
	before *MarginThresholds
	userID int64
}

// marginThresholdStoreFake implements MarginThresholdStore in memory.
type marginThresholdStoreFake struct {
	cat      string
	catErr   error
	override *MarginThresholds
	ovErr    error
	setCalls []mgnThresholdWrite
	setErr   error
}

func (f *marginThresholdStoreFake) ThresholdsFor(context.Context, int64) (*MarginThresholds, error) {
	return f.override, f.ovErr
}
func (f *marginThresholdStoreFake) ClientCategory(context.Context, int64) (string, error) {
	if f.catErr != nil {
		return "", f.catErr
	}
	if f.cat == "" {
		return CategoryRetail, nil
	}
	return f.cat, nil
}
func (f *marginThresholdStoreFake) SetThresholds(_ context.Context, acct int64,
	th MarginThresholds, before *MarginThresholds, userID int64) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.setCalls = append(f.setCalls, mgnThresholdWrite{acct, th, before, userID})
	return nil
}

var _ MarginThresholdStore = (*marginThresholdStoreFake)(nil)

// marginLevelErrReader is the failing MarginLevelReader.
type marginLevelErrReader struct{ err error }

func (r marginLevelErrReader) MarginLevel(context.Context, int64) (*MarginLevel, error) {
	return nil, r.err
}

var _ MarginLevelReader = (*marginLevelErrReader)(nil)

// marginLevelListerFake implements MarginLevelHashLister.
type marginLevelListerFake struct {
	keys []string
	err  error
}

func (f marginLevelListerFake) ScanKeys(context.Context, string) ([]string, error) {
	return f.keys, f.err
}

// marginLevelPublishSpy records MarginLevelPublisher invocations.
type marginLevelPublishSpy struct {
	calls []struct {
		acct      int64
		channel   string
		eventType string
		payload   any
	}
	err error
}

func (p *marginLevelPublishSpy) fn() MarginLevelPublisher {
	return func(_ context.Context, acct int64, channel, eventType string, payload any) error {
		p.calls = append(p.calls, struct {
			acct      int64
			channel   string
			eventType string
			payload   any
		}{acct, channel, eventType, payload})
		return p.err
	}
}

// ---------------------------------------------------------------------------
// MarginLevel helpers — §13.3/§13.6d thresholds
// ---------------------------------------------------------------------------

func TestMarginLevelThresholdHelpers(t *testing.T) {
	var nilLevel *MarginLevel
	if nilLevel.IsMarginCallLevel(d("111.1")) || nilLevel.IsStopOutLevel(d("50")) {
		t.Fatal("nil level must read false on both helpers")
	}
	// Zero used margin ⇒ no margin in use ⇒ never a breach.
	lv := &MarginLevel{AccountID: 7, UsedMargin: decimal.Zero, MarginLevelPct: d("10")}
	if lv.IsMarginCallLevel(d("111.1")) || lv.IsStopOutLevel(d("50")) {
		t.Fatal("zero used margin must not trip the helpers")
	}
	// Canonical §13.3 margin-call trigger 111.1%, inclusive boundary.
	lv.UsedMargin = d("100")
	lv.MarginLevelPct = d("111.1")
	if !lv.IsMarginCallLevel(d("111.1")) {
		t.Fatal("111.1 must be at-or-below the 111.1 trigger")
	}
	if lv.IsMarginCallLevel(d("110")) {
		t.Fatal("111.1 > 110 must not trip")
	}
	// ESMA retail tiers: call 100 / stop-out 50.
	lv.MarginLevelPct = d("100")
	if !lv.IsMarginCallLevel(d("100")) {
		t.Fatal("100 must trip the ESMA margin-call tier")
	}
	lv.MarginLevelPct = d("50")
	if !lv.IsStopOutLevel(d("50")) {
		t.Fatal("50 must trip the retail stop-out (inclusive)")
	}
	lv.MarginLevelPct = d("50.0001")
	if lv.IsStopOutLevel(d("50")) {
		t.Fatal("50.0001 > 50 must not trip stop-out")
	}
}

func TestMarginThresholdFloorMirrorsCategory(t *testing.T) {
	for _, cat := range []string{"RETAIL", "PROFESSIONAL", "ELIGIBLE_COUNTERPARTY", "GARBAGE"} {
		got, want := ThresholdFloor(cat), ThresholdsFor(cat)
		if !got.Warning.Equal(want.Warning) || !got.Call.Equal(want.Call) ||
			!got.StopOut.Equal(want.StopOut) {
			t.Fatalf("floor %q = %+v, want %+v", cat, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// MarginThresholdService — resolve + guarded set
// ---------------------------------------------------------------------------

func TestMarginThresholdServiceResolve(t *testing.T) {
	ctx := context.Background()
	f := &marginThresholdStoreFake{}
	svc := NewMarginThresholdService(f)

	// No override → category default.
	th, err := svc.Thresholds(ctx, 7)
	if err != nil || !th.Warning.Equal(d("120")) || !th.Call.Equal(d("100")) ||
		!th.StopOut.Equal(d("50")) {
		t.Fatalf("retail default: %+v %v", th, err)
	}
	f.cat = CategoryProfessional
	th, err = svc.Thresholds(ctx, 7)
	if err != nil || !th.Warning.Equal(d("100")) || !th.Call.Equal(d("80")) ||
		!th.StopOut.Equal(d("30")) {
		t.Fatalf("professional default: %+v %v", th, err)
	}
	// Override row wins outright.
	f.override = &MarginThresholds{Warning: d("200"), Call: d("150"), StopOut: d("90")}
	th, err = svc.Thresholds(ctx, 7)
	if err != nil || !th.Call.Equal(d("150")) {
		t.Fatalf("override: %+v %v", th, err)
	}
	// Store errors propagate (never silently default — a stricter
	// override could otherwise be lost).
	f.ovErr = fmt.Errorf("pg down")
	if _, err := svc.Thresholds(ctx, 7); err == nil {
		t.Fatal("override read error must propagate")
	}
	f.ovErr = nil
	f.catErr = fmt.Errorf("pg down")
	if _, err := svc.Thresholds(ctx, 7); err == nil {
		t.Fatal("category read error must propagate")
	}
}

func TestMarginThresholdServiceSetGuards(t *testing.T) {
	ctx := context.Background()
	f := &marginThresholdStoreFake{cat: CategoryRetail}
	svc := NewMarginThresholdService(f)
	th := func(w, c, s string) MarginThresholds {
		return MarginThresholds{Warning: d(w), Call: d(c), StopOut: d(s)}
	}

	// Shape guards: positive, ≤1000, strictly ordered.
	for _, bad := range []MarginThresholds{
		th("0", "100", "50"),    // non-positive warning
		th("120", "0", "50"),    // non-positive call
		th("120", "100", "0"),   // non-positive stop-out
		th("1001", "100", "50"), // sanity bound
		th("100", "100", "50"),  // warning == call (not strict)
		th("120", "50", "50"),   // call == stop-out
		th("120", "100", "150"), // inverted ordering
	} {
		if _, err := svc.Set(ctx, 7, bad, 42); err == nil {
			t.Fatalf("bad thresholds accepted: %+v", bad)
		} else {
			requireCode(t, err, CodeLeverageInvalid)
		}
	}
	if len(f.setCalls) != 0 {
		t.Fatal("rejected sets must never reach the store")
	}

	// Retail floor (ESMA 120/100/50): any weakening is regulatory.
	for _, bad := range []MarginThresholds{
		th("119", "105", "60"), // warning below floor
		th("150", "99", "60"),  // call below floor
		th("150", "120", "45"), // stop-out below floor
	} {
		if _, err := svc.Set(ctx, 7, bad, 42); err == nil {
			t.Fatalf("floor breach accepted: %+v", bad)
		} else {
			requireCode(t, err, "PRODUCT_NOT_PERMITTED")
		}
	}
	// Stricter-than-floor is permitted (floor is a lower bound).
	if _, err := svc.Set(ctx, 7, th("150", "110", "60"), 42); err != nil {
		t.Fatalf("stricter retail set must pass: %v", err)
	}
	if len(f.setCalls) != 1 || f.setCalls[0].userID != 42 ||
		f.setCalls[0].before != nil || !f.setCalls[0].th.StopOut.Equal(d("60")) {
		t.Fatalf("recorded write: %+v", f.setCalls)
	}

	// Professional floor is the §13.6d pro tier (100/80/30).
	f.cat = CategoryProfessional
	if _, err := svc.Set(ctx, 9, th("95", "75", "20"), 42); err == nil {
		t.Fatal("below pro floor must be rejected")
	} else {
		requireCode(t, err, "PRODUCT_NOT_PERMITTED")
	}
	if _, err := svc.Set(ctx, 9, th("100", "80", "30"), 42); err != nil {
		t.Fatalf("floor-equal pro set must pass: %v", err)
	}

	// ECP: no floor enforcement — negotiable within sanity bounds.
	f.cat = CategoryEligible
	if _, err := svc.Set(ctx, 10, th("90", "70", "20"), 42); err != nil {
		t.Fatalf("ECP set must pass: %v", err)
	}

	// Read/write failures propagate.
	f.catErr = fmt.Errorf("pg down")
	if _, err := svc.Set(ctx, 7, th("150", "110", "60"), 42); err == nil {
		t.Fatal("category read failure must abort the set")
	}
	f.catErr = nil
	f.cat = CategoryRetail
	f.setErr = fmt.Errorf("pg down")
	if _, err := svc.Set(ctx, 7, th("150", "110", "60"), 42); err == nil {
		t.Fatal("store failure must propagate")
	}
}

func TestMarginThresholdServiceSetCapturesBeforeImage(t *testing.T) {
	ctx := context.Background()
	prior := &MarginThresholds{Warning: d("150"), Call: d("110"), StopOut: d("60")}
	f := &marginThresholdStoreFake{cat: CategoryRetail, override: prior}
	svc := NewMarginThresholdService(f)
	if _, err := svc.Set(ctx, 7, MarginThresholds{
		Warning: d("160"), Call: d("115"), StopOut: d("55")}, 99); err != nil {
		t.Fatal(err)
	}
	if len(f.setCalls) != 1 || f.setCalls[0].before == nil ||
		!f.setCalls[0].before.Call.Equal(d("110")) || f.setCalls[0].userID != 99 {
		t.Fatalf("before image / actor: %+v", f.setCalls)
	}
}

// ---------------------------------------------------------------------------
// MarginLevelViewService — GET /api/v1/account/margin-level
// ---------------------------------------------------------------------------

func TestMarginLevelViewService(t *testing.T) {
	ctx := context.Background()
	th := NewMarginThresholdService(&marginThresholdStoreFake{cat: CategoryRetail})

	// Nil reader → coded degradation error.
	v := NewMarginLevelViewService(nil, th)
	if _, err := v.View(ctx, 7); err == nil {
		t.Fatal("nil reader must fail closed")
	} else {
		requireCode(t, err, CodeLeverageUnavailable)
	}
	// Reader error → coded wrap.
	v = NewMarginLevelViewService(marginLevelErrReader{fmt.Errorf("redis down")}, th)
	if _, err := v.View(ctx, 7); err == nil {
		t.Fatal("reader error must propagate coded")
	} else {
		requireCode(t, err, CodeLeverageUnavailable)
	}

	// No hash → honest "not evaluated" view, thresholds still resolved.
	v = NewMarginLevelViewService(staticLevels{}, th)
	view, err := v.View(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if view.Evaluated || view.Equity != nil || view.MarginLevelPct != nil ||
		view.Status != "NORMAL" || view.Source != "engine" ||
		view.WarningPct != "120" || view.MarginCallPct != "100" || view.StopOutPct != "50" {
		t.Fatalf("not-evaluated view: %+v", view)
	}

	// Evaluated hash decorates the view.
	updated := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	v = NewMarginLevelViewService(staticLevels{
		7: {AccountID: 7, Equity: d("3500"), UsedMargin: d("1000"),
			MarginLevelPct: d("350"), Status: "MARGIN_CALL", UpdatedAt: updated},
	}, th)
	view, err = v.View(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Evaluated || *view.Equity != "3500" || *view.UsedMargin != "1000" ||
		*view.MarginLevelPct != "350" || view.Status != "MARGIN_CALL" ||
		view.UpdatedAt == nil || !view.UpdatedAt.Equal(updated) {
		t.Fatalf("evaluated view: %+v", view)
	}

	// margin_level_pct=0 (no margin in use) omits the field.
	v = NewMarginLevelViewService(staticLevels{
		8: {AccountID: 8, Equity: d("10"), UsedMargin: decimal.Zero,
			MarginLevelPct: decimal.Zero, Status: "NORMAL"},
	}, th)
	view, err = v.View(ctx, 8)
	if err != nil {
		t.Fatal(err)
	}
	if view.MarginLevelPct != nil || view.Status != "NORMAL" {
		t.Fatalf("zero-level view: %+v", view)
	}

	// Threshold service nil → view still answers, pct fields empty.
	v = NewMarginLevelViewService(staticLevels{}, nil)
	view, err = v.View(ctx, 7)
	if err != nil || view.WarningPct != "" || view.MarginCallPct != "" {
		t.Fatalf("threshold-less view: %v %+v", err, view)
	}
	// Threshold resolution failure propagates.
	v = NewMarginLevelViewService(staticLevels{},
		NewMarginThresholdService(&marginThresholdStoreFake{catErr: fmt.Errorf("pg down")}))
	if _, err := v.View(ctx, 7); err == nil {
		t.Fatal("threshold failure must abort the view")
	}
}

// ---------------------------------------------------------------------------
// MarginLevelWatcher — private:margin pushes on hash change only
// ---------------------------------------------------------------------------

func TestMarginLevelWatcherPublishesOnlyOnChange(t *testing.T) {
	ctx := context.Background()
	updated := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	levels := staticLevels{
		7: {AccountID: 7, Equity: d("100"), UsedMargin: d("50"),
			MarginLevelPct: d("200"), Status: "NORMAL", UpdatedAt: updated},
	}
	lister := marginLevelListerFake{keys: []string{
		"margin:level:7", "margin:level:garbage", "margin:level:-4", "margin:level:8"}}
	spy := &marginLevelPublishSpy{}
	w := NewMarginLevelWatcher(levels, lister, spy.fn())

	if err := w.TickOnce(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// acct 8 has no hash (nil level) and the malformed keys are skipped —
	// exactly one publish for acct 7.
	if len(spy.calls) != 1 {
		t.Fatalf("publishes: %+v", spy.calls)
	}
	c := spy.calls[0]
	if c.acct != 7 || c.channel != "private:margin" || c.eventType != "margin_level" {
		t.Fatalf("publish: %+v", c)
	}
	payload, ok := c.payload.(map[string]any)
	if !ok || payload["equity"] != "100" || payload["used_margin"] != "50" ||
		payload["margin_level_pct"] != "200" || payload["status"] != "NORMAL" {
		t.Fatalf("payload: %+v", c.payload)
	}

	// Second tick with identical fields → no publish.
	if err := w.TickOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("unchanged hash must not republish (calls=%d)", len(spy.calls))
	}

	// A status change republishes.
	levels[7].Status = "MARGIN_CALL"
	if err := w.TickOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(spy.calls) != 2 {
		t.Fatalf("status change must republish (calls=%d)", len(spy.calls))
	}
	if p := spy.calls[1].payload.(map[string]any); p["status"] != "MARGIN_CALL" {
		t.Fatalf("payload status: %+v", p)
	}

	// ForgetAccount forces a re-announce even without a field change.
	w.ForgetAccount(7)
	if err := w.TickOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(spy.calls) != 3 {
		t.Fatalf("forget must re-announce (calls=%d)", len(spy.calls))
	}

	// Publish failure propagates (ForgetAccount forces the publish attempt).
	spy.err = fmt.Errorf("ws down")
	w.ForgetAccount(7)
	if err := w.TickOnce(ctx); err == nil {
		t.Fatal("publish error must propagate")
	}
	spy.err = nil

	// Lister failure propagates.
	w = NewMarginLevelWatcher(levels, marginLevelListerFake{err: fmt.Errorf("scan down")}, spy.fn())
	if err := w.TickOnce(ctx); err == nil {
		t.Fatal("scan error must propagate")
	}

	// Nil seams → safe no-op.
	w = NewMarginLevelWatcher(nil, nil, nil)
	if err := w.TickOnce(ctx); err != nil {
		t.Fatalf("nil seams must no-op, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// RedisMarginLevelReader — ungated nil-client + gated hash contract
// ---------------------------------------------------------------------------

func TestMarginLevelReaderNilClient(t *testing.T) {
	r := RedisMarginLevelReader{}
	if _, err := r.MarginLevel(context.Background(), 7); err == nil {
		t.Fatal("nil client must error (fail closed)")
	}
	if err := r.SetMarginLevel(context.Background(), MarginLevel{AccountID: 7}); err == nil {
		t.Fatal("nil client write must error")
	}
}

func TestRedisMarginLevelReaderRoundTrip(t *testing.T) {
	rdb := adlTestRedis(t)
	ctx := context.Background()
	r := RedisMarginLevelReader{C: rdb}
	acct := int64(424242)
	t.Cleanup(func() { _ = rdb.Del(ctx, MarginLevelKey(acct)).Err() })

	// Missing hash → (nil, nil): "not evaluated", never fabricated.
	lv, err := r.MarginLevel(ctx, acct)
	if err != nil || lv != nil {
		t.Fatalf("missing hash: %+v %v", lv, err)
	}

	// Full canonical shape round-trips (SetMarginLevel is the writer twin).
	updated := time.Date(2026, 10, 5, 12, 34, 56, 789000000, time.UTC)
	if err := r.SetMarginLevel(ctx, MarginLevel{
		AccountID: acct, Equity: d("3500.5"), UsedMargin: d("999.25"),
		MarginLevelPct: d("350.375"), Status: "MARGIN_CALL", UpdatedAt: updated}); err != nil {
		t.Fatalf("write: %v", err)
	}
	lv, err = r.MarginLevel(ctx, acct)
	if err != nil || lv == nil {
		t.Fatalf("read: %v %+v", err, lv)
	}
	if lv.AccountID != acct || !lv.Equity.Equal(d("3500.5")) ||
		!lv.UsedMargin.Equal(d("999.25")) || !lv.MarginLevelPct.Equal(d("350.375")) ||
		lv.Status != "MARGIN_CALL" || !lv.UpdatedAt.Equal(updated) {
		t.Fatalf("round-trip: %+v", lv)
	}

	// Epoch-millis updated_at parses too (the reader's second contract).
	if err := rdb.HSet(ctx, MarginLevelKey(acct), "updated_at", "1700000000123").Err(); err != nil {
		t.Fatal(err)
	}
	lv, err = r.MarginLevel(ctx, acct)
	if err != nil || !lv.UpdatedAt.Equal(time.UnixMilli(1700000000123).UTC()) {
		t.Fatalf("epoch updated_at: %v %v", lv.UpdatedAt, err)
	}

	// A malformed decimal field is a hard read error — fail closed.
	if err := rdb.HSet(ctx, MarginLevelKey(acct), "equity", "not-a-decimal").Err(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.MarginLevel(ctx, acct); err == nil {
		t.Fatal("unreadable equity field must error")
	}
	// Missing optional fields decode as zero (used_margin omitted ⇒ ∞ level).
	if err := rdb.HSet(ctx, MarginLevelKey(acct), "equity", "12.5").Err(); err != nil {
		t.Fatal(err)
	}
	if err := rdb.HDel(ctx, MarginLevelKey(acct), "used_margin", "margin_level_pct").Err(); err != nil {
		t.Fatal(err)
	}
	lv, err = r.MarginLevel(ctx, acct)
	if err != nil || lv == nil {
		t.Fatalf("partial read: %v %+v", err, lv)
	}
	if !lv.Equity.Equal(d("12.5")) || !lv.UsedMargin.IsZero() || !lv.MarginLevelPct.IsZero() {
		t.Fatalf("partial hash decode: %+v", lv)
	}
}

// ---------------------------------------------------------------------------
// PgMarginThresholdStore — gated round-trips (EXC_PG_TEST=1)
// ---------------------------------------------------------------------------

func TestPgMarginThresholdStore(t *testing.T) {
	pool := marginSchemaFixture(t, marginStoreMigrations)
	ctx := context.Background()
	st := NewPgMarginThresholdStore(pool)
	retail := liqSeedAccount(t, pool, "RETAIL")
	pro := liqSeedAccount(t, pool, "PROFESSIONAL")

	// ClientCategory: seeded values; a missing account row reads RETAIL
	// (the reader's documented fail-closed default for this seam).
	if c, err := st.ClientCategory(ctx, retail); err != nil || c != "RETAIL" {
		t.Fatalf("retail: %q %v", c, err)
	}
	if c, err := st.ClientCategory(ctx, pro); err != nil || c != "PROFESSIONAL" {
		t.Fatalf("pro: %q %v", c, err)
	}
	if c, err := st.ClientCategory(ctx, 4242424242); err != nil || c != "RETAIL" {
		t.Fatalf("missing account must default retail: %q %v", c, err)
	}

	// ThresholdsFor: nil before the first override.
	got, err := st.ThresholdsFor(ctx, retail)
	if err != nil || got != nil {
		t.Fatalf("absent override: %+v %v", got, err)
	}

	// SetThresholds upserts + writes the audit row atomically.
	first := MarginThresholds{Warning: d("150"), Call: d("120"), StopOut: d("60")}
	if err := st.SetThresholds(ctx, retail, first, nil, 42); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err = st.ThresholdsFor(ctx, retail)
	if err != nil || got == nil {
		t.Fatalf("read-back: %v %+v", err, got)
	}
	if !got.Warning.Equal(d("150")) || !got.Call.Equal(d("120")) || !got.StopOut.Equal(d("60")) {
		t.Fatalf("override decode: %+v", got)
	}
	var audits int
	var beforeNull bool
	if err := pool.QueryRow(ctx, `
		SELECT count(*), bool_and(before_state IS NULL OR before_state::text='null')
		FROM admin_audit_log
		WHERE action='account.margin_thresholds.update' AND target_id=$1`, retail).
		Scan(&audits, &beforeNull); err != nil || audits != 1 || !beforeNull {
		t.Fatalf("first audit row: n=%d null=%v %v", audits, beforeNull, err)
	}

	// Update path: values replaced, before-state captured in the audit.
	second := MarginThresholds{Warning: d("140"), Call: d("110"), StopOut: d("55")}
	if err := st.SetThresholds(ctx, retail, second, &first, 43); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = st.ThresholdsFor(ctx, retail)
	if err != nil || !got.Warning.Equal(d("140")) || !got.StopOut.Equal(d("55")) {
		t.Fatalf("update read-back: %v %+v", err, got)
	}
	var beforeJSON string
	if err := pool.QueryRow(ctx, `
		SELECT before_state::text FROM admin_audit_log
		WHERE action='account.margin_thresholds.update' AND target_id=$1
		ORDER BY id DESC LIMIT 1`, retail).Scan(&beforeJSON); err != nil {
		t.Fatal(err)
	}
	if beforeJSON == "" || beforeJSON == "null" ||
		!strings.Contains(beforeJSON, "150") ||
		!strings.Contains(beforeJSON, "120") ||
		!strings.Contains(beforeJSON, "60") {
		t.Fatalf("before-state must capture the prior row: %s", beforeJSON)
	}

	// The migration-235 CHECK rejects unordered overrides at the DB layer.
	if err := st.SetThresholds(ctx, retail, MarginThresholds{
		Warning: d("50"), Call: d("80"), StopOut: d("30")}, nil, 1); err == nil {
		t.Fatal("unordered thresholds must violate the CHECK constraint")
	}
}
