// Phase-23 Task 23.3.7 — historical block-trade read model over the
// Phase-06 Task 6.3.20 public block tape (spec §10.8/§16.1, §24 #291;
// §28.1 Block Tape matrix row):
//
//	GET /api/v1/history/block-trades/{symbol}?from=&to=&limit=&cursor=
//
// DATA SOURCE — the ClickHouse `block_trades_tape` table, written by
// THIS file's BlockTapeStore in its BlockTapeSink role (stats.go calls
// Record for each delay-gated publication and RecordCorrection for each
// published correction/bust). Deployed DDL (drift hatch — if the schema
// track renames a column, the const block below is the fix surface):
//
//	block_trades_tape (
//	    entry_id        UInt64,      -- print: block_trade_id<<1;
//	                                 -- correction: block_trade_id<<1|1
//	    kind            LowCardinality(String),  -- 'PRINT'|'CORRECTION'|'BUST'
//	    block_trade_id  UInt64,      -- corrections: the print superseded
//	    symbol          LowCardinality(String),
//	    price           Decimal(38,8), quantity Decimal(38,8),
//	    notional_usd    Decimal(38,8),
//	    exec_ts         DateTime64(3,'UTC'), pub_ts DateTime64(3,'UTC'),
//	    delay_ms        UInt64, venue_flags Array(String),
//	    original_trade_id UInt64,    -- corrections only (0 on prints)
//	    corrected_price    Nullable(Decimal(38,8)),
//	    corrected_quantity Nullable(Decimal(38,8)),
//	    ver             UInt64
//	) ENGINE = ReplacingMergeTree(ver)
//	  PARTITION BY toYYYYMM(pub_ts)
//	  ORDER BY (symbol, pub_ts, entry_id)
//	  TTL pub_ts + INTERVAL 5 YEAR   -- MiFID II record-keeping, §19.12
//
// ANONYMITY INVARIANT (§24 #291, task text): this store ingests ONLY
// post-gate `blockTradeData`/`blockCorrectionData` payloads — the wire
// shapes the venue already published on `blockTrades@{symbol}`. Those
// shapes carry no side, no account ids, no order ids and no resting
// liquidity by construction (see stats.go); BlockTapeEntry therefore
// carries no participant fields at all — anonymity is structural, not a
// masking step that could be forgotten. Hidden orders never reach the
// producer's absorb path (they emit no public TradeEvent fill), and a
// print corrected while still in the delay gate is cancelled
// pre-publication and never lands here.
//
// CORRECTION LINEAGE (spec §5.29 heritage, task text): corrections are
// separate tape rows — `kind` CORRECTION|BUST, `block_trade_id` naming
// the print they supersede — so the history API serves the original
// print annotated `bust`/`corrected_by` alongside the correcting row's
// `supersedes` link. ReplacingMergeTree ver collapses a replayed row;
// one executed correction per trade is the §5.29 ceiling
// (trade_busts_executed_ux), so the <<1|1 entry-id slot suffices.
//
// Fail-closed (§2.7): a store outage surfaces SERVICE_DEGRADED at the
// handler — never an empty page masquerading as a quiet tape.
package marketdata

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"exchange/pkg/decimal"
)

// BlockTapeTable is the ClickHouse table name (§28.1 matrix row).
const BlockTapeTable = "block_trades_tape"

// BlockTapeKind enumerates tape row kinds. The CORRECTION/BUST values
// mirror blockCorrectionData.Kind on the wire.
type BlockTapeKind string

const (
	BlockTapePrint      BlockTapeKind = "PRINT"
	BlockTapeCorrection BlockTapeKind = "CORRECTION"
	BlockTapeBust       BlockTapeKind = "BUST"
)

// blockTapeColumns is the insert column tuple; blockTapeReadColumns the
// select projection (ver is engine-internal).
const blockTapeColumns = "entry_id, kind, block_trade_id, symbol, " +
	"price, quantity, notional_usd, exec_ts, pub_ts, delay_ms, " +
	"venue_flags, original_trade_id, corrected_price, corrected_quantity, ver"

