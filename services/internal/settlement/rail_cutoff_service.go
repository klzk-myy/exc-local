// rail_cutoff_service.go — banking rail cut-off enforcement & automatic
// value-date roll (Phase-24 Task 24.3.20; spec §17.16a, §24 #413).
//
// banking_rail_schedules (migration 107) is the DB-driven source of truth
// for per-rail daily cut-offs; the Phase-11 static RailMatrix remains the
// fallback table for rails without a scheduled row. Evaluation is
// TIMEZONE-AWARE: the cut-off is a wall-clock time in the rail's IANA
// timezone (Fedwire 18:30 America/New_York, TARGET2 18:00 Europe/Berlin,
// CHAPS 17:00 Europe/London, CLS PvP pay-in 06:30 Europe/Berlin).
//
// Semantics (spec §17.16a):
//   - an instruction submitted past its rail's cut-off automatically
//     rolls value_date to the next business day (holiday calendar, Task
//     3.3.8) and the instruction is flagged QUEUED_FOR_NEXT_CYCLE;
//   - a client request demanding same-day value past the cut-off is
//     rejected RAIL_CUTOFF_EXCEEDED (HTTP 422 — already registered).
//
// Fail-closed (§2.7): an unscheduled (rail, currency) pair is an error —
// instructions never ride an unscheduled rail.
package settlement

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	excerrors "exchange/pkg/errors"
)

// RailSchedule is one banking_rail_schedules row with the parsed
// timezone/cut-off.
type RailSchedule struct {
	ID            int64  `json:"id"`
	RailName      string `json:"rail_name"`         // FEDWIRE|TARGET2|CHAPS|CLS_PVP|...
	Currency      string `json:"currency"`          // '*' = wildcard for all currencies
	Timezone      string `json:"timezone"`          // IANA name
	CutoffLocal   string `json:"daily_cutoff_time"` // "HH:MM" wall-clock in Timezone
	CycleDays     int    `json:"settlement_cycle_days"`
	loc           *time.Location
	cutoffMinutes int
}

// RailScheduleRow is the raw row read shape.
type RailScheduleRow struct {
	ID          int64
	RailName    string
	Currency    string
	Timezone    string
	CutoffLocal string
	CycleDays   int
}

// RailScheduleStore loads banking_rail_schedules.
type RailScheduleStore interface {
	RailSchedules(ctx context.Context) ([]RailScheduleRow, error)
}

// CutoffDecision reports one evaluation.
type CutoffDecision struct {
	Rail         string    `json:"rail"`
	Currency     string    `json:"currency"`
	EvaluatedAt  time.Time `json:"evaluated_at"`
	CutoffLocal  string    `json:"cutoff_local"`
	Timezone     string    `json:"timezone"`
	CutoffPassed bool      `json:"cutoff_passed"`
	// ValueDate is the effective value date: today when before the
	// cut-off, else the next business day per the holiday calendar
	// (QUEUED_FOR_NEXT_CYCLE semantics).
	ValueDate          time.Time `json:"value_date"`
	QueuedForNextCycle bool      `json:"queued_for_next_cycle"`
}

// RailCutoffService evaluates banking_rail_schedules cut-offs.
type RailCutoffService struct {
	store RailScheduleStore
	cal   *HolidayCalendar
	clock func() time.Time
	sched atomic.Pointer[[]RailSchedule] // refreshed snapshot
}

// NewRailCutoffService wires the service; store + calendar are required
// (the value-date roll evaluates the holiday calendar — never a naive
// +1d). Call Reload once before serving.
func NewRailCutoffService(store RailScheduleStore, cal *HolidayCalendar, clock func() time.Time) (*RailCutoffService, error) {
	if store == nil {
		return nil, fmt.Errorf("rail cutoff: nil store")
	}
	if cal == nil {
		return nil, fmt.Errorf("rail cutoff: nil holiday calendar")
	}
	if clock == nil {
		clock = time.Now
	}
	return &RailCutoffService{store: store, cal: cal, clock: clock}, nil
}

// Reload re-reads banking_rail_schedules and atomically swaps the live
// set. A malformed row fails the whole load (partial schedules must never
// gate money movement — spec §2.7); on error the previous snapshot stays.
func (s *RailCutoffService) Reload(ctx context.Context) error {
	rows, err := s.store.RailSchedules(ctx)
	if err != nil {
		return fmt.Errorf("rail cutoff: load schedules: %w", err)
	}
	if len(rows) == 0 {
		return fmt.Errorf("rail cutoff: banking_rail_schedules is empty")
	}
	parsed := make([]RailSchedule, 0, len(rows))
	for _, r := range rows {
		sch, err := parseRailSchedule(r)
		if err != nil {
			return err
		}
		parsed = append(parsed, sch)
	}
	s.sched.Store(&parsed)
	return nil
}

