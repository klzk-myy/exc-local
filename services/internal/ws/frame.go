package ws

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Inbound client frame — `action` is the canonical discriminator
// (remediation #9). Field set is tolerant: control frames carry
// token/signature/timestamp/protocol_version, order frames carry
// request_id + params, subscription frames carry params (array) or
// channel/channels, resume carries channel + last_seq.
type clientFrame struct {
	Action          string          `json:"action"`
	RequestID       string          `json:"request_id"`
	Token           string          `json:"token"`
	Signature       string          `json:"signature"`
	Timestamp       json.RawMessage `json:"timestamp"`        // epoch seconds (number or string)
	ProtocolVersion *int            `json:"protocol_version"` // mandatory on authenticate
	Params          json.RawMessage `json:"params"`
	Channel         string          `json:"channel"`
	Channels        []string        `json:"channels"`
	LastSeq         uint64          `json:"last_seq"`
}

// tsString returns the timestamp verbatim (signed surface — never
// reformatted) as a bare scalar string.
func (f clientFrame) tsString() string {
	s := strings.TrimSpace(string(f.Timestamp))
	return strings.Trim(s, `"`)
}

// channels parses the subscription channel list: `channels` array,
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
// Server frames — declared field order IS the canonical wire order
// (spec §10.5 items 4–6, internal-consistency audit F2/F4).
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
// `code` rides at the tail (Task 5.3.26 shows it on AUTH_EXPIRED carrying
// the 4019 close code) — the canonical field prefix is preserved.
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

// eventFrame carries channel data to subscribers. seq is the per
// (connection, channel) monotonic sequence — the cursor Phase-06's
// resume/ring-buffer protocol binds to.
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

// resyncFrame answers {"action":"resume"}: without the Phase-06 ring
// buffer the honest answer is always a resync directive — the client
// refetches a snapshot and resubscribes from seq 0.
type resyncFrame struct {
	Type    string `json:"type"` // "resync"
	Channel string `json:"channel"`
	LastSeq uint64 `json:"last_seq"`
	TsMs    int64  `json:"ts_ms"`
}

// parseFrame decodes one inbound text frame.
func parseFrame(msg []byte) (clientFrame, error) {
	var f clientFrame
	if err := json.Unmarshal(msg, &f); err != nil {
		return clientFrame{}, err
	}
	return f, nil
}

// marshalFrame renders one outbound frame. Encoding never fails on the
// concrete types above; Data is caller-controlled so errors surface.
func marshalFrame(v any) ([]byte, error) { return json.Marshal(v) }

// maxRequestIDLen bounds client-supplied correlation ids (spec §10.5
// leaves it implementation-defined; bounded so dedup keys stay sane).
const maxRequestIDLen = 128

// itoa renders small ints without strconv noise at call sites.
func itoa(v int) string { return strconv.Itoa(v) }