const blockTapeReadColumns = "entry_id, kind, block_trade_id, symbol, " +
	"price, quantity, notional_usd, exec_ts, pub_ts, delay_ms, " +
	"venue_flags, original_trade_id, corrected_price, corrected_quantity"

// blockTapeFrom is FINAL-deduped so a replayed publication or a re-sent
// correction can never double (same contract as `ticks FINAL`).
const blockTapeFrom = BlockTapeTable + " FINAL"

// blockTapeEntryID maps (block_trade_id, correction?) onto the unique
// keyset id: prints occupy the even slot, corrections the odd.
func blockTapeEntryID(blockTradeID uint64, correction bool) uint64 {
	id := blockTradeID << 1
	if correction {
		id |= 1
	}
	return id
}

// CHTapeConn is the narrow read+write surface the tape store needs —
// the driver's driver.Conn (and analytics.Conn) satisfy it
// structurally; declared locally because this package cannot import
// internal/analytics (analytics → funding → marketdata import cycle —
// historical.go's CHQuerier documents the same constraint).
type CHTapeConn interface {
	Query(ctx context.Context, query string, args ...any) (driver.Rows, error)
	PrepareBatch(ctx context.Context, query string, opts ...driver.PrepareBatchOption) (driver.Batch, error)
}

// BlockTapeEntry is one row of the public tape — either a published
// print or a published correction/bust. CorrectedBy and Bust are
// READ-RESOLVED annotations on prints (populated by Query, never
// stored): a print superseded by a correction row carries the
// correction's entry_id.
type BlockTapeEntry struct {
	EntryID      uint64
	Kind         BlockTapeKind
	BlockTradeID uint64 // prints: own id; corrections: the print superseded
	Symbol       string
	Price        decimal.Decimal // prints only (zero on corrections)
	Quantity     decimal.Decimal
	NotionalUSD  decimal.Decimal
	ExecTs       time.Time // print: engine fill time; correction: correction event time
	PubTs        time.Time // publication time (the ordering axis — when the venue disclosed it)
	DelayMs      int64
	VenueFlags   []string
	// Correction rows only.
	OriginalTradeID   uint64           // engine trade id (already public via the correction frame)
	CorrectedPrice    *decimal.Decimal // nil when the correction carried no price
	CorrectedQuantity *decimal.Decimal
	// Read-resolved print annotations (not columns).
	CorrectedBy uint64 // entry_id of the superseding CORRECTION/BUST row (0 = none)
	Bust        bool   // a BUST correction supersedes this print
}

// IsCorrection reports whether the row is a correction/bust entry.
func (e *BlockTapeEntry) IsCorrection() bool {
	return e.Kind == BlockTapeCorrection || e.Kind == BlockTapeBust
}

// BlockTapeCursor is the (pub_ts, entry_id) keyset position for the
// newest-first stream — the api layer renders it as the opaque §8.8
// token. pub_ts is the ordering axis: it is monotone with publication
// order, the axis the venue actually disclosed.
type BlockTapeCursor struct {
	Ts      time.Time // pub_ts
	EntryID uint64
}

// BlockTapeQuery is one bounded read of the public tape. From/To bound
// exec_ts (the trade's own time — what a "block trades between X and Y"
// query means); PublishedBefore bounds pub_ts — the handler sets it to
// now−15min for the free tier so a just-published print is invisible to
// unpaid readers (the API-side delay rides ON TOP of the producer's
// MiFID II deferral). A zero bound is unbounded.
type BlockTapeQuery struct {
	Symbol          string
	From            time.Time        // inclusive lower bound on exec_ts
	To              time.Time        // exclusive upper bound on exec_ts
	PublishedBefore time.Time        // upper bound on pub_ts (zero = none)
	After           *BlockTapeCursor // keyset: strictly before (pub_ts, entry_id)
	Limit           int              // 0 → store default (500)
}

// BlockTapeReader is the history-read seam the api handler consumes —
// *BlockTapeStore satisfies it; tests inject fakes.
type BlockTapeReader interface {
	Query(ctx context.Context, q BlockTapeQuery) ([]*BlockTapeEntry, error)
}

