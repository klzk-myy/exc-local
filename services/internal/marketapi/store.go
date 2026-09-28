// Storage seams for the market-data REST surface. PgStore implements all
// of them over pgxpool; tests inject fakes. Every method returns a bare
// error to the caller — the fail-closed contract (spec §2.7) means a store
// failure surfaces as SERVICE_DEGRADED, never as an empty dataset that
// could be mistaken for a quiet market.
package marketapi

import (
	"context"
	"time"
)

// Store is the read side of the market-data surface.
type Store interface {
	// ListInstruments returns every instrument row (all statuses — the
	// endpoint publishes lifecycle state, clients filter). Ordered by
	// symbol for a stable venue document.
	ListInstruments(ctx context.Context) ([]Instrument, error)
	// InstrumentBySymbol returns (nil, nil) for an unknown symbol.
	InstrumentBySymbol(ctx context.Context, symbol string) (*Instrument, error)
	// RecentTrades returns up to limit public tape rows, newest first.
	RecentTrades(ctx context.Context, symbol string, limit int) ([]Trade, error)
	// Ticker24h aggregates the trailing 24h window ending at now. Returns
	// (nil, nil) for an unknown symbol; a known symbol with no trades
	// returns a zeroed Ticker (TradeCount 0, nil price fields).
	Ticker24h(ctx context.Context, symbol string, now time.Time) (*Ticker, error)
	// Klines reads the pre-materialized fx_klines aggregates (spec §10.3
	// contract — never computed from the trades table in-request). Rows
	// come back oldest→newest, bounded to [from,to) and limit. to==zero
	// means "up to now".
	Klines(ctx context.Context, symbol, timeframe string,
		from, to time.Time, limit int) ([]Kline, error)
}

// BookSource is the L2 depth seam. PgBookSource (pg.go) derives the
// persisted resting-order book; a lower-latency engine-fed source can
// replace it at wiring time without touching the handler.
type BookSource interface {
	// Snapshot returns the top-N aggregated book for symbol — bids sorted
	// best-first. (nil, nil) = unknown symbol.
	Snapshot(ctx context.Context, symbol string, depth int) (*BookSnapshot, error)
}

// AnnouncementFilter scopes the public/admin announcement listing.
type AnnouncementFilter struct {
	Category   string // "" = all
	ActiveOnly bool   // PUBLISHED and inside publish/expiry window
	IncludeAll bool   // admin view: no status/window filtering
	Limit      int
}

// AnnouncementStore covers the Task 5.3.14 announcement surface.
type AnnouncementStore interface {
	ListAnnouncements(ctx context.Context, f AnnouncementFilter) ([]Announcement, error)
	Announcement(ctx context.Context, id int64) (*Announcement, error)
	CreateAnnouncement(ctx context.Context, a Announcement) (*Announcement, error)
	UpdateAnnouncement(ctx context.Context, a Announcement) (*Announcement, error)
	// RetractAnnouncement sets status=RETRACTED — announcements are
	// disclosure records, so deletion is a state change, never row
	// removal. Returns false when the id is absent.
	RetractAnnouncement(ctx context.Context, id int64, by string) (bool, error)
}

// MaintenanceStore covers the Task 5.3.14 maintenance calendar.
type MaintenanceStore interface {
	// UpcomingMaintenance returns SCHEDULED/IN_PROGRESS windows whose
	// ends_at is still ahead of now, ordered by starts_at.
	UpcomingMaintenance(ctx context.Context, now time.Time) ([]MaintenanceWindow, error)
	// ListMaintenance returns every window regardless of status (admin).
	ListMaintenance(ctx context.Context, limit int) ([]MaintenanceWindow, error)
	CreateMaintenance(ctx context.Context, m MaintenanceWindow) (*MaintenanceWindow, error)
	UpdateMaintenance(ctx context.Context, m MaintenanceWindow) (*MaintenanceWindow, error)
	// CancelMaintenance flips status to CANCELLED — windows stay on
	// record. Returns false when the id is absent.
	CancelMaintenance(ctx context.Context, id int64, by string) (bool, error)
}
