package reporting

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// SWIFT MT515 confirmation writer (ISO 15022) — Task 20.3.8 item 2c
//
// A companion mapping exists for the settlement pipeline (settlement
// MT542/543/548). MT515 is the post-execution confirmation variant —
// generated here from the sibling-archived confirmation JSON rather
// than from engine fields so both consumers (portal PDF + MT515)
// describe one document of record.
//
// Transport is Phase-24: MT515Submitter today resolves to
// LogMT515Submitter (writes the raw SWIFT text to the ops log /
// secured-HTTP POST feed upstream, per io design). The writer is pure —
// deterministic output for a given record.
// ---------------------------------------------------------------------------

// MT515Doc renders an MT515 message for one confirmation record +
// stored document payload. doc carries execution semantics; nil doc
// produces a record-fields-only message (trade/account/venue) — valid
// MT515 skeleton for the not-yet-fetched-doc edge, never fabricated
// quantities.
func MT515Doc(rec ConfirmationRecord, doc *confirmationJSON) string {
	var b strings.Builder
	w := func(tag, val string) { fmt.Fprintf(&b, ":%s:%s\n", tag, val) }

	side, qty, price := "BUY", "", ""
	settlement, execTS, symbol := "", "", ""
	venue := "XOFF"
	if doc != nil {
		side = strings.ToUpper(doc.Side)
		if side == "" {
			side = "BUY"
		}
		if side != "BUY" && side != "SELL" {
			side = "BUY"
		}
		qty = doc.Quantity
		if len(doc.Symbol) >= 7 {
			qty = fmt.Sprintf("%s%s", doc.Symbol[4:], doc.Quantity) // ccy prefix + amount
		}
		price = doc.Price
		settlement = mt515Date(doc.SettlementDate)
		execTS = mt515Timestamp(doc.ExecutedAt)
		symbol = doc.Symbol
		if doc.Venue != "" {
			venue = doc.Venue
		}
	}
	if settlement == "" {
		settlement = rec.GeneratedAt.UTC().Format("20060102")
	}
	if execTS == "" {
		execTS = rec.GeneratedAt.UTC().Format("20060102150405")
	}

	// A — basic identification
	b.WriteString(":16R:GENL\n")
	w("20C", fmt.Sprintf(":SEME//%016d-%d", rec.TradeID, rec.Version))
	w("23G", ":NEWM") // NEWM on every issue — CANC/REPL handled by 20C linkage
	if rec.SupersedesID != nil {
		w("20C", fmt.Sprintf(":PREV//CONF%016d", *rec.SupersedesID))
	}
	b.WriteString(":16S:GENL\n")

	// B — confirmation details
	b.WriteString(":16R:CONFDET\n")
	w("98A", fmt.Sprintf(":TRAD//%s", execTS[:8]))
	w("98B", fmt.Sprintf(":TRAD//DATI/%s", execTS))
	w("98A", fmt.Sprintf(":SETT//%s", settlement))
	w("94B", ":TRAD//EXCH/"+venue)
	b.WriteString(":35B:" + fmt.Sprintf("ISIN %s\n", symbolISIN(symbol)))
	w("36B", fmt.Sprintf(":CONF//FAMT/%s", qty))
	w("90B", fmt.Sprintf(":DEAL//ACTU//%s", price))
	w("22H", ":BUSE//"+map[bool]string{true: "BUYI", false: "SELI"}[side == "BUY"])
	if doc != nil && doc.Fee != "" && doc.Fee != "0" {
		w("19A", fmt.Sprintf(":CHAR//%s%s", doc.FeeCurrency, doc.Fee))
	}
	b.WriteString(":16S:CONFDET\n")

	// C — parties (account identification)
	b.WriteString(":16R:CONFPRTY\n")
	w("95Q", fmt.Sprintf(":ACOW//ACCT-%d", rec.AccountID))
	b.WriteString(":16S:CONFPRTY\n")
	return b.String()
}

func symbolISIN(symbol string) string {
	// FX pairs carry no ISIN; keep the canonical pair code so the
	// receiving OMS reconciles by instrument mnemonic.
	return strings.ReplaceAll(symbol, "/", "")
}

func mt515Date(s string) string {
	// sibling emits DATE (YYYY-MM-DD) → MT515 wants YYYYMMDD
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.Format("20060102")
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format("20060102")
	}
	return ""
}

func mt515Timestamp(s string) string {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Format("20060102150405")
	}
	if t, err := time.Parse("2006-01-02 15:04:05.999999999", s); err == nil {
		return t.UTC().Format("20060102150405")
	}
	return ""
}

// MT515Submitter hands a rendered MT515 to the transport seam.
type MT515Submitter interface {
	SubmitMT515(ctx context.Context, rec ConfirmationRecord, doc *confirmationJSON) error
}

// LogMT515Submitter is the Phase-24-seam default: renders + logs the
// message length and header line (full body goes to the secured
// SWIFT feed, never to plain logs).
type LogMT515Submitter struct {
	Log *slog.Logger
	// Hook, if set, receives the rendered body (secured uplink seam).
	Hook func(ctx context.Context, body string) error
}

// SubmitMT515 implements MT515Submitter.
func (s LogMT515Submitter) SubmitMT515(ctx context.Context, rec ConfirmationRecord, doc *confirmationJSON) error {
	body := MT515Doc(rec, doc)
	l := s.Log
	if l == nil {
		l = slog.Default()
	}
	l.Info("MT515 confirmation",
		"trade_id", rec.TradeID, "version", rec.Version,
		"account_id", rec.AccountID, "bytes", len(body))
	if s.Hook != nil {
		return s.Hook(ctx, body)
	}
	return nil
}

// MemMT515Submitter captures rendered bodies for tests.
type MemMT515Submitter struct {
	mu      sync.Mutex
	Bodies  []string
	FailErr error
}

// SubmitMT515 implements MT515Submitter.
func (m *MemMT515Submitter) SubmitMT515(_ context.Context, rec ConfirmationRecord, doc *confirmationJSON) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailErr != nil {
		return m.FailErr
	}
	m.Bodies = append(m.Bodies, MT515Doc(rec, doc))
	return nil
}
