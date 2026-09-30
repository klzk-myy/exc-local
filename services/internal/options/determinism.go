// determinism.go — Phase-22 Task 22.3.15 items 3–5: the deterministic
// decision functions downstream lifecycle code (Task 22.3.10 option
// lifecycle, Task 22.3.3 NDF/roll, Phase-19.5 staleness gates) calls.
// Every function is pure: identical inputs always produce identical
// outputs, and every decision carries an audit-friendly reason code
// (spec §15.7 item 3, §24 #346).
//
// Contents:
//   - AssignWriters — pro-rata writer assignment by open interest
//     with a seeded deterministic tie-break (§15.4 amendment).
//   - MarkStale — the 5s staleness predicate producers apply when
//     building MarkTick.Stale for the sibling-owned BarrierMonitor
//     (barrier.go, Task 22.3.5; a stale feed never fabricates a
//     knock, §15.7 item 3).
//   - DecideExercise — manual/auto exercise decision under the
//     15:00 UTC expiry-day cutoff (§15.4, §15.6 item 2).
//   - DecideRoll — expiry/roll coordination: spread tolerance,
//     same-day-expiry conflict, close+open vs auto-settle (§15.7 item 5).
//
// Error-code constants for the §23 codes this package emits live in
// the sibling types.go block; the two lifecycle codes below are
// spec-registered but not yet in that block.
package options

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"time"

	excerrors "exchange/pkg/errors"
)

// Lifecycle codes emitted here — both are spec §23 rows (HTTP 409);
// the rest of the package's codes live in the types.go block.
const (
	// CodeExerciseCutoffPassed — manual exercise at/after the 15:00
	// UTC expiry-day cutoff (spec §23, §15.4).
	CodeExerciseCutoffPassed = "EXERCISE_CUTOFF_PASSED"
	// CodeOptionAssignmentFailed — writer assignment could not cover
	// the exercised quantity (spec §23, Phase-22 Task 22.3.10).
	CodeOptionAssignmentFailed = "OPTION_ASSIGNMENT_FAILED"
)

// Both lifecycle codes are already in types.go's DeclaredCodes — the
// wiring layer asserts the whole set via errs.Registry.CheckRegistered
// at startup. Input guards here use invalidInput →
// OPTION_PRICING_INPUT_INVALID from that same block.

// ---------------------------------------------------------------------------
// Writer assignment (spec §15.4 amendment: pro-rata by OI + tie-break)
// ---------------------------------------------------------------------------

// OptionWriter is one assignable writer of the exercised series.
type OptionWriter struct {
	AccountID    int64
	OpenInterest int64 // contracts written, still open — must be >0
}

// Assignment is one writer's allocated share of an exercise batch.
type Assignment struct {
	AccountID      int64
	Quantity       int64  // total contracts assigned
	RemainderUnits int64  // extra units beyond the pro-rata floor
	TieBreak       bool   // remainder unit(s) won by seeded tie-break
	Reason         string // audit reason code
}

// Assignment reason codes — stable tokens for the audit trail.
const (
	AssignReasonProRata   = "PRO_RATA_OI"        // floor(qty·OI_i/ΣOI) share
	AssignReasonRemainder = "LARGEST_REMAINDER"  // leftover unit by largest remainder
	AssignReasonTieBreak  = "REMAINDER_TIEBREAK" // leftover unit by seeded hash order
)

