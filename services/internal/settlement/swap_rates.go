// Swap-rate feed ingestion and effective-dated store — Phase-03
// Task 3.3.11 (spec §17.4, §24 #221; history surface §17.15).
//
// Swap points (the interest-rate differential expressed in price points
// per unit of base currency per day) are published once per New York
// trading day by interbank feeds (Refinitiv / Bloomberg — Phase-19.5
// Task 19.5.3.5 owns the yield-curve sources that derive them). This file
// provides:
//
//   - SwapRateFeed — the scheduled-pull seam. FileSwapRateFeed (ops
//     drop-file / tests) and HTTPSwapRateFeed (vendor endpoint) are the
//     bundled implementations; the Phase-19.5 oracle can satisfy the same
//     interface without changes here.
//   - SwapRateStore / PgSwapRateStore — effective-dated persistence in
//     swap_rates (migration 114), plus the markup-policy and accrual-record
//     reads/writes the engine needs.
//   - SwapRateIngester — one PullOnce per day at the 17:00 ET rollover
//     boundary (RolloverClock drives the schedule; see swap_engine.go).
//
// Wire schema (file and HTTP share it):
//
//	[{"symbol":"EUR/USD","effective_date":"2026-01-05",
//	  "long_swap_points":"0.000021","short_swap_points":"-0.000025",
//	  "source":"REFINITIV"}]
//
// Fail-closed (spec §2.7): malformed feed rows are rejected individually
// and reported; a feed that yields no usable rows fails the pull. Missing
// rates at rollover surface as SWAP_RATE_STALE (spec §23, 503/L1), never
// an implicit zero.
package settlement

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excerrors "exchange/pkg/errors"
)

// Spec §23 / scaffold codes emitted by the swap-rate path. Central
// registration (code + HTTP status) is Phase-05 Task 5.3.21.
const (
	// CodeSwapRateStale — spec §23 (503, L1): no usable swap rate for the
	// requested effective date inside the staleness bound.
	CodeSwapRateStale = "SWAP_RATE_STALE"
	// CodeSwapFeedUnavailable — scaffold: the scheduled feed pull failed
	// outright (transport/parse) or produced zero usable rows.
	CodeSwapFeedUnavailable = "SWAP_FEED_UNAVAILABLE"
)

// SwapRate is one effective-dated Tom-Next point pair for an instrument.
// Points are signed: positive accrues TO the position holder (credit),
// negative is a charge (debit). Negative policy rates therefore post
// symmetrically (spec §5.21a.2).
type SwapRate struct {
	InstrumentID  int64
	Symbol        string
	EffectiveDate time.Time // normalized to UTC midnight
	LongPoints    decimal.Decimal
	ShortPoints   decimal.Decimal
	Source        string // REFINITIV | BLOOMBERG | FILE | HTTP | MANUAL
}

// swapRateWire is the feed JSON row shared by the file and HTTP feeds.
// Decimal fields accept both quoted and bare JSON numbers (shopspring
// unmarshals both); effective_date is "YYYY-MM-DD".
type swapRateWire struct {
	Symbol          string          `json:"symbol"`
	EffectiveDate   string          `json:"effective_date"`
	LongSwapPoints  decimal.Decimal `json:"long_swap_points"`
	ShortSwapPoints decimal.Decimal `json:"short_swap_points"`
	Source          string          `json:"source,omitempty"`
}

