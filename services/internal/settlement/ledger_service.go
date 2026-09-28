// ledger_service.go — Double-Entry General Ledger Posting Service
// (Phase-03 Task 3.3.6; spec §5.21 + §5.3 + §5.40, §24 #121/#301).
//
// DoubleEntryLedgerService is THE posting contract for every balance
// mutation in the suite (trade fill, fee, deposit, withdrawal, EOD
// rollover, liquidation, internal transfer — spec §5.3 invariant 4 bans
// any GL bypass). One Post call is:
//
//	validate journal (structural + zero-sum per currency)
//	  → acquire account:lock:{id} Redis mutexes (sorted, deadlock-safe)
//	  → BEGIN ISOLATION LEVEL SERIALIZABLE
//	  → INSERT journal_entries (+ payload_sha256 for replay checks)
//	  → resolve every ledger_lines.account_code against chart_of_accounts
//	    (unknown → LEDGER_UNKNOWN_ACCOUNT, tx aborted — fail-closed)
//	  → INSERT ledger_lines (pure debit XOR credit)
//	  → in-tx re-verify SUM(debits)==SUM(credits) per currency
//	  → per AccountEffect: upsert + SELECT ... FOR UPDATE the balances row,
//	    validate non-negative components, update versioned row,
//	    INSERT ledger_entries (append-only, running_balance), UPSERT
//	    journal_sums, then assert journal_sums.net_balance == balances.total
//	  → COMMIT  (the deferred gl_journal_zero_sum_chk trigger re-checks
//	    the zero-sum invariant at commit — DB-enforced, not just app-level)
//	  → dispatch BalanceChanged to NATS account.balance.changed.{account_id}
//
// Retries: SQLSTATE 40001/40P01 get exponential backoff + jitter
// (5/15/45ms, max 3 attempts — spec §5.40) then
// TRANSACTION_CONFLICT_RETRY_EXHAUSTED. Idempotency: a non-empty
// IdempotencyKey replays to the original committed journal; a replayed key
// with a different payload is IDEMPOTENCY_KEY_MISMATCH.
//
// DEPENDENCY (documented for the wiring wave): ledger_entries and
// journal_sums are the spec §5.3 tables created by migration 102
// (wallets_ledger_entries — Phase-01 doc assigns it to this task /
// Phase-04 Task 4.3.9). Postings that carry Effects require those tables;
// a posting without Effects touches only the §5.21 GL tables created by
// migration 036. Migration 102 must also carry ledger_entries.journal_entry_id
// (BIGINT NULL) and a generated journal_sums.net_balance — the service
// asserts net_balance == balances.total in-transaction, so a non-generated
// column fails loudly rather than silently.
package settlement

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excredis "exchange/internal/redis"
	excerrors "exchange/pkg/errors"
)

// Publisher dispatches a post-commit event to a subject — the seam where
// NATS JetStream (account.balance.changed.{account_id}) plugs in.
type Publisher interface {
	Publish(ctx context.Context, subject string, payload []byte) error
}

// NatsPublisher adapts a JetStream context to Publisher.
type NatsPublisher struct{ JS jetstream.JetStream }

// Publish sends the payload to subject and awaits the stream PubAck.
func (p NatsPublisher) Publish(ctx context.Context, subject string, payload []byte) error {
	if p.JS == nil {
		return fmt.Errorf("nats publisher: nil JetStream context")
	}
	_, err := p.JS.Publish(ctx, subject, payload)
	if err != nil {
		return fmt.Errorf("nats publish %q: %w", subject, err)
	}
	return nil
}

// LedgerService is the DoubleEntryLedgerService of spec §5.3. Construct it
// with NewLedgerService; the zero value is unusable (fail-closed).
type LedgerService struct {
	pool *pgxpool.Pool
	rdb  *excredis.Client
	pub  Publisher
	// maxAttempts bounds the §5.40 conflict-retry loop (default 3).
	maxAttempts int
	// backoff is the retry base schedule (default 5ms/15ms/45ms + jitter).
	backoff []time.Duration
	// dispatchAttempts bounds the post-commit event publish retry.
	dispatchAttempts int
	// lockTTL is the account:lock:{id} lease (canonical 10s — §5.3).
	lockTTL time.Duration
}

// DoubleEntryLedgerService is the spec §5.3 name for LedgerService —
// consumers referencing `DoubleEntryLedgerService::postJournal` should use
// (*DoubleEntryLedgerService).PostJournal (tx-scoped) or .Post (managed).
type DoubleEntryLedgerService = LedgerService

