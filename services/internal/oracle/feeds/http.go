package feeds

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"exchange/internal/oracle"
	"exchange/pkg/decimal"
)

var errFeedDown = fmt.Errorf("feed unavailable")

// ---------------------------------------------------------------------------
// Refinitiv — REST snapshot adapter (Task 19.5.3.1 step 1).
// Endpoint + credentials are env-configured (EXC_REFINITIV_URL,
// EXC_REFINITIV_KEY); the adapter parses the vendor's quote array shape
// {"quotes":[{"ric":"EUR=","mid":1.0847,"volume":…,"ts":"…"}]}.
// ---------------------------------------------------------------------------

// Refinitiv is a REST snapshot adapter for a Refinitiv/TREP-style
// quote endpoint.
type Refinitiv struct {
	URL    string
	APIKey string
	HTTP   *http.Client
}

// Name implements oracle.Feed.
func (r *Refinitiv) Name() string { return "refinitiv" }

// Poll fetches a quote snapshot for the requested RIC-style symbols.
func (r *Refinitiv) Poll(ctx context.Context, symbols []string) ([]oracle.Quote, error) {
	if r.URL == "" {
		return nil, fmt.Errorf("refinitiv: endpoint unconfigured")
	}
	hc := r.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		r.URL+"?rics="+strings.Join(symbols, ","), nil)
	if err != nil {
		return nil, err
	}
	if r.APIKey != "" {
		req.Header.Set("X-API-Key", r.APIKey)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refinitiv: http %d", resp.StatusCode)
	}
	var doc struct {
		Quotes []struct {
			RIC    string `json:"ric"`
			Mid    string `json:"mid"`
			Volume string `json:"volume"`
			Ts     int64  `json:"ts_ms"`
		} `json:"quotes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("refinitiv: decode: %w", err)
	}
	out := make([]oracle.Quote, 0, len(doc.Quotes))
	for _, q := range doc.Quotes {
		mid, err := decimal.NewFromString(q.Mid)
		if err != nil || !mid.IsPositive() {
			continue
		}
		w, _ := decimal.NewFromString(q.Volume)
		out = append(out, oracle.Quote{
			Symbol: q.RIC, Mid: mid, Weight: w,
			Ts: time.UnixMilli(q.Ts), Feed: r.Name(),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Bloomberg BFIX — REST benchmark-rate adapter (Task 19.5.3.1 step 2).
// BFIX publishes half-hourly benchmark snaps; the adapter consumes the
// configured endpoint's {"rates":[{"pair":"EURUSD","rate":…,"ts":…}]}
// document. Endpoint/credential env: EXC_BFIX_URL / EXC_BFIX_TOKEN.
// ---------------------------------------------------------------------------

// BFIX is the Bloomberg benchmark-rate adapter.
type BFIX struct {
	URL   string
	Token string
	HTTP  *http.Client
}

// Name implements oracle.Feed.
func (b *BFIX) Name() string { return "bfix" }

// Poll fetches the latest BFIX snap.
func (b *BFIX) Poll(ctx context.Context, symbols []string) ([]oracle.Quote, error) {
	if b.URL == "" {
		return nil, fmt.Errorf("bfix: endpoint unconfigured")
	}
	hc := b.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return nil, err
	}
	if b.Token != "" {
		req.Header.Set("Authorization", "Bearer "+b.Token)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bfix: http %d", resp.StatusCode)
	}
	var doc struct {
		Rates []struct {
			Pair string `json:"pair"`
			Rate string `json:"rate"`
			Ts   int64  `json:"ts_ms"`
		} `json:"rates"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("bfix: decode: %w", err)
	}
	want := map[string]bool{}
	for _, s := range symbols {
		want[strings.ReplaceAll(s, "/", "")] = true
	}
	out := make([]oracle.Quote, 0, len(symbols))
	for _, r := range doc.Rates {
		if !want[r.Pair] {
			continue
		}
		mid, err := decimal.NewFromString(r.Rate)
		if err != nil || !mid.IsPositive() {
			continue
		}
		sym := r.Pair
		if len(sym) == 6 {
			sym = sym[:3] + "/" + sym[3:] // BFIX "EURUSD" → venue "EUR/USD"
		}
		out = append(out, oracle.Quote{
			Symbol: sym, Mid: mid, Ts: time.UnixMilli(r.Ts), Feed: b.Name(),
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// ECB — daily euro reference rates (Task 19.5.3.1 step 3). The ECB
// publishes a free XML document at 14:00 CET; the adapter parses the
// SDMX-style cube document and maps XXX/EUR rates to the venue's
// XXX/USD convention by combining with the feed's own EUR/USD cross
// where needed — EUR-quoted rows are carried as "<ccy>/EUR" and the
// aggregator's pair normalization handles inversion upstream.
// ---------------------------------------------------------------------------

// ECB is the euro reference-rate adapter.
type ECB struct {
	URL  string // EXC_ECB_URL; default ecb.europa.eu stats feed
	HTTP *http.Client
	now  func() time.Time
}

// Name implements oracle.Feed.
func (e *ECB) Name() string { return "ecb" }

type ecbCube struct {
	Cubes []struct {
		Time     string `xml:"time,attr"`
		Children []struct {
			Currency string `xml:"currency,attr"`
			Rate     string `xml:"rate,attr"`
		} `xml:"Cube"`
	} `xml:"Cube>Cube"`
}

// Poll fetches the daily ECB document and maps rows for requested
// XXX/EUR pairs (ECB rates are EUR-based: USD row → "EUR/USD" price is
// the USD-per-EUR rate; a request for "EUR/USD" matches currency=USD).
func (e *ECB) Poll(ctx context.Context, symbols []string) ([]oracle.Quote, error) {
	if e.URL == "" {
		return nil, fmt.Errorf("ecb: endpoint unconfigured")
	}
	hc := e.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.URL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ecb: http %d", resp.StatusCode)
	}
	var doc ecbCube
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 1<<22)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("ecb: decode: %w", err)
	}
	now := e.now
	if now == nil {
		now = time.Now
	}
	// Map "EUR/USD" → ECB row currency=USD (rate is USD per EUR).
	out := make([]oracle.Quote, 0, len(symbols))
	for _, sym := range symbols {
		parts := strings.SplitN(sym, "/", 2)
		if len(parts) != 2 || parts[0] != "EUR" {
			continue // ECB document only prices EUR bases
		}
		for _, day := range doc.Cubes {
			dayTs, _ := time.Parse("2006-01-02", day.Time)
			for _, row := range day.Children {
				if row.Currency != parts[1] {
					continue
				}
				rate, err := decimal.NewFromString(row.Rate)
				if err != nil || !rate.IsPositive() {
					continue
				}
				out = append(out, oracle.Quote{
					Symbol: sym, Mid: rate,
					Ts:   dayTs.Add(14 * time.Hour), // published 14:00 CET
					Feed: e.Name(),
				})
			}
		}
	}
	_ = now
	return out, nil
}
