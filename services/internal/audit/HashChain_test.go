package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"
)

var testBase = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// mkTail adapts an Entry into the tailRow readTail would have returned.
func mkTail(e Entry) *tailRow {
	return &tailRow{id: e.ID, sequenceNum: e.SequenceNum,
		payloadHash: e.PayloadHash, prevHash: e.PrevHash}
}

// mkChain builds n well-formed rows exactly as Append would.
func mkChain(n int) []Entry {
	var rows []Entry
	var tail *tailRow
	for i := 1; i <= n; i++ {
		rid := int64(1000 + i)
		e := buildEntry(tail, "orders", &rid, "INSERT", nil,
			int64(100+i), int64(i), testBase.Add(time.Duration(i)*time.Second))
		rows = append(rows, e)
		tail = mkTail(e)
	}
	return rows
}

func TestGenesisPrevHashIsSHA256Empty(t *testing.T) {
	sum := sha256.Sum256(nil)
	if GenesisPrevHash != hex.EncodeToString(sum[:]) {
		t.Fatalf("GenesisPrevHash = %s, want %s", GenesisPrevHash, hex.EncodeToString(sum[:]))
	}
}

func TestPayloadHashKnownVector(t *testing.T) {
	rid := int64(42)
	ts := testBase // already UTC, micro-aligned
	got := PayloadHash("orders", &rid, "INSERT", 7, 55, ts, nil)

	preimage := fmt.Sprintf("orders|42|INSERT|7|55|%s|", ts.Format(time.RFC3339Nano))
	want := sha256.Sum256([]byte(preimage))
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("payload hash = %s, want sha256(%q) = %s", got, preimage, hex.EncodeToString(want[:]))
	}
}

func TestChainHashVector(t *testing.T) {
	a, b := "aa", "bb"
	want := sha256.Sum256([]byte("aabb"))
	if got := ChainHash(a, b); got != hex.EncodeToString(want[:]) {
		t.Fatalf("chain hash = %s, want %s", got, hex.EncodeToString(want[:]))
	}
}

func TestCanonicalRecordID(t *testing.T) {
	if got := canonicalRecordID(nil); got != "-" {
		t.Fatalf("NULL record_id = %q, want \"-\"", got)
	}
	rid := int64(7)
	if got := canonicalRecordID(&rid); got != "7" {
		t.Fatalf("record_id = %q, want \"7\"", got)
	}
}

func TestGenesisRowUsesGenesisPrevHash(t *testing.T) {
	rows := mkChain(1)
	if rows[0].PrevHash != GenesisPrevHash {
		t.Fatalf("genesis prev_hash = %s, want %s", rows[0].PrevHash, GenesisPrevHash)
	}
	if rep := verifyRows(rows, nil); !rep.OK() {
		t.Fatalf("genesis row failed verify: %+v", rep.Violations)
	}
}

func TestVerifyCleanChain(t *testing.T) {
	rows := mkChain(5)
	if rep := verifyRows(rows, nil); !rep.OK() {
		t.Fatalf("clean chain reported violations: %+v", rep.Violations)
	}
}

// hasViolationOn reports whether a violation names the given sequence + field.
func hasViolationOn(rep VerifyReport, seq int64, field string) bool {
	for _, v := range rep.Violations {
		if v.SequenceNum == seq && v.Field == field {
			return true
		}
	}
	return false
}

func TestVerifyDetectsRecordIDTamper(t *testing.T) {
	rows := mkChain(5)
	rid := int64(2002) // was 1002 — the DoD UPDATE record_id=record_id+1 case
	rows[1].RecordID = &rid
	rep := verifyRows(rows, nil)
	if rep.OK() {
		t.Fatal("record_id tamper not detected")
	}
	if !hasViolationOn(rep, 2, "payload_hash") {
		t.Fatalf("expected payload_hash violation at seq=2, got %+v", rep.Violations)
	}
	if rep.Violations[0].Day == "" {
		t.Fatal("violation missing offending day")
	}
}

func TestVerifyDetectsActionTamper(t *testing.T) {
	rows := mkChain(4)
	rows[2].Action = "DELETE"
	rep := verifyRows(rows, nil)
	if !hasViolationOn(rep, 3, "payload_hash") {
		t.Fatalf("action tamper not detected: %+v", rep.Violations)
	}
}

