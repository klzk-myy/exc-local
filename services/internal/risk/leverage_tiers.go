// Phase-19 Task 19.3.17 — tiered leverage by notional exposure
// (spec §13.6f, §24 #231).
//
// leverage_tiers (migration 234) schedules a max_leverage per
// (instrument_group × regulatory_regime) notional band: effective
// leverage steps DOWN as the account's gross notional on the instrument
// crosses each boundary. The LeverageService resolver consumes
// TierCap as one input into the most-restrictive-wins minimum; the
// banded-margin helper (BandMargin) is the §13.6f aggregate margin the
// pre-trade margin check consumes — margin is band-SUMMED, not applied
// at a single blended rate.
//
// Admin CRUD is dual-controlled (Task 19.3.17/§8.2): mutations ride the
// admin.OpLeverageTierChange queue; the executor calls UpsertTier /
// DeleteTier inside the approval transaction, then the onExecuted hook
// flushes the leverage:eff:* cache so stale bands never resolve.
package risk

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"exchange/pkg/decimal"
	excerrors "exchange/pkg/errors"
)

// LeverageTier is one notional band row (migration 234). To == nil
// means the open-ended top band.
type LeverageTier struct {
	ID              int64            `json:"id"`
	InstrumentGroup string           `json:"instrument_group"`  // MAJOR | MINOR | EXOTIC
	Regime          string           `json:"regulatory_regime"` // ESMA | CFTC | PROFESSIONAL
	NotionalFrom    decimal.Decimal  `json:"notional_from"`
	NotionalTo      *decimal.Decimal `json:"notional_to,omitempty"`
	MaxLeverage     int              `json:"max_leverage"`
	UpdatedAt       time.Time        `json:"updated_at"`
}

// ValidTierGroup / ValidTierRegime gate the enum axes (same CHECK sets
// as migration 234).
func ValidTierGroup(g string) bool {
	return g == GroupMajor || g == GroupMinor || g == GroupExotic
}

// ValidTierRegime gates the regulatory_regime axis.
func ValidTierRegime(r string) bool {
	return r == RegimeESMA || r == RegimeCFTC || r == RegimeProfessional
}

// TierStore is the persistence seam (PgTierStore in production).
type TierStore interface {
	ListTiers(ctx context.Context, group, regime string) ([]LeverageTier, error)
	// TierAt resolves the band cap for (group, regime, notional);
	// nil = no configured band covers the notional.
	TierAt(ctx context.Context, group, regime string, notional decimal.Decimal) (*LeverageTier, error)
	UpsertTier(ctx context.Context, t LeverageTier, updatedBy int64) (*LeverageTier, error)
	DeleteTier(ctx context.Context, id int64) error
}

// TierAt selects the band covering notional from a sorted-by-
// notional_from band set (contiguous bands per migration seeds; a gap
// resolves nothing — callers treat nil as "no tier cap").
func TierAt(bands []LeverageTier, notional decimal.Decimal) *LeverageTier {
	for i := range bands {
		b := &bands[i]
		if notional.LessThan(b.NotionalFrom) {
			continue
		}
		if b.NotionalTo != nil && notional.GreaterThanOrEqual(*b.NotionalTo) {
			continue
		}
		return b
	}
	return nil
}

// SortTiers orders bands by notional_from (the tile order the schedule
// requires).
func SortTiers(bands []LeverageTier) {
	sort.Slice(bands, func(i, j int) bool {
		return bands[i].NotionalFrom.LessThan(bands[j].NotionalFrom)
	})
}

// BandMargin is the §13.6f band-summed margin for gross notional:
// margin = Σ_band slice_notional / band_leverage. Notional below the
// first band start contributes nothing (unreachable under contiguous
// seeds — the 0..1M band covers it); notional above the top band edge
// uses the open-ended band's leverage when notional_to is NULL.
// A call with no covering top band uses the LAST band's leverage for
// the overflow slice (strict-by-construction fallback, never zero
// margin on the excess).
func BandMargin(bands []LeverageTier, notional decimal.Decimal) (decimal.Decimal, error) {
	if !notional.IsPositive() {
		return decimal.Zero, nil
	}
	if len(bands) == 0 {
		return decimal.Zero, excerrors.New(CodeRiskLimitsInternal,
			"leverage bands: no bands configured")
	}
	SortTiers(bands)
	margin := decimal.Zero
	remaining := notional
	for i := range bands {
		b := &bands[i]
		if remaining.LessThanOrEqual(decimal.Zero) {
			break
		}
		width := remaining
		if b.NotionalTo != nil {
			span := b.NotionalTo.Sub(b.NotionalFrom)
			if span.LessThanOrEqual(decimal.Zero) {
				return decimal.Zero, excerrors.New(CodeRiskLimitsInternal,
					fmt.Sprintf("leverage bands: band %d has non-positive width", b.ID))
			}
			if remaining.GreaterThan(span) {
				width = span
			}
		}
		if b.MaxLeverage <= 0 {
			return decimal.Zero, excerrors.New(CodeRiskLimitsInternal,
				fmt.Sprintf("leverage bands: band %d has invalid leverage %d", b.ID, b.MaxLeverage))
		}
		margin = margin.Add(width.Div(decimal.NewFromInt(int64(b.MaxLeverage))))
		remaining = remaining.Sub(width)
	}
	return margin, nil
}

