package orders

import (
	"testing"

	"exchange/pkg/decimal"
)

// netPendingQty implements the §24 #287 net-proceeds sizing: the
// pending SELL leg carries the working BUY's received base minus its
// base-denominated commission, rounded down to the lot boundary; the
// sub-lot residue stays locked on the list row until placement settles.
func TestNetPendingQty(t *testing.T) {
	cases := []struct {
		name               string
		filled, commission string
		lot                string
		wantNet, wantRes   string
	}{
		{"exact lot", "10", "0.1", "0.1", "9.9", "0"},
		{"sub-lot residue", "10", "0.1", "1", "9", "0.9"},
		{"fractional lot", "5", "0.3", "0.25", "4.5", "0.2"},
		{"commission consumes fill", "1", "1", "0.1", "0", "0"},
		{"commission exceeds fill", "1", "1.5", "0.1", "0", "0"},
		{"no lot rounding", "7.77", "0.7", "0", "7.07", "0"},
		{"below one lot", "1", "0.6", "1", "0", "0.4"},
	}
	for _, tc := range cases {
		net, res := netPendingQty(
			decimal.MustFromString(tc.filled),
			decimal.MustFromString(tc.commission),
			decimal.MustFromString(tc.lot))
		if !net.Equal(decimal.MustFromString(tc.wantNet)) ||
			!res.Equal(decimal.MustFromString(tc.wantRes)) {
			t.Errorf("%s: net=%s res=%s, want %s/%s",
				tc.name, net, res, tc.wantNet, tc.wantRes)
		}
	}
}