func TestVerifyDetectsCreatedAtTamper(t *testing.T) {
	rows := mkChain(4)
	rows[0].CreatedAt = rows[0].CreatedAt.Add(time.Hour)
	rep := verifyRows(rows, nil)
	if !hasViolationOn(rep, 1, "payload_hash") {
		t.Fatalf("created_at tamper not detected: %+v", rep.Violations)
	}
}

func TestVerifyDetectsIDTamper(t *testing.T) {
	rows := mkChain(4)
	rows[2].ID++
	rep := verifyRows(rows, nil)
	if !hasViolationOn(rep, 3, "payload_hash") {
		t.Fatalf("id tamper not detected: %+v", rep.Violations)
	}
}

func TestVerifyDetectsSequenceTamper(t *testing.T) {
	rows := mkChain(4)
	rows[2].SequenceNum = 99
	rep := verifyRows(rows, nil)
	if rep.OK() {
		t.Fatal("sequence_num tamper not detected")
	}
	// Both the contiguity check and the payload_hash preimage (which
	// includes sequence_num) fire on the same row.
	if !hasViolationOn(rep, 99, "sequence_num") && !hasViolationOn(rep, 99, "payload_hash") {
		t.Fatalf("expected sequence/payload violation, got %+v", rep.Violations)
	}
}

func TestVerifyDetectsPayloadHashTamper(t *testing.T) {
	rows := mkChain(4)
	rows[1].PayloadHash = "deadbeef" + rows[1].PayloadHash[8:]
	rep := verifyRows(rows, nil)
	if !hasViolationOn(rep, 2, "payload_hash") {
		t.Fatalf("payload_hash tamper not detected at seq=2: %+v", rep.Violations)
	}
	// The link into the NEXT row is also broken because the stored
	// payload_hash no longer matches what row 3 chained against.
	if !hasViolationOn(rep, 3, "prev_hash") {
		t.Fatalf("successor link mismatch not detected at seq=3: %+v", rep.Violations)
	}
}

func TestVerifyDetectsPrevHashTamper(t *testing.T) {
	rows := mkChain(4)
	rows[2].PrevHash = "deadbeef" + rows[2].PrevHash[8:]
	rep := verifyRows(rows, nil)
	if !hasViolationOn(rep, 3, "prev_hash") {
		t.Fatalf("prev_hash tamper not detected at seq=3: %+v", rep.Violations)
	}
	if !hasViolationOn(rep, 4, "prev_hash") {
		t.Fatalf("successor link mismatch not detected at seq=4: %+v", rep.Violations)
	}
}

func TestVerifyDetectsDeletedMiddleRow(t *testing.T) {
	rows := mkChain(5)
	// Drop seq=3 entirely: seq=4's stored prev_hash points at seq=3's hashes.
	deleted := append([]Entry{}, rows[:2]...)
	deleted = append(deleted, rows[3:]...)
	rep := verifyRows(deleted, nil)
	if rep.OK() {
		t.Fatal("deleted middle row not detected")
	}
	if !hasViolationOn(rep, 4, "sequence_num") || !hasViolationOn(rep, 4, "prev_hash") {
		t.Fatalf("expected gap + link violation at seq=4, got %+v", rep.Violations)
	}
}

func TestVerifyDetectsGenesisTamper(t *testing.T) {
	rows := mkChain(3)
	rows[0].PrevHash = "0000" + rows[0].PrevHash[4:]
	rep := verifyRows(rows, nil)
	if !hasViolationOn(rep, 1, "prev_hash") {
		t.Fatalf("genesis anchor tamper not detected: %+v", rep.Violations)
	}
}

func TestVerifyEmptyChainIsClean(t *testing.T) {
	if rep := verifyRows(nil, nil); !rep.OK() || rep.RowsChecked != 0 {
		t.Fatalf("empty chain should verify clean, got %+v", rep)
	}
}

func TestValidateAppendInput(t *testing.T) {
	rid := int64(1)
	for _, tc := range []struct {
		name, table, action string
		rid                 *int64
		wantErr             bool
	}{
		{"ok", "orders", "INSERT", &rid, false},
		{"ok null rid", "orders", "DELETE", nil, false},
		{"empty table", "", "INSERT", &rid, true},
		{"pipe in table", "ord|ers", "INSERT", &rid, true},
		{"pipe in action", "orders", "IN|SERT", &rid, true},
		{"action too long", "orders", "ABCDEFGHIJKLMNOPQ", &rid, true},
		{"negative rid", "orders", "INSERT", func() *int64 { i := int64(-1); return &i }(), true},
	} {
		err := validateAppendInput(tc.table, tc.rid, tc.action)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%s: err=%v wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}
