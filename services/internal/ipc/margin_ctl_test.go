// margin_ctl_test.go — wire-codec conformance against the C++ packed
// struct layout (core/include/risk/CrossShardMarginCoordinator.h).
// Golden vectors below are computed from the C++ field order/widths —
// any drift in either direction breaks the engine↔coordinator contract.
package ipc

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func TestMarginCtlEncodeReserveReqGolden(t *testing.T) {
	req := &MarginReserveReqBody{
		ReservationID: 0x0002_0000000000AB, // issuer shard 2, seq 0xAB
		AccountID:     42,
		OrderID:       777,
		SrcShard:      2,
		DstShard:      MarginCoordinatorPicks,
		InstrumentID:  3,
		ReqFlags:      MarginReqFlagCorrelationOffset,
		Amount:        1_234_567_890_123, // 1e8-scaled ticks
		ExpiresAtNs:   1_700_000_000_000_000_000,
	}
	buf := make([]byte, MarginCtlMaxFrame)
	n := MarginCtlEncode(buf, MarginCtlReserveReq, req)
	if n != MarginCtlMaxFrame {
		t.Fatalf("encoded %d bytes, want %d", n, MarginCtlMaxFrame)
	}
	// Header: magic LE at [0:4], type at [4], flags 0 at [5], version LE [6:8].
	if got := binary.LittleEndian.Uint32(buf[0:]); got != MarginCtlMagic {
		t.Fatalf("magic %#x", got)
	}
	if buf[4] != 1 || buf[5] != 0 || binary.LittleEndian.Uint16(buf[6:]) != 1 {
		t.Fatalf("header type/flags/version bytes: %v", buf[4:8])
	}
	p := buf[MarginCtlHeaderLen:]
	if got := binary.LittleEndian.Uint64(p[0:]); got != req.ReservationID {
		t.Fatalf("reservation_id %#x", got)
	}
	if got := binary.LittleEndian.Uint64(p[8:]); got != 42 {
		t.Fatalf("account_id %d", got)
	}
	if got := binary.LittleEndian.Uint64(p[16:]); got != 777 {
		t.Fatalf("order_id %d", got)
	}
	if got := binary.LittleEndian.Uint32(p[24:]); got != 2 {
		t.Fatalf("src_shard %d", got)
	}
	if got := binary.LittleEndian.Uint32(p[28:]); got != MarginCoordinatorPicks {
		t.Fatalf("dst_shard %#x", got)
	}
	if got := binary.LittleEndian.Uint32(p[32:]); got != 3 {
		t.Fatalf("instrument_id %d", got)
	}
	if p[36] != MarginReqFlagCorrelationOffset {
		t.Fatalf("req_flags %#x", p[36])
	}
	if got := int64(binary.LittleEndian.Uint64(p[37:])); got != req.Amount {
		t.Fatalf("amount %d", got)
	}
	if got := binary.LittleEndian.Uint64(p[45:]); got != req.ExpiresAtNs {
		t.Fatalf("expires_at_ns %d", got)
	}

	// Golden vector produced by the C++ margin_ctl_encode (ASan build,
	// libexch_core.a) — verified byte-for-byte 2026-09-29.
	const wantHex = "4352474d01000100ab000000000002002a0000000000000009" +
		"0300000000000002000000ffffffff0300000001cb04fb711f010000" +
		"00002a36fe9c9717"
	if got := hex.EncodeToString(buf[:n]); got != wantHex {
		t.Fatalf("C++ golden mismatch:\n got  %s\n want %s", got, wantHex)
	}

	var v MarginCtlView
	if rc := MarginCtlDecodeFrame(buf[:n], &v); rc != MarginCtlDecodeOK {
		t.Fatalf("decode %s", rc)
	}
	if v.Type != MarginCtlReserveReq || v.Req != *req {
		t.Fatalf("roundtrip mismatch: %+v", v.Req)
	}
}

