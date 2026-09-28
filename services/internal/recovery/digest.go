// Package recovery — Phase-04 Tasks 4.3.10/4.3.11/4.3.12: crash-recovery
// and DR orchestration, the time-boxed pre-open audit, and per-shard
// scoped reopen.
//
// This file (Task 4.3.11) implements the running-digest layer of the
// audit: per-shard checkpoints written every OrchDigestInterval trades
// (spec §18.6.7 "running digests"), persisted to the recovery_digests
// table (migration 092). During pre-open audit the engine verifies the
// digest chain and recomputes only the post-checkpoint window; a digest
// mismatch falls back to the full scan for that shard only.
//
// Hash discipline: both digests are SHA-256 over a canonical text
// encoding of the sorted vector they cover — deterministic across
// processes and runs, so a checkpoint written pre-crash verifies byte-
// identical post-restart.
package recovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"exchange/pkg/decimal"
)

// OrchDigestInterval is the checkpoint cadence: one digest row per shard
// per 1,000 executed trades (spec §18.6.7).
const OrchDigestInterval uint64 = 1000

// OrchDigest is one checkpoint row (recovery_digests, migration 092).
type OrchDigest struct {
	ShardID          int
	CheckpointSeq    uint64 // trade-count watermark; multiple of OrchDigestInterval
	JournalSeq       uint64 // journal_entries.id watermark — GL window scan cursor
	BookSeq          uint64 // engine book_seq watermark at the checkpoint
	GLZeroSumHash    string // OrchGLZeroSumHash over the window's GL sums
	BalanceDeltaHash string // OrchBalanceDeltaHash over the window's deltas
	TradeCount       uint64 // cumulative trades covered at checkpoint
	CreatedAt        time.Time
}

// OrchGLSum is the per-currency GL debit/credit pair feeding stage 1.
type OrchGLSum struct {
	Currency string
	Debits   decimal.Decimal
	Credits  decimal.Decimal
}

// OrchBalanceDelta is one account-code net movement inside a checkpoint
// window (credit - debit over ledger_lines in the window).
type OrchBalanceDelta struct {
	AccountCode string
	Currency    string
	Net         decimal.Decimal // signed
}

// orchDec normalizes a Decimal for canonical hashing: fixed 8dp (the
// exchange wire scale, spec §5.3) so textual and trailing-zero variance
// can never break digest equality.
func orchDec(d decimal.Decimal) string { return d.StringFixed(8) }

// OrchGLZeroSumHash computes the canonical digest of a GL sum vector:
// rows sorted by currency, encoded "CCY|debits|credits", newline-joined,
// SHA-256 hex.
func OrchGLZeroSumHash(sums []OrchGLSum) string {
	rows := make([]string, 0, len(sums))
	for _, s := range sums {
		rows = append(rows, s.Currency+"|"+orchDec(s.Debits)+"|"+orchDec(s.Credits))
	}
	sort.Strings(rows)
	return orchSHA256(strings.Join(rows, "\n"))
}

// OrchBalanceDeltaHash computes the canonical digest of a balance-delta
// vector: rows sorted by (account_code, currency), encoded
// "CODE|CCY|net", newline-joined, SHA-256 hex.
func OrchBalanceDeltaHash(deltas []OrchBalanceDelta) string {
	rows := make([]string, 0, len(deltas))
	for _, d := range deltas {
		rows = append(rows, d.AccountCode+"|"+d.Currency+"|"+orchDec(d.Net))
	}
	sort.Strings(rows)
	return orchSHA256(strings.Join(rows, "\n"))
}

func orchSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---------------------------------------------------------------------------
// Digest store (migration 092) — pluggable so audits run in-memory in tests.
// ---------------------------------------------------------------------------

