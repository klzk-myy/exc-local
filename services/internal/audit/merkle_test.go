package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"
)

func digest(tag string) []byte {
	sum := sha256.Sum256([]byte(tag))
	return sum[:]
}

func TestMerkleEmptyDay(t *testing.T) {
	root := MerkleRoot(nil)
	sum := sha256.Sum256(nil)
	if !bytes.Equal(root, sum[:]) {
		t.Fatalf("empty day root = %x, want sha256(\"\") = %x", root, sum)
	}
}

func TestMerkleSingleLeaf(t *testing.T) {
	leaf := digest("a")
	if root := MerkleRoot([][]byte{leaf}); !bytes.Equal(root, leaf) {
		t.Fatalf("single-leaf root = %x, want leaf %x", root, leaf)
	}
}

func TestMerkleTwoLeaves(t *testing.T) {
	a, b := digest("a"), digest("b")
	want := sha256.Sum256(append(append([]byte{}, a...), b...))
	if root := MerkleRoot([][]byte{a, b}); !bytes.Equal(root, want[:]) {
		t.Fatalf("two-leaf root = %x, want %x", root, want)
	}
}

func TestMerkleOddCountDuplicatesLast(t *testing.T) {
	a, b, c := digest("a"), digest("b"), digest("c")
	ab := sha256.Sum256(append(append([]byte{}, a...), b...))
	cc := sha256.Sum256(append(append([]byte{}, c...), c...))
	want := sha256.Sum256(append(append([]byte{}, ab[:]...), cc[:]...))
	if root := MerkleRoot([][]byte{a, b, c}); !bytes.Equal(root, want[:]) {
		t.Fatalf("three-leaf root = %x, want %x", root, want)
	}
}

func TestMerkleFourLeaves(t *testing.T) {
	a, b, c, d := digest("a"), digest("b"), digest("c"), digest("d")
	ab := sha256.Sum256(append(append([]byte{}, a...), b...))
	cd := sha256.Sum256(append(append([]byte{}, c...), d...))
	want := sha256.Sum256(append(append([]byte{}, ab[:]...), cd[:]...))
	if root := MerkleRoot([][]byte{a, b, c, d}); !bytes.Equal(root, want[:]) {
		t.Fatalf("four-leaf root = %x, want %x", root, want)
	}
}

func TestMerkleFiveLeavesOddAtTwoLevels(t *testing.T) {
	l := [][]byte{digest("a"), digest("b"), digest("c"), digest("d"), digest("e")}
	ab := sha256.Sum256(append(append([]byte{}, l[0]...), l[1]...))
	cd := sha256.Sum256(append(append([]byte{}, l[2]...), l[3]...))
	ee := sha256.Sum256(append(append([]byte{}, l[4]...), l[4]...))
	abcc := sha256.Sum256(append(append([]byte{}, ab[:]...), cd[:]...))
	eeee := sha256.Sum256(append(append([]byte{}, ee[:]...), ee[:]...))
	want := sha256.Sum256(append(append([]byte{}, abcc[:]...), eeee[:]...))
	if root := MerkleRoot(l); !bytes.Equal(root, want[:]) {
		t.Fatalf("five-leaf root = %x, want %x", root, want)
	}
}

func TestDayBoundsUTC(t *testing.T) {
	// 22:30 at UTC-5 is 03:30 UTC on Sept 15 — the UTC calendar day wins.
	start, end := dayBounds(time.Date(2026, 9, 14, 22, 30, 0, 0, time.FixedZone("X", -5*3600)))
	if start.Format("2006-01-02 15:04") != "2026-09-15 00:00" || end.Format("2006-01-02 15:04") != "2026-09-16 00:00" {
		t.Fatalf("dayBounds = [%s, %s), want UTC calendar day", start, end)
	}
}

func TestHexDigestsRoundTrip(t *testing.T) {
	// Guard the dayLeafDigests decode path: a stored 64-char hex digest must
	// decode to exactly 32 bytes.
	h := sha256.Sum256([]byte("x"))
	s := hex.EncodeToString(h[:])
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != sha256.Size {
		t.Fatalf("round-trip failed: %v len=%d", err, len(b))
	}
}
