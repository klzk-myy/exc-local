// WS frame shapes (spec §10.5 items 4–6 + §10.9 + Task 6.3.9).
//
// Declared field order IS the canonical wire order, matching the
// internal/ws frames byte-for-byte so the marketdata and gateway WS
// surfaces are protocol-identical.
package marketdata

import (
	"encoding/json"
	"strings"
)

// Inbound client frame — `action` is the canonical discriminator.
// The field set is tolerant: control frames carry
// token/signature/timestamp/protocol_version; subscription frames carry
// channels/params; resume carries channel + last_seq; request frames
// carry request_id (or the `id` alias) + method + params; order frames
// carry request_id + params.
type clientFrame struct {
	Action          string          `json:"action"`
	RequestID       string          `json:"request_id"`
	ID              json.RawMessage `json:"id"` // legacy alias for request_id
	Method          string          `json:"method"`
	Token           string          `json:"token"`
	Signature       string          `json:"signature"`
	Timestamp       json.RawMessage `json:"timestamp"`        // epoch seconds (number or string)
	ProtocolVersion *int            `json:"protocol_version"` // mandatory on authenticate
	Params          json.RawMessage `json:"params"`
	Channel         string          `json:"channel"`
	Channels        []string        `json:"channels"`
	Symbol          string          `json:"symbol"` // resync form (§10.9): {"action":"resync","symbol":..,"last_seq":..}
	LastSeq         uint64          `json:"last_seq"`
}

// rid resolves the correlation id: request_id is canonical; the bare `id`
// form ({"action":"request","id":N,...}) is accepted as an alias.
func (f clientFrame) rid() string {
	if f.RequestID != "" {
		return f.RequestID
	}
	if len(f.ID) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(f.ID))
	return strings.Trim(s, `"`)
}

// tsString returns the timestamp verbatim (signed surface — never
// reformatted) as a bare scalar string.
func (f clientFrame) tsString() string {
	s := strings.TrimSpace(string(f.Timestamp))
	return strings.Trim(s, `"`)
}

// channelList parses the subscription channel list: `channels` array,
// `params` array-of-strings, `params.channels`, or scalar `channel`.
func (f clientFrame) channelList() ([]string, error) {
	if len(f.Channels) > 0 {
		return f.Channels, nil
	}
	if len(f.Params) > 0 {
		var arr []string
		if err := json.Unmarshal(f.Params, &arr); err == nil {
			return arr, nil
		}
		var obj struct {
			Channels []string `json:"channels"`
			Channel  string   `json:"channel"`
		}
		if err := json.Unmarshal(f.Params, &obj); err == nil {
			if len(obj.Channels) > 0 {
				return obj.Channels, nil
			}
			if obj.Channel != "" {
				return []string{obj.Channel}, nil
			}
		}
		return nil, errBadParams
	}
	if f.Channel != "" {
		return []string{f.Channel}, nil
	}
	return nil, errBadParams
}

// ---------------------------------------------------------------------------
// Server frames — field order is the canonical §10.5 wire order.
// ---------------------------------------------------------------------------

// responseFrame: {"type":"response","request_id":..,"action":..,
// "status":"ACK"|"NACK","data":{...},"ts_ms":..}
type responseFrame struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`
	Action    string `json:"action"`
	Status    string `json:"status"`
	Data      any    `json:"data"`
	TsMs      int64  `json:"ts_ms"`
}

// errorFrame: {"type":"error","request_id":..,"action":..,"error":"CODE",
// "message":"..","ts_ms":..,"retry_after_ms":..,"code":..}.
type errorFrame struct {
	Type         string `json:"type"`
	RequestID    string `json:"request_id,omitempty"`
	Action       string `json:"action,omitempty"`
	Error        string `json:"error"`
	Message      string `json:"message"`
	TsMs         int64  `json:"ts_ms"`
	RetryAfterMs int64  `json:"retry_after_ms,omitempty"`
	Code         int    `json:"code,omitempty"`
}

// eventFrame carries channel data to subscribers. Seq is the
// channel-scoped monotonic sequence the resume protocol binds to — for
// L2 channels it is the md:seq:{symbol} cursor's emitted last_seq.
type eventFrame struct {
	Type    string `json:"type"` // "event"
	Channel string `json:"channel"`
	Seq     uint64 `json:"seq"`
	Data    any    `json:"data"`
	TsMs    int64  `json:"ts_ms"`
}

// subscribedFrame acknowledges subscribe/unsubscribe control frames.
type subscribedFrame struct {
	Type     string   `json:"type"` // "subscribed" | "unsubscribed"
	Channels []string `json:"channels"`
	Total    int      `json:"total"`
	TsMs     int64    `json:"ts_ms"`
}

// pongFrame answers {"action":"ping"}.
type pongFrame struct {
	Type string `json:"type"` // "pong"
	TsMs int64  `json:"ts_ms"`
}

// resyncFrame directs the client to resynchronize when its last_seq
// cannot be replayed (Task 6.3.9 item 5, spec §10.7 explicit resync).
// Reason: "gap_too_large" | "invalid_sequence" | "no_snapshot_available".
type resyncFrame struct {
	Type    string `json:"type"` // "resync"
	Channel string `json:"channel"`
	Reason  string `json:"reason"`
	LastSeq uint64 `json:"last_seq"`
	TsMs    int64  `json:"ts_ms"`
}

// snapshotFrame delivers a full channel-state snapshot (Task 6.3.9 item
// 5: resume out-of-range falls back to snapshot with reason metadata).
type snapshotFrame struct {
	Type    string `json:"type"` // "snapshot"
	Channel string `json:"channel"`
	Reason  string `json:"reason"`
	Seq     uint64 `json:"seq"`
	Data    any    `json:"data"`
	TsMs    int64  `json:"ts_ms"`
}

// resumedFrame confirms a successful replay: "replayed N buffered
// messages, now live". Count==0 means the cursor was already current.
type resumedFrame struct {
	Type    string `json:"type"` // "resumed"
	Channel string `json:"channel"`
	FromSeq uint64 `json:"from_seq"` // first seq in this stream after the client's cursor
	ToSeq   uint64 `json:"to_seq"`   // current channel tail seq
	Count   int    `json:"count"`
	TsMs    int64  `json:"ts_ms"`
}

// marshalFrame renders one outbound frame.
func marshalFrame(v any) ([]byte, error) { return json.Marshal(v) }

// parseFrame decodes one inbound text frame.
func parseFrame(msg []byte) (clientFrame, error) {
	var f clientFrame
	if err := json.Unmarshal(msg, &f); err != nil {
		return clientFrame{}, err
	}
	return f, nil
}

// maxRequestIDLen bounds client-supplied correlation ids.
const maxRequestIDLen = 128