// AssignWriters allocates exercised contracts pro-rata across writers
// by open interest: each writer's floor share is ⌊qty·OI_i/ΣOI⌋ and
// the leftover units go to the largest fractional remainders; equal
// remainders resolve by FNV-1a(seed‖accountID) order — the spec §15.4
// "pro-rata by open interest with random tie-break" made
// deterministic: same inputs + same seed → identical assignment.
//
// Integer arithmetic only — no float tie ambiguity. Failures:
// exercised ≤ 0 or ΣOI < exercised → OPTION_ASSIGNMENT_FAILED.
func AssignWriters(exercised int64, writers []OptionWriter, seed uint64) ([]Assignment, error) {
	if exercised <= 0 {
		return nil, excerrors.New(CodeOptionAssignmentFailed,
			"assignment: non-positive exercised quantity")
	}
	var total int64
	for _, w := range writers {
		if w.OpenInterest <= 0 {
			return nil, excerrors.New(CodeOptionAssignmentFailed,
				fmt.Sprintf("assignment: writer %d non-positive OI", w.AccountID))
		}
		total += w.OpenInterest
	}
	if len(writers) == 0 || total < exercised {
		return nil, excerrors.New(CodeOptionAssignmentFailed,
			fmt.Sprintf("assignment: exercised %d exceeds total OI %d", exercised, total))
	}

	// Deterministic scan order: OI desc, then hash tie-key asc — the
	// input slice order must never influence the outcome.
	type cand struct {
		w      OptionWriter
		floor  int64
		rem    int64
		tieKey uint64
	}
	cs := make([]cand, len(writers))
	var floors int64
	for i, w := range writers {
		exact := exercised * w.OpenInterest
		cs[i] = cand{
			w:      w,
			floor:  exact / total,
			rem:    exact % total,
			tieKey: assignTieKey(seed, w.AccountID),
		}
		floors += cs[i].floor
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].rem != cs[j].rem {
			return cs[i].rem > cs[j].rem
		}
		if cs[i].tieKey != cs[j].tieKey {
			return cs[i].tieKey < cs[j].tieKey
		}
		return cs[i].w.AccountID < cs[j].w.AccountID
	})

	leftover := exercised - floors
	byAcct := make(map[int64]*Assignment, len(cs))
	out := make([]Assignment, 0, len(cs))
	// Equal remainders are adjacent after the sort; a remainder unit
	// granted while another candidate holds the same remainder value
	// is a tie-break grant.
	grantIdx := 0
	for leftover > 0 && grantIdx < len(cs) {
		c := &cs[grantIdx]
		tie := grantIdx+1 < len(cs) && cs[grantIdx+1].rem == c.rem && c.rem > 0 ||
			grantIdx-1 >= 0 && cs[grantIdx-1].rem == c.rem && c.rem > 0
		c.floor++
		leftover--
		a := &Assignment{
			AccountID:      c.w.AccountID,
			Quantity:       c.floor,
			RemainderUnits: 1,
			TieBreak:       tie,
			Reason:         AssignReasonRemainder,
		}
		if tie {
			a.Reason = AssignReasonTieBreak
		}
		byAcct[c.w.AccountID] = a
		grantIdx++
	}
	for _, c := range cs {
		if a, ok := byAcct[c.w.AccountID]; ok {
			out = append(out, *a)
			continue
		}
		if c.floor == 0 {
			continue // no allocation — zero-quantity rows are never emitted
		}
		out = append(out, Assignment{
			AccountID: c.w.AccountID,
			Quantity:  c.floor,
			Reason:    AssignReasonProRata,
		})
	}
	// Canonical output order: AccountID ascending — deterministic and
	// index-independent.
	sort.Slice(out, func(i, j int) bool { return out[i].AccountID < out[j].AccountID })
	return out, nil
}

// assignTieKey is the deterministic pseudo-random tie-break key:
// FNV-1a-64 over seed‖accountID. No math/rand — replay-safe.
func assignTieKey(seed uint64, accountID int64) uint64 {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], seed)
	h.Write(b[:])
	binary.LittleEndian.PutUint64(b[:], uint64(accountID))
	h.Write(b[:])
	return h.Sum64()
}

// ---------------------------------------------------------------------------
// Barrier monitoring (spec §15.7 item 3: discrete mark ticks, 5s gate)
// ---------------------------------------------------------------------------

// MarkStalenessGate is the Phase-19.5 mark-price staleness gate —
// a tick whose age at evaluation time exceeds it cannot trigger a
// knock (spec §15.7 item 3).
const MarkStalenessGate = 5 * time.Second

// MarkStale is the pure staleness predicate the tick consumer applies
// when building a MarkTick for the sibling BarrierMonitor
// (barrier.go): Stale = MarkStale(now, tick.At). Both directions are
// gated — a mark dated beyond the gate into the future is as suspect
// as a lagged one (clock-skew fail-closed, spec §2.7). Deterministic:
// same (now, asOf) → same verdict.
func MarkStale(now, asOf time.Time) bool {
	age := now.Sub(asOf)
	return age > MarkStalenessGate || age < -MarkStalenessGate
}

// ---------------------------------------------------------------------------
// Exercise decision (spec §15.4 cutoff + auto-exercise, §15.6 item 2)
// ---------------------------------------------------------------------------

