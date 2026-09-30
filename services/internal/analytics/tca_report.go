// TCA reporting — Phase-20 Task 20.3.9 items 3–6: aggregated
// execution-quality reads (daily / monthly / quarterly buckets), the
// account-scoped REST backing query, the quarterly RTS 28 summary job
// (persisted period='quarterly' rollup rows + an archived PDF per
// account), and the venue-comparison section used by the institutional
// white-label render.
package analytics

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/objectstore"
	"exchange/internal/reporting"
	"exchange/pkg/decimal"
)

// Report periods — the `period` column vocabulary on tca_results.
const (
	PeriodFill      = "fill"
	PeriodDaily     = "daily"
	PeriodMonthly   = "monthly"
	PeriodQuarterly = "quarterly"
)

// Venue labels — the exchange operates a single internal order book;
// the venue-comparison report still groups by venue so external venues
// (Phase-24 CLS/PB give-ups routed out) slot in without a schema change.
const VenueInternal = "EXC.LOCAL"

// AggregateRow is one (bucket, account, instrument) rollup over the
// per-fill rows.
type AggregateRow struct {
	BucketStart  time.Time `json:"bucket_start"`
	AccountID    int64     `json:"account_id"`
	InstrumentID int64     `json:"instrument_id"`
	Symbol       string    `json:"symbol"`
	Fills        int64     `json:"fills"`
	// Averages over fills with the benchmark present (NULLs excluded by
	// CH avg() semantics).
	AvgSlipArrivalBps *decimal.Decimal `json:"avg_slip_arrival_bps,omitempty"`
	AvgSlipVWAPBps    *decimal.Decimal `json:"avg_slip_vwap_bps,omitempty"`
	AvgSlipFixBps     *decimal.Decimal `json:"avg_slip_fix_bps,omitempty"`
	// AvgImprovement averages the recorded price_improvement_delta values
	// (limit − exec; §24 #400 — improvement appears in reports).
	AvgImprovement *decimal.Decimal `json:"avg_price_improvement_delta,omitempty"`
}

// ReportFilter narrows an aggregate read.
type ReportFilter struct {
	AccountID int64 // 0 = all accounts (RTS 28 venue view)
	Period    string
	// InstrumentIDs restricts to a resolved class set (nil = all).
	InstrumentIDs []int64
	// From/To bound the fill timestamps (zero = unbounded).
	From, To time.Time
}

// ReportQuerier reads aggregates — CH impl + mem fake.
type ReportQuerier interface {
	Aggregate(ctx context.Context, f ReportFilter) ([]AggregateRow, error)
}

// bucketExpr maps the API period onto the ClickHouse bucket function.
func bucketExpr(period string) (string, error) {
	switch strings.ToLower(period) {
	case "", PeriodDaily:
		return "toStartOfDay(ts)", nil
	case PeriodMonthly:
		return "toStartOfMonth(ts)", nil
	case PeriodQuarterly:
		return "toStartOfQuarter(ts)", nil
	default:
		return "", fmt.Errorf("tca: unknown period %q", period)
	}
}

// CHReportStore runs the aggregate query over tca_results.
type CHReportStore struct {
	CH    Conn
	Table string // override; default TCATable
}

// NewCHReportStore wires the store.
func NewCHReportStore(ch Conn) *CHReportStore {
	return &CHReportStore{CH: ch, Table: TCATable}
}