// Schedules returns the live schedule snapshot (handler surface).
func (s *RailCutoffService) Schedules() []RailSchedule {
	p := s.sched.Load()
	if p == nil {
		return nil
	}
	out := make([]RailSchedule, len(*p))
	copy(out, *p)
	return out
}

func parseRailSchedule(r RailScheduleRow) (RailSchedule, error) {
	var sch RailSchedule
	sch.ID = r.ID
	sch.RailName = strings.ToUpper(strings.TrimSpace(r.RailName))
	sch.Currency = strings.ToUpper(strings.TrimSpace(r.Currency))
	if sch.RailName == "" {
		return sch, fmt.Errorf("rail cutoff: schedule %d has empty rail_name", r.ID)
	}
	if sch.Currency == "" {
		return sch, fmt.Errorf("rail cutoff: schedule %d has empty currency", r.ID)
	}
	if sch.Currency != "*" && !currencyRe.MatchString(sch.Currency) {
		return sch, fmt.Errorf("rail cutoff: schedule %d bad currency %q", r.ID, r.Currency)
	}
	loc, err := time.LoadLocation(r.Timezone)
	if err != nil {
		return sch, fmt.Errorf("rail cutoff: schedule %d bad timezone %q: %w", r.ID, r.Timezone, err)
	}
	var hh, mm int
	if n, _ := fmt.Sscanf(strings.TrimSpace(r.CutoffLocal), "%d:%d", &hh, &mm); n != 2 ||
		hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return sch, fmt.Errorf("rail cutoff: schedule %d bad cutoff %q (want HH:MM)", r.ID, r.CutoffLocal)
	}
	sch.Timezone = r.Timezone
	sch.CutoffLocal = fmt.Sprintf("%02d:%02d", hh, mm)
	sch.CycleDays = r.CycleDays
	sch.loc = loc
	sch.cutoffMinutes = hh*60 + mm
	return sch, nil
}

// scheduleFor resolves the schedule for (rail, currency): exact currency
// row first, then the '*' wildcard. found=false when unscheduled.
func (s *RailCutoffService) scheduleFor(rail, currency string) (RailSchedule, bool) {
	rail = strings.ToUpper(strings.TrimSpace(rail))
	currency = strings.ToUpper(strings.TrimSpace(currency))
	p := s.sched.Load()
	if p == nil {
		return RailSchedule{}, false
	}
	var wild *RailSchedule
	for i := range *p {
		sch := (*p)[i]
		if sch.RailName != rail {
			continue
		}
		if sch.Currency == currency {
			return sch, true
		}
		if sch.Currency == "*" && wild == nil {
			cp := sch
			wild = &cp
		}
	}
	if wild != nil {
		return *wild, true
	}
	return RailSchedule{}, false
}

// Evaluate computes the cut-off decision for (rail, currency) at `at`
// (clock() when zero). Past cut-off → ValueDate rolls to the next
// business day in the currency's center (holiday calendar) and
// QueuedForNextCycle is set.
func (s *RailCutoffService) Evaluate(rail, currency string, at time.Time) (CutoffDecision, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if !currencyRe.MatchString(currency) {
		return CutoffDecision{}, excerrors.New("INVALID_REQUEST",
			fmt.Sprintf("rail cutoff: bad currency %q", currency))
	}
	if at.IsZero() {
		at = s.clock()
	}
	sch, ok := s.scheduleFor(rail, currency)
	if !ok {
		return CutoffDecision{}, excerrors.New("BANKING_RAIL_UNAVAILABLE", fmt.Sprintf(
			"rail cutoff: no schedule for rail %q currency %s (fail-closed)", rail, currency))
	}
	local := at.In(sch.loc)
	localDay := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
	cutoffAt := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, sch.loc).
		Add(time.Duration(sch.cutoffMinutes) * time.Minute)
	// The window is closed when the local day is not a settlement
	// business day at all (weekend/holiday — no same-day value exists)
	// or the daily cut-off has passed on a business day.
	passed := !s.cal.IsMutualBusinessDay(localDay, currency) || !local.Before(cutoffAt)

	vd := localDay
	queued := false
	if passed {
		// Roll to the next business day — settlement_cycle_days=0 means
		// the rail settles same-day when inside the window, so a missed
		// window slides the whole instruction one business day.
		vd = s.cal.MutualBusinessDayOnOrAfter(localDay.AddDate(0, 0, 1+sch.CycleDays), currency)
		queued = true
	} else if sch.CycleDays > 0 {
		vd = s.cal.MutualBusinessDayOnOrAfter(localDay.AddDate(0, 0, sch.CycleDays), currency)
	}
	return CutoffDecision{
		Rail:               sch.RailName,
		Currency:           currency,
		EvaluatedAt:        at.UTC(),
		CutoffLocal:        sch.CutoffLocal,
		Timezone:           sch.Timezone,
		CutoffPassed:       passed,
		ValueDate:          vd,
		QueuedForNextCycle: queued,
	}, nil
}

