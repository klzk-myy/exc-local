package rates

import (
	"encoding/json"
	"fmt"
	"time"

	"exchange/pkg/decimal"
)

// curveJSON is the on-the-wire shape of curve:{ccy} — pillar rates as
// decimal strings (no float64 on the financial path).
type curveJSON struct {
	Currency string            `json:"currency"`
	Rates    map[string]string `json:"rates"`
	AsOfNs   int64             `json:"as_of_ns"`
	Feeds    []string          `json:"feeds,omitempty"`
}

func jsonMarshalCurve(c Curve) ([]byte, error) {
	rates := make(map[string]string, len(c.Rates))
	for t, r := range c.Rates {
		rates[string(t)] = r.String()
	}
	return json.Marshal(curveJSON{
		Currency: c.Currency, Rates: rates,
		AsOfNs: c.AsOf.UnixNano(), Feeds: c.SourceFeeds,
	})
}

func jsonUnmarshalCurve(doc string) (Curve, error) {
	var j curveJSON
	if err := json.Unmarshal([]byte(doc), &j); err != nil {
		return Curve{}, fmt.Errorf("rates: curve decode: %w", err)
	}
	c := Curve{
		Currency: j.Currency, Rates: map[Tenor]decimal.Decimal{},
		AsOf: time.Unix(0, j.AsOfNs), SourceFeeds: j.Feeds,
	}
	for t, rs := range j.Rates {
		d, err := decimal.NewFromString(rs)
		if err != nil {
			return Curve{}, fmt.Errorf("rates: curve %s tenor %s malformed: %w",
				j.Currency, t, err)
		}
		c.Rates[Tenor(t)] = d
	}
	return c, nil
}
