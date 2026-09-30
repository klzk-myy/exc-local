// Phase-21 Tasks 21.3.1/21.3.10 — Go-side publisher for the C++
// SanctionsHook. Confirmed screening outcomes are pushed to the
// coordination Redis so the in-process SanctionsCache on the matching
// thread (core/src/risk/SanctionsCache.cpp) can reject flagged accounts
// at order admission without a synchronous DB/Redis round-trip.
//
// Key scheme (consumed by SanctionsRefresher.cpp):
//
//	exc:sanctions:flagged:{account_id}    — "1" while the account is
//	                                      sanctions-flagged (blocked at
//	                                      order entry, fail closed)
//	exc:sanctions:flagged:cleared:{id}    — "1" for a flag that was
//	                                      affirmatively cleared by a
//	                                      compliance disposition
//	exc:sanctions:screened:bloom          — bitmap of accounts that have
//	                                      been screened at least once;
//	                                      the C++ cache distinguishes
//	                                      "screened clean" from "never
//	                                      screened" (unscreened +
//	                                      account-aware binding fails
//	                                      closed on a bound cache)
//	exc:sanctions:feed:heartbeat          — unix seconds; refresher
//	                                      staleness probe (publication
//	                                      cadence is the publisher's
//	                                      Heartbeat loop, ~10s)
//	exc:sanctions:flagged:set             — SCAN pagination anchor the
//	                                      C++ refresher walks (keyspace
//	                                      is the authority; this set is
//	                                      the SCAN-free flag inventory)
//
// Every publish is idempotent — SetFlag/ClearFlag overwrite, no
// dedupe. Fail-closed discipline is preserved end-to-end: a publisher
// that cannot reach Redis returns the error; a C++ cache whose
// heartbeat is stale marks itself unverifiable and rejects.
package compliance

import (
	"context"
	"fmt"
	"strconv"
	"time"

	exchredis "exchange/internal/redis"
)

// Redis key scheme constants — SanctionsCache.cpp parses the same
// prefixes; keep in lockstep.
const (
	SanctionsFlagPrefix   = "exc:sanctions:flagged:"
	SanctionsClearedMark  = "cleared:"
	SanctionsFlagSet      = "exc:sanctions:flagged:set"
	SanctionsScreenedKey  = "exc:sanctions:screened:bloom"
	SanctionsHeartbeatKey = "exc:sanctions:feed:heartbeat"
	sanctionsFlagValue    = "1"
)

// HeartbeatInterval is the publisher's heartbeat cadence — well under
// the C++ staleness window (60s default) while keeping coordination-DB
// chatter negligible.
const HeartbeatInterval = 10 * time.Second

// FlagPublisher writes per-account sanctions flags + the screened
// bitmap + the feed heartbeat to the coordination Redis. Nil-safe:
// with a nil client every method no-ops (file-backed dev binding may
// run without a matching core attached — quarantine/blocking still
// enforced on the Go side).
type FlagPublisher struct {
	r   *exchredis.Client
	log func(format string, args ...any)
	now func() time.Time
}

// NewFlagPublisher binds the coordination client (may be nil → no-op).
func NewFlagPublisher(r *exchredis.Client) *FlagPublisher {
	return &FlagPublisher{r: r,
		log: func(string, ...any) {},
		now: func() time.Time { return time.Now().UTC() }}
}

// WithLogger wires a diagnostic sink.
func (p *FlagPublisher) WithLogger(f func(format string, args ...any)) *FlagPublisher {
	if f != nil {
		p.log = f
	}
	return p
}

// WithClock injects the clock (tests).
func (p *FlagPublisher) WithClock(now func() time.Time) *FlagPublisher {
	p.now = now
	return p
}

// Bound reports whether the publisher has a live client — false means
// the Go-side remains authoritative but the C++ hook is detached
// (operator-visible via status surfaces).
func (p *FlagPublisher) Bound() bool { return p != nil && p.r != nil }

func flagKey(accountID int64) string {
	return SanctionsFlagPrefix + strconv.FormatInt(accountID, 10)
}