// ---------------------------------------------------------------------------
// BlockTapeStore — ClickHouse ingest + read over CHTapeConn
// ---------------------------------------------------------------------------

// BlockTapeStore is the columnar tape store. It is BOTH write paths the
// Phase-06 producer needs: Record satisfies stats.go's BlockTapeSink
// (published prints), and RecordCorrection persists the corrections
// PushCorrection emits — wired there via the BlockTapeCorrectionSink
// type-assertion, so a store that only knows Record can still serve as
// a prints-only sink without an interface break.
type BlockTapeStore struct {
	conn CHTapeConn
	// Now supplies the `ver` version clock; injectable for tests.
	Now func() time.Time
}

var _ BlockTapeSink = (*BlockTapeStore)(nil)
var _ BlockTapeCorrectionSink = (*BlockTapeStore)(nil)
var _ BlockTapeReader = (*BlockTapeStore)(nil)

// NewBlockTapeStore wires the store over an open CHTapeConn
// (analytics.Dial's Conn satisfies the seam).
func NewBlockTapeStore(conn CHTapeConn) *BlockTapeStore {
	return &BlockTapeStore{conn: conn}
}

func (s *BlockTapeStore) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// BlockTapeCorrectionSink is the optional correction-persistence seam.
// BlockTapeProducer.PushCorrection type-asserts its configured
// BlockTapeSink to this shape (stats.go) — a sink that implements it
// receives every published correction/bust; one that does not simply
// keeps a prints-only tape and the correction still broadcasts.
type BlockTapeCorrectionSink interface {
	RecordCorrection(ctx context.Context, corr blockCorrectionData) error
}

// Record implements BlockTapeSink — persists one delay-gated print.
func (s *BlockTapeStore) Record(ctx context.Context, pub blockTradeData) error {
	price, err := decimal.NewFromString(pub.Price)
	if err != nil {
		return fmt.Errorf("block tape price %q: %w", pub.Price, err)
	}
	qty, err := decimal.NewFromString(pub.Quantity)
	if err != nil {
		return fmt.Errorf("block tape quantity %q: %w", pub.Quantity, err)
	}
	notional, err := decimal.NewFromString(pub.NotionalUSD)
	if err != nil {
		return fmt.Errorf("block tape notional %q: %w", pub.NotionalUSD, err)
	}
	batch, err := s.conn.PrepareBatch(ctx,
		"INSERT INTO "+BlockTapeTable+" ("+blockTapeColumns+")")
	if err != nil {
		return fmt.Errorf("block tape batch prepare: %w", err)
	}
	err = batch.Append(
		blockTapeEntryID(pub.BlockTradeID, false), string(BlockTapePrint),
		pub.BlockTradeID, pub.Symbol,
		price, qty, notional,
		time.UnixMilli(pub.ExecTsMs).UTC(), time.UnixMilli(pub.PubTsMs).UTC(),
		uint64(pub.DelayMs), pub.VenueFlags,
		uint64(0), nil, nil,
		uint64(s.now().UnixMilli()))
	if err != nil {
		_ = batch.Abort()
		return fmt.Errorf("block tape append %d: %w", pub.BlockTradeID, err)
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("block tape send: %w", err)
	}
	return nil
}