// OrchDigestStore is the persistence seam for recovery_digests.
type OrchDigestStore interface {
	// LatestDigest returns the newest checkpoint for the shard.
	// found=false means "no digests yet" — NOT an error (first boot).
	LatestDigest(ctx context.Context, shardID int) (d OrchDigest, found bool, err error)
	// StoreDigest upserts one checkpoint row (UNIQUE shard_id,checkpoint_seq).
	StoreDigest(ctx context.Context, d OrchDigest) error
	// DigestsSince lists checkpoints with checkpoint_seq >= from, ascending.
	DigestsSince(ctx context.Context, shardID int, fromCheckpoint uint64) ([]OrchDigest, error)
}

// ---------------------------------------------------------------------------
// Chain verification
// ---------------------------------------------------------------------------

// OrchDigestVerdict is the outcome of verifying a shard's digest chain.
type OrchDigestVerdict struct {
	OK              bool
	CheckpointSeq   uint64 // newest verified checkpoint (0 when none)
	JournalSeq      uint64 // GL scan cursor at the verified checkpoint
	BookSeq         uint64
	FirstDivergent  uint64 // first checkpoint_seq that broke the chain
	Reason          string
	DigestsExamined int
}

// OrchVerifyDigestChain checks structural integrity of a digest chain:
// checkpoints strictly increase, spacing never exceeds OrchDigestInterval
// (a gap means a checkpoint was lost — the chain can no longer be trusted
// to cover history), trade_count is monotonic, and hashes are present.
// digests must be ascending by checkpoint_seq.
func OrchVerifyDigestChain(digests []OrchDigest) OrchDigestVerdict {
	v := OrchDigestVerdict{OK: true}
	if len(digests) == 0 {
		return v // no chain yet — nothing to distrust; audit does a full scan
	}
	var prev uint64
	for i, d := range digests {
		v.DigestsExamined++
		fail := func(reason string) OrchDigestVerdict {
			v.OK = false
			v.Reason = reason
			v.FirstDivergent = d.CheckpointSeq
			return v
		}
		if d.CheckpointSeq == 0 {
			return fail("checkpoint_seq must be > 0")
		}
		if i > 0 {
			if d.CheckpointSeq <= prev {
				return fail("checkpoint_seq not strictly increasing")
			}
			if d.CheckpointSeq-prev > OrchDigestInterval {
				return fail(fmt.Sprintf("checkpoint gap %d -> %d exceeds interval %d",
					prev, d.CheckpointSeq, OrchDigestInterval))
			}
		}
		if len(d.GLZeroSumHash) != 64 || len(d.BalanceDeltaHash) != 64 {
			return fail("digest hash field malformed")
		}
		prev = d.CheckpointSeq
	}
	last := digests[len(digests)-1]
	v.CheckpointSeq = last.CheckpointSeq
	v.JournalSeq = last.JournalSeq
	v.BookSeq = last.BookSeq
	return v
}

// ---------------------------------------------------------------------------
// Checkpointer — the write-side seam driven by the trade pipeline.
// ---------------------------------------------------------------------------

// OrchDigestDataSource is the windowed recompute provider the checkpointer
// and the audit fallback share. sinceJournalSeq = 0 means "from genesis"
// (full scan); >0 means "since that journal watermark" (window scan).
type OrchDigestDataSource interface {
	// GLSums returns per-currency debit/credit totals over ledger lines in
	// journals with id > sinceJournalSeq (all lines when sinceJournalSeq=0).
	GLSums(ctx context.Context, shardID int, sinceJournalSeq uint64) ([]OrchGLSum, error)
	// BalanceDeltas returns per-account-code net movement over the window.
	BalanceDeltas(ctx context.Context, shardID int, sinceJournalSeq uint64) ([]OrchBalanceDelta, error)
	// BookSeqWatermark returns the engine's current book_seq for the shard
	// (the WAL-tail cursor it has consumed).
	BookSeqWatermark(ctx context.Context, shardID int) (uint64, error)
	// JournalWatermark returns the current journal_entries.id high-water
	// mark — the cursor the next window resumes from.
	JournalWatermark(ctx context.Context, shardID int) (uint64, error)
}