// parseSwapRateWire validates one wire row into a SwapRate (InstrumentID
// unresolved — the ingester maps symbols through InstrumentProvider).
// Empty effective_date falls back to fallbackDate.
func parseSwapRateWire(w swapRateWire, fallbackDate time.Time) (SwapRate, error) {
	r := SwapRate{
		Symbol:      NormalizeSymbol(w.Symbol),
		LongPoints:  w.LongSwapPoints,
		ShortPoints: w.ShortSwapPoints,
		Source:      strings.ToUpper(strings.TrimSpace(w.Source)),
	}
	if r.Symbol == "" || !strings.Contains(r.Symbol, "/") {
		return SwapRate{}, fmt.Errorf("invalid symbol %q", w.Symbol)
	}
	if r.Source == "" {
		r.Source = "MANUAL"
	}
	if strings.TrimSpace(w.EffectiveDate) == "" {
		r.EffectiveDate = normalizeDay(fallbackDate)
	} else {
		d, err := time.Parse("2006-01-02", strings.TrimSpace(w.EffectiveDate))
		if err != nil {
			return SwapRate{}, fmt.Errorf("invalid effective_date %q", w.EffectiveDate)
		}
		r.EffectiveDate = d
	}
	if !r.LongPoints.Round(8).Equal(r.LongPoints) ||
		!r.ShortPoints.Round(8).Equal(r.ShortPoints) {
		return SwapRate{}, fmt.Errorf("swap points exceed DECIMAL(28,8) quantum")
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// SwapRateFeed — the scheduled-pull seam
// ---------------------------------------------------------------------------

// SwapRateFeed fetches the daily swap-point sheet for asOf (a New York
// civil date context — implementations decide the effective date).
type SwapRateFeed interface {
	FetchSwapRates(ctx context.Context, asOf time.Time) ([]swapRateWire, error)
}

// FileSwapRateFeed reads a JSON sheet from disk — the ops drop-file path
// and the test fixture.
type FileSwapRateFeed struct {
	Path string
}

// FetchSwapRates implements SwapRateFeed.
func (f FileSwapRateFeed) FetchSwapRates(_ context.Context, _ time.Time) ([]swapRateWire, error) {
	raw, err := os.ReadFile(f.Path)
	if err != nil {
		return nil, excerrors.Wrap(CodeSwapFeedUnavailable,
			fmt.Sprintf("swap feed file %s", f.Path), err)
	}
	return decodeSwapRateWire(raw)
}

// HTTPSwapRateFeed pulls the daily sheet from a vendor endpoint. The
// effective date is passed as "?effective_date=YYYY-MM-DD" (appended with
// & when the URL already carries a query string).
type HTTPSwapRateFeed struct {
	URL    string
	Client *http.Client // nil → http.DefaultClient
	Source string       // stamped onto rows lacking a source (default HTTP)
}

// FetchSwapRates implements SwapRateFeed.
func (f HTTPSwapRateFeed) FetchSwapRates(ctx context.Context, asOf time.Time) ([]swapRateWire, error) {
	url := f.URL
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	url += sep + "effective_date=" + asOf.Format("2006-01-02")

	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, excerrors.Wrap(CodeSwapFeedUnavailable, "swap feed request", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, excerrors.Wrap(CodeSwapFeedUnavailable,
			"swap feed fetch "+f.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, excerrors.New(CodeSwapFeedUnavailable,
			fmt.Sprintf("swap feed %s returned HTTP %d", f.URL, resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, excerrors.Wrap(CodeSwapFeedUnavailable, "swap feed read", err)
	}
	return decodeSwapRateWire(body)
}

func decodeSwapRateWire(raw []byte) ([]swapRateWire, error) {
	var rows []swapRateWire
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, excerrors.Wrap(CodeSwapFeedUnavailable,
			"swap feed decode", err)
	}
	return rows, nil
}

// ---------------------------------------------------------------------------
// SwapRateStore — effective-dated persistence
// ---------------------------------------------------------------------------

// InstrumentIDResolver maps a canonical symbol ("EUR/USD") to its
// instruments.id — the ingester resolves feed symbols through it so bad
// or unlisted symbols are rejected rather than persisted unattributed.
// PgSwapRateStore implements it; tests substitute a static map.
type InstrumentIDResolver interface {
	InstrumentID(ctx context.Context, symbol string) (int64, error)
}

// SwapRateStore is the persistence seam for swap_rates plus the accrual
// reads/writes the engine needs. PgSwapRateStore is the production
// implementation; tests substitute an in-memory fake.
type SwapRateStore interface {
	// UpsertSwapRates inserts or refreshes rows on (instrument_id,
	// effective_date); returns the number of rows written.
	UpsertSwapRates(ctx context.Context, rates []SwapRate) (int, error)
	// SwapRateFor returns the rate effective on exactly date.
	SwapRateFor(ctx context.Context, instrumentID int64, date time.Time) (SwapRate, bool, error)
	// LatestSwapRate returns the most recent rate effective on or before
	// date — the fallback when the roll date's own row is absent (a vendor
	// non-publish day). Callers apply the staleness bound.
	LatestSwapRate(ctx context.Context, instrumentID int64, onOrBefore time.Time) (SwapRate, bool, error)
	// SwapRateHistory returns rates for the instrument in [from, to]
	// descending by date, capped at limit (<=0 → default 30).
	SwapRateHistory(ctx context.Context, instrumentID int64, from, to time.Time, limit int) ([]SwapRate, error)
	// ActiveSwapMarkupPolicies loads the ACTIVE swap_markup_policies rows
	// (migration 088) for markup resolution.
	ActiveSwapMarkupPolicies(ctx context.Context) ([]ledger.SwapMarkupPolicy, error)
	// InsertSwapAccrual persists one accrual audit row
	// (swap_accrual_records, migration 088); returns the row id.
	InsertSwapAccrual(ctx context.Context, rec ledger.SwapAccrualRecord) (int64, error)
}

// ---------------------------------------------------------------------------
// SwapRateIngester — scheduled daily pull
// ---------------------------------------------------------------------------

// IngestRejection records one refused feed row.
type IngestRejection struct {
	Row    int    `json:"row"`
	Symbol string `json:"symbol"`
	Reason string `json:"reason"`
}

// IngestReport summarizes one pull.
type IngestReport struct {
	AsOf     time.Time         `json:"as_of"`
	Fetched  int               `json:"fetched"`
	Stored   int               `json:"stored"`
	Rejected []IngestRejection `json:"rejected,omitempty"`
}

// SwapRateIngester runs the daily pull: fetch → validate → resolve symbols
// → upsert. Row-level defects are quarantined into the report (a single
// corrupt row must not starve the book of the remaining valid rates);
// transport failures and all-invalid batches fail the pull entirely.
type SwapRateIngester struct {
	feed   SwapRateFeed
	store  SwapRateStore
	ids    InstrumentIDResolver
	clock  func() time.Time
	source string // default source stamped on unlabelled rows
}

// NewSwapRateIngester wires the ingester. feed, store and ids are required
// (fail-closed); clock may be nil (real time).
func NewSwapRateIngester(feed SwapRateFeed, store SwapRateStore, ids InstrumentIDResolver, clock func() time.Time) (*SwapRateIngester, error) {
	if feed == nil || store == nil || ids == nil {
		return nil, fmt.Errorf("swap ingester: feed, store and symbol resolver are required")
	}
	if clock == nil {
		clock = time.Now
	}
	return &SwapRateIngester{feed: feed, store: store, ids: ids, clock: clock}, nil
}

// PullOnce fetches today's sheet and persists it. asOf is the New York
// civil date at pull time.
func (i *SwapRateIngester) PullOnce(ctx context.Context) (*IngestReport, error) {
	asOf := i.clock().UTC()
	rep := &IngestReport{AsOf: asOf}

	rows, err := i.feed.FetchSwapRates(ctx, asOf)
	if err != nil {
		return nil, err
	}
	rep.Fetched = len(rows)

	rates := make([]SwapRate, 0, len(rows))
	for idx, w := range rows {
		r, perr := parseSwapRateWire(w, asOf)
		if i.source != "" && r.Source == "MANUAL" {
			r.Source = i.source
		}
		if perr == nil {
			var id int64
			id, perr = i.ids.InstrumentID(ctx, r.Symbol)
			if perr == nil {
				if id <= 0 {
					perr = fmt.Errorf("instrument %s has no id", r.Symbol)
				} else {
					r.InstrumentID = id
				}
			}
		}
		if perr != nil {
			rep.Rejected = append(rep.Rejected, IngestRejection{
				Row: idx, Symbol: w.Symbol, Reason: perr.Error()})
			continue
		}
		rates = append(rates, r)
	}

	stored, err := i.store.UpsertSwapRates(ctx, rates)
	if err != nil {
		return rep, excerrors.Wrap(CodeSwapFeedUnavailable, "swap rate persist", err)
	}
	rep.Stored = stored

	if rep.Fetched > 0 && stored == 0 {
		return rep, excerrors.New(CodeSwapFeedUnavailable,
			fmt.Sprintf("swap feed produced %d rows, none usable (%d rejected)",
				rep.Fetched, len(rep.Rejected)))
	}
	return rep, nil
}

// WithSource stamps a default source label on feed rows lacking one.
func (i *SwapRateIngester) WithSource(source string) *SwapRateIngester {
	i.source = strings.ToUpper(strings.TrimSpace(source))
	return i
}

// RunDaily fires PullOnce at each rollover boundary (17:00 ET, DST-aware —
// RolloverClock.NextCutoff) until ctx is cancelled. Per-pull failures are
// reported through onErr and do not stop the loop; the ingester retries at
// the next boundary. The parallel-owned rollover daemon may instead call
// PullOnce itself — this loop is the standalone wiring path.
func (i *SwapRateIngester) RunDaily(ctx context.Context, rc *RolloverClock, onErr func(error)) error {
	for {
		next := rc.NextCutoff(i.clock())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
			if _, err := i.PullOnce(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// PgSwapRateStore — production SwapRateStore over pgx (PostgreSQL 16)
// ---------------------------------------------------------------------------

// PgSwapRateStore implements SwapRateStore and InstrumentIDResolver over
// the instruments, swap_rates, swap_markup_policies and
// swap_accrual_records tables. Numerics cross the wire as text (::text on
// read, string→::numeric on write) to avoid the pgtype-decimal shim.
type PgSwapRateStore struct {
	Pool *pgxpool.Pool
}

// NewPgSwapRateStore wraps a pool.
func NewPgSwapRateStore(pool *pgxpool.Pool) *PgSwapRateStore {
	return &PgSwapRateStore{Pool: pool}
}

// InstrumentID implements InstrumentIDResolver.
func (s *PgSwapRateStore) InstrumentID(ctx context.Context, symbol string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		`SELECT id FROM instruments WHERE symbol = $1`, NormalizeSymbol(symbol)).Scan(&id)
	if err != nil {
		if err == pgx.ErrNoRows {
			return 0, excerrors.New(codeNotFound, "unknown instrument "+symbol)
		}
		return 0, fmt.Errorf("instrument id lookup %s: %w", symbol, err)
	}
	return id, nil
}

// UpsertSwapRates implements SwapRateStore.
func (s *PgSwapRateStore) UpsertSwapRates(ctx context.Context, rates []SwapRate) (int, error) {
	n := 0
	for _, r := range rates {
		_, err := s.Pool.Exec(ctx, `
			INSERT INTO swap_rates
			    (instrument_id, effective_date, long_swap_points, short_swap_points, source)
			VALUES ($1, $2, $3::numeric, $4::numeric, $5)
			ON CONFLICT (instrument_id, effective_date)
			DO UPDATE SET long_swap_points = EXCLUDED.long_swap_points,
			              short_swap_points = EXCLUDED.short_swap_points,
			              source = EXCLUDED.source,
			              ingested_at = now()`,
			r.InstrumentID, r.EffectiveDate, r.LongPoints.String(),
			r.ShortPoints.String(), r.Source)
		if err != nil {
			return n, fmt.Errorf("swap_rates upsert instrument=%d date=%s: %w",
				r.InstrumentID, r.EffectiveDate.Format("2006-01-02"), err)
		}
		n++
	}
	return n, nil
}

// SwapRateFor implements SwapRateStore.
func (s *PgSwapRateStore) SwapRateFor(ctx context.Context, instrumentID int64, date time.Time) (SwapRate, bool, error) {
	var (
		r                 SwapRate
		longStr, shortStr string
		eff               time.Time
		symbol            string
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT sr.instrument_id, i.symbol, sr.effective_date,
		       sr.long_swap_points::text, sr.short_swap_points::text, sr.source
		FROM swap_rates sr JOIN instruments i ON i.id = sr.instrument_id
		WHERE sr.instrument_id = $1 AND sr.effective_date = $2`,
		instrumentID, normalizeDay(date)).
		Scan(&r.InstrumentID, &symbol, &eff, &longStr, &shortStr, &r.Source)
	if err != nil {
		if err == pgx.ErrNoRows {
			return SwapRate{}, false, nil
		}
		return SwapRate{}, false, fmt.Errorf("swap rate lookup: %w", err)
	}
	if err := scanRateDecimals(&r, eff, symbol, longStr, shortStr); err != nil {
		return SwapRate{}, false, err
	}
	return r, true, nil
}

// LatestSwapRate implements SwapRateStore.
func (s *PgSwapRateStore) LatestSwapRate(ctx context.Context, instrumentID int64, onOrBefore time.Time) (SwapRate, bool, error) {
	var (
		r                 SwapRate
		longStr, shortStr string
		eff               time.Time
		symbol            string
	)
	err := s.Pool.QueryRow(ctx, `
		SELECT sr.instrument_id, i.symbol, sr.effective_date,
		       sr.long_swap_points::text, sr.short_swap_points::text, sr.source
		FROM swap_rates sr JOIN instruments i ON i.id = sr.instrument_id
		WHERE sr.instrument_id = $1 AND sr.effective_date <= $2
		ORDER BY sr.effective_date DESC LIMIT 1`,
		instrumentID, normalizeDay(onOrBefore)).
		Scan(&r.InstrumentID, &symbol, &eff, &longStr, &shortStr, &r.Source)
	if err != nil {
		if err == pgx.ErrNoRows {
			return SwapRate{}, false, nil
		}
		return SwapRate{}, false, fmt.Errorf("swap rate latest: %w", err)
	}
	if err := scanRateDecimals(&r, eff, symbol, longStr, shortStr); err != nil {
		return SwapRate{}, false, err
	}
	return r, true, nil
}

// SwapRateHistory implements SwapRateStore.
func (s *PgSwapRateStore) SwapRateHistory(ctx context.Context, instrumentID int64, from, to time.Time, limit int) ([]SwapRate, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT sr.instrument_id, i.symbol, sr.effective_date,
		       sr.long_swap_points::text, sr.short_swap_points::text, sr.source
		FROM swap_rates sr JOIN instruments i ON i.id = sr.instrument_id
		WHERE sr.instrument_id = $1 AND sr.effective_date >= $2 AND sr.effective_date <= $3
		ORDER BY sr.effective_date DESC LIMIT $4`,
		instrumentID, normalizeDay(from), normalizeDay(to), limit)
	if err != nil {
		return nil, fmt.Errorf("swap rate history: %w", err)
	}
	defer rows.Close()
	var out []SwapRate
	for rows.Next() {
		var (
			r                 SwapRate
			longStr, shortStr string
			eff               time.Time
			symbol            string
		)
		if err := rows.Scan(&r.InstrumentID, &symbol, &eff, &longStr, &shortStr, &r.Source); err != nil {
			return nil, fmt.Errorf("swap rate history scan: %w", err)
		}
		if err := scanRateDecimals(&r, eff, symbol, longStr, shortStr); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveSwapMarkupPolicies implements SwapRateStore.
func (s *PgSwapRateStore) ActiveSwapMarkupPolicies(ctx context.Context) ([]ledger.SwapMarkupPolicy, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, COALESCE(instrument_id, 0),
		       long_markup_bps::text, short_markup_bps::text,
		       status::text, proposed_by, COALESCE(approved_by, '')
		FROM swap_markup_policies`)
	if err != nil {
		return nil, fmt.Errorf("swap markup policies: %w", err)
	}
	defer rows.Close()
	var out []ledger.SwapMarkupPolicy
	for rows.Next() {
		var (
			p          ledger.SwapMarkupPolicy
			lm, sm, st string
		)
		if err := rows.Scan(&p.ID, &p.InstrumentID, &lm, &sm, &st, &p.ProposedBy, &p.ApprovedBy); err != nil {
			return nil, fmt.Errorf("swap markup scan: %w", err)
		}
		var err error
		if p.LongMarkupBps, err = decimal.NewFromString(lm); err != nil {
			return nil, fmt.Errorf("swap markup long parse %q: %w", lm, err)
		}
		if p.ShortMarkupBps, err = decimal.NewFromString(sm); err != nil {
			return nil, fmt.Errorf("swap markup short parse %q: %w", sm, err)
		}
		p.Status = ledger.MarkupPolicyStatus(st)
		out = append(out, p)
	}
	return out, rows.Err()
}

// InsertSwapAccrual implements SwapRateStore (swap_accrual_records,
// migration 088).
func (s *PgSwapRateStore) InsertSwapAccrual(ctx context.Context, rec ledger.SwapAccrualRecord) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, ledger.SwapAccrualInsertSQL,
		rec.AccountID, rec.PositionID, rec.InstrumentID, rec.Symbol,
		string(rec.Side), rec.Currency, rec.Days, string(rec.DayCount),
		rec.InterbankAmount.String(), rec.MarkupAmount.String(),
		rec.ClientDelta.String(), rec.MarkupBps.String(), rec.SwapFree,
		rec.ForegoneAmount.String(), rec.Narrative, rec.JournalEntryID).
		Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("swap accrual insert: %w", err)
	}
	return id, nil
}

// scanRateDecimals fills the decimal/date fields of r from text columns.
func scanRateDecimals(r *SwapRate, eff time.Time, symbol, longStr, shortStr string) error {
	var err error
	if r.LongPoints, err = decimal.NewFromString(longStr); err != nil {
		return fmt.Errorf("swap rate long parse %q: %w", longStr, err)
	}
	if r.ShortPoints, err = decimal.NewFromString(shortStr); err != nil {
		return fmt.Errorf("swap rate short parse %q: %w", shortStr, err)
	}
	r.EffectiveDate = normalizeDay(eff)
	r.Symbol = symbol
	return nil
}
