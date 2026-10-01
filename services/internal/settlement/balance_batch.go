// balance_batch.go — set-based fill commit for the 50k/s ingest path
// (Phase-03 Task 3.3.1 DoD "zero loss at 50k/sec").
//
// commitBatch settles each fill with ~25 sequential round trips; at that
// shape the SERIALIZABLE commit path saturates near ~1k fills/s. This file
// is the identical contract expressed set-based: one multi-row statement
// per step — processed_trades dedup, journal_entries, chart resolution,
// ledger_lines, balances FOR UPDATE + ordered effect validation,
// ledger_entries, journal_sums, and the net_balance==total assertion —
// so a batch costs ~10 RTs regardless of size. pgxBalanceStore opts in
// via setBasedStore; unit-test fakes keep the serial path.
package settlement

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"exchange/internal/ledger"
	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// setBasedStore is implemented by pgxBalanceStore; other stores (test
// fakes) keep the serial commitBatch path.
type setBasedStore interface {
	InTxPgx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error
}

// InTxPgx runs fn inside a pool SERIALIZABLE tx exposing the raw pgx.Tx.
func (s pgxBalanceStore) InTxPgx(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("balance tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("balance tx commit: %w", err)
	}
	return nil
}

// commitBatchSetDispatch is the set-based sibling of commitBatch: all
// fills posted in one SERIALIZABLE tx via unnest-based statements;
// BalanceChanged events dispatched after commit.
func (s *BalanceService) commitBatchSetDispatch(ctx context.Context, trades []ResolvedTrade,
	journals []ledger.Journal) ([]FillOutcome, error) {

	sb, ok := s.store.(setBasedStore)
	if !ok {
		return s.commitBatch(ctx, trades, journals)
	}
	var outcomes []FillOutcome
	var events []ledger.BalanceEvent
	err := sb.InTxPgx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		outcomes, events, err = s.commitBatchSet(ctx, tx, trades, journals)
		return err
	})
	if err != nil {
		return nil, err
	}
	if s.dispatch != nil && len(events) > 0 {
		if err := s.dispatch.Dispatch(ctx, events); err != nil {
			return outcomes, err // committed — funds final; resync downstream
		}
	}
	return outcomes, nil
}

// pairKey is one wallet cell: (account_id, currency).
type pairKey struct {
	acct int64
	ccy  string
}

// batchTimingHook, when non-nil (tests only), receives per-step wall time
// inside commitBatchSet — the 50k/s path needs per-stage visibility.
var batchTimingHook func(step string, d time.Duration)

func markStep(step string, since time.Time) {
	if batchTimingHook != nil {
		batchTimingHook(step, time.Since(since))
	}
}