// OrchCheckpointer writes a digest row each time the shard's trade count
// crosses a multiple of OrchDigestInterval.
type OrchCheckpointer struct {
	Store OrchDigestStore
	Data  OrchDigestDataSource
	Now   func() time.Time // injectable for tests; nil → time.Now
}

func (c *OrchCheckpointer) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// MaybeCheckpoint records a digest when tradeCount is an exact interval
// boundary. Returns (digest, written, err); written=false with nil error
// means "not at a boundary". Fail-closed: a store/recompute error is
// returned to the caller, never swallowed.
func (c *OrchCheckpointer) MaybeCheckpoint(ctx context.Context, shardID int, tradeCount uint64) (OrchDigest, bool, error) {
	var zero OrchDigest
	if tradeCount == 0 || tradeCount%OrchDigestInterval != 0 {
		return zero, false, nil
	}
	prev, found, err := c.Store.LatestDigest(ctx, shardID)
	if err != nil {
		return zero, false, fmt.Errorf("digest checkpoint: latest: %w", err)
	}
	var since uint64
	if found {
		since = prev.JournalSeq
	}
	sums, err := c.Data.GLSums(ctx, shardID, since)
	if err != nil {
		return zero, false, fmt.Errorf("digest checkpoint: gl sums: %w", err)
	}
	// Fail-closed: never checkpoint an imbalanced window. A stored digest
	// certifies "this window was zero-sum at checkpoint time", which is
	// exactly what lets the audit trust pre-checkpoint history.
	for _, s := range sums {
		if !s.Debits.Equal(s.Credits) {
			return zero, false, fmt.Errorf(
				"digest checkpoint: window debits %s != credits %s for %s — refusing to certify imbalance",
				s.Debits, s.Credits, s.Currency)
		}
	}
	deltas, err := c.Data.BalanceDeltas(ctx, shardID, since)
	if err != nil {
		return zero, false, fmt.Errorf("digest checkpoint: balance deltas: %w", err)
	}
	bookSeq, err := c.Data.BookSeqWatermark(ctx, shardID)
	if err != nil {
		return zero, false, fmt.Errorf("digest checkpoint: book seq: %w", err)
	}
	journalSeq, err := c.Data.JournalWatermark(ctx, shardID)
	if err != nil {
		return zero, false, fmt.Errorf("digest checkpoint: journal seq: %w", err)
	}
	d := OrchDigest{
		ShardID:          shardID,
		CheckpointSeq:    tradeCount,
		JournalSeq:       journalSeq,
		BookSeq:          bookSeq,
		GLZeroSumHash:    OrchGLZeroSumHash(sums),
		BalanceDeltaHash: OrchBalanceDeltaHash(deltas),
		TradeCount:       tradeCount,
		CreatedAt:        c.now(),
	}
	if err := c.Store.StoreDigest(ctx, d); err != nil {
		return zero, false, fmt.Errorf("digest checkpoint: store: %w", err)
	}
	return d, true, nil
}

// ---------------------------------------------------------------------------
// Digest verification against live data (audit fast path)
// ---------------------------------------------------------------------------

