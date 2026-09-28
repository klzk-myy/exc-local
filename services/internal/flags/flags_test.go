package flags

import (
	"testing"
)

// flag builds a manual-percentage flag (stage_idx=-1 = the schema
// default; a zero-value Flag has StageIdx=0 which arms the ladder at
// its first rung — see Flag.StageIdx doc).
func flag(pct int) Flag {
	f := Flag{Name: "x-feature", Enabled: true, RolloutPct: pct, StageIdx: -1}
	f.Normalize()
	return f
}

// Determinism: the same (flag, subject) always lands in the same bucket.
func TestEvalDeterministic(t *testing.T) {
	f := flag(50)
	c := EvalContext{AccountID: 4242}
	for i := 0; i < 8; i++ {
		if f.Eval(c) != f.Eval(c) {
			t.Fatal("eval non-deterministic")
		}
	}
	// Same account on another flag may differ (bucket salts on name).
}

// Monotonicity: a subject in the 1% cohort stays in at 10%/25%/100%.
func TestEvalMonotonicRollout(t *testing.T) {
	var bucketed []int64
	f1 := flag(1)
	for id := int64(1); id <= 20000; id++ {
		if f1.Eval(EvalContext{AccountID: id}) {
			bucketed = append(bucketed, id)
		}
	}
	if len(bucketed) == 0 {
		t.Fatal("1% rollout produced zero members over 20k accounts")
	}
	f10, f100 := flag(10), flag(100)
	for _, id := range bucketed {
		if !f10.Eval(EvalContext{AccountID: id}) {
			t.Fatalf("account %d in 1%% cohort dropped at 10%%", id)
		}
		if !f100.Eval(EvalContext{AccountID: id}) {
			t.Fatalf("account %d in 1%% cohort dropped at 100%%", id)
		}
	}
}

// Disabled flags are off even for allowlisted accounts.
func TestEvalDisabled(t *testing.T) {
	f := flag(100)
	f.Enabled = false
	f.Accounts = []int64{7}
	if f.Eval(EvalContext{AccountID: 7, Tier: "institutional"}) {
		t.Fatal("disabled flag evaluated on")
	}
}

// Allowlists bypass the percentage entirely.
func TestEvalAllowlists(t *testing.T) {
	f := flag(0) // 0% — nobody gets in via percentage
	f.Accounts = []int64{11, 22}
	f.Tiers = []string{"institutional"}
	if !f.Eval(EvalContext{AccountID: 22}) {
		t.Fatal("allowlisted account not on")
	}
	if !f.Eval(EvalContext{AccountID: 33, Tier: "institutional"}) {
		t.Fatal("allowlisted tier not on")
	}
	if f.Eval(EvalContext{AccountID: 33, Tier: "basic"}) {
		t.Fatal("non-allowlisted account on at 0%")
	}
}

// Unkeyed callers never land in a percentage bucket (fail-closed).
func TestEvalUnkeyedCaller(t *testing.T) {
	f := flag(50)
	if f.Eval(EvalContext{}) {
		t.Fatal("anonymous unkeyed caller entered a percentage rollout")
	}
	// …but still see allowlist/global truth.
	f.RolloutPct = 100
	if !f.Eval(EvalContext{}) {
		t.Fatal("100% rollout should apply to everyone, keyed or not")
	}
}

// Staged rollout: EffectivePct follows the ladder; Advance is bounded.
func TestStageLadder(t *testing.T) {
	f := Flag{Name: "staged", Enabled: true, StageIdx: -1}
	f.Normalize()
	if got := f.EffectivePct(); got != 0 {
		t.Fatalf("fresh flag pct = %d, want 0", got)
	}
	want := []int{1, 10, 25, 50, 100}
	for i, p := range want {
		idx, done, err := f.Advance()
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		if idx != i || f.EffectivePct() != p {
			t.Fatalf("step %d: idx=%d pct=%d, want idx=%d pct=%d",
				i, idx, f.EffectivePct(), i, p)
		}
		if done != (i == len(want)-1) {
			t.Fatalf("step %d done=%v", i, done)
		}
	}
	// Past the end is a no-op reporting done.
	idx, done, err := f.Advance()
	if err != nil || !done || idx != len(want)-1 {
		t.Fatalf("post-ladder advance: idx=%d done=%v err=%v", idx, done, err)
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		f       Flag
		wantErr bool
	}{
		{Flag{Name: "ok_flag", Stages: []int{1, 100}, StageIdx: -1}, false},
		{Flag{Name: "BAD"}, true},
		{Flag{Name: "", Stages: []int{1}}, true},
		{Flag{Name: "ok", RolloutPct: 101, Stages: []int{1}}, true},
		{Flag{Name: "ok", Stages: nil}, true},
		{Flag{Name: "ok", Stages: []int{50, 10}}, true}, // not ascending
		{Flag{Name: "ok", Stages: []int{1, -5}}, true},
		{Flag{Name: "ok", Stages: []int{1}, StageIdx: 5}, true},
	}
	for i, c := range cases {
		if err := c.f.Validate(); (err != nil) != c.wantErr {
			t.Fatalf("case %d: err=%v wantErr=%v", i, err, c.wantErr)
		}
	}
}

// Bucket distribution sanity: ~pct% of accounts in cohort at 25%.
func TestBucketDistribution(t *testing.T) {
	f := flag(25)
	n := 0
	const total = 40000
	for id := int64(1); id <= total; id++ {
		if f.Eval(EvalContext{AccountID: id}) {
			n++
		}
	}
	got := float64(n) / total
	if got < 0.20 || got > 0.30 {
		t.Fatalf("25%% rollout hit rate %.3f outside [0.20,0.30]", got)
	}
}

// fnv1a32 must match internal/config's shard hash so flag buckets and
// shard assignment share one hash family (documented in flags.go).
func TestFnv1a32KnownVectors(t *testing.T) {
	// FNV-1a 32 reference vectors.
	if got := fnv1a32(""); got != 2166136261 {
		t.Fatalf("empty = %d", got)
	}
	if got := fnv1a32("a"); got != 3826002220 {
		t.Fatalf(`"a" = %d`, got)
	}
	if got := fnv1a32("foobar"); got != 0xbf9cf968 {
		t.Fatalf(`"foobar" = %#x`, got)
	}
}
