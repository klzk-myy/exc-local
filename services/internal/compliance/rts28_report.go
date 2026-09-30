// RTS 28 annual top-5 venue publication — Phase-21 Task 21.3.19
// (spec §14.5, §24 #202): per instrument class, the top 5 execution
// venues by volume broken down by client category
// (RETAIL|PROFESSIONAL|ELIGIBLE_COUNTERPARTY).
//
// The venue operates a single internal order book — the venue axis is
// carried explicitly (analytics.VenueInternal) so external venues slot
// in without a schema change when CLS/PB give-up routing lands.
// Volume attribution: each fill counts once per client side — a trade
// between a RETAIL and a PROFESSIONAL account lands its volume in both
// categories (each client's own order executed on this venue).
// Client category resolves through accounts.client_category (migration
// 042) in Go — the CH trades table deliberately carries no category
// column.
//
// Distinct from analytics.RTS28SummaryJob (Phase-20 Task 20.3.9): that
// job writes the per-account TCA quarterly rollup; this file owns the
// PUBLIC annual venue publication the MiFID II best-execution duty
// requires.
package compliance

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/internal/analytics"
	"exchange/internal/audit"
	excerrors "exchange/pkg/errors"
)

// RTS28Service owns the annual publication.
type RTS28Service struct {
	pool     *pgxpool.Pool
	ch       analytics.Conn
	resolver HoldRoleResolver
	now      func() time.Time
}

// NewRTS28Service wires the service; ch may be nil — generation then
// fails closed SERVICE_DEGRADED.
func NewRTS28Service(pool *pgxpool.Pool, ch analytics.Conn,
	resolver HoldRoleResolver) (*RTS28Service, error) {
	if pool == nil {
		return nil, excerrors.New("INTERNAL_ERROR",
			"rts28 service requires pool")
	}
	return &RTS28Service{pool: pool, ch: ch, resolver: resolver,
		now: time.Now}, nil
}

// WithClock overrides the clock (tests).
func (s *RTS28Service) WithClock(c func() time.Time) *RTS28Service {
	s.now = c
	return s
}

func (s *RTS28Service) checkRole(ctx context.Context, userID int64) error {
	if s.resolver == nil {
		return excerrors.New("INTERNAL_ERROR", "role resolver not wired")
	}
	role, err := s.resolver(ctx, userID)
	if err != nil {
		return excerrors.New("INTERNAL_ERROR", "role lookup: "+err.Error())
	}
	if role != "Compliance Officer" && role != "Super Admin" {
		return excerrors.New("UNAUTHORIZED_ROLE",
			"role "+role+" cannot mutate RTS 28 reports")
	}
	return nil
}

// Client categories (accounts.client_category, migration 042).
const (
	CatRetail     = "RETAIL"
	CatPro        = "PROFESSIONAL"
	CatEligibleCP = "ELIGIBLE_COUNTERPARTY"
)

// VenueRankRow is one venue's RTS 28 row inside a category table.
type VenueRankRow struct {
	Rank              int      `json:"rank"`
	Venue             string   `json:"venue"`         // EXC.LOCAL while internal-only
	LEI               *string  `json:"lei,omitempty"` // venue LEI when registered
	PctVolume         *float64 `json:"pct_volume"`    // of category volume on ranked venues
	PctOrders         *float64 `json:"pct_orders"`
	PctAggressive     *float64 `json:"pct_aggressive_orders"`
	PctPassive        *float64 `json:"pct_passive_orders"`
	VolumeBase        float64  `json:"volume_base"`
	Orders            int64    `json:"orders"`
	Counterparties    int      `json:"counterparties"`
	AnonymizationNote string   `json:"anonymization_note,omitempty"`
}

// CategoryTable is the per-category RTS 28 section.
type CategoryTable struct {
	Category        string         `json:"category"`
	TotalVolume     float64        `json:"total_volume"`
	TotalOrders     int64          `json:"total_orders"`
	Venues          []VenueRankRow `json:"venues"` // ≤5, volume-ranked
	QualitativeNote string         `json:"qualitative_note,omitempty"`
}

// RTS28Report is one rts28_reports row.
type RTS28Report struct {
	ID                    int64           `json:"id"`
	Year                  int             `json:"year"`
	InstrumentClass       string          `json:"instrument_class"`
	Version               int             `json:"version"`
	Status                string          `json:"status"`
	Categories            json.RawMessage `json:"categories"`
	QualitativeAssessment string          `json:"qualitative_assessment,omitempty"`
	CSV                   string          `json:"-"`
	GeneratedBy           int64           `json:"generated_by"`
	CreatedAt             time.Time       `json:"created_at"`
	PublishedBy           *int64          `json:"published_by,omitempty"`
	PublishedAt           *time.Time      `json:"published_at,omitempty"`
}