// OrchVerifyShardDigest verifies the newest digest for a shard by
// recomputing the window between the previous checkpoint and the latest
// one, and comparing hashes. Structural chain problems are checked across
// the whole chain; only the tail window is recomputed (the point of the
// design: audit verifies digests instead of rescanning history).
//
// Returns:
//   - verdict.OK, checkpoint cursor → audit scans only the tail window
//   - !verdict.OK → digest mismatch; audit must full-scan THIS shard
//   - err → store/data failure (fail closed — treat as mismatch upstream)
func OrchVerifyShardDigest(ctx context.Context, shardID int, store OrchDigestStore, data OrchDigestDataSource) (OrchDigestVerdict, error) {
	if store == nil || data == nil {
		// No digest infra → nothing to distrust; CheckpointSeq=0 directs
		// the caller to a full scan.
		return OrchDigestVerdict{OK: true}, nil
	}
	chain, err := store.DigestsSince(ctx, shardID, 0)
	if err != nil {
		return OrchDigestVerdict{}, fmt.Errorf("digest verify: load chain: %w", err)
	}
	v := OrchVerifyDigestChain(chain)
	if !v.OK {
		return v, nil
	}
	if len(chain) == 0 {
		return v, nil // OK with CheckpointSeq=0 → caller does full scan
	}
	last := chain[len(chain)-1]

	// Window to recompute = (previous checkpoint journal_seq, latest journal_seq].
	var prevJournal uint64
	if len(chain) >= 2 {
		prevJournal = chain[len(chain)-2].JournalSeq
	}
	sums, err := data.GLSums(ctx, shardID, prevJournal)
	if err != nil {
		return OrchDigestVerdict{}, fmt.Errorf("digest verify: gl sums window: %w", err)
	}
	if got := OrchGLZeroSumHash(sums); got != last.GLZeroSumHash {
		v.OK = false
		v.Reason = "gl_zero_sum_hash mismatch at latest checkpoint"
		v.FirstDivergent = last.CheckpointSeq
		return v, nil
	}
	deltas, err := data.BalanceDeltas(ctx, shardID, prevJournal)
	if err != nil {
		return OrchDigestVerdict{}, fmt.Errorf("digest verify: deltas window: %w", err)
	}
	if got := OrchBalanceDeltaHash(deltas); got != last.BalanceDeltaHash {
		v.OK = false
		v.Reason = "balance_delta_hash mismatch at latest checkpoint"
		v.FirstDivergent = last.CheckpointSeq
		return v, nil
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// In-memory digest store (tests, dev wiring)
// ---------------------------------------------------------------------------

// OrchMemDigestStore is a deterministic in-memory OrchDigestStore.
type OrchMemDigestStore struct {
	mu   chan struct{}
	rows map[int][]OrchDigest // per shard, ascending checkpoint_seq
}

func NewOrchMemDigestStore() *OrchMemDigestStore {
	m := &OrchMemDigestStore{mu: make(chan struct{}, 1), rows: map[int][]OrchDigest{}}
	m.mu <- struct{}{}
	return m
}

func (m *OrchMemDigestStore) lock()   { <-m.mu }
func (m *OrchMemDigestStore) unlock() { m.mu <- struct{}{} }

func (m *OrchMemDigestStore) LatestDigest(_ context.Context, shardID int) (OrchDigest, bool, error) {
	m.lock()
	defer m.unlock()
	rows := m.rows[shardID]
	if len(rows) == 0 {
		return OrchDigest{}, false, nil
	}
	return rows[len(rows)-1], true, nil
}

func (m *OrchMemDigestStore) StoreDigest(_ context.Context, d OrchDigest) error {
	if d.ShardID < 0 || d.CheckpointSeq == 0 {
		return errors.New("digest store: shard_id < 0 or checkpoint_seq == 0")
	}
	m.lock()
	defer m.unlock()
	rows := m.rows[d.ShardID]
	for i := range rows {
		if rows[i].CheckpointSeq == d.CheckpointSeq {
			rows[i] = d // upsert
			m.rows[d.ShardID] = rows
			return nil
		}
	}
	rows = append(rows, d)
	sort.Slice(rows, func(i, j int) bool { return rows[i].CheckpointSeq < rows[j].CheckpointSeq })
	m.rows[d.ShardID] = rows
	return nil
}

func (m *OrchMemDigestStore) DigestsSince(_ context.Context, shardID int, from uint64) ([]OrchDigest, error) {
	m.lock()
	defer m.unlock()
	var out []OrchDigest
	for _, d := range m.rows[shardID] {
		if d.CheckpointSeq >= from {
			out = append(out, d)
		}
	}
	return out, nil
}