// ---------------------------------------------------------------------------
// PgTierStore
// ---------------------------------------------------------------------------

// PgTierStore implements TierStore over pgx.
type PgTierStore struct {
	pool *pgxpool.Pool
}

// NewPgTierStore wraps pool.
func NewPgTierStore(pool *pgxpool.Pool) *PgTierStore {
	return &PgTierStore{pool: pool}
}

const tierCols = `id, instrument_group, regulatory_regime,
	notional_from::text, notional_to::text, max_leverage, updated_at`

func scanTier(scan func(...any) error) (*LeverageTier, error) {
	var (
		t        LeverageTier
		from, to *string
	)
	if err := scan(&t.ID, &t.InstrumentGroup, &t.Regime, &from, &to,
		&t.MaxLeverage, &t.UpdatedAt); err != nil {
		return nil, err
	}
	f, err := decimal.NewFromString(*from)
	if err != nil {
		return nil, fmt.Errorf("parse notional_from %q: %w", *from, err)
	}
	t.NotionalFrom = f
	if to != nil {
		td, err := decimal.NewFromString(*to)
		if err != nil {
			return nil, fmt.Errorf("parse notional_to %q: %w", *to, err)
		}
		t.NotionalTo = &td
	}
	return &t, nil
}

// ListTiers returns bands filtered by the non-empty axes, ordered.
func (s *PgTierStore) ListTiers(ctx context.Context, group, regime string) ([]LeverageTier, error) {
	q := `SELECT ` + tierCols + ` FROM leverage_tiers`
	args := []any{}
	w := ""
	if group != "" {
		args = append(args, group)
		w += fmt.Sprintf(" AND instrument_group = $%d", len(args))
	}
	if regime != "" {
		args = append(args, regime)
		w += fmt.Sprintf(" AND regulatory_regime = $%d", len(args))
	}
	if w != "" {
		q += " WHERE" + w[4:]
	}
	q += ` ORDER BY instrument_group, regulatory_regime, notional_from`
	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LeverageTier
	for rows.Next() {
		t, err := scanTier(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *PgTierStore) TierAt(ctx context.Context, group, regime string, notional decimal.Decimal) (*LeverageTier, error) {
	t, err := scanTier(s.pool.QueryRow(ctx, `
		SELECT `+tierCols+` FROM leverage_tiers
		 WHERE instrument_group = $1 AND regulatory_regime = $2
		   AND notional_from <= $3::numeric
		   AND (notional_to IS NULL OR notional_to > $3::numeric)
		 ORDER BY notional_from DESC LIMIT 1`,
		group, regime, notional.String()).Scan)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return t, nil
}

// UpsertTier inserts or rewrites the (group, regime, notional_from)
// cell — the dual-control executor's write path.
func (s *PgTierStore) UpsertTier(ctx context.Context, t LeverageTier, updatedBy int64) (*LeverageTier, error) {
	if !ValidTierGroup(t.InstrumentGroup) {
		return nil, excerrors.New(CodeLeverageInvalid,
			fmt.Sprintf("leverage tier: invalid instrument_group %q", t.InstrumentGroup))
	}
	if !ValidTierRegime(t.Regime) {
		return nil, excerrors.New(CodeLeverageInvalid,
			fmt.Sprintf("leverage tier: invalid regulatory_regime %q", t.Regime))
	}
	if t.MaxLeverage <= 0 {
		return nil, excerrors.New(CodeLeverageInvalid, "leverage tier: max_leverage must be > 0")
	}
	if !t.NotionalFrom.IsZero() && !t.NotionalFrom.IsPositive() {
		return nil, excerrors.New(CodeLeverageInvalid, "leverage tier: notional_from must be >= 0")
	}
	if t.NotionalTo != nil && !t.NotionalTo.GreaterThan(t.NotionalFrom) {
		return nil, excerrors.New(CodeLeverageInvalid,
			"leverage tier: notional_to must exceed notional_from")
	}
	var to *string
	if t.NotionalTo != nil {
		s := t.NotionalTo.String()
		to = &s
	}
	out, err := scanTier(s.pool.QueryRow(ctx, `
		INSERT INTO leverage_tiers
		    (instrument_group, regulatory_regime, notional_from, notional_to, max_leverage)
		VALUES ($1,$2,$3::numeric,$4::numeric,$5)
		ON CONFLICT (instrument_group, regulatory_regime, notional_from)
		DO UPDATE SET notional_to = EXCLUDED.notional_to,
		              max_leverage = EXCLUDED.max_leverage,
		              updated_at = now()
		RETURNING `+tierCols,
		t.InstrumentGroup, t.Regime, t.NotionalFrom.String(), to, t.MaxLeverage).Scan)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteTier removes one band row by id.
func (s *PgTierStore) DeleteTier(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM leverage_tiers WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return excerrors.New(CodeLeverageNotFound,
			fmt.Sprintf("leverage tier %d not found", id))
	}
	return nil
}
