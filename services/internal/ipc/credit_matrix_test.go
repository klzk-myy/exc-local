package ipc

import (
	"encoding/binary"
	"os"
	"testing"
)

// TestCreditMatrixLayout asserts every byte the C++ BilateralCreditMatrix
// expects lands at the ABI offset — header fields, cell addressing,
// update_seq ordering — by writing via the Go handle and reading the raw
// file bytes, exactly what a core-side mmap would see.
func TestCreditMatrixLayout(t *testing.T) {
	const name = "test_credit_matrix_layout"
	_ = os.Remove("/dev/shm/" + name)
	t.Cleanup(func() { _ = os.Remove("/dev/shm/" + name) })

	m, err := OpenCreditMatrix(name, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	raw, err := os.ReadFile("/dev/shm/" + name)
	if err != nil {
		t.Fatalf("read shm: %v", err)
	}
	if len(raw) != CreditMatrixFileSize || CreditMatrixFileSize != 8388864 {
		t.Fatalf("size %d, want 8388864", len(raw))
	}
	// magic "EXCRDIT1" at offset 0 — byte string, not byte-swapped.
	if got := string(raw[0:8]); got != "EXCRDIT1" {
		t.Fatalf("magic %q", got)
	}
	if got := binary.LittleEndian.Uint32(raw[8:]); got != 1 {
		t.Fatalf("version %d", got)
	}
	if got := binary.LittleEndian.Uint32(raw[12:]); got != 1024 {
		t.Fatalf("max_parties %d", got)
	}
	if got := binary.LittleEndian.Uint64(raw[16:]); got != 0 {
		t.Fatalf("fresh update_seq %d", got)
	}
	if got := binary.LittleEndian.Uint64(raw[24:]); got != uint64(os.Getpid()) {
		t.Fatalf("writer_pid %d != %d", got, os.Getpid())
	}
	// Reserved region 32..255 must be zero.
	for i := 32; i < 256; i++ {
		if raw[i] != 0 {
			t.Fatalf("reserved byte %d = %d", i, raw[i])
		}
	}
	// Apply a directed credit line a=7 → b=42, $5M in 1e8 ticks.
	const ticks5M = 500_000_000_000_000
	if !m.ApplyUpdate(7, 42, ticks5M) {
		t.Fatal("ApplyUpdate failed")
	}
	raw, _ = os.ReadFile("/dev/shm/" + name)
	off := CreditCellOffset(7, 42)
	if off != 256+(uint64(7)*1024+42)*8 {
		t.Fatalf("cell offset %d", off)
	}
	if got := binary.LittleEndian.Uint64(raw[off:]); got != ticks5M {
		t.Fatalf("cell[7][42]=%d", got)
	}
	// update_seq must have moved AFTER the cell write.
	if got := binary.LittleEndian.Uint64(raw[16:]); got != 1 {
		t.Fatalf("update_seq %d", got)
	}
	// Opposite direction stays zero — directed credit fails closed.
	if m.CreditLimit(42, 7) != 0 || m.HasHeadroom(42, 7, 1) {
		t.Fatal("non-directed headroom present")
	}
	// HasHeadroom is the MUTUAL check: with only 7→42 set, a match still
	// cannot proceed until the 42→7 line exists — spec §13.8 rule.
	if m.HasHeadroom(7, 42, 1) {
		t.Fatal("unilateral credit admitted a match")
	}
	m.ApplyUpdate(42, 7, ticks5M)
	if !m.HasHeadroom(7, 42, ticks5M) || m.HasHeadroom(7, 42, ticks5M+1) {
		t.Fatal("mutual headroom wrong")
	}
}

// TestCreditMatrixConsumeOrSkip debits both directed cells atomically and
// rolls back when the reverse line lacks headroom.
func TestCreditMatrixConsumeOrSkip(t *testing.T) {
	const name = "test_credit_matrix_consume"
	_ = os.Remove("/dev/shm/" + name)
	t.Cleanup(func() { _ = os.Remove("/dev/shm/" + name) })
	m, err := OpenCreditMatrix(name, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	m.ApplyUpdate(1, 2, 1000)
	m.ApplyUpdate(2, 1, 500)
	// Mutual headroom min(1000,500): a 600-debit must fail AND leave both
	// cells untouched (rollback).
	if m.ConsumeOrSkip(1, 2, 600) {
		t.Fatal("consume beyond mutual headroom succeeded")
	}
	if m.CreditLimit(1, 2) != 1000 || m.CreditLimit(2, 1) != 500 {
		t.Fatalf("rollback failed: %d %d", m.CreditLimit(1, 2), m.CreditLimit(2, 1))
	}
	if !m.ConsumeOrSkip(1, 2, 400) {
		t.Fatal("consume within mutual headroom failed")
	}
	if m.CreditLimit(1, 2) != 600 || m.CreditLimit(2, 1) != 100 {
		t.Fatalf("post-debit cells: %d %d", m.CreditLimit(1, 2), m.CreditLimit(2, 1))
	}
}

// TestCreditCtlCodec round-trips the 56-byte CREDIT_UPDATE wire format
// and verifies every reject branch of the C++ decode order.
func TestCreditCtlCodec(t *testing.T) {
	const name = "test_credit_matrix_ctl"
	_ = os.Remove("/dev/shm/" + name)
	t.Cleanup(func() { _ = os.Remove("/dev/shm/" + name) })
	m, err := OpenCreditMatrix(name, true)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer m.Close()

	frame := EncodeCreditUpdate(99, 3, 4, 777)
	if len(frame) != 56 {
		t.Fatalf("frame size %d", len(frame))
	}
	msg, res := DecodeCreditUpdate(frame)
	if res != CreditCtlOK || msg.PartyA != 3 || msg.PartyB != 4 ||
		msg.NewLimit != 777 || msg.Seq != 99 {
		t.Fatalf("decode: %v %+v", res, msg)
	}
	if r := m.OnControlMessage(frame); r != CreditCtlOK {
		t.Fatalf("apply: %v", r)
	}
	if m.CreditLimit(3, 4) != 777 {
		t.Fatal("control message did not publish cell")
	}
	// Reject branches — same order as credit_ctl_decode.
	if _, r := DecodeCreditUpdate(frame[:20]); r != CreditCtlTooShort {
		t.Fatalf("short: %v", r)
	}
	bad := append([]byte(nil), frame...)
	bad[0] = 'X'
	if _, r := DecodeCreditUpdate(bad); r != CreditCtlBadMagic {
		t.Fatalf("magic: %v", r)
	}
	bad = append([]byte(nil), frame...)
	bad[6] = 2 // version
	if _, r := DecodeCreditUpdate(bad); r != CreditCtlBadVersion {
		t.Fatalf("version: %v", r)
	}
	bad = append([]byte(nil), frame...)
	bad[4] = 9 // type
	if _, r := DecodeCreditUpdate(bad); r != CreditCtlUnknownType {
		t.Fatalf("type: %v", r)
	}
	bad = append([]byte(nil), frame...)
	binary.LittleEndian.PutUint32(bad[32:], 1024) // party out of range
	if r := m.OnControlMessage(bad); r != CreditCtlPartyOutOfRange {
		t.Fatalf("party: %v", r)
	}
	// Malformed frames must not partially apply.
	if m.CreditLimit(3, 4) != 777 {
		t.Fatal("malformed frame mutated state")
	}
}
