// Package marketapi is the read/query layer behind the Phase-05 market-data
// and venue-information REST surface:
//
//	Task 5.3.5  — market data REST endpoints (book/trades/ticker/klines,
//	              instruments) with the §10.3 time-based caches
//	Task 5.3.14 — announcements & maintenance calendar storage/reads
//	Task 5.3.35 — structured instrument filter objects
//	Task 5.3.43 — server-time endpoint backing (clock source lives in
//	              internal/timesync; this package only shapes the payload)
//	Task 5.3.44 — unified venue-info document (exchange-info) with ETag
//
// Data provenance rules (spec §2.7 fail-closed; §10.3 remediation #38):
//   - Instruments, trades and the persisted book view read PostgreSQL
//     (migrations 001/005/006/050/170) — real stored state, never synthetic.
//   - Klines read ONLY the pre-materialized fx_klines aggregate table
//     (migration 173); runtime candle aggregation over the trades table is
//     prohibited by spec §10.3. The Phase-06 Task 6.3.8 candle engine is the
//     writer; until it lands the endpoint returns an empty data set.
//   - Nothing here fabricates prices: empty/absent data returns honest
//     empty payloads or the registered §23 error, never invented levels.
//   - All numeric payloads are DECIMAL strings (fixed-point, spec §5.3
//     invariant 1); timestamps are epoch millis.
package marketapi

import "time"

// Canonical trading-hours descriptor (spec §6.7/§7.1, AGENTS.md canonical
// values): FX runs 24/5 — Sydney open Sunday 21:00 UTC to New York close
// Friday 22:00 UTC. Per-instrument session overrides arrive with the
// instruments_reference.trading_hours column (migration 087, Phase-15 Task
// 15.3.11); until then every instrument advertises the venue schedule.
type TradingHours struct {
	Type           string `json:"type"`             // always "24/5"
	WeeklyOpenUTC  string `json:"weekly_open_utc"`  // "Sunday 21:00"
	WeeklyCloseUTC string `json:"weekly_close_utc"` // "Friday 22:00"
	DailyBreakUTC  string `json:"daily_break_utc"`  // rollover window marker
}

// VenueTradingHours is the canonical 24/5 descriptor.
func VenueTradingHours() TradingHours {
	return TradingHours{
		Type:           "24/5",
		WeeklyOpenUTC:  "Sunday 21:00",
		WeeklyCloseUTC: "Friday 22:00",
		DailyBreakUTC:  "22:00",
	}
}

// Instrument is the public reference-data projection of one instruments row
// (migrations 001 + 050 + 170). Decimal columns stay text to preserve the
// fixed-point contract end to end.
type Instrument struct {
	ID               int64   `json:"-"`
	Symbol           string  `json:"symbol"`
	BaseCurrency     string  `json:"base_currency"`
	QuoteCurrency    string  `json:"quote_currency"`
	InstrumentType   string  `json:"instrument_type"` // SPOT|FORWARD|SWAP|NDF|OPTION
	Status           string  `json:"status"`          // instrument_status_enum (§7.1)
	TickSize         string  `json:"tick_size"`
	LotSize          string  `json:"lot_size"`
	MinOrderQty      string  `json:"min_order_qty"`
	MaxOrderQty      string  `json:"max_order_qty"`
	MinNotional      string  `json:"min_notional"`
	MinPrice         *string `json:"min_price"` // nil = unbounded
	MaxPrice         *string `json:"max_price"` // nil = unbounded
	PriceBandPctUp   string  `json:"price_band_pct_up"`
	PriceBandPctDown string  `json:"price_band_pct_down"`
	MaxSpreadPips    *string `json:"max_spread_pips"` // nil = protection not configured
	MaxOpenOrders    *int64  `json:"max_open_orders"` // nil = no instrument-level cap
	MaxAlgoOrders    *int64  `json:"max_algo_orders"`
	MaxLeverage      int64   `json:"max_leverage"`
	SettlementCycle  int     `json:"settlement_cycle"` // 0 same-day, 1 T+1, 2 T+2
	// Phase-15 Task 15.3.11 / §7.4 reference columns (migration 087).
	ContractSize  string    `json:"contract_size"`
	DecimalPlaces int       `json:"decimal_places"`
	PipSize       string    `json:"pip_size"`
	UpdatedAt     time.Time `json:"-"`
}

