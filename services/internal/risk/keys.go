// keys.go — canonical Redis key layout for the Phase-19 liquidation
// cluster (spec §4 naming convention; §13.3–§13.6d, §13.15).
//
// Cluster-ownership note (multi-cluster Phase-19 landing): this file is
// the single source of truth for the liquidation-family keys. Cluster B
// (margin level producer) owns WRITES to margin:level:{account_id}; this
// cluster owns every other key below. The builders are shared so both
// sides read/write the identical layout.
package risk

import "fmt"

const (
	// LiquidationQueueKey is the §13.5 liquidation dispatch queue
	// (Redis LIST of JSON LiquidationJob).
	LiquidationQueueKey = "liquidation:queue"
	// LiquidationDelayedQueueKey is the retry back-off ZSET used by
	// ConsumeLiquidationQueue's release(delay) protocol (§13.15 item 4).
	LiquidationDelayedQueueKey = "liquidation:queue:delayed"

	// LiquidationDedupTTL is the dedup window during which an account is
	// enqueued at most once (spec §13.15 names the stranding scenario
	// "3600s dedup keys").
	LiquidationDedupTTLSeconds = 3600

	// MarginCallWindowSeconds is the §13.3 deposit window (15 min).
	MarginCallWindowSeconds = 900
)

// MarginLevelKey is the margin:level:{account_id} HASH produced by the
// margin-level engine (Phase-19 sibling cluster) — fields:
// {equity, used_margin, margin_level_pct, status, updated_at}.
// margin_level_pct is OMITTED when used_margin == 0 (infinity has no
// decimal encoding; a missing field reads as "no margin in use").
func MarginLevelKey(accountID int64) string {
	return fmt.Sprintf("margin:level:%d", accountID)
}

// MarkPriceKey is the mark:{symbol} STRING carrying the latest mark as a
// decimal string (RedisMarkCache.BatchMarks MGETs these).
func MarkPriceKey(symbol string) string {
	return fmt.Sprintf("mark:%s", symbol)
}

// MarkChannelPattern is the PSUBSCRIBE pattern matching every mark
// delta channel (mark:{symbol}) — the MarkDeltaSource wire contract.
const MarkChannelPattern = "mark:*"

// MarginCallKey is the §13.3 deposit-window key margin_call:{account_id}
// (15-minute TTL). Its presence means the window is open; the order block
// is the persistent companion MarginCallBlockKey.
func MarginCallKey(accountID int64) string {
	return fmt.Sprintf("margin_call:%d", accountID)
}

// MarginCallBlockKey is the order-entry block flag for the margin-call
// episode (spec §13.3 precedence: the MARGIN_CALL_EXCEEDED order block
// persists for the entire episode — the 15-minute window cures the
// shortfall, it does not restore trading). Cleared only on recovery above
// the margin-call threshold or an audited Risk Manager re-enable.
func MarginCallBlockKey(accountID int64) string {
	return fmt.Sprintf("margin_call:block:%d", accountID)
}

// LiquidationDedupKey is the liquidation:dedup:{account_id} STRING —
// one in-flight liquidation per account.
func LiquidationDedupKey(accountID int64) string {
	return fmt.Sprintf("liquidation:dedup:%d", accountID)
}

// LiquidationLockKey is the lock:liquidation:account:{account_id} mutex
// serializing stop-out processing (spec §13.15 item 4).
func LiquidationLockKey(accountID int64) string {
	return fmt.Sprintf("lock:liquidation:account:%d", accountID)
}

// LiquidationScannerStateKey is the §13.5
// liquidation:scanner:state:{shard} HASH (last sweep watermark).
func LiquidationScannerStateKey(shard string) string {
	return fmt.Sprintf("liquidation:scanner:state:%s", shard)
}

// AdlPriorityKey is the adl:priority:{symbol}:{side} ZSET — member
// account_id, score = profit_pct × effective_leverage (Task 19.3.19).
// side is the POSITION side being ranked (LONG|SHORT).
func AdlPriorityKey(symbol, side string) string {
	return fmt.Sprintf("adl:priority:%s:%s", symbol, side)
}

// AdlIndicatorKey is the adl:indicator:{account_id} HASH
// (position_id → quintile level 1–5) backing the REST/WS indicator.
func AdlIndicatorKey(accountID int64) string {
	return fmt.Sprintf("adl:indicator:%d", accountID)
}

// AuctionStateKey is the §13.4 auction:{instrument_id}:{position_id}
// HASH carrying the live auction phase for LP/market-data fan-out.
func AuctionStateKey(instrumentID, positionID int64) string {
	return fmt.Sprintf("auction:%d:%d", instrumentID, positionID)
}