// ExerciseCutoffHourUTC is the canonical expiry-day cutoff hour —
// spec §15.4 pins manual exercise acceptance to 15:00 UTC.
const ExerciseCutoffHourUTC = 15

// AutoExerciseITMFraction is the spec §15.4 auto-exercise threshold:
// options ≥0.5% in-the-money vs mark auto-exercise at expiry.
const AutoExerciseITMFraction = 0.005

// ExerciseCutoff returns the 15:00 UTC instant on the expiry day.
func ExerciseCutoff(expiryDay time.Time) time.Time {
	d := expiryDay.UTC()
	return time.Date(d.Year(), d.Month(), d.Day(),
		ExerciseCutoffHourUTC, 0, 0, 0, time.UTC)
}

// ExerciseInstruction is the holder's standing instruction at
// decision time.
type ExerciseInstruction int

const (
	InstrNone          ExerciseInstruction = iota // no instruction on file
	InstrExercise                                 // manual exercise instruction
	InstrDoNotExercise                            // explicit opt-out
)

// ExerciseAction is the decider's verdict.
type ExerciseAction int

const (
	ExercisePending   ExerciseAction = iota // before expiry cutoff — nothing due
	ExerciseManual                          // holder instruction accepted
	ExerciseAuto                            // auto-exercise (ITM ≥ 0.5%)
	ExerciseExpireOTM                       // expired out-of-the-money
	ExerciseOptedOut                        // do-not-exercise honored
)

func (a ExerciseAction) String() string {
	switch a {
	case ExercisePending:
		return "PENDING"
	case ExerciseManual:
		return "MANUAL"
	case ExerciseAuto:
		return "AUTO"
	case ExerciseExpireOTM:
		return "EXPIRE_OTM"
	case ExerciseOptedOut:
		return "OPTED_OUT"
	default:
		return "UNKNOWN"
	}
}

// ExerciseDecision is the audit-friendly output.
type ExerciseDecision struct {
	Action ExerciseAction
	Reason string
}

// Exercise reason codes.
const (
	ExerciseReasonBeforeCutoff   = "BEFORE_EXPIRY_CUTOFF"
	ExerciseReasonManualAccepted = "MANUAL_EXERCISE_ACCEPTED"
	ExerciseReasonOptOut         = "HOLDER_OPTED_OUT"
	ExerciseReasonAutoITM        = "AUTO_EXERCISE_ITM"
	ExerciseReasonOTM            = "EXPIRED_OTM"
)

// DecideExercise resolves what happens to one option position at one
// instant. Manual instructions (Exercise / DoNotExercise) are honored
// only if submitted before the 15:00 UTC expiry-day cutoff — at or
// after the cutoff the instruction is refused with
// EXERCISE_CUTOFF_PASSED (the amend/cancel window closes with it).
// With no instruction, positions at-or-past cutoff auto-exercise when
// ITM ≥ 0.5% vs mark and expire OTM otherwise.
func DecideExercise(now time.Time, expiryDay time.Time, put bool,
	strike, mark float64, instr ExerciseInstruction) (ExerciseDecision, error) {
	if !finite(strike) || !finite(mark) || strike <= 0 || mark <= 0 {
		return ExerciseDecision{}, invalidInput("DecideExercise", fmt.Sprintf(
			"non-positive/non-finite strike=%v mark=%v", strike, mark))
	}
	cutoff := ExerciseCutoff(expiryDay)

	if instr == InstrExercise || instr == InstrDoNotExercise {
		if !now.Before(cutoff) {
			return ExerciseDecision{}, excerrors.New(CodeExerciseCutoffPassed,
				"exercise instruction at/after the 15:00 UTC expiry cutoff")
		}
		if instr == InstrExercise {
			return ExerciseDecision{Action: ExerciseManual,
				Reason: ExerciseReasonManualAccepted}, nil
		}
		return ExerciseDecision{Action: ExerciseOptedOut,
			Reason: ExerciseReasonOptOut}, nil
	}
	if instr != InstrNone {
		return ExerciseDecision{}, invalidInput("DecideExercise",
			fmt.Sprintf("unknown instruction %d", int(instr)))
	}

	if now.Before(cutoff) {
		return ExerciseDecision{Action: ExercisePending,
			Reason: ExerciseReasonBeforeCutoff}, nil
	}
	// Auto-exercise evaluation at/after cutoff.
	itm := mark/strike - 1.0
	if put {
		itm = -itm
	}
	if itm >= AutoExerciseITMFraction {
		return ExerciseDecision{Action: ExerciseAuto,
			Reason: ExerciseReasonAutoITM}, nil
	}
	return ExerciseDecision{Action: ExerciseExpireOTM,
		Reason: ExerciseReasonOTM}, nil
}

