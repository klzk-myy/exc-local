// Stable machine-readable error codes (spec §23 registry). Canonical
// registration of every emitted code + HTTP status lands in Phase-05
// Task 5.3.21 — this file carries the codes owned by Task 1.3.12
// (spec §2.7, §3.6) until then.
package errors

import (
	stderrors "errors"
	"net/http"
)

const (
	// CodeArithmeticOverflowDetected: checked fixed-point price/volume
	// calculation overflow or underflow (spec §3.6.2) — HTTP 400, L2.
	CodeArithmeticOverflowDetected = "ARITHMETIC_OVERFLOW_DETECTED"
	// CodeOrderBookCapacityExceeded: static pre-allocated order book
	// level/node pool exhausted (spec §3.6.1) — HTTP 503, L2.
	CodeOrderBookCapacityExceeded = "ORDER_BOOK_CAPACITY_EXCEEDED"
	// CodeTimeSyncLossHalt: PTP/NTP clock drift > 100µs; matching halted
	// (spec §2.7.2 L0 trigger, MiFID II RTS 25) — HTTP 503, L0.
	CodeTimeSyncLossHalt = "TIME_SYNC_LOSS_HALT"
)

// HTTPStatus maps a code to its spec §23 HTTP status. Unknown codes map to
// 500: an unclassified internal failure must never be reported to the
// client as a client fault (fail-closed, spec §2.7.1).
func HTTPStatus(code string) int {
	switch code {
	case CodeArithmeticOverflowDetected:
		return http.StatusBadRequest
	case CodeOrderBookCapacityExceeded, CodeTimeSyncLossHalt:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// HTTPStatusOf resolves the HTTP status for err: a *Error contributes its
// Code; any other error maps to 500.
func HTTPStatusOf(err error) int {
	var e *Error
	if stderrors.As(err, &e) {
		return HTTPStatus(e.Code)
	}
	return http.StatusInternalServerError
}
