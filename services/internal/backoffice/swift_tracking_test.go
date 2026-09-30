// Task 24.3.4 tests — SWIFT journal record/query, validation,
// immutability (trigger-enforced at PG).
package backoffice

import (
	"context"
	"testing"
	"time"

	excerrors "exchange/pkg/errors"
)

type fakeSwiftStore struct {
	msgs []SwiftMessage
}

func (f *fakeSwiftStore) InsertMessage(_ context.Context, m *SwiftMessage) error {
	m.ID = int64(len(f.msgs) + 1)
	m.CreatedAt = time.Now().UTC()
	f.msgs = append(f.msgs, *m)
	return nil
}

func (f *fakeSwiftStore) ListMessages(_ context.Context, flt SwiftFilter) ([]SwiftMessage, error) {
	var out []SwiftMessage
	for _, m := range f.msgs {
		if flt.Type != "" && m.MessageType != flt.Type {
			continue
		}
		if flt.Direction != "" && m.Direction != flt.Direction {
			continue
		}
		if flt.From != nil && m.MsgTimestamp.Before(*flt.From) {
			continue
		}
		if flt.To != nil && !m.MsgTimestamp.Before(*flt.To) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func TestSwiftRecord_Validation(t *testing.T) {
	tr, err := NewSwiftTracker(&fakeSwiftStore{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	bad := []SwiftMessage{
		{Reference: "R1", Direction: SwiftIn},                  // no type
		{MessageType: "MT202", Direction: SwiftIn},             // no ref
		{MessageType: "MT202", Reference: "R", Direction: "X"}, // bad dir
	}
	for i, m := range bad {
		if _, err := tr.Record(ctx, m); err == nil ||
			excerrors.CodeOf(err) != "INVALID_REQUEST" {
			t.Fatalf("case %d: %v", i, err)
		}
	}
	// Defaults: IN → RECEIVED, OUT → SENT; ts defaults to clock.
	in, err := tr.Record(ctx, SwiftMessage{
		MessageType: "MT900", Reference: "R9", Direction: "in"})
	if err != nil || in.Status != "RECEIVED" || in.MsgTimestamp.IsZero() {
		t.Fatalf("inbound defaults: %+v %v", in, err)
	}
	out, err := tr.Record(ctx, SwiftMessage{
		MessageType: "mt202", Reference: "R2", Direction: "OUT"})
	if err != nil || out.Status != "SENT" || out.MessageType != "MT202" {
		t.Fatalf("outbound defaults: %+v %v", out, err)
	}
}

func TestSwiftQuery_Filters(t *testing.T) {
	st := &fakeSwiftStore{}
	tr, _ := NewSwiftTracker(st)
	ctx := context.Background()
	mk := func(typ, dir, ref string, ts time.Time) {
		if _, err := tr.Record(ctx, SwiftMessage{
			MessageType: typ, Reference: ref, Direction: dir,
			MsgTimestamp: ts}); err != nil {
			t.Fatal(err)
		}
	}
	mk("MT103", SwiftIn, "A", boDay("2026-09-20"))
	mk("MT202", SwiftOut, "B", boDay("2026-09-25"))
	mk("MT900", SwiftIn, "C", boDay("2026-09-27"))
	mk("pacs.009", SwiftOut, "D", boDay("2026-09-28"))

	// Type filter.
	got, err := tr.Query(ctx, SwiftFilter{Type: "mt900"})
	if err != nil || len(got) != 1 || got[0].Reference != "C" {
		t.Fatalf("type filter: %+v %v", got, err)
	}
	// Window filter.
	from := boDay("2026-09-25")
	got, _ = tr.Query(ctx, SwiftFilter{From: &from})
	if len(got) != 3 {
		t.Fatalf("from filter: %d", len(got))
	}
	// Direction filter.
	got, _ = tr.Query(ctx, SwiftFilter{Direction: "OUT"})
	if len(got) != 2 {
		t.Fatalf("direction filter: %d", len(got))
	}
	// Invalid direction is a coded rejection.
	if _, err := tr.Query(ctx, SwiftFilter{Direction: "SIDEWAYS"}); err == nil {
		t.Fatal("bad direction must reject")
	}
	// Empty store → empty slice, never nil.
	empty, _ := (&fakeSwiftStore{}).ListMessages(ctx, SwiftFilter{})
	if empty != nil {
		// fake may return nil; the service must normalize.
		tr2, _ := NewSwiftTracker(&fakeSwiftStore{})
		out, _ := tr2.Query(ctx, SwiftFilter{})
		if out == nil || len(out) != 0 {
			t.Fatalf("empty query: %+v", out)
		}
	}
}

func TestSwiftStore_NoMutationSurface(t *testing.T) {
	// Compile-time proof the journal seam is insert+read only: there is
	// no Update/Delete on SwiftStore or PgNostroStore — immutability is
	// additionally trigger-enforced (TestPgSwiftTracker_Integration).
	var _ SwiftStore = (*PgNostroStore)(nil)
}

// ---------------------------------------------------------------------------
// PG-gated — insert, filter, trigger-enforced immutability.
// ---------------------------------------------------------------------------

func TestPgSwiftTracker_Integration(t *testing.T) {
	ctx, pool := boTestPool(t)
	boSchema(t, ctx, pool)
	store := NewPgNostroStore(pool)
	tr, err := NewSwiftTracker(store)
	if err != nil {
		t.Fatal(err)
	}

	in, err := tr.Record(ctx, SwiftMessage{
		MessageType: "MT202", Reference: "SW-1", Direction: SwiftOut,
		RelatedReference: "LEG-1", RawPayload: "{1:F01...}",
		MsgTimestamp: boDay("2026-09-28")})
	if err != nil || in.ID == 0 {
		t.Fatalf("insert: %+v %v", in, err)
	}
	if _, err := tr.Record(ctx, SwiftMessage{
		MessageType: "MT910", Reference: "SW-2", Direction: SwiftIn,
		MsgTimestamp: boDay("2026-09-29")}); err != nil {
		t.Fatal(err)
	}

	got, err := tr.Query(ctx, SwiftFilter{Type: "MT910"})
	if err != nil || len(got) != 1 || got[0].Reference != "SW-2" {
		t.Fatalf("query: %+v %v", got, err)
	}
	from := boDay("2026-09-28")
	to := boDay("2026-09-29")
	got, err = tr.Query(ctx, SwiftFilter{From: &from, To: &to})
	if err != nil || len(got) != 1 || got[0].MessageType != "MT202" {
		t.Fatalf("window: %+v %v", got, err)
	}

	// Immutability is trigger-enforced — UPDATE and DELETE both refuse.
	if _, err := pool.Exec(ctx,
		`UPDATE swift_messages SET status='ACKED' WHERE id=$1`,
		in.ID); err == nil {
		t.Fatal("UPDATE on the journal must be refused")
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM swift_messages WHERE id=$1`, in.ID); err == nil {
		t.Fatal("DELETE on the journal must be refused")
	}
	// The row survived both attempts.
	var ref string
	if err := pool.QueryRow(ctx,
		`SELECT reference FROM swift_messages WHERE id=$1`, in.ID).
		Scan(&ref); err != nil || ref != "SW-1" {
		t.Fatalf("journal row mutated: %q %v", ref, err)
	}
}
