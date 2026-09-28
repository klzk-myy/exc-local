// Task 4.3.1 — snapshot_service coverage: SnapReadyMsg parse, descriptor
// verification (size + CRC32C + SnapFileHeader), idempotent upsert into
// book_snapshots (fake store), SnapAckMsg emission, malformed-slot drain,
// and the snap-dir catch-up scan. Rings are real /dev/shm SPSC rings —
// same mechanism as production (shm_test.go precedent).

package recovery

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"exchange/internal/ipc"
)

const testShard uint16 = 3

func uniqBase(t *testing.T) string {
	return fmt.Sprintf("exch_test_snap_%s_%d", t.Name(), os.Getpid())
}

// fakeStore is an in-memory SnapshotStore honoring the upsert contract.
type fakeStore struct {
	mu   sync.Mutex
	rows map[string][]byte
	ids  map[string]int64
	next int64
	err  error // injected failure
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string][]byte{}, ids: map[string]int64{}, next: 1}
}

func snapKey(shard uint16, iid uint32, seq uint64) string {
	return fmt.Sprintf("%d/%d/%d", shard, iid, seq)
}

func (f *fakeStore) UpsertBookSnapshot(_ context.Context, shardID uint16,
	instrumentID uint32, snapshotSeq uint64, data []byte) (int64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, false, f.err
	}
	k := snapKey(shardID, instrumentID, snapshotSeq)
	if have, ok := f.rows[k]; ok {
		if len(have) != len(data) {
			return 0, false, fmt.Errorf("divergence")
		}
		return f.ids[k], false, nil
	}
	f.rows[k] = append([]byte(nil), data...)
	f.ids[k] = f.next
	f.next++
	return f.ids[k], true, nil
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.rows)
}

