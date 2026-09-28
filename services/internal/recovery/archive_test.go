package recovery_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"exchange/internal/devs3"
	"exchange/internal/objectstore"
	"exchange/internal/recovery"
)

func testStack(t *testing.T, bucket string) (objectstore.Client, func()) {
	t.Helper()
	srv, err := devs3.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	c, err := objectstore.NewDev(context.Background(), objectstore.Config{
		Bucket: bucket, Endpoint: ts.URL, Region: "us-east-1",
	})
	if err != nil {
		ts.Close()
		t.Fatal(err)
	}
	return c, ts.Close
}

func pload(t recovery.EventType, body []byte) []byte {
	return append([]byte{byte(t)}, body...)
}

func encOrderNew(orderID, acct uint64, inst uint32, side, typ, tif uint8,
	price, qty int64) []byte {
	p := make([]byte, 80)
	put64 := func(off int, v uint64) { binary64(p[off:], v) }
	put64(0, orderID)
	put64(8, acct)
	le32(p[16:], inst)
	p[20], p[21], p[22] = side, typ, tif
	put64(24, uint64(price))
	put64(32, uint64(qty))
	put64(40, uint64(qty))
	return pload(recovery.EvOrderNew, p)
}

func encTrade(id, buy, sell uint64, inst uint32, price, qty int64) []byte {
	p := make([]byte, 48)
	binary64(p[0:], id)
	binary64(p[8:], buy)
	binary64(p[16:], sell)
	le32(p[24:], inst)
	binary64(p[32:], uint64(price))
	binary64(p[40:], uint64(qty))
	return pload(recovery.EvTrade, p)
}

func encTick(ns uint64) []byte {
	p := make([]byte, 16)
	binary64(p[0:], ns)
	return pload(recovery.EvTimeTick, p)
}

// helpers over the byte encoder — keep them tiny and explicit
func binary64(dst []byte, v uint64) {
	dst[0] = byte(v)
	dst[1] = byte(v >> 8)
	dst[2] = byte(v >> 16)
	dst[3] = byte(v >> 24)
	dst[4] = byte(v >> 32)
	dst[5] = byte(v >> 40)
	dst[6] = byte(v >> 48)
	dst[7] = byte(v >> 56)
}
func le32(dst []byte, v uint32) {
	dst[0], dst[1], dst[2], dst[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}

// segment builds a WAL segment image; each payload's first byte is the
// event type (test-local convention).
func segment(shard uint16, seqStart uint64, tsBase uint64, step uint64,
	payloads [][]byte) []byte {
	out := recovery.FileHeader(shard)
	for i, p := range payloads {
		out = append(out, recovery.EncodeEntry(
			seqStart+uint64(i), tsBase+uint64(i)*step,
			recovery.EventType(p[0]), p[1:])...)
	}
	return out
}

func TestArchiveAndStatus(t *testing.T) {
	c, done := testStack(t, "exchange-wal")
	defer done()
	ctx := context.Background()
	svc := recovery.NewArchiveService(c)

	dir := t.TempDir()
	tsBase := uint64(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC).UnixNano())
	segOld := segment(0, 0, tsBase, 1e9,
		[][]byte{encOrderNew(1, 101, 1, 1, 1, 0, 110000000, 1000000)})
	segNew := segment(0, 100, tsBase+2e9, 1e9,
		[][]byte{encOrderNew(2, 102, 1, 0, 1, 0, 110000000, 1000000)})
	os.WriteFile(filepath.Join(dir, "00000000000000000000.wal"), segOld, 0o644)
	os.WriteFile(filepath.Join(dir, "00000000000000000100.wal"), segNew, 0o644)

	metas, err := svc.ArchiveDir(ctx, dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Segment != "00000000000000000000.wal" {
		t.Fatalf("archived %+v", metas)
	}
	// Local trim happened only for the sealed one.
	if _, err := os.Stat(filepath.Join(dir, "00000000000000000000.wal")); !os.IsNotExist(err) {
		t.Fatal("sealed segment not trimmed")
	}
	if _, err := os.Stat(filepath.Join(dir, "00000000000000000100.wal")); err != nil {
		t.Fatal("active segment wrongly trimmed")
	}

	// Index + status.
	rows, err := svc.Status(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].Present {
		t.Fatalf("status %+v", rows)
	}
	if rows[0].Key != "0/2026-01-05/00000000000000000000.wal" {
		t.Fatalf("key %q", rows[0].Key)
	}
	if rows[0].StorageClass != "STANDARD" || rows[0].ETag == "" {
		t.Fatalf("meta %+v", rows[0])
	}

	// Idempotent re-archive: same segment replaced, not duplicated.
	data, _, err := c.Get(ctx, rows[0].Key)
	if err != nil || !bytes.Equal(data, segOld) {
		t.Fatalf("object bytes mismatch %v", err)
	}
}

type etagMangler struct{ objectstore.Client }

func (e etagMangler) Put(ctx context.Context, in objectstore.PutInput) (objectstore.Object, error) {
	o, err := e.Client.Put(ctx, in)
	o.ETag = "deadbeef"
	return o, err
}

func TestArchiveNoTrimOnBadETag(t *testing.T) {
	c, done := testStack(t, "exchange-wal")
	defer done()
	bad := etagMangler{c}
	svc := recovery.NewArchiveService(bad)

	dir := t.TempDir()
	path := filepath.Join(dir, "0.wal")
	os.WriteFile(path, segment(0, 0, 1, 1, [][]byte{encTick(1)}), 0o644)
	_, err := svc.TrimSegment(context.Background(), 0, path)
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("want upload-not-confirmed, got %v", err)
	}
	if _, serr := os.Stat(path); serr != nil {
		t.Fatal("local segment trimmed despite unconfirmed upload — zero-loss violated")
	}
}

func TestLifecycleTransition(t *testing.T) {
	c, done := testStack(t, "exchange-wal")
	defer done()
	svc := recovery.NewArchiveService(c)
	clock := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	svc.SetClock(func() time.Time { return clock })

	// Segment dated 2026-01-01 → >90 days before clock.
	tsBase := uint64(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	seg := segment(0, 0, tsBase, 1, [][]byte{encTick(tsBase)})
	if _, err := svc.ArchiveSegment(context.Background(), 0, "0.wal", seg, clock); err != nil {
		t.Fatal(err)
	}
	n, err := svc.ApplyLifecycle(context.Background(), 0)
	if err != nil || n != 1 {
		t.Fatalf("lifecycle n=%d err=%v", n, err)
	}
	rows, _ := svc.Status(context.Background(), 0)
	if rows[0].StorageClass != "GLACIER" || rows[0].GlacierTransitionAt == "" {
		t.Fatalf("not transitioned: %+v", rows[0])
	}
	// lifecycle.json policy doc published.
	if _, _, err := c.Get(context.Background(), "lifecycle.json"); err != nil {
		t.Fatal("lifecycle.json missing")
	}
}
