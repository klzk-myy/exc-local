// Task 13.3.7 — Merkle tree unit tests: known vectors, inclusion-proof
// verification, tamper detection, edge cases and the documented
// O(n) build / O(n log n) proof-materialization bound.
package reconciliation

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"testing"

	"exchange/pkg/decimal"
)

// fixedSalt returns a deterministic salt — tests only.
func fixedSalt(start byte) [SaltBytes]byte {
	var s [SaltBytes]byte
	for i := range s {
		s[i] = start + byte(i)
	}
	return s
}

func leafIn(acc int64, ccy, bal string, salt [SaltBytes]byte) LeafInput {
	return LeafInput{AccountID: acc, Currency: ccy,
		Balance: decimal.RequireFromString(bal), Salt: salt}
}

// referenceLeaf re-derives the leaf digest with an explicit, independent
// preimage construction — the point of the known-vector check is that
// leafDigest has no hidden normalization.
func referenceLeaf(acc int64, ccy, bal string, salt [SaltBytes]byte) [sha256.Size]byte {
	var pre []byte
	pre = append(pre, "EXC-SOLVENCY-LEAF\x00"...)
	var idb [8]byte
	binary.BigEndian.PutUint64(idb[:], uint64(acc))
	pre = append(pre, idb[:]...)
	pre = append(pre, '|')
	pre = append(pre, ccy...)
	pre = append(pre, '|')
	pre = append(pre, bal...)
	pre = append(pre, '|')
	pre = append(pre, hex.EncodeToString(salt[:])...)
	return sha256.Sum256(pre)
}

func TestMerkleKnownVector(t *testing.T) {
	l42 := leafIn(42, "USD", "100.5", fixedSalt(0x00))
	l43 := leafIn(43, "USD", "0", fixedSalt(0x10))
	l44 := leafIn(44, "EUR", "7.25", fixedSalt(0x20))

	// Leaf digests (cross-checked against an independent Python
	// computation of the same preimage — see task report).
	if got := HexDigest(leafDigest(l42)); got !=
		"49ba9df9886e211d42f47b6f56188525a64d4ba22f53d84875b70786aed15b27" {
		t.Fatalf("leaf42 = %s", got)
	}
	if got := HexDigest(leafDigest(l43)); got !=
		"8bd40e2636bb26eacf086dbb10fb305ba06a33b000f1df0bbbfcd4d82130a20f" {
		t.Fatalf("leaf43 = %s", got)
	}
	if got := HexDigest(leafDigest(l44)); got !=
		"d16d63385057ec3094ccc6cef31e26b6a20e817098b9c1be8fc0a87ae78fb708" {
		t.Fatalf("leaf44 = %s", got)
	}

	// The independent reference preimage must agree.
	if referenceLeaf(42, "USD", "100.5", fixedSalt(0x00)) != leafDigest(l42) {
		t.Fatal("reference preimage mismatch")
	}

	// Single leaf IS the root.
	if got := HexDigest(Build([]LeafInput{l42}).Root()); got != HexDigest(leafDigest(l42)) {
		t.Fatalf("single-leaf root = %s", got)
	}
	// Two leaves → SHA-256(l42 || l43).
	if got := HexDigest(Build([]LeafInput{l42, l43}).Root()); got !=
		"b794f6a703e9fa1270243814b0255419be8b07b96619de8a10d34a061766e53a" {
		t.Fatalf("two-leaf root = %s", got)
	}
	// Three leaves → odd level duplicates last: root = h(h(a,b) || h(c,c)).
	if got := HexDigest(Build([]LeafInput{l42, l43, l44}).Root()); got !=
		"e3f42ce588b59acf2ece1c187b20d7900abf7a9d5dcdc6913dbacdac860aae49" {
		t.Fatalf("three-leaf root = %s", got)
	}
	// Empty tree roots at SHA-256("") — a published "no liabilities" digest.
	if got := HexDigest(Build(nil).Root()); got !=
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Fatalf("empty root = %s", got)
	}
}

func TestMerkleCanonicalOrder(t *testing.T) {
	leaves := []LeafInput{
		leafIn(9, "USD", "1", fixedSalt(1)),
		leafIn(3, "USD", "2", fixedSalt(2)),
		leafIn(3, "EUR", "3", fixedSalt(3)),
		leafIn(1, "USD", "4", fixedSalt(4)),
	}
	// Any input permutation must produce the same root.
	want := Build(leaves).Root()
	reversed := []LeafInput{leaves[3], leaves[2], leaves[1], leaves[0]}
	if got := Build(reversed).Root(); got != want {
		t.Fatal("root changed under input reorder — canonical sort broken")
	}
	// And the stored leaf order is (account_id, currency) ascending.
	tree := Build(leaves)
	for i := 1; i < len(tree.Leaves); i++ {
		a, b := tree.Leaves[i-1], tree.Leaves[i]
		if a.AccountID > b.AccountID ||
			(a.AccountID == b.AccountID && a.Currency >= b.Currency) {
			t.Fatalf("leaves not canonically ordered at %d", i)
		}
	}
}