// buildSnapFile fabricates a byte-valid snap file (SnapFileHeader +
// payload + payload CRC32C) exactly as FileSnapshotSink::store writes it.
func buildSnapFile(t *testing.T, dir string, shard uint16, iid uint32,
	seq uint64, payload []byte) string {
	t.Helper()
	sub := filepath.Join(dir, fmt.Sprintf("i%d", iid))
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	hdr := make([]byte, orchSnapFileHeaderLen)
	binary.LittleEndian.PutUint32(hdr[0:], 0x50414E53) // 'SNAP'
	binary.LittleEndian.PutUint16(hdr[4:], 1)
	binary.LittleEndian.PutUint16(hdr[6:], shard)
	binary.LittleEndian.PutUint64(hdr[8:], seq)
	binary.LittleEndian.PutUint32(hdr[16:], iid)
	binary.LittleEndian.PutUint32(hdr[20:], uint32(len(payload)))
	binary.LittleEndian.PutUint32(hdr[24:], CRC32C(payload))
	data := append(hdr, payload...)
	path := filepath.Join(sub, fmt.Sprintf("snap_%020d.bin", seq))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// encodeReady builds a SnapReadyMsg buffer (mirrors SnapshotManager.cpp).
func encodeReady(shard uint16, iid uint32, seq, byteSize uint64,
	crc uint32, rel string) []byte {
	b := make([]byte, readyMsgLen)
	binary.LittleEndian.PutUint32(b[0:], readyMagic)
	binary.LittleEndian.PutUint16(b[4:], snapMsgVer)
	binary.LittleEndian.PutUint16(b[6:], shard)
	binary.LittleEndian.PutUint32(b[8:], iid)
	binary.LittleEndian.PutUint64(b[16:], seq)
	binary.LittleEndian.PutUint64(b[24:], byteSize)
	binary.LittleEndian.PutUint32(b[32:], crc)
	binary.LittleEndian.PutUint32(b[36:], uint32(len(rel)))
	copy(b[40:], rel)
	return b
}

// openPair opens the test's view of the two rings: ready as producer
// (simulating the core), ack as consumer (observing the ack).
func openPair(t *testing.T, base string, shard uint16) (prod, ackCons *ipc.Ring) {
	t.Helper()
	_ = os.Remove("/dev/shm/" + ReadyRingName(base, shard))
	_ = os.Remove("/dev/shm/" + AckRingName(base, shard))
	var err error
	prod, err = ipc.OpenRing(ReadyRingName(base, shard), ipc.RoleProducer,
		true, ringCapacity, ringSlotPayload)
	if err != nil {
		t.Fatalf("ready prod: %v", err)
	}
	ackCons, err = ipc.OpenRing(AckRingName(base, shard), ipc.RoleConsumer,
		true, ringCapacity, ringSlotPayload)
	if err != nil {
		t.Fatalf("ack cons: %v", err)
	}
	t.Cleanup(func() {
		_ = prod.Close()
		_ = ackCons.Close()
		_ = os.Remove("/dev/shm/" + ReadyRingName(base, shard))
		_ = os.Remove("/dev/shm/" + AckRingName(base, shard))
	})
	return prod, ackCons
}

func readAck(t *testing.T, r *ipc.Ring) (seq, id uint64, iid uint32) {
	t.Helper()
	p := r.Peek()
	if p == nil {
		t.Fatal("no ack on ring")
	}
	if len(p) != ackMsgLen {
		t.Fatalf("ack len %d", len(p))
	}
	if binary.LittleEndian.Uint32(p[0:]) != ackMagic {
		t.Fatal("ack magic")
	}
	iid = binary.LittleEndian.Uint32(p[8:])
	seq = binary.LittleEndian.Uint64(p[16:])
	id = binary.LittleEndian.Uint64(p[24:])
	r.Consume()
	return seq, id, iid
}

func TestCRC32CPinsCastagnoliContract(t *testing.T) {
	// Canonical CRC-32C vector — wal_crc32c on the C++ side
	// (core/src/wal/WalEntry.cpp) must produce the same value or the
	// SnapReadyMsg file_crc32c check is cross-language broken.
	if got := CRC32C([]byte("123456789")); got != 0xE3069283 {
		t.Fatalf("CRC32C(123456789) = %#x, want 0xE3069283", got)
	}
}

func TestParseReadyMsg(t *testing.T) {
	buf := encodeReady(3, 7, 42, 4096, 0xdeadbeef, "i7/snap_00000000000000000042.bin")
	m, ok := ParseReadyMsg(buf)
	if !ok {
		t.Fatal("parse failed")
	}
	if m.ShardID != 3 || m.InstrumentID != 7 || m.SnapshotSeq != 42 ||
		m.ByteSize != 4096 || m.FileCRC32C != 0xdeadbeef ||
		m.RelPath != "i7/snap_00000000000000000042.bin" {
		t.Fatalf("bad decode: %+v", m)
	}
	if _, ok := ParseReadyMsg(buf[:100]); ok {
		t.Fatal("truncated msg parsed")
	}
	buf[0] = 0xFF // corrupt magic
	if _, ok := ParseReadyMsg(buf); ok {
		t.Fatal("bad magic parsed")
	}
}

func TestPersistViaReadyNotifyAndAck(t *testing.T) {
	dir := t.TempDir()
	base := uniqBase(t)
	prod, ackCons := openPair(t, base, testShard)

	payload := []byte("fake-book-blob-payload")
	path := buildSnapFile(t, dir, testShard, 7, 42, payload)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	crc := CRC32C(data)

	ready, err := ipc.OpenRing(ReadyRingName(base, testShard),
		ipc.RoleConsumer, false, ringCapacity, ringSlotPayload)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := ipc.OpenRing(AckRingName(base, testShard), ipc.RoleProducer,
		false, ringCapacity, ringSlotPayload)
	if err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	svc, err := New(Config{ShardID: testShard, IPCBase: base, SnapDir: dir},
		store, ready, ack, nil)
	if err != nil {
		t.Fatal(err)
	}

	msg := encodeReady(testShard, 7, 42, uint64(fi.Size()), crc,
		"i7/snap_00000000000000000042.bin")
	if !prod.TryWrite(msg) {
		t.Fatal("ready write failed")
	}
	if n := svc.PumpOnce(context.Background()); n != 1 {
		t.Fatalf("pumped %d", n)
	}
	if store.count() != 1 {
		t.Fatalf("store rows %d", store.count())
	}
	seq, id, iid := readAck(t, ackCons)
	if seq != 42 || iid != 7 || id != 1 {
		t.Fatalf("ack seq=%d iid=%d id=%d", seq, iid, id)
	}
	m := svc.Metrics()
	if m.Persisted != 1 || m.Acked != 1 || m.Malformed != 0 {
		t.Fatalf("metrics %+v", m)
	}
}

func TestPersistRejectsCorruptDescriptor(t *testing.T) {
	dir := t.TempDir()
	base := uniqBase(t)
	prod, ackCons := openPair(t, base, testShard)

	payload := []byte("blob")
	buildSnapFile(t, dir, testShard, 7, 9, payload)

	ready, _ := ipc.OpenRing(ReadyRingName(base, testShard),
		ipc.RoleConsumer, false, ringCapacity, ringSlotPayload)
	ack, _ := ipc.OpenRing(AckRingName(base, testShard), ipc.RoleProducer,
		false, ringCapacity, ringSlotPayload)
	store := newFakeStore()
	svc, _ := New(Config{ShardID: testShard, IPCBase: base, SnapDir: dir},
		store, ready, ack, nil)

	// Wrong CRC in descriptor.
	msg := encodeReady(testShard, 7, 9, 32+uint64(len(payload)), 0x11111111,
		"i7/snap_00000000000000000009.bin")
	if !prod.TryWrite(msg) {
		t.Fatal("write")
	}
	svc.PumpOnce(context.Background())
	if store.count() != 0 {
		t.Fatal("corrupt descriptor persisted")
	}
	if svc.Metrics().VerifyFailed != 1 {
		t.Fatal("verifyFailed not counted")
	}
	if p := ackCons.Peek(); p != nil {
		t.Fatal("ack emitted for failed persist")
	}
}

func TestMalformedSlotConsumedNotWedged(t *testing.T) {
	dir := t.TempDir()
	base := uniqBase(t)
	prod, _ := openPair(t, base, testShard)

	ready, _ := ipc.OpenRing(ReadyRingName(base, testShard),
		ipc.RoleConsumer, false, ringCapacity, ringSlotPayload)
	ack, _ := ipc.OpenRing(AckRingName(base, testShard), ipc.RoleProducer,
		false, ringCapacity, ringSlotPayload)
	svc, _ := New(Config{ShardID: testShard, IPCBase: base, SnapDir: dir},
		newFakeStore(), ready, ack, nil)

	if !prod.TryWrite([]byte{1, 2, 3}) { // junk length
		t.Fatal("write junk")
	}
	good := encodeReady(testShard+1, 7, 1, 0, 0, "x") // foreign shard
	if !prod.TryWrite(good) {
		t.Fatal("write foreign")
	}
	if n := svc.PumpOnce(context.Background()); n != 2 {
		t.Fatalf("consumed %d — a malformed slot wedged the ring", n)
	}
	if svc.Metrics().Malformed != 2 {
		t.Fatalf("malformed=%d", svc.Metrics().Malformed)
	}
}

func TestScanOnceCatchupPersistsAndAcks(t *testing.T) {
	dir := t.TempDir()
	base := uniqBase(t)
	_, ackCons := openPair(t, base, testShard)

	// Snapshot file lands with NO ready notification (ring drop / restart).
	payload := []byte("scan-path-blob")
	buildSnapFile(t, dir, testShard, 7, 100, payload)

	ready, _ := ipc.OpenRing(ReadyRingName(base, testShard),
		ipc.RoleConsumer, false, ringCapacity, ringSlotPayload)
	ack, _ := ipc.OpenRing(AckRingName(base, testShard), ipc.RoleProducer,
		false, ringCapacity, ringSlotPayload)
	store := newFakeStore()
	svc, _ := New(Config{ShardID: testShard, IPCBase: base, SnapDir: dir},
		store, ready, ack, nil)

	if err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.count() != 1 {
		t.Fatalf("scan persisted %d rows", store.count())
	}
	seq, _, _ := readAck(t, ackCons)
	if seq != 100 {
		t.Fatalf("ack seq %d", seq)
	}
	// Second scan is a no-op — the seen set suppresses re-read + re-ack.
	if err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := ackCons.Peek(); p != nil {
		t.Fatal("duplicate ack emitted")
	}
	if svc.Metrics().ScanPersist != 1 {
		t.Fatalf("scanPersist=%d", svc.Metrics().ScanPersist)
	}
}

func TestStoreFailureBlocksAckAndRetries(t *testing.T) {
	dir := t.TempDir()
	base := uniqBase(t)
	_, ackCons := openPair(t, base, testShard)

	buildSnapFile(t, dir, testShard, 7, 55, []byte("p"))

	ready, _ := ipc.OpenRing(ReadyRingName(base, testShard),
		ipc.RoleConsumer, false, ringCapacity, ringSlotPayload)
	ack, _ := ipc.OpenRing(AckRingName(base, testShard), ipc.RoleProducer,
		false, ringCapacity, ringSlotPayload)
	store := newFakeStore()
	store.err = fmt.Errorf("pg down")
	svc, _ := New(Config{ShardID: testShard, IPCBase: base, SnapDir: dir},
		store, ready, ack, nil)

	if err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := ackCons.Peek(); p != nil {
		t.Fatal("ack emitted despite store failure")
	}
	if svc.Metrics().StoreFailed != 1 {
		t.Fatal("store failure not counted")
	}
	// Recover — the retry persists + acks.
	store.err = nil
	if err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	seq, _, _ := readAck(t, ackCons)
	if seq != 55 {
		t.Fatalf("ack seq %d", seq)
	}
}

func TestDivergentExistingRowNeverAcked(t *testing.T) {
	dir := t.TempDir()
	base := uniqBase(t)
	_, ackCons := openPair(t, base, testShard)

	buildSnapFile(t, dir, testShard, 7, 60, []byte("payload-A"))

	ready, _ := ipc.OpenRing(ReadyRingName(base, testShard),
		ipc.RoleConsumer, false, ringCapacity, ringSlotPayload)
	ack, _ := ipc.OpenRing(AckRingName(base, testShard), ipc.RoleProducer,
		false, ringCapacity, ringSlotPayload)
	store := newFakeStore()
	// Pre-seed a DIFFERENT-length row for the same key — a tampered or
	// divergent record must block the ack (fail closed).
	k := snapKey(testShard, 7, 60)
	store.rows[k] = []byte("payload-A-different-length")
	store.ids[k] = 77
	svc, _ := New(Config{ShardID: testShard, IPCBase: base, SnapDir: dir},
		store, ready, ack, nil)

	if err := svc.ScanOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := ackCons.Peek(); p != nil {
		t.Fatal("ack emitted for divergent row")
	}
}