// NewLedgerService wires the posting service. pool is required. rdb is
// required whenever a journal carries wallet Effects (§5.3 mandates the
// Redis lock for balance mutations); journals that are pure GL postings
// may run with rdb nil. pub is required whenever Effects are posted —
// BalanceChanged dispatch on commit is a MUST, not best-effort.
func NewLedgerService(pool *pgxpool.Pool, rdb *excredis.Client, pub Publisher) (*LedgerService, error) {
	if pool == nil {
		return nil, fmt.Errorf("ledger service: nil pgx pool")
	}
	return &LedgerService{
		pool:             pool,
		rdb:              rdb,
		pub:              pub,
		maxAttempts:      3,
		backoff:          []time.Duration{5 * time.Millisecond, 15 * time.Millisecond, 45 * time.Millisecond},
		dispatchAttempts: 3,
		lockTTL:          excredis.AccountLockTTL,
	}, nil
}

// ---------------------------------------------------------------------------
// Post — the canonical entry point (spec's DoubleEntryLedgerService::postJournal
// wrapped with lock acquisition + retry + event dispatch).
// ---------------------------------------------------------------------------

// Post validates and durably posts journal j, mutating the wallet rows
// named by j.Effects and emitting BalanceChanged for each touched account.
//
// Error contract (spec §23): LEDGER_IMBALANCE_ABORT / LEDGER_UNKNOWN_ACCOUNT /
// LEDGER_INVALID_JOURNAL (500, L0-fatal class — never silently absorb),
// INSUFFICIENT_BALANCE (400), ACCOUNT_BUSY (423), IDEMPOTENCY_KEY_MISMATCH
// (409-class), TRANSACTION_CONFLICT_RETRY_EXHAUSTED (503),
// BALANCE_EVENT_DISPATCH_FAILED (committed=true — funds final, resync
// downstream).
func (s *LedgerService) Post(ctx context.Context, j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}

	// §5.3 locking protocol: Redis account mutexes BEFORE the transaction,
	// sorted for deterministic (deadlock-free) acquisition order.
	token, err := lockToken()
	if err != nil {
		return ledger.PostResult{}, fmt.Errorf("ledger: lock token: %w", err)
	}
	locked, err := s.lockAccounts(ctx, j.AffectedAccounts(), token)
	if err != nil {
		return ledger.PostResult{}, err
	}
	defer s.unlockAccounts(locked, token)

	var res ledger.PostResult
	var lastErr error
	for attempt := 0; attempt < s.maxAttempts; attempt++ {
		res, err = s.postOnce(ctx, j)
		if err == nil {
			break
		}
		lastErr = err
		if stderrors.Is(err, errIdempotentConflict) {
			// The failed tx is already rolled back inside postOnce;
			// resolve the committed original outside any tx.
			return s.resolveReplay(ctx, j)
		}
		if !isRetryableConflict(err) {
			return ledger.PostResult{}, err
		}
		if attempt+1 < s.maxAttempts {
			sleep := s.backoff[min(attempt, len(s.backoff)-1)]
			select {
			case <-ctx.Done():
				return ledger.PostResult{}, ctx.Err()
			case <-time.After(sleep + jitter(sleep)):
			}
		}
	}
	if err != nil {
		return ledger.PostResult{}, excerrors.Wrap(ledger.CodeTxnConflictExhausted,
			fmt.Sprintf("ledger: posting aborted after %d attempts", s.maxAttempts), lastErr)
	}
	res.Committed = true

	if err := s.dispatch(ctx, res.Events); err != nil {
		return res, err // committed — caller MUST NOT retry the write
	}
	return res, nil
}

// PostJournal is the tx-scoped half of the contract for callers already
// inside a SERIALIZABLE transaction that hold their own wallet locks and
// will dispatch the returned events themselves after their commit. It runs
// the identical write path as Post minus lock/retry/dispatch; the result's
// Committed stays false — the caller's commit decides finality.
func (s *LedgerService) PostJournal(ctx context.Context, tx pgx.Tx, j ledger.Journal) (ledger.PostResult, error) {
	if err := j.Validate(); err != nil {
		return ledger.PostResult{}, err
	}
	return postJournalTx(ctx, tx, j)
}

// ---------------------------------------------------------------------------
// Transaction body
// ---------------------------------------------------------------------------

// errIdempotentConflict marks a journal_entries.idempotency_key collision
// (SQLSTATE 23505 on journal_entries_idem_ux) — Post resolves it to the
// committed original outside the aborted tx.
var errIdempotentConflict = stderrors.New("idempotency conflict")

