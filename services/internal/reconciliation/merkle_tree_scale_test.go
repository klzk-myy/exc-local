package reconciliation

import (
	"testing"
	"time"
)

// >1M-leaf build scalability — Phase-13 Task 13.3.7 SDD edge case.
func TestMerkleTreeMillionLeaves(t *testing.T) {
	const n = 1_000_000
	leaves := make([]LeafInput, n)
	for i := range leaves {
		leaves[i] = leafIn(int64(i), "USD", "1", fixedSalt(1))
	}
	t0 := time.Now()
	tree := Build(leaves)
	build := time.Since(t0)
	root := tree.Root()
	t0 = time.Now()
	path, ok := tree.Proof(n - 1)
	if !ok || !Verify(tree.Leaves[n-1], path, root) {
		t.Fatal("proof failed at n-1")
	}
	t.Logf("n=%d build=%v depth=%d verify_ok", n, build, len(tree.Levels)-1)
}
