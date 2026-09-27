// Package models holds shared domain models for FX instruments, orders
// and accounts. Field sets grow as their owning phases land (instruments:
// Phase-15; orders: Phase-02/05/16).
package models

import "exchange/pkg/decimal"

// Instrument is a tradable FX currency pair (spec §5/§6).
type Instrument struct {
	Symbol          string // e.g. "EURUSD"
	BaseCurrency    string // e.g. "EUR"
	QuoteCurrency   string // e.g. "USD"
	SettlementCycle string // T+0 | T+1 | T+2 per spec §6.3
	MaxLeverage     int    // e.g. 30 for ESMA retail majors
}

// Side is the order direction.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// Order is the scaffold order record. The full field set (execution params,
// derivative params, OCO/algo links) is assembled by Phases 02/16/22.
type Order struct {
	ID         int64
	AccountID  int64
	Instrument string
	Side       Side
	Quantity   decimal.Decimal
	LimitPrice decimal.Decimal // zero for market orders
}