// ---------------------------------------------------------------------------
// Expiry/roll coordination (spec §15.7 item 5)
// ---------------------------------------------------------------------------

// RollInput is the deterministic decision basis for one forward/
// option position approaching maturity. The funding sequence itself
// (nostro dispatch, value dates) is Phase-24's execution leg — this
// function only decides WHICH path the lifecycle takes.
type RollInput struct {
	SpreadBps           float64 // quoted roll spread, basis points
	SpreadToleranceBps  float64 // configured tolerance ceiling, ≥0
	SameDayOptionExpiry bool    // an option on the same book expires today
	PhysicalDelivery    bool    // instrument settles physically (not cash-settled)
}

// RollAction is the coordinated outcome.
type RollAction int

const (
	// RollCloseOpen: standard roll — close the maturing leg and open
	// the deferred leg preserving the trade group.
	RollCloseOpen RollAction = iota
	// RollAutoSettle: forward-maturity cash auto-settle instead of a
	// physical-delivery funding sequence.
	RollAutoSettle
	// RollDeferExpiryConflict: a same-day option expiry must settle
	// first — the roll close+open defers to the next window.
	RollDeferExpiryConflict
	// RollRejectSpread: quoted spread outside tolerance — the roll is
	// refused, position stays to its natural settlement.
	RollRejectSpread
)

func (a RollAction) String() string {
	switch a {
	case RollCloseOpen:
		return "CLOSE_OPEN"
	case RollAutoSettle:
		return "AUTO_SETTLE"
	case RollDeferExpiryConflict:
		return "DEFER_EXPIRY_CONFLICT"
	case RollRejectSpread:
		return "REJECT_SPREAD"
	default:
		return "UNKNOWN"
	}
}

// RollDecision is the audit-friendly output.
type RollDecision struct {
	Action RollAction
	Reason string
}

// Roll reason codes.
const (
	RollReasonSpreadBreach   = "ROLL_SPREAD_TOLERANCE_BREACH"
	RollReasonExpiryConflict = "EXPIRY_SETTLES_FIRST"
	RollReasonCloseOpen      = "ROLL_CLOSE_OPEN_GROUPED"
	RollReasonCashAutoSettle = "CASH_AUTO_SETTLE"
)

// DecideRoll resolves the expiry/roll path with a fixed precedence —
// the same ordering every replay applies:
//  1. spread outside tolerance → reject (never roll into a bad price);
//  2. same-day option expiry → defer the roll behind the expiry
//     settlement (auto-roll vs same-day-expiry conflict, §15.7);
//  3. physical-delivery instrument → close+open preserving the trade
//     group (the Phase-24 nostro funding sequence runs on the close);
//  4. otherwise → forward-maturity cash auto-settle.
func DecideRoll(in RollInput) (RollDecision, error) {
	if !finite(in.SpreadBps) || !finite(in.SpreadToleranceBps) ||
		in.SpreadToleranceBps < 0 {
		return RollDecision{}, invalidInput("DecideRoll", fmt.Sprintf(
			"non-finite spread=%v or negative tolerance=%v",
			in.SpreadBps, in.SpreadToleranceBps))
	}
	if math.Abs(in.SpreadBps) > in.SpreadToleranceBps {
		return RollDecision{Action: RollRejectSpread,
			Reason: RollReasonSpreadBreach}, nil
	}
	if in.SameDayOptionExpiry {
		return RollDecision{Action: RollDeferExpiryConflict,
			Reason: RollReasonExpiryConflict}, nil
	}
	if in.PhysicalDelivery {
		return RollDecision{Action: RollCloseOpen,
			Reason: RollReasonCloseOpen}, nil
	}
	return RollDecision{Action: RollAutoSettle,
		Reason: RollReasonCashAutoSettle}, nil
}