// Aggregate implements ReportQuerier — roll up period='fill' rows into
// the requested bucket granularity. Decimal columns scan as strings to
// keep the Decimal-type mapping driver-agnostic.
func (s *CHReportStore) Aggregate(ctx context.Context, f ReportFilter) ([]AggregateRow, error) {
	table := s.Table
	if table == "" {
		table = TCATable
	}
	bucket, err := bucketExpr(f.Period)
	if err != nil {
		return nil, err
	}
	var sb strings.Builder
	sb.WriteString("SELECT " + bucket + " AS bucket, account_id, instrument_id, symbol, " +
		"count() AS fills, " +
		"avg(slip_arrival_bps), avg(slip_vwap_bps), avg(slip_fix_bps), " +
		"avg(price_improvement_delta) " +
		"FROM " + table + " WHERE period = 'fill'")
	args := []any{}
	if f.AccountID != 0 {
		sb.WriteString(" AND account_id = ?")
		args = append(args, f.AccountID)
	}
	if len(f.InstrumentIDs) > 0 {
		sb.WriteString(" AND instrument_id IN (")
		for i, id := range f.InstrumentIDs {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("?")
			args = append(args, id)
		}
		sb.WriteString(")")
	}
	if !f.From.IsZero() {
		sb.WriteString(" AND ts >= ?")
		args = append(args, f.From.UTC())
	}
	if !f.To.IsZero() {
		sb.WriteString(" AND ts < ?")
		args = append(args, f.To.UTC())
	}
	sb.WriteString(" GROUP BY bucket, account_id, instrument_id, symbol ORDER BY bucket, symbol")
	rows, err := s.CH.Query(ctx, sb.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("tca: aggregate query: %w", err)
	}
	defer rows.Close()
	out := []AggregateRow{}
	for rows.Next() {
		var (
			r                        AggregateRow
			ts                       time.Time
			fills                    uint64 // CH count() is UInt64
			slipA, slipV, slipF, imp *float64
		)
		if err := rows.Scan(&ts, &r.AccountID, &r.InstrumentID, &r.Symbol,
			&fills, &slipA, &slipV, &slipF, &imp); err != nil {
			return nil, fmt.Errorf("tca: aggregate scan: %w", err)
		}
		r.BucketStart = ts.UTC()
		r.Fills = int64(fills)
		r.AvgSlipArrivalBps = f64ToDec(slipA)
		r.AvgSlipVWAPBps = f64ToDec(slipV)
		r.AvgSlipFixBps = f64ToDec(slipF)
		r.AvgImprovement = f64ToDec(imp)
		out = append(out, r)
	}
	return out, rows.Err()
}

// f64ToDec converts a nullable CH Float64 (avg() of Decimal) into the
// decimal facade — averages are statistics, not financial quantities,
// so the float bridge is honest here.
func f64ToDec(f *float64) *decimal.Decimal {
	if f == nil {
		return nil
	}
	d := decimal.NewFromFloat(*f)
	return &d
}

// MemReportStore aggregates MemTCASink-style rows in-memory (tests +
// the no-CH dev path).
type MemReportStore struct {
	Fills []TCARecord
}