// settlementRailPreference orders rails for currency-scoped evaluation of
// settlement_instructions (bank-to-bank nostro moves): the wholesale
// settlement rails first, then alphabetical; wildcard rows never resolve
// a currency on their own.
var settlementRailPreference = []string{"FEDWIRE", "TARGET2", "CHAPS"}

// EvaluateForCurrency resolves the currency's scheduled settlement rail
// (settlementRailPreference order) and evaluates its cut-off. No
// scheduled rail for the currency → error (fail-closed).
func (s *RailCutoffService) EvaluateForCurrency(currency string, at time.Time) (CutoffDecision, error) {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	for _, rail := range settlementRailPreference {
		if _, ok := s.scheduleFor(rail, currency); ok {
			return s.Evaluate(rail, currency, at)
		}
	}
	p := s.sched.Load()
	if p != nil {
		for i := range *p {
			if (*p)[i].Currency == currency {
				return s.Evaluate((*p)[i].RailName, currency, at)
			}
		}
	}
	return CutoffDecision{}, excerrors.New("BANKING_RAIL_UNAVAILABLE",
		fmt.Sprintf("rail cutoff: no scheduled settlement rail for %s (fail-closed)", currency))
}

// CutoffPassed implements RailCutoffChecker (netting dispatch gate): the
// first scheduled settlement rail for the currency answers.
func (s *RailCutoffService) CutoffPassed(ctx context.Context, currency string, at time.Time) (bool, string, error) {
	_ = ctx
	p := s.sched.Load()
	if p == nil {
		return false, "", excerrors.New("BANKING_RAIL_UNAVAILABLE",
			"rail cutoff: schedules not loaded")
	}
	currency = strings.ToUpper(currency)
	for i := range *p {
		if (*p)[i].Currency == currency {
			d, err := s.Evaluate((*p)[i].RailName, currency, at)
			if err != nil {
				return false, "", err
			}
			return d.CutoffPassed, d.Rail, nil
		}
	}
	// wildcard rows (e.g. CLS_PVP '*') apply too.
	for i := range *p {
		if (*p)[i].Currency == "*" {
			d, err := s.Evaluate((*p)[i].RailName, currency, at)
			if err != nil {
				return false, "", err
			}
			return d.CutoffPassed, d.Rail, nil
		}
	}
	return false, "", excerrors.New("BANKING_RAIL_UNAVAILABLE",
		fmt.Sprintf("rail cutoff: no scheduled rail for %s", currency))
}

// EnforceSameDay rejects a same-day-value request whose rail cut-off has
// passed — RAIL_CUTOFF_EXCEEDED (422). Passes clean when inside the
// window. An empty rail resolves the currency's scheduled settlement
// rail (EvaluateForCurrency).
func (s *RailCutoffService) EnforceSameDay(rail, currency string, at time.Time) error {
	var d CutoffDecision
	var err error
	if strings.TrimSpace(rail) == "" {
		d, err = s.EvaluateForCurrency(currency, at)
	} else {
		d, err = s.Evaluate(rail, currency, at)
	}
	if err != nil {
		return err
	}
	if d.CutoffPassed {
		return excerrors.New("RAIL_CUTOFF_EXCEEDED", fmt.Sprintf(
			"rail %s cut-off %s %s passed for %s — same-day value date unavailable",
			d.Rail, d.CutoffLocal, d.Timezone, d.Currency))
	}
	return nil
}

// ---------------------------------------------------------------------------
// PgxRailScheduleStore
// ---------------------------------------------------------------------------

// PgxRailScheduleStore loads banking_rail_schedules.
type PgxRailScheduleStore struct{ Pool *pgxpool.Pool }

// NewPgxRailScheduleStore wires the store.
func NewPgxRailScheduleStore(pool *pgxpool.Pool) *PgxRailScheduleStore {
	return &PgxRailScheduleStore{Pool: pool}
}

// RailSchedules reads every schedule row.
func (s *PgxRailScheduleStore) RailSchedules(ctx context.Context) ([]RailScheduleRow, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, rail_name, currency, timezone,
		       to_char(daily_cutoff_time, 'HH24:MI'), settlement_cycle_days
		  FROM banking_rail_schedules ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RailScheduleRow
	for rows.Next() {
		var r RailScheduleRow
		if err := rows.Scan(&r.ID, &r.RailName, &r.Currency, &r.Timezone,
			&r.CutoffLocal, &r.CycleDays); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