func TestMerkleProofVerify(t *testing.T) {
	for n := 1; n <= 17; n++ {
		leaves := make([]LeafInput, n)
		for i := range leaves {
			leaves[i] = leafIn(int64(1000+i), "USD",
				decimal.NewFromInt(int64(i)).String(), fixedSalt(byte(i)))
		}
		tree := Build(leaves)
		root := tree.Root()
		for i, l := range tree.Leaves {
			path, ok := tree.Proof(i)
			if !ok {
				t.Fatalf("n=%d leaf %d: no proof", n, i)
			}
			if !Verify(l, path, root) {
				t.Fatalf("n=%d leaf %d: proof does not fold to root", n, i)
			}
			wantDepth := 0
			if n > 1 {
				wantDepth = bits.Len(uint(n - 1)) // ceil(log2 n)
			}
			if len(path) != wantDepth {
				t.Fatalf("n=%d leaf %d: path depth %d, want %d", n, i, len(path), wantDepth)
			}
		}
	}
}

func TestMerkleTamper(t *testing.T) {
	leaves := []LeafInput{
		leafIn(1, "USD", "10", fixedSalt(0)),
		leafIn(2, "USD", "20", fixedSalt(1)),
		leafIn(3, "EUR", "30", fixedSalt(2)),
		leafIn(4, "USD", "40", fixedSalt(3)),
	}
	tree := Build(leaves)
	root := tree.Root()

	for i, l := range tree.Leaves {
		path, _ := tree.Proof(i)

		// Flipped sibling bit → root mismatch.
		bad := append([]ProofStep(nil), path...)
		bad[0].Hash[0] ^= 0x01
		if Verify(l, bad, root) {
			t.Fatalf("leaf %d: tampered sibling hash still verifies", i)
		}
		// Flipped sibling orientation → root mismatch (except the
		// single-leaf-proof corner — n=4 so every proof is non-empty).
		swapped := append([]ProofStep(nil), path...)
		swapped[0].Right = !swapped[0].Right
		if Verify(l, swapped, root) {
			t.Fatalf("leaf %d: swapped sibling orientation still verifies", i)
		}
		// Wrong salt → different leaf digest → root mismatch.
		forged := l
		forged.Salt[0] ^= 0xFF
		if Verify(forged, path, root) {
			t.Fatalf("leaf %d: forged salt still verifies", i)
		}
		// Inflated balance → root mismatch.
		fat := l
		fat.Balance = l.Balance.Add(decimal.NewFromInt(1))
		if Verify(fat, path, root) {
			t.Fatalf("leaf %d: inflated balance still verifies", i)
		}
		// Another leaf's path must not verify this leaf.
		j := (i + 1) % len(tree.Leaves)
		other, _ := tree.Proof(j)
		if Verify(l, other, root) {
			t.Fatalf("leaf %d: foreign path still verifies", i)
		}
	}
}

// TestMerkleZeroBalance — a 0-balance row produces a real, provable leaf
// (inclusion must exist for empty balances too — task edge case).
func TestMerkleZeroBalance(t *testing.T) {
	l := leafIn(7, "USD", "0", fixedSalt(9))
	tree := Build([]LeafInput{l, leafIn(8, "USD", "5", fixedSalt(10))})
	path, ok := tree.Proof(0)
	if !ok || !Verify(l, path, tree.Root()) {
		t.Fatal("zero-balance leaf has no valid proof")
	}
}

// TestMerkleProofBound — the honest scalability claim: build is O(n)
// hashes and every proof carries ceil(log2 n) siblings, so materializing
// all proofs is O(n log n). n=65536 exercises a multi-level tree cheaply;
// the 1M+ case is the same bound with ~20 steps per proof (~640B each) —
// no asymptotic cliff exists in this construction.
func TestMerkleProofBound(t *testing.T) {
	const n = 1 << 16 // 65536
	leaves := make([]LeafInput, n)
	for i := range leaves {
		leaves[i] = leafIn(int64(i), "USD", "1", fixedSalt(1))
	}
	tree := Build(leaves)
	root := tree.Root()

	// Node count: n leaves + n-1 internal (levels halve, odd duplicated)
	// → exactly 2n-1 digests materialized → O(n) build confirmed by count.
	total := 0
	for _, lv := range tree.Levels {
		total += len(lv)
	}
	if want := 2*n - 1; total != want {
		t.Fatalf("materialized %d digests, want %d", total, want)
	}
	if got := len(tree.Levels) - 1; got != 16 {
		t.Fatalf("depth %d, want 16", got)
	}
	// Sample proofs at spread indices — each exactly 16 steps, all verify.
	for _, i := range []int{0, 1, 12345, n / 2, n - 2, n - 1} {
		path, ok := tree.Proof(i)
		if !ok || len(path) != 16 {
			t.Fatalf("leaf %d: path depth %d", i, len(path))
		}
		if !Verify(tree.Leaves[i], path, root) {
			t.Fatalf("leaf %d: proof invalid", i)
		}
	}
}

func TestParseHexDigest(t *testing.T) {
	d, err := ParseHexDigest(HexDigest(sha256.Sum256([]byte("x"))))
	if err != nil {
		t.Fatal(err)
	}
	if HexDigest(d) != HexDigest(sha256.Sum256([]byte("x"))) {
		t.Fatal("round-trip mismatch")
	}
	for _, bad := range []string{"", "zz", "ab", HexDigest(sha256.Sum256(nil)) + "00"} {
		if _, err := ParseHexDigest(bad); err == nil {
			t.Fatalf("ParseHexDigest(%q) accepted", bad)
		}
	}
}