// acctSide is the per-fill, per-client-side observation the Go rollup
// consumes — (buy account, sell account, aggressor flag).
type sideObs struct {
	instID     int64
	buyAcct    int64 // trades.maker_account_id — buy-side (schema caveat)
	sellAcct   int64 // trades.taker_account_id — sell-side
	aggressor  string
	fills      int64
	volumeBase float64
}

// GenerateYear rolls the year's trades up per instrument class ×
// client category × venue and files one DRAFT report per class.
// Versioned per (year, class) — regeneration lands version+1.
func (s *RTS28Service) GenerateYear(ctx context.Context, year int,
	actor int64) ([]RTS28Report, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if s.ch == nil {
		return nil, excerrors.New("SERVICE_DEGRADED",
			"clickhouse not wired — RTS 28 generation unavailable")
	}
	if year < 2000 || year > 2100 {
		return nil, excerrors.New("INVALID_REQUEST", "bad year")
	}
	yearStart := time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC)
	yearEnd := yearStart.AddDate(1, 0, 0)

	meta, err := s.instrumentMeta(ctx)
	if err != nil {
		return nil, err
	}
	cats, err := s.accountCategories(ctx)
	if err != nil {
		return nil, err
	}

	rows, err := s.ch.Query(ctx, `
		SELECT instrument_id, maker_account_id, taker_account_id,
		       aggressor_side, count(), sum(qty)
		  FROM `+"exchange_analytics.trades"+`
		 WHERE ts >= ? AND ts < ?
		 GROUP BY instrument_id, maker_account_id, taker_account_id,
		          aggressor_side`, yearStart, yearEnd)
	if err != nil {
		return nil, excerrors.Wrap("SERVICE_DEGRADED",
			"rts28 trades aggregate failed", err)
	}
	// class → category → accumulated row
	type acc struct {
		volume       float64
		orders       int64
		aggressive   int64
		counterparty map[int64]bool
	}
	agg := map[string]map[string]*acc{}
	var classesTouched []string
	for rows.Next() {
		var o sideObs
		var fills uint64
		if err := rows.Scan(&o.instID, &o.buyAcct, &o.sellAcct,
			&o.aggressor, &fills, &o.volumeBase); err != nil {
			rows.Close()
			return nil, excerrors.Wrap("INTERNAL_ERROR",
				"rts28 trades scan", err)
		}
		o.fills = int64(fills)
		m, ok := meta[o.instID]
		if !ok {
			continue // instrument unmapped (delisted pre-register) — skip
		}
		class := m.class
		buyAgg := o.aggressor == "BUY"
		sellAgg := o.aggressor == "SELL"
		for _, side := range []struct {
			acct int64
			agg  bool
		}{{o.buyAcct, buyAgg}, {o.sellAcct, sellAgg}} {
			if side.acct == 0 {
				continue // unresolved order→account — excluded, not guessed
			}
			cat := cats[side.acct]
			if cat == "" {
				cat = CatRetail // onboarding default per migration 042
			}
			if agg[class] == nil {
				agg[class] = map[string]*acc{}
				classesTouched = append(classesTouched, class)
			}
			a := agg[class][cat]
			if a == nil {
				a = &acc{counterparty: map[int64]bool{}}
				agg[class][cat] = a
			}
			a.volume += o.volumeBase
			a.orders += o.fills
			if side.agg {
				a.aggressive += o.fills
			}
			a.counterparty[side.acct] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(classesTouched)

	var out []RTS28Report
	for _, class := range classesTouched {
		catTables := map[string]CategoryTable{}
		for _, cat := range []string{CatRetail, CatPro, CatEligibleCP} {
			a := agg[class][cat]
			if a == nil || a.orders == 0 {
				continue // category absent in this class — omitted section
			}
			row := VenueRankRow{
				Rank: 1, Venue: analytics.VenueInternal,
				VolumeBase: a.volume, Orders: a.orders,
				Counterparties: len(a.counterparty),
			}
			pv := 100.0
			po := 100.0
			row.PctVolume, row.PctOrders = &pv, &po
			aggPct := float64(a.aggressive) / float64(a.orders) * 100
			pasPct := 100 - aggPct
			row.PctAggressive, row.PctPassive = &aggPct, &pasPct
			if len(a.counterparty) < 5 {
				// SDD edge: <5 counterparties — counterparty detail is
				// never emitted, the note records why.
				row.AnonymizationNote = "fewer than 5 client counterparties — " +
					"counterparty-level detail suppressed"
			}
			catTables[cat] = CategoryTable{
				Category: cat, TotalVolume: a.volume,
				TotalOrders: a.orders, Venues: []VenueRankRow{row},
			}
		}
		if len(catTables) == 0 {
			continue
		}
		raw, _ := json.Marshal(catTables)
		csv := rts28CSV(year, class, catTables)
		var id int64
		var ver int
		err := s.pool.QueryRow(ctx, `
			INSERT INTO rts28_reports
			    (year, instrument_class, version, categories, csv,
			     generated_by)
			VALUES ($1,$2,
			    COALESCE((SELECT max(version) FROM rts28_reports
			              WHERE year=$1 AND instrument_class=$2),0)+1,
			    $3,$4,$5)
			RETURNING id, version`, year, class, raw, csv, actor).
			Scan(&id, &ver)
		if err != nil {
			return out, excerrors.Wrap("INTERNAL_ERROR",
				"rts28 report insert", err)
		}
		if _, err := audit.AppendAuto(ctx, s.pool, "rts28_reports", &id,
			"RTS28_GENERATED", nil); err != nil {
			return out, excerrors.Wrap("INTERNAL_ERROR", "rts28 audit", err)
		}
		out = append(out, RTS28Report{ID: id, Year: year,
			InstrumentClass: class, Version: ver, Status: "DRAFT",
			Categories: raw, GeneratedBy: actor,
			CreatedAt: s.now().UTC()})
	}
	return out, nil
}

// rts28CSV renders the ESMA-style CSV: one section per category.
func rts28CSV(year int, class string, cats map[string]CategoryTable) string {
	var b strings.Builder
	b.WriteString("year,instrument_class,client_category,rank,venue," +
		"pct_volume,pct_orders,pct_passive_orders,pct_aggressive_orders," +
		"volume_base,orders,counterparties\n")
	for _, cat := range []string{CatRetail, CatPro, CatEligibleCP} {
		t, ok := cats[cat]
		if !ok {
			continue
		}
		for _, v := range t.Venues {
			pct := func(p *float64) string {
				if p == nil {
					return ""
				}
				return fmt.Sprintf("%.6f", *p)
			}
			fmt.Fprintf(&b, "%d,%s,%s,%d,%s,%s,%s,%s,%s,%.8f,%d,%d\n",
				year, class, cat, v.Rank, v.Venue,
				pct(v.PctVolume), pct(v.PctOrders), pct(v.PctPassive),
				pct(v.PctAggressive), v.VolumeBase, v.Orders,
				v.Counterparties)
		}
	}
	return b.String()
}

// instrumentMeta resolves id → product_type/pair_class/class — same
// taxonomy as rts27_report.go.
func (s *RTS28Service) instrumentMeta(ctx context.Context) (map[int64]instMeta, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, instrument_type::text, base_currency, quote_currency
		   FROM instruments`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "instrument meta", err)
	}
	defer rows.Close()
	out := map[int64]instMeta{}
	for rows.Next() {
		var id int64
		var pt, b, q string
		if err := rows.Scan(&id, &pt, &b, &q); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "instrument scan", err)
		}
		pc := analytics.ClassifyPair(b, q)
		out[id] = instMeta{productType: pt, pairClass: pc, class: pt + ":" + pc}
	}
	return out, rows.Err()
}

// accountCategories resolves id → client_category for every account
// that traded in the window (accounts carry the regulatory category —
// migration 042).
func (s *RTS28Service) accountCategories(ctx context.Context) (map[int64]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, client_category::text FROM accounts`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "account categories", err)
	}
	defer rows.Close()
	out := map[int64]string{}
	for rows.Next() {
		var id int64
		var c string
		if err := rows.Scan(&id, &c); err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "category scan", err)
		}
		out[id] = c
	}
	return out, rows.Err()
}