func clearedKey(accountID int64) string {
	return SanctionsFlagPrefix + SanctionsClearedMark + strconv.FormatInt(accountID, 10)
}

// FlagAccount marks an account sanctions-flagged — the matching core
// rejects its orders on the next snapshot refresh. reason is recorded
// in the flag value's companion detail (audit surface keeps the full
// hit detail; the Redis value is deliberately just "1" plus the flag
// inventory set).
func (p *FlagPublisher) FlagAccount(ctx context.Context, accountID int64,
	reason string) error {
	if !p.Bound() {
		return nil
	}
	key := flagKey(accountID)
	if err := p.r.Set(ctx, key, sanctionsFlagValue, 0).Err(); err != nil {
		return fmt.Errorf("sanctions flag set account %d: %w", accountID, err)
	}
	if err := p.r.Del(ctx, clearedKey(accountID)).Err(); err != nil {
		return fmt.Errorf("sanctions flag clear-marker account %d: %w",
			accountID, err)
	}
	if err := p.r.SAdd(ctx, SanctionsFlagSet,
		strconv.FormatInt(accountID, 10)).Err(); err != nil {
		return fmt.Errorf("sanctions flag inventory account %d: %w",
			accountID, err)
	}
	p.log("sanctions: flagged account %d (%s)", accountID, reason)
	return nil
}

// ClearFlag drops the flagged key + sets the cleared marker + removes
// the inventory entry — compliance disposition path only. The cleared
// marker lets a refreshed snapshot distinguish "never flagged" from
// "flag cleared" for audit trails.
func (p *FlagPublisher) ClearFlag(ctx context.Context, accountID int64) error {
	if !p.Bound() {
		return nil
	}
	if err := p.r.Del(ctx, flagKey(accountID)).Err(); err != nil {
		return fmt.Errorf("sanctions flag clear account %d: %w", accountID, err)
	}
	if err := p.r.Set(ctx, clearedKey(accountID), sanctionsFlagValue,
		0).Err(); err != nil {
		return fmt.Errorf("sanctions cleared marker account %d: %w",
			accountID, err)
	}
	if err := p.r.SRem(ctx, SanctionsFlagSet,
		strconv.FormatInt(accountID, 10)).Err(); err != nil {
		return fmt.Errorf("sanctions flag inventory clear account %d: %w",
			accountID, err)
	}
	p.log("sanctions: cleared flag account %d", accountID)
	return nil
}

// MarkScreened flips the account's screened-bitmap bit — set for every
// account that completed a clean screen (onboarding, deposit/withdrawal
// screening, replay rescreen). The C++ cache treats "bit set + no flag"
// as screened-clean; "no bit" as never-screened.
func (p *FlagPublisher) MarkScreened(ctx context.Context, accountID int64) error {
	if !p.Bound() {
		return nil
	}
	if err := p.r.SetBit(ctx, SanctionsScreenedKey, accountID, 1).Err(); err != nil {
		return fmt.Errorf("sanctions screened bit account %d: %w",
			accountID, err)
	}
	return nil
}

// Heartbeat refreshes the feed-liveness key — called on the publisher
// loop and after every vendor refresh so the C++ refresher's staleness
// gate tracks the Go-side pipeline, not wall-clock guesses.
func (p *FlagPublisher) Heartbeat(ctx context.Context) error {
	if !p.Bound() {
		return nil
	}
	return p.r.Set(ctx, SanctionsHeartbeatKey,
		strconv.FormatInt(p.now().Unix(), 10), 0).Err()
}

// HeartbeatLoop publishes the heartbeat every HeartbeatInterval until
// ctx ends — started once at wiring when a flag publisher is bound.
func (p *FlagPublisher) HeartbeatLoop(ctx context.Context) {
	if !p.Bound() {
		return
	}
	t := time.NewTicker(HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.Heartbeat(ctx); err != nil {
				p.log("sanctions: heartbeat publish: %v", err)
			}
		}
	}
}