// SettlementLabel renders the §6.3 settlement-cycle label.
func (i Instrument) SettlementLabel() string {
	switch i.SettlementCycle {
	case 0:
		return "SAME_DAY"
	case 1:
		return "T+1"
	case 2:
		return "T+2"
	default:
		return "T+" + itoa(i.SettlementCycle)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// Filter is one Task 5.3.35 structured filter object. Every known filter
// type is emitted for every instrument; a nil field means the bound is not
// configured (unbounded), never silently dropped.
type Filter struct {
	FilterType string `json:"filter_type"`
	// PRICE_FILTER
	MinPrice *string `json:"min_price"`
	MaxPrice *string `json:"max_price"`
	TickSize *string `json:"tick_size"`
	// LOT_SIZE
	MinQty   *string `json:"min_qty"`
	MaxQty   *string `json:"max_qty"`
	StepSize *string `json:"step_size"`
	// MIN_NOTIONAL
	MinNotional *string `json:"min_notional"`
	// PRICE_BAND
	PriceBandPctUp   *string `json:"price_band_pct_up"`
	PriceBandPctDown *string `json:"price_band_pct_down"`
	// MAX_ORDERS
	MaxOpenOrders *int64 `json:"max_open_orders"`
	MaxAlgoOrders *int64 `json:"max_algo_orders"`
	// SPREAD_PROTECTION
	MaxSpreadPips *string `json:"max_spread_pips"`
}

// Filters returns the canonical six-object filter set for the instrument
// (Task 5.3.35 / §24 #259): PRICE_FILTER, LOT_SIZE, MIN_NOTIONAL,
// PRICE_BAND, MAX_ORDERS, SPREAD_PROTECTION — in that order.
func (i Instrument) Filters() []Filter {
	strp := func(s string) *string { return &s }
	return []Filter{
		{FilterType: "PRICE_FILTER",
			MinPrice: i.MinPrice, MaxPrice: i.MaxPrice, TickSize: strp(i.TickSize)},
		{FilterType: "LOT_SIZE",
			MinQty: strp(i.MinOrderQty), MaxQty: strp(i.MaxOrderQty), StepSize: strp(i.LotSize)},
		{FilterType: "MIN_NOTIONAL",
			MinNotional: strp(i.MinNotional)},
		{FilterType: "PRICE_BAND",
			PriceBandPctUp: strp(i.PriceBandPctUp), PriceBandPctDown: strp(i.PriceBandPctDown)},
		{FilterType: "MAX_ORDERS",
			MaxOpenOrders: i.MaxOpenOrders, MaxAlgoOrders: i.MaxAlgoOrders},
		{FilterType: "SPREAD_PROTECTION",
			MaxSpreadPips: i.MaxSpreadPips},
	}
}

// OrderTypes lists the order types a symbol permits. SPOT instruments take
// the full §6.1/§6.2 set; forwards/swaps/NDFs restrict to the two-sided
// price-contribution types (conditional triggers evaluate against spot
// marks and stay available); options keep the directional set — barrier /
// binary structures and exercise semantics are order-parameter level, not
// a distinct order_type (spec §5.1 note).
func (i Instrument) OrderTypes() []string {
	switch i.InstrumentType {
	case "FORWARD", "SWAP", "NDF":
		return []string{"LIMIT", "MARKET", "STOP", "STOP_LIMIT"}
	case "OPTION":
		return []string{"LIMIT", "MARKET"}
	default: // SPOT and anything unrecognized: the standard + advanced set
		return []string{
			"LIMIT", "MARKET", "STOP", "STOP_LIMIT", "ICEBERG",
			"TWAP", "VWAP", "TRAILING_STOP", "BRACKET", "OCO",
			"SPREAD", "SCALE", "PEG", "FIXING", "MOO", "MOC",
		}
	}
}

// MarketOrdersAllowed reports the §7.1 status gate the venue-info document
// discloses: only ACTIVE instruments take market orders — RESTRICTED takes
// limit orders only; DRAFT (pre-activation), CANCEL_ONLY, SUSPENDED,
// HALTED and DELISTED take no new orders at all.
func (i Instrument) MarketOrdersAllowed() bool {
	return i.Status == "ACTIVE"
}

// NewOrdersAllowed reports whether the lifecycle state accepts order entry
// at all (spec §7.1): ACTIVE yes, RESTRICTED yes (limit-only), every other
// state no.
func (i Instrument) NewOrdersAllowed() bool {
	return i.Status == "ACTIVE" || i.Status == "RESTRICTED"
}

// BookLevel is one aggregated L2 price level: [price, remaining_qty] as
// fixed-point strings.
type BookLevel struct {
	Price    string `json:"price"`
	Quantity string `json:"quantity"`
}

// BookSnapshot is the L2 depth payload: top-N bid/ask levels plus the
// persisted book sequence cursor (orders.book_seq — the engine-assigned
// sequencing of the stored book state).
type BookSnapshot struct {
	Symbol      string      `json:"symbol"`
	Seq         int64       `json:"seq"`
	Depth       int         `json:"depth"`
	Bids        []BookLevel `json:"bids"`
	Asks        []BookLevel `json:"asks"`
	UpdatedAtMs int64       `json:"updated_at_ms"`
}

// Trade is one public tape row — the trades-table projection carrying no
// account or order identity (§22.5: participant identities never publish).
type Trade struct {
	ID       int64  `json:"id"`
	Symbol   string `json:"symbol"`
	Price    string `json:"price"`
	Quantity string `json:"quantity"`
	Seq      int64  `json:"seq"`
	TimeMs   int64  `json:"time_ms"`
}

// Ticker is the rolling 24h aggregate for one symbol. Price fields are nil
// when the window holds no trades — an empty tape is reported as such,
// never back-filled with synthetic prices.
type Ticker struct {
	Symbol         string  `json:"symbol"`
	Window         string  `json:"window"` // "24h"
	Open           *string `json:"open"`
	High           *string `json:"high"`
	Low            *string `json:"low"`
	Last           *string `json:"last"`
	PriceChange    *string `json:"price_change"`
	PriceChangePct *string `json:"price_change_pct"`
	Volume         string  `json:"volume"`
	QuoteVolume    string  `json:"quote_volume"`
	TradeCount     int64   `json:"trade_count"`
	FirstTradeMs   *int64  `json:"first_trade_ms"`
	LastTradeMs    *int64  `json:"last_trade_ms"`
	ServerTimeMs   int64   `json:"server_time_ms"`
}

// Kline is one materialized OHLCV bar from fx_klines (migration 173).
type Kline struct {
	OpenTimeMs  int64  `json:"open_time_ms"`
	Open        string `json:"open"`
	High        string `json:"high"`
	Low         string `json:"low"`
	Close       string `json:"close"`
	Volume      string `json:"volume"`
	QuoteVolume string `json:"quote_volume"`
	TradeCount  int64  `json:"trade_count"`
	Closed      bool   `json:"closed"`
}

// KlineIntervals is the canonical 13-interval set (Phase-06 Tasks
// 6.3.8/6.3.14). 1s bars are produced memory-only upstream but the read
// contract accepts the label.
var KlineIntervals = map[string]bool{
	"1s": true, "1m": true, "5m": true, "15m": true, "30m": true,
	"1h": true, "2h": true, "4h": true, "6h": true, "8h": true,
	"1D": true, "1W": true, "1M": true,
}

// Announcement is one status-page record (migration 171).
type Announcement struct {
	ID        int64      `json:"id"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Category  string     `json:"category"` // GENERAL|MAINTENANCE|INCIDENT|PRODUCT|PROMOTION
	Status    string     `json:"status"`   // DRAFT|PUBLISHED|EXPIRED|RETRACTED
	PublishAt time.Time  `json:"publish_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedBy string     `json:"created_by,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Live reports whether the announcement is publicly visible now.
func (a Announcement) Live(now time.Time) bool {
	if a.Status != "PUBLISHED" {
		return false
	}
	if now.Before(a.PublishAt) {
		return false
	}
	return a.ExpiresAt == nil || now.Before(*a.ExpiresAt)
}

// MaintenanceWindow is one scheduled-work calendar entry (migration 172).
type MaintenanceWindow struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description,omitempty"`
	Scope       string    `json:"scope"` // FULL_VENUE|GATEWAY|MARKET_DATA|SETTLEMENT|FUNDING|INSTRUMENT
	Symbols     []string  `json:"symbols,omitempty"`
	Status      string    `json:"status"` // SCHEDULED|IN_PROGRESS|COMPLETED|CANCELLED
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Stats24h is the Phase-11 Task 11.3.5 market-statistics row: the rolling
// 24-hour aggregate for one symbol, ending at the request's server time.
// The window is TRAILING (same semantics as Ticker24h and the marketdata
// stats@all producer) — open_time_ms/close_time_ms are the [now-24h, now)
// bounds, first/last_trade_ms the observed trade times inside it. Price
// fields are nil when the window holds no trades — no synthesized quotes.
type Stats24h struct {
	Symbol         string  `json:"symbol"`
	Window         string  `json:"window"`         // "24h"
	OpenTimeMs     int64   `json:"open_time_ms"`   // window start (close-24h)
	CloseTimeMs    int64   `json:"close_time_ms"`  // window end (server time)
	Open           *string `json:"open,omitempty"` // first trade price in window
	High           *string `json:"high,omitempty"`
	Low            *string `json:"low,omitempty"`
	Last           *string `json:"last,omitempty"`
	PriceChange    *string `json:"price_change,omitempty"`
	PriceChangePct *string `json:"price_change_pct,omitempty"`
	Volume         string  `json:"volume"`       // base currency
	QuoteVolume    string  `json:"quote_volume"` // quote currency
	TradeCount     int64   `json:"trade_count"`
	FirstTradeMs   *int64  `json:"first_trade_ms,omitempty"`
	LastTradeMs    *int64  `json:"last_trade_ms,omitempty"`
	ServerTimeMs   int64   `json:"server_time_ms"`
}
