package archiver

import "testing"

func TestParseBoundEnd_Internal(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		{`FOR VALUES FROM ('2020-01-01 00:00:00+00') TO ('2020-02-01 00:00:00+00')`, "2020-02-01"},
		{`FOR VALUES FROM ('2020-01-01') TO ('2020-02-01')`, "2020-02-01"},
		{`FOR VALUES FROM (MINVALUE) TO ('2020-02-01')`, "2020-02-01"},
		{`FOR VALUES FROM ('2020-01-01') TO (MAXVALUE)`, ""},
	}
	for _, c := range cases {
		got := parseBoundEnd(c.expr)
		if c.want == "" {
			if !got.IsZero() {
				t.Errorf("%s → %v, want zero", c.expr, got)
			}
			continue
		}
		if got.Format("2006-01-02") != c.want {
			t.Errorf("%s → %v, want %s", c.expr, got, c.want)
		}
	}
}