// Publish flips a DRAFT report PUBLISHED; the officer's qualitative
// assessment of execution quality rides the same update (RTS 28 table
// 3 — required narrative).
func (s *RTS28Service) Publish(ctx context.Context, reportID,
	actor int64, qualitativeAssessment string) (*RTS28Report, error) {
	if err := s.checkRole(ctx, actor); err != nil {
		return nil, err
	}
	if strings.TrimSpace(qualitativeAssessment) == "" {
		return nil, excerrors.New("INVALID_REQUEST",
			"qualitative_assessment is required — RTS 28 carries a "+
				"narrative on execution quality")
	}
	res, err := s.pool.Exec(ctx, `
		UPDATE rts28_reports
		SET status='PUBLISHED', qualitative_assessment=$2,
		    published_by=$3, published_at=now()
		WHERE id=$1 AND status='DRAFT'`,
		reportID, qualitativeAssessment, actor)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts28 publish", err)
	}
	if res.RowsAffected() == 0 {
		return nil, excerrors.New("INVALID_REQUEST",
			"rts28 report not found or already PUBLISHED")
	}
	if _, err := audit.AppendAuto(ctx, s.pool, "rts28_reports",
		&reportID, "RTS28_PUBLISHED", nil); err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts28 audit", err)
	}
	return s.GetReport(ctx, reportID)
}