// RecordCorrection persists one published correction/bust linked to its
// print. Corrected fields that fail to parse are stored NULL rather
// than dropping the row — a BUST must land even when its payload is
// degraded, else the public tape keeps a stale print (§2.7).
func (s *BlockTapeStore) RecordCorrection(ctx context.Context, corr blockCorrectionData) error {
	kind := BlockTapeCorrection
	if strings.EqualFold(corr.Kind, string(BlockTapeBust)) {
		kind = BlockTapeBust
	}
	var cPrice, cQty *decimal.Decimal
	if v, err := decimal.NewFromString(corr.CorrectedPrice); err == nil {
		cPrice = &v
	}
	if v, err := decimal.NewFromString(corr.CorrectedQuantity); err == nil {
		cQty = &v
	}
	batch, err := s.conn.PrepareBatch(ctx,
		"INSERT INTO "+BlockTapeTable+" ("+blockTapeColumns+")")
	if err != nil {
		return fmt.Errorf("block tape correction prepare: %w", err)
	}
	err = batch.Append(
		blockTapeEntryID(corr.BlockTradeID, true), string(kind),
		corr.BlockTradeID, corr.Symbol,
		decimal.Zero, decimal.Zero, decimal.Zero,
		// exec_ts carries the correction event time so corrections
		// interleave with prints under the exec_ts range filter rather
		// than pinning to epoch 0 (which would hide them from any
		// bounded query).
		time.UnixMilli(corr.TsMs).UTC(), time.UnixMilli(corr.TsMs).UTC(),
		uint64(0), []string{},
		corr.OriginalTradeID, cPrice, cQty,
		uint64(s.now().UnixMilli()))
	if err != nil {
		_ = batch.Abort()
		return fmt.Errorf("block tape correction append %d: %w", corr.BlockTradeID, err)
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("block tape correction send: %w", err)
	}
	return nil
}

// Query reads one keyset page newest-first on (pub_ts, entry_id), then
// resolves print annotations with a second bounded read: every
// CORRECTION|BUST row superseding a print on the page, regardless of
// whether the correction itself fell inside the requested window (a
// bust lands minutes after the print — lineage must not depend on the
// caller's range).
func (s *BlockTapeStore) Query(ctx context.Context, q BlockTapeQuery) ([]*BlockTapeEntry, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 500
	}
	var b strings.Builder
	b.WriteString("SELECT " + blockTapeReadColumns + " FROM " + blockTapeFrom +
		" WHERE symbol = ?")
	args := []any{q.Symbol}
	if !q.From.IsZero() {
		b.WriteString(" AND exec_ts >= ?")
		args = append(args, q.From.UTC())
	}
	if !q.To.IsZero() {
		b.WriteString(" AND exec_ts < ?")
		args = append(args, q.To.UTC())
	}
	if !q.PublishedBefore.IsZero() {
		b.WriteString(" AND pub_ts <= ?")
		args = append(args, q.PublishedBefore.UTC())
	}
	if q.After != nil {
		b.WriteString(" AND (pub_ts, entry_id) < (?, ?)")
		args = append(args, q.After.Ts.UTC(), q.After.EntryID)
	}
	b.WriteString(" ORDER BY pub_ts DESC, entry_id DESC LIMIT ?")
	args = append(args, limit)

	rows, err := s.conn.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("block tape query %s: %w", q.Symbol, err)
	}
	defer rows.Close()

	out := make([]*BlockTapeEntry, 0, limit)
	for rows.Next() {
		e, err := scanBlockTapeRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("block tape rows: %w", err)
	}
	if err := s.annotateCorrections(ctx, q.Symbol, out); err != nil {
		return nil, err
	}
	return out, nil
}