// TestMarginCtlCrossLanguageVectors pins all four frame types plus both
// WAL payloads to hex produced by the C++ encoder (margin_ctl_encode +
// packed-struct memcpy) — the exact bytes a shard engine emits/reads.
func TestMarginCtlCrossLanguageVectors(t *testing.T) {
	buf := make([]byte, MarginCtlMaxFrame)
	cases := []struct {
		name string
		typ  MarginCtlType
		body any
		want string
	}{
		{"ack", MarginCtlReserveAck, &MarginReserveAckBody{
			ReservationID: 0x0001_000000000001, AccountID: 9, ShardID: 5,
			GrantedAmount: 987_654_321, ExpiresAtNs: 1_700_000_005_000_000_000},
			"4352474d020001000100000000000100090000000000000005000000" +
				"b168de3a0000000000f22f60ff9c9717"},
		{"nack", MarginCtlReserveNack, &MarginReserveNackBody{
			ReservationID: 0x0001_000000000001, AccountID: 9, ShardID: 5,
			Reason: uint32(MarginNackInsufficientHeadroom)},
			"4352474d030001000100000000000100090000000000000005000000" +
				"01000000"},
		{"release", MarginCtlRelease, &MarginReleaseBody{
			ReservationID: 0x0001_000000000001, AccountID: 9, ShardID: 5,
			Reason: uint8(MarginReleaseTimeoutCompensate)},
			"4352474d04000100010000000000010009000000000000000500000003"},
	}
	for _, tc := range cases {
		n := MarginCtlEncode(buf, tc.typ, tc.body)
		if got := hex.EncodeToString(buf[:n]); got != tc.want {
			t.Fatalf("%s: C++ golden mismatch:\n got  %s\n want %s",
				tc.name, got, tc.want)
		}
	}
	// WAL payloads (memcpy'd packed structs on the C++ side).
	wr := EncodeWalMarginReserve(&WalMarginReservePayload{
		ReservationID: 0x0003_0000000000FF, AccountID: 7, OrderID: 11,
		ConsumerShard: 1, HostShard: 2, InstrumentID: 9,
		Origin: WalMarginOriginHosted, Amount: 55_000_000_000,
		ExpiresAtNs: 1_700_000_001_000_000_000})
	const wrWant = "ff0000000000030007000000000000000b00000000000000" +
		"01000000020000000900000001006641ce0c00000000cac471fe9c9717"
	if got := hex.EncodeToString(wr); got != wrWant {
		t.Fatalf("wal reserve C++ golden mismatch:\n got  %s\n want %s", got, wrWant)
	}
	wl := EncodeWalMarginRelease(&WalMarginReleasePayload{
		ReservationID: 0x0003_0000000000FF, AccountID: 7,
		Origin: WalMarginOriginHosted, Reason: uint8(MarginReleaseRecoveryOrphan)})
	if got := hex.EncodeToString(wl); got != "ff0000000000030007000000000000000106" {
		t.Fatalf("wal release C++ golden mismatch: %s", got)
	}
}

func TestMarginCtlRoundTripAllTypes(t *testing.T) {
	buf := make([]byte, MarginCtlMaxFrame)
	ack := &MarginReserveAckBody{
		ReservationID: 0x0001_000000000001, AccountID: 9, ShardID: 5,
		GrantedAmount: 987_654_321, ExpiresAtNs: 1_700_000_005_000_000_000,
	}
	if n := MarginCtlEncode(buf, MarginCtlReserveAck, ack); n != 44 {
		t.Fatalf("ack encoded %d, want 44", n)
	}
	nack := &MarginReserveNackBody{
		ReservationID: 0x0001_000000000001, AccountID: 9, ShardID: 5,
		Reason: uint32(MarginNackInsufficientHeadroom),
	}
	if n := MarginCtlEncode(buf, MarginCtlReserveNack, nack); n != 32 {
		t.Fatalf("nack encoded %d, want 32", n)
	}
	rel := &MarginReleaseBody{
		ReservationID: 0x0001_000000000001, AccountID: 9, ShardID: 5,
		Reason: uint8(MarginReleaseTimeoutCompensate),
	}
	if n := MarginCtlEncode(buf, MarginCtlRelease, rel); n != 29 {
		t.Fatalf("release encoded %d, want 29", n)
	}

	for _, tc := range []struct {
		name string
		typ  MarginCtlType
		body any
		want int
	}{
		{"req", MarginCtlReserveReq, &MarginReserveReqBody{ReservationID: 1, AccountID: 2, OrderID: 3, SrcShard: 4, DstShard: 5, InstrumentID: 6, ReqFlags: 1, Amount: -50, ExpiresAtNs: 9}, 61},
		{"ack", MarginCtlReserveAck, ack, 44},
		{"nack", MarginCtlReserveNack, nack, 32},
		{"release", MarginCtlRelease, rel, 29},
	} {
		n := MarginCtlEncode(buf, tc.typ, tc.body)
		if n != tc.want {
			t.Fatalf("%s: encoded %d, want %d", tc.name, n, tc.want)
		}
		// Trailing padding must be tolerated (forward-compat, C++ rule).
		var v MarginCtlView
		if rc := MarginCtlDecodeFrame(buf[:MarginCtlMaxFrame], &v); rc != MarginCtlDecodeOK {
			t.Fatalf("%s: decode with padding: %s", tc.name, rc)
		}
		if v.Type != tc.typ {
			t.Fatalf("%s: type %d", tc.name, v.Type)
		}
	}
}