// ---------------------------------------------------------------------------
// Reads — admin (all) + public (PUBLISHED only)
// ---------------------------------------------------------------------------

const rts28Cols = `
	id, year, instrument_class, version, status, categories,
	qualitative_assessment, csv, generated_by, created_at, published_by,
	published_at`

func scanRTS28(row interface{ Scan(dest ...any) error }) (*RTS28Report, error) {
	var r RTS28Report
	var qa *string
	err := row.Scan(&r.ID, &r.Year, &r.InstrumentClass, &r.Version,
		&r.Status, &r.Categories, &qa, &r.CSV, &r.GeneratedBy,
		&r.CreatedAt, &r.PublishedBy, &r.PublishedAt)
	if qa != nil {
		r.QualitativeAssessment = *qa
	}
	return &r, err
}

// GetReport reads one row (admin surface).
func (s *RTS28Service) GetReport(ctx context.Context,
	id int64) (*RTS28Report, error) {
	r, err := scanRTS28(s.pool.QueryRow(ctx,
		`SELECT `+rts28Cols+` FROM rts28_reports WHERE id=$1`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "rts28 report not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts28 read", err)
	}
	return r, nil
}

// ListReports returns the register (?status=&year=), newest first.
func (s *RTS28Service) ListReports(ctx context.Context, status string,
	year int, limit int) ([]RTS28Report, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := `SELECT ` + rts28Cols + ` FROM rts28_reports WHERE true`
	args := []any{}
	if status != "" {
		args = append(args, status)
		q += fmt.Sprintf(" AND status=$%d", len(args))
	}
	if year > 0 {
		args = append(args, year)
		q += fmt.Sprintf(" AND year=$%d", len(args))
	}
	q += " ORDER BY year DESC, instrument_class, version DESC" +
		" LIMIT " + fmt.Sprint(limit)
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts28 list", err)
	}
	defer rows.Close()
	var out []RTS28Report
	for rows.Next() {
		r, err := scanRTS28(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "rts28 scan", err)
		}
		out = append(out, *r)
	}
	return out, rows.Err()
}

// ListPublished is the unauthenticated surface — PUBLISHED rows only.
func (s *RTS28Service) ListPublished(ctx context.Context) ([]RTS28Report, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+rts28Cols+` FROM rts28_reports
		 WHERE status='PUBLISHED'
		 ORDER BY year DESC, instrument_class, version DESC`)
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts28 public list", err)
	}
	defer rows.Close()
	var out []RTS28Report
	for rows.Next() {
		r, err := scanRTS28(rows)
		if err != nil {
			return nil, excerrors.Wrap("INTERNAL_ERROR", "rts28 scan", err)
		}
		r.CSV = ""
		out = append(out, *r)
	}
	return out, rows.Err()
}

// GetPublished returns a PUBLISHED row (public download); DRAFT rows
// answer NOT_FOUND.
func (s *RTS28Service) GetPublished(ctx context.Context,
	id int64) (*RTS28Report, error) {
	r, err := scanRTS28(s.pool.QueryRow(ctx,
		`SELECT `+rts28Cols+` FROM rts28_reports
		 WHERE id=$1 AND status='PUBLISHED'`, id))
	if err == pgx.ErrNoRows {
		return nil, excerrors.New("NOT_FOUND", "report not found")
	}
	if err != nil {
		return nil, excerrors.Wrap("INTERNAL_ERROR", "rts28 read", err)
	}
	return r, nil
}