// annotateCorrections fills CorrectedBy/Bust on the page's PRINT rows.
// The lookup keys on the page's block_trade_ids — corrections may fall
// outside the caller's exec_ts window, so they are fetched by linkage,
// not by range.
func (s *BlockTapeStore) annotateCorrections(ctx context.Context, symbol string, page []*BlockTapeEntry) error {
	byPrint := map[uint64]*BlockTapeEntry{}
	ids := make([]any, 0, len(page))
	for _, e := range page {
		if e.Kind == BlockTapePrint {
			byPrint[e.BlockTradeID] = e
			ids = append(ids, e.BlockTradeID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	marks := make([]string, len(ids))
	for i := range marks {
		marks[i] = "?"
	}
	rows, err := s.conn.Query(ctx,
		"SELECT block_trade_id, kind, entry_id FROM "+blockTapeFrom+
			" WHERE symbol = ? AND kind != 'PRINT' AND block_trade_id IN ("+
			strings.Join(marks, ",")+")",
		append([]any{symbol}, ids...)...)
	if err != nil {
		return fmt.Errorf("block tape corrections %s: %w", symbol, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			printID uint64
			kind    string
			entryID uint64
		)
		if err := rows.Scan(&printID, &kind, &entryID); err != nil {
			return fmt.Errorf("block tape correction scan: %w", err)
		}
		if p, ok := byPrint[printID]; ok {
			p.CorrectedBy = entryID
			p.Bust = kind == string(BlockTapeBust)
		}
	}
	return rows.Err()
}

// scanBlockTapeRow decodes one read-projection row.
func scanBlockTapeRow(rows interface {
	Scan(dest ...any) error
}) (*BlockTapeEntry, error) {
	var (
		e        BlockTapeEntry
		kind     string
		price    decimal.Decimal
		qty      decimal.Decimal
		notional decimal.Decimal
	)
	if err := rows.Scan(&e.EntryID, &kind, &e.BlockTradeID, &e.Symbol,
		&price, &qty, &notional, &e.ExecTs, &e.PubTs, &e.DelayMs,
		&e.VenueFlags, &e.OriginalTradeID, &e.CorrectedPrice,
		&e.CorrectedQuantity); err != nil {
		return nil, fmt.Errorf("block tape row scan: %w", err)
	}
	e.Kind = BlockTapeKind(kind)
	e.Price, e.Quantity, e.NotionalUSD = price, qty, notional
	return &e, nil
}

// ---------------------------------------------------------------------------
// CSV renderer (Accept: text/csv — Task 23.3.7 export leg)
// ---------------------------------------------------------------------------

// WriteBlockTapeCSV streams one RFC-4180-style row per tape entry:
//
//	entry_id,kind,block_trade_id,symbol,exec_ts,pub_ts,price,quantity,
//	notional_usd,delay_ms,venue_flags,bust,corrected_by,supersedes,
//	original_trade_id,corrected_price,corrected_quantity
//
// Money fields stay decimal strings (StringFixed(8)) — never float.
// Correction rows leave the print-only columns empty; `supersedes` is
// the print's block_trade_id.
func WriteBlockTapeCSV(w io.Writer, entries []*BlockTapeEntry) error {
	if _, err := io.WriteString(w,
		"entry_id,kind,block_trade_id,symbol,exec_ts,pub_ts,price,quantity,"+
			"notional_usd,delay_ms,venue_flags,bust,corrected_by,supersedes,"+
			"original_trade_id,corrected_price,corrected_quantity\n"); err != nil {
		return err
	}
	for _, e := range entries {
		var price, qty, notional, execTs, correctedBy, supersedes string
		var origID, cPrice, cQty string
		if e.Kind == BlockTapePrint {
			price = e.Price.StringFixed(8)
			qty = e.Quantity.StringFixed(8)
			notional = e.NotionalUSD.StringFixed(8)
			execTs = e.ExecTs.UTC().Format("2006-01-02T15:04:05.000Z")
			if e.CorrectedBy != 0 {
				correctedBy = fmt.Sprintf("%d", e.CorrectedBy)
			}
		} else {
			supersedes = fmt.Sprintf("%d", e.BlockTradeID)
			origID = fmt.Sprintf("%d", e.OriginalTradeID)
			if e.CorrectedPrice != nil {
				cPrice = e.CorrectedPrice.StringFixed(8)
			}
			if e.CorrectedQuantity != nil {
				cQty = e.CorrectedQuantity.StringFixed(8)
			}
		}
		bust := "false"
		if e.Bust || e.Kind == BlockTapeBust {
			bust = "true"
		}
		if _, err := fmt.Fprintf(w, "%d,%s,%d,%s,%s,%s,%s,%s,%s,%d,%s,%s,%s,%s,%s,%s,%s\n",
			e.EntryID, e.Kind, e.BlockTradeID, e.Symbol, execTs,
			e.PubTs.UTC().Format("2006-01-02T15:04:05.000Z"),
			price, qty, notional, e.DelayMs,
			strings.Join(e.VenueFlags, ";"), bust,
			correctedBy, supersedes, origID, cPrice, cQty); err != nil {
			return err
		}
	}
	return nil
}
