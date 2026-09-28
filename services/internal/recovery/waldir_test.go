package recovery

// waldir_test.go — Task 4.3.9 coverage for the offline ladder primitives:
// segment-dir prescan, torn-tail repair, rebase-marker segments, verified
// snapshot peek, and the recovery_reports JSONL row round-trip.

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeSeg(t *testing.T, dir, name string, shard uint16, seqStart uint64, payloads ...[]byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path,
		buildSegment(shard, seqStart, 1700000000000000000, payloads...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func corruptTail(t *testing.T, path string) uint64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	validEnd := uint64(len(data))
	bad := EncodeEntry(999, 1, EvOrderCancel, cancelBytes(42, 42)[1:])
	bad[WalEntryHeaderSize] ^= 0xFF // payload flip → bad CRC
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(bad); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return validEnd
}

func writeSnapFile(t *testing.T, dir, name string, shard uint16,
	seq uint64, instrument uint32, payload []byte) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, orchSnapFileHeaderLen)
	binary.LittleEndian.PutUint32(hdr[0:4], orchSnapFileMagic)
	binary.LittleEndian.PutUint16(hdr[4:6], orchSnapFileVersion)
	binary.LittleEndian.PutUint16(hdr[6:8], shard)
	binary.LittleEndian.PutUint64(hdr[8:16], seq)
	binary.LittleEndian.PutUint32(hdr[16:20], instrument)
	binary.LittleEndian.PutUint32(hdr[20:24], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[24:28], CRC32C(payload))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, append(hdr, payload...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecScanWalDirCleanContiguous(t *testing.T) {
	dir := t.TempDir()
	writeSeg(t, dir, "0.wal", 3, 0, orderNewBytes(1, 1, 7, 0, 1, 0, 100, 50))
	writeSeg(t, dir, "1.wal", 3, 1, orderNewBytes(2, 1, 7, 0, 1, 0, 99, 10),
		cancelBytes(2, 42))

	p, err := RecScanWalDir(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !p.HasEntries || p.WalTail != 3 || p.WalEntries != 3 || p.LastValidSeq != 2 {
		t.Fatalf("prescan: %+v", p)
	}
	if p.TailCorrupt || p.SealedDamage || p.Divergence != "" || len(p.LostRanges) != 0 {
		t.Fatalf("unexpected divergence: %+v", p)
	}
}

func TestRecScanWalDirTailCorruptAndRepair(t *testing.T) {
	dir := t.TempDir()
	p0 := writeSeg(t, dir, "0.wal", 3, 0, orderNewBytes(1, 1, 7, 0, 1, 0, 100, 50))
	corruptTail(t, p0)

	p, err := RecScanWalDir(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !p.TailCorrupt || p.TailPath != p0 {
		t.Fatalf("tail corruption not detected: %+v", p)
	}
	if p.WalTail != 1 || p.WalEntries != 1 {
		t.Fatalf("post-scan tail wrong: %+v", p)
	}

	repaired, err := RecRepairTail(p.TailPath, p.TailValidEnd)
	if err != nil {
		t.Fatal(err)
	}
	if !repaired {
		t.Fatal("expected truncation to occur")
	}
	p2, err := RecScanWalDir(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if p2.TailCorrupt || p2.SealedDamage || p2.Divergence != "" {
		t.Fatalf("repair incomplete: %+v", p2)
	}
	if p2.WalTail != 1 || p2.WalEntries != 1 {
		t.Fatalf("repaired stream wrong: %+v", p2)
	}
}

func TestRecScanWalDirSealedDamageLostRange(t *testing.T) {
	dir := t.TempDir()
	p0 := writeSeg(t, dir, "0.wal", 3, 0, orderNewBytes(1, 1, 7, 0, 1, 0, 100, 50))
	corruptTail(t, p0) // sealed: a later segment follows
	writeSeg(t, dir, "5.wal", 3, 5, orderNewBytes(5, 1, 7, 1, 1, 0, 101, 20))

	p, err := RecScanWalDir(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !p.SealedDamage {
		t.Fatalf("sealed damage not flagged: %+v", p)
	}
	if len(p.LostRanges) != 1 || p.LostRanges[0].Begin != 1 ||
		p.LostRanges[0].End != 5 || !p.LostRanges[0].CorruptCaused {
		t.Fatalf("lost range wrong: %+v", p.LostRanges)
	}
	if p.WalTail != 6 || p.WalEntries != 2 {
		t.Fatalf("stream accounting wrong: %+v", p)
	}
}

func TestRecWriteRebaseMarkerAndRescan(t *testing.T) {
	dir := t.TempDir()
	writeSeg(t, dir, "0.wal", 3, 0, orderNewBytes(1, 1, 7, 0, 1, 0, 100, 50))

	path, wrote, err := RecWriteRebaseMarker(dir, 3, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote || filepath.Base(path) != "9.wal" {
		t.Fatalf("marker: wrote=%v path=%s", wrote, path)
	}
	// Idempotent.
	_, wrote2, err := RecWriteRebaseMarker(dir, 3, 9)
	if err != nil {
		t.Fatal(err)
	}
	if wrote2 {
		t.Fatal("marker rewrite not idempotent")
	}

	p, err := RecScanWalDir(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	if p.WalTail != 10 || p.WalEntries != 2 {
		t.Fatalf("post-marker tail wrong: %+v", p)
	}
	if len(p.LostRanges) != 1 || p.LostRanges[0].Begin != 1 || p.LostRanges[0].End != 9 {
		t.Fatalf("marker gap accounting wrong: %+v", p.LostRanges)
	}
	// Marker entry decodes as a BOOK_SNAPSHOT anchor with book_seq = 9.
	data, _ := os.ReadFile(path)
	_, ents, err := ScanSegment(data)
	if err != nil || len(ents) != 1 {
		t.Fatalf("marker scan: %v entries=%d", err, len(ents))
	}
	if ents[0].Type != EvBookSnapshot || ents[0].Seq != 9 ||
		binary.LittleEndian.Uint64(ents[0].Payload[16:24]) != 9 {
		t.Fatalf("marker entry wrong: %+v", ents[0])
	}
}

func TestRecLatestVerifiedSnapshotFallback(t *testing.T) {
	root := t.TempDir()
	idir := filepath.Join(root, "i7")
	// Prior generation (seq 3) + corrupt newest (seq 9 → crc mismatch).
	pay := []byte("snapshot-payload")
	writeSnapFile(t, idir, "snap_00000000000000000003.bin", 3, 3, 7, pay)
	bad := filepath.Join(idir, "snap_00000000000000000009.bin")
	good := append([]byte(nil), pay...)
	good[0] ^= 0xFF
	writeSnapFile(t, idir, filepath.Base(bad), 3, 9, 7, good)
	// Write the corrupt file with a MISMATCHED crc deliberately: rewrite
	// with original payload but flip a stored byte after write.
	data, _ := os.ReadFile(bad)
	data[len(data)-1] ^= 0x01
	if err := os.WriteFile(bad, data, 0o644); err != nil {
		t.Fatal(err)
	}

	info, fellBack, err := RecLatestVerifiedSnapshot(root, 7)
	if err != nil {
		t.Fatal(err)
	}
	if info == nil || info.Seq != 3 || !fellBack {
		t.Fatalf("expected prior-generation fallback to seq 3: %+v fellBack=%v", info, fellBack)
	}

	// Cold start: no files → (nil,false,nil).
	empty := t.TempDir()
	info2, fb2, err := RecLatestVerifiedSnapshot(empty, 7)
	if err != nil || info2 != nil || fb2 {
		t.Fatalf("cold start should be (nil,false,nil): %+v %v %v", info2, fb2, err)
	}
}

func TestRecReportJSONRoundTrip(t *testing.T) {
	// Mirror the C++ emitter's exact field names.
	line := `{"shard_id":3,"book_seq":-1,"wal_tail":6,"last_valid_seq":5,` +
		`"snapshot_seq":0,"first_divergent_seq":5,"stage":"boot_ladder",` +
		`"outcome":"WAL_RECOVERY_HALT","detail":{"status":"WalCorrupt"},` +
		`"ts_unix_ns":1700000000000000000}`
	r, err := RecParseReportLine([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	if r.ShardID != 3 || r.Outcome != RecOutcomeHalt || r.FirstDivergentSeq != 5 {
		t.Fatalf("parse: %+v", r)
	}
	var det map[string]any
	if err := json.Unmarshal(r.Detail, &det); err != nil {
		t.Fatal(err)
	}
	if det["status"] != "WalCorrupt" {
		t.Fatalf("detail: %v", det)
	}
	// Missing stage/outcome is a hard parse failure.
	if _, err := RecParseReportLine([]byte(`{"shard_id":1}`)); err == nil {
		t.Fatal("expected parse failure on missing stage/outcome")
	}
}