// Aggregate implements ReportQuerier.
func (s *MemReportStore) Aggregate(_ context.Context, f ReportFilter) ([]AggregateRow, error) {
	type key struct {
		bucket time.Time
		acct   int64
		inst   int64
		sym    string
	}
	groups := map[key][]TCARecord{}
	for _, r := range s.Fills {
		if r.Period != PeriodFill {
			continue
		}
		if f.AccountID != 0 && r.AccountID != f.AccountID {
			continue
		}
		if len(f.InstrumentIDs) > 0 {
			hit := false
			for _, id := range f.InstrumentIDs {
				if r.InstrumentID == id {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		if !f.From.IsZero() && r.Ts.Before(f.From) {
			continue
		}
		if !f.To.IsZero() && !r.Ts.Before(f.To) {
			continue
		}
		groups[key{bucketStart(r.Ts, f.Period), r.AccountID, r.InstrumentID, r.Symbol}] =
			append(groups[key{bucketStart(r.Ts, f.Period), r.AccountID, r.InstrumentID, r.Symbol}], r)
	}
	out := []AggregateRow{}
	for k, rows := range groups {
		ar := AggregateRow{
			BucketStart: k.bucket, AccountID: k.acct,
			InstrumentID: k.inst, Symbol: k.sym, Fills: int64(len(rows)),
			AvgSlipArrivalBps: avgDec(rows, func(r TCARecord) *decimal.Decimal { return r.SlipArrivalBps }),
			AvgSlipVWAPBps:    avgDec(rows, func(r TCARecord) *decimal.Decimal { return r.SlipVWAPBps }),
			AvgSlipFixBps:     avgDec(rows, func(r TCARecord) *decimal.Decimal { return r.SlipFixBps }),
			AvgImprovement:    avgDec(rows, func(r TCARecord) *decimal.Decimal { return r.PriceImprovement }),
		}
		out = append(out, ar)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].BucketStart.Equal(out[j].BucketStart) {
			return out[i].BucketStart.Before(out[j].BucketStart)
		}
		return out[i].Symbol < out[j].Symbol
	})
	return out, nil
}

// bucketStart truncates t to the requested period boundary.
func bucketStart(t time.Time, period string) time.Time {
	u := t.UTC()
	switch strings.ToLower(period) {
	case PeriodMonthly:
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	case PeriodQuarterly:
		m := ((int(u.Month())-1)/3)*3 + 1
		return time.Date(u.Year(), time.Month(m), 1, 0, 0, 0, 0, time.UTC)
	default: // daily
		return u.Truncate(24 * time.Hour)
	}
}

func avgDec(rows []TCARecord, pick func(TCARecord) *decimal.Decimal) *decimal.Decimal {
	sum, n := decimal.Zero, int64(0)
	for _, r := range rows {
		if v := pick(r); v != nil {
			sum = sum.Add(*v)
			n++
		}
	}
	if n == 0 {
		return nil
	}
	d := sum.Div(decimal.NewFromInt(n))
	return &d
}

// ---------------------------------------------------------------------------
// Instrument-class filter
// ---------------------------------------------------------------------------

// Instrument-class vocabulary — the instruments table's instrument_type
// axis plus the FX pair-class taxonomy used by RTS 28 grouping.
const (
	ClassSpot    = "SPOT"
	ClassForward = "FORWARD"
	ClassSwap    = "SWAP"
	ClassNDF     = "NDF"
	ClassOption  = "OPTION"
	// Pair classes (FX Global Code / ESMA leverage taxonomy).
	ClassFXMajor  = "FX_MAJOR"
	ClassFXMinor  = "FX_MINOR"
	ClassFXExotic = "FX_EXOTIC"
)

// majorCCY is the G10 set — majors are USD pairs of these, minors the
// non-USD crosses between them, everything else is exotic.
var majorCCY = map[string]bool{
	"USD": true, "EUR": true, "JPY": true, "GBP": true,
	"AUD": true, "NZD": true, "CAD": true, "CHF": true,
}

// ClassifyPair maps a base/quote currency pair onto FX_MAJOR / FX_MINOR /
// FX_EXOTIC.
func ClassifyPair(base, quote string) string {
	if base == "USD" || quote == "USD" {
		if majorCCY[base] && majorCCY[quote] {
			return ClassFXMajor
		}
		return ClassFXExotic
	}
	if majorCCY[base] && majorCCY[quote] {
		return ClassFXMinor
	}
	return ClassFXExotic
}

// InstrumentClassResolver maps an instrument_class filter value to the
// instrument id set it covers — REST layer resolves once per request.
type InstrumentClassResolver interface {
	InstrumentIDs(ctx context.Context, class string) ([]int64, error)
}

// PgInstrumentClassResolver resolves class filters over the instruments
// table: product classes (SPOT|FORWARD|SWAP|NDF|OPTION) hit the
// instrument_type column; FX_MAJOR/MINOR/EXOTIC classify currency pairs
// in Go (the taxonomy is derivable, not stored).
type PgInstrumentClassResolver struct {
	Pool *pgxpool.Pool
}

// NewPgInstrumentClassResolver wraps a pool.
func NewPgInstrumentClassResolver(pool *pgxpool.Pool) *PgInstrumentClassResolver {
	return &PgInstrumentClassResolver{Pool: pool}
}

// InstrumentIDs implements InstrumentClassResolver. Unknown class →
// error (fail closed — a typo'd class must not silently widen the
// report to everything).
func (r *PgInstrumentClassResolver) InstrumentIDs(ctx context.Context, class string) ([]int64, error) {
	c := strings.ToUpper(strings.TrimSpace(class))
	switch c {
	case ClassSpot, ClassForward, ClassSwap, ClassNDF, ClassOption:
		rows, err := r.Pool.Query(ctx,
			`SELECT id FROM instruments WHERE instrument_type::text = $1`, c)
		if err != nil {
			return nil, fmt.Errorf("tca: class filter %s: %w", c, err)
		}
		defer rows.Close()
		return scanIDs(rows)
	case ClassFXMajor, ClassFXMinor, ClassFXExotic:
		rows, err := r.Pool.Query(ctx,
			`SELECT id, base_currency, quote_currency FROM instruments`)
		if err != nil {
			return nil, fmt.Errorf("tca: class filter %s: %w", c, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var id int64
			var b, q string
			if err := rows.Scan(&id, &b, &q); err != nil {
				return nil, err
			}
			if ClassifyPair(b, q) == c {
				out = append(out, id)
			}
		}
		return out, rows.Err()
	default:
		return nil, fmt.Errorf("tca: unknown instrument_class %q", class)
	}
}

func scanIDs(rows pgx.Rows) ([]int64, error) {
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// RTS 28 quarterly summary job (MiFID II RTS 28 — top-5 venue
// execution-quality disclosure)
// ---------------------------------------------------------------------------

// RTS28SummaryJob produces the quarterly per-account execution-quality
// rollup: persisted tca_results rows with period='quarterly' plus an
// archived PDF summary per account (venue-comparison section included —
// the venue axis is part of the record even though the venue is
// currently EXC.LOCAL only).
//
// Orchestrator binding: run monthly/daily; the job is idempotent per
// quarter — the ReplacingMergeTree `ver` column collapses reruns to the
// latest version.
type RTS28SummaryJob struct {
	Reports ReportQuerier
	Sink    TCASink
	Objects objectstore.Client // nil → PDF render skipped (rows still persist)
	Now     func() time.Time
	Logf    func(string, ...any)
}

// NewRTS28SummaryJob wires the job.
func NewRTS28SummaryJob(reports ReportQuerier, sink TCASink, objects objectstore.Client) (*RTS28SummaryJob, error) {
	if reports == nil || sink == nil {
		return nil, fmt.Errorf("tca: RTS28 job requires reports+sink")
	}
	return &RTS28SummaryJob{
		Reports: reports, Sink: sink, Objects: objects,
		Now: time.Now,
	}, nil
}

func (j *RTS28SummaryJob) log(format string, args ...any) {
	if j.Logf != nil {
		j.Logf(format, args...)
	}
}

// QuarterContaining returns the quarter's first instant for t.
func QuarterContaining(t time.Time) time.Time {
	return bucketStart(t.UTC(), PeriodQuarterly)
}

// RunQuarter generates the summary for the quarter beginning at
// quarterStart (normalized to the quarter boundary). Returns the number
// of rollup rows persisted.
func (j *RTS28SummaryJob) RunQuarter(ctx context.Context, quarterStart time.Time) (int, error) {
	qs := QuarterContaining(quarterStart)
	qe := qs.AddDate(0, 3, 0)
	rows, err := j.Reports.Aggregate(ctx, ReportFilter{
		Period: PeriodQuarterly, From: qs, To: qe,
	})
	if err != nil {
		return 0, fmt.Errorf("tca: rts28 aggregate: %w", err)
	}
	now := j.Now().UTC()
	ver := uint64(now.UnixNano())
	// Group by account for the archived per-account documents.
	byAcct := map[int64][]AggregateRow{}
	for _, r := range rows {
		byAcct[r.AccountID] = append(byAcct[r.AccountID], r)
		rec := TCARecord{
			AccountID: r.AccountID, InstrumentID: r.InstrumentID,
			Symbol: r.Symbol, ExecPrice: decimal.Zero,
			SlipArrivalBps: r.AvgSlipArrivalBps, SlipVWAPBps: r.AvgSlipVWAPBps,
			SlipFixBps: r.AvgSlipFixBps, PriceImprovement: r.AvgImprovement,
			Period: PeriodQuarterly, PeriodStart: qs,
			Ts:  now,
			Ver: ver,
		}
		if err := j.Sink.InsertTCA(ctx, rec); err != nil {
			return len(byAcct) - 1, fmt.Errorf("tca: rts28 persist acct %d: %w",
				r.AccountID, err)
		}
	}
	if j.Objects != nil {
		for acct, acctRows := range byAcct {
			pdf := RenderRTS28PDF(acct, qs, acctRows)
			key := fmt.Sprintf("rts28/%s/%d.pdf",
				qs.Format("2006")+"Q"+quarterNum(qs), acct)
			if _, err := j.Objects.Put(ctx, objectstore.PutInput{
				Key: key, ContentType: "application/pdf",
				Size: int64(len(pdf)),
				Body: bytes.NewReader(pdf),
				Metadata: map[string]string{
					"retention-class": "7y", "doc-type": "rts28_summary",
					"account-id": fmt.Sprint(acct), "period": qs.Format("2006-01"),
				},
				// 7-year regulatory retention — COMPLIANCE-mode object
				// lock, not just a metadata label (MiFID RTS 28 records).
				ObjectLockMode:        "COMPLIANCE",
				ObjectLockRetainUntil: now.AddDate(7, 0, 0),
			}); err != nil {
				return len(byAcct), fmt.Errorf("tca: rts28 archive %s: %w", key, err)
			}
		}
	}
	return len(rows), nil
}

func quarterNum(q time.Time) string {
	return fmt.Sprint((int(q.Month())-1)/3 + 1)
}

// RenderRTS28PDF renders the quarterly venue-comparison summary — one
// section per venue (the venue axis is carried even in single-venue
// operation so a second venue's onboarding needs no doc change).
func RenderRTS28PDF(accountID int64, quarterStart time.Time, rows []AggregateRow) []byte {
	lines := []reporting.PDFLine{
		{Text: "RTS 28 Execution Quality Summary — " + VenueInternal, Size: 15},
		{Text: fmt.Sprintf("Account %d   Quarter %s-Q%s", accountID,
			quarterStart.Format("2006"), quarterNum(quarterStart)), Size: 11},
		{Text: fmt.Sprintf("Generated %s UTC", time.Now().UTC().Format("2006-01-02 15:04:05")), Size: 9},
		{Text: " ", Size: 8},
		{Text: "Venue comparison (avg slippage in bps vs benchmark)", Size: 11},
		{Text: fmt.Sprintf("%-10s %-8s %-8s %6s %10s %10s %10s %14s",
			"Venue", "Symbol", "Inst", "Fills", "ArrivalBps", "VWAPBps", "FixBps", "Improvement"), Size: 9},
	}
	for _, r := range rows {
		lines = append(lines, reporting.PDFLine{Text: fmt.Sprintf("%-10s %-8s %-8d %6d %10s %10s %10s %14s",
			VenueInternal, truncSym(r.Symbol), r.InstrumentID, r.Fills,
			decPtrStr(r.AvgSlipArrivalBps), decPtrStr(r.AvgSlipVWAPBps),
			decPtrStr(r.AvgSlipFixBps), decPtrStr(r.AvgImprovement)), Size: 9})
	}
	lines = append(lines,
		reporting.PDFLine{Text: " ", Size: 8},
		reporting.PDFLine{Text: "Positive slippage = execution worse than benchmark; improvement = limit minus exec.", Size: 8})
	return reporting.RenderPDFDoc(lines)
}

func truncSym(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

func decPtrStr(d *decimal.Decimal) string {
	if d == nil {
		return "-"
	}
	return d.StringFixed(2)
}