func TestMarginCtlDecodeRejects(t *testing.T) {
	good := make([]byte, MarginCtlMaxFrame)
	if n := MarginCtlEncode(good, MarginCtlReserveAck,
		&MarginReserveAckBody{ReservationID: 1, AccountID: 2}); n == 0 {
		t.Fatal("encode ack failed")
	}
	var v MarginCtlView

	if rc := MarginCtlDecodeFrame(good[:3], &v); rc != MarginCtlDecodeTooShort {
		t.Fatalf("short: %s", rc)
	}
	bad := append([]byte(nil), good...)
	bad[0] ^= 0xFF
	if rc := MarginCtlDecodeFrame(bad, &v); rc != MarginCtlDecodeBadMagic {
		t.Fatalf("magic: %s", rc)
	}
	bad = append([]byte(nil), good...)
	bad[6] = 0xFF // version LE low byte
	if rc := MarginCtlDecodeFrame(bad, &v); rc != MarginCtlDecodeBadVersion {
		t.Fatalf("version: %s", rc)
	}
	bad = append([]byte(nil), good...)
	bad[4] = 99
	if rc := MarginCtlDecodeFrame(bad, &v); rc != MarginCtlDecodeUnknownType {
		t.Fatalf("type: %s", rc)
	}
	// Truncated body: header valid, body short → TooShort.
	if rc := MarginCtlDecodeFrame(good[:20], &v); rc != MarginCtlDecodeTooShort {
		t.Fatalf("truncated body: %s", rc)
	}
}

func TestMarginCtlEncodeGuards(t *testing.T) {
	buf := make([]byte, 8) // too small for any body
	if n := MarginCtlEncode(buf, MarginCtlRelease, &MarginReleaseBody{}); n != 0 {
		t.Fatalf("short dst encoded %d", n)
	}
	big := make([]byte, MarginCtlMaxFrame)
	if n := MarginCtlEncode(big, MarginCtlType(0), &MarginReleaseBody{}); n != 0 {
		t.Fatalf("unknown type encoded %d", n)
	}
	if n := MarginCtlEncode(big, MarginCtlReserveReq, nil); n != 0 {
		t.Fatalf("nil body encoded %d", n)
	}
	if n := MarginCtlEncode(big, MarginCtlReserveReq, "not a body"); n != 0 {
		t.Fatalf("wrong body type encoded %d", n)
	}
	if n := MarginCtlEncode(big, MarginCtlReserveReq, []byte{1, 2, 3}); n != 0 {
		t.Fatalf("short []byte body encoded %d", n)
	}
}

func TestWalMarginPayloadsRoundTrip(t *testing.T) {
	rp := &WalMarginReservePayload{
		ReservationID: 0x0003_0000000000FF, AccountID: 7, OrderID: 11,
		ConsumerShard: 1, HostShard: 2, InstrumentID: 9,
		Origin: WalMarginOriginHosted, Amount: 55_000_000_000,
		ExpiresAtNs: 1_700_000_001_000_000_000,
	}
	enc := EncodeWalMarginReserve(rp)
	if len(enc) != WalMarginReserveLen {
		t.Fatalf("wal reserve len %d", len(enc))
	}
	// Field offset check against the C++ packed layout: origin sits at
	// byte 36 immediately after instrument_id — same as req_flags in REQ.
	if enc[36] != WalMarginOriginHosted {
		t.Fatalf("origin byte %d", enc[36])
	}
	got, err := DecodeWalMarginReserve(enc)
	if err != nil || got != *rp {
		t.Fatalf("wal reserve roundtrip: %+v err=%v", got, err)
	}
	if _, err := DecodeWalMarginReserve(enc[:20]); err == nil {
		t.Fatal("short wal reserve accepted")
	}

	lp := &WalMarginReleasePayload{
		ReservationID: 0x0003_0000000000FF, AccountID: 7,
		Origin: WalMarginOriginHosted, Reason: uint8(MarginReleaseRecoveryOrphan),
	}
	lenc := EncodeWalMarginRelease(lp)
	if len(lenc) != WalMarginReleaseLen {
		t.Fatalf("wal release len %d", len(lenc))
	}
	lgot, err := DecodeWalMarginRelease(lenc)
	if err != nil || lgot != *lp {
		t.Fatalf("wal release roundtrip: %+v err=%v", lgot, err)
	}
	if _, err := DecodeWalMarginRelease(lenc[:4]); err == nil {
		t.Fatal("short wal release accepted")
	}
}