// commitBatchSet posts every journal in one SERIALIZABLE transaction with
// a constant number of set-based statements. Semantics are identical to
// commitBatch/applyEffect — same dedup, same ordered validation, same
// ledger_entries/journal_sums writes, same net==total assertion.
func (s *BalanceService) commitBatchSet(ctx context.Context, tx pgx.Tx, trades []ResolvedTrade,
	journals []ledger.Journal) ([]FillOutcome, []ledger.BalanceEvent, error) {

	// 1. processed_trades dedup — one set insert; RETURNING gives the
	//    applied set, the rest are replays (Duplicate outcome).
	stepStart := time.Now()
	tids := make([]int64, len(trades))
	shards := make([]int64, len(trades))
	for i := range trades {
		tids[i] = int64(trades[i].Fill.TradeID)
		shards[i] = trades[i].Fill.ShardID
	}
	appliedSet := map[int64]struct{}{}
	rows, err := tx.Query(ctx, `
		INSERT INTO processed_trades (trade_id, processed_at, shard_id)
		SELECT t.tid, now(), t.sid
		  FROM unnest($1::bigint[], $2::bigint[]) AS t(tid, sid)
		ON CONFLICT (trade_id) DO NOTHING
		RETURNING trade_id`, tids, shards)
	if err != nil {
		return nil, nil, fmt.Errorf("balance: batch dedup insert: %w", err)
	}
	for rows.Next() {
		var tid int64
		if err := rows.Scan(&tid); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("balance: scan dedup id: %w", err)
		}
		appliedSet[tid] = struct{}{}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("balance: batch dedup: %w", err)
	}

	markStep("1.dedup", stepStart)
	appliedIdx := make([]int, 0, len(trades))
	for i := range trades {
		if _, ok := appliedSet[int64(trades[i].Fill.TradeID)]; !ok {
			continue
		}
		appliedIdx = append(appliedIdx, i)
	}
	if len(appliedIdx) == 0 {
		outcomes := make([]FillOutcome, 0, len(trades))
		for i := range trades {
			outcomes = append(outcomes, FillOutcome{TradeID: trades[i].Fill.TradeID, Duplicate: true})
		}
		return outcomes, nil, nil // pure replay batch — nothing to post
	}

	// 1b. trades tape — the public tape row (id = engine trade id, the key
	//     the resolver's fee lookup reads) commits with the ledger, so the
	//     REST tape can never advertise an unsettled fill. ON CONFLICT DO
	//     NOTHING covers a same-instant re-delivery under the
	//     (id, created_at) partition key.
	stepStart = time.Now()
	tids2 := make([]int64, 0, len(appliedIdx))
	iids := make([]int64, 0, len(appliedIdx))
	bos := make([]int64, 0, len(appliedIdx))
	sos := make([]int64, 0, len(appliedIdx))
	buyers := make([]int64, 0, len(appliedIdx))
	sellers := make([]int64, 0, len(appliedIdx))
	pxs := make([]string, 0, len(appliedIdx))
	qtys := make([]string, 0, len(appliedIdx))
	bfs := make([]string, 0, len(appliedIdx))
	sfs := make([]string, 0, len(appliedIdx))
	sids2 := make([]int64, 0, len(appliedIdx))
	seqs := make([]int64, 0, len(appliedIdx))
	for _, i := range appliedIdx {
		t := trades[i]
		tids2 = append(tids2, int64(t.Fill.TradeID))
		iids = append(iids, t.InstrumentID)
		bos = append(bos, int64(t.Fill.BuyOrderID))
		sos = append(sos, int64(t.Fill.SellOrderID))
		buyers = append(buyers, t.BuyerAccountID)
		sellers = append(sellers, t.SellerAccountID)
		pxs = append(pxs, t.Fill.Price.String())
		qtys = append(qtys, t.Fill.Qty.String())
		bfs = append(bfs, t.BuyerFee.String())
		sfs = append(sfs, t.SellerFee.String())
		sids2 = append(sids2, t.Fill.ShardID)
		seqs = append(seqs, int64(t.Fill.EngineSeq))
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO trades (id, instrument_id, buy_order_id, sell_order_id,
		    buyer_account_id, seller_account_id, price, quantity,
		    buyer_fee, seller_fee, shard_id, trade_seq)
		OVERRIDING SYSTEM VALUE
		SELECT u.id, u.iid, u.bo, u.so, u.ba, u.sa,
		       u.px::numeric, u.qty::numeric, u.bf::numeric, u.sf::numeric,
		       u.sid::smallint, u.seq
		  FROM unnest($1::bigint[], $2::bigint[], $3::bigint[], $4::bigint[],
		              $5::bigint[], $6::bigint[], $7::text[], $8::text[],
		              $9::text[], $10::text[], $11::bigint[], $12::bigint[])
		      WITH ORDINALITY
		      AS u(id, iid, bo, so, ba, sa, px, qty, bf, sf, sid, seq, ord)
		 ORDER BY u.ord
		ON CONFLICT DO NOTHING`,
		tids2, iids, bos, sos, buyers, sellers, pxs, qtys, bfs, sfs,
		sids2, seqs); err != nil {
		return nil, nil, fmt.Errorf("balance: batch tape insert: %w", err)
	}
	markStep("1b.trades", stepStart)

	// 2. journal_entries — one unnest insert; RETURNING id +
	//    idempotency_key gives the exact journal→id map (no reliance on
	//    return order). 23505 → serial replay semantics apply per journal.
	stepStart = time.Now()
	ets := make([]string, 0, len(appliedIdx))
	refs := make([]int64, 0, len(appliedIdx))
	descs := make([]string, 0, len(appliedIdx))
	pbs := make([]string, 0, len(appliedIdx))
	idems := make([]string, 0, len(appliedIdx))
	shas := make([]string, 0, len(appliedIdx))
	for _, i := range appliedIdx {
		j := journals[i]
		ets = append(ets, string(j.EntryType))
		refs = append(refs, j.ReferenceID)
		descs = append(descs, j.Description)
		pbs = append(pbs, j.PostedBy)
		idems = append(idems, j.IdempotencyKey)
		shas = append(shas, journalHash(j))
	}
	journalIDs := make(map[int]int64, len(appliedIdx)) // trades-index → journal id
	rows, err = tx.Query(ctx, `
		INSERT INTO journal_entries
		    (entry_type, reference_id, description, posted_by, idempotency_key, payload_sha256)
		SELECT u.et::gl_entry_type_enum, NULLIF(u.ref,0), u.dsc, u.pb, NULLIF(u.idem,''), u.sha
		  FROM unnest($1::text[], $2::bigint[], $3::text[], $4::text[], $5::text[], $6::text[])
		      WITH ORDINALITY AS u(et, ref, dsc, pb, idem, sha, ord)
		 ORDER BY u.ord
		RETURNING id, idempotency_key`,
		ets, refs, descs, pbs, idems, shas)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: batch insert journal_entries: %w", err)
	}
	idemToID := map[string]int64{}
	for rows.Next() {
		var jid int64
		var idem *string
		if err := rows.Scan(&jid, &idem); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("ledger: scan journal id: %w", err)
		}
		if idem != nil {
			idemToID[*idem] = jid
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("ledger: batch insert journal_entries: %w", err)
	}
	for _, i := range appliedIdx {
		jid, ok := idemToID[journals[i].IdempotencyKey]
		if !ok {
			return nil, nil, fmt.Errorf("ledger: journal id missing for fill trade %d", trades[i].Fill.TradeID)
		}
		journalIDs[i] = jid
	}

	markStep("2.journal_entries", stepStart)
	stepStart = time.Now()
	// 3. Resolve every distinct account_code across applied journals in one
	//    query; validate each journal against the fetched chart.
	codeSeen := map[string]struct{}{}
	codes := make([]string, 0, 8)
	for _, i := range appliedIdx {
		for _, l := range journals[i].Lines {
			if _, ok := codeSeen[l.AccountCode]; !ok {
				codeSeen[l.AccountCode] = struct{}{}
				codes = append(codes, l.AccountCode)
			}
		}
	}
	chartAccs := make([]ledger.Account, 0, len(codes))
	rows, err = tx.Query(ctx, `
		SELECT account_code, account_name, account_type::text, currency
		  FROM chart_of_accounts WHERE account_code = ANY($1)`, codes)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: resolve accounts: %w", err)
	}
	for rows.Next() {
		var a ledger.Account
		if err := rows.Scan(&a.Code, &a.Name, (*string)(&a.Type), &a.Currency); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("ledger: scan account: %w", err)
		}
		chartAccs = append(chartAccs, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("ledger: resolve accounts: %w", err)
	}
	chart := ledger.NewChart(chartAccs)
	for _, i := range appliedIdx {
		if err := journals[i].ValidateAccounts(chart); err != nil {
			return nil, nil, err
		}
	}

	markStep("3.resolve_accounts", stepStart)
	stepStart = time.Now()
	// 4. ledger_lines — flat unnest across all applied journals.
	ljids := make([]int64, 0, len(appliedIdx)*4)
	lcodes := make([]string, 0, len(appliedIdx)*4)
	ldr := make([]string, 0, len(appliedIdx)*4)
	lcr := make([]string, 0, len(appliedIdx)*4)
	lccy := make([]string, 0, len(appliedIdx)*4)
	lnar := make([]string, 0, len(appliedIdx)*4)
	for _, i := range appliedIdx {
		for _, l := range journals[i].Lines {
			ljids = append(ljids, journalIDs[i])
			lcodes = append(lcodes, l.AccountCode)
			ldr = append(ldr, l.Debit.String())
			lcr = append(lcr, l.Credit.String())
			lccy = append(lccy, l.Currency)
			lnar = append(lnar, l.Narrative)
		}
	}
	// COPY beats multi-row INSERT ~5-10x on this shape (no ids needed back —
	// line ids are unused by FillOutcome).
	lineRows := pgx.CopyFromSlice(len(ljids), func(i int) ([]interface{}, error) {
		return []interface{}{ljids[i], lcodes[i], ldr[i], lcr[i], lccy[i], lnar[i]}, nil
	})
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"ledger_lines"},
		[]string{"journal_entry_id", "account_code", "debit_amount", "credit_amount", "currency", "narrative"},
		lineRows); err != nil {
		return nil, nil, fmt.Errorf("ledger: copy ledger_lines: %w", err)
	}

	markStep("4.ledger_lines", stepStart)
	stepStart = time.Now()
	// 5. In-tx zero-sum re-verify per currency across the whole batch —
	//    mirror of verifyZeroSum; any imbalance aborts (L0).
	rows, err = tx.Query(ctx, `
		SELECT journal_entry_id, currency, SUM(debit_amount)::text, SUM(credit_amount)::text
		  FROM ledger_lines WHERE journal_entry_id = ANY($1)
		 GROUP BY journal_entry_id, currency
		HAVING SUM(debit_amount) <> SUM(credit_amount)`, ljids0(journalIDs, appliedIdx))
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: batch zero-sum verify: %w", err)
	}
	for rows.Next() {
		var jid int64
		var ccy, dStr, cStr string
		if err := rows.Scan(&jid, &ccy, &dStr, &cStr); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("ledger: scan zero-sum: %w", err)
		}
		rows.Close()
		return nil, nil, excerrors.New(ledger.CodeLedgerImbalanceAbort, fmt.Sprintf(
			"journal %d currency %s: stored SUM(debits)=%s != SUM(credits)=%s", jid, ccy, dStr, cStr))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("ledger: batch zero-sum verify: %w", err)
	}

	markStep("5.zero_sum", stepStart)
	stepStart = time.Now()
	// 6. Wallet effects. Collect ordered effect list + distinct pairs;
	//    reject journals where one (acct,ccy) is touched twice — the
	//    ledger-entry-id map keys on it (ambiguous; force per-fill retry).
	type effRef struct {
		tradeIdx int
		journal  ledger.Journal
		effect   ledger.AccountEffect
	}
	var effs []effRef
	pairSeen := map[pairKey]struct{}{}
	pairs := make([]pairKey, 0, 64)
	for _, i := range appliedIdx {
		j := journals[i]
		inJournal := map[pairKey]struct{}{}
		for _, e := range j.Effects {
			pk := pairKey{e.AccountID, e.Currency}
			if _, dup := inJournal[pk]; dup {
				return nil, nil, fmt.Errorf("ledger: journal for trade %d touches acct %d %s twice — refusing ambiguous batch",
					trades[i].Fill.TradeID, e.AccountID, e.Currency)
			}
			inJournal[pk] = struct{}{}
			if _, ok := pairSeen[pk]; !ok {
				pairSeen[pk] = struct{}{}
				pairs = append(pairs, pk)
			}
			effs = append(effs, effRef{i, j, e})
		}
	}

	markStep("6.collect_effects", stepStart)
	stepStart = time.Now()
	// Ensure zero rows exist for untouched pairs (INSERT..ON CONFLICT),
	// then SELECT ... FOR UPDATE the whole set in one shot.
	pa := make([]int64, len(pairs))
	pc := make([]string, len(pairs))
	for k, p := range pairs {
		pa[k], pc[k] = p.acct, p.ccy
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO balances (account_id, currency, available, locked)
		SELECT u.aid, u.ccy, 0, 0 FROM unnest($1::bigint[], $2::text[]) AS u(aid, ccy)
		ON CONFLICT (account_id, currency) DO NOTHING`, pa, pc); err != nil {
		return nil, nil, fmt.Errorf("ledger: ensure balances rows: %w", err)
	}
	cur := map[pairKey][2]decimal.Decimal{}
	rows, err = tx.Query(ctx, `
		SELECT account_id, currency, available, locked FROM balances
		 WHERE (account_id, currency) IN (SELECT * FROM unnest($1::bigint[], $2::text[]))
		 FOR UPDATE`, pa, pc)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: lock balances rows: %w", err)
	}
	for rows.Next() {
		var pk pairKey
		var av, lk decimal.Decimal
		if err := rows.Scan(&pk.acct, &pk.ccy, &av, &lk); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("ledger: scan balances: %w", err)
		}
		cur[pk] = [2]decimal.Decimal{av, lk}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("ledger: lock balances rows: %w", err)
	}
	if len(cur) != len(pairs) {
		return nil, nil, fmt.Errorf("ledger: locked %d of %d balance rows", len(cur), len(pairs))
	}

	markStep("7.wallet_lock", stepStart)
	stepStart = time.Now()
	// Ordered simulation per pair — identical to sequential applyEffect:
	// every intermediate state is validated (locked never <0; available
	// <0 only under AllowNegative). Ledger-entry rows and journal_sums
	// deltas accumulate in journal order.
	final := map[pairKey][2]decimal.Decimal{}
	for p, v := range cur {
		final[p] = v
	}
	sumDr := map[pairKey]decimal.Decimal{}
	sumCr := map[pairKey]decimal.Decimal{}
	var events []ledger.BalanceEvent

	leJid := make([]int64, 0, len(effs))
	leType := make([]string, 0, len(effs))
	leRef := make([]int64, 0, len(effs))
	leAcct := make([]int64, 0, len(effs))
	leCcy := make([]string, 0, len(effs))
	leDir := make([]string, 0, len(effs))
	leAmt := make([]string, 0, len(effs))
	leRun := make([]string, 0, len(effs))
	leDesc := make([]string, 0, len(effs))
	leBy := make([]string, 0, len(effs))

	for _, r := range effs {
		e := r.effect
		pk := pairKey{e.AccountID, e.Currency}
		jid := journalIDs[r.tradeIdx]
		st := final[pk]
		newAvail := st[0].Add(e.AvailableDelta)
		newLocked := st[1].Add(e.LockedDelta)
		if newLocked.IsNegative() {
			return nil, nil, excerrors.New(ledger.CodeInsufficientBalance, fmt.Sprintf(
				"acct %d %s: locked %s + %s < 0 — locked funds can never go negative",
				e.AccountID, e.Currency, st[1].String(), e.LockedDelta.String()))
		}
		if newAvail.IsNegative() && !e.AllowNegative {
			return nil, nil, excerrors.New(ledger.CodeInsufficientBalance, fmt.Sprintf(
				"acct %d %s: available %s + %s < 0",
				e.AccountID, e.Currency, st[0].String(), e.AvailableDelta.String()))
		}
		final[pk] = [2]decimal.Decimal{newAvail, newLocked}
		newTotal := newAvail.Add(newLocked)
		ev := ledger.BalanceEvent{
			AccountID: e.AccountID, Currency: e.Currency, JournalID: jid,
			Available: newAvail.String(), Locked: newLocked.String(),
			Total: newTotal.String(), EventType: "BALANCE_CHANGED",
		}

		net := e.Net()
		if !net.IsZero() {
			dir := "CREDIT"
			if net.IsPositive() {
				dir = "DEBIT" // §5.3: net_balance = debits − credits = total
			}
			leJid = append(leJid, jid)
			leType = append(leType, r.journal.EntryType.LedgerEntryType())
			leRef = append(leRef, r.journal.ReferenceID)
			leAcct = append(leAcct, e.AccountID)
			leCcy = append(leCcy, e.Currency)
			leDir = append(leDir, dir)
			leAmt = append(leAmt, net.Abs().String())
			leRun = append(leRun, newTotal.String())
			leDesc = append(leDesc, r.journal.Description)
			leBy = append(leBy, r.journal.PostedBy)
			if dir == "DEBIT" {
				sumDr[pk] = sumDr[pk].Add(net)
			} else {
				sumCr[pk] = sumCr[pk].Add(net.Neg())
			}
		}
		events = append(events, ev)
	}

	markStep("8.simulate", stepStart)
	stepStart = time.Now()
	// Batch UPDATE balances.
	fa := make([]int64, 0, len(final))
	fc := make([]string, 0, len(final))
	fav := make([]string, 0, len(final))
	flk := make([]string, 0, len(final))
	for p, v := range final {
		fa = append(fa, p.acct)
		fc = append(fc, p.ccy)
		fav = append(fav, v[0].String())
		flk = append(flk, v[1].String())
	}
	if _, err := tx.Exec(ctx, `
		UPDATE balances AS b
		   SET available = v.av, locked = v.lk, version = b.version + 1
		  FROM unnest($1::bigint[], $2::text[], $3::numeric[], $4::numeric[]) AS v(aid, ccy, av, lk)
		 WHERE b.account_id = v.aid AND b.currency = v.ccy`,
		fa, fc, fav, flk); err != nil {
		return nil, nil, fmt.Errorf("ledger: batch update balances: %w", err)
	}

	markStep("9.update_balances", stepStart)
	stepStart = time.Now()
	// Batch INSERT ledger_entries; then map ids back per
	// (journal, acct, ccy) — unique within a journal (dup pairs rejected
	// above), so the BalanceEvent.LedgerEntryID assignment is exact.
	if len(leJid) > 0 {
		// COPY for the append-only wallet ledger; ids are mapped back via
		// the (journal,acct,ccy) read-back below.
		entRows := pgx.CopyFromSlice(len(leJid), func(i int) ([]interface{}, error) {
			var ref interface{}
			if leRef[i] != 0 {
				ref = leRef[i]
			}
			return []interface{}{leType[i], ref, leAcct[i], leCcy[i], leDir[i],
				leAmt[i], leRun[i], leDesc[i], leBy[i], leJid[i]}, nil
		})
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"ledger_entries"},
			[]string{"entry_type", "reference_id", "account_id", "currency", "direction",
				"amount", "running_balance", "description", "posted_by", "journal_entry_id"},
			entRows); err != nil {
			return nil, nil, fmt.Errorf("ledger: copy ledger_entries: %w", err)
		}
		type entryKey struct {
			jid  int64
			acct int64
			ccy  string
		}
		entryIDs := map[entryKey]int64{} // (journal,acct,ccy) — dup-in-journal rejected above
		jids := ljids0(journalIDs, appliedIdx)
		rows, err = tx.Query(ctx, `
			SELECT id, journal_entry_id, account_id, currency FROM ledger_entries
			 WHERE journal_entry_id = ANY($1)`, jids)
		if err != nil {
			return nil, nil, fmt.Errorf("ledger: read back ledger_entries: %w", err)
		}
		for rows.Next() {
			var id int64
			var k entryKey
			if err := rows.Scan(&id, &k.jid, &k.acct, &k.ccy); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("ledger: scan ledger_entry id: %w", err)
			}
			entryIDs[k] = id
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, fmt.Errorf("ledger: read back ledger_entries: %w", err)
		}
		for k := range events {
			key := entryKey{events[k].JournalID, events[k].AccountID, events[k].Currency}
			if id, ok := entryIDs[key]; ok {
				events[k].LedgerEntryID = id
			}
		}
	}

	markStep("10.ledger_entries", stepStart)
	stepStart = time.Now()
	// journal_sums — one upsert over aggregated per-pair debits/credits,
	// then the §5.3 assertion across the whole touched set in one query.
	jsa := make([]int64, 0, len(pairs))
	jsc := make([]string, 0, len(pairs))
	jsd := make([]string, 0, len(pairs))
	jscr := make([]string, 0, len(pairs))
	for _, p := range pairs {
		jsa = append(jsa, p.acct)
		jsc = append(jsc, p.ccy)
		jsd = append(jsd, sumDr[p].String())
		jscr = append(jscr, sumCr[p].String())
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO journal_sums (account_id, currency, total_debits, total_credits, entry_count)
		SELECT u.aid, u.ccy, u.dr, u.cr, 0
		  FROM unnest($1::bigint[], $2::text[], $3::numeric[], $4::numeric[]) AS u(aid, ccy, dr, cr)
		ON CONFLICT (account_id, currency) DO UPDATE SET
		    total_debits  = journal_sums.total_debits  + EXCLUDED.total_debits,
		    total_credits = journal_sums.total_credits + EXCLUDED.total_credits`,
		jsa, jsc, jsd, jscr); err != nil {
		return nil, nil, fmt.Errorf("ledger: batch upsert journal_sums: %w", err)
	}
	rows, err = tx.Query(ctx, `
		SELECT js.account_id, js.currency, js.net_balance::text, b.total::text
		  FROM journal_sums js
		  JOIN balances b ON b.account_id = js.account_id AND b.currency = js.currency
		 WHERE (js.account_id, js.currency) IN (SELECT * FROM unnest($1::bigint[], $2::text[]))
		   AND js.net_balance <> b.total`, jsa, jsc)
	if err != nil {
		return nil, nil, fmt.Errorf("ledger: batch net-balance assert: %w", err)
	}
	var mismatch string
	for rows.Next() {
		var aid int64
		var ccy, nb, tot string
		if err := rows.Scan(&aid, &ccy, &nb, &tot); err == nil {
			mismatch = fmt.Sprintf("acct %d %s: journal_sums.net_balance=%s != balances.total=%s", aid, ccy, nb, tot)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("ledger: batch net-balance assert: %w", err)
	}
	if mismatch != "" {
		return nil, nil, excerrors.New(ledger.CodeLedgerImbalanceAbort, mismatch)
	}

	markStep("11.journal_sums_assert", stepStart)

	// Outcomes in input order.
	outcomes := make([]FillOutcome, 0, len(trades))
	for i := range trades {
		tid := int64(trades[i].Fill.TradeID)
		if _, ok := appliedSet[tid]; !ok {
			outcomes = append(outcomes, FillOutcome{TradeID: trades[i].Fill.TradeID, Duplicate: true})
			continue
		}
		outcomes = append(outcomes, FillOutcome{
			TradeID: trades[i].Fill.TradeID, JournalID: journalIDs[i], Applied: true,
		})
	}
	return outcomes, events, nil
}

// ljids0 flattens the journal-id map for ANY($1) predicates.
func ljids0(m map[int]int64, idx []int) []int64 {
	out := make([]int64, 0, len(idx))
	for _, i := range idx {
		out = append(out, m[i])
	}
	return out
}
