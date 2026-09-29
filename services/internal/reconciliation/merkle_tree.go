// Package reconciliation implements the Phase-13 Task 13.3.7
// proof-of-reserves solvency machinery: the salted-leaf Merkle tree over
// all client liabilities, the daily snapshot generator, the signing seam
// and the client inclusion-proof reads.
//
// Tree scheme (this file — the pure, DB-free half):
//
//   - one leaf per (account_id, currency) balance row, ordered by
//     (account_id, currency) for a deterministic canonical ordering;
//   - leaf = SHA-256(domain || account_id || "|" || currency || "|" ||
//     balance || "|" || salt_hex) — explicit separators make the
//     concatenation unambiguous (a decimal account id abutting a currency
//     code could otherwise alias across fields);
//   - each parent is SHA-256(left || right) over raw 32-byte digests;
//   - an odd level duplicates its last node before pairing (same
//     convention as internal/audit.MerkleRoot);
//   - a single leaf IS the root; an empty snapshot roots at SHA-256("").
//
// Complexity: building is O(n) hashes — n leaves + n-1 internal nodes
// (each level halves the node count); materializing every proof is
// O(n log n) because a proof carries log2(n) siblings. Both bounds are
// exercised by tests in merkle_tree_test.go.
package reconciliation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"exchange/pkg/decimal"
)

// leafDomain separates solvency leaves from every other SHA-256 domain in
// the codebase (audit chain, internal nodes) so no preimage constructed
// elsewhere can masquerade as a liability leaf.
const leafDomain = "EXC-SOLVENCY-LEAF\x00"

// SaltBytes is the per-leaf salt length (128 bits — unguessable, makes
// leaf preimages non-derivable without the published proof row).
const SaltBytes = 16

// LeafInput is one client's liability in one currency. Balance uses the
// canonical fixed-point string form (decimal.String()) so the preimage is
// identical wherever it is reconstructed.
type LeafInput struct {
	AccountID int64
	Currency  string
	Balance   decimal.Decimal
	Salt      [SaltBytes]byte
}

// ProofStep is one sibling on the path from leaf to root. Right=true means
// the sibling hash sits to the RIGHT of the running node (node || sibling);
// false means the sibling is on the left (sibling || node).
//
// A step carries ONLY the sibling digest — never the sibling's account or
// balance — which is the zero-peer-data-leak contract of the proof API.
type ProofStep struct {
	Hash  [sha256.Size]byte `json:"-"`
	Right bool              `json:"-"`
}

// leafDigest computes the salted leaf digest for one balance row.
func leafDigest(l LeafInput) [sha256.Size]byte {
	var pre []byte
	pre = append(pre, leafDomain...)
	var idb [8]byte
	binary.BigEndian.PutUint64(idb[:], uint64(l.AccountID))
	pre = append(pre, idb[:]...)
	pre = append(pre, '|')
	pre = append(pre, l.Currency...)
	pre = append(pre, '|')
	pre = append(pre, l.Balance.String()...)
	pre = append(pre, '|')
	pre = append(pre, hex.EncodeToString(l.Salt[:])...)
	return sha256.Sum256(pre)
}

// newSalt mints a random per-leaf salt.
func newSalt() ([SaltBytes]byte, error) {
	var s [SaltBytes]byte
	if _, err := rand.Read(s[:]); err != nil {
		return s, fmt.Errorf("reconciliation: salt generation: %w", err)
	}
	return s, nil
}

// Tree is a materialized binary Merkle tree: Levels[0] are the leaf
// digests in canonical order, Levels[len-1][0] is the root.
type Tree struct {
	Leaves []LeafInput
	Levels [][][sha256.Size]byte
}

// Root returns the tree's root digest. An empty tree roots at
// sha256("") — the "empty snapshot" digest is still published so a root
// proves "no liabilities", not "job never ran".
func (t *Tree) Root() [sha256.Size]byte {
	if t == nil || len(t.Levels) == 0 || len(t.Levels[len(t.Levels)-1]) == 0 {
		return sha256.Sum256(nil)
	}
	return t.Levels[len(t.Levels)-1][0]
}

// Build sorts the inputs into canonical (account_id, currency) order and
// materializes the full level pyramid. Leaves are used as given — callers
// needing fresh salts must populate them first (Generate does).
func Build(leaves []LeafInput) *Tree {
	sorted := append([]LeafInput(nil), leaves...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].AccountID != sorted[j].AccountID {
			return sorted[i].AccountID < sorted[j].AccountID
		}
		return sorted[i].Currency < sorted[j].Currency
	})
	level := make([][sha256.Size]byte, len(sorted))
	for i, l := range sorted {
		level[i] = leafDigest(l)
	}
	t := &Tree{Leaves: sorted}
	if len(level) == 0 {
		t.Levels = [][][sha256.Size]byte{{sha256.Sum256(nil)}}
		return t
	}
	t.Levels = append(t.Levels, level)
	for len(level) > 1 {
		next := make([][sha256.Size]byte, 0, (len(level)+1)/2)
		for i := 0; i < len(level); i += 2 {
			right := level[i]
			if i+1 < len(level) {
				right = level[i+1]
			} // odd count: duplicate last (audit.MerkleRoot convention)
			next = append(next, sha256.Sum256(concat(level[i], right)))
		}
		level = next
		t.Levels = append(t.Levels, level)
	}
	return t
}

// Proof returns the sibling path for the leaf at index i (canonical
// order), or false when i is out of range.
func (t *Tree) Proof(i int) ([]ProofStep, bool) {
	if t == nil || len(t.Levels) == 0 || i < 0 || i >= len(t.Leaves) {
		return nil, false
	}
	var path []ProofStep
	idx := i
	for level := 0; level < len(t.Levels)-1; level++ {
		nodes := t.Levels[level]
		sibling := idx ^ 1 // even idx → sibling on the right; odd → left
		step := ProofStep{Right: idx%2 == 0}
		if sibling < len(nodes) {
			step.Hash = nodes[sibling]
		} else {
			// Odd level: the last node pairs with a copy of itself.
			step.Hash = nodes[idx]
		}
		path = append(path, step)
		idx /= 2
	}
	return path, true
}

// Verify folds a leaf preimage and its sibling path up to a root digest
// and reports whether it equals want. Used by the proof endpoint's
// self-check and by the test-suite's client-side verification.
func Verify(l LeafInput, path []ProofStep, want [sha256.Size]byte) bool {
	cur := leafDigest(l)
	for _, step := range path {
		if step.Right {
			cur = sha256.Sum256(concat(cur, step.Hash))
		} else {
			cur = sha256.Sum256(concat(step.Hash, cur))
		}
	}
	return cur == want
}

// concat pairs two raw digests for the parent hash.
func concat(a, b [sha256.Size]byte) []byte {
	out := make([]byte, 0, 2*sha256.Size)
	out = append(out, a[:]...)
	out = append(out, b[:]...)
	return out
}

// HexDigest renders a digest lowercase-hex for the API/storage surface.
func HexDigest(d [sha256.Size]byte) string { return hex.EncodeToString(d[:]) }

// ParseHexDigest decodes a 64-char lowercase-hex digest.
func ParseHexDigest(s string) ([sha256.Size]byte, error) {
	var d [sha256.Size]byte
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != sha256.Size {
		return d, fmt.Errorf("reconciliation: %q is not a 32-byte hex digest", s)
	}
	copy(d[:], b)
	return d, nil
}
