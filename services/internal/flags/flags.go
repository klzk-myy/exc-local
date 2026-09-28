// Package flags implements Phase-09 Task 9.3.7 — feature flags for
// canary deploys.
//
// Contract (task items 1–4):
//   - per-tier, per-account and global flag scope;
//   - canary deploy via a staged rollout ladder (1% → 10% → 100%);
//   - definitions persist in PostgreSQL feature_flags (migration 193)
//     and are cached in Redis under flags:{name};
//   - admin CRUD/toggle routes under /api/v1/admin/flags*.
//
// Evaluation order (Flag.Eval):
//  1. enabled=false            → always off;
//  2. account allowlist hit    → on;
//  3. tier allowlist hit       → on;
//  4. percentage rollout: EffectivePct() is stages[stage_idx] when a
//     ladder step is active, else rollout_pct; membership is the
//     deterministic bucket fnv1a32(name ":" subject) % 100 < pct.
//
// Fail-closed (spec §2.7): callers without a stable subject (no account
// id, no tier, no IP) can only observe allowlist/global states — a
// percentage bucket is never assigned to an anonymous, unkeyed caller.
package flags

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// nameRe is the schema-side identifier rule (migration 193 CHECK).
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{1,63}$`)

// DefaultStages is the published canary ladder (task item 2).
var DefaultStages = []int{1, 10, 25, 50, 100}

// Flag is one feature_flags row.
type Flag struct {
	Name       string `json:"name"`
	Enabled    bool   `json:"enabled"`
	RolloutPct int    `json:"rollout_pct"` // manual percentage (stage_idx = -1)
	Stages     []int  `json:"stages"`      // canary ladder, ascending %
	// StageIdx is the armed ladder position; -1 (the schema default)
	// means manual RolloutPct. NOTE: the Go zero value is 0 = ladder
	// armed at the first rung — constructors must set -1 explicitly
	// for manual rollout. Normalize does not touch StageIdx.
	StageIdx    int       `json:"stage_idx"`
	Tiers       []string  `json:"tiers"`    // tier names always-on
	Accounts    []int64   `json:"accounts"` // account ids always-on
	Description string    `json:"description"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// EvalContext is the caller identity a flag evaluates against.
type EvalContext struct {
	AccountID int64  // authenticated account, 0 when unknown
	Tier      string // fee/rate tier name ("basic", "institutional", ...)
	Subject   string // stable anon key — user id or client IP
}

// SubjectKey picks the deterministic bucket key: account id first
// (survives IP churn), then the explicit subject (anon IP/user), else "".
func (c EvalContext) SubjectKey() string {
	if c.AccountID > 0 {
		return strconv.FormatInt(c.AccountID, 10)
	}
	return c.Subject
}

// Validate checks a flag before persistence (fail-closed — the schema
// CHECKs are the backstop, not the validator).
func (f Flag) Validate() error {
	if !nameRe.MatchString(f.Name) {
		return fmt.Errorf("invalid flag name %q (want %s)", f.Name, nameRe.String())
	}
	if f.RolloutPct < 0 || f.RolloutPct > 100 {
		return fmt.Errorf("rollout_pct %d out of 0..100", f.RolloutPct)
	}
	if len(f.Stages) == 0 {
		return errors.New("stages ladder must not be empty")
	}
	for i, s := range f.Stages {
		if s < 0 || s > 100 {
			return fmt.Errorf("stage %d value %d out of 0..100", i, s)
		}
		if i > 0 && s <= f.Stages[i-1] {
			return fmt.Errorf("stages must be strictly ascending (%v)", f.Stages)
		}
	}
	if f.StageIdx < -1 || f.StageIdx >= len(f.Stages) {
		return fmt.Errorf("stage_idx %d outside ladder of %d stages", f.StageIdx, len(f.Stages))
	}
	if len(f.Tiers) > 256 || len(f.Accounts) > 65536 {
		return errors.New("allowlists exceed sanity bound")
	}
	return nil
}

// EffectivePct resolves the live rollout percentage: the ladder step
// when a stage is armed, the manual percentage otherwise.
func (f Flag) EffectivePct() int {
	if f.StageIdx >= 0 && f.StageIdx < len(f.Stages) {
		return f.Stages[f.StageIdx]
	}
	return f.RolloutPct
}

// Eval evaluates the flag for one caller. Disabled flags are off for
// everyone; allowlisted accounts/tiers bypass the percentage; the
// percentage bucket is deterministic per (flag, subject) so a caller's
// membership is stable across deploys and replicas.
func (f Flag) Eval(c EvalContext) bool {
	if !f.Enabled {
		return false
	}
	if c.AccountID > 0 && containsInt64(f.Accounts, c.AccountID) {
		return true
	}
	if c.Tier != "" && containsStr(f.Tiers, c.Tier) {
		return true
	}
	pct := f.EffectivePct()
	if pct >= 100 {
		return true
	}
	if pct <= 0 {
		return false
	}
	key := c.SubjectKey()
	if key == "" {
		return false // unkeyed caller — no stable bucket, fail closed
	}
	return int(fnv1a32(f.Name+":"+key)%100) < pct
}

// Advance steps the canary ladder one position forward. Returns the new
// stage index and whether the ladder is complete (last step reached).
func (f *Flag) Advance() (idx int, done bool, err error) {
	if len(f.Stages) == 0 {
		return f.StageIdx, false, errors.New("no stages ladder")
	}
	if f.StageIdx >= len(f.Stages)-1 {
		return f.StageIdx, true, nil // already at the final step
	}
	f.StageIdx++
	return f.StageIdx, f.StageIdx == len(f.Stages)-1, nil
}

// Normalize fills defaults for a fresh flag definition.
func (f *Flag) Normalize() {
	if len(f.Stages) == 0 {
		f.Stages = append([]int(nil), DefaultStages...)
	}
	if f.Tiers == nil {
		f.Tiers = []string{}
	}
	if f.Accounts == nil {
		f.Accounts = []int64{}
	}
	sort.Strings(f.Tiers)
	sort.Slice(f.Accounts, func(i, j int) bool { return f.Accounts[i] < f.Accounts[j] })
}

func containsStr(s []string, v string) bool {
	i := sort.SearchStrings(s, v)
	return i < len(s) && s[i] == v
}

func containsInt64(s []int64, v int64) bool {
	i := sort.Search(len(s), func(i int) bool { return s[i] >= v })
	return i < len(s) && s[i] == v
}

// fnv1a32 — FNV-1a; identical to internal/config's shard hash so flag
// bucketing and shard assignment share one hash family.
func fnv1a32(s string) uint32 {
	const prime = 16777619
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * prime
	}
	return h
}