func (s *LedgerService) postOnce(ctx context.Context, j ledger.Journal) (ledger.PostResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ledger.PostResult{}, fmt.Errorf("ledger: begin serializable tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res, err := postJournalTx(ctx, tx, j)
	if err != nil {
		return ledger.PostResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ledger.PostResult{}, fmt.Errorf("ledger: commit: %w", err)
	}
	return res, nil
}

// postJournalTx writes journal_entries + ledger_lines + ledger_entries +
// journal_sums inside tx. Shared by Post (own tx) and PostJournal
// (caller's tx).
func postJournalTx(ctx context.Context, tx pgx.Tx, j ledger.Journal) (ledger.PostResult, error) {
	res := ledger.PostResult{}

	// 1. journal_entries — the idempotency UNIQUE index turns replays into
	//    a 23505 the caller resolves to the committed original.
	hash := journalHash(j)
	var journalID int64
	err := tx.QueryRow(ctx, `
		INSERT INTO journal_entries
		    (entry_type, reference_id, description, posted_by, idempotency_key, payload_sha256)
		VALUES ($1, NULLIF($2::bigint,0), $3, $4, NULLIF($5,''), $6)
		RETURNING id`,
		string(j.EntryType), j.ReferenceID, j.Description, j.PostedBy, j.IdempotencyKey, hash,
	).Scan(&journalID)
	if err != nil {
		if isIdemConflict(err) {
			return res, errIdempotentConflict
		}
		return res, fmt.Errorf("ledger: insert journal_entries: %w", err)
	}
	res.JournalID = journalID

	// 2. Resolve every line's account against chart_of_accounts —
	//    unknown codes abort fail-closed (Task 3.3.19 AC). The FK on
	//    ledger_lines backstops this; the app check produces the coded error.
	if err := resolveAccounts(ctx, tx, j); err != nil {
		return res, err
	}

	// 3. ledger_lines — each a pure debit XOR credit (CHECK enforced).
	for _, l := range j.Lines {
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO ledger_lines
			    (journal_entry_id, account_code, debit_amount, credit_amount, currency, narrative)
			VALUES ($1,$2,$3,$4,$5,NULLIF($6,''))
			RETURNING id`,
			journalID, l.AccountCode, l.Debit.String(), l.Credit.String(),
			l.Currency, l.Narrative,
		).Scan(&id); err != nil {
			return res, fmt.Errorf("ledger: insert ledger_lines (%s): %w", l.AccountCode, err)
		}
		res.LedgerLineIDs = append(res.LedgerLineIDs, id)
	}

	// 4. In-transaction zero-sum re-verification against what was actually
	//    stored — defense in depth under the deferred commit trigger.
	if err := verifyZeroSum(ctx, tx, journalID); err != nil {
		return res, err
	}

	// 5. Wallet effects — balances FOR UPDATE, ledger_entries append,
	//    journal_sums upsert, net_balance==total assertion (§5.3).
	for _, e := range j.Effects {
		ev, err := applyEffect(ctx, tx, journalID, j, e)
		if err != nil {
			return res, err
		}
		res.Events = append(res.Events, ev)
		if ev.LedgerEntryID != 0 {
			res.LedgerEntryIDs = append(res.LedgerEntryIDs, ev.LedgerEntryID)
		}
	}
	return res, nil
}

// resolveAccounts loads the chart rows for the journal's line codes and
// validates each line against them.
func resolveAccounts(ctx context.Context, tx pgx.Tx, j ledger.Journal) error {
	codes := make([]string, 0, len(j.Lines))
	seen := map[string]struct{}{}
	for _, l := range j.Lines {
		if _, ok := seen[l.AccountCode]; !ok {
			seen[l.AccountCode] = struct{}{}
			codes = append(codes, l.AccountCode)
		}
	}
	rows, err := tx.Query(ctx, `
		SELECT account_code, account_name, account_type::text, currency
		FROM chart_of_accounts WHERE account_code = ANY($1)`, codes)
	if err != nil {
		return fmt.Errorf("ledger: resolve accounts: %w", err)
	}
	defer rows.Close()
	accs := make([]ledger.Account, 0, len(codes))
	for rows.Next() {
		var a ledger.Account
		if err := rows.Scan(&a.Code, &a.Name, (*string)(&a.Type), &a.Currency); err != nil {
			return fmt.Errorf("ledger: scan account: %w", err)
		}
		accs = append(accs, a)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("ledger: resolve accounts: %w", err)
	}
	return j.ValidateAccounts(ledger.NewChart(accs))
}

// verifyZeroSum re-aggregates the stored lines per currency — the
// application-level mirror of the deferred gl_journal_zero_sum_chk trigger.
func verifyZeroSum(ctx context.Context, tx pgx.Tx, journalID int64) error {
	rows, err := tx.Query(ctx, `
		SELECT currency, SUM(debit_amount)::text, SUM(credit_amount)::text
		FROM ledger_lines WHERE journal_entry_id = $1 GROUP BY currency`, journalID)
	if err != nil {
		return fmt.Errorf("ledger: zero-sum verify: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ccy, dStr, cStr string
		if err := rows.Scan(&ccy, &dStr, &cStr); err != nil {
			return fmt.Errorf("ledger: zero-sum scan: %w", err)
		}
		d, err1 := decimal.NewFromString(dStr)
		c, err2 := decimal.NewFromString(cStr)
		if err1 != nil || err2 != nil {
			return fmt.Errorf("ledger: zero-sum decimal parse (%s/%s)", dStr, cStr)
		}
		if !d.Equal(c) {
			return excerrors.New(ledger.CodeLedgerImbalanceAbort, fmt.Sprintf(
				"journal %d currency %s: stored SUM(debits)=%s != SUM(credits)=%s",
				journalID, ccy, dStr, cStr))
		}
	}
	return rows.Err()
}

// applyEffect locks and mutates one balances row inside tx, appends the
// §5.3 ledger_entries row when money actually moved, keeps journal_sums in
// step, and builds the post-commit BalanceChanged event.
func applyEffect(ctx context.Context, tx pgx.Tx, journalID int64, j ledger.Journal, e ledger.AccountEffect) (ledger.BalanceEvent, error) {
	ev := ledger.BalanceEvent{AccountID: e.AccountID, Currency: e.Currency, JournalID: journalID, EventType: "BALANCE_CHANGED"}

	// First-touch posts may have no balances row yet — create the zero row
	// then lock it (INSERT ... ON CONFLICT is a no-op when it exists).
	if _, err := tx.Exec(ctx, `
		INSERT INTO balances (account_id, currency, available, locked)
		VALUES ($1,$2,0,0) ON CONFLICT (account_id, currency) DO NOTHING`,
		e.AccountID, e.Currency); err != nil {
		return ev, fmt.Errorf("ledger: ensure balances row acct=%d ccy=%s: %w", e.AccountID, e.Currency, err)
	}

	var avail, locked decimal.Decimal
	if err := tx.QueryRow(ctx, `
		SELECT available, locked FROM balances
		WHERE account_id = $1 AND currency = $2 FOR UPDATE`,
		e.AccountID, e.Currency).Scan(&avail, &locked); err != nil {
		return ev, fmt.Errorf("ledger: lock balances row acct=%d ccy=%s: %w", e.AccountID, e.Currency, err)
	}

	newAvail := avail.Add(e.AvailableDelta)
	newLocked := locked.Add(e.LockedDelta)
	if newLocked.IsNegative() {
		return ev, excerrors.New(ledger.CodeInsufficientBalance, fmt.Sprintf(
			"acct %d %s: locked %s + %s < 0 — locked funds can never go negative",
			e.AccountID, e.Currency, locked.String(), e.LockedDelta.String()))
	}
	if newAvail.IsNegative() && !e.AllowNegative {
		return ev, excerrors.New(ledger.CodeInsufficientBalance, fmt.Sprintf(
			"acct %d %s: available %s + %s < 0",
			e.AccountID, e.Currency, avail.String(), e.AvailableDelta.String()))
	}

	if _, err := tx.Exec(ctx, `
		UPDATE balances SET available = $3, locked = $4, version = version + 1
		WHERE account_id = $1 AND currency = $2`,
		e.AccountID, e.Currency, newAvail.String(), newLocked.String()); err != nil {
		return ev, fmt.Errorf("ledger: update balances acct=%d ccy=%s: %w", e.AccountID, e.Currency, err)
	}
	newTotal := newAvail.Add(newLocked)
	ev.Available, ev.Locked, ev.Total = newAvail.String(), newLocked.String(), newTotal.String()

	// ledger_entries + journal_sums only when money actually moved — a pure
	// available↔locked shift preserves both totals and adds no ledger row.
	net := e.Net()
	if net.IsZero() {
		return ev, nil
	}
	direction := "CREDIT"
	if net.IsPositive() {
		direction = "DEBIT" // §5.3: net_balance = debits − credits = total
	}
	var entryID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO ledger_entries
		    (entry_type, reference_id, account_id, currency, direction, amount,
		     running_balance, description, posted_by, journal_entry_id)
		VALUES ($1, NULLIF($2::bigint,0), $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id`,
		j.EntryType.LedgerEntryType(), j.ReferenceID, e.AccountID, e.Currency,
		direction, net.Abs().String(), newTotal.String(), j.Description, j.PostedBy, journalID,
	).Scan(&entryID); err != nil {
		return ev, fmt.Errorf("ledger: insert ledger_entries acct=%d: %w", e.AccountID, err)
	}
	ev.LedgerEntryID = entryID

	// journal_sums (§5.3 verification cache). net_balance is a GENERATED
	// column in the §5.3 schema (like balances.total in migration 004) — the
	// upsert maintains debits/credits/count and the assertion below proves
	// net_balance == balances.total inside the same transaction.
	debits, credits := decimal.Zero, decimal.Zero
	if direction == "DEBIT" {
		debits = net
	} else {
		credits = net.Neg()
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO journal_sums (account_id, currency, total_debits, total_credits, entry_count, last_entry_id)
		VALUES ($1,$2,$3,$4,1,$5)
		ON CONFLICT (account_id, currency) DO UPDATE SET
		    total_debits  = journal_sums.total_debits  + EXCLUDED.total_debits,
		    total_credits = journal_sums.total_credits + EXCLUDED.total_credits,
		    entry_count   = journal_sums.entry_count   + 1,
		    last_entry_id = EXCLUDED.last_entry_id`,
		e.AccountID, e.Currency, debits.String(), credits.String(), entryID); err != nil {
		return ev, fmt.Errorf("ledger: upsert journal_sums acct=%d: %w", e.AccountID, err)
	}
	var netBalance decimal.Decimal
	if err := tx.QueryRow(ctx, `
		SELECT net_balance FROM journal_sums
		WHERE account_id = $1 AND currency = $2`,
		e.AccountID, e.Currency).Scan(&netBalance); err != nil {
		return ev, fmt.Errorf("ledger: read journal_sums acct=%d: %w", e.AccountID, err)
	}
	if !netBalance.Equal(newTotal) {
		return ev, excerrors.New(ledger.CodeLedgerImbalanceAbort, fmt.Sprintf(
			"acct %d %s: journal_sums.net_balance=%s != balances.total=%s",
			e.AccountID, e.Currency, netBalance.String(), newTotal.String()))
	}
	return ev, nil
}

// ---------------------------------------------------------------------------
// Locking, retry, idempotent replay, event dispatch
// ---------------------------------------------------------------------------

func lockToken() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("ledger-%d-%s", time.Now().UnixNano(), hex.EncodeToString(b[:])), nil
}

// lockAccounts acquires account:lock:{id} for each id in sorted order.
// All-or-nothing: on any failure the already-held locks are released.
func (s *LedgerService) lockAccounts(ctx context.Context, ids []int64, token string) ([]int64, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if s.rdb == nil {
		return nil, excerrors.New(ledger.CodeLedgerLockUnavailable,
			"balance mutation requires the Redis account mutex backend (§5.3) — client is nil")
	}
	held := make([]int64, 0, len(ids))
	for _, id := range ids {
		ok, err := s.rdb.TryLockAccount(ctx, fmt.Sprint(id), token, s.lockTTL)
		if err != nil {
			s.unlockAccounts(held, token)
			return nil, excerrors.Wrap(ledger.CodeLedgerLockUnavailable,
				fmt.Sprintf("ledger: lock account %d", id), err)
		}
		if !ok {
			s.unlockAccounts(held, token)
			return nil, excerrors.New(ledger.CodeAccountBusy,
				fmt.Sprintf("ledger: account %d mutex held by another operation", id))
		}
		held = append(held, id)
	}
	return held, nil
}

func (s *LedgerService) unlockAccounts(ids []int64, token string) {
	if s.rdb == nil {
		return
	}
	// Release in reverse acquisition order on a fresh bounded context — the
	// request ctx may already be dead; token-checked compare-and-del means
	// a stale owner can never evict a live lock.
	for i := len(ids) - 1; i >= 0; i-- {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, _ = s.rdb.UnlockAccount(ctx, fmt.Sprint(ids[i]), token)
		cancel()
	}
}

// isRetryableConflict reports whether err is a §5.40 whole-transaction
// retry candidate (SQLSTATE 40001 serialization_failure / 40P01 deadlock).
func isRetryableConflict(err error) bool {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

// isIdemConflict reports whether err is the idempotency_key unique
// violation on journal_entries.
func isIdemConflict(err error) bool {
	var pgErr *pgconn.PgError
	if stderrors.As(err, &pgErr) {
		return pgErr.Code == "23505" && pgErr.ConstraintName == "journal_entries_idem_ux"
	}
	return false
}

// jitter returns [0, d) — decorrelated jitter for the §5.40 schedule.
func jitter(d time.Duration) time.Duration {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return d / 2 // crypto/rand failure must not deadlock the retry loop
	}
	n := int64(b[0]) | int64(b[1])<<8 | int64(b[2])<<16 | int64(b[3])<<24 |
		int64(b[4])<<32 | int64(b[5])<<40 | int64(b[6])<<48 | int64(b[7]&0x7f)<<56
	return time.Duration(n % int64(d))
}

// resolveReplay answers a replayed IdempotencyKey: same payload → return
// the committed journal id; different payload → IDEMPOTENCY_KEY_MISMATCH.
func (s *LedgerService) resolveReplay(ctx context.Context, j ledger.Journal) (ledger.PostResult, error) {
	var id int64
	var storedHash *string
	err := s.pool.QueryRow(ctx, `
		SELECT id, payload_sha256 FROM journal_entries WHERE idempotency_key = $1`,
		j.IdempotencyKey).Scan(&id, &storedHash)
	if err != nil {
		return ledger.PostResult{}, fmt.Errorf("ledger: resolve idempotent replay: %w", err)
	}
	if storedHash == nil || *storedHash != journalHash(j) {
		return ledger.PostResult{}, excerrors.New(ledger.CodeIdempotencyMismatch,
			"idempotency_key replayed with a different journal payload")
	}
	return ledger.PostResult{JournalID: id, Committed: true, Replayed: true}, nil
}

// journalHash is the canonical payload fingerprint stored on the journal
// row — replay compares bytes, not fields, so any semantic drift in the
// replayed request is caught.
func journalHash(j ledger.Journal) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%d|%s|%s|", j.EntryType, j.ReferenceID, j.Description, j.PostedBy)
	for _, l := range j.Lines {
		fmt.Fprintf(h, "%s:%s:%s:%s:%s;", l.AccountCode, l.Currency,
			l.Debit.String(), l.Credit.String(), l.Narrative)
	}
	h.Write([]byte("|"))
	for _, e := range j.Effects {
		fmt.Fprintf(h, "%d:%s:%s:%s:%t;", e.AccountID, e.Currency,
			e.AvailableDelta.String(), e.LockedDelta.String(), e.AllowNegative)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// dispatch publishes the post-commit BalanceChanged events (§5.3 invariant
// 4). Events are committed facts — a publish failure returns a coded error
// with Committed still true; the caller must resync, never repost.
func (s *LedgerService) dispatch(ctx context.Context, events []ledger.BalanceEvent) error {
	if len(events) == 0 {
		return nil
	}
	if s.pub == nil {
		return excerrors.New(ledger.CodeBalanceDispatchFailed,
			"ledger committed but no BalanceChanged publisher is configured")
	}
	// Deterministic order: sort by (account, currency).
	sort.Slice(events, func(a, b int) bool {
		if events[a].AccountID != events[b].AccountID {
			return events[a].AccountID < events[b].AccountID
		}
		return events[a].Currency < events[b].Currency
	})
	var failed []string
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			return excerrors.Wrap(ledger.CodeBalanceDispatchFailed,
				"ledger: marshal BalanceChanged", err)
		}
		subject := ledger.BalanceChangedSubject(ev.AccountID)
		var pubErr error
		for attempt := 0; attempt < s.dispatchAttempts; attempt++ {
			if pubErr = s.pub.Publish(ctx, subject, payload); pubErr == nil {
				break
			}
			select {
			case <-ctx.Done():
				return excerrors.Wrap(ledger.CodeBalanceDispatchFailed,
					"ledger: dispatch aborted", ctx.Err())
			case <-time.After(time.Duration(attempt+1) * 50 * time.Millisecond):
			}
		}
		if pubErr != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", subject, pubErr))
		}
	}
	if len(failed) > 0 {
		return excerrors.New(ledger.CodeBalanceDispatchFailed,
			fmt.Sprintf("ledger committed; BalanceChanged dispatch failed for %v", failed))
	}
	return nil
}
