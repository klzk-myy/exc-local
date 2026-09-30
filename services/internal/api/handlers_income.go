// Phase-20 Task 20.3.12 — per-account income history (spec §16.7,
// §24 #361):
//
//	GET /api/v1/account/income?type=&symbol=&from=&to=&limit=&cursor=
//
// Data provenance (fail-closed, spec §2.7): reads ONLY the ClickHouse
// income_ledger projection via analytics.IncomeStore — the same cold-tier
// contract as the history handlers: a dead/unpopulated ClickHouse answers
// SERVICE_DEGRADED (503), never a silently-substituted PostgreSQL read
// (the wallet ledger has different projection semantics and substituting
// it would hide ETL loss).
//
// Emptiness is disambiguated honestly: an account with zero matching rows
// while the projection is populated gets 200 + empty data; a projection
// that holds no rows at all (never synced / cluster rebuilt) answers
// SERVICE_DEGRADED because the stream cannot be trusted.
//
// type= accepts the §16.7 taxonomy (COMMISSION, SWAP_ROLLOVER, REBATE,
// NBP_ADJUSTMENT, DUST_CONVERT, FUNDING_FEE) plus the extended tokens the
// refined classifier emits (TRADING_FEE, CONVERSION_FEE,
// SWAPFREE_ADMIN_FEE, INACTIVITY_FEE, LIQUIDATION_PENALTY) and verbatim
// ledger entry types —
// the projection is open by design (analytics.ClassifyIncome).
// symbol= takes BASE/QUOTE or BASEQUOTE forms; from/to are RFC3339
// [from, to) bounds over posted_at. Rows are newest-first; the opaque
// §8.8 cursor keys on (posted_at, ledger_entry_id).
package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"exchange/internal/analytics"
	"exchange/internal/gateway"
)

// incomeListSpec is this route's §8.8 pagination contract — declared
// locally because the published ListSpecs table is owned by Task 5.3.42's
// file (pagination.go, outside this task's write scope); the orchestrator
// may fold this row into the table when the meta surface is next edited.
var incomeListSpec = &ListSpec{
	Path: "/api/v1/account/income", Default: 100, Max: 1000,
	Sortable:   []string{"posted_at", "ledger_entry_id"},
	Filterable: []string{"type", "symbol", "from", "to"},
}

// IncomeHistorySource is the cold-tier income read seam —
// *analytics.IncomeStore satisfies it; tests inject fakes.
type IncomeHistorySource interface {
	Query(ctx context.Context, q analytics.IncomeQuery) ([]analytics.IncomeRow, int64, error)
	// HasAny distinguishes "account has no income" (200 + []) from
	// "projection never populated" (503 fail-closed).
	HasAny(ctx context.Context) (bool, error)
}

var incomeSymbolRe = regexp.MustCompile(`^[A-Za-z]{6}$`)

// normalizeIncomeSymbol accepts "EURUSD" or "EUR/USD" and returns the
// canonical slash form the projection derives.
func normalizeIncomeSymbol(raw string) (string, bool) {
	s := strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(raw, "/", "")))
	if !incomeSymbolRe.MatchString(s) {
		return "", false
	}
	return s[:3] + "/" + s[3:], true
}

// incomeDoc is the wire projection of one income row — decimal strings
// for money (spec §5.3), signed from the wallet's perspective.
type incomeDoc struct {
	PostedAt       string `json:"posted_at"`
	Type           string `json:"type"`
	EntryType      string `json:"entry_type"`
	Symbol         string `json:"symbol,omitempty"`
	Amount         string `json:"amount"`
	Currency       string `json:"currency"`
	LedgerEntryID  uint64 `json:"ledger_entry_id"`
	JournalEntryID uint64 `json:"journal_entry_id"`
	ReferenceID    uint64 `json:"reference_id,omitempty"`
	Description    string `json:"description,omitempty"`
}

// AccountIncome serves the authenticated account's income history.
func AccountIncome(src IncomeHistorySource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if src == nil {
			WriteError(w, "SERVICE_DEGRADED",
				"income store not configured",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		accountID, claims, ok := claimsAccount(w, r)
		if !ok {
			return
		}
		if rejectForeignAccount(w, r, claims, r.URL.Query().Get("account_id")) {
			return
		}
		p, err := ParseListParams(r, incomeListSpec)
		if err != nil {
			WriteError(w, "INVALID_REQUEST", err.Error(),
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		q := r.URL.Query()
		iq := analytics.IncomeQuery{AccountID: accountID, Limit: p.Limit}

		if raw := strings.TrimSpace(q.Get("type")); raw != "" {
			iq.Type = strings.ToUpper(raw)
			if !analytics.ValidIncomeType(iq.Type) {
				WriteError(w, "INVALID_REQUEST",
					"type must be an income-type token (e.g. COMMISSION, REBATE)",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}
		if raw := strings.TrimSpace(q.Get("symbol")); raw != "" {
			sym, ok := normalizeIncomeSymbol(raw)
			if !ok {
				WriteError(w, "INVALID_REQUEST",
					"symbol must be a 6-letter pair (EURUSD or EUR/USD)",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
			iq.Symbol = sym
		}
		from, err := parseTimeQuery(q.Get("from"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "from must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if from != nil {
			iq.From = *from
		}
		to, err := parseTimeQuery(q.Get("to"))
		if err != nil {
			WriteError(w, "INVALID_REQUEST", "to must be RFC3339",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if to != nil {
			iq.To = *to
		}
		if p.Decoded != nil {
			iq.After = &analytics.IncomeCursor{
				PostedAt:      p.Decoded.CreatedAt,
				LedgerEntryID: uint64(p.Decoded.ID),
			}
		}

		rows, total, err := src.Query(r.Context(), iq)
		if err != nil {
			WriteError(w, "SERVICE_DEGRADED",
				"income store unavailable",
				gateway.RequestIDFrom(r.Context()), nil)
			return
		}
		if len(rows) == 0 {
			// Fail-closed distinction: an empty page on a populated
			// projection is a real 200; an empty PROJECTION means the
			// stream was never synced and an empty answer would lie.
			populated, perr := src.HasAny(r.Context())
			if perr != nil || !populated {
				WriteError(w, "SERVICE_DEGRADED",
					"income projection not populated",
					gateway.RequestIDFrom(r.Context()), nil)
				return
			}
		}

		docs := make([]incomeDoc, 0, len(rows))
		for _, row := range rows {
			docs = append(docs, incomeDoc{
				PostedAt:       row.PostedAt.UTC().Format(time.RFC3339Nano),
				Type:           row.IncomeType,
				EntryType:      row.EntryType,
				Symbol:         row.Symbol,
				Amount:         row.Amount.StringFixed(8),
				Currency:       row.Currency,
				LedgerEntryID:  row.LedgerEntryID,
				JournalEntryID: row.JournalEntryID,
				ReferenceID:    row.ReferenceID,
				Description:    row.Description,
			})
		}
		env := NewListEnvelope(docs, p,
			PageCursors(rows, func(row analytics.IncomeRow) (time.Time, int64) {
				return row.PostedAt, int64(row.LedgerEntryID)
			}), total)
		WriteJSON(w, http.StatusOK, env)
	}
}
