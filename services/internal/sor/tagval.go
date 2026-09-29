// tagval.go — minimal FIX tag-value frame codec for the venue initiator
// path. Deliberately tiny: the loopback/test adapter encodes real
// 35=D/35=8 frames so the connector contract exercises actual wire
// framing — connectors to real venues plug in behind VenueConnector.
package sor

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"exchange/pkg/decimal"
)

// decimalFromString parses a FIX numeric tag; empty input yields zero.
func decimalFromString(s string) (decimal.Decimal, error) {
	if s == "" {
		return decimal.Decimal{}, nil
	}
	return decimal.NewFromString(s)
}

// buildTagValue builds a wire-valid FIX frame:
// 8=FIX.4.4 | 9=<bodylen> | fields… | 10=<checksum>.
func buildTagValue(fields [][2]string) []byte {
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%s=%s\x01", f[0], f[1])
	}
	payload := b.String()
	frame := fmt.Sprintf("8=FIX.4.4\x019=%d\x01%s", len(payload), payload)
	return []byte(frame + "10=" + tvChecksum(frame) + "\x01")
}

func tvChecksum(s string) string {
	sum := 0
	for i := 0; i < len(s); i++ {
		sum += int(s[i])
	}
	return fmt.Sprintf("%03d", sum%256)
}

// tvField is one parsed tag=value pair (ordered).
type tvField struct {
	Tag string
	Val string
}

// parseTagValue tokenizes a frame into ordered fields (no checksum
// validation — the transport layer verified it).
func parseTagValue(raw []byte) []tvField {
	var out []tvField
	for _, seg := range strings.Split(string(raw), "\x01") {
		if seg == "" {
			continue
		}
		eq := strings.IndexByte(seg, '=')
		if eq <= 0 {
			continue
		}
		out = append(out, tvField{seg[:eq], seg[eq+1:]})
	}
	return out
}

func tvGet(fs []tvField, tag string) string {
	for _, f := range fs {
		if f.Tag == tag {
			return f.Val
		}
	}
	return ""
}

// fixNow renders TransactTime-style timestamps.
func fixNow(t time.Time) string { return t.UTC().Format("20060102-15:04:05.000") }

// encodeNewOrder builds the initiator 35=D frame the venue receives.
func encodeNewOrder(o *VenueOrder, senderComp, targetComp string, seq int64) []byte {
	side := "1"
	if o.Side == "SELL" {
		side = "2"
	}
	ordType := "1" // market
	if o.LimitPrice != nil {
		ordType = "2" // limit
	}
	fields := [][2]string{
		{"35", "D"},
		{"49", senderComp},
		{"56", targetComp},
		{"34", strconv.FormatInt(seq, 10)},
		{"52", fixNow(time.Now())},
		{"11", o.ClOrdID},
		{"55", o.Symbol},
		{"54", side},
		{"38", o.Qty.String()},
		{"40", ordType},
		{"59", "3"}, // IOC — external liquidity-taking flow never rests
		{"60", fixNow(time.Now())},
	}
	if o.LimitPrice != nil {
		fields = append(fields, [2]string{"44", o.LimitPrice.String()})
	}
	return buildTagValue(fields)
}

// parseExecReport decodes a venue 35=8 frame into a VenueEvent.
func parseExecReport(venueID string, raw []byte, at time.Time) (*VenueEvent, error) {
	fs := parseTagValue(raw)
	if tvGet(fs, "35") != "8" {
		return nil, fmt.Errorf("sor: venue frame MsgType=%q, expected 8", tvGet(fs, "35"))
	}
	qty, _ := decimal.NewFromString(tvGet(fs, "32")) // LastQty
	px, _ := decimal.NewFromString(tvGet(fs, "31"))  // LastPx
	cum, _ := decimal.NewFromString(tvGet(fs, "14")) // CumQty
	leaves, _ := decimal.NewFromString(tvGet(fs, "151"))
	ev := &VenueEvent{
		VenueID:         venueID,
		ExternalOrderID: tvGet(fs, "37"),
		ExecID:          tvGet(fs, "17"),
		RejectReason:    tvGet(fs, "58"),
		Qty:             qty,
		Price:           px,
		CumQty:          cum,
		LeavesQty:       leaves,
		At:              at,
	}
	switch tvGet(fs, "150") { // ExecType
	case "F", "1", "2": // trade / partial fill / fill
		ev.Kind = VenueFill
		ev.Done = leaves.IsZero() || tvGet(fs, "39") == "2"
	case "0": // new
		ev.Kind = VenueAck
	case "8": // rejected
		ev.Kind = VenueReject
	case "4": // canceled
		ev.Kind = VenueCancelConfirm
	default:
		return nil, fmt.Errorf("sor: venue ExecType=%q unsupported", tvGet(fs, "150"))
	}
	return ev, nil
}
